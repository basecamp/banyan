package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	sc, win, row, err := scoreLive(dir, clone, liveOptions{})
	if err == nil {
		printLive(sc, row)
	}
	return sc, win, err
}

func scoreLive(dir, clone string, opts liveOptions) (Score, window, liveRow, error) {
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	bounded := opts.from != nil || opts.to != nil
	context := opts.context()
	var recs []queue.Recording
	for _, name := range names {
		if tick, err := time.Parse("20060102T150405Z", strings.SplitN(strings.TrimSuffix(filepath.Base(name), ".json"), "-", 2)[0]); err == nil {
			if context.to != nil && !tick.Before(*context.to) || context.from != nil && !tick.Add(time.Second).After(*context.from) {
				continue
			}
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return Score{}, window{}, liveRow{}, err
		}
		var rec queue.Recording
		if err := json.Unmarshal(data, &rec); err != nil {
			return Score{}, window{}, liveRow{}, fmt.Errorf("%s: %w", name, err)
		}
		if !context.contains(rec.World.Now) {
			continue
		}
		recs = append(recs, rec)
	}
	slices.SortStableFunc(recs, func(a, b queue.Recording) int { return a.World.Now.Compare(b.World.Now) })
	var ticks []queue.Recording
	for _, rec := range recs {
		if opts.contains(rec.World.Now) {
			ticks = append(ticks, rec)
		}
	}
	if len(recs) == 0 {
		return Score{}, window{}, liveRow{}, fmt.Errorf("%s holds no recorded ticks", dir)
	}
	if clone != "" {
		if _, err := gitRead(clone, nil, "rev-parse", "--git-dir"); err != nil {
			return Score{}, window{}, liveRow{}, fmt.Errorf("clone %s: %w", clone, err)
		}
	}

	var (
		before     = map[queue.SHA]bool{}
		observed   = map[queue.SHA]bool{}
		firstPush  = map[queue.SHA]time.Time{}
		pushed     = map[queue.SHA]queue.Build{}
		pushTimes  = map[queue.SHA]time.Time{}
		answered   = map[queue.SHA]time.Time{}
		candidates = map[queue.SHA]queue.Build{}
		ready      = map[recordedHead]time.Time{}
		seen       = map[recordedHead]bool{}
		seenQueue  = map[[2]any]bool{}
		knownHeads = map[headKey]recordedHead{}

		labelled     = map[int]time.Time{}
		ejected      = map[[2]any]bool{}    // a number and a head ejected as the cause, however often queued
		ejectedHeads = map[int][]ejection{} // each pull request's ejections as the cause, with the trunk it was ejected on
		patchIDs     = map[[2]queue.SHA]patchResult{}
		trunks       = map[queue.SHA]bool{}
		unconfirmed  = map[queue.SHA]bool{}
		took         = map[queue.Kind][]float64{}
		timed        = map[queue.Kind][]timedBuild{}   // took, less the builds whose verdict was first read after an unrecorded stretch
		verdicts     = map[queue.SHA][]queue.Verdict{} // each tree's whole builds, by the prefix they test
		good         = map[queue.SHA]bool{}            // the trees a whole build passed, each prefix of its chain among them
		waits        []time.Duration
		spans        []time.Duration
		ejects       []time.Duration
		busy         time.Duration
		landings     []liveLanding
		landed       int
		wrong        int
		unchecked    int
		unreadable   int
		causes       int
		withoutWait  int
		errored      int
	)
	for _, rec := range recs {
		trunks[rec.World.Trunk] = true
		for _, a := range rec.Actions {
			if a.Do == "land" && opts.contains(rec.World.Now) {
				unconfirmed[a.SHA] = true
			}
		}
	}
	for i, rec := range recs {
		now := rec.World.Now
		delete(unconfirmed, rec.World.Trunk)
		if i > 0 && len(recs[i-1].World.PRs) > 0 {
			from, to := recs[i-1].World.Now, now
			if opts.from != nil && from.Before(*opts.from) {
				from = *opts.from
			}
			if opts.to != nil && to.After(*opts.to) {
				to = *opts.to
			}
			if to.After(from) {
				busy += to.Sub(from)
			}
		}
		at := map[int]recordedHead{}
		for _, pr := range rec.World.PRs {
			if !pr.Queued.IsZero() && (labelled[pr.Number].IsZero() || pr.Queued.Before(labelled[pr.Number])) {
				labelled[pr.Number] = pr.Queued
			}
			h := recordedHead{pr.Number, pr.Head, pr.Queued}
			if at[pr.Number] = h; ready[h].IsZero() && pr.Wait == "" && pr.Checks == queue.Success {
				if ready[h] = pr.Queued; seen[h] || seenQueue[[2]any{pr.Number, pr.Queued}] || pr.Queued.IsZero() {
					ready[h] = now
				}
			}
			seen[h] = true
			seenQueue[[2]any{pr.Number, pr.Queued}] = true
			knownHeads[headKey{pr.Number, pr.Head}] = h
		}
		for _, b := range rec.World.Builds {
			if !b.Pushed.IsZero() {
				if _, ok := pushed[b.SHA]; !ok {
					pushed[b.SHA], pushTimes[b.SHA] = b, b.Pushed
				}
			}
			_, counted := answered[b.SHA]
			if !b.Landed.IsZero() && (!observed[b.SHA] || b.SHA == rec.World.Trunk && !counted) {
				before[b.SHA] = true
			}
			observed[b.SHA] = true
			if _, seen := firstPush[b.SHA]; !seen {
				firstPush[b.SHA] = b.Pushed
			}
			if before[b.SHA] {
				continue
			}
			if b.Verdict == queue.Pending || counted {
				continue
			}
			answerAt, pushedAt := now, b.Pushed
			if !b.Landed.IsZero() {
				answerAt, pushedAt = b.Landed, firstPush[b.SHA]
			}
			answered[b.SHA] = answerAt
			if opts.contains(answerAt) {
				took[b.Kind] = append(took[b.Kind], answerAt.Sub(pushedAt).Minutes())
				if (!b.Landed.IsZero() && b.Landed.Before(now)) || i == 0 || now.Sub(recs[i-1].World.Now) <= unrecordedStretch {
					timed[b.Kind] = append(timed[b.Kind], timedBuild{b.SHA, b.Verdict == queue.Failure, time.Duration(answerAt.Sub(pushedAt).Minutes() * float64(time.Minute))})
				}
				if b.Kind == queue.Candidate {
					candidates[b.SHA] = b
				}
			}
			if b.Kind != queue.Probe {
				if opts.contains(answerAt) {
					verdicts[b.Tests()] = append(verdicts[b.Tests()], b.Verdict)
				}
				for _, m := range b.Chain {
					good[m.Merge] = good[m.Merge] || b.Verdict == queue.Success
				}
				good[b.Tests()] = good[b.Tests()] || b.Verdict == queue.Success
			}
		}
		for _, a := range rec.Actions {
			if before[a.SHA] || before[a.Build.SHA] {
				continue
			}
			switch {
			case a.Do == "push":
				pushed[a.Build.SHA] = a.Build
				if _, ok := pushTimes[a.Build.SHA]; !ok {
					pushTimes[a.Build.SHA] = now
				}
				if _, seen := firstPush[a.Build.SHA]; !seen {
					firstPush[a.Build.SHA] = now
				}
				observed[a.Build.SHA] = true
			case a.Do == "eject" && a.Cause == "culprit":
				if opts.contains(now) {
					causes++
				}
				if h, ok := at[a.PR]; ok {
					ejected[[2]any{h.number, h.head}] = true
					ejectedHeads[h.number] = append(ejectedHeads[h.number], ejection{h.head, rec.World.Trunk})
					if opts.contains(now) && !ready[h].IsZero() {
						ejects = append(ejects, now.Sub(ready[h]))
					}
				}
			case a.Do == "land" && trunks[a.SHA]:
				// The plan to land is not the landing: it counts once a later tick read the
				// trunk there, and once however often a refused push had it planned again.
				trunks[a.SHA] = false
				for _, m := range a.Members {
					h := at[m.Number]
					if h.head != m.Head {
						h = knownHeads[headKey{m.Number, m.Head}]
					}
					if h.head != m.Head {
						h = recordedHead{}
					}
					r := ready[h]
					landings = append(landings, liveLanding{at: now, ready: r, sha: a.SHA})
					if !opts.contains(now) {
						continue
					}
					landed++
					start := r
					if start.IsZero() {
						start = h.queued
					}
					if start.IsZero() {
						withoutWait++
					} else {
						waits = append(waits, now.Sub(start))
						since := labelled[m.Number]
						if since.IsZero() {
							since = start
						}
						spans = append(spans, now.Sub(since))
					}
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

	whole := percentile(append(slices.Clone(took[queue.Candidate]), took[queue.Check]...), 50)
	if whole == 0 && !opts.row {
		return Score{}, window{}, liveRow{}, fmt.Errorf("%s holds no build with a verdict", dir)
	}
	builds, hasProbe := 0.0, false
	wholeBuilds := 0
	for _, sha := range slices.Sorted(maps.Keys(pushed)) {
		b := pushed[sha]
		if !opts.contains(pushTimes[sha]) {
			continue
		}
		if b.Kind == queue.Probe {
			hasProbe = true
			if whole > 0 {
				probe := percentile(took[queue.Probe], 50)
				if probe == 0 && opts.from != nil && opts.to != nil {
					probe = whole
				}
				builds += probe / whole
			}
		} else {
			builds++
			wholeBuilds++
		}
	}
	inBuilds := func(ds []time.Duration) []float64 {
		var xs []float64
		for _, d := range ds {
			if whole > 0 {
				xs = append(xs, d.Minutes()/whole)
			}
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
	// A tree that a whole build passed is good, alone or with more merged onto it, so every
	// other verdict on it is CI's own failure.
	for tree, vs := range verdicts {
		if good[tree] {
			sc.Good += len(vs)
			for _, v := range vs {
				if v != queue.Success {
					sc.Flaked++
					if v != queue.Failure {
						errored++
					}
				}
			}
		}
	}
	win := window{build: time.Duration(whole * float64(time.Minute))}
	if len(ticks) > 0 {
		win.cfg = ticks[len(ticks)-1].Config
	}
	if opts.from != nil && opts.to != nil {
		win.stop = opts.to.Sub(*opts.from)
		win.timed = append(slices.Clone(timed[queue.Candidate]), timed[queue.Check]...)
		win.probe = win.build
		if len(took[queue.Probe]) > 0 {
			win.probe = time.Duration(percentile(took[queue.Probe], 50) * float64(time.Minute))
		}
		if sc.Good > 0 {
			win.errored = float64(errored) / float64(sc.Good)
		}
		pairs := ""
		if opts.replays {
			pairs = clone
		}
		if err := win.recorded(recs, ready, candidates, *opts.from, *opts.to, pairs); err != nil {
			return Score{}, window{}, liveRow{}, fmt.Errorf("clone %s: %w", clone, err)
		}
	} else {
		for _, at := range ready {
			if len(ticks) > 0 && (opts.to == nil || at.Before(*opts.to)) {
				win.arrivals = append(win.arrivals, max(at.Sub(ticks[0].World.Now), 0))
			}
		}
	}
	slices.Sort(win.arrivals)
	row := liveRow{
		From: opts.from, To: opts.to,
		Ticks: len(ticks), ContextTicks: len(recs) - len(ticks), Commit: evalRevision(), Landed: landed, busy: busy, BusyMinutes: busy.Minutes(),
		WholeBuilds: wholeBuilds, WrongEjections: wrong, Unchecked: unchecked, Unreadable: unreadable, EjectionsAsCause: causes,
		GoodBuilds: sc.Good, NonPassingBuilds: sc.Flaked, liveMeasures: measureLive(recs, opts, landings),
	}
	if withoutWait > 0 {
		row.LandingsWithoutAWait = &withoutWait
	}
	if bounded {
		count := len(unconfirmed)
		row.UnconfirmedLandingPlans = &count
	}
	if win.stop > 0 {
		settings := map[queue.Config]bool{}
		for _, rec := range ticks {
			settings[rec.Config] = true
		}
		arrivals, configs := len(win.arrivals), len(settings)
		opening, left := win.opening, win.left
		row.Arrivals, row.WaitingAtOpen, row.SettingsInWindow, row.LeftWaiting = &arrivals, &opening, &configs, &left
		row.Withdrawn = &win.withdrawn
		row.DeparturesAfterAnUnconfirmedPlan = &win.unconfirmedDepartures
	}
	if row.SplitLandings+row.UnsplitLandings != landed {
		return Score{}, window{}, liveRow{}, fmt.Errorf("split landings do not match counted landings")
	}
	if len(ticks) > 0 {
		row.FirstTick, row.LastTick = ticks[0].World.Now, ticks[len(ticks)-1].World.Now
	}
	if len(took[queue.Candidate])+len(took[queue.Check]) > 0 {
		row.BuildMedianMinutes = &whole
	}
	if len(waits) > 0 {
		row.WaitP50Minutes, row.WaitP90Minutes = number(sc.WaitP50*whole), number(sc.WaitP90*whole)
		row.LabelledP50Minutes, row.LabelledP90Minutes, row.LabelledSlowest20thMinutes = number(sc.LabelP50*whole), number(sc.LabelP90*whole), number(sc.LabelSlow*whole)
		if whole > 0 {
			row.WaitP50, row.WaitP90 = number(sc.WaitP50), number(sc.WaitP90)
			row.LabelledP50, row.LabelledP90, row.LabelledSlowest20th = number(sc.LabelP50), number(sc.LabelP90), number(sc.LabelSlow)
		} else {
			row.WaitP50Minutes, row.WaitP90Minutes = durationQuantile(waits, 50), durationQuantile(waits, 90)
			row.LabelledP50Minutes, row.LabelledP90Minutes = durationQuantile(spans, 50), durationQuantile(spans, 90)
			var minutes []float64
			for _, d := range spans {
				minutes = append(minutes, d.Minutes())
			}
			row.LabelledSlowest20thMinutes = number(slowest(minutes, 20))
		}
	}
	if len(ejects) > 0 {
		row.EjectedP90Minutes = durationQuantile(ejects, 90)
		if whole > 0 {
			row.EjectedP90 = number(sc.EjectP90)
		}
	}
	if landed > 0 {
		row.WrongPer1000 = number(sc.Wrong)
		if whole > 0 || !hasProbe {
			row.BuildsPerLanding = number(sc.Builds)
		}
	}
	if busy > 0 {
		row.LandingsPerHour = number(sc.PerHour)
	}
	return sc, win, row, nil
}

// unrecordedStretch is the longest time between two recorded ticks that still times a build
// by the tick that first read its verdict. A verdict first read after a longer stretch came at
// some unknown time within it, so the replay draws nothing from that build's time. A landing
// marked by that same tick is no earlier a time; one marked before it is.
const unrecordedStretch = 30 * time.Minute

// window holds the arrivals or recorded acts, CI times and settings for a replay.
type window struct {
	arrivals                                        []time.Duration
	timed                                           []timedBuild                  // a bounded window's whole builds whose time the record holds
	times                                           []time.Duration               // the same, each as long as it took, less those a guilty head failed
	failing                                         map[queue.SHA][]time.Duration // each guilty head's failed builds among them, each as long as it took
	acts                                            []Act
	unblamedRedCandidates, candidatesWithoutCulprit int
	opening, left, withdrawn, settings              int
	ignoredHolds                                    int
	clashing                                        map[queue.SHA]map[queue.SHA]bool
	clashingPairs                                   *int
	unattributed, unconfirmedDepartures             int
	unejectedGuilty                                 int
	flake, errored                                  float64
	build                                           time.Duration
	probe                                           time.Duration
	stop                                            time.Duration
	cfg                                             queue.Config
}

// timedBuild is a whole build whose time the record holds, and whether it failed.
type timedBuild struct {
	sha    queue.SHA
	failed bool
	took   time.Duration
}

// perturb changes only the replay's build times or recorded settings.
func (w window) perturb(name string) (window, error) {
	switch name {
	case "":
		return w, nil
	case "builds-double", "builds-half":
		scale := func(d time.Duration) time.Duration {
			if name == "builds-double" {
				return 2 * d
			}
			return d / 2
		}
		w.build, w.probe = scale(w.build), scale(w.probe)
		w.times = slices.Clone(w.times)
		for i := range w.times {
			w.times[i] = scale(w.times[i])
		}
		failing := map[queue.SHA][]time.Duration{}
		for head, times := range w.failing {
			for _, d := range times {
				failing[head] = append(failing[head], scale(d))
			}
		}
		w.failing = failing
	case "one-slot", "no-batch":
		change := func(c queue.Config) queue.Config {
			if name == "one-slot" {
				c.Slots = 1
			} else {
				c.MaxBatch = 1
			}
			return c
		}
		w.cfg = change(w.cfg)
		w.acts = slices.Clone(w.acts)
		for i, a := range w.acts {
			if a.Do == "settings" {
				c := change(*a.Config)
				w.acts[i].Config = &c
			}
		}
	default:
		return w, fmt.Errorf("unknown perturbation %q", name)
	}
	return w, nil
}

// simulated scores recorded traffic using the window's CI observations.
func (w window) simulated(shape Shape, was Score) Score {
	sc, _ := w.simulate(shape, was)
	return sc
}

func (w window) simulate(shape Shape, was Score) (Score, liveComparison) {
	shape.Backlog, shape.Arrivals, shape.Build, shape.Hours = 0, 0, w.build, 0
	if w.stop > 0 {
		shape.Stop, shape.Recorded, shape.Acts = w.stop, true, w.acts
		shape.Queue = nil
		shape.Moves, shape.Removals, shape.Break, shape.Conflict, shape.Ahead, shape.Clash = 0, 0, 0, 0, 0, 0
		shape.Agents, shape.Lost, shape.Flake, shape.Errored = 0, 0, w.flake, w.errored
		shape.Outside, shape.Race, shape.Crash = 0, 0, 0
		shape.ProbeFlake, shape.Spread, shape.Fails = 0, 0, 0
		shape.Episodes, shape.Script, shape.Takes = nil, nil, nil
		shape.Rerun, shape.Rescued, shape.Whole = 0, 0, 0
		shape.Times, shape.Probe = w.times, w.probe
		shape.Clashing, shape.Failing = w.clashing, w.failing
	} else if was.Good > 0 {
		shape.Flake = float64(was.Flaked) / float64(was.Good)
	}
	var sc Score
	var waits, labelled, ejects []float64
	var added []float64
	whole, wrong, hours := 0, 0, 0.0
	probes, red, causes, left, opening, failures := 0, 0, 0, 0, 0, 0
	redCandidates, redPullRequests, over30 := 0, 0, 0
	for _, s := range each(seeds, func(i int) *Sim {
		world, rng := shape, rand.New(rand.NewSource(int64(i)))
		if w.stop <= 0 {
			for n, at := range w.arrivals {
				world.Queue = append(slices.Clone(world.Queue), Queued{Number: n + 1, At: at, Breaks: rng.Float64() < shape.Break})
				world.Hours = max(world.Hours, at.Hours())
			}
		}
		s := NewSim(world, w.cfg, int64(i))
		s.Run()
		return s
	}) {
		sc.Landed, whole, wrong, hours = sc.Landed+s.Landed, whole+s.Whole, wrong+s.Wrong, hours+s.LastLand.Hours()
		probes, red, causes = probes+s.Probes, red+s.Red, causes+s.Ejected["culprit"]
		redCandidates, redPullRequests = redCandidates+s.RedCandidates, redPullRequests+s.RedPullRequests
		added = append(added, s.QueueAdded...)
		for _, minutes := range s.QueueAdded {
			if minutes > 30 {
				over30++
			}
		}
		opening += s.opening
		failures += len(s.Broken)
		for _, p := range s.prs {
			if s.leftWaiting(p) {
				left++
			}
		}
		waits, labelled, ejects = append(waits, s.Waits...), append(labelled, s.Labelled...), append(ejects, s.Ejects...)
	}
	if sc.Landed > 0 {
		sc.Wrong, sc.Builds, sc.PerHour = 1000*float64(wrong)/float64(sc.Landed), float64(whole)/float64(sc.Landed), float64(sc.Landed)/hours
		if w.stop > 0 {
			probe := shape.Probe
			if probe == 0 {
				probe = shape.Build
			}
			sc.Builds += float64(probes) * float64(probe) / float64(shape.Build) / float64(sc.Landed)
		}
	}
	sc.Shape, sc.Config = was.Shape, "simulated"
	sc.WaitP50, sc.WaitP90, sc.EjectP90 = percentile(waits, 50), percentile(waits, 90), percentile(ejects, 90)
	sc.LabelP50, sc.LabelP90, sc.LabelSlow = percentile(labelled, 50), percentile(labelled, 90), slowest(labelled, 20)
	comparison := liveComparison{Landed: number(float64(sc.Landed / seeds))}
	if w.stop > 0 {
		comparison.Landed = number(float64(sc.Landed) / seeds)
		comparison.simulatedMeasures = &simulatedMeasures{
			WholeBuilds: float64(whole) / seeds, Probes: float64(probes) / seeds,
			RedBuilds: float64(red) / seeds, EjectedAsCause: float64(causes) / seeds, LeftWaiting: float64(left) / seeds,
			QueueAddedP50Minutes: median(added), QueueAddedP90Minutes: quantile(added, 90), QueueAddedOver30: float64(over30) / seeds,
			RedCandidates: float64(redCandidates) / seeds, RedPullRequests: float64(redPullRequests) / seeds,
			UnblamedRedCandidates: w.unblamedRedCandidates, CandidatesWithoutACulprit: w.candidatesWithoutCulprit,
			WaitingAtOpen: float64(opening) / seeds, InvariantFailures: failures, SettingsInWindow: w.settings, Acts: map[string]int{"label": 0, "unlabel": 0, "head": 0, "checks": 0, "wait": 0, "conflict": 0, "hold": 0, "ejected": 0, "settings": 0}, UnattributedHolds: w.ignoredHolds, ClashingPairs: w.clashingPairs, CulpritsUnattributed: w.unattributed, UnejectedGuiltyHeads: w.unejectedGuilty, DeparturesAfterAnUnconfirmedPlan: w.unconfirmedDepartures,
		}
		for _, a := range w.acts {
			if a.At >= 0 {
				comparison.Acts[a.Do]++
			}
		}
		if len(waits) > 0 {
			comparison.WaitP50Minutes, comparison.WaitP90Minutes = number(sc.WaitP50*shape.Build.Minutes()), number(sc.WaitP90*shape.Build.Minutes())
		}
	}
	if len(waits) > 0 {
		comparison.WaitP50, comparison.WaitP90 = number(sc.WaitP50), number(sc.WaitP90)
	}
	if sc.Landed > 0 {
		comparison.BuildsPerLanding = number(sc.Builds)
	}
	sc.Landed /= seeds
	return sc, comparison
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
