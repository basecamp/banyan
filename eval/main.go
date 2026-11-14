// The eval: go run ./eval
//
// It plays the planner against simulated worlds whose hidden facts decide every verdict,
// holds it to the charter's invariants, plays the cases in eval/cases, measures the charter's
// indicators, and compares each with the best it has been on a kept row since the last fresh
// baselines. It exits 1 on a broken invariant, on a failed case, on an eval that a broken
// planner passes, and on an indicator that got worse.
//
//	go run ./eval                    check against the scoreboard
//	go run ./eval -record "<note>"   append this run's rows to the scoreboard
//	go run ./eval -judge "<idea>"    append them with the verdict the rule gives: kept or rejected
//	go run ./eval -try "<dials>"     measure a shape under other dials, and record nothing
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// The shapes the indicators are measured on.
//
// hey2 is fitted to what it measured, and it cancels a build whose branch is deleted. Its pool
// is the 13 agents that keep its compiles warm, which hey-native-apps' jobs have shared since
// 2026-11-13: in the 13 hours after, 49 of those an hour ran there for 9 minutes each and 10 of
// hey2's other builds for 19, and nothing waited, since a build that found them busy started
// on a cold agent, as 31 in 100 did (`go run ./eval -fit`). Its queue's builds that passed
// took 31 minutes on a warm agent and 36 on a cold one. Of its first 45 builds of
// trees that also passed, 8 failed: 5 on an agent that was lost, which its pipeline now runs
// again, and 3 on a step that dropped its connection, since fixed. None of the 15 after both
// failed, so its flake rate is taken as 1 in 20 until more builds say. A queue build stops at
// its first failed test: the four that failed on a broken tree took 19 to 23 minutes.
// Nothing reaches its trunk but through the queue. In its first day with no other way in, 57
// pull requests landed and 8 conflicted with the trunk and left. Six were mended, with their
// own checks passed 47 minutes to nine hours later. Four of those were labelled again, 10 to
// 91 minutes after that, and two never were.
//
// hey-native-apps is moving to Buildkite, which builds its candidates beside the CI it lands
// on. Its build time is those builds': the 30 that
// passed from 2026-11-13 took 42 minutes, 35 at their tenth fastest in a hundred and 52 at
// their tenth slowest, and the red ones took 38, so a broken tree reports late. Their jobs
// waited two minutes or less for an agent nine times in ten, so its pool has no limit. No probe
// has run there and too few builds have for a flake rate, so its probe is taken as a third of
// a build and its rates are estimates.
var shapes = []Shape{
	{Name: "hey2", Build: 31 * time.Minute, Spread: 0.15, Fails: 0.6, Flake: 0.05, Break: 0.03, Clash: 0.02, Conflict: 0.12, Ahead: 0.02,
		Lost: 0.005, Errored: 0.01, Backlog: 15, Arrivals: 5, Hours: 10, Moves: 0.3, Removals: 0.05,
		Agents: 13, Cold: 5 * time.Minute, Others: 59, Burst: 4, Hold: 10 * time.Minute, Cancels: true},
	{Name: "hey-native-apps", Build: 42 * time.Minute, Probe: 14 * time.Minute, Spread: 0.2, Fails: 0.9, Flake: 0.07, ProbeFlake: 0.02,
		Break: 0.015, Clash: 0.005, Conflict: 0.08, Ahead: 0.03, Lost: 0.005, Errored: 0.01,
		Backlog: 20, Arrivals: 16, Hours: 10, Moves: 1, Removals: 0.1, Cancels: true},
}

// stages are the other shapes -try measures.
var stages = []Shape{sandbox()}

// sandbox is the rehearsal repository's volume run: thirty pull requests queued twenty seconds
// apart, one in ten builds a flake and one pair that clashes, on a CI that neither cancels nor
// says when a build began. Its builds are little longer than a tick, so its row is coarse.
func sandbox() Shape {
	s := Shape{Name: "sandbox", Build: 200 * time.Second, Flake: 0.1, Hours: 1.0 / 6, Silent: true}
	for i := 0; i < 30; i++ {
		q := Queued{Number: 101 + i, At: time.Duration(i) * 20 * time.Second}
		if i == 15 {
			q.Clash = 115
		}
		s.Queue = append(s.Queue, q)
	}
	return s
}

// storm is the world the invariants are held in: everything that can go wrong, often.
var storm = Shape{Name: "storm", Build: 25 * time.Minute, Probe: 8 * time.Minute, Flake: 0.05, ProbeFlake: 0.03,
	Break: 0.05, Clash: 0.05, Conflict: 0.05, Ahead: 0.05, Lost: 0.03, Errored: 0.05,
	Backlog: 8, Arrivals: 6, Hours: 6, Moves: 1, Removals: 0.5, Outside: 0.4, Crash: 0.15, Race: 0.3, Cancels: true,
	Episodes: []Episode{{At: time.Hour, For: 70 * time.Minute}, {At: 4 * time.Hour, NeedsFix: true, Fix: 20 * time.Minute}}}

// tight is the storm on a pool of three agents that builds deleted branches to their end,
// where builds wait their turn and outwait the timeout.
func tight() Shape {
	s := storm
	s.Name, s.Agents, s.Others, s.Hold, s.Cancels = "storm on three agents", 3, 2, storm.Build, false
	return s
}

type named struct {
	Name string
	queue.Config
}

func dials(slots, batch int, probes bool) queue.Config {
	return queue.Config{Slots: slots, MaxBatch: batch, Probes: probes, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}
}

// challenger is the configuration each shape runs with, or would. The hillclimb moves these,
// and hey2's moves only with the settings hey2 runs.
var challenger = map[string]queue.Config{
	"hey2":            dials(5, 2, false),
	"hey-native-apps": dials(3, 12, true),
}

// configs is the challenger and the baselines it has to beat, all settings of one planner.
func configs(s Shape) []named {
	probes := s.Probe > 0
	return []named{
		{"challenger", challenger[s.Name]},
		{"serial", dials(1, 1, false)},
		{"every-prefix", dials(8, 1, false)},
		{"batched", dials(2, 24, probes)},
	}
}

// Score is one row of the scoreboard.
type Score struct {
	Shape, Config                                                            string
	Landed                                                                   int
	Wrong, WaitP50, WaitP90, LabelP50, LabelP90, LabelSlow, EjectP90, Builds float64
	PerHour                                                                  float64
	// What the queue costs everyone else: how long the other builds wait for an agent, or how
	// much longer they take on a cold one, in minutes, beside the queue and with no queue. It
	// is printed, and not on the scoreboard.
	Beside, Alone agentWait
	// CI's own failures, which only a live score counts: the whole builds of trees that some
	// whole build passed, and those of them that did not pass.
	Good, Flaked int
	// Landings of a pull request ejected as the cause at an earlier head: unchecked when scored
	// without a clone, and unreadable when a commit could not be read from the clone.
	Unchecked, Unreadable int
}

type agentWait struct{ Mean, P90, Share float64 }

func waited(minutes []float64) agentWait {
	var sum, some float64
	for _, m := range minutes {
		sum += m
		if m > 0 {
			some++
		}
	}
	return agentWait{sum / float64(max(len(minutes), 1)), percentile(minutes, 90), some / float64(max(len(minutes), 1))}
}

const seeds = 200

// wide is how many times more worlds wrong ejections are counted over than the rest. They are
// a few in ten thousand landings: over one set of worlds the same planner counts 5 in one
// thousand worlds and 15 in the next.
const wide = 5

const columns = "%-16s %-13s %7s %9s %8s %8s %8s %8s %9s %9s %10s %8s\n"

// print is the score's line in a run's output, and under it what the queue costs the other
// builds, where the shape's pool has a limit.
func (sc Score) print(shape Shape) {
	fmt.Printf("%-16s %-13s %7d %9.2f %8.2f %8.2f %8.2f %8.2f %9.2f %9.2f %10.2f %8.1f\n", sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.LabelP50, sc.LabelP90, sc.LabelSlow, sc.EjectP90, sc.Builds, sc.PerHour)
	switch {
	case shape.Cold > 0:
		fmt.Printf("  %.0f in 100 of the other builds start on a cold agent and take %s longer, against %.0f with no queue\n",
			100*sc.Beside.Share, shape.Cold, 100*sc.Alone.Share)
	case shape.Agents > 0:
		fmt.Printf("  the other builds wait %.1f min longer for an agent: %.1f at the mean and %.1f at p90, against %.1f and %.1f with no queue\n",
			sc.Beside.Mean-sc.Alone.Mean, sc.Beside.Mean, sc.Beside.P90, sc.Alone.Mean, sc.Alone.P90)
	}
	if sc.Unchecked > 0 || sc.Unreadable > 0 {
		var parts []string
		if sc.Unchecked > 0 {
			parts = append(parts, fmt.Sprintf("%d landed at a newer head after ejection unchecked", sc.Unchecked))
		}
		if sc.Unreadable > 0 {
			parts = append(parts, fmt.Sprintf("%d unreadable in the clone", sc.Unreadable))
		}
		fmt.Printf("  %s\n", strings.Join(parts, "; "))
	}
}

// each runs fn for 1..n on every core and returns the results in order, so a run's output
// does not depend on scheduling.
func each[T any](n int, fn func(i int) T) []T {
	out := make([]T, n)
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < max(runtime.NumCPU()/2, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				out[i-1] = fn(i)
			}
		}()
	}
	for i := 1; i <= n; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	return out
}

func measure(shape Shape, cfg queue.Config) (Score, []string) {
	var sc Score
	var waits, labelled, ejects []float64
	var broken []string
	var wrong, landed, whole, probes int
	var beside, alone []float64
	for i, s := range each(wide*seeds, func(i int) *Sim { s := NewSim(shape, cfg, int64(i)); s.Run(); return s }) {
		wrong, landed = wrong+s.Wrong, landed+s.Landed
		for _, b := range s.Broken {
			broken = append(broken, fmt.Sprintf("%s seed %d: %s", shape.Name, i+1, b))
		}
		if i >= seeds {
			continue
		}
		if s.pool != nil {
			beside, alone = append(beside, s.pool.Waited...), append(alone, s.alone.Waited...)
		}
		sc.Landed += s.Landed
		whole, probes = whole+s.Whole, probes+s.Probes
		waits, labelled, ejects = append(waits, s.Waits...), append(labelled, s.Labelled...), append(ejects, s.Ejects...)
	}
	if sc.Landed > 0 {
		cost := float64(whole)
		if shape.Probe > 0 {
			cost += float64(probes) * float64(shape.Probe) / float64(shape.Build)
		}
		sc.Wrong, sc.Builds = 1000*float64(wrong)/float64(landed), cost/float64(sc.Landed)
	}
	sc.WaitP50, sc.WaitP90, sc.EjectP90 = percentile(waits, 50), percentile(waits, 90), percentile(ejects, 90)
	sc.LabelP50, sc.LabelP90, sc.LabelSlow = percentile(labelled, 50), percentile(labelled, 90), slowest(labelled, 20)
	sc.Beside, sc.Alone = waited(beside), waited(alone)

	// Throughput is what lands in an hour while a backlog stands: 120 queued and nothing
	// arriving, timed to the landing that leaves a quarter of them. The last to land are the
	// ones that waited on their authors, and how long an author takes is no part of what the
	// queue can do.
	backlog := shape
	backlog.Backlog, backlog.Arrivals, backlog.Hours = 120, 0, 0
	var drained int
	var hours float64
	for _, s := range each(seeds, func(i int) *Sim { s := NewSim(backlog, cfg, int64(i)); s.Run(); return s }) {
		if n := 3 * backlog.Backlog / 4; len(s.Lands) >= n {
			drained, hours = drained+n, hours+s.Lands[n-1].Hours()
		}
	}
	if hours > 0 {
		sc.PerHour = float64(drained) / hours
	}
	return sc, broken
}

func percentile(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[min(len(xs)-1, len(xs)*p/100)]
}

func slowest(xs []float64, n int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	k := max(1, len(xs)/n)
	var sum float64
	for _, x := range xs[len(xs)-k:] {
		sum += x
	}
	return sum / float64(k)
}

// kinds is one configuration of each kind: every prefix, serial, batched with probes and
// without, and a narrow batch.
var kinds = []queue.Config{dials(6, 1, false), dials(1, 1, false), dials(4, 12, true), dials(2, 24, false), dials(3, 6, true)}

// invariants holds the planner to the charter in the storm, with and without flakes and on a
// tight pool, under each kind of configuration, and in every small world.
func invariants(m mutant, worlds int) []string {
	var broken []string
	calm := storm
	calm.Name, calm.Flake, calm.ProbeFlake = "storm without flakes", 0, 0
	for _, shape := range []Shape{storm, calm, tight()} {
		if m.uncrashed {
			shape.Crash = 0
		}
		if m.unwaited && shape.Agents > 0 {
			continue
		}
		shape.Instant = m.unwaited
		for _, cfg := range kinds {
			for i, s := range each(worlds, func(i int) *Sim {
				s := NewSim(shape, cfg, int64(i))
				s.plan, s.verify = m.plan, m.verify
				s.Run()
				return s
			}) {
				for _, b := range s.Broken {
					broken = append(broken, fmt.Sprintf("%s, %d slots of %d, probes %v, seed %d: %s", shape.Name, cfg.Slots, cfg.MaxBatch, cfg.Probes, i+1, b))
				}
			}
		}
	}
	return append(broken, small(m)...)
}

// small plays every world of three pull requests: each one good, breaking, clashing or
// conflicting with one before it, under every pattern of flakes in the first six builds.
func small(m mutant) []string {
	var broken []string
	kinds := []int{0, 1, 2, 3} // good, breaks, clashes with the one before, conflicts with the one before
	for _, cfg := range []queue.Config{dials(2, 1, false), dials(2, 3, false), dials(2, 3, true)} {
		for _, a := range kinds[:2] {
			for _, b := range kinds {
				for _, c := range kinds {
					for script := 0; script < 64; script++ {
						shape := Shape{Name: "small", Build: 20 * time.Minute, Probe: 6 * time.Minute, Backlog: 3, Script: make([]bool, 6), Instant: m.unwaited}
						for i := range shape.Script {
							shape.Script[i] = script>>i&1 == 1
						}
						s := NewSim(shape, cfg, 1)
						s.plan, s.verify = m.plan, m.verify
						for n, kind := range []int{a, b, c} {
							p := s.prs[n+1]
							p.breaks, p.clash, p.conflict, p.ahead, p.Checks = kind == 1, 0, false, 0, queue.Success
							if kind == 2 {
								p.clash = n
							} else if kind == 3 {
								p.ahead = n
							}
						}
						s.Run()
						for _, x := range s.Broken {
							broken = append(broken, fmt.Sprintf("small %d%d%d, flakes %06b, %d slots of %d, probes %v: %s", a, b, c, script, cfg.Slots, cfg.MaxBatch, cfg.Probes, x))
						}
					}
				}
			}
		}
	}
	return broken
}

// A mutant is a queue broken on purpose. An eval that passes one is blind.
type mutant struct {
	plan   func(queue.World, queue.Config) []queue.Action
	verify func(queue.World, queue.Action) error
	// crashes marks a queue that is right until a tick dies partway, which is the sixth
	// invariant's own failure. uncrashed plays it in worlds where no tick does.
	crashes, uncrashed bool
	// waits marks a queue that is right until a build waits to begin, which is what the
	// eighth invariant's fair test is for. unwaited plays it in worlds where none does.
	waits, unwaited bool
}

var mutants = map[string]mutant{
	"lands a red candidate": {verify: func(queue.World, queue.Action) error { return nil },
		plan: func(w queue.World, c queue.Config) []queue.Action {
			for _, b := range w.Builds {
				if b.Kind == queue.Candidate && b.Verdict == queue.Failure {
					return []queue.Action{{Do: "land", SHA: b.SHA, Members: b.Chain}}
				}
			}
			return queue.Plan(w, c)
		}},
	"lands a head that moved": {verify: func(queue.World, queue.Action) error { return nil }, plan: queue.Plan},
	"ejects at the first red": {verify: queue.Verify,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			for _, b := range w.Builds {
				if b.Kind == queue.Candidate && b.Verdict == queue.Failure && len(b.Chain) > 0 {
					return []queue.Action{{Do: "eject", PR: b.Chain[len(b.Chain)-1].Number, Cause: "culprit"}}
				}
			}
			return queue.Plan(w, c)
		}},
	"blames whatever order a test's builds began in": {verify: queue.Verify, waits: true,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			w.Builds = slices.Clone(w.Builds)
			for i := range w.Builds {
				w.Builds[i].Started = w.Builds[i].Pushed
			}
			return queue.Plan(w, c)
		}},
	"never lands": {verify: queue.Verify,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			var acts []queue.Action
			for _, a := range queue.Plan(w, c) {
				if a.Do != "land" {
					acts = append(acts, a)
				}
			}
			return acts
		}},
	"holds a lock across its tick": {verify: queue.Verify, crashes: true,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			for _, pr := range w.PRs {
				if pr.Note == "locked" {
					return nil
				}
			}
			acts := queue.Plan(w, c)
			if len(acts) == 0 || len(w.PRs) == 0 {
				return acts
			}
			holder, after := w.PRs[0].Number, w.PRs[0].Note
			for _, a := range acts {
				if a.Do == "note" && a.PR == holder {
					after = a.Text
				}
			}
			acts = append([]queue.Action{{Do: "note", PR: holder, Text: "locked"}}, acts...)
			return append(acts, queue.Action{Do: "note", PR: holder, Text: after})
		}},
	"builds a commit twice": {verify: queue.Verify,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			acts := queue.Plan(w, c)
			for _, b := range w.Builds {
				if b.Verdict == queue.Failure {
					acts = append(acts, queue.Action{Do: "push", Build: b})
				}
			}
			return acts
		}},
}

const scoreboard = "eval/scoreboard.tsv"
const header = "when\tcommit\tshape\tconfig\tlanded\twrong_per_1000\twait_p50\twait_p90\teject_p90\tbuilds_per_landing\tlandings_per_hour\tengine_lines\tnote\tlabelled_p50\tlabelled_p90\tlabelled_slowest_20th"

func main() {
	record := flag.String("record", "", "append this run to the scoreboard, with this note")
	judge := flag.String("judge", "", "record this run as the test of this hypothesis, with the verdict the rule gives it: kept, or rejected and why")
	baselines := flag.Bool("baselines", false, "measure the baselines too, as -record does")
	trace := flag.String("trace", "", `replay one world and print what the queue did: "<shape> <slots> <batch> <probes> <seed>"`)
	try := flag.String("try", "", `measure shapes under other dials, and record nothing: "<shape> <slots> <batch> <probes> [<flake rate> [<build minutes> [<share of a build a broken tree takes to fail> [<share of the agents broken for three hours>]]]]; ..."`)
	fitted := flag.Bool("fit", false, "play each shape's pool with no queue, and print it beside what its pipeline measured")
	recordings := flag.String("live", "", "score a real queue from the folder its ticks recorded into, and record nothing")
	clone := flag.String("clone", "", "with -live: local clone to compare changes when a pull request ejected as the cause lands at a newer head")
	like := flag.String("like", "", "with -live: play the same arrivals on this shape's CI too, and print that row under the live one")
	again := flag.String("replay", "", "plan one recorded tick again from its recording alone, and print the actions")
	flag.Parse()
	if *again != "" {
		if err := planAgain(*again); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return
	}
	if *recordings != "" {
		sc, win, err := live(*recordings, *clone)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf(columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "label50", "label90", "slow20th", "eject90", "builds/pr", "per hour")
		sc.print(Shape{})
		for _, shape := range shapes {
			if shape.Name == *like {
				win.simulated(shape, sc).print(Shape{})
			}
		}
		return
	}
	if *trace != "" {
		replay(*trace)
		return
	}
	if *fitted {
		fit()
		return
	}
	if *try != "" {
		fmt.Printf(columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "label50", "label90", "slow20th", "eject90", "builds/pr", "per hour")
		for _, spec := range strings.Split(*try, ";") {
			var name string
			var slots, batch int
			var probes bool
			flake, minutes, fails, out := -1.0, 0.0, 0.0, 0.0
			fmt.Sscan(spec, &name, &slots, &batch, &probes, &flake, &minutes, &fails, &out)
			for _, shape := range slices.Concat(shapes, stages) {
				if shape.Name == name {
					sc := Score{Config: fmt.Sprintf("%d of %d", slots, batch)}
					if flake >= 0 {
						shape.Flake = flake
						sc.Config += fmt.Sprintf(", %.0f%% flakes", 100*flake)
					}
					if minutes > 0 {
						shape.Build = time.Duration(minutes * float64(time.Minute))
						sc.Config += fmt.Sprintf(", %.0f min", minutes)
					}
					if fails > 0 {
						shape.Fails = fails
						sc.Config += fmt.Sprintf(", red in %.0f%%", 100*fails)
					}
					if out > 0 {
						shape.Episodes = append(shape.Episodes, Episode{At: 2 * time.Hour, For: 3 * time.Hour, Share: out})
						sc.Config += fmt.Sprintf(", %.0f%% of agents out for 3h", 100*out)
					}
					config := sc.Config
					sc, _ = measure(shape, dials(slots, batch, probes))
					sc.Shape, sc.Config = name, config
					sc.print(shape)
				}
			}
		}
		return
	}
	failed := false
	var why []string // what the rule holds against this run

	broken := invariants(mutant{plan: queue.Plan, verify: queue.Verify}, seeds/2)
	if len(broken) > 0 {
		why = append(why, fmt.Sprintf("%d invariants broken, the first in %s", len(broken), broken[0]))
	}
	fmt.Printf("invariants: %d broken\n", len(broken))
	for i, b := range broken {
		if i == 12 {
			fmt.Printf("  ... and %d more\n", len(broken)-i)
			break
		}
		fmt.Println("  " + b)
	}
	failed = failed || len(broken) > 0

	wrong := history(mutant{plan: queue.Plan, verify: queue.Verify})
	fmt.Printf("cases: %d played, %d failures\n", len(cases()), len(wrong))
	for _, w := range wrong {
		fmt.Println("  " + w)
	}
	if len(wrong) > 0 {
		why = append(why, "a case fails: "+wrong[0])
	}
	failed = failed || len(wrong) > 0

	var names []string
	for name := range mutants {
		names = append(names, name)
	}
	sort.Strings(names)
	fails := map[string]int{}
	for _, name := range names {
		seen := map[string]bool{}
		for _, w := range history(mutants[name]) {
			seen[w[:strings.Index(w, ",")]] = true
		}
		for c := range seen {
			fails[c]++
		}
		kinds := map[string]bool{}
		for _, b := range invariants(mutants[name], 8) {
			kinds[b[strings.Index(b, "invariant "):][:11]] = true
		}
		var which []string
		for k := range kinds {
			which = append(which, k)
		}
		sort.Strings(which)
		fmt.Printf("mutant that %s: breaks %s\n", name, strings.Join(which, ", "))
		if len(which) == 0 {
			fmt.Println("  the eval is blind to it")
			why = append(why, "the mutant that "+name+" is not caught")
			failed = true
		}
		if m := mutants[name]; m.crashes {
			m.uncrashed = true
			if n := len(invariants(m, 8)); n > 0 {
				fmt.Printf("  it breaks %d worlds where no tick crashes, so it proves nothing about invariant 6\n", n)
				failed = true
			} else if len(which) > 0 {
				fmt.Println("  and nothing where no tick crashes: the eval sees invariant 6")
			}
		}
		if m := mutants[name]; m.waits {
			m.unwaited = true
			if n := len(invariants(m, 8)); n > 0 {
				fmt.Printf("  it breaks %d worlds where no build waits to begin, so it proves nothing about the order of a fair test\n", n)
				failed = true
			} else if len(which) > 0 {
				fmt.Println("  and nothing where no build waits to begin: the eval sees the order of a fair test")
			}
		}
	}

	for _, c := range cases() {
		if fails[c.Name] == 0 {
			fmt.Printf("case %s: no mutant fails it, so it proves nothing\n", c.Name)
			failed = true
		}
	}

	for _, c := range contracts() {
		kinds := map[string]bool{}
		for _, cfg := range []queue.Config{challenger["hey2"], challenger["hey-native-apps"]} {
			for _, w := range c.play(mutant{plan: queue.Plan, verify: queue.Verify}, cfg) {
				if i := strings.Index(w, "invariant "); i >= 0 {
					kinds[w[i:][:11]] = true
				}
			}
		}
		var which []string
		for k := range kinds {
			which = append(which, k)
		}
		sort.Strings(which)
		fmt.Printf("a CI where %s: breaks %s\n", strings.ReplaceAll(c.Name, "-", " "), strings.Join(which, ", "))
		if len(which) == 0 {
			fmt.Println("  the eval is blind to that line of the contract")
			why = append(why, "the broken contract "+c.Name+" is not caught")
			failed = true
		}
	}

	lines := engineLines()
	last := recorded()
	var scores []Score
	fmt.Printf("\n"+columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "label50", "label90", "slow20th", "eject90", "builds/pr", "per hour")
	for _, shape := range shapes {
		for _, cfg := range configs(shape) {
			if cfg.Name != "challenger" && !*baselines {
				continue
			}
			sc, br := measure(shape, cfg.Config)
			sc.Shape, sc.Config = shape.Name, cfg.Name
			if len(br) > 0 && cfg.Name == "challenger" {
				fmt.Printf("  %s broke %d invariants, the first: %s\n", cfg.Name, len(br), br[0])
				why = append(why, fmt.Sprintf("%d invariants broken, the first in %s", len(br), br[0]))
				failed = true
			}
			sc.print(shape)
			scores = append(scores, sc)
			if was, ok := last[sc.Shape+"\t"+sc.Config]; ok && cfg.Name == "challenger" && *record == "" {
				for _, w := range worse(was, sc) {
					fmt.Println("  worse than its best kept row: " + w)
					why = append(why, w)
					failed = true
				}
			}
		}
	}
	fmt.Printf("\nengine: %d lines of Go outside tests and the eval\n", lines)

	if *judge != "" {
		if *record = *judge + ": kept"; len(why) > 0 {
			*record = *judge + ": rejected, " + strings.Join(why, "; ")
		}
		fmt.Println(*record)
	}
	if *record != "" {
		var rows []string
		for _, sc := range scores {
			rows = append(rows, sc.row(lines, *record))
		}
		f, err := os.OpenFile(scoreboard, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if st, _ := f.Stat(); st.Size() == 0 {
			fmt.Fprintln(f, header)
		}
		fmt.Fprintln(f, strings.Join(rows, "\n"))
		f.Close()
		fmt.Println("recorded in " + scoreboard)
	}
	if failed {
		os.Exit(1)
	}
}

// replay prints every action the queue takes in one world, and each verdict as it arrives.
func replay(spec string) {
	var name string
	var slots, batch int
	var probes bool
	var seed int64
	fmt.Sscan(spec, &name, &slots, &batch, &probes, &seed)
	shape := storm
	for _, s := range shapes {
		if s.Name == name {
			shape = s
		}
	}
	if name == "calm" {
		shape.Flake, shape.ProbeFlake = 0, 0
	}
	if name == "tight" {
		shape = tight()
	}
	s := NewSim(shape, dials(slots, batch, probes), seed)
	s.Trace = os.Stdout
	s.Run()
	fmt.Printf("landed %d, wrong %d, ejected %v, broken %v\n", s.Landed, s.Wrong, s.Ejected, s.Broken)
}

// cold is the share of each shape's other builds that started on a cold agent, as its CI
// measured. hey2's is its pipeline's builds in the 13 hours from 2026-11-13T17:37Z, read on
// 2026-11-14: 38 of 124.
var cold = map[string]float64{"hey2": 0.31}

// fit plays each shape's pool with no queue, 37 hours at a time, and prints what it does
// beside what its CI measured.
func fit() {
	for _, shape := range shapes {
		if shape.Cold == 0 {
			fmt.Printf("%s: no pool fitted, and nothing measured\n", shape.Name)
			continue
		}
		quiet := Shape{Name: shape.Name, Build: shape.Build, Hours: 37, Agents: shape.Agents, Cold: shape.Cold, Others: shape.Others, Burst: shape.Burst, Hold: shape.Hold}
		var waits []float64
		for _, s := range each(seeds, func(i int) *Sim { s := NewSim(quiet, queue.Config{}, int64(i)); s.Run(); return s }) {
			waits = append(waits, s.pool.Waited...)
		}
		fmt.Printf("%s: %d warm agents, %.0f other builds an hour in bursts of %.1f, %s each, over %d worlds of 37 hours\n",
			shape.Name, shape.Agents, shape.Others, shape.Burst, shape.Hold, seeds)
		fmt.Printf("  share of them that start on a cold agent: %.2f simulated, %.2f measured\n", waited(waits).Share, cold[shape.Name])
	}
}

// worse lists the indicators that are worse than they were by more than a twentieth. Wrong
// ejections are a count of rare events, so they are worse only past twice the count's own
// scatter, and never for fewer than three more.
func worse(was, now Score) []string {
	var out []string
	if n := float64(wide * was.Landed); n > 0 && now.Wrong > was.Wrong+1000*max(3, 2*math.Sqrt(was.Wrong*n/1000))/n {
		out = append(out, fmt.Sprintf("%s %s, wrong ejections per 1,000: %.2f, was %.2f", now.Shape, now.Config, now.Wrong, was.Wrong))
	}
	check := func(name string, before, after float64, lowerIsBetter bool) {
		slack := 0.05*before + 0.01
		if (lowerIsBetter && after > before+slack) || (!lowerIsBetter && after < before-slack) {
			out = append(out, fmt.Sprintf("%s %s, %s: %.2f, was %.2f", now.Shape, now.Config, name, after, before))
		}
	}
	check("wait p50", was.WaitP50, now.WaitP50, true)
	check("wait p90", was.WaitP90, now.WaitP90, true)
	if was.LabelP50 > 0 {
		check("labelled p50", was.LabelP50, now.LabelP50, true)
	}
	if was.LabelP90 > 0 {
		check("labelled p90", was.LabelP90, now.LabelP90, true)
	}
	if was.LabelSlow > 0 {
		check("labelled slowest 20th", was.LabelSlow, now.LabelSlow, true)
	}
	check("ejected p90", was.EjectP90, now.EjectP90, true)
	check("builds per landing", was.Builds, now.Builds, true)
	check("landings an hour", was.PerHour, now.PerHour, false)
	return out
}

// recorded is what a run is held to, read from the scoreboard.
func recorded() map[string]Score {
	data, err := os.ReadFile(scoreboard)
	if err != nil {
		return map[string]Score{}
	}
	return best(strings.Split(string(data), "\n")[1:])
}

// best is, for each shape and config, the best each indicator has been on a kept row since
// the last fresh baselines, whose notes begin with "baseline". A run is compared with that
// and not with the newest row, so small losses do not add up over kept steps. A rejected
// experiment's row says so in its note, and nothing is compared with it.
func best(rows []string) map[string]Score {
	low := func(a, b float64) float64 {
		if a == 0 {
			return b
		}
		if b == 0 {
			return a
		}
		return min(a, b)
	}
	out := map[string]Score{}
	for _, line := range rows {
		f := strings.Split(line, "\t")
		if len(f) < 13 || strings.Contains(f[12], "rejected") {
			continue
		}
		sc := Score{Shape: f[2], Config: f[3]}
		fmt.Sscan(f[4], &sc.Landed)
		fmt.Sscan(f[5]+" "+f[6]+" "+f[7]+" "+f[8]+" "+f[9]+" "+f[10], &sc.Wrong, &sc.WaitP50, &sc.WaitP90, &sc.EjectP90, &sc.Builds, &sc.PerHour)
		if len(f) >= 16 {
			fmt.Sscan(f[13]+" "+f[14]+" "+f[15], &sc.LabelP50, &sc.LabelP90, &sc.LabelSlow)
		}
		key := sc.Shape + "\t" + sc.Config
		if was, ok := out[key]; ok && !strings.HasPrefix(f[12], "baseline") {
			sc.Wrong, sc.WaitP50, sc.WaitP90 = min(sc.Wrong, was.Wrong), min(sc.WaitP50, was.WaitP50), min(sc.WaitP90, was.WaitP90)
			sc.LabelP50, sc.LabelP90, sc.LabelSlow = low(sc.LabelP50, was.LabelP50), low(sc.LabelP90, was.LabelP90), low(sc.LabelSlow, was.LabelSlow)
			sc.EjectP90, sc.Builds, sc.PerHour = min(sc.EjectP90, was.EjectP90), min(sc.Builds, was.Builds), max(sc.PerHour, was.PerHour)
		}
		out[key] = sc
	}
	return out
}

// row is the score's line on the scoreboard.
func (sc Score) row(lines int, note string) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\t%d\t%s\t%.2f\t%.2f\t%.2f",
		time.Now().UTC().Format("2006-01-02T15:04Z"), commit(), sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.EjectP90, sc.Builds, sc.PerHour, lines, note, sc.LabelP50, sc.LabelP90, sc.LabelSlow)
}

func commit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "none"
	}
	sha := strings.TrimSpace(string(out))
	if dirty, _ := exec.Command("git", "status", "--porcelain", "--", "internal", "cmd", "eval/sim.go", "eval/main.go").Output(); len(dirty) > 0 {
		sha += "+"
	}
	return sha
}

// engineLines counts the Go the queue ships: everything outside tests, the tests' helpers in
// internal/gittest, and the eval.
func engineLines() int {
	n := 0
	for _, dir := range []string{"internal", "cmd"} {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") && filepath.Dir(path) != filepath.Join("internal", "gittest") {
				data, _ := os.ReadFile(path)
				n += strings.Count(string(data), "\n")
			}
			return nil
		})
	}
	return n
}
