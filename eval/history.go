package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// Act is one thing a recorded window did to a pull request: a person labelled it or took the
// label off, a head was pushed, the head's own checks reported, it came to conflict with the
// trunk or with a pull request ahead, or the queue of the time took it out for breaking the
// build.
type Act struct {
	At      time.Duration
	Number  int
	Do      string        // label, unlabel, head, checks, conflict, hold or ejected
	Head    queue.SHA     // head, checks
	First   bool          // label: it goes first in line
	Breaks  bool          // head: it fails the whole build
	Verdict queue.Verdict // checks
	Ahead   int           // hold: the pull request ahead that it conflicts with
	After   []int         // conflict: the pull requests whose landing together brought it
	Mended  time.Duration // conflict, hold: when the head that mends it was pushed
}

// script is where a replay stands in its acts, and what it saw happen to each pull request.
type script struct {
	numbers  []int
	acts     map[int][]Act
	behind   map[int]time.Duration // how much later than recorded its author acts, having been ejected later
	labelled map[int]time.Duration // when it was first labelled, as recorded
	mended   map[int]time.Duration
	waited   map[int]bool          // it waited on a conflict, or was ejected
	blamed   map[int]time.Duration // when it was first ejected for breaking the build
	spared   int                   // conflicts that came to a pull request already landed
}

func newScript(acts []Act) *script {
	h := &script{acts: map[int][]Act{}, behind: map[int]time.Duration{}, labelled: map[int]time.Duration{}, blamed: map[int]time.Duration{}, mended: map[int]time.Duration{}, waited: map[int]bool{}}
	for _, a := range acts {
		if _, ok := h.acts[a.Number]; !ok {
			h.numbers = append(h.numbers, a.Number)
		}
		h.acts[a.Number] = append(h.acts[a.Number], a)
	}
	sort.Ints(h.numbers)
	return h
}

// play does every act whose time has come, each pull request's in the order recorded.
func (s *Sim) play() {
	h := s.history
	if h == nil {
		return
	}
	for _, n := range h.numbers {
		for len(h.acts[n]) > 0 && (h.acts[n][0].At+h.behind[n] <= s.now || s.early(h.acts[n][0])) && s.act(h.acts[n][0]) {
			h.acts[n] = h.acts[n][1:]
		}
	}
}

// early says an act comes sooner than recorded: a conflict, once one of the pull requests that
// brought it has landed, and the record's ejection of a pull request that broke the build, once
// this queue has ejected it. What its author did next comes that much sooner too.
func (s *Sim) early(a Act) bool {
	p := s.prs[a.Number]
	early := a.Do == "conflict" && p != nil && !p.landed && slices.ContainsFunc(a.After, func(n int) bool { return s.prs[n] != nil && s.prs[n].landed })
	if _, blamed := s.history.blamed[a.Number]; early || (a.Do == "ejected" && blamed) {
		s.history.behind[a.Number] = s.now - a.At
		return true
	}
	return false
}

// blame notes when this queue first ejected a pull request that broke the build.
func (h *script) blame(n int, at time.Duration) {
	if h == nil {
		return
	}
	if _, ok := h.blamed[n]; !ok {
		h.blamed[n] = at
	}
}

// act does one act, and says whether it could. The record's ejection of a pull request that
// breaks the build waits for this queue's: what its author did next, they did on being told.
func (s *Sim) act(a Act) bool {
	h, p := s.history, s.prs[a.Number]
	if p == nil {
		p = &simPR{}
		p.Number, p.Head, p.Checks = a.Number, queue.SHA(fmt.Sprintf("head%d", a.Number)), queue.Success
		s.prs[a.Number], s.byHead[p.Head] = p, a.Number
		s.next = max(s.next, a.Number)
	}
	if p.landed {
		if a.Do == "conflict" && !s.passOn(a) {
			h.spared++
		}
		return true
	}
	switch a.Do {
	case "label":
		if _, ok := h.labelled[a.Number]; !ok {
			h.labelled[a.Number] = a.At
		}
		p.First = p.First || a.First
		if !p.queued {
			p.queued, p.gone, p.back, p.ready = true, false, 0, s.now
			p.Queued = s.epoch.Add(s.now)
		}
	case "unlabel":
		p.queued = false
	case "head":
		if (p.conflict || p.ahead > 0) && a.At >= h.mended[a.Number]-2*time.Minute {
			p.conflict, p.ahead = false, 0
		}
		p.Head, p.Checks, p.checksGreen, p.breaks, p.Note = a.Head, queue.Pending, never, a.Breaks, ""
		s.byHead[p.Head] = p.Number
	case "checks":
		if p.Head == a.Head {
			p.Checks, p.ready = a.Verdict, s.now
		}
	case "conflict":
		p.conflict, h.mended[a.Number], h.waited[a.Number] = true, a.Mended, true
	case "hold":
		p.ahead, h.mended[a.Number] = a.Ahead, a.Mended
	case "ejected":
		if p.queued && p.breaks {
			return false
		}
		p.breaks, h.behind[a.Number] = false, s.now-a.At
	}
	return true
}

// passOn gives a conflict that came to a pull request already landed to the first still
// queued of those whose landing brought it in the record: the two conflict whichever lands
// first. Its author mends it as soon after as the record's was mended.
func (s *Sim) passOn(a Act) bool {
	h := s.history
	for _, n := range a.After {
		p := s.prs[n]
		if p == nil || !p.queued || p.conflict {
			continue
		}
		mended := s.now + a.Mended - a.At - h.behind[n]
		p.conflict, h.mended[n], h.waited[n] = true, mended, true
		head := queue.SHA(fmt.Sprintf("%s+", p.Head))
		acts := append(h.acts[n], Act{At: mended, Number: n, Do: "head", Head: head}, Act{At: mended + 16*time.Minute, Number: n, Do: "checks", Head: head, Verdict: queue.Success})
		sort.SliceStable(acts, func(i, j int) bool { return acts[i].At < acts[j].At })
		h.acts[n] = acts
		return true
	}
	return false
}

// trace is a queue's recorded window, as scratch tooling wrote it from GitHub's record: times
// are seconds from the window's start, and a null is a time the record does not hold.
type trace struct {
	Window struct{ From, To time.Time }
	PRs    []tracePR
	Trunk  []struct {
		SHA string
		PRs []int
	}
	Candidates []struct {
		Attempts []struct {
			Started, Ended float64
			Conclusion     string
		}
	}
}

type tracePR struct {
	Number int
	Labels []struct {
		At    float64
		Label string
		On    bool
		By    string
	}
	Heads []struct {
		At     *float64
		Done   *float64 `json:"checks_done"`
		Checks *string
	}
	Conflicts []struct {
		At     float64
		Kind   string
		Onto   string
		Ahead  int
		Mended *float64
		By     string `json:"mended_by"`
	}
	Ejections []struct {
		At         float64
		Cause      string
		Attributed string
	}
	Merged, Closed *float64
	Breaks         bool
}

func readTrace(path string) (t trace, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &t)
	}
	return t, err
}

func seconds(x float64) time.Duration { return time.Duration(x * float64(time.Second)) }

// authorMends is how long a person took to push the head that mended a conflict where the
// record's queue left it to them: the median of the few it holds.
const authorMends = 32*time.Minute + 36*time.Second

// acts is the window as the world's acts. A head the record holds no time for is taken to
// have passed its own checks before the label, and one the record's queue landed while its
// checks still ran, to have passed them when they were stopped. Where repair is off, a conflict the record's
// bot mended is mended as late as a person would have, and what followed moves with it.
func (t trace) acts(repair bool) (all []Act) {
	landed := map[string][]int{}
	for _, push := range t.Trunk {
		landed[push.SHA] = push.PRs
	}
	for _, pr := range t.PRs {
		var acts []Act
		add := func(at time.Duration, a Act) {
			a.At, a.Number = at, pr.Number
			acts = append(acts, a)
		}
		first, broke := never, never
		for _, l := range pr.Labels {
			if l.By != "person" {
				continue
			}
			if l.On {
				first = min(first, seconds(l.At))
				add(seconds(l.At), Act{Do: "label", First: l.Label != "land"})
			} else if l.Label == "land" {
				add(seconds(l.At), Act{Do: "unlabel"})
			}
		}
		for _, e := range pr.Ejections {
			if pr.Breaks && e.Cause == "culprit" && e.Attributed != "checks" && e.At >= 0 {
				broke = min(broke, seconds(e.At))
			}
		}
		if broke < never {
			add(broke, Act{Do: "ejected"})
		}
		head := func(at time.Duration, i int) queue.SHA {
			sha := queue.SHA(fmt.Sprintf("head%d.%d", pr.Number, i))
			add(at, Act{Do: "head", Head: sha, Breaks: at < broke && broke < never})
			return sha
		}
		var pushed []time.Duration
		for i, h := range pr.Heads {
			if h.At == nil {
				continue
			}
			pushed = append(pushed, seconds(*h.At))
			sha := head(seconds(*h.At), i+1)
			switch landedAt := pr.Merged != nil && i == len(pr.Heads)-1; {
			case h.Done == nil || h.Checks == nil:
			case *h.Checks == "success", *h.Checks == "cancelled" && landedAt:
				add(seconds(*h.Done), Act{Do: "checks", Head: sha, Verdict: queue.Success})
			case *h.Checks == "failure":
				add(seconds(*h.Done), Act{Do: "checks", Head: sha, Verdict: queue.Failure})
			}
		}
		if len(pushed) == 0 || pushed[0] > first {
			at := first - time.Second
			add(at, Act{Do: "checks", Head: head(at, 0), Verdict: queue.Success})
		}
		for i, c := range pr.Conflicts {
			if c.Mended == nil && c.Kind == "hold" {
				continue
			}
			at, mended := seconds(c.At), never
			if c.Mended != nil {
				mended = seconds(*c.Mended)
				if !slices.ContainsFunc(pushed, func(p time.Duration) bool { return p >= mended-2*time.Minute }) {
					add(mended+16*time.Minute, Act{Do: "checks", Head: head(mended, 100+i), Verdict: queue.Success})
				}
			}
			if late := authorMends - (mended - at); !repair && c.By == "bot" && late > 0 {
				for j := range acts {
					if acts[j].At >= mended-2*time.Minute && (acts[j].Do == "head" || acts[j].Do == "checks" || acts[j].Do == "label") {
						acts[j].At += late
					}
				}
				mended += late
			}
			if c.Kind == "hold" {
				add(at, Act{Do: "hold", Ahead: c.Ahead, Mended: mended})
			} else {
				add(at, Act{Do: "conflict", Mended: mended, After: landed[c.Onto]})
			}
		}
		if pr.Merged == nil && pr.Closed != nil {
			add(seconds(*pr.Closed), Act{Do: "unlabel"})
		}
		order := map[string]int{"head": 0, "checks": 1, "conflict": 2, "hold": 2, "ejected": 3, "unlabel": 4, "label": 5}
		sort.SliceStable(acts, func(i, j int) bool {
			if acts[i].At != acts[j].At {
				return acts[i].At < acts[j].At
			}
			return order[acts[i].Do] < order[acts[j].Do]
		})
		all = append(all, acts...)
	}
	return all
}

// times is how long the record's candidates took to pass, and the share of that a red one
// took to fail.
func (t trace) times() (passed []time.Duration, fails float64) {
	var failed []float64
	var took []float64
	for _, c := range t.Candidates {
		for _, a := range c.Attempts {
			switch d := a.Ended - a.Started; a.Conclusion {
			case "success":
				passed, took = append(passed, seconds(d)), append(took, d)
			case "failure":
				failed = append(failed, d)
			}
		}
	}
	return passed, percentile(failed, 50) / percentile(took, 50)
}

// outcome is what a queue did with a recorded window, by the measures the record's own
// readout takes: minutes from a pull request's first label to its landing, for all that
// were first labelled in the window and landed in it, and for those of them that never
// waited on a conflict or left the queue.
type outcome struct {
	name                   string
	runs                   int
	label, clean, blame    []float64 // blame: minutes from the first label of a pull request that broke the build to its ejection
	landed, ejected, wrong int
	builds                 int
	minutes                float64
	left, spared           int
	inHand                 int     // of those left, the ones labelled, passed by their own checks and clear of a conflict at the window's end
	longest                float64 // the longest any pull request took from its first label to its landing, in minutes, a run
	broken                 []string
}

// inHand says the record's queue could have landed the pull request at this time: it was
// labelled, its newest head had passed its own checks, and no conflict stood.
func (pr tracePR) inHand(at float64) bool {
	on, passed, clear := false, false, true
	for _, l := range pr.Labels {
		if l.At <= at {
			on = l.On
		}
	}
	for _, h := range pr.Heads {
		if h.At != nil && *h.At <= at {
			passed = h.Done != nil && *h.Done <= at && h.Checks != nil && *h.Checks == "success"
		}
	}
	for _, c := range pr.Conflicts {
		if c.At <= at && (c.Mended == nil || *c.Mended > at) {
			clear = false
		}
	}
	return on && passed && clear && (pr.Closed == nil || *pr.Closed > at)
}

// recorded is the record's own queue, read from the trace.
func (t trace) recorded() outcome {
	o := outcome{name: "recorded", runs: 1}
	window := t.Window.To.Sub(t.Window.From).Seconds()
	for _, pr := range t.PRs {
		first, asked := -1.0, false
		for _, l := range pr.Labels {
			if l.On {
				first = l.At
				break
			}
		}
		for _, c := range pr.Conflicts {
			asked = asked || c.Kind == "repair-ask"
		}
		for _, e := range pr.Ejections {
			if e.At >= 0 && e.At <= window {
				o.ejected++
			}
		}
		for _, e := range pr.Ejections {
			if pr.Breaks && e.Cause == "culprit" && e.Attributed != "checks" && first >= 0 {
				o.blame = append(o.blame, (e.At-first)/60)
				break
			}
		}
		switch {
		case pr.Merged == nil || *pr.Merged > window:
			o.left++
			if pr.inHand(window) {
				o.inHand++
			}
		case first >= 0:
			o.landed++
			o.label = append(o.label, (*pr.Merged-first)/60)
			o.longest = max(o.longest, (*pr.Merged-first)/60)
			if !asked && len(pr.Ejections) == 0 {
				o.clean = append(o.clean, (*pr.Merged-first)/60)
			}
		}
	}
	for _, c := range t.Candidates {
		o.builds += len(c.Attempts)
		for _, a := range c.Attempts {
			o.minutes += (a.Ended - a.Started) / 60
		}
	}
	return o
}

// replayed plays the window to the planner under these dials, once a seed.
func (t trace) replayed(cfg queue.Config, seeds int, shape Shape, repair bool) outcome {
	passed, fails := t.times()
	sort.Slice(passed, func(i, j int) bool { return passed[i] < passed[j] })
	shape.Name, shape.Build, shape.Times, shape.Fails, shape.Acts, shape.Cancels = "history", passed[len(passed)/2], passed, fails, t.acts(repair), true
	if shape.Whole == 0 {
		shape.Whole = passed[len(passed)*9/10]
	}
	window := t.Window.To.Sub(t.Window.From)
	o := outcome{name: fmt.Sprintf("%d x %d / %d", cfg.Slots, cfg.MaxBatch, cfg.Gather), runs: seeds}
	if cfg.Probes {
		o.name += " p"
	}
	for _, s := range each(seeds, func(i int) *Sim {
		s := NewSim(shape, cfg, int64(i))
		for s.now < window && len(s.Broken) == 0 {
			s.Step()
		}
		return s
	}) {
		o.broken = append(o.broken, s.Broken...)
		longest := 0.0
		for n, at := range s.history.blamed {
			if first, ok := s.history.labelled[n]; ok && first >= 0 {
				o.blame = append(o.blame, (at - first).Minutes())
			}
		}
		o.builds, o.minutes, o.wrong, o.spared = o.builds+s.Whole, o.minutes+s.Minutes, o.wrong+s.Wrong, o.spared+s.history.spared
		for _, n := range s.Ejected {
			o.ejected += n
		}
		for n, p := range s.prs {
			first, ok := s.history.labelled[n]
			switch {
			case !p.landed:
				o.left++
				if p.queued && p.Checks == queue.Success && !p.conflict && p.ahead == 0 {
					o.inHand++
				}
			case ok && first >= 0:
				o.landed++
				o.label = append(o.label, (p.at - first).Minutes())
				longest = max(longest, (p.at - first).Minutes())
				if !s.history.waited[n] && s.Left[n] == "" {
					o.clean = append(o.clean, (p.at - first).Minutes())
				}
			}
		}
		o.longest += longest
	}
	return o
}

func (o outcome) print() {
	n, landed := float64(o.runs), float64(max(o.landed, 1))
	fmt.Printf("%-10s %7.0f %8.1f %8.1f %8.1f %8.1f %7.3f %9.1f %7.1f %9.2f %10.1f %6.1f %8.1f %8.1f %8.0f %7.1f %7.0f\n", o.name, float64(o.landed)/n,
		percentile(o.label, 50), percentile(o.label, 90), percentile(o.clean, 50), percentile(o.clean, 90), float64(len(o.clean))/landed,
		100*float64(o.ejected)/landed, float64(o.wrong)/n, float64(o.builds)/landed, o.minutes/landed, float64(o.left)/n, float64(o.inHand)/n, percentile(o.label, 99), o.longest/n, float64(o.spared)/n, percentile(o.blame, 50))
	for _, b := range o.broken {
		fmt.Println("  broken:", b)
	}
}

// replayHistory prints what the record's queue did with a window, and under it what the planner
// does with the same pull requests, heads, checks and conflicts under each of the dials.
func replayHistory(path string, dials []queue.Config, seeds int, shape Shape, repair bool) error {
	t, err := readTrace(path)
	if err != nil {
		return err
	}
	fmt.Printf("%-10s %7s %8s %8s %8s %8s %7s %9s %7s %9s %10s %6s %8s %8s %8s %7s %7s\n", "queue", "landed", "label50", "label90", "clean50", "clean90", "clean", "eject/100", "wrong", "builds/pr", "build min", "left", "in hand", "label99", "longest", "spared", "blame")
	t.recorded().print()
	for _, cfg := range dials {
		t.replayed(cfg, seeds, shape, repair).print()
	}
	return nil
}
