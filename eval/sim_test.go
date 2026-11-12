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

var _ = queue.Plan
