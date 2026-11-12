package main

import (
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
			s := NewSim(storm, dials(6, 1, false), seed)
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
	sc := Score{Shape: "hey2", Config: "challenger", WaitP50: 1.2, WaitP90: 2.5, BlameP90: 3, Builds: 1.44, PerHour: 12}
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
