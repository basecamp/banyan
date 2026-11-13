package main

import (
	"encoding/json"
	"fmt"
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
func live(dir string) (Score, error) {
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(names)
	var recs []queue.Recording
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return Score{}, err
		}
		var rec queue.Recording
		if err := json.Unmarshal(data, &rec); err != nil {
			return Score{}, fmt.Errorf("%s: %w", name, err)
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return Score{}, fmt.Errorf("%s holds no recorded ticks", dir)
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
		return Score{}, fmt.Errorf("%s holds no build with a verdict", dir)
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
	// A tree that a whole build passed is good, so every other verdict on it is CI's own failure.
	for _, vs := range verdicts {
		if slices.Contains(vs, queue.Success) {
			sc.Good += len(vs)
			for _, v := range vs {
				if v != queue.Success {
					sc.Flaked++
				}
			}
		}
	}
	fmt.Printf("of %d whole builds of trees that passed, %d did not pass\n", sc.Good, sc.Flaked)
	return sc, nil
}
