// Package github reads the queue from GitHub and writes the queue's statuses, comments and
// labels there. It is the only thing the queue asks of any service: CI's verdicts are read
// as the statuses and check runs CI posts on a commit.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// Client talks to one repository.
type Client struct {
	API   string // https://api.github.com
	Repo  string // owner/name
	Token string
	HTTP  *http.Client
	Wait  func(time.Duration)
	// Spent is what the client has asked of GitHub so far, by the quota each reply named
	// (core, graphql): how many requests it made, and how much of the hour's quota the newest
	// reply said is left.
	Spent map[string]Quota
}

// Quota is one of GitHub's hourly quotas, as a client has drawn on it.
type Quota struct {
	Requests int `json:"requests"`
	Left     int `json:"left"`
}

// Status is the newest report of one context on a commit.
type Status struct {
	State       queue.Verdict
	Description string
	URL         string
	At          time.Time
	// Running is a check run saying it has begun. A commit status has only its description.
	Running bool
}

const maxWait = 5 * time.Second

func (c *Client) do(again bool, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		payload, _ = json.Marshal(in)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	wait := c.Wait
	if wait == nil {
		wait = time.Sleep
	}
	for attempt := 0; ; attempt++ {
		after, retry, err := c.once(client, method, path, payload, out)
		if err == nil || !again || !retry || attempt == 3 {
			return err
		}
		base := min(500*time.Millisecond<<attempt, maxWait)
		wait(min(max(base, after)+rand.N(base/2), maxWait))
	}
}

func (c *Client) once(client *http.Client, method, path string, payload []byte, out any) (time.Duration, bool, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, _ := http.NewRequest(method, strings.TrimRight(c.API, "/")+path, body)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := client.Do(req)
	if err != nil {
		return 0, true, &Error{Text: fmt.Sprintf("%s %s: %v", method, path, err), Err: err, Unavailable: true}
	}
	defer res.Body.Close()
	if quota := res.Header.Get("X-RateLimit-Resource"); quota != "" {
		left, _ := strconv.Atoi(res.Header.Get("X-RateLimit-Remaining"))
		if c.Spent == nil {
			c.Spent = map[string]Quota{}
		}
		c.Spent[quota] = Quota{c.Spent[quota].Requests + 1, left}
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, true, &Error{Code: res.StatusCode, Text: fmt.Sprintf("%s %s: %v", method, path, err), Err: err, Unavailable: true}
	}
	if res.StatusCode >= 300 {
		var after time.Duration
		if s, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && s > 0 {
			after = time.Duration(s) * time.Second
		}
		secondary := res.StatusCode == 403 && (res.Header.Get("Retry-After") != "" || bytes.Contains(bytes.ToLower(data), []byte("secondary rate limit")))
		retry := res.StatusCode == 429 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504 || secondary
		unavailable := res.StatusCode >= 500 || res.StatusCode == 429 || secondary || (res.StatusCode == 403 && res.Header.Get("X-RateLimit-Remaining") == "0")
		return after, retry, &Error{Code: res.StatusCode, Text: fmt.Sprintf("%s %s: %s: %.300s", method, path, res.Status, data), Unavailable: unavailable}
	}
	if out != nil {
		return 0, false, json.Unmarshal(data, out)
	}
	return 0, false, nil
}

// Error is a refusal or transport failure talking to GitHub.
type Error struct {
	Code        int
	Text        string
	Err         error
	Unavailable bool
}

func (e *Error) Error() string { return e.Text }
func (e *Error) Unwrap() error { return e.Err }

// Unavailable says whether err was caused by GitHub failing or rate-limiting the queue.
func Unavailable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Unavailable
}

func (c *Client) rest(again bool, method, path string, in, out any) error {
	return c.do(again, method, "/repos/"+c.Repo+path, in, out)
}

func (c *Client) graphql(query string, vars map[string]any, out any) error {
	owner, name, _ := strings.Cut(c.Repo, "/")
	if vars == nil {
		vars = map[string]any{}
	}
	vars["owner"], vars["name"] = owner, name
	var reply struct {
		Data   json.RawMessage
		Errors []struct {
			Type    string
			Message string
		}
	}
	if err := c.do(true, "POST", "/graphql", map[string]any{"query": query, "variables": vars}, &reply); err != nil {
		return err
	}
	if len(reply.Errors) > 0 {
		t := reply.Errors[0].Type
		return &Error{
			Text:        "graphql: " + reply.Errors[0].Message,
			Unavailable: t == "RATE_LIMITED" || t == "SERVICE_UNAVAILABLE" || t == "INTERNAL",
		}
	}
	return json.Unmarshal(reply.Data, out)
}

type contextNode struct {
	Typename    string `json:"__typename"`
	Context     string
	State       string
	Description string
	TargetURL   string `json:"targetUrl"`
	CreatedAt   time.Time
	Name        string
	Status      string
	Conclusion  string
	DetailsURL  string `json:"detailsUrl"`
	StartedAt   time.Time
}

// rollup is a commit's statuses and check runs, as GraphQL gives them.
type rollup struct {
	Contexts struct {
		PageInfo struct {
			HasNextPage bool
			EndCursor   string
		}
		Nodes []contextNode
	}
}

const contextFields = `pageInfo { hasNextPage endCursor } nodes { __typename
	... on StatusContext { context state description targetUrl createdAt }
	... on CheckRun { name status conclusion detailsUrl startedAt } }`

const rollupFields = `statusCheckRollup { contexts(first: 100) { ` + contextFields + ` } }`

// statuses is the newest report of each context: a commit status by its context, a check
// run by its name.
func (r *rollup) statuses() map[string]Status {
	out := map[string]Status{}
	if r == nil {
		return out
	}
	for _, n := range r.Contexts.Nodes {
		name, s := n.Context, Status{Description: n.Description, URL: n.TargetURL, At: n.CreatedAt}
		if n.Typename == "CheckRun" {
			name, s = n.Name, Status{URL: n.DetailsURL, At: n.StartedAt, Running: n.Status == "IN_PROGRESS"}
			switch {
			case n.Status != "COMPLETED":
				s.State = queue.Pending
			case n.Conclusion == "SUCCESS" || n.Conclusion == "NEUTRAL" || n.Conclusion == "SKIPPED":
				s.State = queue.Success
			case n.Conclusion == "FAILURE" || n.Conclusion == "ACTION_REQUIRED":
				s.State = queue.Failure
			default:
				s.State = queue.Error
			}
		} else {
			switch n.State {
			case "SUCCESS":
				s.State = queue.Success
			case "FAILURE":
				s.State = queue.Failure
			case "ERROR":
				s.State = queue.Error
			}
		}
		if old, ok := out[name]; !ok || !s.At.Before(old.At) {
			out[name] = s
		}
	}
	return out
}

// Verdict is what the named contexts say together: failure once one failed, success once
// all passed, and no verdict where one ended without one. Beside the verdict is the name
// that keeps it from success.
func Verdict(statuses map[string]Status, contexts []string) (queue.Verdict, string) {
	v, gate := queue.Success, ""
	for _, c := range contexts {
		state := queue.Pending
		if s, ok := statuses[c]; ok {
			state = s.State
		}
		switch {
		case state == queue.Failure:
			return queue.Failure, c
		case state == queue.Error && v != queue.Error:
			v, gate = queue.Error, c
		case state == queue.Pending && v == queue.Success:
			v, gate = queue.Pending, c
		}
	}
	return v, gate
}

// Names are the labels and contexts a repository's queue goes by.
type Names struct {
	Trunk   string
	Label   string   // queues a pull request
	First   string   // puts it first
	Hold    string   // keeps it out
	Status  string   // the queue's own context
	Admit   []string // the contexts a head must pass before it joins the chain
	Threads bool     // whether an unresolved review thread keeps it out
}

// Pull is a pull request the queue knows, open or merged.
type Pull struct {
	queue.PR
	Merged bool
	// Statuses are those of its head.
	Statuses map[string]Status
}

type labelEvent struct {
	CreatedAt time.Time
	Label     struct{ Name string }
}

type pullTimeline struct {
	PageInfo struct {
		HasPreviousPage bool
		StartCursor     string
	}
	Nodes []labelEvent
}

type pullNode struct {
	Number        int
	IsDraft       bool
	Merged        bool
	BaseRefName   string
	HeadRefOid    string
	HeadRefName   string
	Owner         struct{ Login string } `json:"headRepositoryOwner"`
	Labels        struct{ Nodes []struct{ Name string } }
	Timeline      pullTimeline `json:"timelineItems"`
	ReviewThreads struct {
		TotalCount int
		Nodes      []struct{ IsResolved bool }
	}
	Commits struct {
		Nodes []struct {
			Commit struct {
				Oid    string
				Rollup *rollup `json:"statusCheckRollup"`
			}
		}
	}
}

const timelineFields = `pageInfo { hasPreviousPage startCursor } nodes {
	... on LabeledEvent { createdAt label { name } } }`

const pullFields = `number isDraft merged baseRefName headRefOid headRefName headRepositoryOwner { login }
	labels(first: 50) { nodes { name } }
	timelineItems(last: 50, itemTypes: [LABELED_EVENT]) { ` + timelineFields + ` }
	reviewThreads(first: 100) { totalCount nodes { isResolved } }
	commits(last: 1) { nodes { commit { oid ` + rollupFields + ` } } }`

func queuedAt(events []labelEvent, label string) time.Time {
	var at time.Time
	for _, e := range events {
		if e.Label.Name == label && e.CreatedAt.After(at) {
			at = e.CreatedAt
		}
	}
	return at
}

func (n Names) pull(node pullNode) Pull {
	p := Pull{Merged: node.Merged}
	p.Number, p.Head = node.Number, node.HeadRefOid
	if node.Owner.Login != "" && node.HeadRefName != "" {
		p.Branch = node.Owner.Login + "/" + node.HeadRefName
	}
	labels := map[string]bool{}
	for _, l := range node.Labels.Nodes {
		labels[l.Name] = true
	}
	p.First = labels[n.First]
	p.Queued = queuedAt(node.Timeline.Nodes, n.Label)
	for _, c := range node.Commits.Nodes {
		if c.Commit.Oid == p.Head {
			p.Statuses = c.Commit.Rollup.statuses()
		}
	}
	// An ejection ends the queue's status, so only a pending one says where the head stands.
	if said := p.Statuses[n.Status]; said.State == queue.Pending {
		p.Note = said.Description
	}
	p.Checks, p.Gate = Verdict(p.Statuses, n.Admit)
	switch open := 0; {
	case labels[n.Hold]:
		p.Wait = "it is labelled " + n.Hold
	case node.IsDraft:
		p.Wait = "it is a draft"
	case node.BaseRefName != n.Trunk:
		p.Wait = "its base is " + node.BaseRefName + ", not " + n.Trunk
	case n.Threads:
		for _, t := range node.ReviewThreads.Nodes {
			if !t.IsResolved {
				open++
			}
		}
		if open > 0 {
			p.Wait = fmt.Sprintf("%d review threads are unresolved", open)
		} else if node.ReviewThreads.TotalCount > len(node.ReviewThreads.Nodes) {
			// One past the page read may be unresolved, so it waits rather than lands unread.
			p.Wait = "it has more review threads than the queue reads"
		}
	}
	return p
}

func (c *Client) pulls(n Names, nodes []pullNode) ([]Pull, error) {
	var pages []rollupPage
	for i := range nodes {
		for queuedAt(nodes[i].Timeline.Nodes, n.Label).IsZero() && nodes[i].Timeline.PageInfo.HasPreviousPage && nodes[i].Timeline.PageInfo.StartCursor != "" {
			prev := nodes[i].Timeline.PageInfo.StartCursor
			var out struct {
				Repository struct {
					PullRequest struct {
						Timeline pullTimeline `json:"timelineItems"`
					}
				}
			}
			err := c.graphql(`query Timeline($owner: String!, $name: String!, $number: Int!, $before: String!) {
				repository(owner: $owner, name: $name) {
					pullRequest(number: $number) {
						timelineItems(last: 100, before: $before, itemTypes: [LABELED_EVENT]) { `+timelineFields+` } } } }`,
				map[string]any{"number": nodes[i].Number, "before": prev}, &out)
			if err != nil {
				return nil, err
			}
			if nodes[i].Timeline = out.Repository.PullRequest.Timeline; nodes[i].Timeline.PageInfo.StartCursor == prev {
				break
			}
		}
		for _, cm := range nodes[i].Commits.Nodes {
			if cm.Commit.Oid == nodes[i].HeadRefOid && cm.Commit.Rollup != nil && cm.Commit.Rollup.Contexts.PageInfo.HasNextPage {
				pages = append(pages, rollupPage{cm.Commit.Oid, cm.Commit.Rollup})
			}
		}
	}
	if err := c.moreContexts(pages); err != nil {
		return nil, err
	}
	var pulls []Pull
	for _, node := range nodes {
		pulls = append(pulls, n.pull(node))
	}
	return pulls, nil
}

// Queue is the open pull requests labelled for the queue.
func (c *Client) Queue(n Names) ([]Pull, error) {
	var nodes []pullNode
	for after := any(nil); ; {
		var out struct {
			Repository struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []pullNode
				}
			}
		}
		err := c.graphql(`query Queue($owner: String!, $name: String!, $label: String!, $after: String) {
			repository(owner: $owner, name: $name) {
				pullRequests(states: OPEN, labels: [$label], first: 25, after: $after) {
					pageInfo { hasNextPage endCursor } nodes { `+pullFields+` } } } }`,
			map[string]any{"label": n.Label, "after": after}, &out)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, out.Repository.PullRequests.Nodes...)
		if !out.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		after = out.Repository.PullRequests.PageInfo.EndCursor
	}
	pulls, err := c.pulls(n, nodes)
	if err != nil {
		return nil, err
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	return pulls, nil
}

// Landed is the pull requests merged lately that still carry the queue's label.
func (c *Client) Landed(n Names) ([]Pull, error) {
	var out struct {
		Repository struct{ PullRequests struct{ Nodes []pullNode } }
	}
	if err := c.graphql(`query Landed($owner: String!, $name: String!, $label: String!) {
		repository(owner: $owner, name: $name) {
			pullRequests(states: MERGED, labels: [$label], first: 30, orderBy: {field: UPDATED_AT, direction: DESC}) {
				nodes { `+pullFields+` } } } }`, map[string]any{"label": n.Label}, &out); err != nil {
		return nil, err
	}
	return c.pulls(n, out.Repository.PullRequests.Nodes)
}

type rollupPage struct {
	oid string
	r   *rollup
}

func (c *Client) moreContexts(pages []rollupPage) error {
	for len(pages) > 0 {
		batch := pages[:min(len(pages), 40)]
		pages = pages[len(batch):]
		q := "query Contexts($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {"
		for i, p := range batch {
			q += fmt.Sprintf(" c%d: object(oid: %q) { ... on Commit { statusCheckRollup { contexts(first: 100, after: %q) { %s } } } }",
				i, p.oid, p.r.Contexts.PageInfo.EndCursor, contextFields)
		}
		var reply struct {
			Repository map[string]*struct {
				Rollup *rollup `json:"statusCheckRollup"`
			}
		}
		if err := c.graphql(q+" } }", nil, &reply); err != nil {
			return err
		}
		for i, p := range batch {
			node := reply.Repository[fmt.Sprintf("c%d", i)]
			if node == nil || node.Rollup == nil {
				continue
			}
			p.r.Contexts.Nodes = append(p.r.Contexts.Nodes, node.Rollup.Contexts.Nodes...)
			p.r.Contexts.PageInfo = node.Rollup.Contexts.PageInfo
			if p.r.Contexts.PageInfo.HasNextPage {
				pages = append(pages, p)
			}
		}
	}
	return nil
}

// Commits is the statuses of each commit named, by SHA and context.
func (c *Client) Commits(shas []queue.SHA) (map[queue.SHA]map[string]Status, error) {
	out := map[queue.SHA]map[string]Status{}
	for len(shas) > 0 {
		page := shas[:min(len(shas), 40)]
		shas = shas[len(page):]
		q := "query Commits($owner: String!, $name: String!) { repository(owner: $owner, name: $name) {"
		for i, sha := range page {
			q += fmt.Sprintf(" c%d: object(oid: %q) { ... on Commit { %s } }", i, sha, rollupFields)
		}
		var reply struct {
			Repository map[string]*struct {
				Rollup *rollup `json:"statusCheckRollup"`
			}
		}
		if err := c.graphql(q+" } }", nil, &reply); err != nil {
			return nil, err
		}
		var pages []rollupPage
		nodes := make([]*rollup, len(page))
		for i, sha := range page {
			if node := reply.Repository[fmt.Sprintf("c%d", i)]; node != nil && node.Rollup != nil {
				nodes[i] = node.Rollup
				if node.Rollup.Contexts.PageInfo.HasNextPage {
					pages = append(pages, rollupPage{sha, node.Rollup})
				}
			}
		}
		if err := c.moreContexts(pages); err != nil {
			return nil, err
		}
		for i, sha := range page {
			out[sha] = nodes[i].statuses()
		}
	}
	return out, nil
}

// Pushed says whether a commit carries the queue's own status, which every commit the queue
// pushed for CI does. A commit GitHub does not hold was never pushed.
func (c *Client) Pushed(sha queue.SHA, context string) (bool, error) {
	var out struct {
		Repository struct {
			Object struct {
				Status struct {
					Context struct{ Context string }
				}
			}
		}
	}
	err := c.graphql(`query Pushed($owner: String!, $name: String!, $oid: GitObjectID!, $context: String!) {
		repository(owner: $owner, name: $name) {
			object(oid: $oid) { ... on Commit { status { context(name: $context) { context } } } } } }`,
		map[string]any{"oid": sha, "context": context}, &out)
	return err == nil && out.Repository.Object.Status.Context.Context != "", err
}

// SetStatus reports the queue's own context on a commit.
func (c *Client) SetStatus(sha queue.SHA, context string, state queue.Verdict, description, url string) error {
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	body := map[string]string{"context": context, "description": description,
		"state": map[queue.Verdict]string{queue.Pending: "pending", queue.Success: "success", queue.Failure: "failure", queue.Error: "error"}[state]}
	if url != "" {
		body["target_url"] = url
	}
	return c.rest(true, "POST", "/statuses/"+sha, body, nil)
}

// Comment says something on a pull request, once: where a comment made since the given time
// already holds marker, it is not made again.
func (c *Client) Comment(number int, body, marker string, since time.Time) error {
	var comments []struct {
		Body      string
		CreatedAt time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("/issues/%d/comments?per_page=100", number)
	if !since.IsZero() {
		path += "&since=" + since.UTC().Format(time.RFC3339)
	}
	if err := c.rest(true, "GET", path, nil, &comments); err != nil {
		return err
	}
	for _, old := range comments {
		if strings.Contains(old.Body, marker) && !old.CreatedAt.Before(since) {
			return nil
		}
	}
	// Posting a comment twice makes two; a later tick checks for marker first.
	return c.rest(false, "POST", fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body + "\n\n" + marker}, nil)
}

// Unlabel takes a label off a pull request that may not carry it.
func (c *Client) Unlabel(number int, label string) error {
	err := c.rest(true, "DELETE", fmt.Sprintf("/issues/%d/labels/%s", number, label), nil, nil)
	var e *Error
	if errors.As(err, &e) && e.Code == 404 {
		return nil
	}
	return err
}
