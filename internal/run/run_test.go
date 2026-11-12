package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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
}

type pull struct {
	number   int
	heads    []string
	labels   map[string]time.Time
	draft    bool
	base     string
	comments []comment
	merged   bool
	// threads is its review threads, true where resolved, and unread is how many more GitHub
	// holds past the page it returns.
	threads []bool
	unread  int
	// forced is the head a force-push replaced, and when.
	forced   string
	forcedAt time.Time
}

type comment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type status struct {
	Context, State, Description, URL string
	At                               time.Time
}

func (h *hub) open(n int, file, content string) *pull {
	p := &pull{number: n, heads: []string{h.o.PR(n, file, content)}, labels: map[string]time.Time{}, base: "main"}
	h.pulls[n] = p
	return p
}

func (h *hub) set(sha, context, state string) {
	h.statuses[sha] = append(h.statuses[sha], status{context, state, "", "https://ci.example/" + sha[:12], h.now})
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
	var labels, events, commits []any
	threads := []any{}
	for _, resolved := range p.threads {
		threads = append(threads, map[string]any{"isResolved": resolved})
	}
	for name, at := range p.labels {
		labels = append(labels, map[string]any{"name": name})
		events = append(events, map[string]any{"createdAt": at, "label": map[string]any{"name": name}})
	}
	if p.forced != "" {
		events = append(events, map[string]any{"createdAt": p.forcedAt, "beforeCommit": map[string]any{"oid": p.forced}})
	}
	for _, sha := range p.heads {
		commits = append(commits, map[string]any{"commit": map[string]any{"oid": sha, "statusCheckRollup": h.rollup(sha)}})
	}
	return map[string]any{"number": p.number, "isDraft": p.draft, "merged": p.merged, "closed": p.merged, "baseRefName": p.base,
		"headRefName": fmt.Sprintf("pr%d", p.number), "headRepositoryOwner": map[string]any{"login": "o"},
		"headRefOid": p.heads[len(p.heads)-1], "labels": map[string]any{"nodes": labels}, "timelineItems": map[string]any{"nodes": events},
		"reviewThreads": map[string]any{"totalCount": len(p.threads) + p.unread, "nodes": threads}, "commits": map[string]any{"nodes": commits}}
}

func (h *hub) rollup(sha string) any {
	newest := map[string]status{}
	for _, s := range h.statuses[sha] {
		newest[s.Context] = s
	}
	if len(newest) == 0 {
		return nil
	}
	var nodes []any
	for _, s := range newest {
		nodes = append(nodes, map[string]any{"__typename": "StatusContext", "context": s.Context, "state": strings.ToUpper(s.State),
			"description": s.Description, "targetUrl": s.URL, "createdAt": s.At})
	}
	return map[string]any{"contexts": map[string]any{"nodes": nodes}}
}

var commitAlias = regexp.MustCompile(`(c\d+): object\(oid: "([0-9a-f]+)"\)`)

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
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
		case strings.Contains(q.Query, "query Pull"):
			if p := h.pulls[int(q.Variables["number"].(float64))]; p != nil {
				repo["pullRequest"] = h.node(p)
			}
		default:
			for _, m := range commitAlias.FindAllStringSubmatch(q.Query, -1) {
				if _, err := gittest.Try(h.o.Bare, "git", "cat-file", "-e", m[2]); err == nil {
					repo[m[1]] = map[string]any{"statusCheckRollup": h.rollup(m[2])}
				}
			}
		}
		reply(map[string]any{"data": map[string]any{"repository": repo}})
	case parts[0] == "statuses" && r.Method == "POST":
		var s struct{ Context, State, Description, Target_url string }
		json.Unmarshal(body, &s)
		h.statuses[parts[1]] = append(h.statuses[parts[1]], status{s.Context, s.State, s.Description, s.Target_url, h.now})
		reply(map[string]any{})
	case parts[0] == "commits" && len(parts) == 3:
		out := []any{}
		for _, s := range h.statuses[parts[1]] {
			out = append(out, map[string]any{"context": s.Context})
		}
		reply(out)
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
				}
			}
			reply([]any{})
		case r.Method == "DELETE":
			delete(p.labels, parts[3])
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
	built []string
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
	return w
}

// queue opens a pull request that writes a file, and queues it.
func (w *world) queue(n int, file, content string) *pull {
	p := w.open(n, file, content)
	w.now = w.now.Add(time.Second)
	if _, err := w.q.Add(n, false); err != nil {
		w.t.Fatalf("adding #%d: %v", n, err)
	}
	return p
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
		if w.last(sha, context).State != "" || (w.slow != nil && w.slow(sha)) {
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
	w.queue(1, "a", "1\n")
	w.queue(2, "b", "2\n")
	w.rounds(10)
	if !w.queued(1) || !w.queued(2) || len(w.pulls[1].comments)+len(w.pulls[2].comments) != 0 {
		t.Fatalf("a red trunk took a pull request out\n%s", w.log)
	}
	broken = false
	w.rounds(8)
	if !w.landed(1) || !w.landed(2) {
		t.Errorf("after the trunk passed again #1 landed %v and #2 landed %v\n%s", w.landed(1), w.landed(2), w.log)
	}
}

func TestAConflictWithTheTrunkAndAMovedHeadLeave(t *testing.T) {
	w := newWorld(t, `{}`)
	one := w.queue(1, "same", "one\n")
	w.o.Outside("same", "other\n")
	two := w.queue(2, "b", "2\n")
	w.round()
	two.heads = append(two.heads, w.o.Amend(2, "b", "2 again\n"))
	w.rounds(2)
	if w.queued(1) || !w.said(1, "attributed=conflict") {
		t.Errorf("#1 conflicts with the trunk: queued %v, comments %+v", w.queued(1), one.comments)
	}
	if w.queued(2) || w.landed(2) || !w.said(2, "<!-- merge-queue eject "+two.heads[0]+" attributed=moved -->") {
		t.Errorf("#2's head moved: queued %v, landed %v, comments %+v\n%s", w.queued(2), w.landed(2), two.comments, w.log)
	}
	if s := w.last(two.heads[0], "merge-queue"); s.State != "error" {
		t.Errorf("the status on #2's old head is %q", s.State)
	}
	if _, err := w.q.Add(2, false); err != nil {
		t.Fatal(err)
	}
	w.rounds(3)
	if !w.landed(2) || !w.o.Holds(w.o.Trunk(), "b") {
		t.Errorf("#2, queued again at its new head, did not land\n%s", w.log)
	}
}

func TestAForcePushAfterItWasQueuedTakesAPullRequestOut(t *testing.T) {
	w := newWorld(t, `{}`)
	p := w.queue(1, "a", "1\n")
	was := p.heads[0]
	w.now = w.now.Add(time.Second)
	// The old head, and the queue's status on it, are no longer among its commits.
	p.heads, p.forced, p.forcedAt = []string{w.o.Amend(1, "a", "1 again\n")}, was, w.now
	w.rounds(3)
	if w.queued(1) || w.landed(1) || !w.said(1, "<!-- merge-queue eject "+was+" attributed=moved -->") {
		t.Errorf("queued %v, landed %v, comments %+v\n%s", w.queued(1), w.landed(1), p.comments, w.log)
	}
	if _, err := w.q.Add(1, false); err != nil {
		t.Fatal(err)
	}
	w.rounds(3)
	if !w.landed(1) {
		t.Errorf("#1, queued again at its new head, did not land\n%s", w.log)
	}
}

func TestARemovalRefusesTheLandingUnderWay(t *testing.T) {
	w := newWorld(t, `{}`)
	w.queue(1, "a", "1\n")
	w.round() // pushed, and CI passed it
	before := w.o.Trunk()
	if err := w.q.Remove(1, "a person", "not yet"); err != nil {
		t.Fatal(err)
	}
	w.rounds(2)
	if w.landed(1) || w.o.Trunk() != before || !w.said(1, "attributed=removed") {
		t.Errorf("#1 was removed and landed %v; comments %+v", w.landed(1), w.pulls[1].comments)
	}
}

func TestAReviewWithdrawnAsTheLandingBeginsStopsIt(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff", "review"]}`)
	p := w.open(1, "a", "1\n")
	w.set(p.heads[0], "signoff", "success")
	w.set(p.heads[0], "review", "success")
	if _, err := w.q.Add(1, false); err != nil {
		t.Fatal(err)
	}
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

func TestAnAdmissionCheckPassesOnAnyOfItsAlternatives(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff", "review|factory/review"]}`)
	ours, theirs, neither, failed := w.open(1, "a", "1\n"), w.open(2, "b", "2\n"), w.open(3, "c", "3\n"), w.open(4, "d", "4\n")
	for _, p := range []*pull{ours, theirs, neither, failed} {
		w.set(p.heads[0], "signoff", "success")
	}
	w.set(ours.heads[0], "review", "success")
	w.set(theirs.heads[0], "factory/review", "success")
	w.set(theirs.heads[0], "review", "failure")
	w.set(failed.heads[0], "factory/review", "failure")
	for n := 1; n <= 3; n++ {
		if _, err := w.q.Add(n, false); err != nil {
			t.Fatalf("adding #%d: %v", n, err)
		}
	}
	if _, err := w.q.Add(4, false); err == nil || !strings.HasPrefix(err.Error(), "refused #4: checks: ") {
		t.Errorf("no alternative passed and one failed: %v", err)
	}
	w.rounds(5)
	if !w.landed(1) || !w.landed(2) || w.landed(3) || !w.queued(3) {
		t.Errorf("landed: ours %v, theirs %v, neither %v\n%s", w.landed(1), w.landed(2), w.landed(3), w.log)
	}
}

func TestAddRefusesInOneLineAndTwiceIsOnce(t *testing.T) {
	w := newWorld(t, `{"admit": ["signoff"]}`)
	draft := w.open(1, "a", "1\n")
	draft.draft = true
	if _, err := w.q.Add(1, false); err == nil || err.Error() != "refused #1: ready: it is a draft" {
		t.Errorf("a draft: %v", err)
	}
	red := w.open(2, "b", "2\n")
	w.set(red.heads[0], "signoff", "failure")
	if _, err := w.q.Add(2, false); err == nil || !strings.HasPrefix(err.Error(), "refused #2: checks: ") {
		t.Errorf("red checks: %v", err)
	}
	ok := w.open(3, "c", "3\n")
	for i := 0; i < 2; i++ {
		if said, err := w.q.Add(3, false); err != nil || said != "queued #3 at "+ok.heads[0] {
			t.Errorf("adding #3: %q, %v", said, err)
		}
	}
	// Its own checks have not passed, so it waits in place and says so.
	w.rounds(2)
	if e, _ := w.q.Entries(3); len(e) != 1 || e[0].State != "held" || !w.queued(3) {
		t.Errorf("#3 waits on its checks: %+v", e)
	}
	w.set(ok.heads[0], "signoff", "success")
	w.rounds(3)
	if e, _ := w.q.Entries(3); len(e) != 1 || e[0].State != "landed" {
		t.Errorf("#3 after its checks passed: %+v\n%s", e, w.log)
	}
}

func TestAnUnresolvedThreadKeepsAPullRequestOutAndSoDoThreadsTheQueueCannotRead(t *testing.T) {
	w := newWorld(t, `{"threads": true}`)
	open := w.open(1, "a", "1\n")
	open.threads = []bool{true, false}
	if _, err := w.q.Add(1, false); err == nil || err.Error() != "refused #1: ready: 1 review threads are unresolved" {
		t.Errorf("an unresolved thread: %v", err)
	}
	// GitHub hands over the first hundred threads, and any past them may be unresolved.
	long := w.open(2, "b", "2\n")
	long.threads, long.unread = []bool{true}, 1
	if _, err := w.q.Add(2, false); err == nil || err.Error() != "refused #2: ready: it has more review threads than the queue reads" {
		t.Errorf("threads past the page: %v", err)
	}
	open.threads[1] = true
	if _, err := w.q.Add(1, false); err != nil {
		t.Errorf("every thread resolved: %v", err)
	}
	w.rounds(3)
	if !w.landed(1) {
		t.Errorf("#1 never landed\n%s", w.log)
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
	w := newWorld(t, `{"slots": 2}`)
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
	// #1; #2 and #2 with #3; the round's two checks; #3 beside it.
	if len(w.built) != 6 {
		t.Errorf("CI built %d commits, and the round begun before the trunk moved should have decided it in 6\n%s", len(w.built), w.log)
	}
	if left := w.o.Branches("merge-queue/"); len(left) != 0 {
		t.Errorf("branches left behind: %v", left)
	}
}
