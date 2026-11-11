// The eval: go run ./eval
//
// It plays the planner against simulated worlds whose hidden facts decide every verdict,
// holds it to the charter's invariants, measures the charter's indicators, and compares them
// with the last recorded run. It exits 1 on a broken invariant, on an eval that a broken
// planner passes, and on an indicator that got worse.
//
//	go run ./eval                    check against the scoreboard
//	go run ./eval -record "<note>"   append this run's rows to the scoreboard
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// The shapes the indicators are measured on. Each rate is an estimate until `banyan report`
// replaces it with a measured one.
var shapes = []Shape{
	{Name: "hey2", Build: 25 * time.Minute, Flake: 0.03, Break: 0.03, Clash: 0.02, Conflict: 0.05, Ahead: 0.02,
		Lost: 0.005, Errored: 0.01, Backlog: 15, Arrivals: 5, Hours: 10, Moves: 0.3, Removals: 0.05},
	{Name: "hey-native-apps", Build: 30 * time.Minute, Probe: 10 * time.Minute, Flake: 0.07, ProbeFlake: 0.02,
		Break: 0.015, Clash: 0.005, Conflict: 0.08, Ahead: 0.03, Lost: 0.005, Errored: 0.01,
		Backlog: 20, Arrivals: 16, Hours: 10, Moves: 1, Removals: 0.1},
}

// storm is the world the invariants are held in: everything that can go wrong, often.
var storm = Shape{Name: "storm", Build: 25 * time.Minute, Probe: 8 * time.Minute, Flake: 0.05, ProbeFlake: 0.03,
	Break: 0.05, Clash: 0.05, Conflict: 0.05, Ahead: 0.05, Lost: 0.03, Errored: 0.05,
	Backlog: 8, Arrivals: 6, Hours: 6, Moves: 1, Removals: 0.5, Outside: 0.4, Crash: 0.15, Race: 0.3,
	Episodes: []Episode{{At: time.Hour, For: 70 * time.Minute}, {At: 4 * time.Hour, NeedsFix: true, Fix: 20 * time.Minute}}}

type named struct {
	Name string
	queue.Config
}

func dials(slots, batch int, probes bool) queue.Config {
	return queue.Config{Slots: slots, MaxBatch: batch, Probes: probes, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}
}

// challenger is the configuration each shape would ship with. The hillclimb moves these.
var challenger = map[string]queue.Config{
	"hey2":            dials(6, 1, false),
	"hey-native-apps": dials(4, 12, true),
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
	Wrong, WaitP50, WaitP90, BlameP90, Builds float64
	PerHour                                   float64
}

const seeds = 200

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
	var waits, blames []float64
	var broken []string
	var wrong, whole, probes int
	for i, s := range each(seeds, func(i int) *Sim { s := NewSim(shape, cfg, int64(i)); s.Run(); return s }) {
		sc.Landed += s.Landed
		wrong, whole, probes = wrong+s.Wrong, whole+s.Whole, probes+s.Probes
		waits, blames = append(waits, s.Waits...), append(blames, s.Blames...)
		for _, b := range s.Broken {
			broken = append(broken, fmt.Sprintf("%s seed %d: %s", shape.Name, i+1, b))
		}
	}
	if sc.Landed > 0 {
		cost := float64(whole)
		if shape.Probe > 0 {
			cost += float64(probes) * float64(shape.Probe) / float64(shape.Build)
		}
		sc.Wrong, sc.Builds = 1000*float64(wrong)/float64(sc.Landed), cost/float64(sc.Landed)
	}
	sc.WaitP50, sc.WaitP90, sc.BlameP90 = percentile(waits, 50), percentile(waits, 90), percentile(blames, 90)

	// Throughput is measured on a backlog alone: forty queued, nothing arriving.
	backlog := shape
	backlog.Backlog, backlog.Arrivals, backlog.Hours = 40, 0, 0
	var landed int
	var hours float64
	for _, s := range each(seeds/4, func(i int) *Sim { s := NewSim(backlog, cfg, int64(i)); s.Run(); return s }) {
		landed, hours = landed+s.Landed, hours+s.LastLand.Hours()
	}
	if hours > 0 {
		sc.PerHour = float64(landed) / hours
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

// invariants holds the planner to the charter in the storm, with and without flakes, under
// each kind of configuration, and in every small world.
func invariants(m mutant, worlds int) []string {
	var broken []string
	calm := storm
	calm.Name, calm.Flake, calm.ProbeFlake = "storm without flakes", 0, 0
	for _, shape := range []Shape{storm, calm} {
		for _, cfg := range []queue.Config{dials(6, 1, false), dials(1, 1, false), dials(4, 12, true), dials(2, 24, false), dials(3, 6, true)} {
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
						shape := Shape{Name: "small", Build: 20 * time.Minute, Probe: 6 * time.Minute, Backlog: 3, Script: make([]bool, 6)}
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
	"blames at the first red": {verify: queue.Verify,
		plan: func(w queue.World, c queue.Config) []queue.Action {
			for _, b := range w.Builds {
				if b.Kind == queue.Candidate && b.Verdict == queue.Failure && len(b.Chain) > 0 {
					return []queue.Action{{Do: "eject", PR: b.Chain[len(b.Chain)-1].Number, Cause: "culprit"}}
				}
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
const header = "when\tcommit\tshape\tconfig\tlanded\twrong_per_1000\twait_p50\twait_p90\tblame_p90\tbuilds_per_landing\tlandings_per_hour\tengine_lines\tnote"

func main() {
	record := flag.String("record", "", "append this run to the scoreboard, with this note")
	baselines := flag.Bool("baselines", false, "measure the baselines too, as -record does")
	trace := flag.String("trace", "", `replay one world and print what the queue did: "<shape> <slots> <batch> <probes> <seed>"`)
	flag.Parse()
	if *trace != "" {
		replay(*trace)
		return
	}
	failed := false

	broken := invariants(mutant{queue.Plan, queue.Verify}, seeds/2)
	fmt.Printf("invariants: %d broken\n", len(broken))
	for i, b := range broken {
		if i == 12 {
			fmt.Printf("  ... and %d more\n", len(broken)-i)
			break
		}
		fmt.Println("  " + b)
	}
	failed = failed || len(broken) > 0

	var names []string
	for name := range mutants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
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
			failed = true
		}
	}

	lines := engineLines()
	last := recorded()
	var rows []string
	fmt.Printf("\n%-16s %-13s %7s %9s %8s %8s %9s %10s %8s\n", "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "blame90", "builds/pr", "per hour")
	for _, shape := range shapes {
		for _, cfg := range configs(shape) {
			if cfg.Name != "challenger" && !*baselines && *record == "" {
				continue
			}
			sc, br := measure(shape, cfg.Config)
			sc.Shape, sc.Config = shape.Name, cfg.Name
			if len(br) > 0 && cfg.Name == "challenger" {
				fmt.Printf("  %s broke %d invariants, the first: %s\n", cfg.Name, len(br), br[0])
				failed = true
			}
			fmt.Printf("%-16s %-13s %7d %9.2f %8.2f %8.2f %9.2f %10.2f %8.1f\n", sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.BlameP90, sc.Builds, sc.PerHour)
			rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.1f\t%d\t%s",
				time.Now().UTC().Format("2006-01-02T15:04Z"), commit(), sc.Shape, sc.Config, sc.Landed, sc.Wrong, sc.WaitP50, sc.WaitP90, sc.BlameP90, sc.Builds, sc.PerHour, lines, *record))
			if was, ok := last[sc.Shape+"\t"+sc.Config]; ok && cfg.Name == "challenger" && *record == "" {
				for _, w := range worse(was, sc) {
					fmt.Println("  worse than recorded: " + w)
					failed = true
				}
			}
		}
	}
	fmt.Printf("\nengine: %d lines of Go outside tests and the eval\n", lines)

	if *record != "" {
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
	s := NewSim(shape, dials(slots, batch, probes), seed)
	s.Trace = os.Stdout
	s.Run()
	fmt.Printf("landed %d, wrong %d, ejected %v, broken %v\n", s.Landed, s.Wrong, s.Ejected, s.Broken)
}

// worse lists the indicators that moved the wrong way by more than a twentieth.
func worse(was, now Score) []string {
	var out []string
	check := func(name string, before, after float64, lowerIsBetter bool) {
		slack := 0.05*before + 0.01
		if (lowerIsBetter && after > before+slack) || (!lowerIsBetter && after < before-slack) {
			out = append(out, fmt.Sprintf("%s %s, %s: %.2f, was %.2f", now.Shape, now.Config, name, after, before))
		}
	}
	check("wrong ejections per 1,000", was.Wrong, now.Wrong, true)
	check("wait p50", was.WaitP50, now.WaitP50, true)
	check("wait p90", was.WaitP90, now.WaitP90, true)
	check("blame p90", was.BlameP90, now.BlameP90, true)
	check("builds per landing", was.Builds, now.Builds, true)
	check("landings an hour", was.PerHour, now.PerHour, false)
	return out
}

// recorded is the newest row of each shape and config on the scoreboard.
func recorded() map[string]Score {
	out := map[string]Score{}
	data, err := os.ReadFile(scoreboard)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Split(line, "\t")
		if len(f) < 12 {
			continue
		}
		sc := Score{Shape: f[2], Config: f[3]}
		fmt.Sscan(f[5]+" "+f[6]+" "+f[7]+" "+f[8]+" "+f[9]+" "+f[10], &sc.Wrong, &sc.WaitP50, &sc.WaitP90, &sc.BlameP90, &sc.Builds, &sc.PerHour)
		out[sc.Shape+"\t"+sc.Config] = sc
	}
	return out
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

// engineLines counts the Go the queue ships: everything outside tests and the eval.
func engineLines() int {
	n := 0
	for _, dir := range []string{"internal", "cmd"} {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				data, _ := os.ReadFile(path)
				n += strings.Count(string(data), "\n")
			}
			return nil
		})
	}
	return n
}
