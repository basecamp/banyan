package main

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// One world of each shape runs to its end without breaking an invariant.
func TestOneWorldOfEachShape(t *testing.T) {
	for _, shape := range append([]Shape{storm}, shapes...) {
		runs := append(configs(shape), named{"probes", dials(3, 6, true)})
		if shape.Name == shapes[0].Name {
			kept := dials(3, 1, false)
			kept.Keep = 6 * time.Minute
			runs = append(runs, named{"keep", kept})
		}
		for _, cfg := range runs {
			if cfg.Slots == 0 {
				continue
			}
			start := time.Now()
			s := NewSim(shape, cfg.Config, 1)
			s.Run()
			t.Logf("%s %s: landed %d, wrong %d, ejected %v, whole %d, probes %d, simulated %s in %s",
				shape.Name, cfg.Name, s.Landed, s.Wrong, s.Ejected, s.Whole, s.Probes, s.now, time.Since(start).Round(time.Millisecond))
			for _, b := range s.Broken {
				t.Errorf("%s %s: %s", shape.Name, cfg.Name, b)
			}
		}
	}
}

func TestCases(t *testing.T) {
	if len(cases()) == 0 {
		t.Error("no case was read from eval/cases")
	}
	for _, w := range history(mutant{plan: queue.Plan, verify: queue.Verify}) {
		t.Error(w)
	}
}

func TestMutantsAreCaught(t *testing.T) {
	for name, m := range mutants {
		caught := 0
		for seed := int64(1); seed <= 20 && caught == 0; seed++ {
			shape := storm
			if m.waits {
				shape = tight()
			}
			if m.crashes {
				// A push to a pull request clears what a dead tick left on its head, and so
				// does taking its label off, so nobody does either here.
				shape.Moves, shape.Removals = 0, 0
			}
			s := NewSim(shape, dials(6, 1, false), seed)
			s.plan, s.verify = m.plan, m.verify
			s.Run()
			caught += len(s.Broken)
		}
		if caught == 0 {
			t.Errorf("the mutant that %s broke no invariant", name)
		}
	}
}

// Four steps that each cost 4% of builds per landing pass one by one against the newest kept
// row, which is how hey2 went from 1.44 to 1.60. Held to the best kept row since the fresh
// baselines, the second is refused, where the sum passes a twentieth, and so is each after it.
func TestSmallLossesDoNotAddUp(t *testing.T) {
	sc := Score{Shape: "hey2", Config: "challenger", WaitP50: 1.2, WaitP90: 2.5, EjectP90: 3, Builds: 1.44, PerHour: 12}
	key := sc.Shape + "\t" + sc.Config
	rows := []string{sc.row(0, "an older run: kept"), sc.row(0, "baseline: fresh")}
	newest := sc
	for step := 1; step <= 4; step++ {
		sc.Builds *= 1.04
		if len(worse(newest, sc)) > 0 {
			t.Errorf("step %d is more than a twentieth worse than the step before it, so this test shows nothing", step)
		}
		refused := len(worse(best(rows)[key], sc)) > 0
		if refused != (step >= 2) {
			t.Errorf("step %d, builds per landing %.2f against a best of %.2f: refused %v", step, sc.Builds, best(rows)[key].Builds, refused)
		}
		if !refused {
			rows = append(rows, sc.row(0, "kept"))
		} else {
			rows = append(rows, sc.row(0, "rejected"))
		}
		newest = sc
	}
}

// The best kept row starts again at fresh baselines, takes each indicator from whichever kept
// row had it best, and reads nothing from a rejected row.
func TestBestKeptRow(t *testing.T) {
	row := func(builds, perHour float64, note string) string {
		return Score{Shape: "hey2", Config: "challenger", Builds: builds, PerHour: perHour}.row(0, note)
	}
	got := best([]string{row(1.0, 20, "kept"), row(1.5, 10, "baseline: fresh"), row(1.4, 9, "kept"), row(1.6, 12, "kept"), row(1.1, 30, "rejected")})["hey2\tchallenger"]
	if got.Builds != 1.4 || got.PerHour != 12 {
		t.Errorf("best is builds %.2f and %.1f an hour, not 1.40 and 12.0", got.Builds, got.PerHour)
	}
}

var _ = queue.Plan

// Forty wrong ejections in sixty thousand landings scatter by six or seven with nothing
// changed, so eight more are not held against a run, and twenty more are.
func TestAFewMoreWrongEjectionsAreScatter(t *testing.T) {
	was := Score{Shape: "hey2", Config: "challenger", Landed: 12000, Wrong: 1000 * 40.0 / 60000}
	for more, refused := range map[float64]bool{8: false, 20: true} {
		now := was
		now.Wrong = 1000 * (40 + more) / 60000
		if got := len(worse(was, now)) > 0; got != refused {
			t.Errorf("%v more wrong ejections than 40: refused %v", more, got)
		}
	}
	none := Score{Shape: "hey2", Config: "challenger", Landed: 12000}
	two, four := none, none
	two.Wrong, four.Wrong = 1000*2.0/60000, 1000*4.0/60000
	if len(worse(none, two)) > 0 || len(worse(none, four)) == 0 {
		t.Error("against none, two wrong ejections are scatter and four are not")
	}
}

func TestLabelledToLandedKeepsTimeBeforeMendAndMove(t *testing.T) {
	// A conflict waits 50m to mend and 10m for its new head's checks before its 20m build lands it.
	s := NewSim(Shape{Build: 20 * time.Minute, Queue: []Queued{{Number: 1, At: 0, Conflict: true}}}, dials(2, 1, false), 1)
	s.Run()
	if !slices.Equal(s.Waits, []float64{1}) || !slices.Equal(s.Labelled, []float64{4.1}) {
		t.Errorf("after mend: waits %v, labelled %v, want [1] and [4.1]", s.Waits, s.Labelled)
	}

	// A push mid-build moves the head at 10m and takes 10m for checks before a fresh 20m build lands it.
	s = NewSim(Shape{Build: 20 * time.Minute, Queue: []Queued{{Number: 1, At: 0}}}, dials(2, 1, false), 1)
	for range 5 {
		s.Step()
	}
	s.move()
	s.Run()
	if !slices.Equal(s.Waits, []float64{1}) || !slices.Equal(s.Labelled, []float64{2}) {
		t.Errorf("after move: waits %v, labelled %v, want [1] and [2]", s.Waits, s.Labelled)
	}
}

func TestOldScoreboardRowsStayReadableAndJudgeHoldsNewColumns(t *testing.T) {
	if got := slowest([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40}, 20); got != 39.5 {
		t.Errorf("slowest twentieth of 1..40 is %v, want 39.5", got)
	}

	// Real 13-column history stays readable and gives empty label baselines that do not fail -judge.
	raw, err := os.ReadFile("scoreboard.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var old []string
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		if len(strings.Split(line, "\t")) == 13 {
			old = append(old, line)
		}
	}
	realBest := best(old)["hey2\tchallenger"]
	if realBest.Builds == 0 || realBest.LabelP50 != 0 || realBest.LabelP90 != 0 || realBest.LabelSlow != 0 {
		t.Fatalf("unexpected real best from 13-column scoreboard: %+v", realBest)
	}
	probe := realBest
	probe.LabelP50, probe.LabelP90, probe.LabelSlow = 2, 5, 10
	if w := worse(realBest, probe); len(w) > 0 {
		t.Errorf("13-column baseline rejected new labelled columns: %v", w)
	}

	// A 13-column baseline followed by kept 16-column rows keeps each column's best and judges against it.
	oldBaseline := "2026-11-12T05:35Z\ta97776c\they2\tchallenger\t12400\t0.00\t1.12\t2.56\t5.28\t1.37\t11.2\t2234\tbaseline: fresh"
	keptFirst := Score{Shape: "hey2", Config: "challenger", Landed: 12400, WaitP50: 1.12, WaitP90: 2.56, LabelP50: 2.0, LabelP90: 4.0, LabelSlow: 8.0, EjectP90: 5.28, Builds: 1.37, PerHour: 11.2}.row(2234, "kept")
	oldAfter := "2026-11-12T06:00Z\tb97776c\they2\tchallenger\t12400\t0.00\t1.10\t2.56\t5.28\t1.37\t11.2\t2234\tkept"
	got := best([]string{oldBaseline, keptFirst, oldAfter})["hey2\tchallenger"]
	if got.WaitP50 != 1.10 || got.LabelP50 != 2.0 || got.LabelP90 != 4.0 || got.LabelSlow != 8.0 {
		t.Fatalf("mixed 13/16-column best gave %+v", got)
	}
	slow := got
	slow.LabelSlow = 9.0
	if w := worse(got, slow); len(w) == 0 {
		t.Error("worse did not catch regression in labelled_slowest_20th")
	}
}

// Sixteen pull requests an hour arrive, and one at a time lands under two: the queue falls
// behind and does not recover. Its world is stopped and says when, with no invariant broken.
// The same world at the challenger's dials keeps up and runs to its end.
func TestAQueueThatCannotKeepUpIsStopped(t *testing.T) {
	serial := NewSim(shapes[1], dials(1, 1, false), 1)
	serial.Run()
	if serial.Stopped == 0 || len(serial.Broken) > 0 {
		t.Errorf("stopped at %s with %v broken, where one pull request at a time cannot keep up", serial.Stopped, serial.Broken)
	}
	batched := NewSim(shapes[1], dials(3, 12, true), 1)
	batched.Run()
	if batched.Stopped > 0 {
		t.Errorf("stopped at %s at three slots of twelve, where the queue keeps up", batched.Stopped)
	}
}

// The trunk breaks twice over, ten minutes apart, and each break has a fix of its own: a tree
// passes only with both. Whichever fix is built first fails, as the trunk does, and the two
// land once one is stacked on the other, where the first once waited on a red candidate of its
// own and the queue stood still.
func TestATrunkBrokenTwiceOverLandsItsTwoFixesTogether(t *testing.T) {
	twice := Shape{Build: 20 * time.Minute, Probe: 7 * time.Minute, Hours: 3, Arrivals: 2, Episodes: []Episode{
		{At: time.Hour, NeedsFix: true, Fix: 30 * time.Minute}, {At: 70 * time.Minute, NeedsFix: true, Fix: 40 * time.Minute}}}
	for _, cfg := range []queue.Config{dials(1, 1, false), dials(5, 2, false), dials(3, 8, false), dials(3, 8, true)} {
		for seed := int64(1); seed <= 5; seed++ {
			s := NewSim(twice, cfg, seed)
			s.Run()
			if s.Stopped > 0 || len(s.Broken) > 0 {
				t.Errorf("%d slots of %d, seed %d: stopped at %s with %v broken, where the two fixes together pass", cfg.Slots, cfg.MaxBatch, seed, s.Stopped, s.Broken)
			}
		}
	}
}

// Every build takes 58 minutes and lands ten pull requests, so a full chain lands ten in every
// 58 minutes. Timed from an empty chain, the hour in which nothing can land counts against it.
func TestThroughputIsCountedOnceTheChainIsFull(t *testing.T) {
	steady := Shape{Name: "steady", Build: 58 * time.Minute, Instant: true, Cancels: true, Takes: []time.Duration{118 * time.Minute}}
	sc, broken := measure(steady, dials(1, 10, false))
	if len(broken) > 0 {
		t.Fatal(broken)
	}
	if math.Abs(sc.PerHour-10) > 0.1 {
		t.Errorf("%.2f landings an hour, where a full chain lands 10", sc.PerHour)
	}
}

// Eight blocks of seeds, each landing 100 pull requests on 100 to 170 builds: builds per
// landing scatter from 1.0 to 1.7, with a standard deviation of 0.245 across blocks. Twice the
// standard error of a difference between two runs of all eight is 0.245. A change inside
// that is not called worse, and one past it is.
func TestAChangeInsideTheNoiseIsNotWorse(t *testing.T) {
	worlds := make([]worldScore, wide*seeds)
	for i := range worlds {
		worlds[i] = worldScore{landed: 4, whole: 4}
	}
	for b := 0; b < blocks; b++ {
		worlds[b*seeds/blocks].whole += 10 * b
	}
	n := noise(worlds, make([]span, seeds), Shape{})
	if builds := n[7]; math.Abs(builds-0.245) > 0.001 {
		t.Fatalf("builds per landing move %.3f by chance, where the blocks' scatter makes it 0.245", builds)
	}
	was := Score{Shape: "hey2", Config: "challenger", Landed: 100, Builds: 1.35}
	inside, past := was, was
	inside.Noise, inside.Builds = n, 1.55
	past.Noise, past.Builds = n, 1.65
	if w := worse(was, inside); len(w) > 0 {
		t.Errorf("a change of 0.20 inside noise of 0.245 was called worse: %v", w)
	}
	if w := worse(was, past); len(w) != 1 {
		t.Errorf("a change of 0.30 past noise of 0.245 gave %v", w)
	}
}

// The simulator reads a build as git does. #2 is stacked on the first attempt at merging #1.
// While that attempt is what landed, #2's build stands on the trunk with #2 alone in its
// chain. Had a second attempt landed instead, the first would be no part of the trunk, and
// the build would read as standing on the old trunk with both in its chain: the same tree,
// and a commit the trunk cannot move to.
func TestTheSimulatorReadsABuildAgainstTheTrunkAsGitDoes(t *testing.T) {
	chain := func(s *Sim, land int) (queue.SHA, []int) {
		one, two := s.arrive(), s.arrive()
		one.conflict, one.ahead, two.conflict, two.ahead = false, 0, false, 0
		first, _ := s.Merge("trunk0", one.PR, "first attempt")
		second, _ := s.Merge("trunk0", one.PR, "second attempt")
		both, _ := s.Merge(first, two.PR, "")
		s.advance([]queue.SHA{first, second}[land])
		b := s.read(queue.Build{SHA: both, Chain: []queue.Member{{Number: one.Number, Head: one.Head, Merge: first}, {Number: two.Number, Head: two.Head, Merge: both}}})
		var numbers []int
		for _, m := range b.Chain {
			numbers = append(numbers, m.Number)
		}
		return b.Base, numbers
	}
	s := NewSim(shapes[0], dials(5, 2, false), 1)
	if base, numbers := chain(s, 0); base != s.trunk || len(numbers) != 1 {
		t.Errorf("stacked on what landed, the build reads on %.7s holding %v, where it stands on the trunk %.7s holding #2 alone", base, numbers, s.trunk)
	}
	s = NewSim(shapes[0], dials(5, 2, false), 1)
	if base, numbers := chain(s, 1); base != "trunk0" || len(numbers) != 2 {
		t.Errorf("stacked beside what landed, the build reads on %.7s holding %v, where it stands on the old trunk holding both", base, numbers)
	}
}

// The second to arrive is queued before the first, which clashes with it.
func TestTheSimulatorJudgesAClashByQueueOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		later bool
		want  bool
	}{
		{"the later taken out", true, true},
		{"the earlier taken out with the later ready behind it", false, false},
	} {
		s := NewSim(shapes[0], dials(5, 2, false), 1)
		one, two := s.arrive(), s.arrive()
		for _, p := range []*simPR{one, two} {
			p.breaks, p.clash, p.conflict, p.ahead, p.Checks, p.Wait = false, 0, false, 0, queue.Success, ""
		}
		one.clash, one.Queued = two.Number, two.Queued.Add(time.Minute)
		out := map[bool]*simPR{true: one, false: two}[tc.later]
		out.queued = false
		if got := s.culprit(out); got != tc.want {
			t.Errorf("%s: culprit %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestKeepSimRetainsBuildsUntilExpiry(t *testing.T) {
	cfg := dials(1, 1, false)
	cfg.Keep = 5 * time.Minute
	s := NewSim(Shape{Build: time.Minute, Backlog: 1}, cfg, 1)
	s.Step()
	s.Step()
	if s.Landed != 1 || len(s.builds) != 1 {
		t.Fatalf("landing: landed=%d builds=%d", s.Landed, len(s.builds))
	}
	for _, b := range s.builds {
		if len(s.World().Builds) != 1 || s.World().Builds[0].Landed.IsZero() {
			t.Fatal("landing mark not read")
		}
		if !b.Landed.Equal(s.epoch.Add(s.now)) {
			t.Fatal("landing did not record its own time")
		}
	}
	s.Step()
	s.Step()
	if len(s.builds) != 1 {
		t.Fatal("deleted inside keep")
	}
	s.Step()
	if len(s.builds) != 0 {
		t.Fatal("did not delete at keep")
	}
	if len(s.Broken) != 0 {
		t.Fatal(s.Broken)
	}
}

func TestKeepSimDoesNotCountKeptBranchesAsOverload(t *testing.T) {
	cfg := dials(1, 1, false)
	cfg.Keep = time.Hour
	s := NewSim(Shape{}, cfg, 1)
	for i := range 301 {
		sha := fmt.Sprint(i)
		s.trunks[sha] = true
		s.builds[sha] = &simBuild{Build: queue.Build{SHA: sha, Verdict: queue.Success, Landed: s.epoch}}
	}
	if s.overloaded() {
		t.Fatal("kept branches counted as overload")
	}
	if !s.idle() {
		t.Fatal("marked branches prevented idle")
	}
	for _, verdict := range []queue.Verdict{queue.Pending, queue.Success} {
		for _, b := range s.builds {
			b.Verdict, b.Landed = verdict, time.Time{}
		}
		if !s.overloaded() {
			t.Fatalf("verdict=%d: landed branches without marks escaped overload count", verdict)
		}
		keptOverload, keptIdle := s.overloaded(), s.idle()
		s.cfg.Keep = 0
		if keptOverload != s.overloaded() || keptIdle != s.idle() {
			t.Fatalf("verdict=%d: landed branches without marks changed overload or idle", verdict)
		}
		s.cfg.Keep = cfg.Keep
	}
}

func TestKeepSimMarksLowerOnlyOnceItsVerdictIsIn(t *testing.T) {
	for _, cancels := range []bool{false, true} {
		t.Run(fmt.Sprint(cancels), func(t *testing.T) {
			cfg := queue.Config{Slots: 2, MaxBatch: 1, Keep: time.Hour}
			s := NewSim(Shape{Cancels: cancels, Queue: []Queued{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}}, Build: tick, Takes: []time.Duration{10 * tick, tick}}, cfg, 1)
			s.Step()
			first := ""
			for sha, b := range s.builds {
				if len(b.Chain) == 1 {
					first = sha
				}
			}
			if first == "" {
				t.Fatal("no lower candidate")
			}
			for n := 0; n < 5 && s.Landed < 2; n++ {
				s.Step()
			}
			if len(s.Broken) != 0 || s.Landed != 2 {
				t.Fatalf("landed=%d broken=%v", s.Landed, s.Broken)
			}
			w := s.World()
			pending := 0
			for _, b := range w.Builds {
				if b.Kind == queue.Candidate && b.Verdict == queue.Pending {
					pending++
				}
			}
			if pending != cfg.Slots {
				t.Fatalf("pending candidates = %d, want %d", pending, cfg.Slots)
			}
			lower := s.builds[first]
			if (lower == nil) != cancels {
				t.Fatalf("cancels=%v lower=%+v", cancels, lower)
			}
			if lower != nil {
				if lower.Verdict != queue.Pending || !lower.Landed.IsZero() || !s.trunks[first] {
					t.Fatalf("lower build is not landed and pending: %+v", lower)
				}
				read := func() queue.Build {
					t.Helper()
					for _, b := range s.World().Builds {
						if b.SHA == first {
							return b
						}
					}
					t.Fatal("lower branch missing from world")
					return queue.Build{}
				}
				if b := read(); b.Verdict != queue.Pending || !b.Landed.IsZero() {
					t.Fatalf("pending lower was marked: %+v", b)
				}
				lower.Verdict = queue.Failure
				if b := read(); !b.Landed.Equal(s.epoch.Add(s.now)) || b.Verdict != queue.Failure {
					t.Fatalf("answered lower was not marked: %+v", b)
				}
				if msg := checkKeep(s.World(), cfg, queue.Plan(s.World(), cfg)); msg != "" {
					t.Fatal(msg)
				}
			}
		})
	}
}

func TestKeepSimCatchesEarlyAndLateDeletion(t *testing.T) {
	for _, cm := range keepMutants()[:2] {
		t.Run(cm.name, func(t *testing.T) {
			cfg := dials(1, 1, false)
			cfg.Keep = 6 * time.Minute
			s := NewSim(Shape{Build: tick, Backlog: 1}, cfg, 1)
			s.plan = cm.mutant.plan
			s.Run()
			rule := "keep retention:"
			if strings.HasPrefix(cm.name, "never") {
				rule = "keep expiry:"
			}
			if !slices.ContainsFunc(s.Broken, func(msg string) bool { return strings.HasPrefix(msg, rule) }) {
				t.Fatalf("mutant broke %v, want %s", s.Broken, rule)
			}
			t.Logf("%v", s.Broken)
		})
	}
}

func TestKeepSimCrashBeforeMarkIsRepaired(t *testing.T) {
	cfg := dials(1, 1, false)
	cfg.Keep = 5 * time.Minute
	s := NewSim(Shape{Build: time.Minute, Backlog: 1}, cfg, 1)
	s.Step()
	var sha string
	for _, b := range s.builds {
		sha = b.SHA
		b.Verdict = queue.Success
	}
	s.now += time.Minute
	s.crashAfterPush = true
	for _, a := range queue.Plan(s.World(), cfg) {
		if a.Do == "land" {
			s.apply(a)
		}
	}
	if !s.trunks[sha] || !s.builds[sha].Landed.IsZero() {
		t.Fatal("crash did not fall before mark")
	}
	if !s.kept() {
		t.Fatal("run can end with an unmarked landing")
	}
	s.now += time.Minute
	s.World()
	if !s.builds[sha].Landed.Equal(s.epoch.Add(s.now)) {
		t.Fatal("next read did not mark")
	}
}

func TestKeepSimCrashMarksLandingBeforeALaterAction(t *testing.T) {
	continued := false
	for seed := int64(1); seed <= 20; seed++ {
		cfg := dials(1, 1, false)
		cfg.Keep = 5 * time.Minute
		s := NewSim(Shape{Build: time.Minute, Backlog: 1}, cfg, seed)
		s.Step()
		var sha queue.SHA
		for _, b := range s.builds {
			sha, b.Verdict = b.SHA, queue.Success
		}
		s.shape.Crash = 1
		s.plan = func(w queue.World, c queue.Config) []queue.Action {
			for _, a := range queue.Plan(w, c) {
				if a.Do == "land" {
					return []queue.Action{a, {Do: "note", PR: 1, Text: "after landing"}}
				}
			}
			return nil
		}
		s.tick()
		if s.prs[1].Note == "after landing" {
			continued = true
			if s.builds[sha].Landed.IsZero() {
				t.Fatal("landing left unmarked though the crashed tick continued")
			}
		}
	}
	if !continued {
		t.Fatal("no crash continued past the landing")
	}
}
