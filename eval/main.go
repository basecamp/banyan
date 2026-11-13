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
// hey2 is fitted to what it measured. Its pool is its pipeline's, as `go run ./eval -fit`
// shows, and it cancels a build whose branch is deleted. Its build time and its flakes are its
// queue's own builds from 2026-11-12 to 2026-11-13: a whole build that passed took 34 minutes,
// 29 at its tenth fastest in a hundred and 38 at its tenth slowest. Of its first 45 builds of
// trees that also passed, 8 failed: 5 on an agent that was lost, which its pipeline now runs
// again, and 3 on a step that dropped its connection, since fixed. None of the 15 after both
// failed, so its flake rate is taken as 1 in 20 until more builds say.
// Nothing reaches its trunk but through the queue. hey-native-apps is moving to a CI nobody has
// measured yet, so its pool has no limit and is taken to cancel, and its rates are estimates.
var shapes = []Shape{
	{Name: "hey2", Build: 34 * time.Minute, Spread: 0.15, Flake: 0.05, Break: 0.03, Clash: 0.02, Conflict: 0.05, Ahead: 0.02,
		Lost: 0.005, Errored: 0.01, Backlog: 15, Arrivals: 5, Hours: 10, Moves: 0.3, Removals: 0.05,
		Agents: 13, Others: 21, Burst: 2.5, Hold: 18 * time.Minute, Cancels: true},
	{Name: "hey-native-apps", Build: 30 * time.Minute, Probe: 10 * time.Minute, Flake: 0.07, ProbeFlake: 0.02,
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
	Shape, Config                             string
	Landed                                    int
	Wrong, WaitP50, WaitP90, EjectP90, Builds float64
	PerHour                                   float64
	// What the queue costs everyone else: how long the other builds wait for an agent, in
	// minutes, beside the queue and with no queue. It is printed, and not on the scoreboard.
	Beside, Alone agentWait
	// CI's own failures, which only a live score counts: the whole builds of trees that some
	// whole build passed, and those of them that did not pass.
	Good, Flaked int
}

type agentWait struct{ Mean, P90 float64 }

func waited(minutes []float64) agentWait {
	var sum float64
	for _, m := range minutes {
		sum += m
	}
	return agentWait{sum / float64(max(len(minutes), 1)), percentile(minutes, 90)}
}

const seeds = 200

// wide is how many times more worlds wrong ejections are counted over than the rest. They are
// a few in ten thousand landings: over one set of worlds the same planner counts 5 in one
// thousand worlds and 15 in the next.
const wide = 5

const columns = "%-16s %-13s %7s %9s %8s %8s %9s %10s %8s\n"

// print is the score's line in a run's output, and under it what the queue costs the other
// builds, where the shape's pool has a limit.
func (sc Score) print(shape Shape) {
	fmt.Printf("%-16s %-13s %7d %9.2f %8.2f %8.2f %9.2f %10.2f %8.1f\n", sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.EjectP90, sc.Builds, sc.PerHour)
	if shape.Agents > 0 {
		fmt.Printf("  the other builds wait %.1f min longer for an agent: %.1f at the mean and %.1f at p90, against %.1f and %.1f with no queue\n",
			sc.Beside.Mean-sc.Alone.Mean, sc.Beside.Mean, sc.Beside.P90, sc.Alone.Mean, sc.Alone.P90)
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
	var waits, ejects []float64
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
		waits, ejects = append(waits, s.Waits...), append(ejects, s.Ejects...)
	}
	if sc.Landed > 0 {
		cost := float64(whole)
		if shape.Probe > 0 {
			cost += float64(probes) * float64(shape.Probe) / float64(shape.Build)
		}
		sc.Wrong, sc.Builds = 1000*float64(wrong)/float64(landed), cost/float64(sc.Landed)
	}
	sc.WaitP50, sc.WaitP90, sc.EjectP90 = percentile(waits, 50), percentile(waits, 90), percentile(ejects, 90)
	sc.Beside, sc.Alone = waited(beside), waited(alone)

	// Throughput is measured on a backlog alone: forty queued, nothing arriving. A world is
	// short and its last landing is one draw, so it takes many of them to hold still.
	backlog := shape
	backlog.Backlog, backlog.Arrivals, backlog.Hours = 40, 0, 0
	var drained int
	var hours float64
	for _, s := range each(2*seeds, func(i int) *Sim { s := NewSim(backlog, cfg, int64(i)); s.Run(); return s }) {
		drained, hours = drained+s.Landed, hours+s.LastLand.Hours()
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
const header = "when\tcommit\tshape\tconfig\tlanded\twrong_per_1000\twait_p50\twait_p90\teject_p90\tbuilds_per_landing\tlandings_per_hour\tengine_lines\tnote"

func main() {
	record := flag.String("record", "", "append this run to the scoreboard, with this note")
	judge := flag.String("judge", "", "record this run as the test of this hypothesis, with the verdict the rule gives it: kept, or rejected and why")
	baselines := flag.Bool("baselines", false, "measure the baselines too, as -record does")
	trace := flag.String("trace", "", `replay one world and print what the queue did: "<shape> <slots> <batch> <probes> <seed>"`)
	try := flag.String("try", "", `measure shapes under other dials, and record nothing: "<shape> <slots> <batch> <probes> [<flake rate> [<build minutes> [<share of a build a broken tree takes to fail>]]]; ..."`)
	fitted := flag.Bool("fit", false, "play each shape's pool with no queue, and print it beside what its pipeline measured")
	recordings := flag.String("live", "", "score a real queue from the folder its ticks recorded into, and record nothing")
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
		sc, win, err := live(*recordings)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf(columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "eject90", "builds/pr", "per hour")
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
		fmt.Printf(columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "eject90", "builds/pr", "per hour")
		for _, spec := range strings.Split(*try, ";") {
			var name string
			var slots, batch int
			var probes bool
			flake, minutes, fails := -1.0, 0.0, 0.0
			fmt.Sscan(spec, &name, &slots, &batch, &probes, &flake, &minutes, &fails)
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

	lines := engineLines()
	last := recorded()
	var scores []Score
	fmt.Printf("\n"+columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "eject90", "builds/pr", "per hour")
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

// pipelines is what each shape's CI measured with no queue of ours running. hey2's is its
// pipeline's newest 800 builds, 37 hours of them, read on 2026-11-11.
var pipelines = map[string]pipeline{
	"hey2": {Running: 6, AtMost10: 0.72, Full: 0.147, Wait: [3]float64{0.3, 6.8, 11.1}, Starts: [2]float64{20, 39}},
}

// pipeline is a pool with no queue: the builds running at once at the median, the share of
// the time at ten or fewer and with no agent free, a build's wait for an agent in minutes at
// p50, p90 and p99, and the builds that start in an hour at p50 and p90.
type pipeline struct {
	Running, AtMost10, Full float64
	Wait                    [3]float64
	Starts                  [2]float64
}

// fit plays each shape's pool with no queue, 37 hours at a time, and prints what it does
// beside what the pipeline measured.
func fit() {
	for _, shape := range shapes {
		if shape.Agents == 0 {
			fmt.Printf("%s: no limit on its agents, and nothing measured\n", shape.Name)
			continue
		}
		quiet := Shape{Name: shape.Name, Build: shape.Build, Hours: 37, Agents: shape.Agents, Others: shape.Others, Burst: shape.Burst, Hold: shape.Hold}
		var busy, waits, starts []float64
		var sim pipeline
		for _, s := range each(seeds, func(i int) *Sim { s := NewSim(quiet, queue.Config{}, int64(i)); s.Run(); return s }) {
			for _, n := range s.pool.Busy {
				busy = append(busy, float64(n))
				if n <= 10 {
					sim.AtMost10++
				}
				if n >= shape.Agents {
					sim.Full++
				}
			}
			for _, n := range s.pool.Came[:min(37, len(s.pool.Came))] {
				starts = append(starts, float64(n))
			}
			waits = append(waits, s.pool.Waited...)
		}
		sim.Running, sim.AtMost10, sim.Full = percentile(busy, 50), sim.AtMost10/float64(len(busy)), sim.Full/float64(len(busy))
		sim.Wait = [3]float64{percentile(waits, 50), percentile(waits, 90), percentile(waits, 99)}
		sim.Starts = [2]float64{percentile(starts, 50), percentile(starts, 90)}
		was := pipelines[shape.Name]
		fmt.Printf("%s: %d agents, %.0f other builds an hour in bursts of %.1f, %s each, over %d worlds of 37 hours\n",
			shape.Name, shape.Agents, shape.Others, shape.Burst, shape.Hold, seeds)
		fmt.Printf("  %-42s %9s %9s\n", "", "simulated", "measured")
		for _, row := range []struct {
			name      string
			sim, real float64
		}{
			{"builds running at once, median", sim.Running, was.Running},
			{"share of the time at 10 or fewer", sim.AtMost10, was.AtMost10},
			{"share of the time with no agent free", sim.Full, was.Full},
			{"wait for an agent, p50, minutes", sim.Wait[0], was.Wait[0]},
			{"wait for an agent, p90, minutes", sim.Wait[1], was.Wait[1]},
			{"wait for an agent, p99, minutes", sim.Wait[2], was.Wait[2]},
			{"builds started in an hour, p50", sim.Starts[0], was.Starts[0]},
			{"builds started in an hour, p90", sim.Starts[1], was.Starts[1]},
		} {
			fmt.Printf("  %-42s %9.2f %9.2f\n", row.name, row.sim, row.real)
		}
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
	out := map[string]Score{}
	for _, line := range rows {
		f := strings.Split(line, "\t")
		if len(f) < 13 || strings.Contains(f[12], "rejected") {
			continue
		}
		sc := Score{Shape: f[2], Config: f[3]}
		fmt.Sscan(f[4], &sc.Landed)
		fmt.Sscan(f[5]+" "+f[6]+" "+f[7]+" "+f[8]+" "+f[9]+" "+f[10], &sc.Wrong, &sc.WaitP50, &sc.WaitP90, &sc.EjectP90, &sc.Builds, &sc.PerHour)
		key := sc.Shape + "\t" + sc.Config
		if was, ok := out[key]; ok && !strings.HasPrefix(f[12], "baseline") {
			sc.Wrong, sc.WaitP50, sc.WaitP90 = min(sc.Wrong, was.Wrong), min(sc.WaitP50, was.WaitP50), min(sc.WaitP90, was.WaitP90)
			sc.EjectP90, sc.Builds, sc.PerHour = min(sc.EjectP90, was.EjectP90), min(sc.Builds, was.Builds), max(sc.PerHour, was.PerHour)
		}
		out[key] = sc
	}
	return out
}

// row is the score's line on the scoreboard.
func (sc Score) row(lines int, note string) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\t%d\t%s",
		time.Now().UTC().Format("2006-01-02T15:04Z"), commit(), sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.EjectP90, sc.Builds, sc.PerHour, lines, note)
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
