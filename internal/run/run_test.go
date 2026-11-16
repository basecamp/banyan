package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/github"
	"github.com/basecamp/banyan/internal/gittest"
	"github.com/basecamp/banyan/internal/queue"
)

// hub is a GitHub that holds pull requests, labels, comments and statuses, and answers the
// few questions the queue asks. Its git is the test's origin.
type hub struct {
	t        *testing.T
	o        *gittest.Origin
	now      time.Time
	pulls    map[int]*pull
	statuses map[string][]status
	// listing, where set, runs before each listing of the queue, with how many came before.
	listing  func(n int)
	listings int
	// left, where set, is what remains of each hourly quota, which every reply then names.
	left         map[string]int
	contextPages int
	intercept    func(http.ResponseWriter, *http.Request) bool
	lagUnlabel   bool
}

type pull struct {
	number    int
	title     string
	body      string
	heads     []string
	labels    map[string]time.Time
	events    []labelEvent
	draft     bool
	base      string
	comments  []comment
	merged    bool
	crossRepo bool
	// threads is its review threads, true where resolved, and unread is how many more GitHub
	// holds past the page it returns.
	threads []bool
	unread  int
}

type labelEvent struct {
	name string
	at   time.Time
}

type comment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type status struct {
	Context, State, Description, URL string
	At                               time.Time
	Check                            bool
	App                              int
}

func (h *hub) open(n int, file, content string) *pull {
	p := &pull{number: n, heads: []string{h.o.PR(n, file, content)}, labels: map[string]time.Time{}, base: "main"}
	h.pulls[n] = p
	return p
}

func (h *hub) set(sha, context, state string) {
	h.statuses[sha] = append(h.statuses[sha], status{Context: context, State: state, URL: "https://ci.example/" + sha[:12], At: h.now})
}

func (h *hub) check(sha, name, state string, at time.Time, app ...int) {
	var id int
	if len(app) > 0 {
		id = app[0]
	}
	h.statuses[sha] = append(h.statuses[sha], status{Context: name, State: state, URL: "https://ci.example/" + sha[:12], At: at, Check: true, App: id})
}

// last is the newest status of a context on a commit.
func (h *hub) last(sha, context string) status {
	var out status
	for _, s := range h.statuses[sha] {
		if s.Context == context {
			out = s
		}
	}
	return out
}

func (h *hub) node(p *pull) map[string]any {
	var labels, commits []any
	threads := []any{}
	for _, resolved := range p.threads {
		threads = append(threads, map[string]any{"isResolved": resolved})
	}
	for name := range p.labels {
		labels = append(labels, map[string]any{"name": name})
	}
	for _, sha := range p.heads {
		commits = append(commits, map[string]any{"commit": map[string]any{"oid": sha, "statusCheckRollup": h.rollup(sha, "", 100)}})
	}
	return map[string]any{"number": p.number, "title": p.title, "body": p.body, "isDraft": p.draft, "merged": p.merged, "closed": p.merged, "baseRefName": p.base, "isCrossRepository": p.crossRepo,
		"headRefName": fmt.Sprintf("pr%d", p.number), "headRepositoryOwner": map[string]any{"login": "o"},
		"headRefOid": p.heads[len(p.heads)-1], "labels": map[string]any{"nodes": labels}, "timelineItems": h.timeline(p, 50, ""),
		"reviewThreads": map[string]any{"totalCount": len(p.threads) + p.unread, "nodes": threads}, "commits": map[string]any{"nodes": commits}}
}

func (h *hub) timeline(p *pull, last int, before string) map[string]any {
	events := slices.Clone(p.events)
	if len(events) == 0 {
		for name, at := range p.labels {
			events = append(events, labelEvent{name, at})
		}
		sort.Slice(events, func(i, j int) bool {
			if !events[i].at.Equal(events[j].at) {
				return events[i].at.Before(events[j].at)
			}
			return events[i].name < events[j].name
		})
	}
	end := len(events)
	if before != "" {
		end, _ = strconv.Atoi(before)
	}
	start := max(end-last, 0)
	var nodes []any
	for _, e := range events[start:end] {
		nodes = append(nodes, map[string]any{"createdAt": e.at, "label": map[string]any{"name": e.name}})
	}
	return map[string]any{
		"pageInfo": map[string]any{"hasPreviousPage": start > 0, "startCursor": strconv.Itoa(start)},
		"nodes":    nodes,
	}
}

func (h *hub) rollup(sha, after string, first int) any {
	type key struct {
		context string
		check   bool
		app     int
	}
	var order []key
	newest := map[key]status{}
	for _, s := range h.statuses[sha] {
		k := key{s.Context, s.Check, s.App}
		if _, seen := newest[k]; !seen {
			order = append(order, k)
		}
		newest[k] = s
	}
	if len(order) == 0 {
		return nil
	}
	start, _ := strconv.Atoi(after)
	end := min(start+first, len(order))
	var nodes []any
	for _, k := range order[start:end] {
		s := newest[k]
		if s.Check {
			node := map[string]any{"__typename": "CheckRun", "name": s.Context, "status": strings.ToUpper(s.State), "detailsUrl": s.URL, "startedAt": s.At,
				"checkSuite": map[string]any{"app": map[string]any{"databaseId": s.App}}}
			if s.State != "queued" && s.State != "in_progress" {
				node["status"], node["conclusion"] = "COMPLETED", strings.ToUpper(s.State)
			}
			nodes = append(nodes, node)
			continue
		}
		nodes = append(nodes, map[string]any{"__typename": "StatusContext", "context": s.Context, "state": strings.ToUpper(s.State),
			"description": s.Description, "targetUrl": s.URL, "createdAt": s.At})
	}
	return map[string]any{"contexts": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": end < len(order), "endCursor": strconv.Itoa(end)},
		"nodes":    nodes,
	}}
}

var commitAlias = regexp.MustCompile(`(c\d+): object\(oid: "([0-9a-f]+)"\)[^{]*\{\s*\.\.\. on Commit \{\s*statusCheckRollup \{\s*contexts\(first: (\d+)(?:, after: "([^"]*)")?\)`)

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.intercept != nil && h.intercept(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	if quota := map[bool]string{true: "graphql", false: "core"}[r.URL.Path == "/graphql"]; h.left != nil {
		h.left[quota]--
		w.Header().Set("X-RateLimit-Resource", quota)
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(h.left[quota]))
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/o/r/"), "/")
	switch {
	case r.URL.Path == "/graphql":
		var q struct {
			Query     string
			Variables map[string]any
		}
		json.Unmarshal(body, &q)
		repo := map[string]any{}
		listed := func(merged bool) any {
			var nodes []any
			for _, n := range h.numbers() {
				p := h.pulls[n]
				if _, labelled := p.labels[q.Variables["label"].(string)]; labelled && p.merged == merged {
					nodes = append(nodes, h.node(p))
				}
			}
			return map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}
		}
		switch {
		case strings.Contains(q.Query, "query Queue"):
			if h.listing != nil {
				h.listing(h.listings)
			}
			h.listings++
			repo["pullRequests"] = listed(false)
		case strings.Contains(q.Query, "query Landed"):
			repo["pullRequests"] = listed(true)
		case strings.Contains(q.Query, "query Timeline"):
			if p := h.pulls[int(q.Variables["number"].(float64))]; p != nil {
				before, _ := q.Variables["before"].(string)
				repo["pullRequest"] = map[string]any{"timelineItems": h.timeline(p, 100, before)}
			}
		case strings.Contains(q.Query, "query Pushed"):
			sha, _ := q.Variables["oid"].(string)
			ctx, _ := q.Variables["context"].(string)
			if _, err := gittest.Try(h.o.Bare, "git", "cat-file", "-e", sha); err == nil {
				if s := h.last(sha, ctx); s.Context != "" {
					repo["object"] = map[string]any{"status": map[string]any{"context": map[string]any{"context": s.Context}}}
				} else {
					repo["object"] = map[string]any{}
				}
			}
		case strings.Contains(q.Query, "query Pull"):
			if p := h.pulls[int(q.Variables["number"].(float64))]; p != nil {
				repo["pullRequest"] = h.node(p)
			}
		case strings.Contains(q.Query, "query Contexts"):
			h.contextPages++
			fallthrough
		default:
			for _, m := range commitAlias.FindAllStringSubmatch(q.Query, -1) {
				if _, err := gittest.Try(h.o.Bare, "git", "cat-file", "-e", m[2]); err == nil {
					first, _ := strconv.Atoi(m[3])
					repo[m[1]] = map[string]any{"statusCheckRollup": h.rollup(m[2], m[4], first)}
				}
			}
		}
		reply(map[string]any{"data": map[string]any{"repository": repo}})
	case parts[0] == "statuses" && r.Method == "POST":
		var s struct{ Context, State, Description, Target_url string }
		json.Unmarshal(body, &s)
		h.statuses[parts[1]] = append(h.statuses[parts[1]], status{Context: s.Context, State: s.State, Description: s.Description, URL: s.Target_url, At: h.now})
		reply(map[string]any{})
	case parts[0] == "issues":
		n, _ := strconv.Atoi(parts[1])
		p := h.pulls[n]
		switch {
		case parts[2] == "comments" && r.Method == "GET":
			reply(append([]comment{}, p.comments...))
		case parts[2] == "comments":
			var c comment
			json.Unmarshal(body, &c)
			c.CreatedAt = h.now
			p.comments = append(p.comments, c)
			reply(c)
		case r.Method == "POST":
			var l struct{ Labels []string }
			json.Unmarshal(body, &l)
			for _, name := range l.Labels {
				if _, has := p.labels[name]; !has {
					p.labels[name] = h.now
					p.events = append(p.events, labelEvent{name, h.now})
				}
			}
			reply([]any{})
		case r.Method == "DELETE":
			if !h.lagUnlabel {
				delete(p.labels, parts[3])
			}
			reply([]any{})
		}
	default:
		h.t.Errorf("the queue asked GitHub for %s %s, which this one does not answer", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}
}

func (h *hub) numbers() []int {
	var ns []int
	for n := range h.pulls {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	return ns
}

// world is a queue, its GitHub and its CI, which judges a commit by what its tree holds.
type world struct {
	*hub
	q   *Queue
	log *bytes.Buffer
	// red says whether a whole build of the commit fails.
	red func(sha string) bool
	// slow says CI has yet to answer for the commit.
	slow func(sha string) bool
	// built is every commit CI judged, in order.
	built  []string
	waited []time.Duration
}

func newWorld(t *testing.T, config string) *world {
	o := gittest.New(t)
	h := &hub{t: t, o: o, now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), pulls: map[int]*pull{}, statuses: map[string][]status{}}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	path := o.Clone + "/merge-queue.json"
	os.WriteFile(path, []byte(config), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Repo = "o/r"
	w := &world{hub: h, log: &bytes.Buffer{}, red: func(string) bool { return false }}
	w.q = New(c, o.Clone, srv.URL, "token")
	w.q.Now, w.q.Log = func() time.Time { return h.now }, w.log
	w.q.Hub.Wait = func(d time.Duration) { w.waited = append(w.waited, d) }
	return w
}

// queue opens a pull request that writes a file, and queues it.
func (w *world) queue(n int, file, content string) *pull {
	p := w.open(n, file, content)
	w.now = w.now.Add(time.Second)
	w.label(n)
	return p
}

// label queues a pull request as a person does, by labelling it.
func (w *world) label(n int) {
	w.pulls[n].labels["merge-queue"] = w.now
	w.pulls[n].events = append(w.pulls[n].events, labelEvent{"merge-queue", w.now})
}

// ci judges every commit on the queue's branches that it has not judged.
func (w *world) ci() {
	branches := w.o.Branches("merge-queue/")
	var names []string
	for name := range branches {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sha := branches[name]
		context := "merge-queue/build"
		if strings.Contains(gittest.Sh(w.t, w.o.Bare, "git", "show", "--no-patch", "--format=%B", sha), "Banyan-Probe-Of") {
			context = "merge-queue/probe"
		}
		last := w.last(sha, context).State
		if last == "success" || last == "failure" {
			continue
		}
		// A build CI has begun and not answered says so, as Buildkite does.
		if w.slow != nil && w.slow(sha) {
			if last == "" {
				w.statuses[sha] = append(w.statuses[sha], status{Context: context, State: "pending", Description: "Build started", URL: "https://ci.example/" + sha[:12], At: w.now})
			}
			continue
		}
		state := "success"
		if w.red(sha) {
			state = "failure"
		}
		w.built = append(w.built, sha)
		w.set(sha, context, state)
	}
}

// round is a tick, GitHub noticing what landed, and CI answering what was pushed.
func (w *world) round() {
	w.t.Helper()
	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err != nil {
		w.t.Fatalf("tick: %v\n%s", err, w.log)
	}
	for _, p := range w.pulls {
		p.merged = p.merged || w.o.Landed(p.heads[len(p.heads)-1])
	}
	w.ci()
}

func (w *world) rounds(n int) {
	w.t.Helper()
	for i := 0; i < n; i++ {
		w.round()
	}
	if os.Getenv("BANYAN_SHOW") != "" {
		w.t.Logf("after %d rounds, CI built %d commits:\n%s", n, len(w.built), w.log)
	}
}

func (w *world) landed(n int) bool { return w.pulls[n].merged }

func (w *world) queued(n int) bool {
	_, in := w.pulls[n].labels["merge-queue"]
	return in && !w.pulls[n].merged
}

func (w *world) said(n int, text string) bool {
	for _, c := range w.pulls[n].comments {
		if strings.Contains(c.Body, text) {
			return true
		}
	}
	return false
}

func TestGreenPullRequestsLandInOrder(t *testing.T) {
	w := newWorld(t, `{"slots": 3}`)
	a, b, c := w.queue(1, "a", "1\n"), w.queue(2, "b", "2\n"), w.queue(3, "c", "3\n")
	w.rounds(3)
	for _, p := range []*pull{a, b, c} {
		if !w.landed(p.number) {
			t.Errorf("#%d did not land\n%s", p.number, w.log)
		}
		if s := w.last(p.heads[0], "merge-queue"); s.State != "success" || !strings.HasPrefix(s.Description, "landed as ") {
			t.Errorf("#%d's status is %s %q", p.number, s.State, s.Description)
		}
	}
	trunk := w.o.Trunk()
	if !w.o.Holds(trunk, "a") || !w.o.Holds(trunk, "b") || !w.o.Holds(trunk, "c") {
		t.Error("the trunk does not hold all three")
	}
	if w.last(trunk, "merge-queue/build").State != "success" {
		t.Error("the trunk's tip is a commit CI did not pass")
	}
	if subject := gittest.Sh(t, w.o.Bare, "git", "log", "-1", "--format=%s", trunk); subject != "Merge pull request #3 from o/pr3" {
		t.Errorf("the trunk's tip is titled %q, not as GitHub titles a merge", subject)
	}
	if left := w.o.Branches("merge-queue/"); len(left) != 0 {
		t.Errorf("branches left behind: %v", left)
	}
	if len(w.built) != 3 {
		t.Errorf("CI built %d commits for three landings", len(w.built))
	}
}

func TestTheLaterOfTwoThatClashLeavesAndTheRestLand(t *testing.T) {
	w := newWorld(t, `{"slots": 3}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "caller") && w.o.Holds(sha, "signature") }
	w.queue(1, "signature", "changed\n")
	two := w.queue(2, "caller", "old call\n")
	w.queue(3, "c", "3\n")
	w.rounds(8)
	if !w.landed(1) || !w.landed(3) {
		t.Errorf("#1 landed %v, #3 landed %v\n%s", w.landed(1), w.landed(3), w.log)
	}
	if w.landed(2) || w.queued(2) {
		t.Errorf("#2 clashes with #1 and is landed %v, queued %v", w.landed(2), w.queued(2))
	}
	if !w.said(2, "<!-- merge-queue eject "+two.heads[0]+" attributed=culprit -->") || !w.said(2, "passed without it: https://ci.example/") {
		t.Errorf("#2 was not told why: %+v", two.comments)
	}
	if s := w.last(two.heads[0], "merge-queue"); s.State != "failure" {
		t.Errorf("#2's status is %s %q", s.State, s.Description)
	}
	if len(w.pulls[1].comments)+len(w.pulls[3].comments) != 0 {
		t.Error("the queue commented on a pull request it landed")
	}
	if w.o.Holds(w.o.Trunk(), "caller") {
		t.Error("the trunk holds the pull request that broke it")
	}
}

func TestABuildThatFailsOnceIsBuiltAgainAndLands(t *testing.T) {
	w := newWorld(t, `{"slots": 2}`)
	failed := false
	w.red = func(sha string) bool {
		flake := !failed && w.o.Holds(sha, "a")
		failed = failed || flake
		return flake
	}
	w.queue(1, "a", "1\n")
	w.rounds(4)
	if !w.landed(1) || len(w.pulls[1].comments) != 0 {
		t.Errorf("#1 landed %v with comments %+v\n%s", w.landed(1), w.pulls[1].comments, w.log)
	}
	seen := map[string]bool{}
	for _, sha := range w.built {
		if seen[sha] {
			t.Errorf("%s was built twice", sha)
		}
		seen[sha] = true
	}
}

func TestARedTrunkEjectsNobody(t *testing.T) {
	w := newWorld(t, `{"slots": 2, "recheck": "4m"}`)
	broken := true
	w.red = func(string) bool { return broken }
	a, b := w.queue(1, "a", "1\n"), w.queue(2, "b", "2\n")
	w.rounds(10)
	if !w.queued(1) || !w.queued(2) || len(w.pulls[1].comments)+len(w.pulls[2].comments) != 0 {
		t.Fatalf("a red trunk took a pull request out\n%s", w.log)
	}
	for _, p := range []*pull{a, b} {
		if s := w.last(p.heads[0], "merge-queue"); s.Description != "waits: the trunk itself is failing, and the queue holds until it passes" {
			t.Errorf("the trunk is red and a waiting pull request's status says %q", s.Description)
		}
	}
	broken = false
	w.rounds(8)
	if !w.landed(1) || !w.landed(2) {
		t.Errorf("after the trunk passed again #1 landed %v and #2 landed %v\n%s", w.landed(1), w.landed(2), w.log)
	}
}

func TestAConflictWithTheTrunkWaitsInPlaceUntilItIsMendedAndHoldsNobodyUp(t *testing.T) {
	w := newWorld(t, `{}`)
	one := w.queue(1, "same", "one\n")
	w.o.Outside("same", "other\n")
	w.rounds(2)
	if s := w.last(one.heads[0], "merge-queue"); !w.queued(1) || len(one.comments) != 0 || s.State != "pending" || s.Description != "waits: it conflicts with the trunk" {
		t.Fatalf("#1 conflicts with the trunk: queued %v, comments %+v, status %q %q\n%s", w.queued(1), one.comments, s.State, s.Description, w.log)
	}
	said := len(w.statuses[one.heads[0]])
	w.queue(2, "b", "two\n")
	w.rounds(3)
	if !w.landed(2) || !w.queued(1) || len(w.statuses[one.heads[0]]) != said {
		t.Fatalf("with #1 left in conflict, #2 landed %v, #1 is queued %v and its head was given %d more statuses\n%s",
			w.landed(2), w.queued(1), len(w.statuses[one.heads[0]])-said, w.log)
	}
	one.heads = append(one.heads, w.o.PR(1, "same", "one on the other\n"))
	w.rounds(3)
	if !w.landed(1) {
		t.Errorf("#1 was mended and did not land\n%s", w.log)
	}
}

func TestAPushToAQueuedPullRequestLandsItsNewHeadOnceThatHeadIsAdmitted(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff"]}`)
	p := w.open(1, "a", "1\n")
	w.set(p.heads[0], "signoff", "success")
	w.label(1)
	w.round() // built at its first head, and CI passed the build
	before := w.o.Trunk()
	p.heads = append(p.heads, w.o.Amend(1, "a", "1 again\n"))
	w.rounds(3)
	if !w.queued(1) || w.o.Trunk() != before || len(p.comments) != 0 {
		t.Fatalf("a push to #1: queued %v, trunk moved %v, comments %+v\n%s", w.queued(1), w.o.Trunk() != before, p.comments, w.log)
	}
	if s := w.last(p.heads[1], "merge-queue"); s.State != "pending" || s.Description != "waits: signoff has not passed" {
		t.Errorf("the status on #1's new head says %q, %q", s.State, s.Description)
	}
	w.set(p.heads[1], "signoff", "success")
	w.rounds(3)
	if s := w.last(p.heads[1], "merge-queue"); !w.landed(1) || s.State != "success" || !strings.HasPrefix(s.Description, "landed as ") {
		t.Errorf("#1 did not land at its new head once that head was signed off\n%s", w.log)
	}
}

func TestReadWhenThePullRefDisagreesWithTheAPIHead(t *testing.T) {
	for _, shape := range []string{"behind", "behind landed parent", "beside", "push arriving", "force-push arriving", "three different heads", "branch gone", "another repository", "heads agree branch gone", "heads agree another repository", "branch unreachable"} {
		t.Run(shape, func(t *testing.T) {
			w := newWorld(t, `{"admit": ["signoff"]}`)
			p := w.queue(1, "a", "one\n")
			parent := p.heads[0]
			head := w.o.Amend(1, "a", "two\n")
			branch := head
			wait := ""
			switch shape {
			case "behind", "behind landed parent":
				gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/pull/1/head", parent)
				if shape == "behind landed parent" {
					gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/heads/main", parent)
				}
			case "beside":
				head = w.o.PR(1, "a", "other\n")
				branch = head
				gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/pull/1/head", parent)
			case "push arriving":
				head = parent
				wait = "its newest push is still arriving"
			case "force-push arriving":
				branch = w.o.PR(1, "a", "other\n")
				head = parent
				wait = "its newest push is still arriving"
			case "three different heads":
				branch = w.o.PR(1, "a", "other\n")
				gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/pull/1/head", parent)
				wait = "its newest push is still arriving"
			case "branch gone", "another repository":
				gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/pull/1/head", parent)
				p.crossRepo = shape == "another repository"
				wait = "its newest push is still arriving"
			case "heads agree another repository":
				p.crossRepo = true
			case "branch unreachable":
				gittest.Sh(t, w.o.Bare, "git", "update-ref", "refs/pull/1/head", parent)
			}
			gittest.Sh(t, w.o.Work, "git", "push", "--quiet", "--force", "origin", branch+":refs/heads/pr1")
			if shape == "branch gone" || shape == "heads agree branch gone" || shape == "heads agree another repository" {
				gittest.Sh(t, w.o.Work, "git", "push", "--quiet", "origin", ":refs/heads/pr1")
			}
			p.heads = []string{head}
			w.set(head, "signoff", "success")
			if shape == "branch unreachable" {
				// The pull refs arrive, then the remote is unreachable for the branch's fetch.
				hook := filepath.Join(w.o.Clone, ".git", "hooks", "reference-transaction")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\nif test \"$1\" = committed; then\n  git config remote.origin.url ../unreachable.git\nfi\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			r, err := w.q.Read()
			if shape == "branch unreachable" {
				if err == nil || !strings.Contains(err.Error(), "+refs/heads/pr1:refs/banyan/head") {
					t.Fatalf("read error = %v, want the head branch's fetch error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(r.World.PRs) != 1 {
				t.Fatalf("read %d pull requests, want one at the API head", len(r.World.PRs))
			}
			if pr := r.World.PRs[0]; pr.Wait != wait || pr.Head != head {
				t.Fatalf("read head %s, wait %q; want head %s, wait %q", pr.Head, pr.Wait, head, wait)
			}
			if err := w.q.Tick(); err != nil {
				t.Fatal(err)
			}
			branches := w.o.Branches("merge-queue/")
			if wait != "" {
				if len(branches) != 0 || w.last(head, "merge-queue").Description != "waits: "+wait {
					t.Fatalf("waiting pull request: branches %v, status %+v", branches, w.last(head, "merge-queue"))
				}
				return
			}
			if len(branches) != 1 {
				t.Fatalf("got %d candidates, want one", len(branches))
			}
			for _, sha := range branches {
				if !w.q.Git.Holds(sha, head) {
					t.Fatalf("candidate %s does not hold the API head %s", sha, head)
				}
			}
			if shape == "behind" || shape == "beside" {
				w.ci()
				if err := w.q.Tick(); err != nil {
					t.Fatal(err)
				}
				landed := w.last(head, "merge-queue")
				if !w.o.Landed(head) || p.merged || landed.State != "success" || !strings.HasPrefix(landed.Description, "landed as ") {
					t.Fatalf("landing with the pull request still open: landed %v, merged %v, status %+v\n%s", w.o.Landed(head), p.merged, landed, w.log)
				}
				gittest.Sh(t, w.o.Work, "git", "push", "--quiet", "origin", ":refs/heads/pr1")
				trunk := w.o.Trunk()
				pushed := strings.Count(w.log.String(), `"do":"push"`)
				if err := w.q.Tick(); err != nil {
					t.Fatal(err)
				}
				if got := w.last(head, "merge-queue"); got != landed {
					t.Errorf("after the head branch was deleted, status %+v, want %+v", got, landed)
				}
				if left := w.o.Branches("merge-queue/"); len(left) != 0 || w.o.Trunk() != trunk || strings.Count(w.log.String(), `"do":"push"`) != pushed {
					t.Errorf("after the head branch was deleted, new work was pushed: branches %v\n%s", left, w.log)
				}
			}
		})
	}
}

func TestAForcePushToAQueuedPullRequestLandsItsNewHead(t *testing.T) {
	w := newWorld(t, `{}`)
	p := w.queue(1, "a", "1\n")
	w.round()
	// The old head, and the queue's status on it, are no longer among its commits.
	p.heads = []string{w.o.Amend(1, "a", "1 again\n")}
	w.rounds(3)
	if !w.landed(1) || len(p.comments) != 0 {
		t.Errorf("landed %v, comments %+v\n%s", w.landed(1), p.comments, w.log)
	}
}

func TestALandingWhoseTicksDiedBeforeSayingAnythingIsSaidByTheNext(t *testing.T) {
	w := newWorld(t, `{}`)
	p := w.open(1, "a", "1\n")
	p.labels["merge-queue"] = w.now
	w.rounds(2)
	if !w.landed(1) {
		t.Fatalf("#1, labelled by hand, did not land\n%s", w.log)
	}
	// The tick that built it died before its note, and the one that landed it before its status.
	delete(w.statuses, p.heads[0])
	w.round()
	if s := w.last(p.heads[0], "merge-queue"); s.State != "success" || s.Description != "landed" {
		t.Errorf("the status on #1's landed head says %q, %q", s.State, s.Description)
	}
	said := len(w.statuses[p.heads[0]])
	w.round()
	if len(w.statuses[p.heads[0]]) != said {
		t.Errorf("a landing already said was said again")
	}
}

func TestAQueueThatCannotPushSaysSoOnWhatWaits(t *testing.T) {
	w := newWorld(t, `{}`)
	p := w.queue(1, "a", "1\n")
	// Git can't hold a branch named as the queue's own are prefixed, so every push is refused.
	gittest.Sh(t, w.o.Bare, "git", "branch", "merge-queue", "main")
	for i := 0; i < 2; i++ {
		w.now = w.now.Add(2 * time.Minute)
		if err := w.q.Tick(); err == nil {
			t.Fatalf("the tick pushed past a branch named merge-queue\n%s", w.log)
		}
	}
	said := 0
	for _, s := range w.statuses[p.heads[0]] {
		if s.Context == "merge-queue" && strings.HasPrefix(s.Description, "the queue is stopped: ") {
			said++
		}
	}
	if s := w.last(p.heads[0], "merge-queue"); said != 1 || s.State != "pending" || len(s.Description) > 140 {
		t.Errorf("after two refused ticks #1's status is %q %q, said %d times", s.State, s.Description, said)
	}
	gittest.Sh(t, w.o.Bare, "git", "branch", "-D", "merge-queue")
	w.rounds(3)
	if !w.landed(1) {
		t.Errorf("#1 never landed once the branch was gone\n%s", w.log)
	}
}

func TestALandingTheTrunkKeepsRefusingStopsTheQueueAndSaysWhy(t *testing.T) {
	w := newWorld(t, `{}`)
	p := w.queue(1, "a", "1\n")
	w.round() // pushed, and CI passed it
	// As GitHub refuses a push its branch protection does not take.
	hook := filepath.Join(w.o.Bare, "hooks", "update")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte(`#!/bin/sh
[ "$1" = refs/heads/main ] || exit 0
echo 'error: GH013: Repository rule violations found for refs/heads/main.' >&2
echo '- Required status check "signoff" is expected.' >&2
exit 1
`), 0o755)
	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err == nil {
		t.Fatalf("the tick ended well though the trunk refused its landing\n%s", w.log)
	}
	if n := strings.Count(w.log.String(), `{"refused"`); n != 2 {
		t.Errorf("the tick tried the refused landing %d times, where twice tells a refusal from a race\n%s", n, w.log)
	}
	if s := w.last(p.heads[0], "merge-queue"); s.State != "pending" || s.Description != `the queue is stopped: land: Required status check "signoff" is expected.` {
		t.Errorf("#1's status is %q %q", s.State, s.Description)
	}
	os.Remove(hook)
	w.rounds(2)
	if !w.landed(1) {
		t.Errorf("#1 never landed once the trunk took the push\n%s", w.log)
	}
}

func TestAPullRequestWhoseLabelGoesDoesNotLand(t *testing.T) {
	w := newWorld(t, `{}`)
	w.queue(1, "a", "1\n")
	w.round() // pushed, and CI passed it
	before := w.o.Trunk()
	delete(w.pulls[1].labels, "merge-queue")
	w.rounds(2)
	if w.landed(1) || w.o.Trunk() != before || len(w.o.Branches("merge-queue/")) != 0 {
		t.Errorf("#1 lost its label and landed %v; the queue's branches are %v\n%s", w.landed(1), w.o.Branches("merge-queue/"), w.log)
	}
}

func TestAReviewWithdrawnAsTheLandingBeginsStopsIt(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff", "review"]}`)
	p := w.open(1, "a", "1\n")
	w.set(p.heads[0], "signoff", "success")
	w.set(p.heads[0], "review", "success")
	w.label(1)
	w.round() // pushed, and CI passed it
	before := w.o.Trunk()
	if w.landed(1) {
		t.Fatal("#1 landed before the tick under test")
	}
	// The next tick reads the queue and plans the landing; the landing reads it again.
	from := w.listings
	w.listing = func(n int) {
		if n == from+1 {
			w.set(p.heads[0], "review", "failure")
		}
	}
	w.round()
	if w.landed(1) || w.o.Trunk() != before {
		t.Errorf("#1 landed though its review failed before the trunk moved\n%s", w.log)
	}
	w.listing = nil
	w.set(p.heads[0], "review", "success")
	w.rounds(3)
	if !w.landed(1) {
		t.Errorf("#1 never landed once its review passed again\n%s", w.log)
	}
}

func TestAPullRequestThatWaitsOnACheckNamesIt(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff", "review"]}`)
	passed, unreviewed, failed := w.open(1, "a", "1\n"), w.open(2, "b", "2\n"), w.open(3, "c", "3\n")
	for _, p := range []*pull{passed, unreviewed, failed} {
		w.set(p.heads[0], "signoff", "success")
	}
	w.set(passed.heads[0], "review", "success")
	w.set(failed.heads[0], "review", "failure")
	for n := 1; n <= 3; n++ {
		w.label(n)
	}
	w.rounds(5)
	if s := w.last(failed.heads[0], "merge-queue"); s.Description != "waits: review failed" {
		t.Errorf("#3's review failed and its status says %q", s.Description)
	}
	if s := w.last(unreviewed.heads[0], "merge-queue"); s.Description != "waits: review has not passed" {
		t.Errorf("#2 waits on a review and its status says %q", s.Description)
	}
	if !w.landed(1) || w.landed(2) || !w.queued(2) {
		t.Errorf("landed: passed %v, unreviewed %v\n%s", w.landed(1), w.landed(2), w.log)
	}
}

func TestAPullRequestThatMayNotJoinWaitsInPlaceAndSaysWhy(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff"]}`)
	draft, red, ok := w.open(1, "a", "1\n"), w.open(2, "b", "2\n"), w.open(3, "c", "3\n")
	draft.draft = true
	w.set(draft.heads[0], "signoff", "success")
	w.set(red.heads[0], "signoff", "failure")
	for n := 1; n <= 3; n++ {
		w.label(n)
	}
	w.rounds(2)
	for p, want := range map[*pull]string{draft: "waits: it is a draft", red: "waits: signoff failed", ok: "waits: signoff has not passed"} {
		if s := w.last(p.heads[0], "merge-queue"); s.State != "pending" || s.Description != want || !w.queued(p.number) {
			t.Errorf("#%d says %q, %q, and should wait in place saying %q", p.number, s.State, s.Description, want)
		}
	}
	w.set(ok.heads[0], "signoff", "success")
	w.rounds(3)
	if s := w.last(ok.heads[0], "merge-queue"); !w.landed(3) || w.landed(1) || w.landed(2) || s.State != "success" {
		t.Errorf("landed: draft %v, red %v, passed %v; #3 says %q\n%s", w.landed(1), w.landed(2), w.landed(3), s.Description, w.log)
	}
}

func TestAnUnresolvedThreadKeepsAPullRequestOutAndSoDoThreadsTheQueueCannotRead(t *testing.T) {
	w := newWorld(t, `{"threads": true}`)
	open := w.queue(1, "a", "1\n")
	open.threads = []bool{true, false}
	// GitHub hands over the first hundred threads, and any past them may be unresolved.
	long := w.queue(2, "b", "2\n")
	long.threads, long.unread = []bool{true}, 1
	w.rounds(3)
	for p, want := range map[*pull]string{open: "waits: 1 review threads are unresolved", long: "waits: it has more review threads than the queue reads"} {
		if s := w.last(p.heads[0], "merge-queue"); s.Description != want || w.landed(p.number) {
			t.Errorf("#%d says %q and landed %v, and should wait saying %q", p.number, s.Description, w.landed(p.number), want)
		}
	}
	open.threads[1] = true
	w.rounds(3)
	if !w.landed(1) || w.landed(2) {
		t.Errorf("landed: resolved %v, unread %v\n%s", w.landed(1), w.landed(2), w.log)
	}
}

func TestARecordedTickReplaysToTheSameActions(t *testing.T) {
	w := newWorld(t, `{"slots": 2}`)
	w.q.Record = t.TempDir()
	w.queue(1, "a", "1\n")
	w.queue(2, "b", "2\n")
	w.rounds(2)
	files, _ := os.ReadDir(w.q.Record)
	if len(files) == 0 {
		t.Fatal("no tick was recorded")
	}
	for _, f := range files {
		data, _ := os.ReadFile(w.q.Record + "/" + f.Name())
		var rec queue.Recording
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(rec.Replay()), fmt.Sprint(rec.Actions); got != want {
			t.Errorf("%s replays to\n %s\nrecorded\n %s", f.Name(), got, want)
		}
	}
}

func TestProbesNarrowABatchToItsCulprit(t *testing.T) {
	w := newWorld(t, `{"slots": 3, "max_batch": 4, "probe": "merge-queue/probe"}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }
	w.queue(1, "a", "1\n")
	w.queue(2, "broken", "2\n")
	w.queue(3, "c", "3\n")
	w.queue(4, "d", "4\n")
	w.rounds(12)
	if w.landed(2) || w.queued(2) || !w.said(2, "attributed=culprit") {
		t.Errorf("#2 breaks the build: landed %v, queued %v\n%s", w.landed(2), w.queued(2), w.log)
	}
	for _, n := range []int{1, 3, 4} {
		if !w.landed(n) || len(w.pulls[n].comments) != 0 {
			t.Errorf("#%d: landed %v, comments %+v", n, w.landed(n), w.pulls[n].comments)
		}
	}
}

func TestARedCandidateIsJudgedWhereItStoodAfterTheTrunkMovesOn(t *testing.T) {
	w := newWorld(t, `{"slots": 2, "started": "started"}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }
	w.queue(1, "a", "1\n")
	w.rounds(2)
	if !w.landed(1) {
		t.Fatalf("#1 did not land\n%s", w.log)
	}

	// #2 breaks the build. Its round is still running when #3, built beside it, lands.
	checks := true
	w.slow = func(sha string) bool {
		return checks && strings.Contains(gittest.Sh(t, w.o.Bare, "git", "show", "--no-patch", "--format=%B", sha), "Banyan-Check")
	}
	two := w.queue(2, "broken", "2\n")
	w.queue(3, "c", "3\n")
	w.rounds(4)
	if !w.landed(3) || !w.queued(2) {
		t.Fatalf("with #2's round running, #3 landed %v and #2 is queued %v\n%s", w.landed(3), w.queued(2), w.log)
	}
	checks = false
	w.ci()
	w.rounds(2)
	if w.queued(2) || !w.said(2, "<!-- merge-queue eject "+two.heads[0]+" attributed=culprit -->") {
		t.Errorf("#2 was not ejected as the culprit: %+v\n%s", two.comments, w.log)
	}
	// #1; #2 and #2 with #3; #3 beside the test; and the test's three builds, one a round:
	// #2 again, the trunk it stood on, #2 once more.
	if len(w.built) != 7 {
		t.Errorf("CI built %d commits, and the test begun before the trunk moved should have decided it in 7\n%s", len(w.built), w.log)
	}
	if left := w.o.Branches("merge-queue/"); len(left) != 0 {
		t.Errorf("branches left behind: %v", left)
	}
}

func TestAPullRequestQueuedBehindABuildBegunAgainIsBuiltOnIt(t *testing.T) {
	w := newWorld(t, `{"slots": 2}`)
	failed, first, second := false, false, true
	w.red = func(sha string) bool {
		flake := !failed && w.o.Holds(sha, "a")
		failed = failed || flake
		return flake
	}
	w.slow = func(sha string) bool {
		if w.o.Holds(sha, "b") {
			return second
		}
		return first
	}
	holding := func() (shas []string) {
		for _, sha := range w.o.Branches("merge-queue/") {
			if w.o.Holds(sha, "b") {
				shas = append(shas, sha)
			}
		}
		return shas
	}
	w.queue(1, "a", "1\n")
	w.rounds(2) // its build failed, and the same tree then passed
	first = true
	w.round() // so it is built again to land
	w.queue(2, "b", "2\n")
	w.round()
	behind := holding()
	first = false
	w.rounds(2)
	if after := holding(); !w.landed(1) || len(behind) != 1 || !slices.Equal(behind, after) {
		t.Fatalf("#1 landed %v, and #2's build was %v before and is %v after\n%s", w.landed(1), behind, after, w.log)
	}
	second = false
	w.rounds(2)
	if !w.landed(2) {
		t.Errorf("#2 did not land\n%s", w.log)
	}
}

func TestATickSaysWhatItSpentOfGitHubsQuotas(t *testing.T) {
	w := newWorld(t, `{}`)
	w.left = map[string]int{"core": 5000, "graphql": 5000}
	w.queue(1, "a", "one\n")
	w.round()
	lines := strings.Split(strings.TrimSpace(w.log.String()), "\n")
	var said struct{ GitHub map[string]github.Quota }
	json.Unmarshal([]byte(lines[len(lines)-1]), &said)
	core, graphql := said.GitHub["core"], said.GitHub["graphql"]
	if core.Requests == 0 || core.Left != 5000-core.Requests || graphql.Requests == 0 || graphql.Left != 5000-graphql.Requests {
		t.Errorf("the tick's last line counts %+v of core and %+v of graphql, from quotas of 5000 each\n%s", core, graphql, w.log)
	}
}

func TestACommitWithMoreThanAHundredContextsStillLands(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff"]}`)
	p := w.open(1, "a", "1\n")
	for i := 0; i < 135; i++ {
		w.set(p.heads[0], fmt.Sprintf("check-%d", i), "success")
	}
	w.set(p.heads[0], "signoff", "success")
	w.now = w.now.Add(time.Second)
	w.label(1)
	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatalf("tick: %v\n%s", err, w.log)
	}
	branches := w.o.Branches("merge-queue/")
	if len(branches) != 1 {
		t.Fatalf("#1 was not admitted with signoff on page 2; branches: %v\n%s", branches, w.log)
	}
	var sha string
	for _, s := range branches {
		sha = s
	}
	for i := 0; i < 135; i++ {
		w.set(sha, fmt.Sprintf("job-%d", i), "success")
	}
	w.ci()
	w.rounds(2)
	if !w.landed(1) {
		t.Fatalf("#1 did not land when its build had > 100 contexts\n%s", w.log)
	}

	w2 := newWorld(t, `{"admit": ["signoff"]}`)
	two := w2.open(2, "b", "2\n")
	w2.set(two.heads[0], "signoff", "success")
	w2.now = w2.now.Add(time.Second)
	w2.label(2)
	w2.rounds(3)
	if !w2.landed(2) {
		t.Fatalf("#2 did not land\n%s", w2.log)
	}
	if w2.contextPages != 0 {
		t.Errorf("made %d extra context page requests for commits that fit in one page", w2.contextPages)
	}
}

func TestAPullRequestRelabelledFiftyTimesDoesNotJumpTheQueue(t *testing.T) {
	w := newWorld(t, `{"slots": 1}`)
	two := w.queue(2, "b", "2\n")
	for i := 0; i < 55; i++ {
		two.events = append(two.events, labelEvent{fmt.Sprintf("noise-%d", i), w.now})
	}
	w.queue(3, "c", "3\n")
	w.now = w.now.Add(time.Second)
	one := w.open(1, "a", "1\n")
	one.labels["merge-queue"] = w.now
	for i := 0; i < 55; i++ {
		one.events = append(one.events, labelEvent{fmt.Sprintf("noise-%d", i), w.now})
	}

	w.rounds(2)
	if !w.landed(2) || w.landed(3) || w.landed(1) {
		t.Fatalf("after 2 rounds: #2=%v #3=%v #1=%v\n%s", w.landed(2), w.landed(3), w.landed(1), w.log)
	}
	w.round()
	if !w.landed(3) || w.landed(1) {
		t.Fatalf("after 3 rounds: #3=%v #1=%v\n%s", w.landed(3), w.landed(1), w.log)
	}
	w.round()
	if !w.landed(1) {
		t.Fatalf("after 4 rounds: #1=%v\n%s", w.landed(1), w.log)
	}
}

func TestACommitWithMoreThanAHundredStatusEventsIsNotBuiltTwice(t *testing.T) {
	w := newWorld(t, `{}`)
	w.queue(1, "a", "1\n")
	w.round()
	if len(w.built) != 1 {
		t.Fatalf("expected 1 build after first round, got %d\n%s", len(w.built), w.log)
	}
	first := w.built[0]
	for i := 0; i < 105; i++ {
		w.set(first, fmt.Sprintf("step-%d", i), "success")
	}
	delete(w.pulls[1].labels, "merge-queue")
	w.round()

	w.now = w.now.Add(time.Second)
	w.label(1)
	w.rounds(2)
	if !w.landed(1) {
		t.Fatalf("#1 did not land after re-queue\n%s", w.log)
	}
	if pushes := strings.Count(w.log.String(), fmt.Sprintf(`"do":"push","build":{"SHA":%q`, first)); pushes != 1 {
		t.Errorf("%s was pushed %d times, want 1\n%s", first, pushes, w.log)
	}
	if len(w.built) != 2 || w.built[1] == first {
		t.Errorf("built %v, expected a fresh second commit after %s", w.built, first)
	}
}

func TestATickRetriesTransientRefusalsAndNeverSendsACommentTwice(t *testing.T) {
	w := newWorld(t, `{}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }

	step := 0
	w.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/graphql" && step == 0:
			step++
			rw.WriteHeader(502)
			return true
		case r.URL.Path == "/graphql" && step == 1:
			step++
			rw.WriteHeader(503)
			return true
		case r.URL.Path == "/graphql" && step == 2:
			step++
			rw.WriteHeader(504)
			return true
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/statuses/") && step == 3:
			step++
			if hj, ok := rw.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return true
		case step == 4:
			step++
			rw.Header().Set("Retry-After", "3")
			rw.WriteHeader(429)
			return true
		case step == 5:
			step++
			rw.Header().Set("Retry-After", "120")
			rw.WriteHeader(403)
			return true
		}
		return false
	}

	w.queue(1, "a", "1\n")
	w.rounds(3)
	if !w.landed(1) {
		t.Fatalf("#1 did not land after transient refusals\n%s", w.log)
	}
	if len(w.waited) != 6 {
		t.Fatalf("waited %v (%d times), want 6", w.waited, len(w.waited))
	}
	if d := w.waited[4]; d < 3*time.Second || d > 5*time.Second {
		t.Errorf("Retry-After: 3 waited %v, want [3s, 5s]", d)
	}
	if d := w.waited[5]; d != 5*time.Second {
		t.Errorf("Retry-After: 120 waited %v, want 5s cap", d)
	}

	commentPosts, getComment503, unlabel503 := 0, false, false
	two := w.queue(2, "broken", "2\n")
	w.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/2/comments":
			commentPosts++
			if commentPosts == 1 {
				body, _ := io.ReadAll(r.Body)
				var c comment
				json.Unmarshal(body, &c)
				c.CreatedAt = w.now
				two.comments = append(two.comments, c)
				rw.WriteHeader(502)
				return true
			}
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/2/comments" && commentPosts > 0 && !getComment503:
			getComment503 = true
			rw.WriteHeader(503)
			return true
		case r.Method == "DELETE" && r.URL.Path == "/repos/o/r/issues/2/labels/merge-queue" && !unlabel503:
			unlabel503 = true
			rw.WriteHeader(503)
			return true
		}
		return false
	}

	failed := 0
	for i := 0; i < 10 && w.queued(2); i++ {
		w.now = w.now.Add(2 * time.Minute)
		if err := w.q.Tick(); err != nil {
			failed++
		}
		w.ci()
	}
	if failed != 1 || commentPosts != 1 || len(two.comments) != 1 || w.queued(2) || !getComment503 || !unlabel503 {
		t.Errorf("failed=%d commentPosts=%d comments=%d queued=%v getComment503=%v unlabel503=%v\n%s",
			failed, commentPosts, len(two.comments), w.queued(2), getComment503, unlabel503, w.log)
	}
}

func TestAGitHubOutageOrRateLimitDoesNotWriteStoppedStatuses(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff"]}`)
	one, two, three := w.queue(1, "a", "1\n"), w.queue(2, "b", "2\n"), w.queue(3, "c", "3\n")

	statusPosts := 0
	w.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/o/r/statuses/") {
			statusPosts++
			rw.WriteHeader(503)
			return true
		}
		return false
	}

	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err == nil {
		t.Fatalf("expected error on 503 outage\n%s", w.log)
	}
	if statusPosts != 4 || !strings.Contains(w.log.String(), `"error":`) {
		t.Errorf("on 503 outage: statusPosts=%d (want 4), log:\n%s", statusPosts, w.log)
	}
	for _, p := range []*pull{one, two, three} {
		if s := w.last(p.heads[0], "merge-queue"); strings.HasPrefix(s.Description, "the queue is stopped:") {
			t.Errorf("#%d was written %q during 503 outage", p.number, s.Description)
		}
	}

	statusPosts = 0
	w.log.Reset()
	w.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/repos/o/r/statuses/") {
			statusPosts++
			rw.Header().Set("Retry-After", "60")
			rw.WriteHeader(403)
			return true
		}
		return false
	}

	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err == nil {
		t.Fatalf("expected error on 403 secondary rate limit\n%s", w.log)
	}
	if statusPosts != 4 || !strings.Contains(w.log.String(), `"error":`) {
		t.Errorf("on 403 secondary limit: statusPosts=%d (want 4), log:\n%s", statusPosts, w.log)
	}
	for _, p := range []*pull{one, two, three} {
		if s := w.last(p.heads[0], "merge-queue"); strings.HasPrefix(s.Description, "the queue is stopped:") {
			t.Errorf("#%d was written %q during 403 limit", p.number, s.Description)
		}
	}

	w.intercept = nil
	for _, p := range []*pull{one, two, three} {
		w.set(p.heads[0], "signoff", "success")
	}
	w.rounds(4)
	for _, p := range []*pull{one, two, three} {
		if !w.landed(p.number) {
			t.Errorf("#%d did not land after outage cleared\n%s", p.number, w.log)
		}
	}
}

func TestATickDoesNotEjectAPullRequestTwiceWhileGraphQLLags(t *testing.T) {
	w := newWorld(t, `{"slots": 2}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }
	one := w.queue(1, "broken", "1\n")
	w.rounds(4)
	if !w.queued(1) {
		t.Fatalf("#1 left before the ejection tick\n%s", w.log)
	}

	w.queue(2, "b", "2\n")
	w.lagUnlabel = true
	w.log.Reset()
	w.now = w.now.Add(2 * time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatalf("tick: %v\n%s", err, w.log)
	}
	w.lagUnlabel = false
	delete(one.labels, "merge-queue")

	if ejects := strings.Count(w.log.String(), `"do":"eject"`); ejects != 1 {
		t.Errorf("ejected %d times in one tick, want 1\n%s", ejects, w.log)
	}
	if len(one.comments) != 1 {
		t.Errorf("#1 got %d comments, want 1", len(one.comments))
	}
	w.ci()
	w.rounds(2)
	if !w.landed(2) {
		t.Errorf("#2 did not land after #1 was ejected\n%s", w.log)
	}
}

func TestABuildHasBegunOnceTheContextNamedForItAnswers(t *testing.T) {
	w := newWorld(t, `{"slots": 3, "build": ["gate (buildkite)"], "begins": "changes (buildkite)"}`)
	w.queue(1, "broken", "1\n")
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var cand string
	for _, sha := range w.o.Branches("merge-queue/") {
		cand = sha
	}
	began := w.now.Add(20 * time.Second)
	w.check(cand, "changes (buildkite)", "success", began)
	r, err := w.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.World.Builds) != 1 || r.World.Builds[0].Verdict != queue.Pending || !r.World.Builds[0].Started.Equal(began) {
		t.Fatalf("with changes passed and gate absent, build is %+v, want pending started at %v", r.World.Builds, began)
	}

	// Once the candidate fails, each round's changes passing after 20s lets the next round
	// start before any gate exists, so all three blame builds run side by side.
	w.check(cand, "gate (buildkite)", "failure", w.now.Add(10*time.Minute))
	var checks []string
	for round := 1; round <= 3; round++ {
		w.now = w.now.Add(time.Minute)
		if err := w.q.Tick(); err != nil {
			t.Fatal(err)
		}
		for _, sha := range w.o.Branches("merge-queue/") {
			if sha != cand && !slices.Contains(checks, sha) {
				checks = append(checks, sha)
				w.check(sha, "changes (buildkite)", "success", w.now.Add(20*time.Second))
			}
		}
	}
	if len(checks) != 3 {
		t.Fatalf("rounds running side by side before gate exists: %d (%v), want 3", len(checks), checks)
	}
	for _, sha := range checks {
		verdict := "success"
		if w.o.Holds(sha, "broken") {
			verdict = "failure"
		}
		w.check(sha, "gate (buildkite)", verdict, w.now.Add(10*time.Minute))
	}
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	if w.queued(1) || !w.said(1, "attributed=culprit") {
		t.Errorf("#1 was not ejected after side-by-side rounds answered: queued=%v comments=%+v", w.queued(1), w.pulls[1].comments)
	}

	// Another context's answer says nothing of when the build began, named for it or not.
	single := newWorld(t, `{"slots": 3, "build": ["changes (buildkite)", "gate (buildkite)"], "begins": "upload (buildkite)"}`)
	single.queue(1, "broken", "1\n")
	single.now = single.now.Add(time.Minute)
	if err := single.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var oneCand string
	for _, sha := range single.o.Branches("merge-queue/") {
		oneCand = sha
	}
	single.check(oneCand, "changes (buildkite)", "success", single.now.Add(20*time.Second))
	single.check(oneCand, "gate (buildkite)", "queued", time.Time{})
	sr, err := single.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.World.Builds) != 1 || !sr.World.Builds[0].Started.IsZero() {
		t.Errorf("a build with no context named for its start began at %v on another context's answer", sr.World.Builds[0].Started)
	}
	single.check(oneCand, "gate (buildkite)", "success", single.now.Add(5*time.Minute))
	sr, err = single.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !sr.World.Builds[0].Started.IsZero() {
		t.Errorf("a build that passed with no context named for its start began at %v", sr.World.Builds[0].Started)
	}
}

func TestLeavingTheQueueTakesBothLabelsOff(t *testing.T) {
	w := newWorld(t, `{"label": "land", "first": "land-first"}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }

	one := w.open(1, "broken", "1\n")
	w.now = w.now.Add(time.Second)
	one.labels["land"] = w.now
	one.labels["land-first"] = w.now
	one.events = append(one.events, labelEvent{"land", w.now}, labelEvent{"land-first", w.now})

	two := w.open(2, "ok", "2\n")
	w.now = w.now.Add(time.Second)
	two.labels["land"] = w.now
	two.events = append(two.events, labelEvent{"land", w.now})

	var unlabelled []string
	w.intercept = func(rw http.ResponseWriter, r *http.Request) bool {
		if r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/") {
			unlabelled = append(unlabelled, r.URL.Path)
		}
		return false
	}

	w.rounds(8)
	if !w.landed(2) {
		t.Errorf("#2 did not land\n%s", w.log)
	}
	if _, has := one.labels["land"]; has {
		t.Errorf("#1 still carries land: %v", one.labels)
	}
	if _, has := one.labels["land-first"]; has {
		t.Errorf("#1 still carries land-first: %v", one.labels)
	}
	want := []string{"/repos/o/r/issues/1/labels/land-first", "/repos/o/r/issues/1/labels/land"}
	if !slices.Equal(unlabelled, want) {
		t.Errorf("unlabel calls = %v, want %v", unlabelled, want)
	}
}

func TestAnEjectionBeginsWithTheSentenceEveryReaderExpects(t *testing.T) {
	w := newWorld(t, `{"name": "Banyan"}`)
	w.red = func(sha string) bool { return w.o.Holds(sha, "broken") }
	one := w.queue(1, "broken", "1\n")
	w.rounds(6)
	if len(one.comments) != 1 {
		t.Fatalf("#1 has %d comments, want 1\n%s", len(one.comments), w.log)
	}
	body := one.comments[0].Body
	head := one.heads[0]

	wantPrefix := "Banyan took this pull request out of the queue (cause: culprit), judging its head " + head + ": the build with it failed, the build without it then passed ("
	if !strings.HasPrefix(body, wantPrefix) {
		t.Errorf("comment does not begin with v2 sentence:\n%s\nwant prefix:\n%s", body, wantPrefix)
	}
	for _, keep := range []string{
		"\n- passed without it: https://ci.example/",
		"\n- failed with it: https://ci.example/",
		"\n\nFix it and queue it again. If the failure isn't this pull request's, queue it again as it is.\n\n<!-- merge-queue eject " + head + " attributed=culprit -->",
	} {
		if !strings.Contains(body, keep) {
			t.Errorf("comment lost %q:\n%s", keep, body)
		}
	}

	// Each reader's expression from the second repository, held against the comment.
	// jq string literals double-escape backslashes (\\( -> \() and Oniguruma writes (?<name>...) where RE2 writes (?P<name>...).
	readers := []struct {
		tool      string
		re        *regexp.Regexp
		lowercase bool // tools/landing-metrics runs (.body | ascii_downcase) first
		cause     int  // submatch index of cause, or 0
		head      int  // submatch index of head, or 0
		why       int  // submatch index of why, or 0
	}{
		{
			tool:  "tools/factory/follow.py:132",
			re:    regexp.MustCompile(`(?:^|\n)Banyan took this pull request out of the queue \(cause: (?P<cause>[a-z]+)\), judging its head (?P<head>[0-9a-f]+): (?P<why>[^\n]*)`),
			cause: 1, head: 2, why: 3,
		},
		{
			// jq: startswith("Banyan took this pull request out of the queue (cause: ")
			tool: "tools/banyan-handoff:109",
			re:   regexp.MustCompile(`^Banyan took this pull request out of the queue \(cause: `),
		},
		{
			// jq: "^Banyan took this pull request out of the queue \\(cause: (?<cause>[a-z]+)\\), judging its head (?<head>[0-9a-f]+): (?<why>[^\n]*)"
			tool:  "tools/banyan-handoff:110",
			re:    regexp.MustCompile(`^Banyan took this pull request out of the queue \(cause: (?P<cause>[a-z]+)\), judging its head (?P<head>[0-9a-f]+): (?P<why>[^\n]*)`),
			cause: 1, head: 2, why: 3,
		},
		{
			tool:  "tools/telemetry/readout.py:69",
			re:    regexp.MustCompile(`\(cause: ([a-z-]+)\)`),
			cause: 1,
		},
		{
			tool:  "tools/telemetry/arcs.py:507",
			re:    regexp.MustCompile(`\(cause: ([A-Za-z0-9_-]+)\)`),
			cause: 1,
		},
		{
			// jq on (.body | ascii_downcase): test("cause")
			tool:      "tools/landing-metrics:527 (test)",
			re:        regexp.MustCompile(`cause`),
			lowercase: true,
		},
		{
			// jq on (.body | ascii_downcase): capture("cause\\W+(?<c>[a-z-]+)")
			tool:      "tools/landing-metrics:527 (capture)",
			re:        regexp.MustCompile(`cause\W+(?P<c>[a-z-]+)`),
			lowercase: true,
			cause:     1,
		},
		{
			// jq on (.body | ascii_downcase): [match("\\b[0-9a-f]{40}\\b"; "g").string] | first
			tool:      "tools/landing-metrics:528 (head)",
			re:        regexp.MustCompile(`(\b[0-9a-f]{40}\b)`),
			lowercase: true,
			head:      1,
		},
		{
			tool:  "tools/catwalk-app/reports/banyan_live.py:22",
			re:    regexp.MustCompile(`\(cause: ([^)]+)\), judging its head [0-9a-f]+: ([^\n]*)`),
			cause: 1, why: 2,
		},
		{
			// jq: "(^|\n)Banyan took this pull request out of the queue \\(cause: (?<cause>[a-z]+)\\), judging its head (?<head>[0-9a-f]+)"
			tool:  "tools/banyan-repair:243",
			re:    regexp.MustCompile(`(^|\n)Banyan took this pull request out of the queue \(cause: (?P<cause>[a-z]+)\), judging its head (?P<head>[0-9a-f]+)`),
			cause: 2, head: 3,
		},
	}
	for _, tc := range readers {
		in := body
		if tc.lowercase {
			in = strings.ToLower(in)
		}
		m := tc.re.FindStringSubmatch(in)
		if m == nil {
			t.Errorf("%s did not match:\n%s", tc.tool, body)
			continue
		}
		if tc.cause > 0 && m[tc.cause] != "culprit" {
			t.Errorf("%s read cause %q, want culprit", tc.tool, m[tc.cause])
		}
		if tc.head > 0 && m[tc.head] != head {
			t.Errorf("%s read head %q, want %s", tc.tool, m[tc.head], head)
		}
		if tc.why > 0 && (!strings.HasPrefix(m[tc.why], "the build with it failed, the build without it then passed (") || !strings.HasSuffix(m[tc.why], "), and the build with it failed again.")) {
			t.Errorf("%s read why %q", tc.tool, m[tc.why])
		}
	}

	def := newWorld(t, `{}`)
	def.red = func(sha string) bool { return def.o.Holds(sha, "broken") }
	two := def.queue(2, "broken", "2\n")
	def.rounds(6)
	if len(two.comments) != 1 || !strings.HasPrefix(two.comments[0].Body, "The merge queue took this pull request out of the queue (cause: culprit), judging its head "+two.heads[0]+": ") {
		t.Errorf("default name comment:\n%+v", two.comments)
	}
}

func TestCandidatesAnotherQueueLeftAreLandedAsTheyStand(t *testing.T) {
	w := newWorld(t, `{"slots": 3}`)
	a, b := w.queue(1, "a", "1\n"), w.queue(2, "b", "2\n")
	tip := w.o.Trunk()
	merge := func(p *pull, said string) string {
		gittest.Sh(t, w.o.Work, "git", "-c", "user.name=Another Queue", "-c", "user.email=other@example.com", "merge", "--quiet", "--no-ff", "-m", said, p.heads[0])
		return gittest.Sh(t, w.o.Work, "git", "rev-parse", "HEAD")
	}
	gittest.Sh(t, w.o.Work, "git", "fetch", "--quiet", "origin")
	gittest.Sh(t, w.o.Work, "git", "checkout", "--quiet", "--detach", tip)
	one := merge(a, "Teach the importer a new trick\n\nWhy, at length.\n\n<!-- a bot's note, copied from the pull request -->\n\nMerges #1")
	two := merge(b, "Fix the thing the importer broke (#2)\n\nMerges #2")
	for name, sha := range map[string]string{two[:12]: two, tip[:12]: tip, one[:12] + "-express": one} {
		gittest.Sh(t, w.o.Work, "git", "push", "--quiet", "origin", sha+":refs/heads/merge-queue/main/"+name)
	}
	w.set(two, "merge-queue/build", "success")

	w.rounds(2)

	if !w.landed(1) || !w.landed(2) || w.o.Trunk() != two {
		t.Fatalf("the trunk is %.12s and #1, #2 landed %v, %v, where the candidate left at %.12s had passed\n%s", w.o.Trunk(), w.landed(1), w.landed(2), two, w.log)
	}
	if strings.Contains(w.log.String(), `"do":"push"`) {
		t.Errorf("the queue pushed a build of its own for a candidate that had already passed\n%s", w.log)
	}
	left := w.o.Branches("merge-queue/")
	if _, read := left["refs/heads/merge-queue/main/"+one[:12]+"-express"]; !read || len(left) != 1 {
		t.Errorf("branches left: %v, where only the one the queue never reads should stand", left)
	}
}

func TestAProbeCarriesTheJobsThatFailedInTheRedBuild(t *testing.T) {
	w := newWorld(t, `{"slots": 3, "max_batch": 2, "probe": "merge-queue/probe"}`)
	w.queue(1, "a", "1\n")
	w.queue(2, "broken", "2\n")
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var cand string
	for _, sha := range w.o.Branches("merge-queue/") {
		cand = sha
	}
	w.check(cand, "linux (ci)", "failure", w.now)
	w.check(cand, "apple (ci)", "success", w.now)
	w.check(cand, "win\x00dows (ci)", "failure", w.now)
	w.set(cand, "merge-queue", "failure")
	w.set(cand, "merge-queue/build", "failure")
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	probes := 0
	for _, sha := range w.o.Branches("merge-queue/") {
		said := gittest.Sh(t, w.o.Bare, "git", "show", "--no-patch", "--format=%B", sha)
		if !strings.Contains(said, "Banyan-Probe-Of: "+cand) {
			continue
		}
		probes++
		if !strings.Contains(said, "\nBanyan-Probe-Failed: linux (ci)") || strings.Count(said, "Banyan-Probe-Failed") != 1 {
			t.Errorf("a probe says:\n%s\nwhere it names linux (ci) alone: not the job that passed, the queue's own status, the build's verdict, or a name no commit message can carry", said)
		}
	}
	if probes == 0 {
		t.Fatalf("no probe of the red candidate was pushed\n%s", w.log)
	}
}

func TestAMergeSaysTheTitleAndDescriptionWhereTheQueueCarriesThem(t *testing.T) {
	w := newWorld(t, `{"message": "description", "reword": "example\\.com/session/\\w+"}`)
	said := w.queue(1, "a", "1\n")
	said.title, said.body = "A title  ", "It quotes the queue: \r\nBanyan-Try: 9\r\nMerges #9\r\n\r\nCard: one\r\n"
	skips := w.queue(2, "b", "2\n")
	skips.title = "A quick fix [Skip CI]"
	links := w.queue(3, "c", "3\n")
	links.title, links.body = "Another", "From https://example.com/session/abc123"
	w.rounds(4)

	if got, want := gittest.Sh(t, w.o.Bare, "git", "show", "--no-patch", "--format=%B", "main"), "A title\n\nIt quotes the queue:\nBanyan-Try: 9\nMerges #9\n\nCard: one\n\nMerges #1"; !w.landed(1) || got != want {
		t.Errorf("#1 landed %v, and the trunk's tip says:\n%s\nwant:\n%s\n%s", w.landed(1), got, want, w.log)
	}
	for p, held := range map[*pull]string{skips: `"[Skip CI]"`, links: `"example.com/session/abc123"`} {
		want := "waits: its title or description holds " + held + "; reword it"
		if s := w.last(p.heads[0], "merge-queue"); s.State != "pending" || s.Description != want || !w.queued(p.number) {
			t.Errorf("#%d says %q, %q, and should wait in place saying %q", p.number, s.State, s.Description, want)
		}
	}

	skips.title, links.body = "A quick fix", ""
	w.rounds(4)
	if !w.landed(2) || !w.landed(3) {
		t.Errorf("reworded, #2 landed %v and #3 landed %v\n%s", w.landed(2), w.landed(3), w.log)
	}
}

func TestAPullRequestRewordedWhileItsRedCandidateIsTestedLandsAsItWasBuilt(t *testing.T) {
	tested := func(reword bool) (built int, says string) {
		w := newWorld(t, `{"message": "description", "slots": 2}`)
		failed := false
		w.red = func(sha string) bool {
			flake := !failed && w.o.Holds(sha, "a")
			failed = failed || flake
			return flake
		}
		p := w.queue(1, "a", "1\n")
		p.title = "As queued"
		w.round()
		if reword {
			p.title = "As reworded since"
		}
		w.rounds(5)
		if !w.landed(1) || len(p.comments) != 0 {
			t.Errorf("reworded %v, #1 landed %v with comments %+v\n%s", reword, w.landed(1), p.comments, w.log)
		}
		return len(w.built), gittest.Sh(t, w.o.Bare, "git", "show", "--no-patch", "--format=%B", "main")
	}
	plain, _ := tested(false)
	if built, says := tested(true); built != plain || !strings.HasPrefix(says, "As queued\n\n") {
		t.Errorf("reworded, CI built %d commits and the trunk says %q; left alone it built %d", built, says, plain)
	}
}

func TestAPatternThatMatchesNoWordsStillKeepsAPullRequestOut(t *testing.T) {
	w := newWorld(t, `{"message": "description", "reword": "^|secret"}`)
	p := w.queue(1, "a", "1\n")
	p.title = "A secret"
	w.rounds(3)
	if s := w.last(p.heads[0], "merge-queue"); w.landed(1) || s.Description != `waits: its title or description matches "^|secret"; reword it` {
		t.Errorf("#1 landed %v saying %q", w.landed(1), s.Description)
	}
}

func TestASettingTheQueueCannotReadStopsItBeforeItTicks(t *testing.T) {
	for config, want := range map[string]string{`{"message": "title"}`: "message", `{"reword": "("}`: "reword"} {
		path := t.TempDir() + "/merge-queue.json"
		os.WriteFile(path, []byte(config), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s loads with %v, and should be refused for its %s", config, err, want)
		}
	}
}

func TestLoadRefusesNegativeAppAndStatusSharedWithAnotherContext(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   string
	}{
		{`{"app": -1}`, "app"},
		{`{"probe": "merge-queue"}`, "status"},
		{`{"begins": "merge-queue"}`, "status"},
		{`{"build": ["ci", "merge-queue"]}`, "status"},
		{`{"admit": ["signoff", "merge-queue"]}`, "status"},
		{`{"status": "ci", "admit": ["ci"]}`, "status"},
	} {
		path := t.TempDir() + "/merge-queue.json"
		os.WriteFile(path, []byte(tc.config), 0o644)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s loads with %v, and should be refused naming %s and %s", tc.config, err, path, tc.want)
		}
	}
}

func TestOnlyTheConfiguredAppsCheckRunsJudgeHeadsBuildsAndProbes(t *testing.T) {
	w := newWorld(t, `{"app": 7, "admit": ["signoff"]}`)
	statusOnly, shadowed, otherApp := w.open(1, "a", "1\n"), w.open(2, "b", "2\n"), w.open(3, "c", "3\n")
	w.set(statusOnly.heads[0], "signoff", "success")
	w.check(shadowed.heads[0], "signoff", "failure", w.now, 7)
	w.now = w.now.Add(time.Second)
	w.set(shadowed.heads[0], "signoff", "success")
	w.check(shadowed.heads[0], "signoff", "success", w.now, 8)
	w.check(otherApp.heads[0], "signoff", "success", w.now, 8)
	for n := 1; n <= 3; n++ {
		w.label(n)
	}
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[*pull]string{
		statusOnly: "waits: signoff has not passed",
		shadowed:   "waits: signoff failed",
		otherApp:   "waits: signoff has not passed",
	} {
		if s := w.last(p.heads[0], "merge-queue"); s.State != "pending" || s.Description != want {
			t.Errorf("#%d says %q %q, want pending %q", p.number, s.State, s.Description, want)
		}
	}
	if branches := w.o.Branches("merge-queue/"); len(branches) != 0 {
		t.Errorf("pushed %v before any head had a green check run from the App", branches)
	}
	w.check(otherApp.heads[0], "signoff", "success", w.now.Add(time.Second), 7)
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	if s := w.last(otherApp.heads[0], "merge-queue"); s.Description != "in the queue, 0 ahead of it" || len(w.o.Branches("merge-queue/")) != 1 {
		t.Errorf("#3 after the App's green check run says %q with branches %v, want admitted as 0 ahead", s.Description, w.o.Branches("merge-queue/"))
	}

	wb := newWorld(t, `{"app": 7}`)
	wb.queue(1, "a", "1\n")
	wb.now = wb.now.Add(time.Minute)
	if err := wb.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var cand string
	for _, sha := range wb.o.Branches("merge-queue/") {
		cand = sha
	}
	pushedAt := wb.now
	wb.set(cand, "merge-queue/build", "success")
	wb.check(cand, "merge-queue/build", "success", wb.now, 8)
	wb.now = wb.now.Add(time.Minute)
	if err := wb.q.Tick(); err != nil {
		t.Fatal(err)
	}
	if wb.landed(1) || wb.o.Trunk() == cand {
		t.Errorf("candidate landed on a commit status and another App's check run")
	}
	rb, err := wb.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(rb.World.Builds) != 1 || rb.World.Builds[0].Verdict != queue.Pending || !rb.World.Builds[0].Pushed.Equal(pushedAt) {
		t.Errorf("candidate = %+v, want pending pushed at %v (the queue's own status read back)", rb.World.Builds, pushedAt)
	}
	wb.check(cand, "merge-queue/build", "failure", wb.now, 7)
	wb.now = wb.now.Add(time.Second)
	wb.set(cand, "merge-queue/build", "success")
	wb.check(cand, "merge-queue/build", "success", wb.now, 8)
	if rb, err = wb.q.Read(); err != nil {
		t.Fatal(err)
	}
	if len(rb.World.Builds) != 1 || rb.World.Builds[0].Verdict != queue.Failure {
		t.Errorf("candidate with failed App check run and newer green status = %+v, want failure", rb.World.Builds)
	}
	wb.check(cand, "merge-queue/build", "success", wb.now.Add(time.Second), 7)
	wb.now = wb.now.Add(time.Minute)
	if err := wb.q.Tick(); err != nil {
		t.Fatal(err)
	}
	if wb.o.Trunk() != cand {
		t.Errorf("candidate did not land on the App's green check run; trunk is %s, want %s", wb.o.Trunk(), cand)
	}

	wp := newWorld(t, `{"app": 7, "slots": 3, "max_batch": 2, "probe": "merge-queue/probe", "begins": "changes"}`)
	wp.queue(1, "a", "1\n")
	wp.queue(2, "broken", "2\n")
	wp.now = wp.now.Add(time.Minute)
	if err := wp.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var redCand string
	for _, sha := range wp.o.Branches("merge-queue/") {
		redCand = sha
	}
	wp.check(redCand, "merge-queue/build", "failure", wp.now, 7)
	wp.now = wp.now.Add(time.Minute)
	if err := wp.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var probes []string
	for _, sha := range wp.o.Branches("merge-queue/") {
		if sha != redCand {
			probes = append(probes, sha)
			wp.set(sha, "changes", "success")
			wp.check(sha, "changes", "success", wp.now, 8)
			wp.set(sha, "merge-queue/probe", "success")
			wp.check(sha, "merge-queue/probe", "success", wp.now, 8)
		}
	}
	rp, err := wp.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rp.World.Builds {
		if b.Kind == queue.Probe && (b.Verdict != queue.Pending || !b.Started.IsZero()) {
			t.Errorf("probe %s = verdict %v started %v on commit status and another App's check run, want pending with zero start", b.SHA, b.Verdict, b.Started)
		}
	}
	for _, sha := range probes {
		wp.check(sha, "merge-queue/probe", "failure", wp.now, 7)
	}
	wp.now = wp.now.Add(time.Second)
	for _, sha := range probes {
		wp.set(sha, "merge-queue/probe", "success")
		wp.check(sha, "merge-queue/probe", "success", wp.now, 8)
	}
	if rp, err = wp.q.Read(); err != nil {
		t.Fatal(err)
	}
	for _, b := range rp.World.Builds {
		if b.Kind == queue.Probe && b.Verdict != queue.Failure {
			t.Errorf("probe %s with failed App check run and newer green status = %v, want failure", b.SHA, b.Verdict)
		}
	}
	began := wp.now.Add(time.Second)
	for _, sha := range probes {
		wp.check(sha, "changes", "success", began, 7)
		wp.check(sha, "merge-queue/probe", "success", began, 7)
	}
	wp.now = wp.now.Add(time.Minute)
	for _, sha := range probes {
		wp.set(sha, "changes", "pending")
		wp.check(sha, "changes", "queued", wp.now, 8)
	}
	if rp, err = wp.q.Read(); err != nil {
		t.Fatal(err)
	}
	for _, b := range rp.World.Builds {
		if b.Kind == queue.Probe && (b.Verdict != queue.Success || !b.Started.Equal(began)) {
			t.Errorf("probe %s after App check runs = verdict %v started %v, want success started at %v", b.SHA, b.Verdict, b.Started, began)
		}
	}

	u := newWorld(t, `{"admit": ["signoff"]}`)
	p := u.open(1, "a", "1\n")
	u.set(p.heads[0], "signoff", "success")
	u.label(1)
	u.rounds(2)
	if !u.landed(1) {
		t.Errorf("with app unset, #1 did not land on commit statuses\n%s", u.log)
	}
}

func TestARedCandidatesFailedJobsComeOnlyFromTheConfiguredApp(t *testing.T) {
	w := newWorld(t, `{"app": 7}`)
	w.queue(1, "a", "1\n")
	w.now = w.now.Add(time.Minute)
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var cand string
	for _, sha := range w.o.Branches("merge-queue/") {
		cand = sha
	}
	w.check(cand, "merge-queue/build", "failure", w.now, 7)
	w.check(cand, "unit", "failure", w.now, 7)
	w.check(cand, "cover", "failure", w.now, 8)
	w.now = w.now.Add(time.Second)
	w.set(cand, "unit", "success")
	w.set(cand, "lint", "failure")
	r, err := w.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.World.Builds) != 1 || !slices.Equal(r.World.Builds[0].Failed, []string{"unit"}) {
		t.Errorf("red candidate Failed = %v, want [unit]", r.World.Builds[0].Failed)
	}
}

func TestKeepConfig(t *testing.T) {
	for _, value := range []string{"bad", "-1s", "0s", "3m", ""} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.json")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"keep":%q}`, value)), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			bad := value == "bad" || value == "-1s"
			if bad {
				if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "keep") {
					t.Fatalf("Load = %v, want file and keep refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want, _ := time.ParseDuration(value)
			if c.dials().Keep != want || New(c, "", "", "").Git.Keep != (want > 0) {
				t.Fatalf("keep %q not wired to queue and git: %+v", value, c.dials())
			}
		})
	}
}

// The Keep tests exercise the forge handler in memory, without a listener or wall time.
type keepTransport struct{ h http.Handler }

func (tr keepTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}
func newKeepWorld(t *testing.T, config string) *world {
	o := gittest.New(t)
	h := &hub{t: t, o: o, now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), pulls: map[int]*pull{}, statuses: map[string][]status{}}
	path := filepath.Join(o.Clone, "queue.json")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Repo = "o/r"
	w := &world{hub: h, log: &bytes.Buffer{}, red: func(string) bool { return false }}
	w.q = New(c, o.Clone, "https://hub.example", "")
	w.q.Hub.HTTP = &http.Client{Transport: keepTransport{h}}
	w.q.Now, w.q.Log = func() time.Time { return h.now }, w.log
	w.q.Hub.Wait = func(d time.Duration) { w.waited = append(w.waited, d) }
	return w
}

func keepTick(t *testing.T, w *world) {
	t.Helper()
	if err := w.q.Tick(); err != nil {
		t.Fatalf("tick: %v\n%s", err, w.log)
	}
}

func keepRead(t *testing.T, w *world) *Read {
	t.Helper()
	r, err := w.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func keepStack(t *testing.T, w *world) (queue.Build, queue.Build) {
	t.Helper()
	w.queue(1, "a", "a\n")
	w.queue(2, "b", "b\n")
	keepTick(t, w)
	r := keepRead(t, w)
	if len(r.World.Builds) != 2 {
		t.Fatalf("builds = %+v", r.World.Builds)
	}
	a, b := r.World.Builds[0], r.World.Builds[1]
	if len(a.Chain) > len(b.Chain) {
		a, b = b, a
	}
	return a, b
}

func keepLand(t *testing.T, w *world, sha string) {
	t.Helper()
	r := keepRead(t, w)
	for _, a := range queue.Plan(r.World, w.q.dials()) {
		if a.Do == "land" && a.SHA == sha {
			if err := w.q.land(a); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no landing for %s", sha)
}

func TestKeepReadAndTickAfterStackedLanding(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2,"max_batch":1}`)
	lower, upper := keepStack(t, w)
	for _, b := range []queue.Build{lower, upper} {
		w.check(b.SHA, "merge-queue/build", "success", w.now, 7)
	}
	w.now = w.now.Add(time.Minute)
	keepLand(t, w, upper.SHA)
	topMark := w.now
	if w.last(lower.SHA, w.q.Status).State != "pending" {
		t.Fatal("landing marked the lower candidate before the next read")
	}
	w.now = w.now.Add(time.Minute)
	r := keepRead(t, w)
	lowerMark := w.now
	if len(r.World.Builds) != 2 {
		t.Fatalf("both branches must stand: %+v", r.World.Builds)
	}
	for _, b := range r.World.Builds {
		want := topMark
		if b.SHA == lower.SHA {
			want = lowerMark
			if !b.Pushed.Equal(lower.Pushed) {
				t.Fatalf("first marked read lost push time: %+v", b)
			}
		}
		if !b.Landed.Equal(want) || w.last(b.SHA, w.q.Status).Description != "landed" {
			t.Fatalf("mark = %+v, want %s", b, want)
		}
	}
	w.log.Reset()
	keepTick(t, w)
	if strings.Contains(w.log.String(), `"do":`) {
		t.Fatalf("unchanged tick acted: %s", w.log)
	}
	w.now = topMark.Add(3*time.Minute - time.Nanosecond)
	keepTick(t, w)
	if len(w.o.Branches("merge-queue/")) != 2 {
		t.Fatal("branch deleted early")
	}
	w.now = topMark.Add(3 * time.Minute)
	keepTick(t, w)
	branches := w.o.Branches("merge-queue/")
	if len(branches) != 1 || branches["refs/heads/merge-queue/main/"+lower.SHA[:12]] != lower.SHA {
		t.Fatalf("only lower should stand: %v", branches)
	}
	w.now = lowerMark.Add(3 * time.Minute)
	keepTick(t, w)
	keepTick(t, w)
	if len(w.o.Branches("merge-queue/")) != 0 {
		t.Fatal("branches outlived marks plus keep")
	}
	for _, b := range []queue.Build{lower, upper} {
		if n := strings.Count(w.log.String(), `"do":"delete","sha":"`+b.SHA+`"`); n != 1 {
			t.Fatalf("%s deleted %d times: %s", b.SHA, n, w.log)
		}
	}
}

func TestKeepLowerCandidateWaitsOnlyForItsVerdict(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2,"build":["one","two"],"cancels":false}`)
			lower, upper := keepStack(t, w)
			w.q.Slots = 1
			w.queue(3, "c", "c\n")
			for _, name := range []string{"one", "two"} {
				w.check(upper.SHA, name, "success", w.now, 7)
				w.check(lower.SHA, name, "in_progress", w.now, 7)
			}
			if partial {
				w.check(lower.SHA, "one", "failure", w.now, 7)
			}
			w.now = w.now.Add(time.Minute)
			keepTick(t, w)
			r := keepRead(t, w)
			marked := w.now
			if !partial {
				if len(r.World.Builds) != 2 || w.last(lower.SHA, w.q.Status).State != "pending" {
					t.Fatalf("running build deleted or another candidate pushed: %+v", r.World.Builds)
				}
				for _, b := range r.World.Builds {
					if b.SHA == lower.SHA && (b.Verdict != queue.Pending || !b.Landed.IsZero()) {
						t.Fatalf("running lower = %+v, want pending and unmarked", b)
					}
				}
				w.now = w.now.Add(2 * time.Minute)
				for _, name := range []string{"one", "two"} {
					w.check(lower.SHA, name, "success", w.now, 7)
				}
				r = keepRead(t, w)
				marked = w.now
			}
			found := false
			for _, b := range r.World.Builds {
				if b.SHA == lower.SHA {
					found = true
					if !b.Landed.Equal(marked) {
						t.Fatalf("lower verdict did not mark at read: %+v", b)
					}
					if partial && b.Verdict != queue.Failure {
						t.Fatalf("failed context lost its verdict: %+v", b)
					}
				}
			}
			if !found {
				t.Fatal("lower branch disappeared")
			}
			keepTick(t, w)
			r = keepRead(t, w)
			if !slices.ContainsFunc(r.World.Builds, func(b queue.Build) bool { return b.SHA != lower.SHA && b.SHA != upper.SHA }) {
				t.Fatal("finished lower did not free its slot")
			}
			w.now = marked.Add(3*time.Minute - time.Nanosecond)
			keepTick(t, w)
			if w.o.Branches("merge-queue/")["refs/heads/merge-queue/main/"+lower.SHA[:12]] == "" {
				t.Fatal("lower deleted before keep")
			}
			w.now = marked.Add(3 * time.Minute)
			keepTick(t, w)
			if w.o.Branches("merge-queue/")["refs/heads/merge-queue/main/"+lower.SHA[:12]] != "" {
				t.Fatal("lower outlived keep")
			}
		})
	}
}

func TestKeepCheckRunLandingDeletesOnceAfterItsMark(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7}`)
	w.queue(1, "a", "a\n")
	if err := w.q.Tick(); err != nil {
		t.Fatal(err)
	}
	var sha string
	for _, head := range w.o.Branches("merge-queue/") {
		sha = head
	}
	if sha == "" {
		t.Fatal("no candidate")
	}
	started := w.now
	w.now = w.now.Add(5 * time.Minute)
	completed := w.now
	w.check(sha, "merge-queue/build", "success", started, 7)
	w.now = w.now.Add(time.Minute)
	tick := func() {
		t.Helper()
		if err := w.q.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if !w.o.Landed(w.pulls[1].heads[0]) {
		t.Fatalf("check-run candidate did not land: %s", w.log)
	}
	marked := w.now
	if !marked.After(completed) {
		t.Fatal("the mark should come after the check run completed")
	}
	r, err := w.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.World.Builds) != 1 || !r.World.Builds[0].Landed.Equal(marked) {
		t.Fatalf("landed check run = %+v", r.World.Builds)
	}
	w.log.Reset()
	for _, now := range []time.Time{w.now, marked.Add(3*time.Minute - time.Nanosecond)} {
		w.now = now
		tick()
		if len(w.o.Branches("merge-queue/")) != 1 {
			t.Fatal("branch deleted before its mark plus keep")
		}
	}
	w.now = marked.Add(3 * time.Minute)
	tick()
	if len(w.o.Branches("merge-queue/")) != 0 {
		t.Fatal("branch survived its mark plus keep")
	}
	tick()
	if got := strings.Count(w.log.String(), `"do":"delete"`); got != 1 {
		t.Fatalf("delete count = %d, want one: %s", got, w.log)
	}
}

func TestKeepATrunkBuildOfALandedCommitPreservesItsMark(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":1}`)
	w.queue(1, "a", "a\n")
	tick := func() {
		t.Helper()
		if err := w.q.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	var landed string
	for _, sha := range w.o.Branches("merge-queue/") {
		landed = sha
	}
	w.check(landed, "merge-queue/build", "success", w.now, 7)
	w.now = w.now.Add(time.Minute)
	tick()
	if !w.o.Landed(w.pulls[1].heads[0]) {
		t.Fatalf("the candidate did not land: %s", w.log)
	}
	marked := w.now
	w.now = w.now.Add(time.Minute)
	w.check(landed, "merge-queue/build", "in_progress", w.now, 7)
	w.queue(2, "b", "b\n")
	tick()
	r, err := w.q.Read()
	if err != nil {
		t.Fatal(err)
	}
	var kept, next int
	for _, b := range r.World.Builds {
		switch {
		case string(b.SHA) == landed:
			kept++
			if b.Verdict != queue.Pending || !b.Landed.Equal(marked) {
				t.Fatalf("the landed commit under the trunk's own build = %+v, want pending and landed at its mark %v", b, marked)
			}
		default:
			next++
		}
	}
	if kept != 1 || next != 1 {
		t.Fatalf("with one slot, the trunk's build of the landed commit held it: %d kept, %d new candidates: %s", kept, next, w.log)
	}
	w.now = marked.Add(3 * time.Minute)
	tick()
	for _, sha := range w.o.Branches("merge-queue/") {
		if sha == landed {
			t.Fatal("the landed commit's branch outlived its mark plus keep while the trunk's build ran")
		}
	}
}

func TestKeepReadRepairsACrashBeforeTheLandingMark(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2}`)
	_, upper := keepStack(t, w)
	w.check(upper.SHA, "merge-queue/build", "success", w.now, 7)
	r := keepRead(t, w)
	failed := false
	w.intercept = func(out http.ResponseWriter, req *http.Request) bool {
		if req.Method == "POST" && req.URL.Path == "/repos/o/r/statuses/"+upper.SHA && !failed {
			body, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
			var st struct{ Description string }
			if err := json.Unmarshal(body, &st); err != nil {
				t.Fatal(err)
			}
			if st.Description == "landed" {
				failed = true
				out.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(out, `{"message":"crash before mark"}`)
				return true
			}
		}
		return false
	}
	for _, a := range queue.Plan(r.World, w.q.dials()) {
		if a.Do == "land" {
			if err := w.q.land(a); err == nil {
				t.Fatal("landing mark did not fail")
			}
		}
	}
	if !failed || !w.o.Landed(w.pulls[2].heads[0]) || w.last(upper.SHA, w.q.Status).State != "pending" {
		t.Fatal("did not crash between push and mark")
	}
	w.now = w.now.Add(time.Minute)
	r = keepRead(t, w)
	for _, b := range r.World.Builds {
		if b.SHA == upper.SHA {
			if !b.Landed.Equal(w.now) || !b.Pushed.Equal(upper.Pushed) || w.last(b.SHA, w.q.Status).Description != "landed" {
				t.Fatalf("crash repair = %+v", b)
			}
			return
		}
	}
	t.Fatal("crashed landing lost its branch")
}

func TestKeepReadRepairsLandingWhileTrunksBuildRuns(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":1,"build":["one","two"]}`)
	w.queue(1, "a", "a\n")
	keepTick(t, w)
	upper := keepRead(t, w).World.Builds[0]
	for _, name := range []string{"one", "two"} {
		w.check(upper.SHA, name, "success", w.now, 7)
	}
	r := keepRead(t, w)
	failed := false
	w.intercept = func(out http.ResponseWriter, req *http.Request) bool {
		if req.Method == "POST" && req.URL.Path == "/repos/o/r/statuses/"+upper.SHA && !failed {
			body, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
			var st struct{ Description string }
			if err := json.Unmarshal(body, &st); err != nil {
				t.Fatal(err)
			}
			if st.Description == "landed" {
				failed = true
				out.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(out, `{"message":"crash before mark"}`)
				return true
			}
		}
		return false
	}
	for _, a := range queue.Plan(r.World, w.q.dials()) {
		if a.Do == "land" {
			if err := w.q.land(a); err == nil {
				t.Fatal("landing mark did not fail")
			}
		}
	}
	if !failed || !w.o.Landed(w.pulls[1].heads[0]) || w.last(upper.SHA, w.q.Status).State != "pending" {
		t.Fatal("did not crash between push and mark")
	}
	w.now = w.now.Add(time.Minute)
	for _, name := range []string{"one", "two"} {
		w.check(upper.SHA, name, "in_progress", w.now, 7)
	}
	r = keepRead(t, w)
	marked := w.now
	for _, b := range r.World.Builds {
		if b.SHA == upper.SHA {
			if !b.Landed.Equal(w.now) || !b.Pushed.Equal(upper.Pushed) || w.last(b.SHA, w.q.Status).Description != "landed" {
				t.Fatalf("crash repair = %+v", b)
			}
			break
		}
	}
	if !slices.ContainsFunc(r.World.Builds, func(b queue.Build) bool { return b.SHA == upper.SHA && b.Landed.Equal(marked) }) {
		t.Fatal("crashed landing lost its mark")
	}
	w.queue(2, "b", "b\n")
	keepTick(t, w)
	if len(keepRead(t, w).World.Builds) != 2 {
		t.Fatal("repaired landing held the only slot")
	}
	w.now = marked.Add(3*time.Minute - time.Nanosecond)
	keepTick(t, w)
	branch := "refs/heads/merge-queue/main/" + upper.SHA[:12]
	if w.o.Branches("merge-queue/")[branch] != upper.SHA {
		t.Fatal("branch deleted before mark plus keep")
	}
	w.now = marked.Add(3 * time.Minute)
	keepTick(t, w)
	keepTick(t, w)
	if w.o.Branches("merge-queue/")[branch] != "" || strings.Count(w.log.String(), `"do":"delete","sha":"`+upper.SHA+`"`) != 1 {
		t.Fatalf("branch must go once at mark plus keep: %s", w.log)
	}
}

func TestKeepReadMarksOnlyCandidates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  queue.Kind
		retry bool
	}{
		{"probe", queue.Probe, false},
		{"check", queue.Check, false},
		{"round whole build", queue.Candidate, false},
		{"retry candidate", queue.Candidate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind := tc.kind
			w := newKeepWorld(t, `{"keep":"3m","app":7,"probe":"probe"}`)
			p := w.queue(1, "a", "a\n")
			trunk, _, err := w.q.Git.Fetch([]int{1})
			if err != nil {
				t.Fatal(err)
			}
			sha, _ := w.q.Git.Merge(trunk, queue.PR{Number: 1, Head: p.heads[0]}, "")
			note := ""
			if kind == queue.Candidate && !tc.retry {
				note = "Banyan-Round: 1\nBanyan-Of: red"
			}
			if kind == queue.Probe {
				note += "\nBanyan-Probe-Of: red"
			}
			if kind == queue.Check {
				note += "\nBanyan-Check: whole"
			}
			if tc.retry {
				sha, _ = w.q.Git.Merge(trunk, queue.PR{Number: 1, Head: p.heads[0]}, "Banyan-Try: 2")
			} else {
				sha = w.q.Git.Mark(sha, note)
			}
			if err := w.q.Git.Push(sha); err != nil {
				t.Fatal(err)
			}
			w.set(sha, w.q.Status, "pending")
			w.check(sha, "merge-queue/build", "success", w.now, 7)
			w.check(sha, "probe", "success", w.now, 7)
			if err := w.q.Git.Land(sha); err != nil {
				t.Fatal(err)
			}
			r := keepRead(t, w)
			if len(r.World.Builds) != 1 {
				t.Fatalf("builds: %+v", r.World.Builds)
			}
			b := r.World.Builds[0]
			if tc.retry {
				if b.Kind != queue.Candidate || b.Round != 0 || !b.Landed.Equal(w.now) || w.last(sha, w.q.Status).State != "success" {
					t.Fatalf("retry candidate not marked: %+v", b)
				}
			} else if !b.Landed.IsZero() || w.last(sha, w.q.Status).State != "pending" {
				t.Fatalf("round build marked: %+v", b)
			}
		})
	}
}

func TestKeepDryReadWritesNoLandingMark(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2}`)
	_, upper := keepStack(t, w)
	w.check(upper.SHA, "merge-queue/build", "success", w.now, 7)
	if err := w.q.Git.Land(upper.SHA); err != nil {
		t.Fatal(err)
	}
	w.q.DryRun = true
	keepTick(t, w)
	if w.last(upper.SHA, w.q.Status).State != "pending" {
		t.Fatal("dry run wrote landed status")
	}
	w.q.DryRun = false
	r := keepRead(t, w)
	if !slices.ContainsFunc(r.World.Builds, func(b queue.Build) bool { return b.SHA == upper.SHA && b.Landed.Equal(w.now) }) {
		t.Fatal("real read did not mark the candidate")
	}
}

func TestKeepReadDoesNotWaitForAnAbsentContext(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2,"build":["one","two"]}`)
	lower, upper := keepStack(t, w)
	w.check(lower.SHA, "one", "failure", w.now, 7)
	for _, name := range []string{"one", "two"} {
		w.check(upper.SHA, name, "success", w.now, 7)
	}
	keepLand(t, w, upper.SHA)
	w.now = w.now.Add(time.Minute)
	r := keepRead(t, w)
	for _, b := range r.World.Builds {
		if b.SHA == lower.SHA {
			if b.Verdict != queue.Failure || !b.Landed.Equal(w.now) {
				t.Fatalf("absent context delayed marking: %+v", b)
			}
			return
		}
	}
	t.Fatal("lower branch gone")
}

func TestKeepCancelsRunningLowerCandidateAtOnce(t *testing.T) {
	w := newKeepWorld(t, `{"keep":"3m","app":7,"slots":2,"cancels":true}`)
	lower, upper := keepStack(t, w)
	w.check(lower.SHA, "merge-queue/build", "in_progress", w.now, 7)
	w.check(upper.SHA, "merge-queue/build", "success", w.now, 7)
	keepTick(t, w)
	if w.o.Branches("merge-queue/")["refs/heads/merge-queue/main/"+lower.SHA[:12]] != "" || w.last(lower.SHA, w.q.Status).State != "pending" {
		t.Fatal("running lower was marked or retained where CI cancels")
	}
}
