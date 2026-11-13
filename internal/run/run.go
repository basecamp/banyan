// Package run takes a tick against a real repository: it reads the world from git and
// GitHub, plans, and does what the plan says.
package run

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/basecamp/banyan/internal/git"
	"github.com/basecamp/banyan/internal/github"
	"github.com/basecamp/banyan/internal/queue"
)

// Config is a repository's queue, as its config file says it. Every field has a default.
type Config struct {
	Repo     string   `json:"repo"`     // owner/name
	Remote   string   `json:"remote"`   // origin
	Trunk    string   `json:"trunk"`    // main
	Label    string   `json:"label"`    // merge-queue
	First    string   `json:"first"`    // merge-queue-first
	Hold     string   `json:"hold"`     // merge-queue-hold
	Branches string   `json:"branches"` // merge-queue: builds are pushed to <branches>/<trunk>/<sha12>
	Status   string   `json:"status"`   // merge-queue: the queue's own context
	Build    []string `json:"build"`    // merge-queue/build: the contexts that judge a whole build
	Probe    string   `json:"probe"`    // the context that judges a probe; without one there are no probes
	Admit    []string `json:"admit"`    // the contexts a pull request's head must pass to join the chain
	Threads  bool     `json:"threads"`  // an unresolved review thread keeps a pull request out
	Cancels  bool     `json:"cancels"`  // CI stops a build whose branch is deleted
	Started  string   `json:"started"`  // a word CI's pending status carries once the build runs; a check run says so itself
	Slots    int      `json:"slots"`
	MaxBatch int      `json:"max_batch"`
	Timeout  string   `json:"timeout"`
	Recheck  string   `json:"recheck"`
	Author   struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
}

// Load reads a config file, a missing one being all defaults.
func Load(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &c)
	} else if os.IsNotExist(err) {
		err = nil
	}
	def := func(s *string, d string) {
		if *s == "" {
			*s = d
		}
	}
	def(&c.Remote, "origin")
	def(&c.Trunk, "main")
	def(&c.Label, "merge-queue")
	def(&c.First, c.Label+"-first")
	def(&c.Hold, c.Label+"-hold")
	def(&c.Branches, "merge-queue")
	def(&c.Status, "merge-queue")
	def(&c.Timeout, "90m")
	def(&c.Recheck, "10m")
	def(&c.Author.Name, "Banyan")
	def(&c.Author.Email, "banyan@users.noreply.github.com")
	if len(c.Build) == 0 {
		c.Build = []string{c.Status + "/build"}
	}
	if c.Slots == 0 {
		c.Slots = 4
	}
	if c.MaxBatch == 0 {
		c.MaxBatch = 1
	}
	return c, err
}

func (c Config) names() github.Names {
	return github.Names{Trunk: c.Trunk, Label: c.Label, First: c.First, Hold: c.Hold, Status: c.Status, Admit: c.Admit, Threads: c.Threads}
}

func (c Config) dials() queue.Config {
	timeout, _ := time.ParseDuration(c.Timeout)
	recheck, _ := time.ParseDuration(c.Recheck)
	return queue.Config{Slots: c.Slots, MaxBatch: c.MaxBatch, Probes: c.Probe != "", Cancels: c.Cancels, Timeout: timeout, Recheck: recheck}
}

// Queue is one repository's queue, ready to tick.
type Queue struct {
	Config
	Git *git.Repo
	Hub *github.Client
	Now func() time.Time
	// Log takes one JSON line per action, and Record, where set, is the folder each tick's
	// recording is written to.
	Log    io.Writer
	Record string
	DryRun bool
	// refused is the landing the remote refused earlier in this tick.
	refused queue.SHA
}

// New is the queue of the clone at dir.
func New(c Config, dir, api, token string) *Queue {
	q := &Queue{Config: c, Now: time.Now, Log: os.Stdout,
		Hub: &github.Client{API: api, Repo: c.Repo, Token: token},
		Git: &git.Repo{Dir: dir, Remote: c.Remote, Trunk: c.Trunk, Lane: c.Branches, Name: c.Author.Name, Email: c.Author.Email, Token: token}}
	q.Git.Pushed = func(sha queue.SHA) bool {
		seen, err := q.Hub.Pushed(sha, c.Status)
		if err != nil {
			panic(err) // a tick that cannot tell must not guess: Tick recovers and fails
		}
		return seen
	}
	return q
}

// Read is the world as git and GitHub hold it now, with what the executor needs beside it.
type Read struct {
	World    queue.World
	Pulls    map[int]github.Pull
	Statuses map[queue.SHA]map[string]github.Status
}

func (q *Queue) Read() (*Read, error) {
	pulls, err := q.Hub.Queue(q.names())
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, p := range pulls {
		numbers = append(numbers, p.Number)
	}
	trunk, heads, err := q.Git.Fetch(numbers)
	if err != nil {
		return nil, err
	}
	r := &Read{World: queue.World{Now: q.Now(), Trunk: trunk, Repo: q.Git}, Pulls: map[int]github.Pull{}}
	for _, p := range pulls {
		if q.Git.Holds(trunk, heads[p.Number]) {
			continue // it has landed, and GitHub has yet to say so
		}
		if heads[p.Number] != p.Head {
			p.Wait = "its newest push is still arriving"
		}
		r.Pulls[p.Number] = p
		r.World.PRs = append(r.World.PRs, p.PR)
	}
	builds, err := q.Git.Builds(trunk)
	if err != nil {
		return nil, err
	}
	var shas []queue.SHA
	for _, b := range builds {
		shas = append(shas, b.SHA)
	}
	if r.Statuses, err = q.Hub.Commits(shas); err != nil {
		return nil, err
	}
	for _, b := range builds {
		st := r.Statuses[b.SHA]
		contexts := q.Build
		if b.Kind == queue.Probe {
			contexts = []string{q.Probe}
		}
		b.Verdict, _ = github.Verdict(st, contexts)
		b.Pushed = st[q.Status].At
		for _, c := range contexts {
			if s := st[c]; s.State == queue.Pending && (s.Running || q.Started != "" && strings.Contains(s.Description, q.Started)) {
				b.Started = s.At
			}
		}
		if _, marked := st[q.Status]; !marked {
			// A tick died between its push and its mark. The wait starts now.
			b.Pushed = q.Now()
			if !q.DryRun {
				if err := q.Hub.SetStatus(b.SHA, q.Status, queue.Pending, describe(b), ""); err != nil {
					return nil, err
				}
			}
		}
		r.World.Builds = append(r.World.Builds, b)
	}
	return r, nil
}

func describe(b queue.Build) string {
	var ns []string
	for _, m := range b.Chain {
		ns = append(ns, fmt.Sprintf("#%d", m.Number))
	}
	what := map[queue.Kind]string{queue.Candidate: "candidate", queue.Check: "check", queue.Probe: "probe"}[b.Kind]
	if len(ns) == 0 {
		return what + " of the trunk alone"
	}
	return what + " of " + strings.Join(ns, ", ")
}

// Tick reads, plans and acts until the plan is empty: a landing changes the world, so the
// tick reads it again and goes on, six times at most.
func (q *Queue) Tick() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the tick stopped: %v", p)
		}
	}()
	if err := q.settle(); err != nil {
		return err
	}
	q.refused = ""
	for pass := 0; pass < 6; pass++ {
		r, err := q.Read()
		if err != nil {
			return err
		}
		rec := queue.Record(r.World, q.dials())
		if q.Record != "" {
			data, _ := json.MarshalIndent(rec, "", " ")
			name := fmt.Sprintf("%s-%d.json", r.World.Now.UTC().Format("20060102T150405Z"), pass)
			if err := os.WriteFile(filepath.Join(q.Record, name), data, 0o644); err != nil {
				return err
			}
		}
		for _, a := range rec.Actions {
			line, _ := json.Marshal(struct {
				At time.Time `json:"at"`
				queue.Action
			}{r.World.Now.UTC(), a})
			fmt.Fprintln(q.Log, string(line))
			if q.DryRun {
				continue
			}
			if err := q.apply(r, a); err != nil {
				err = fmt.Errorf("%s: %w", a.Do, err)
				q.stopped(r, err)
				return err
			}
		}
		if len(rec.Actions) == 0 || q.DryRun {
			return nil
		}
	}
	return nil
}

// reason is the line of an error that says why: the rule a remote names as it refuses a push,
// as GitHub lists them, or else its last line.
func reason(why error) string {
	lines := strings.Split(strings.TrimSpace(why.Error()), "\n")
	for _, line := range lines {
		if rule, ok := strings.CutPrefix(strings.TrimSpace(line), "remote: - "); ok {
			before, _, _ := strings.Cut(lines[0], ":")
			return before + ": " + strings.TrimSpace(rule)
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// stopped says on every queued pull request why the queue could not go on, once: the next
// tick that can act writes its place over it.
func (q *Queue) stopped(r *Read, why error) {
	text := "the queue is stopped: " + reason(why)
	if len(text) > 140 {
		text = text[:140]
	}
	for _, pr := range r.World.PRs {
		if pr.Note != text {
			q.Hub.SetStatus(pr.Head, q.Status, queue.Pending, text, "")
		}
	}
}

func (q *Queue) apply(r *Read, a queue.Action) error {
	pr := r.Pulls[a.PR]
	switch a.Do {
	case "note":
		return q.Hub.SetStatus(pr.Head, q.Status, queue.Pending, a.Text, "")
	case "push":
		if err := q.Git.Push(a.Build.SHA); err != nil {
			return err
		}
		return q.Hub.SetStatus(a.Build.SHA, q.Status, queue.Pending, describe(a.Build), "")
	case "delete":
		return q.Git.Delete(a.SHA)
	case "eject":
		return q.eject(r, pr, a)
	case "land":
		return q.land(a)
	}
	return fmt.Errorf("the plan asked for %q, which nothing does", a.Do)
}

// eject says why on the pull request, ends its status and takes its label off, in that
// order: the label is what keeps it in the world, so a tick that dies part way ejects again.
func (q *Queue) eject(r *Read, pr github.Pull, a queue.Action) error {
	at := pr.Head
	body := fmt.Sprintf("The merge queue took this pull request out at `%.12s`: %s.", at, a.Text)
	link := ""
	switch a.Cause {
	case "culprit":
		for i, sha := range a.Evidence {
			what := "failed with it"
			if i == 0 {
				what = "passed without it"
			}
			// The build that judged it: with several, the one that failed it.
			url, want := "", queue.Failure
			if i == 0 {
				want = queue.Success
			}
			for _, c := range append(slices.Clone(q.Build), q.Probe) {
				if s := r.Statuses[sha][c]; s.URL != "" && (url == "" || s.State == want) {
					url = s.URL
				}
			}
			if url == "" {
				url = fmt.Sprintf("`%.12s`", sha)
			} else if i > 0 {
				link = url
			}
			body += fmt.Sprintf("\n- %s: %s", what, url)
		}
		body += "\n\nFix it and queue it again. If the failure isn't this pull request's, queue it again as it is."
	case "conflict":
		body += fmt.Sprintf("\n\nMerge `%s` in and queue it again.", q.Trunk)
	}
	marker := fmt.Sprintf("<!-- %s eject %s attributed=%s -->", q.Status, at, a.Cause)
	if err := q.Hub.Comment(pr.Number, body, marker, pr.Queued); err != nil {
		return err
	}
	if err := q.Hub.SetStatus(at, q.Status, queue.Failure, "left the queue: "+a.Text, link); err != nil {
		return err
	}
	return q.Hub.Unlabel(pr.Number, q.Label)
}

// land reads the world once more and moves the trunk only if the landing still holds there.
// A landing the world refuses, or the remote refuses once, is not an error: the next pass plans
// again. One the remote refuses twice in a tick lost no race, and stops the queue.
func (q *Queue) land(a queue.Action) error {
	r, err := q.Read()
	if err != nil {
		return err
	}
	if err := queue.Verify(r.World, a); err != nil {
		fmt.Fprintf(q.Log, "{\"skipped\":%q}\n", err.Error())
		return nil
	}
	if err := q.Git.Land(a.SHA); err != nil {
		fmt.Fprintf(q.Log, "{\"refused\":%q}\n", err.Error())
		if q.refused != a.SHA {
			q.refused = a.SHA
			return nil
		}
		return err
	}
	if err := q.Hub.SetStatus(a.SHA, q.Status, queue.Success, "landed", ""); err != nil {
		return err
	}
	for _, m := range a.Members {
		if err := q.Hub.SetStatus(m.Head, q.Status, queue.Success, fmt.Sprintf("landed as %.12s", a.SHA), ""); err != nil {
			return err
		}
	}
	return nil
}

// settle ends the status of a pull request that landed while a tick died before saying so.
func (q *Queue) settle() error {
	landed, err := q.Hub.Landed(q.names())
	if err != nil || q.DryRun {
		return err
	}
	for _, p := range landed {
		if p.Merged && p.Note != "" {
			if err := q.Hub.SetStatus(p.Head, q.Status, queue.Success, "landed", ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// Add queues a pull request, or says in one line why it may not be queued. Queuing one that
// is queued already changes nothing.
func (q *Queue) Add(number int, first bool) (string, error) {
	p, err := q.Hub.Pull(q.names(), number)
	if err != nil {
		return "", err
	}
	refuse := func(gate, why string) (string, error) {
		return "", fmt.Errorf("refused #%d: %s: %s", number, gate, why)
	}
	switch {
	case p.Merged:
		return refuse("merged", "it has landed")
	case p.Closed:
		return refuse("closed", "it is closed")
	case p.Wait != "":
		return refuse("ready", p.Wait)
	case p.Checks == queue.Failure || p.Checks == queue.Error:
		return refuse("checks", p.Gate+" failed at "+p.Head[:12])
	}
	if q.DryRun {
		return fmt.Sprintf("would queue #%d at %s", number, p.Head), nil
	}
	if err := q.Hub.Label(number, q.Label); err != nil {
		return "", err
	}
	if first {
		if err := q.Hub.Label(number, q.First); err != nil {
			return "", err
		}
	}
	if p.Note == "" {
		if err := q.Hub.SetStatus(p.Head, q.Status, queue.Pending, "queued", ""); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("queued #%d at %s", number, p.Head), nil
}

// Remove takes a pull request out of the queue on someone's word. It deletes the branches
// of the builds that hold it first, which refuses a landing already under way.
func (q *Queue) Remove(number int, who, why string) error {
	r, err := q.Read()
	if err != nil {
		return err
	}
	pr, queued := r.Pulls[number]
	if !queued {
		return fmt.Errorf("#%d is not queued", number)
	}
	for _, b := range r.World.Builds {
		for _, m := range b.Chain {
			if m.Number == number {
				if err := q.Git.Delete(b.SHA); err != nil {
					return err
				}
				break
			}
		}
	}
	if err := q.Hub.Unlabel(number, q.Label); err != nil {
		return err
	}
	at := pr.Head
	marker := fmt.Sprintf("<!-- %s eject %s attributed=removed -->", q.Status, at)
	body := fmt.Sprintf("%s took this pull request out of the merge queue at `%.12s`: %s", who, at, why)
	if err := q.Hub.Comment(number, body, marker, pr.Queued); err != nil {
		return err
	}
	return q.Hub.SetStatus(at, q.Status, queue.Error, "left the queue: removed by "+who, "")
}

// Entry is one pull request's place in the queue, as `status` prints it.
type Entry struct {
	Number    int        `json:"number"`
	Head      queue.SHA  `json:"head"`
	State     string     `json:"state"` // queued, candidate, held, landed, ejected or none
	Note      string     `json:"note"`
	Candidate queue.SHA  `json:"candidate,omitempty"`
	URL       string     `json:"url,omitempty"`
	QueuedAt  *time.Time `json:"queued_at,omitempty"`
	At        *time.Time `json:"at,omitempty"` // when it landed or left
}

// Entries is the queue, in the order it was queued, or the one pull request named, which may
// have landed or left.
func (q *Queue) Entries(number int) ([]Entry, error) {
	r, err := q.Read()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, pr := range r.World.PRs {
		if number != 0 && pr.Number != number {
			continue
		}
		queued := pr.Queued
		e := Entry{Number: pr.Number, Head: pr.Head, State: "queued", Note: pr.Note, QueuedAt: &queued}
		if strings.HasPrefix(pr.Note, "waits:") {
			e.State = "held"
		}
		// Its candidate is the lowest build that holds it, the first whose green lands it.
		depth := 0
		for _, b := range r.World.Builds {
			for _, m := range b.Chain {
				if m.Number == pr.Number && m.Head == pr.Head && b.Kind == queue.Candidate && (depth == 0 || len(b.Chain) < depth) {
					depth, e.State, e.Candidate = len(b.Chain), "candidate", b.SHA
					for _, c := range q.Build {
						if s := r.Statuses[b.SHA][c]; s.URL != "" {
							e.URL = s.URL
						}
					}
				}
			}
		}
		out = append(out, e)
	}
	if number == 0 || len(out) > 0 {
		return out, nil
	}
	p, err := q.Hub.Pull(q.names(), number)
	if err != nil {
		return nil, err
	}
	e := Entry{Number: number, Head: p.Head, State: "none"}
	if s, ok := p.Statuses[q.Status]; ok {
		at := s.At
		e.Note, e.URL, e.At = s.Description, s.URL, &at
		switch {
		case s.State == queue.Success || p.Merged:
			e.State = "landed"
		case s.State != queue.Pending:
			e.State = "ejected"
		}
	}
	return []Entry{e}, nil
}
