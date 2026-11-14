package main

import (
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
		for _, cfg := range append(configs(shape), named{"probes", dials(3, 6, true)}) {
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
	realBest := best(strings.Split(string(raw), "\n")[1:])["hey2\tchallenger"]
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
