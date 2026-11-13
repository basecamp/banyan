package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// live scores a real queue from the ticks it recorded, by the definitions the simulator is
// scored by, so a row from a repository sits beside its simulated row.
//
// A recording holds what a tick read and what it planned, and nothing of what CI did between
// two ticks. So a build's time is taken from the tick that pushed it to the first tick that
// read its verdict, a pull request whose checks passed after it was labelled is ready from the
// first tick that read them passed, and every time here is as coarse as the ticks are far apart.
func live(dir string) (Score, window, error) {
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(names)
	var recs []queue.Recording
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return Score{}, window{}, err
		}
		var rec queue.Recording
		if err := json.Unmarshal(data, &rec); err != nil {
			return Score{}, window{}, fmt.Errorf("%s: %w", name, err)
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return Score{}, window{}, fmt.Errorf("%s holds no recorded ticks", dir)
	}

	// A pull request's clock belongs to one queueing of one head: a new head or a new label
	// starts it again.
	type head struct {
		Number int
		Head   queue.SHA
		Queued time.Time
	}
	var (
		pushed   = map[queue.SHA]queue.Build{}
		answered = map[queue.SHA]time.Time{}
		ready    = map[head]time.Time{}
		seen     = map[head]bool{}
		ejected  = map[[2]any]bool{} // a number and a head ejected as the cause, however often queued
		trunks   = map[queue.SHA]bool{}
		took     = map[queue.Kind][]float64{}
		verdicts = map[queue.SHA][]queue.Verdict{} // each tree's whole builds, by the prefix they test
		good     = map[queue.SHA]bool{}            // the trees a whole build passed, each prefix of its chain among them
		waits    []time.Duration
		ejects   []time.Duration
		busy     time.Duration
		landed   int
		wrong    int
	)
	for _, rec := range recs {
		trunks[rec.World.Trunk] = true
	}
	for i, rec := range recs {
		now := rec.World.Now
		if i > 0 && len(recs[i-1].World.PRs) > 0 {
			busy += now.Sub(recs[i-1].World.Now)
		}
		at := map[int]head{}
		for _, pr := range rec.World.PRs {
			h := head{pr.Number, pr.Head, pr.Queued}
			if at[pr.Number] = h; ready[h].IsZero() && pr.Wait == "" && pr.Checks == queue.Success {
				if ready[h] = pr.Queued; seen[h] {
					ready[h] = now
				}
			}
			seen[h] = true
		}
		for _, b := range rec.World.Builds {
			if _, seen := answered[b.SHA]; b.Verdict == queue.Pending || seen {
				continue
			}
			answered[b.SHA] = now
			took[b.Kind] = append(took[b.Kind], now.Sub(b.Pushed).Minutes())
			if b.Kind != queue.Probe {
				verdicts[b.Tests()] = append(verdicts[b.Tests()], b.Verdict)
				for _, m := range b.Chain {
					good[m.Merge] = good[m.Merge] || b.Verdict == queue.Success
				}
				good[b.Tests()] = good[b.Tests()] || b.Verdict == queue.Success
			}
		}
		for _, a := range rec.Actions {
			switch {
			case a.Do == "push":
				pushed[a.Build.SHA] = a.Build
			case a.Do == "eject" && a.Cause == "culprit":
				if h := at[a.PR]; !ready[h].IsZero() {
					ejected[[2]any{h.Number, h.Head}] = true
					ejects = append(ejects, now.Sub(ready[h]))
				}
			case a.Do == "land" && trunks[a.SHA]:
				// The plan to land is not the landing: it counts once a later tick read the
				// trunk there, and once however often a refused push had it planned again.
				trunks[a.SHA] = false
				for _, m := range a.Members {
					landed++
					h := at[m.Number]
					if ready[h].IsZero() { // no recorded tick read it ready
						ready[h] = h.Queued
					}
					waits = append(waits, now.Sub(ready[h]))
					if ejected[[2]any{m.Number, m.Head}] {
						wrong++
					}
				}
			}
		}
	}

	whole := percentile(append(took[queue.Candidate], took[queue.Check]...), 50)
	if whole == 0 {
		return Score{}, window{}, fmt.Errorf("%s holds no build with a verdict", dir)
	}
	builds := 0.0
	for _, b := range pushed {
		if b.Kind == queue.Probe {
			builds += percentile(took[queue.Probe], 50) / whole
		} else {
			builds++
		}
	}
	in := func(ds []time.Duration, p int) float64 {
		var xs []float64
		for _, d := range ds {
			xs = append(xs, d.Minutes()/whole)
		}
		return percentile(xs, p)
	}
	sc := Score{Shape: filepath.Base(dir), Config: "live", Landed: landed,
		WaitP50: in(waits, 50), WaitP90: in(waits, 90), EjectP90: in(ejects, 90)}
	if landed > 0 {
		sc.Wrong = 1000 * float64(wrong) / float64(landed)
		sc.Builds = builds / float64(landed)
	}
	if busy > 0 {
		sc.PerHour = float64(landed) / busy.Hours()
	}
	fmt.Printf("%d ticks over %s with something queued; a whole build took %.1f min at the median, as the ticks saw it\n",
		len(recs), busy.Round(time.Minute), whole)
	fmt.Printf("ready to landed: %.0f min at the median, %.0f at p90\n", sc.WaitP50*whole, sc.WaitP90*whole)
	// A tree that a whole build passed is good, alone or with more merged onto it, so every
	// other verdict on it is CI's own failure.
	for tree, vs := range verdicts {
		if good[tree] {
			sc.Good += len(vs)
			for _, v := range vs {
				if v != queue.Success {
					sc.Flaked++
				}
			}
		}
	}
	fmt.Printf("of %d whole builds of trees that passed, alone or under more, %d did not pass\n", sc.Good, sc.Flaked)
	win := window{build: time.Duration(whole * float64(time.Minute)), cfg: recs[len(recs)-1].Config}
	for _, at := range ready {
		win.arrivals = append(win.arrivals, max(at.Sub(recs[0].World.Now), 0))
	}
	slices.Sort(win.arrivals)
	return sc, win, nil
}

// window is what a simulation needs of the ticks a real queue recorded, to play the same
// hours again: when each pull request was ready, how long a whole build took, and the dials
// of the last tick.
type window struct {
	arrivals []time.Duration
	build    time.Duration
	cfg      queue.Config
}

// simulated is the row a shape's CI gives the window's own arrivals, with builds as long and
// as flaky as the real queue's were. Each world draws its own flakes and its own breaking
// pull requests, at the shape's rate.
func (w window) simulated(shape Shape, was Score) Score {
	shape.Backlog, shape.Arrivals, shape.Build, shape.Hours = 0, 0, w.build, 0
	if was.Good > 0 {
		shape.Flake = float64(was.Flaked) / float64(was.Good)
	}
	var sc Score
	var waits, ejects []float64
	whole, wrong, hours := 0, 0, 0.0
	for _, s := range each(seeds, func(i int) *Sim {
		world, rng := shape, rand.New(rand.NewSource(int64(i)))
		for n, at := range w.arrivals {
			world.Queue = append(slices.Clone(world.Queue), Queued{Number: n + 1, At: at, Breaks: rng.Float64() < shape.Break})
			world.Hours = max(world.Hours, at.Hours())
		}
		s := NewSim(world, w.cfg, int64(i))
		s.Run()
		return s
	}) {
		sc.Landed, whole, wrong, hours = sc.Landed+s.Landed, whole+s.Whole, wrong+s.Wrong, hours+s.LastLand.Hours()
		waits, ejects = append(waits, s.Waits...), append(ejects, s.Ejects...)
	}
	if sc.Landed > 0 {
		sc.Wrong, sc.Builds, sc.PerHour = 1000*float64(wrong)/float64(sc.Landed), float64(whole)/float64(sc.Landed), float64(sc.Landed)/hours
	}
	sc.Shape, sc.Config = was.Shape, "simulated"
	sc.WaitP50, sc.WaitP90, sc.EjectP90 = percentile(waits, 50), percentile(waits, 90), percentile(ejects, 90)
	sc.Landed /= seeds
	return sc
}

// planAgain plans one recorded tick again from the recording alone, which holds everything the
// tick read, and prints the actions.
func planAgain(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rec queue.Recording
	if err := json.Unmarshal(data, &rec); err != nil {
		return err
	}
	for _, a := range rec.Replay() {
		line, _ := json.Marshal(a)
		fmt.Println(string(line))
	}
	return nil
}
