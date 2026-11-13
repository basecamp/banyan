// Package github reads the queue from GitHub and writes the queue's statuses, comments and
// labels there. It is the only thing the queue asks of any service: CI's verdicts are read
// as the statuses and check runs CI posts on a commit.
package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, strings.TrimRight(c.API, "/")+path, body)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return &Error{Code: res.StatusCode, Text: fmt.Sprintf("%s %s: %s: %.300s", method, path, res.Status, data)}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Error is a refusal from GitHub.
type Error struct {
	Code int
	Text string
}

func (e *Error) Error() string { return e.Text }

func (c *Client) rest(method, path string, in, out any) error {
	return c.do(method, "/repos/"+c.Repo+path, in, out)
}

func (c *Client) graphql(query string, vars map[string]any, out any) error {
	owner, name, _ := strings.Cut(c.Repo, "/")
	if vars == nil {
		vars = map[string]any{}
	}
	vars["owner"], vars["name"] = owner, name
	var reply struct {
		Data   json.RawMessage
		Errors []struct{ Message string }
	}
	if err := c.do("POST", "/graphql", map[string]any{"query": query, "variables": vars}, &reply); err != nil {
		return err
	}
	if len(reply.Errors) > 0 {
		return fmt.Errorf("graphql: %s", reply.Errors[0].Message)
	}
	return json.Unmarshal(reply.Data, out)
}

// rollup is a commit's statuses and check runs, as GraphQL gives them.
type rollup struct {
	Contexts struct {
		Nodes []struct {
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
	}
}

const rollupFields = `statusCheckRollup { contexts(first: 100) { nodes { __typename
	... on StatusContext { context state description targetUrl createdAt }
	... on CheckRun { name status conclusion detailsUrl startedAt } } } }`

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

// Pull is a pull request the queue knows, open or not.
type Pull struct {
	queue.PR
	Merged bool
	Closed bool
	// Statuses are those of its head.
	Statuses map[string]Status
}

type pullNode struct {
	Number      int
	IsDraft     bool
	Merged      bool
	Closed      bool
	BaseRefName string
	HeadRefOid  string
	HeadRefName string
	Owner       struct{ Login string } `json:"headRepositoryOwner"`
	Labels      struct{ Nodes []struct{ Name string } }
	Timeline    struct {
		Nodes []struct {
			CreatedAt time.Time
			Label     struct{ Name string }
		}
	} `json:"timelineItems"`
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

const pullFields = `number isDraft merged closed baseRefName headRefOid headRefName headRepositoryOwner { login }
	labels(first: 50) { nodes { name } }
	timelineItems(last: 50, itemTypes: [LABELED_EVENT]) { nodes {
		... on LabeledEvent { createdAt label { name } } } }
	reviewThreads(first: 100) { totalCount nodes { isResolved } }
	commits(last: 1) { nodes { commit { oid ` + rollupFields + ` } } }`

func (n Names) pull(node pullNode) Pull {
	p := Pull{Merged: node.Merged, Closed: node.Closed}
	p.Number, p.Head = node.Number, node.HeadRefOid
	if node.Owner.Login != "" && node.HeadRefName != "" {
		p.Branch = node.Owner.Login + "/" + node.HeadRefName
	}
	labels := map[string]bool{}
	for _, l := range node.Labels.Nodes {
		labels[l.Name] = true
	}
	p.First = labels[n.First]
	for _, e := range node.Timeline.Nodes {
		if e.Label.Name == n.Label && e.CreatedAt.After(p.Queued) {
			p.Queued = e.CreatedAt
		}
	}
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

// Queue is the open pull requests labelled for the queue.
func (c *Client) Queue(n Names) ([]Pull, error) {
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
	var pulls []Pull
	for after := any(nil); ; {
		err := c.graphql(`query Queue($owner: String!, $name: String!, $label: String!, $after: String) {
			repository(owner: $owner, name: $name) {
				pullRequests(states: OPEN, labels: [$label], first: 25, after: $after) {
					pageInfo { hasNextPage endCursor } nodes { `+pullFields+` } } } }`,
			map[string]any{"label": n.Label, "after": after}, &out)
		if err != nil {
			return nil, err
		}
		for _, node := range out.Repository.PullRequests.Nodes {
			pulls = append(pulls, n.pull(node))
		}
		if !out.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		after = out.Repository.PullRequests.PageInfo.EndCursor
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	return pulls, nil
}

// Pull is one pull request, open or not.
func (c *Client) Pull(n Names, number int) (Pull, error) {
	var out struct {
		Repository struct{ PullRequest *pullNode }
	}
	err := c.graphql(`query Pull($owner: String!, $name: String!, $number: Int!) {
		repository(owner: $owner, name: $name) { pullRequest(number: $number) { `+pullFields+` } } }`,
		map[string]any{"number": number}, &out)
	if err != nil {
		return Pull{}, err
	}
	if out.Repository.PullRequest == nil {
		return Pull{}, fmt.Errorf("#%d is no pull request of %s", number, c.Repo)
	}
	return n.pull(*out.Repository.PullRequest), nil
}

// Landed is the pull requests merged lately that still carry the queue's label.
func (c *Client) Landed(n Names) ([]Pull, error) {
	var out struct {
		Repository struct{ PullRequests struct{ Nodes []pullNode } }
	}
	err := c.graphql(`query Landed($owner: String!, $name: String!, $label: String!) {
		repository(owner: $owner, name: $name) {
			pullRequests(states: MERGED, labels: [$label], first: 30, orderBy: {field: UPDATED_AT, direction: DESC}) {
				nodes { `+pullFields+` } } } }`, map[string]any{"label": n.Label}, &out)
	var pulls []Pull
	for _, node := range out.Repository.PullRequests.Nodes {
		pulls = append(pulls, n.pull(node))
	}
	return pulls, err
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
		for i, sha := range page {
			if node := reply.Repository[fmt.Sprintf("c%d", i)]; node != nil {
				out[sha] = node.Rollup.statuses()
			} else {
				out[sha] = map[string]Status{}
			}
		}
	}
	return out, nil
}

// Pushed says whether a commit carries the queue's own status, which every commit the queue
// pushed for CI does. A commit GitHub does not hold was never pushed.
func (c *Client) Pushed(sha queue.SHA, context string) (bool, error) {
	var statuses []struct{ Context string }
	err := c.rest("GET", "/commits/"+sha+"/statuses?per_page=100", nil, &statuses)
	if e, ok := err.(*Error); ok && (e.Code == 404 || e.Code == 422) {
		return false, nil
	}
	for _, s := range statuses {
		if s.Context == context {
			return true, nil
		}
	}
	return false, err
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
	return c.rest("POST", "/statuses/"+sha, body, nil)
}

// Comment says something on a pull request, once: where a comment made since the given time
// already holds marker, it is not made again.
func (c *Client) Comment(number int, body, marker string, since time.Time) error {
	var comments []struct {
		Body      string
		CreatedAt time.Time `json:"created_at"`
	}
	if err := c.rest("GET", fmt.Sprintf("/issues/%d/comments?per_page=100&since=%s", number, since.UTC().Format(time.RFC3339)), nil, &comments); err != nil {
		return err
	}
	for _, old := range comments {
		if strings.Contains(old.Body, marker) && !old.CreatedAt.Before(since) {
			return nil
		}
	}
	return c.rest("POST", fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body + "\n\n" + marker}, nil)
}

// Label puts a label on a pull request.
func (c *Client) Label(number int, label string) error {
	return c.rest("POST", fmt.Sprintf("/issues/%d/labels", number), map[string][]string{"labels": {label}}, nil)
}

// Unlabel takes a label off a pull request that may not carry it.
func (c *Client) Unlabel(number int, label string) error {
	err := c.rest("DELETE", fmt.Sprintf("/issues/%d/labels/%s", number, label), nil, nil)
	if e, ok := err.(*Error); ok && e.Code == 404 {
		return nil
	}
	return err
}
