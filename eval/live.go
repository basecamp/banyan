package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// live scores a real queue from the ticks it recorded, by the definitions the simulator is
// scored by, so a row from a repository sits beside its simulated row.
//
// A recording holds what a tick read and what it planned, and nothing of what CI did between
// two ticks. So a build's time is taken from the tick that pushed it to the first tick that
// read its verdict, and every time here is as coarse as the ticks are far apart.
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

	type head struct {
		Number int
		Head   queue.SHA
	}
	var (
		pushed   = map[queue.SHA]queue.Build{}
		answered = map[queue.SHA]time.Time{}
		firstRed = map[head]time.Time{}
		ejected  = map[head]bool{}
		trunks   = map[queue.SHA]bool{}
		took     = map[queue.Kind][]float64{}
		waits    []time.Duration
		blames   []time.Duration
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
		queued := map[int]time.Time{}
		for _, pr := range rec.World.PRs {
			queued[pr.Number] = pr.Queued
		}
		for _, b := range rec.World.Builds {
			if _, seen := answered[b.SHA]; b.Verdict == queue.Pending || seen {
				continue
			}
			answered[b.SHA] = now
			took[b.Kind] = append(took[b.Kind], now.Sub(b.Pushed).Minutes())
			for _, m := range b.Chain {
				if h := (head{m.Number, m.Head}); b.Verdict == queue.Failure && firstRed[h].IsZero() {
					firstRed[h] = now
				}
			}
		}
		for _, a := range rec.Actions {
			switch {
			case a.Do == "push":
				pushed[a.Build.SHA] = a.Build
			case a.Do == "eject" && a.Cause == "culprit":
				for _, pr := range rec.World.PRs {
					if h := (head{pr.Number, pr.Head}); pr.Number == a.PR && !firstRed[h].IsZero() {
						ejected[h] = true
						blames = append(blames, now.Sub(firstRed[h]))
					}
				}
			case a.Do == "land" && trunks[a.SHA]:
				// The plan to land is not the landing: it counts once a later tick read the
				// trunk there.
				for _, m := range a.Members {
					landed++
					waits = append(waits, now.Sub(queued[m.Number]))
					if ejected[head{m.Number, m.Head}] {
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
		WaitP50: in(waits, 50), WaitP90: in(waits, 90), BlameP90: in(blames, 90)}
	if landed > 0 {
		sc.Wrong = 1000 * float64(wrong) / float64(landed)
		sc.Builds = builds / float64(landed)
	}
	if busy > 0 {
		sc.PerHour = float64(landed) / busy.Hours()
	}
	fmt.Printf("%d ticks over %s with something queued; a whole build took %.1f min at the median, as the ticks saw it\n",
		len(recs), busy.Round(time.Minute), whole)
	return sc, nil
}
