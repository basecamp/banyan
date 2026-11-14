package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/git"
	"github.com/basecamp/banyan/internal/gittest"
	"github.com/basecamp/banyan/internal/queue"
)

func TestCheckerMaxStatesCap(t *testing.T) {
	w := checkWorld{
		Name:   "bounded-stop",
		Config: dials(2, 2, false),
		Pulls:  pulls(good, breaks),
		Bounds: bounds{Flakes: 1, Lost: 1, Episodes: 1, Moves: 1, Outside: 1, Crashes: 1, Stale: 1, MaxStates: 25},
	}
	res := explore(w, mutant{plan: queue.Plan, verify: queue.Verify})
	if !res.Capped || res.States > 25 {
		t.Fatalf("expected bounded stop at <= 25 states, got %+v", res)
	}
	t.Logf("bounded stop at %d states (depth %d)", res.States, res.Depth)
}

// The checker's first counterexample to invariant 8, played on a real repository, where a
// build's base and chain are read against the trunk as it stands. #1 and #2 failed together,
// #1 was cleared and landed, and a second-round build of the old trunk had passed. That pass
// stood between #2's two failures. Now #2 leaves on a pass of the trunk that holds #1.
func TestARoundsBuildOfTheOldTrunkIsNoEvidenceOnARealRepository(t *testing.T) {
	o := gittest.New(t)
	h1 := o.PR(1, "file1", "1\n")
	h2 := o.PR(2, "file2", "2\n")
	repo := &git.Repo{
		Dir: o.Clone, Remote: "origin", Trunk: "main", Lane: "merge-queue",
		Name: "Banyan", Email: "banyan@example.com",
	}
	seen := map[queue.SHA]bool{}
	repo.Pushed = func(sha queue.SHA) bool { return seen[sha] }

	type ciBuild struct {
		begun   bool
		started time.Time
		pushed  time.Time
		verdict queue.Verdict
	}
	builds := map[queue.SHA]*ciBuild{}
	now := checkEpoch.Add(time.Hour)
	prs := []queue.PR{
		{Number: 1, Head: h1, Queued: checkEpoch, Checks: queue.Success},
		{Number: 2, Head: h2, Queued: checkEpoch, Checks: queue.Success},
	}

	readWorld := func() queue.World {
		trunk, _, err := repo.Fetch([]int{1, 2})
		if err != nil {
			t.Fatal(err)
		}
		var open []queue.PR
		for _, pr := range prs {
			if !repo.Holds(trunk, pr.Head) {
				open = append(open, pr)
			}
		}
		bs, err := repo.Builds(trunk)
		if err != nil {
			t.Fatal(err)
		}
		for i := range bs {
			cb := builds[bs[i].SHA]
			bs[i].Verdict = cb.verdict
			bs[i].Pushed = cb.pushed
			bs[i].Started = cb.started
		}
		return queue.World{Now: now, Trunk: trunk, PRs: open, Builds: bs, Repo: repo}
	}

	var ejected []queue.Action
	applyTick := func() {
		for pass := 0; pass < 6; pass++ {
			w := readWorld()
			acts := queue.Plan(w, dials(2, 2, false))
			if len(acts) == 0 {
				break
			}
			for _, a := range acts {
				switch a.Do {
				case "push":
					if err := repo.Push(a.Build.SHA); err != nil {
						t.Fatal(err)
					}
					seen[a.Build.SHA] = true
					now = now.Add(time.Second)
					builds[a.Build.SHA] = &ciBuild{pushed: now}
				case "delete":
					if err := repo.Delete(a.SHA); err != nil {
						t.Fatal(err)
					}
				case "land":
					if err := queue.Verify(readWorld(), a); err != nil {
						continue
					}
					if err := repo.Land(a.SHA); err != nil {
						t.Fatal(err)
					}
				case "eject":
					ejected = append(ejected, a)
				}
			}
		}
	}
	beginCI := func(sha queue.SHA) {
		cb := builds[sha]
		if !cb.begun {
			now = now.Add(time.Second)
			cb.begun = true
			cb.started = now
		}
	}
	finishCI := func(sha queue.SHA, flake bool) {
		beginCI(sha)
		if flake || o.Holds(sha, "file2") {
			builds[sha].verdict = queue.Failure
		} else {
			builds[sha].verdict = queue.Success
		}
	}
	byRoundChain := func(round, chainLen int) queue.SHA {
		trunk, _, _ := repo.Fetch([]int{1, 2})
		bs, _ := repo.Builds(trunk)
		for _, b := range bs {
			if b.Round == round && len(b.Chain) == chainLen {
				return b.SHA
			}
		}
		t.Fatalf("missing build round=%d chain=%d", round, chainLen)
		return ""
	}

	applyTick()
	finishCI(byRoundChain(0, 2), false)
	applyTick()
	beginCI(byRoundChain(1, 2))
	beginCI(byRoundChain(1, 1))
	applyTick()
	finishCI(byRoundChain(2, 1), true)
	for i := 0; i < 6 && len(ejected) == 0; i++ {
		applyTick()
		trunk, _, _ := repo.Fetch([]int{1, 2})
		bs, _ := repo.Builds(trunk)
		for _, b := range bs {
			if builds[b.SHA].verdict == queue.Pending {
				finishCI(b.SHA, false)
			}
		}
	}
	if len(ejected) == 0 {
		t.Fatal("expected real-git replay to trigger eject")
	}
	a := ejected[0]
	first0 := o.Holds(a.Evidence[0], "file1")
	first1 := o.Holds(a.Evidence[1], "file1")
	first2 := o.Holds(a.Evidence[2], "file1")
	t.Logf("real-git eject #%d evidence=%v first0=%v first1=%v first2=%v",
		a.PR, a.Evidence, first0, first1, first2)
	if !first0 || !first1 || !first2 {
		t.Fatalf("#%d left on a pass of a tree that held #1: %v, and failures of trees that did: %v and %v, where all three hold it", a.PR, first0, first1, first2)
	}
}

func TestCheckerKillsAllInvariants(t *testing.T) {
	worlds := mutantWorlds()
	clean := mutant{plan: queue.Plan, verify: queue.Verify}

	for _, w := range worlds {
		if cr := explore(w, clean); cr.Broken != "" {
			t.Fatalf("baseline world %q unexpectedly broke on clean queue.Plan: %s", w.Name, cr.Broken)
		}
		wi := w
		wi.Instant = true
		if cr := explore(wi, clean); cr.Broken != "" {
			t.Fatalf("baseline instant world %q unexpectedly broke on clean queue.Plan: %s", w.Name, cr.Broken)
		}
	}

	invsSeen := map[int]string{}
	for _, cm := range checkerMutants() {
		_, worldName, res := firstBroken(worlds, cm.mutant)
		if res.Broken == "" {
			t.Errorf("checker did not kill mutant %q", cm.name)
			continue
		}
		t.Logf("mutant %-46s: %s (%s, %d states, %d steps)", cm.name, res.Broken, worldName, res.States, len(res.Trace))
		for n := 1; n <= 8; n++ {
			token := fmt.Sprintf("invariant %d:", n)
			if strings.Contains(res.Broken, token) && invsSeen[n] == "" {
				invsSeen[n] = cm.name
			}
		}
		if cm.mutant.crashes {
			var noCrash []checkWorld
			for _, w := range worlds {
				w.Bounds.Crashes = 0
				noCrash = append(noCrash, w)
			}
			if _, ns, nc := firstBroken(noCrash, cm.mutant); nc.Broken != "" {
				t.Errorf("crash mutant %q broke %q in %s even with 0 crashes", cm.name, nc.Broken, ns)
			}
		}
		if cm.mutant.waits {
			var instant []checkWorld
			for _, w := range worlds {
				w.Instant = true
				instant = append(instant, w)
			}
			if _, is, ir := firstBroken(instant, cm.mutant); ir.Broken != "" {
				t.Errorf("wait mutant %q broke %q in %s even with Instant CI", cm.name, ir.Broken, is)
			}
		}
	}
	var got []int
	for n := 1; n <= 8; n++ {
		if invsSeen[n] != "" {
			got = append(got, n)
		}
	}
	sort.Ints(got)
	t.Logf("invariants killed %v via %v", got, invsSeen)
	if !slices.Equal(got, []int{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("missing killed invariants: got %v (map=%v)", got, invsSeen)
	}
}

func TestCheckerCurrentPlanner(t *testing.T) {
	sum := checkSuite(defaultWorlds(), mutant{plan: queue.Plan, verify: queue.Verify})
	t.Logf("current queue.Plan: %d of %d whole, %d capped, %d states, %d broken",
		sum.whole, sum.worlds, sum.capped, sum.states, sum.brokenWorlds)
	for _, inv := range sum.brokenInvs {
		sample := sum.bestByInv[inv]
		t.Errorf("%s is broken in %s:\n%s", inv, sample.world.Name, formatTrace(sample.result))
	}
}

func TestCheckerCounterexample(t *testing.T) {
	w := checkWorld{
		Name:   "2 PRs crashes+stale (good+breaks, 2 of 2)",
		Config: dials(2, 2, false),
		Pulls:  pulls(good, breaks),
		Bounds: bounds{Flakes: 1, Crashes: 1, Stale: 1, MaxStates: 500},
	}
	m := mutants["ejects at the first red"]
	res := explore(w, m)
	if res.Broken == "" {
		t.Fatal("expected mutant to break")
	}
	out := formatCounterexample(w, m, res)
	for _, want := range []string{"trunk: trunk0", "pull requests:", "builds:", "actions:", "eject #2 cause=culprit"} {
		if !strings.Contains(out, want) {
			t.Errorf("counterexample output missing %q:\n%s", want, out)
		}
	}
}

func TestCheckerClockOrderAndGaps(t *testing.T) {
	cfg := dials(2, 2, false)
	c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good, breaks)}, mutant{plan: queue.Plan, verify: queue.Verify})

	buildMap := func(s *worldState) map[queue.SHA]queue.Build {
		w, _ := c.read(s)
		out := make(map[queue.SHA]queue.Build, len(w.Builds))
		for _, b := range w.Builds {
			out[b.SHA] = b
		}
		return out
	}

	s := &worldState{trunk: "trunk0", nextSeq: 1}
	p1 := s.nextEvent()
	p2 := s.nextEvent()
	s.builds = []pushedBuild{
		{sha: "b1", branch: true, marked: true, verdict: queue.Pending, pushed: p1},
		{sha: "b2", branch: true, marked: true, verdict: queue.Pending, pushed: p2},
	}
	s.begin(&s.builds[0])
	bs := buildMap(s)
	if !(bs["b1"].Pushed.Before(bs["b2"].Pushed) && bs["b2"].Pushed.Before(bs["b1"].Started)) {
		t.Fatalf("expected b1.Pushed < b2.Pushed < b1.Started, got b1=(%v,%v) b2=%v", bs["b1"].Pushed, bs["b1"].Started, bs["b2"].Pushed)
	}

	s2 := &worldState{trunk: "trunk0", nextSeq: 1}
	s2.builds = []pushedBuild{
		{sha: "b1", branch: true, marked: true, verdict: queue.Pending, pushed: s2.nextEvent()},
	}
	s2.begin(&s2.builds[0])
	s2.builds = append(s2.builds, pushedBuild{sha: "b2", branch: true, marked: true, verdict: queue.Pending, pushed: s2.nextEvent()})
	bs2 := buildMap(s2)
	if !(bs2["b1"].Started.Before(bs2["b2"].Pushed)) {
		t.Fatalf("expected b1.Started < b2.Pushed, got started=%v b2.Pushed=%v", bs2["b1"].Started, bs2["b2"].Pushed)
	}

	// Both builds time out, with a timeout elapsing between them in one state and not in the other.
	apart := &worldState{trunk: "trunk0", nextSeq: 1, timeoutsLeft: 1}
	apart.builds = []pushedBuild{{sha: "a", branch: true, marked: true, verdict: queue.Pending, pushed: apart.nextEvent()}}
	apart.timedOut = apart.builds[0].pushed
	apart.rechecked = apart.builds[0].pushed
	apart.builds = append(apart.builds, pushedBuild{sha: "b", branch: true, marked: true, verdict: queue.Pending, pushed: apart.nextEvent(), past: apart.timedOut})
	apart.timedOut = apart.builds[1].pushed
	apart.rechecked = apart.builds[1].pushed

	w, _ := c.read(apart)
	ba := buildMap(apart)
	if ba["b"].Pushed.Sub(ba["a"].Pushed) <= cfg.Timeout {
		t.Fatalf("expected persistent timeout gap > %v between a and b, got %v", cfg.Timeout, ba["b"].Pushed.Sub(ba["a"].Pushed))
	}
	if w.Now.Sub(ba["b"].Pushed) <= cfg.Timeout || w.Now.Sub(ba["b"].Pushed) < cfg.Recheck {
		t.Fatalf("expected b past timeout and recheck at Now, got age %v", w.Now.Sub(ba["b"].Pushed))
	}

	together := &worldState{trunk: "trunk0", nextSeq: 1, timeoutsLeft: 1}
	together.builds = []pushedBuild{
		{sha: "a", branch: true, marked: true, verdict: queue.Pending, pushed: together.nextEvent()},
		{sha: "b", branch: true, marked: true, verdict: queue.Pending, pushed: together.nextEvent()},
	}
	together.timedOut = together.builds[1].pushed
	together.rechecked = together.builds[1].pushed
	bt := buildMap(together)
	if bt["b"].Pushed.Sub(bt["a"].Pushed) > cfg.Timeout {
		t.Fatalf("expected a and b pushed within timeout, got gap %v", bt["b"].Pushed.Sub(bt["a"].Pushed))
	}
	if apart.key() == together.key() {
		t.Fatal("expected different keys for builds pushed a timeout apart vs together")
	}

	shifted := apart.clone()
	shifted.nextSeq += 50
	shifted.timedOut += 20
	shifted.rechecked += 20
	for i := range shifted.builds {
		shifted.builds[i].pushed += 20
		if shifted.builds[i].past > 0 {
			shifted.builds[i].past += 20
		}
	}
	if apart.key() != shifted.key() {
		t.Fatal("expected canonical key to ignore absolute sequence numbers and unused gaps")
	}

	var apartMutant checkMutant
	for _, cm := range checkerMutants() {
		if cm.name == "spans a round that had not begun" {
			apartMutant = cm
			break
		}
	}
	for _, silent := range []bool{false, true} {
		mw := checkWorld{
			Name:   fmt.Sprintf("apart silent=%v", silent),
			Config: cfg,
			Pulls:  pulls(good, breaks),
			Silent: silent,
			Bounds: bounds{Timeouts: 1, MaxStates: 2000},
		}
		if res := explore(mw, apartMutant.mutant); res.Broken == "" {
			t.Fatalf("expected apart mutant to break for silent=%v", silent)
		}
	}
}

func TestCheckerMoveHead(t *testing.T) {
	c := newChecker(checkWorld{
		Config: dials(2, 2, false),
		Pulls:  pulls(good, pullSpec{Conflict: true, Ahead: 1}),
		Bounds: bounds{Moves: 1},
	}, mutant{plan: queue.Plan, verify: queue.Verify})

	s := c.initState()
	findMove2 := func(st *worldState) *worldState {
		for _, mv := range c.pullMoves(st) {
			if mv.step.kind == stepMove && mv.step.pull == 2 {
				return mv.state
			}
		}
		t.Fatal("missing stepMove #2")
		return nil
	}

	ns := findMove2(s)
	p := ns.pulls[1]
	if p.checks != queue.Pending || p.conflict || p.ahead != 1 {
		t.Fatalf("unexpected pull #2 state on trunk0: %+v", p)
	}

	sTrunkHolds1 := c.initState()
	sTrunkHolds1.trunk = c.addCommit(sTrunkHolds1.trunk, sTrunkHolds1.pulls[0].head(1, 0), 1, "")
	nsHeld := findMove2(sTrunkHolds1)
	if p2 := nsHeld.pulls[1]; p2.checks != queue.Pending || p2.conflict || p2.ahead != 0 {
		t.Fatalf("unexpected pull #2 state when trunk holds #1: %+v", p2)
	}
}
