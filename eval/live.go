package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
func live(dir, clone string) (Score, window, error) {
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
	if clone != "" {
		if _, err := gitRead(clone, nil, "rev-parse", "--git-dir"); err != nil {
			return Score{}, window{}, fmt.Errorf("clone %s: %w", clone, err)
		}
	}

	// Ready to landed belongs to one queueing of one head: a new head or a new label starts it
	// again.
	type head struct {
		Number int
		Head   queue.SHA
		Queued time.Time
	}
	var (
		pushed       = map[queue.SHA]queue.Build{}
		answered     = map[queue.SHA]time.Time{}
		ready        = map[head]time.Time{}
		seen         = map[head]bool{}
		labelled     = map[int]time.Time{}
		ejected      = map[[2]any]bool{}    // a number and a head ejected as the cause, however often queued
		ejectedHeads = map[int][]ejection{} // each pull request's ejections as the cause, with the trunk it was ejected on
		patchIDs     = map[[2]queue.SHA]patchResult{}
		trunks       = map[queue.SHA]bool{}
		took         = map[queue.Kind][]float64{}
		verdicts     = map[queue.SHA][]queue.Verdict{} // each tree's whole builds, by the prefix they test
		good         = map[queue.SHA]bool{}            // the trees a whole build passed, each prefix of its chain among them
		waits        []time.Duration
		spans        []time.Duration
		ejects       []time.Duration
		busy         time.Duration
		landed       int
		wrong        int
		unchecked    int
		unreadable   int
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
			if !pr.Queued.IsZero() && (labelled[pr.Number].IsZero() || pr.Queued.Before(labelled[pr.Number])) {
				labelled[pr.Number] = pr.Queued
			}
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
				if h, ok := at[a.PR]; ok {
					ejected[[2]any{h.Number, h.Head}] = true
					ejectedHeads[h.Number] = append(ejectedHeads[h.Number], ejection{h.Head, rec.World.Trunk})
					if !ready[h].IsZero() {
						ejects = append(ejects, now.Sub(ready[h]))
					}
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
					since := labelled[m.Number]
					if since.IsZero() {
						since = ready[h]
					}
					spans = append(spans, now.Sub(since))
					if ejected[[2]any{m.Number, m.Head}] {
						wrong++
					} else if prior := earlierHeads(ejectedHeads[m.Number], m.Head); len(prior) > 0 {
						if clone == "" {
							unchecked++
						} else if matched, lost := sameChange(clone, rec.World.Trunk, m.Head, prior, patchIDs); matched {
							wrong++
						} else if lost {
							unreadable++
						}
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
	inBuilds := func(ds []time.Duration) []float64 {
		var xs []float64
		for _, d := range ds {
			xs = append(xs, d.Minutes()/whole)
		}
		return xs
	}
	waitBuilds, spanBuilds := inBuilds(waits), inBuilds(spans)
	sc := Score{Shape: filepath.Base(dir), Config: "live", Landed: landed,
		WaitP50: percentile(waitBuilds, 50), WaitP90: percentile(waitBuilds, 90),
		LabelP50: percentile(spanBuilds, 50), LabelP90: percentile(spanBuilds, 90), LabelSlow: slowest(spanBuilds, 20),
		EjectP90: percentile(inBuilds(ejects), 90), Unchecked: unchecked, Unreadable: unreadable}
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
	fmt.Printf("labelled to landed: %.0f min at the median, %.0f at p90, %.0f at the mean of the slowest twentieth\n",
		sc.LabelP50*whole, sc.LabelP90*whole, sc.LabelSlow*whole)
	fmt.Printf("wrong ejections: %d in %d landings (%.2f per 1,000)", wrong, landed, sc.Wrong)
	if sc.Unchecked > 0 {
		fmt.Printf(", %d at a newer head unchecked", sc.Unchecked)
	}
	if sc.Unreadable > 0 {
		fmt.Printf(", %d at a newer head unreadable", sc.Unreadable)
	}
	fmt.Println()
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
	var waits, labelled, ejects []float64
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
		waits, labelled, ejects = append(waits, s.Waits...), append(labelled, s.Labelled...), append(ejects, s.Ejects...)
	}
	if sc.Landed > 0 {
		sc.Wrong, sc.Builds, sc.PerHour = 1000*float64(wrong)/float64(sc.Landed), float64(whole)/float64(sc.Landed), float64(sc.Landed)/hours
	}
	sc.Shape, sc.Config = was.Shape, "simulated"
	sc.WaitP50, sc.WaitP90, sc.EjectP90 = percentile(waits, 50), percentile(waits, 90), percentile(ejects, 90)
	sc.LabelP50, sc.LabelP90, sc.LabelSlow = percentile(labelled, 50), percentile(labelled, 90), slowest(labelled, 20)
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

type ejection struct {
	head, trunk queue.SHA
}

type patchResult struct {
	id string
	ok bool
}

func earlierHeads(all []ejection, head queue.SHA) []ejection {
	var out []ejection
	for _, e := range all {
		if e.head != head {
			out = append(out, e)
		}
	}
	return out
}

func sameChange(clone string, trunk, head queue.SHA, prior []ejection, cache map[[2]queue.SHA]patchResult) (matched, unreadable bool) {
	landedID, ok := cachedPatchID(clone, trunk, head, cache)
	if !ok {
		return false, true
	}
	for _, e := range prior {
		eid, eok := cachedPatchID(clone, e.trunk, e.head, cache)
		if !eok {
			unreadable = true
			continue
		}
		if landedID != "" && eid == landedID {
			return true, false
		}
	}
	return false, unreadable
}

func cachedPatchID(clone string, trunk, head queue.SHA, cache map[[2]queue.SHA]patchResult) (string, bool) {
	k := [2]queue.SHA{trunk, head}
	if r, ok := cache[k]; ok {
		return r.id, r.ok
	}
	id, ok := patchID(clone, trunk, head)
	cache[k] = patchResult{id, ok}
	return id, ok
}

// patchID is the patch id of what a head changes from where it left the trunk, which a rebase
// or a merge of the trunk leaves alone. It is empty where the head changes nothing, and not ok
// where the clone lacks a commit. Nothing here writes to the clone.
func patchID(clone string, trunk, head queue.SHA) (string, bool) {
	base, err := gitRead(clone, nil, "merge-base", string(trunk)+"^{commit}", string(head)+"^{commit}")
	if err != nil {
		return "", false
	}
	diff, err := gitRead(clone, nil, "diff", "--no-ext-diff", "--no-textconv", "--binary", strings.TrimSpace(string(base)), string(head)+"^{commit}", "--")
	if err != nil {
		return "", false
	}
	id, err := gitRead(clone, diff, "patch-id", "--stable")
	if err != nil {
		return "", false
	}
	if fields := strings.Fields(string(id)); len(fields) > 0 {
		return fields[0], true
	}
	return "", true
}

func gitRead(dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir, cmd.Stdin = dir, bytes.NewReader(stdin)
	return cmd.Output()
}
