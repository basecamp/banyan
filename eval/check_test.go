package main

import (
	"fmt"
	"reflect"
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
	worlds := slices.DeleteFunc(defaultWorlds(), func(w checkWorld) bool { return slices.Contains(clashSetFirst, w.Name) })
	sum := checkSuite(worlds, mutant{plan: queue.Plan, verify: queue.Verify})
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

func TestKeepCheckerRecordsMarksAndRetainsLandedBranches(t *testing.T) {
	cfg := dials(3, 1, false)
	cfg.Keep = 3 * time.Minute
	c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good, good)}, mutant{plan: queue.Plan, verify: queue.Verify})
	out := (&checkTick{c: c, state: c.initState()}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	s := out.final
	for i := range s.builds {
		s.begin(&s.builds[i])
		s.builds[i].verdict = queue.Success
	}
	w, _ := c.read(s)
	for _, b := range w.Builds {
		if !b.Landed.IsZero() {
			t.Fatalf("CI wrote a landed mark: %+v", b)
		}
	}
	out = (&checkTick{c: c, state: s}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	if len(out.final.builds) != 2 {
		t.Fatalf("landing removed builds the planner did not delete: %d", len(out.final.builds))
	}
	final, _ := c.read(out.final)
	for _, b := range final.Builds {
		if b.Landed.IsZero() || !b.Landed.After(b.Pushed) {
			t.Fatalf("landing or read did not mark: %+v", b)
		}
	}
	if acts := queue.Plan(final, cfg); len(acts) != 0 {
		t.Fatalf("kept landing acted again: %+v", acts)
	}
}

func TestKeepCheckerCutsKeepAndRecheckIndependently(t *testing.T) {
	for _, rc := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		t.Run(rc.String(), func(t *testing.T) {
			cfg := dials(1, 1, false)
			cfg.Keep, cfg.Recheck = 3*time.Minute, rc
			c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good)}, mutant{plan: queue.Plan, verify: queue.Verify})
			s := c.initState()
			sha := c.addCommit(s.trunk, "head1", 1, "")
			s.builds = []pushedBuild{{sha: sha, branch: true, marked: true, pushed: s.nextEvent(), verdict: queue.Success}}
			s.begin(&s.builds[0])
			s.builds[0].landed = s.nextEvent()
			s.pulls[0].queued = false
			s.trunk = sha
			var expired, unexpired bool
			for _, tr := range c.clockMoves(s) {
				w, _ := c.read(tr.state)
				b := w.Builds[0]
				old := w.Now.Sub(b.Landed) >= cfg.Keep
				acts := queue.Plan(w, cfg)
				if old {
					expired = true
					if len(acts) != 1 || acts[0].Do != "delete" {
						t.Fatalf("expired cut: %+v", acts)
					}
				} else {
					unexpired = true
					if len(acts) != 0 {
						t.Fatalf("young cut: %+v", acts)
					}
				}
				unlanded := tr.state.clone()
				unlanded.trunk = "trunk0"
				unlanded.builds[0].landed = 0
				u, _ := c.read(unlanded)
				if acts := queue.Plan(u, cfg); len(acts) != 1 || acts[0].Do != "delete" {
					t.Fatalf("unlanded build waited for keep or recheck: %+v", acts)
				}
				if rc > 0 && tr.state.rechecked > 0 && w.Now.Sub(b.Pushed) < rc {
					t.Fatal("recheck cut was not honoured")
				}
			}
			if !expired || rc > 0 && !unexpired {
				t.Fatalf("missing independent cuts: expired=%v unexpired=%v", expired, unexpired)
			}
		})
	}
}

func TestKeepCheckerReadMarksAndKeys(t *testing.T) {
	cfg := dials(1, 1, false)
	cfg.Keep = time.Minute
	c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good)}, mutant{plan: queue.Plan, verify: queue.Verify})
	s := c.initState()
	sha := c.addCommit(s.trunk, "head1", 1, "")
	s.builds = []pushedBuild{{sha: sha, branch: true, marked: true, pushed: s.nextEvent()}}
	for _, tr := range c.ciMoves(s) {
		if tr.step.kind == stepPass {
			tr.state.trunk = sha
			out := (&checkTick{c: c, state: tr.state}).run()
			if out.broken != "" || out.final.builds[0].landed == 0 {
				t.Fatalf("read did not mark: %+v", out)
			}
			tr.state = out.final
			other := tr.state.clone()
			other.kept = other.builds[0].landed
			if other.key() == tr.state.key() {
				t.Fatal("keep cut missing from state key")
			}
			return
		}
	}
	t.Fatal("no CI pass")
}

func TestKeepCheckerMutants(t *testing.T) {
	for _, cm := range keepMutants() {
		t.Run(cm.name, func(t *testing.T) {
			_, name, res := firstBroken(keepMutantWorlds(), cm.mutant)
			if !strings.HasPrefix(res.Broken, "keep inertness:") && !strings.HasPrefix(res.Broken, "slots:") {
				t.Fatalf("mutant escaped inertness in %s: %+v", name, res)
			}
			t.Logf("%s: %s (%d states, %d steps)", name, res.Broken, res.States, len(res.Trace))
		})
	}
}

func TestKeepCheckerUnlandedLifecycles(t *testing.T) {
	for _, event := range []string{"ejection then same head relabelled", "cleared round then fresh pass", "round ended while probe runs"} {
		t.Run(event, func(t *testing.T) {
			cfg := dials(1, 1, true)
			cfg.Keep = 3 * time.Minute
			c := newChecker(checkWorld{Config: cfg, Pulls: pulls(breaks), Bounds: bounds{Moves: 1}}, mutant{plan: queue.Plan, verify: queue.Verify})
			if event == "cleared round then fresh pass" {
				c.world.Pulls = pulls(good)
			}
			s := c.initState()
			red := c.addCommit(s.trunk, "head1", 1, "")
			add := func(sha string, v queue.Verdict) {
				b := pushedBuild{sha: sha, branch: true, marked: true, pushed: s.nextEvent(), verdict: v}
				s.begin(&b)
				s.builds = append(s.builds, b)
			}
			add(red, queue.Failure)
			for round := 1; round <= 3; round++ {
				parent, v := red, queue.Failure
				if round == 2 {
					parent, v = s.trunk, queue.Success
				}
				note := fmt.Sprintf("Banyan-Probe-Of: %s\nBanyan-Round: %d\nBanyan-Of: %s", red, round, red)
				if event == "cleared round then fresh pass" && round == 1 {
					v = queue.Success
					note = fmt.Sprintf("Banyan-Check: whole\nBanyan-Round: 1\nBanyan-Of: %s", red)
				}
				add(c.addCommit(parent, "", 0, note), v)
			}
			if event == "round ended while probe runs" {
				add(c.addCommit(red, "", 0, fmt.Sprintf("Banyan-Probe-Of: %s\nBanyan-Round: 4\nBanyan-Of: %s", red, red)), queue.Pending)
			}
			c.plan = func(w queue.World, cfg queue.Config) []queue.Action {
				acts := queue.Plan(w, cfg)
				if !slices.ContainsFunc(w.Builds, func(b queue.Build) bool { return !b.Landed.IsZero() }) {
					unset := cfg
					unset.Keep = 0
					if want := queue.Plan(w, unset); !reflect.DeepEqual(acts, want) {
						t.Fatalf("unlanded world differs with keep: got %v want %v", acts, want)
					}
				}
				return acts
			}
			run := func(st *worldState) tickOutcome {
				t.Helper()
				out := (&checkTick{c: c, state: st, trace: true}).run()
				if out.broken != "" {
					t.Fatal(out.broken)
				}
				return out
			}
			out := run(s)
			s = out.final
			for _, b := range s.builds {
				if b.sha == red || c.commits[b.sha].round > 0 {
					t.Fatalf("ended evidence survived: %+v", b)
				}
			}
			if event != "cleared round then fresh pass" {
				if s.pulls[0].queued {
					t.Fatal("culprit not ejected")
				}
				if len(s.builds) != 0 {
					t.Fatalf("ejected builds survived: %+v", s.builds)
				}
				if event == "ejection then same head relabelled" {
					s = moveState(c.pullMoves(s), step{kind: stepRelabel, pull: 1})
					out = run(s)
					for _, r := range out.reads {
						for _, a := range r.actions {
							if a.Do == "eject" {
								t.Fatal("old evidence ejected relabelled head")
							}
						}
					}
					if !out.final.pulls[0].queued {
						t.Fatal("relabelled head lost its label")
					}
				}
			} else {
				if len(s.builds) != 1 {
					t.Fatalf("missing fresh candidate: %+v", s.builds)
				}
				fresh := s.builds[0].sha
				if fresh == red {
					t.Fatal("reused old candidate")
				}
				s = moveState(c.ciMoves(s), step{kind: stepPass, sha: fresh})
				out = run(s)
				if out.final.trunk != fresh {
					t.Fatal("fresh passed candidate did not land")
				}
			}
		})
	}
}

func TestKeepCheckerWorlds(t *testing.T) {
	sum := checkSuite(keepWorlds(), mutant{plan: queue.Plan, verify: queue.Verify})
	t.Logf("keep worlds: %d whole, %d capped, %d states, %d broken", sum.whole, sum.capped, sum.states, sum.brokenWorlds)
	for _, rule := range sum.brokenInvs {
		sample := sum.bestByInv[rule]
		t.Errorf("%s in %s:\n%s", rule, sample.world.Name, formatTrace(sample.result))
	}
}

func TestKeepCheckerSmallWorldsExploreWhole(t *testing.T) {
	seen := map[string]bool{}
	for _, w := range append(smallKeepWorlds(), addedKeepWorlds()...) {
		seen[w.Name] = true
		t.Run(w.Name, func(t *testing.T) {
			res := explore(w, mutant{plan: queue.Plan, verify: queue.Verify})
			t.Logf("%s: whole=%v states=%d depth=%d broken=%s", w.Name, !res.Capped, res.States, res.Depth, res.Broken)
			if res.Capped || res.Broken != "" {
				t.Fatalf("did not explore whole: %+v", res)
			}
		})
	}
	for _, name := range []string{"Keep whole longer than timeout", "Keep whole stacked longer than timeout"} {
		if !seen[name] {
			t.Errorf("missing timeout world %q", name)
		}
	}
}

func TestKeepCheckerDrainSeesLandedBuildStandThenExpire(t *testing.T) {
	w := smallKeepWorlds()[0]
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	msg, trace := c.drain(c.initState(), true)
	if msg != "" {
		t.Fatal(msg)
	}
	stood, deleted := false, false
	for _, tick := range trace {
		for _, r := range tick.reads {
			for _, b := range r.world.Builds {
				if b.Landed.IsZero() {
					continue
				}
				due := r.world.Now.Sub(b.Landed) >= w.Config.Keep
				if !due {
					stood = true
				}
				for _, a := range r.actions {
					if a.Do == "delete" && a.SHA == b.SHA {
						if !stood || !due {
							t.Fatalf("delete before standing through keep: %+v", r)
						}
						deleted = true
					}
				}
			}
		}
	}
	if !stood || !deleted {
		t.Fatalf("drain stood=%v deleted=%v", stood, deleted)
	}
}

func TestKeepCheckerSmallWorldHistories(t *testing.T) {
	for _, w := range smallKeepWorlds() {
		t.Run(w.Name, func(t *testing.T) {
			c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
			s := c.initState()
			var actions []queue.Action
			var reads []tickRead
			run := func() {
				t.Helper()
				out := (&checkTick{c: c, state: s, trace: true}).run()
				if out.broken != "" {
					t.Fatal(out.broken)
				}
				reads = append(reads, out.reads...)
				for _, r := range out.reads {
					actions = append(actions, r.actions...)
				}
				s = out.final
			}
			answer := func(kind stepKind, sha queue.SHA) {
				t.Helper()
				moves := c.ciMoves(s)
				idx := slices.IndexFunc(moves, func(tr transition) bool { return tr.step.kind == kind && tr.step.sha == sha })
				if idx < 0 {
					t.Fatalf("no %v for %s", kind, sha)
				}
				s = moves[idx].state
			}
			run()
			first := s.builds[0].sha
			switch w.Name {
			case "Keep whole culprit relabelled":
				head := s.pulls[0].head(1, s.pulls[0].githubHead)
				for n := 0; s.pulls[0].queued && n < maxDrainTicks; n++ {
					c.drainCI(s)
					run()
				}
				if s.pulls[0].queued || !slices.ContainsFunc(actions, func(a queue.Action) bool { return a.Do == "eject" && a.Cause == "culprit" }) {
					t.Fatal("no culprit ejection")
				}
				s = moveState(c.pullMoves(s), step{kind: stepRelabel, pull: 1})
				run()
				if !s.pulls[0].queued || s.pulls[0].head(1, s.pulls[0].githubHead) != head {
					t.Fatal("culprit was not relabelled at the same head")
				}
				return
			case "Keep whole flaky retry":
				answer(stepFlake, first)
				run()
				idx := slices.IndexFunc(s.builds, func(b pushedBuild) bool { return c.commits[b.sha].kind == queue.Check })
				if idx < 0 {
					t.Fatal("no whole check after flake")
				}
				answer(stepPass, s.builds[idx].sha)
				run()
				if len(s.builds) != 1 || s.builds[0].sha == first || c.commits[s.builds[0].sha].plain != first {
					t.Fatalf("no fresh retry after clearing: %+v", s.builds)
				}
				answer(stepPass, s.builds[0].sha)
			case "Keep whole two stacked":
				if len(s.builds) != 2 {
					t.Fatal("did not push two candidates")
				}
				idx := slices.IndexFunc(s.builds, func(b pushedBuild) bool { return c.holds(b.sha, first) && b.sha != first })
				if idx < 0 {
					t.Fatal("candidates did not stack")
				}
				answer(stepBegin, first)
				answer(stepPass, s.builds[idx].sha)
			default:
				answer(stepPass, first)
			}
			run()
			lands := 0
			for _, a := range actions {
				if a.Do == "land" {
					lands++
					if len(a.Members) != len(w.Pulls) {
						t.Fatalf("landing has %d members, want %d", len(a.Members), len(w.Pulls))
					}
				}
			}
			if lands != 1 {
				t.Fatalf("land pushes = %d, want one", lands)
			}
			landed := s.trunk
			idx := s.findBuild(landed)
			if idx < 0 || !s.builds[idx].branch || s.builds[idx].landed == 0 {
				t.Fatal("landed build was not marked and standing")
			}
			mark := s.builds[idx].landed
			run()
			if idx = s.findBuild(landed); idx < 0 || !s.builds[idx].branch {
				t.Fatal("landed branch disappeared before keep")
			}
			moves := c.clockMoves(s)
			idx = slices.IndexFunc(moves, func(tr transition) bool { return tr.step.kind == stepKeep && tr.state.kept >= mark })
			if idx < 0 {
				t.Fatal("no landing expiry")
			}
			s = moves[idx].state
			run()
			run()
			stood, deleted := false, 0
			for _, r := range reads {
				for _, b := range r.world.Builds {
					if b.SHA == landed && !b.Landed.IsZero() && r.world.Now.Sub(b.Landed) < w.Config.Keep {
						stood = true
					}
				}
				for _, a := range r.actions {
					if a.Do == "delete" && a.SHA == landed {
						b := r.world.Builds[slices.IndexFunc(r.world.Builds, func(b queue.Build) bool { return b.SHA == landed })]
						if !stood || r.world.Now.Sub(b.Landed) < w.Config.Keep {
							t.Fatal("landing deleted before mark plus keep")
						}
						deleted++
					}
				}
			}
			if idx = s.findBuild(landed); !stood || deleted != 1 || idx >= 0 && s.builds[idx].branch {
				t.Fatalf("landed branch stood=%v deletes=%d", stood, deleted)
			}
		})
	}
}

func TestKeepCheckerLandedPendingCandidateUsesOrdinarySlot(t *testing.T) {
	for _, cancels := range []bool{false, true} {
		t.Run(fmt.Sprint(cancels), func(t *testing.T) {
			cfg := queue.Config{Slots: 2, MaxBatch: 1, Keep: 3 * time.Minute, Cancels: cancels}
			c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good, good, good, good)}, mutant{plan: queue.Plan, verify: queue.Verify})
			out := (&checkTick{c: c, state: c.initState()}).run()
			if out.broken != "" || len(out.final.builds) != 2 {
				t.Fatalf("initial candidates: %+v", out)
			}
			s := out.final
			lower, upper := s.builds[0].sha, s.builds[1].sha
			if !c.holds(upper, lower) || lower == upper {
				t.Fatal("candidates did not stack")
			}
			s = moveState(c.ciMoves(s), step{kind: stepBegin, sha: lower})
			s = moveState(c.ciMoves(s), step{kind: stepPass, sha: upper})
			witnessed := false
			c.plan = func(w queue.World, cfg queue.Config) []queue.Action {
				acts := queue.Plan(w, cfg)
				if cfg.Keep <= 0 || !slices.ContainsFunc(w.Builds, func(b queue.Build) bool {
					return b.SHA == lower && b.Landed.IsZero() && b.Verdict == queue.Pending && !b.Started.IsZero()
				}) {
					return acts
				}
				witnessed = true
				without := w
				without.Builds = slices.DeleteFunc(slices.Clone(w.Builds), func(b queue.Build) bool { return !b.Landed.IsZero() })
				unset := cfg
				unset.Keep = 0
				if want := queue.Plan(without, unset); !reflect.DeepEqual(acts, want) {
					t.Fatalf("got %+v, want keep unset %+v", acts, want)
				}

				return acts
			}
			out = (&checkTick{c: c, state: s}).run()
			if out.broken != "" {
				t.Fatal(out.broken)
			}
			if !witnessed {
				t.Fatal("never read the running lower candidate after the upper landed")
			}
			w, _ := c.read(out.final)
			running := 0
			for _, b := range w.Builds {
				if b.Kind == queue.Candidate && b.Verdict == queue.Pending {
					running++
				}
			}
			if running != cfg.Slots {
				t.Fatalf("pending candidates = %d, want %d", running, cfg.Slots)
			}
			if held := slices.ContainsFunc(w.Builds, func(b queue.Build) bool { return b.SHA == lower }); held == cancels {
				t.Fatalf("cancels=%v: lower candidate held=%v", cancels, held)
			}
		})
	}
}

func TestKeepCheckerDrainAllowsLandedPendingBuild(t *testing.T) {
	for _, keep := range []time.Duration{0, 3 * time.Minute} {
		t.Run(keep.String(), func(t *testing.T) {
			cfg := queue.Config{Slots: 2, MaxBatch: 1, Keep: keep}
			c := newChecker(checkWorld{Config: cfg, Pulls: pulls(good, good), Bounds: bounds{Lost: 1}}, mutant{plan: queue.Plan, verify: queue.Verify})
			out := (&checkTick{c: c, state: c.initState()}).run()
			if out.broken != "" || len(out.final.builds) != 2 {
				t.Fatalf("initial candidates: %+v", out)
			}
			s := out.final
			lower, upper := s.builds[0].sha, s.builds[1].sha
			s = moveState(c.ciMoves(s), step{kind: stepBegin, sha: lower})
			s = moveState(c.ciMoves(s), step{kind: stepLost, sha: lower})
			s = moveState(c.ciMoves(s), step{kind: stepPass, sha: upper})
			out = (&checkTick{c: c, state: s}).run()
			if out.broken != "" || out.final.trunk != upper {
				t.Fatalf("stacked landing: %+v", out)
			}
			msg, trace := c.drain(out.final, true)
			if msg != "" {
				t.Fatal(msg)
			}
			pending := false
			for _, tick := range trace {
				for _, r := range tick.reads {
					for _, b := range r.world.Builds {
						if b.SHA == lower && b.Verdict == queue.Pending && b.Landed.IsZero() {
							pending = true
						}
					}
					for _, a := range r.actions {
						if a.Do == "delete" && a.SHA == lower {
							t.Fatal("drain deleted the landed pending build without a timeout")
						}
					}
				}
			}
			if !pending {
				t.Fatal("drain never read the pending ancestor")
			}
		})
	}
}

func TestKeepCheckerLowerCanFailAfterTheStackLands(t *testing.T) {
	w, ok := findWorld("Keep whole lower flakes after landing")
	if !ok {
		t.Fatal("missing world")
	}
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	out := (&checkTick{c: c, state: c.initState()}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	s := out.final
	lower, upper := s.builds[0].sha, s.builds[1].sha
	s = moveState(c.ciMoves(s), step{kind: stepBegin, sha: lower})
	s = moveState(c.ciMoves(s), step{kind: stepPass, sha: upper})
	out = (&checkTick{c: c, state: s}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	s = out.final
	if s.trunk != upper || s.builds[s.findBuild(lower)].landed != 0 {
		t.Fatal("lower was marked while running")
	}
	s = moveState(c.ciMoves(s), step{kind: stepFlake, sha: lower})
	out = (&checkTick{c: c, state: s}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	b := out.final.builds[out.final.findBuild(lower)]
	if b.verdict != queue.Failure || b.landed == 0 {
		t.Fatalf("finished lower was not marked: %+v", b)
	}
	if msg, _ := c.drain(out.final, false); msg != "" {
		t.Fatal(msg)
	}
}

func TestKeepCheckerCrashBeforeMarkIsRepaired(t *testing.T) {
	w, ok := findWorld("Keep whole crash before mark")
	if !ok {
		t.Fatal("missing world")
	}
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	out := (&checkTick{c: c, state: c.initState()}).run()
	s := out.final
	sha := s.builds[0].sha
	s = moveState(c.ciMoves(s), step{kind: stepPass, sha: sha})
	out = (&checkTick{c: c, state: s, crash: true}).run()
	for _, crash := range out.crashes {
		if crash.step.after != "git land" {
			continue
		}
		b := crash.state.builds[crash.state.findBuild(sha)]
		if b.landed != 0 {
			t.Fatal("landing push already marked")
		}
		recovered := (&checkTick{c: c, state: crash.state}).run()
		if recovered.broken != "" || recovered.final.builds[recovered.final.findBuild(sha)].landed == 0 {
			t.Fatalf("read failed to repair: %+v", recovered)
		}
		if msg, _ := c.drain(recovered.final, false); msg != "" {
			t.Fatal(msg)
		}
		return
	}
	t.Fatal("no crash between landing push and mark")
}

func TestCheckerAnsweredBuildDoesNotBoundLaterPush(t *testing.T) {
	for _, keep := range []time.Duration{0, 3 * time.Minute} {
		t.Run(keep.String(), func(t *testing.T) {
			w := checkWorld{Config: queue.Config{Slots: 1, MaxBatch: 1, Keep: keep, Timeout: time.Minute, Recheck: 3 * time.Minute}, Pulls: pulls(good), Bounds: bounds{Lost: 1}}
			c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
			s := c.initState()
			run := func() {
				t.Helper()
				out := (&checkTick{c: c, state: s}).run()
				if out.broken != "" {
					t.Fatal(out.broken)
				}
				s = out.final
			}
			run()
			s = moveState(c.ciMoves(s), step{kind: stepError, sha: s.builds[0].sha})
			s = moveState(c.clockMoves(s), step{kind: stepRecheck})
			run()
		})
	}
}

func TestKeepCheckerSerialExpiryWhileNextBuildRuns(t *testing.T) {
	w := checkWorld{Config: queue.Config{Slots: 1, MaxBatch: 1, Keep: 3 * time.Minute, Timeout: time.Minute}, Pulls: pulls(good, good, good)}
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	s := c.initState()
	run := func() {
		t.Helper()
		out := (&checkTick{c: c, state: s}).run()
		if out.broken != "" {
			t.Fatal(out.broken)
		}
		s = out.final
	}
	run()
	first := s.builds[0].sha
	for range 2 {
		idx := slices.IndexFunc(s.builds, func(b pushedBuild) bool { return b.verdict == queue.Pending })
		if idx < 0 {
			t.Fatal("no pending serial candidate")
		}
		s = moveState(c.ciMoves(s), step{kind: stepPass, sha: s.builds[idx].sha})
		run()
	}
	mark := s.builds[s.findBuild(first)].landed
	if mark == 0 || len(s.builds) != 3 {
		t.Fatalf("serial landings did not retain builds: %+v", s.builds)
	}
	for _, tr := range c.clockMoves(s) {
		if tr.step.kind == stepKeep && tr.state.kept >= mark {
			return
		}
	}
	t.Fatal("first landing's expiry was discarded while the third build ran")
}

func TestKeepCheckerUsesRealPlannerForComparison(t *testing.T) {
	w := smallKeepWorlds()[0]
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	world, _ := c.read(c.initState())
	if msg := checkKeep(world, w.Config, nil); msg == "" {
		t.Fatal("comparison accepted a planner that omitted every action")
	}
}

func TestKeepCheckerLowerTimesOutWhileTheTipIsKept(t *testing.T) {
	w, ok := findWorld("Keep whole stacked longer than timeout")
	if !ok {
		t.Fatal("missing world")
	}
	c := newChecker(w, mutant{plan: queue.Plan, verify: queue.Verify})
	out := (&checkTick{c: c, state: c.initState()}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	s := out.final
	if len(s.builds) != 2 {
		t.Fatalf("pushed %d builds, want two", len(s.builds))
	}
	lower, upper := s.builds[0].sha, s.builds[1].sha
	s = moveState(c.ciMoves(s), step{kind: stepBegin, sha: lower})
	s = moveState(c.ciMoves(s), step{kind: stepPass, sha: upper})
	out = (&checkTick{c: c, state: s, trace: true}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	s = out.final
	if s.trunk != upper || s.builds[s.findBuild(lower)].landed != 0 {
		t.Fatal("upper did not land over the pending lower")
	}
	s = moveState(c.clockMoves(s), step{kind: stepTimeout})
	world, _ := c.read(s)
	if !slices.ContainsFunc(world.Builds, func(b queue.Build) bool {
		return b.SHA == upper && !b.Landed.IsZero() && world.Now.Sub(b.Landed) < w.Config.Keep
	}) {
		t.Fatal("upper was not kept at the lower's timeout")
	}
	out = (&checkTick{c: c, state: s, trace: true}).run()
	if out.broken != "" {
		t.Fatal(out.broken)
	}
	if !slices.ContainsFunc(out.reads, func(r tickRead) bool {
		return slices.ContainsFunc(r.world.Builds, func(b queue.Build) bool {
			return b.SHA == lower && b.Verdict == queue.Pending && r.world.Now.Sub(b.Pushed) > w.Config.Timeout
		})
	}) {
		t.Fatal("no timed-out lower reading")
	}
}

func TestCheckerGuiltyWithNeeds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pulls []pullSpec
		tree  []int
		want  bool
	}{
		{"neither of the pair", pulls(good, needs(3), needs(2)), []int{1}, false},
		{"#2 without #3", pulls(good, needs(3), needs(2)), []int{1, 2}, true},
		{"#3 without #2", pulls(good, needs(3), needs(2)), []int{1, 3}, true},
		{"the pair", pulls(good, needs(3), needs(2)), []int{1, 2, 3}, false},
		{"#3 without the #2 it needs", pulls(good, good, needs(2)), []int{1, 3}, true},
		{"#3 with the #2 it needs", pulls(good, good, needs(2)), []int{1, 2, 3}, false},
		{"#2, which needs nothing", pulls(good, good, needs(2)), []int{1, 2}, false},
	} {
		c := newChecker(checkWorld{Config: dials(2, 2, true), Pulls: tc.pulls}, mutant{plan: queue.Plan, verify: queue.Verify})
		sha := queue.SHA("trunk0")
		for _, n := range tc.tree {
			sha = c.addCommit(sha, queue.SHA(fmt.Sprintf("head%d", n)), n, "")
		}
		if got := c.guilty(sha); got != tc.want {
			t.Errorf("%s: guilty %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckerCulpritPRWithNeeds(t *testing.T) {
	needing := checkedPR{number: 3, needs: 2, queued: true, order: 3, checks: queue.Success}
	for _, tc := range []struct {
		name   string
		needed checkedPR
		want   bool
	}{
		{"the one it needs landed", checkedPR{number: 2, landed: true}, false},
		{"the one it needs is queued ahead", checkedPR{number: 2, queued: true, order: 2, checks: queue.Success}, false},
		{"the one it needs is queued behind", checkedPR{number: 2, queued: true, order: 4, checks: queue.Success}, true},
		{"the one it needs goes first", checkedPR{number: 2, queued: true, first: true, order: 4, checks: queue.Success}, false},
		{"the one it needs is ahead and held", checkedPR{number: 2, queued: true, order: 2, checks: queue.Success, wait: "it is labelled hold"}, true},
		{"the one it needs is ahead and its checks run", checkedPR{number: 2, queued: true, order: 2, checks: queue.Pending}, true},
		{"the one it needs is gone", checkedPR{number: 2}, true},
	} {
		if got := culpritPR(3, []checkedPR{{number: 1, queued: true, order: 1, checks: queue.Success}, tc.needed, needing}); got != tc.want {
			t.Errorf("%s: culprit %v, want %v", tc.name, got, tc.want)
		}
	}
}

// #2 needs #1, which has landed, and #3 clashes with #2.
func TestCheckerCulpritPRWithAClashBesideANeed(t *testing.T) {
	needing := checkedPR{number: 2, needs: 1, queued: true, order: 2, checks: queue.Success}
	for _, tc := range []struct {
		name     string
		clashing checkedPR
		want     bool
	}{
		{"the clashing one is held", checkedPR{number: 3, clash: 2, queued: true, order: 3, checks: queue.Success, wait: "it is labelled hold"}, false},
		{"the clashing one is ready behind", checkedPR{number: 3, clash: 2, queued: true, order: 3, checks: queue.Success}, false},
		{"the clashing one is ready ahead", checkedPR{number: 3, clash: 2, queued: true, first: true, order: 3, checks: queue.Success}, true},
		{"the clashing one landed", checkedPR{number: 3, clash: 2, landed: true}, true},
	} {
		if got := culpritPR(2, []checkedPR{{number: 1, landed: true}, needing, tc.clashing}); got != tc.want {
			t.Errorf("%s: culprit %v, want %v", tc.name, got, tc.want)
		}
	}
}

var clashSetFirst = []string{"2 PRs moves+outside (clash, 2 of 2)", "2 PRs gather moves+outside (clash)"}

// #2 clashes with #1 and is set to go first after the two were built together. #2 is ejected on
// evidence that holds #1 beside it in every failed build, and #1 lands in the same tick, yet #1 is
// not ahead of #2 when #2 leaves.
func TestCheckerCurrentPlannerEjectsAClashingPullRequestSetFirst(t *testing.T) {
	m := mutant{plan: queue.Plan, verify: queue.Verify}
	for _, name := range clashSetFirst {
		w, ok := findWorld(name)
		if !ok {
			t.Fatalf("missing world %s", name)
		}
		res := explore(w, m)
		if res.Broken != "invariant 3: #2 is innocent and was ejected with no flake in play" {
			t.Errorf("%s: broken %q, want #2 innocent on invariant 3", w.Name, res.Broken)
			continue
		}
		eject, ok := ejectRead(newChecker(w, m), res.Trace, 2)
		if !ok {
			t.Errorf("%s: no read ejects #2:\n%s", w.Name, formatCounterexample(w, m, res))
			continue
		}
		pr := eject.world.PRs[slices.IndexFunc(eject.world.PRs, func(p queue.PR) bool { return p.Number == 2 })]
		if !pr.First || !slices.ContainsFunc(eject.world.PRs, func(p queue.PR) bool { return p.Number == 1 && !p.First }) {
			t.Errorf("%s: #2 first %v at its ejection, want #2 first and #1 queued behind it", w.Name, pr.First)
		}
		at := slices.IndexFunc(eject.actions, func(a queue.Action) bool { return a.Do == "eject" && a.PR == 2 })
		for _, sha := range eject.actions[at].Evidence[1:] {
			b := eject.world.Builds[slices.IndexFunc(eject.world.Builds, func(b queue.Build) bool { return b.SHA == sha })]
			if !slices.ContainsFunc(b.Chain, func(m queue.Member) bool { return m.Number == 1 }) {
				t.Errorf("%s: evidence %.7s does not hold #1 beside #2", w.Name, sha)
			}
		}
		if !slices.ContainsFunc(eject.actions[at+1:], func(a queue.Action) bool {
			return a.Do == "land" && slices.ContainsFunc(a.Members, func(m queue.Member) bool { return m.Number == 1 })
		}) {
			t.Errorf("%s: #1 does not land in the tick that ejects #2", w.Name)
		}
	}
}

func TestCheckerCallsAPullRequestACulpritWhenTheOneItNeedsConflictsWithTheTrunk(t *testing.T) {
	w := checkWorld{Name: "3 PRs #3 needs #2, which conflicts", Config: dials(2, 2, true), Pulls: pulls(good, conflict, needs(2)), Instant: true, Bounds: bounds{MaxStates: 5000}}
	res := explore(w, mutant{plan: queue.Plan, verify: queue.Verify})
	if res.Broken != "" || res.Capped {
		t.Errorf("broken %q, capped %v, %d states", res.Broken, res.Capped, res.States)
	}
}

func TestCheckerKillsGoingOnBesideAStackedCandidateWhereAPullRequestNeedsAnother(t *testing.T) {
	m := mutant{plan: besideStacked, verify: queue.Verify}
	for _, w := range needsWorlds()[:1] {
		if res := explore(w, m); res.Broken != "invariant 3: #3 is innocent and was ejected with no flake in play" || res.Capped {
			t.Errorf("%s: broken %q, capped %v, want #3 innocent on invariant 3 in a search not stopped at the cap", w.Name, res.Broken, res.Capped)
		}
	}
	var plain []checkWorld
	for _, w := range append(defaultWorlds(), checkWorld{"3 PRs good+breaks+good (2 of 2, probes)", dials(2, 2, true), pulls(good, breaks, good), false, true, bounds{MaxStates: 5000}}) {
		if w.Config.Probes && w.Config.Keep == 0 && !slices.ContainsFunc(w.Pulls, func(p pullSpec) bool { return p.Needs > 0 }) {
			plain = append(plain, w)
		}
	}
	sum := checkSuite(plain, m)
	t.Logf("passed in %d worlds with probes, no keep and no pull request that needs another: %d explored whole, %d stopped at the cap, %d states",
		sum.worlds, sum.whole, sum.capped, sum.states)
	for _, inv := range sum.brokenInvs {
		t.Errorf("%s is broken in %s:\n%s", inv, sum.bestByInv[inv].world.Name, formatTrace(sum.bestByInv[inv].result))
	}
}

// #1 breaks and #3 needs #2. The candidates of #1 and #2 and of all three fail, and the chain goes
// on with #3 alone while #1 and #2 are suspects. #2 is cleared and lands, and #3 leaves on the
// fair test of its own build, though the trunk with #3 now passes.
func TestCheckerCurrentPlannerEjectsAPullRequestThatNeedsASuspectSetAside(t *testing.T) {
	w := needsASuspectWorld()
	m := mutant{plan: queue.Plan, verify: queue.Verify}
	res := explore(w, m)
	t.Logf("capped %v, %d states:\n%s", res.Capped, res.States, formatCounterexample(w, m, res))
	if res.Broken != "invariant 3: #3 is innocent and was ejected with no flake in play" {
		t.Fatalf("broken %q, want #3 innocent on invariant 3", res.Broken)
	}
	c := newChecker(w, m)
	eject, ok := ejectRead(c, res.Trace, 3)
	if !ok {
		t.Fatal("no read ejects #3")
	}
	trunk := eject.world.Trunk
	if landed, fails := c.holds(trunk, "head2"), c.guilty(c.addCommit(trunk, "head3", 3, "")); !landed || fails {
		t.Errorf("#3 leaves on %.7s, which holds #2 %v and fails with #3 %v, want #2 landed and the trunk with #3 passing", trunk, landed, fails)
	}
}

func ejectRead(c *checker, trace []step, number int) (tickRead, bool) {
	var found tickRead
	var ok bool
	c.replay(trace, func(int, step) {}, func(_ string, r tickRead) {
		if !ok && slices.ContainsFunc(r.actions, func(a queue.Action) bool { return a.Do == "eject" && a.PR == number }) {
			found, ok = r, true
		}
	})
	return found, ok
}
