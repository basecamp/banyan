package main

import (
	"slices"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type checkMutant struct {
	name   string
	mutant mutant
}

func checkerMutants() []checkMutant {
	var all []checkMutant
	var names []string
	for name := range mutants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		all = append(all, checkMutant{name: name, mutant: mutants[name]})
	}
	return append(all,
		checkMutant{
			name: "ejects a waiting pull request",
			mutant: mutant{
				verify: queue.Verify,
				plan: func(w queue.World, c queue.Config) []queue.Action {
					for _, pr := range w.PRs {
						if pr.Wait != "" {
							return []queue.Action{{Do: "eject", PR: pr.Number, Cause: "conflict"}}
						}
					}
					return queue.Plan(w, c)
				},
			},
		},
		checkMutant{
			name: "acts on an unchanged world",
			mutant: mutant{
				verify: queue.Verify,
				plan: func(w queue.World, c queue.Config) []queue.Action {
					acts := queue.Plan(w, c)
					if len(acts) == 0 && len(w.PRs) > 0 {
						return []queue.Action{{Do: "note", PR: w.PRs[0].Number, Text: w.PRs[0].Note}}
					}
					return acts
				},
			},
		},
		checkMutant{
			name: "spans a round that had not begun",
			mutant: mutant{
				verify: queue.Verify,
				waits:  true,
				plan: func(w queue.World, c queue.Config) []queue.Action {
					w.Builds = slices.Clone(w.Builds)
					for i, b := range w.Builds {
						if b.Round > 0 && (!b.Started.IsZero() || (c.Timeout > 0 && w.Now.Sub(b.Pushed) > c.Timeout)) {
							w.Builds[i].Started = time.Unix(1, 0)
						}
					}
					return queue.Plan(w, c)
				},
			},
		},
	)
}

func mutantWorlds() []checkWorld {
	cfg := dials(2, 1, false)
	return []checkWorld{
		{"1 PR (breaks)", cfg, pulls(breaks), false, false, bounds{MaxStates: 500}},
		{"1 PR hold (waiting)", cfg, pulls(hold), false, false, bounds{MaxStates: 500}},
		{"1 PR (good) episodes=1", cfg, pulls(good), false, false, bounds{Episodes: 1, MaxStates: 2000}},
		{"1 PR crashes+stale+moves", cfg, pulls(good), false, false, bounds{Crashes: 1, Stale: 1, Moves: 1, MaxStates: 6000}},
		{"2 PRs (good+good) flakes+crashes", cfg, pulls(good, good), false, false, bounds{Flakes: 1, Crashes: 1, MaxStates: 8000}},
		{"2 PRs (good+breaks) timeouts", dials(2, 2, false), pulls(good, breaks), false, false, bounds{Timeouts: 1, MaxStates: 2000}},
	}
}

func keepMutants() []checkMutant {
	return []checkMutant{
		{name: "deletes a landed build before keep", mutant: mutant{verify: queue.Verify, plan: func(w queue.World, c queue.Config) []queue.Action {
			c.Keep = 0
			return queue.Plan(w, c)
		}}},
		{name: "never deletes a marked build", mutant: mutant{verify: queue.Verify, plan: func(w queue.World, c queue.Config) []queue.Action {
			return slices.DeleteFunc(queue.Plan(w, c), func(a queue.Action) bool {
				return c.Keep > 0 && a.Do == "delete" && slices.ContainsFunc(w.Builds, func(b queue.Build) bool { return !b.Landed.IsZero() && b.SHA == a.SHA })
			})
		}}},
		{name: "marks a running candidate under the trunk", mutant: mutant{verify: queue.Verify, plan: queue.Plan, markRunning: true}},
		{name: "stacks on a kept landed build", mutant: mutant{verify: queue.Verify, plan: func(w queue.World, c queue.Config) []queue.Action {
			acts := queue.Plan(w, c)
			for _, b := range w.Builds {
				if c.Keep <= 0 || b.Landed.IsZero() || b.SHA == w.Trunk || w.Now.Sub(b.Landed) >= c.Keep {
					continue
				}
				for i, a := range acts {
					if a.Do != "push" || a.Build.Kind != queue.Candidate {
						continue
					}
					a.Build.Base, a.Build.Chain = b.SHA, slices.Clone(a.Build.Chain)
					parent := b.SHA
					for j, m := range a.Build.Chain {
						pr := w.PRs[slices.IndexFunc(w.PRs, func(pr queue.PR) bool { return pr.Number == m.Number })]
						parent, _ = w.Repo.Merge(parent, pr, "")
						a.Build.Chain[j].Merge = parent
					}
					a.Build.SHA = parent
					acts[i] = a
				}
				break
			}
			return acts
		}}},
	}
}

func keepMutantWorlds() []checkWorld {
	cfg := dials(3, 1, false)
	cfg.Keep = 3 * time.Minute
	return append(append(smallKeepWorlds(), addedKeepWorlds()...), []checkWorld{
		{Name: "Keep mutants stacked", Config: cfg, Pulls: pulls(good, good), Bounds: bounds{MaxStates: 2000}},
		{Name: "Keep mutants older ancestor", Config: cfg, Pulls: pulls(good, good, hold), Bounds: bounds{Moves: 1, MaxStates: 6000}},
	}...)
}
