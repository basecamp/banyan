package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

func TestReplayQueueAddedIncludesWaitForReplacementAfterRed(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	next := p
	next.Head, next.Checks = "replacement", queue.Pending
	green := queue.Build{SHA: "green", Pushed: replayStart.Add(40 * time.Minute), Verdict: queue.Success, Chain: []queue.Member{{Number: 2, Head: q.Head, Merge: "green"}}}
	day := func(redAt int) []queue.Recording {
		red := queue.Build{SHA: "red", Pushed: replayStart, Chain: []queue.Member{{Number: 1, Head: p.Head, Merge: "p"}, {Number: 2, Head: q.Head, Merge: "red"}}}
		recs := []queue.Recording{replayTick(0, p, q), replayTick(redAt, p, q), replayTick(40, next, q), replayTick(72, next, q), replayTick(73, next)}
		for i := range recs {
			recs[i].Config = dials(1, 2, true)
		}
		recs[0].Actions = []queue.Action{{Do: "push", Build: red}}
		red.Verdict = queue.Failure
		recs[1].World.Builds = []queue.Build{red}
		recs[1].Actions = []queue.Action{{Do: "eject", PR: p.Number, Cause: "culprit", Evidence: []queue.SHA{red.SHA}}}
		recs[2].Actions = []queue.Action{{Do: "push", Build: green}}
		recs[3].World.Builds = []queue.Build{green}
		recs[3].Actions = []queue.Action{{Do: "land", SHA: green.SHA, Members: green.Chain}}
		recs[4].World.Trunk = green.SHA
		return recs
	}
	for _, tc := range []struct {
		name            string
		redAt, from, to int
		landed          float64
		red             float64
		added           *float64
	}{
		{"whole window", 32, 0, 80, 1, 1, number(40)},
		{"opens after a red build shorter than the green one was answered", 10, 20, 80, 1, 0, number(40)},
		{"landing after window", 32, 0, 70, 0, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, row := recordedWindow(t, day(tc.redAt), replayStart.Add(time.Duration(tc.from)*time.Minute), replayStart.Add(time.Duration(tc.to)*time.Minute))
			_, sim := w.simulate(Shape{}, was)
			if *sim.Landed != tc.landed || sim.InvariantFailures != 0 {
				t.Fatalf("landings %v, invariant failures %d", *sim.Landed, sim.InvariantFailures)
			}
			if !reflect.DeepEqual(sim.QueueAddedP50Minutes, tc.added) || !reflect.DeepEqual(sim.QueueAddedP90Minutes, tc.added) || sim.QueueAddedOver30 != tc.landed {
				t.Errorf("queue added p50=%v p90=%v over30=%v, want %v / %v", sim.QueueAddedP50Minutes, sim.QueueAddedP90Minutes, sim.QueueAddedOver30, tc.added, tc.landed)
			}
			if sim.RedCandidates != tc.red || sim.RedPullRequests != 2*tc.red {
				t.Errorf("red candidates=%v pull requests=%v, want %v / %v", sim.RedCandidates, sim.RedPullRequests, tc.red, 2*tc.red)
			}
			if *sim.Landed != float64(row.Landed) || (!reflect.DeepEqual(sim.QueueAddedP90Minutes, row.QueueAddedP90Minutes) || sim.QueueAddedOver30 != float64(row.QueueAddedOver30) || sim.RedCandidates != float64(row.RedCandidates) || sim.RedPullRequests != float64(row.RedPullRequests)) {
				t.Error("simulated measures differ from the recorded clocks")
			}
		})
	}
}

func recordedWindow(t *testing.T, recs []queue.Recording, from, to time.Time) (Score, window, liveRow) {
	t.Helper()
	sc, win, row, err := scoreLive(writeLive(t, recs), "", liveOptions{from: &from, to: &to, row: true})
	if err != nil {
		t.Fatal(err)
	}
	return sc, win, row
}

var replayStart = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func replayPR(n int) queue.PR {
	return queue.PR{Number: n, Head: queue.SHA(string(rune('a' + n))), Queued: replayStart, Checks: queue.Success}
}
func replayTick(minutes int, prs ...queue.PR) queue.Recording {
	return queue.Recording{World: queue.World{Now: replayStart.Add(time.Duration(minutes) * time.Minute), Trunk: "t", PRs: prs}, Config: dials(1, 1, false)}
}
func replayWindow(t *testing.T, recs []queue.Recording, minutes int) (Score, window, liveRow) {
	return recordedWindow(t, recs, replayStart, replayStart.Add(time.Duration(minutes)*time.Minute))
}
func replayShape(w window) Shape {
	return Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: w.build, Probe: w.probe, Times: w.times, Failing: w.failing}
}
func runReplay(w window) *Sim {
	s := NewSim(replayShape(w), w.cfg, 1)
	s.Run()
	return s
}
func durationBuild(recs []queue.Recording, minutes int) []queue.Recording {
	b := queue.Build{SHA: "timing", Pushed: replayStart, Verdict: queue.Success}
	r := replayTick(minutes)
	r.World.Builds = []queue.Build{b}
	recs[0].Actions = append(recs[0].Actions, queue.Action{Do: "push", Build: b})
	var latest time.Time
	for _, rec := range recs {
		if !rec.World.Now.After(r.World.Now) && (latest.IsZero() || !rec.World.Now.Before(latest)) {
			latest, r.World.PRs = rec.World.Now, rec.World.PRs
		}
	}
	return append(recs, r)
}
func replayKinds(w window, n int) []string {
	var out []string
	for _, a := range w.acts {
		if a.Number == n {
			out = append(out, a.Do)
		}
	}
	return out
}

func TestReplayWindowArrivals(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-20 * time.Minute)
	q := replayPR(2)
	q.Queued = p.Queued
	r := q
	r.Queued = replayStart.Add(7 * time.Minute)
	_, w, row := replayWindow(t, []queue.Recording{replayTick(-20, p, q), replayTick(-10, q), replayTick(3, q), replayTick(5), replayTick(7, r)}, 10)
	if *row.Arrivals != 2 || *row.WaitingAtOpen != 1 || *row.LeftWaiting != 1 || *row.Withdrawn != 1 {
		t.Fatalf("population: %+v; %v", row, w.arrivals)
	}
}
func TestReplayWindowLiveOutcomesWithdrawalRule(t *testing.T) {
	p, q, r, s := replayPR(1), replayPR(2), replayPR(3), replayPR(4)
	recs := []queue.Recording{replayTick(0, p, q, r, s), replayTick(1, s)}
	recs[0].Actions = []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 1, Head: p.Head}}}, {Do: "eject", PR: 2, Cause: "conflict"}}
	recs[1].World.Trunk = "b"
	_, w, row := replayWindow(t, recs, 10)
	if *row.LeftWaiting != 1 || *row.Withdrawn != 1 || slices.Contains(replayKinds(w, 1), "unlabel") || slices.Contains(replayKinds(w, 2), "unlabel") || !slices.Contains(replayKinds(w, 3), "unlabel") {
		t.Fatalf("outcomes: %+v / %v", row, w.acts)
	}
}
func TestReplayWindowArrivalFromLandingEvidence(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-time.Hour)
	recs := []queue.Recording{replayTick(-1, p), replayTick(1), replayTick(2)}
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 1, Head: p.Head}, {Number: 2, Head: "unknown"}}}}
	recs[2].World.Trunk = "b"
	_, _, row := replayWindow(t, recs, 10)
	if row.WaitP50Minutes == nil || *row.WaitP50Minutes != 61 || row.LandingsWithoutAWait == nil || *row.LandingsWithoutAWait != 1 || row.Landed != 2 {
		t.Fatalf("landing waits: %+v", row)
	}
}
func TestReplayWindowSettings(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	recs := durationBuild([]queue.Recording{replayTick(0, p, q), replayTick(4, p, q)}, 10)
	recs[0].Config.Slots = 0
	recs[1].Config.Slots = 2
	recs[2].Config.Slots = 2
	_, w, row := replayWindow(t, recs, 30)
	s := runReplay(w)
	if *row.SettingsInWindow != 2 || s.cfg.Slots != 2 || s.Whole != 2 || s.prs[1].at < 14*time.Minute {
		t.Fatalf("timeline: cfg %+v, row %+v, sim %+v", s.cfg, row, s)
	}
}
func TestReplayRecordedCancels(t *testing.T) {
	_, w, _ := replayWindow(t, durationBuild([]queue.Recording{replayTick(0, replayPR(1))}, 10), 30)
	shape := Shape{Recorded: true, Cancels: true, Acts: w.acts, Build: w.build}
	s := NewSim(shape, w.cfg, 1)
	if s.cfg.Cancels || s.shape.Cancels {
		t.Fatal("shape overrode recorded Cancels")
	}
	for _, a := range w.acts {
		if a.Do == "settings" && a.Config.Cancels {
			t.Fatal("unexpected recorded cancellation")
		}
	}
}
func TestReplayDepartureFactsWithdrawalRule(t *testing.T) {
	for _, why := range []string{"absent", "head", "requeued", "checks", "wait", "conflict", "waiting", "lands", "culprit"} {
		t.Run(why, func(t *testing.T) {
			p := replayPR(1)
			next := p
			recs := []queue.Recording{replayTick(0, p), replayTick(1, p), replayTick(2, p)}
			switch why {
			case "head":
				next.Head = "new"
			case "requeued":
				next.Queued = replayStart.Add(time.Minute)
			case "checks":
				next.Checks = queue.Pending
			case "wait":
				next.Wait = "thread"
			case "conflict":
				next.Note = trunkConflict
			case "lands":
				recs[0].Actions = []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
				recs[1].World.Trunk = "b"
			case "culprit":
				recs[0].Actions = []queue.Action{{Do: "eject", PR: 1, Cause: "culprit"}}
			}
			recs[1].World.PRs = []queue.PR{next}
			recs[2].World.PRs = []queue.PR{next}
			if why == "absent" || why == "lands" || why == "culprit" {
				recs[1].World.PRs = nil
				recs[2].World.PRs = nil
			}
			_, w, row := replayWindow(t, recs, 10)
			if slices.Contains(replayKinds(w, 1), "unlabel") != (why == "absent") || (*row.Withdrawn == 1) != (why == "absent") {
				t.Fatalf("departure %s: %v / %+v", why, w.acts, row)
			}
		})
	}
}
func TestReplayReadyPendingReady(t *testing.T) {
	p := replayPR(1)
	pending := p
	pending.Checks = queue.Pending
	recs := durationBuild([]queue.Recording{replayTick(0, p), replayTick(2, pending), replayTick(4, p)}, 10)
	_, w, _ := replayWindow(t, recs, 40)
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: w.build}, w.cfg, 1)
	s.play()
	s.now = 2 * time.Minute
	s.play()
	if s.prs[1].Checks != queue.Pending || !s.prs[1].queued || s.prs[1].ready != 0 {
		t.Fatalf("pending checks: %+v", s.prs[1])
	}
	s.Run()
	if len(s.prs) != 1 || s.Landed != 1 || !s.prs[1].landed || s.prs[1].ready != 0 {
		t.Fatalf("readiness: %+v", s.prs[1])
	}
}
func TestReplayNumberQueuedTwiceLandsOnce(t *testing.T) {
	for _, departure := range []int{2, 16} {
		t.Run(fmt.Sprint(departure), func(t *testing.T) {
			p := replayPR(1)
			again := p
			again.Queued = replayStart.Add(20 * time.Minute)
			recs := durationBuild([]queue.Recording{replayTick(0, p), replayTick(departure), replayTick(20, again)}, 10)
			_, w, _ := replayWindow(t, recs, 60)
			s := runReplay(w)
			if len(s.prs) != 1 || s.Landed != 1 || !s.prs[1].landed || departure == 2 && s.prs[1].at < 30*time.Minute {
				t.Fatalf("requeued: %+v", s)
			}
		})
	}
}
func TestReplayConflictIsDated(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	note, fixed := p, p
	note.Note, fixed.Head = trunkConflict, "fixed"
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(0, p, q), replayTick(5, note, q), replayTick(8, fixed, q)}, 30)
	for _, a := range w.acts {
		if a.Do == "conflict" && (len(a.After) != 0 || a.At != 5*time.Minute) {
			t.Fatalf("conflict: %+v", a)
		}
	}
	for _, winner := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(winner), func(t *testing.T) {
			s := NewSim(Shape{Recorded: true, Acts: w.acts, Build: 10 * time.Minute}, w.cfg, 1)
			s.now = 0
			s.play()
			if winner != 0 {
				s.prs[winner].landed = true
			}
			s.history.behind[1] = time.Hour
			s.now += 4 * time.Minute
			s.play()
			if s.prs[1].conflict || s.prs[2].conflict {
				t.Fatal("conflict activated before its record")
			}
			s.now += time.Minute
			s.play()
			if s.prs[1].conflict != (winner != 1) || s.prs[2].conflict {
				t.Fatal("dated conflict was lost or passed on")
			}
			s.now += 3 * time.Minute
			s.play()
			if s.prs[1].conflict || s.prs[2].conflict {
				t.Fatal("new head did not end the conflict")
			}
			if winner != 1 && s.prs[1].ready != 8*time.Minute {
				t.Fatal("replay delay shifted the ready clock")
			}
		})
	}
}
func TestReplayFirstRecordFailedMergePushesNoBuild(t *testing.T) {
	p := replayPR(2283)
	p.Queued = replayStart.Add(8 * time.Second)
	first := replayTick(0, p)
	first.World.Now = replayStart.Add(46 * time.Second)
	first.Commits = map[string]queue.Answer{fmt.Sprintf("merge t %d %s %q", p.Number, p.Head, ""): {OK: false}}
	note := p
	note.Note = trunkConflict
	_, w, _ := replayWindow(t, []queue.Recording{first, replayTick(1, note)}, 60)
	w.build = 10 * time.Minute
	s := runReplay(w)
	if s.Whole != 0 || s.Landed != 0 || !s.prs[p.Number].conflict {
		t.Fatalf("conflicted head: %+v", s)
	}
	for _, a := range w.acts {
		if a.Number == p.Number && a.At < 46*time.Second {
			t.Fatalf("fact backdated: %+v", a)
		}
	}
}
func TestReplayOpensFourHoursBeforeTheWindowOrAtTheFirstRecord(t *testing.T) {
	p, q, gone := replayPR(1), replayPR(2), replayPR(4)
	p.Queued = replayStart.Add(-400 * time.Minute)
	for _, tc := range []struct {
		name                 string
		recs                 []queue.Recording
		opens, first, second time.Duration
	}{
		{"a record before the lead-in", []queue.Recording{replayTick(-360, p), replayTick(-180, p, q), replayTick(0, p, q)}, -4 * time.Hour, -4 * time.Hour, -3 * time.Hour},
		{"two records before the lead-in", []queue.Recording{replayTick(-360, p, gone), replayTick(-300, p), replayTick(-180, p, q), replayTick(0, p, q)}, -4 * time.Hour, -4 * time.Hour, -3 * time.Hour},
		{"the first record within it", []queue.Recording{replayTick(-120, p), replayTick(-60, p, q), replayTick(0, p, q)}, -2 * time.Hour, -2 * time.Hour, -time.Hour},
		{"no record before the window", []queue.Recording{replayTick(3, p), replayTick(5, p, q)}, 0, 3 * time.Minute, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, _ := replayWindow(t, tc.recs, 30)
			if s := NewSim(replayShape(w), w.cfg, 1); s.now != tc.opens || w.acts[0].At != tc.first || w.acts[0].Do != "settings" {
				t.Fatalf("opens at %v, first act %+v, want %v and %v", s.now, w.acts[0], tc.opens, tc.first)
			}
			if label := labelled(w, 2); !slices.Equal(label, []time.Duration{tc.second}) {
				t.Fatalf("#2 labelled at %v, want %v", label, tc.second)
			}
			if kinds := replayKinds(w, gone.Number); len(kinds) != 0 {
				t.Fatalf("#4, gone by the last record before the lead-in, has acts %v", kinds)
			}
			s := NewSim(replayShape(w), w.cfg, 1)
			s.now = tc.first
			s.play()
			if o := s.prs[1]; o.arrived != -400*time.Minute || !o.Queued.Equal(s.epoch.Add(-400*time.Minute)) || o.ready != -400*time.Minute {
				t.Fatalf("#1 opened arrived at %v, queued at %v, ready at %v, want its recorded -6h40m", o.arrived, o.Queued.Sub(s.epoch), o.ready)
			}
		})
	}
}

// buildingAtTheOpen is a planned day on which #1, #2 and #3 are queued at 2m and their
// candidate, pushed then, lands at 12m, and the replay's window opens at 10m.
func buildingAtTheOpen(t *testing.T) (Score, window, liveRow) {
	recs := plannedDay(threeQueued(), dials(1, 3, false), 40, func(*Sim) {})
	return recordedWindow(t, recs, replayStart.Add(10*time.Minute), replayStart.Add(40*time.Minute))
}

func TestReplayLandsACandidateBuildingAtTheWindowsStartWhenTheRecordDoes(t *testing.T) {
	_, w, row := buildingAtTheOpen(t)
	s := runReplay(w)
	for n := 1; n <= 3; n++ {
		if p := s.prs[n]; !p.landed || p.at != 2*time.Minute {
			t.Fatalf("#%d landed %v at %v, want at 2m as recorded", n, p.landed, p.at)
		}
	}
	if _, sim := w.simulate(Shape{}, Score{}); *sim.Landed != float64(row.Landed) || sim.WholeBuilds != float64(row.WholeBuilds) || sim.WholeBuilds != 0 {
		t.Fatalf("landed %v of %d, whole builds %v of %d", *sim.Landed, row.Landed, sim.WholeBuilds, row.WholeBuilds)
	}
	shape := threeQueued()
	shape.Queue = append(shape.Queue, Queued{At: 14 * time.Minute, Number: 4})
	recs := plannedDay(shape, dials(1, 3, false), 60, func(*Sim) {})
	_, w, row = recordedWindow(t, recs, replayStart.Add(12*time.Minute), replayStart.Add(60*time.Minute))
	s = runReplay(w)
	for n, at := range map[int]time.Duration{1: 0, 2: 0, 3: 0, 4: 12 * time.Minute} {
		if p := s.prs[n]; !p.landed || p.at != at {
			t.Fatalf("#%d landed %v at %v, want at %v as recorded", n, p.landed, p.at, at)
		}
	}
	if !slices.Equal(s.Lands, []time.Duration{0, 0, 0, 12 * time.Minute}) || s.LastLand != 12*time.Minute {
		t.Fatalf("counted landings at %v, the last at %v", s.Lands, s.LastLand)
	}
	if _, sim := w.simulate(Shape{}, Score{}); *sim.Landed != float64(row.Landed) || row.Landed != 4 || sim.WholeBuilds != float64(row.WholeBuilds) || sim.WholeBuilds != 1 {
		t.Fatalf("landed %v of %d, whole builds %v of %d", *sim.Landed, row.Landed, sim.WholeBuilds, row.WholeBuilds)
	}
}

func TestReplayCountsTheWaitOfAPullRequestReadyBeforeTheWindowFromItsOwnReadyClock(t *testing.T) {
	_, w, row := buildingAtTheOpen(t)
	s := runReplay(w)
	if p := s.prs[1]; p.ready != -8*time.Minute || len(s.Waits) != 3 || s.Waits[0] != float64(p.at-p.ready)/float64(w.build) {
		t.Fatalf("ready %v, waits %v", p.ready, s.Waits)
	}
	if _, sim := w.simulate(Shape{}, Score{}); *sim.WaitP50Minutes != *row.WaitP50Minutes || *row.WaitP50Minutes != 10 {
		t.Fatalf("simulated wait %v, live %v", *sim.WaitP50Minutes, *row.WaitP50Minutes)
	}
	readyAt := map[int]time.Duration{1: -12 * time.Minute, 2: -8 * time.Minute, 3: -3 * time.Minute}
	var prs []queue.PR
	for n := 1; n <= 3; n++ {
		p := replayPR(n)
		p.Checks = queue.Pending
		prs = append(prs, p)
	}
	tick := func(minutes int) queue.Recording {
		var at []queue.PR
		for _, p := range prs {
			if readyAt[p.Number] <= time.Duration(minutes-30)*time.Minute {
				p.Checks = queue.Success
			}
			at = append(at, p)
		}
		return replayTick(minutes, at...)
	}
	build := queue.Build{SHA: "all", Pushed: replayStart.Add(27 * time.Minute), Chain: []queue.Member{{Number: 1, Head: prs[0].Head, Merge: "m1"}, {Number: 2, Head: prs[1].Head, Merge: "m2"}, {Number: 3, Head: prs[2].Head, Merge: "all"}}}
	recs := []queue.Recording{tick(0), tick(18), tick(22), tick(27), tick(42), replayTick(43)}
	for i := range recs {
		recs[i].Config = dials(1, 3, false)
	}
	recs[3].Actions = []queue.Action{{Do: "push", Build: build}}
	build.Verdict = queue.Success
	recs[4].World.Builds = []queue.Build{build}
	recs[4].Actions = []queue.Action{{Do: "land", SHA: build.SHA, Members: build.Chain}}
	recs[5].World.Trunk = build.SHA
	_, w, _ = recordedWindow(t, recs, replayStart.Add(30*time.Minute), replayStart.Add(60*time.Minute))
	s = runReplay(w)
	for n, ready := range readyAt {
		if p := s.prs[n]; !p.landed || p.at < 0 || p.arrived != -30*time.Minute || p.ready != ready || !slices.Contains(s.Waits, float64(p.at-ready)/float64(w.build)) {
			t.Fatalf("#%d landed %v, labelled at %v, ready at %v, want %v; waits %v", n, p.landed, p.arrived, p.ready, ready, s.Waits)
		}
	}
	if len(s.Waits) != 3 {
		t.Fatalf("waits %v, want the three", s.Waits)
	}
}

func TestReplayCountsNothingOfALandingBeforeTheWindow(t *testing.T) {
	shape := threeQueued(3)
	shape.Queue = append(shape.Queue, Queued{At: 40 * time.Minute, Number: 4})
	recs := plannedDay(shape, dials(1, 3, true), 90, func(*Sim) {})
	_, w, row := recordedWindow(t, recs, replayStart.Add(34*time.Minute), replayStart.Add(90*time.Minute))
	s := runReplay(w)
	for n, at := range map[int]time.Duration{1: -2 * time.Minute, 2: -2 * time.Minute, 4: 16 * time.Minute} {
		if p := s.prs[n]; !p.landed || p.at != at {
			t.Fatalf("#%d landed %v at %v, want at %v", n, p.landed, p.at, at)
		}
	}
	if s.prs[4].ready != 6*time.Minute || s.Landed != 1 || !slices.Equal(s.Lands, []time.Duration{16 * time.Minute}) || !slices.Equal(s.Waits, []float64{1}) || !slices.Equal(s.Labelled, []float64{1}) || !slices.Equal(s.QueueAdded, []float64{0}) || s.LastLand != 16*time.Minute || s.Whole != 1 || s.Probes != 0 || s.RedCandidates != 0 {
		t.Fatalf("counted %d landings %v, waits %v, labelled %v, added %v, last %v, %d whole builds, %d probes, %d red candidates", s.Landed, s.Lands, s.Waits, s.Labelled, s.QueueAdded, s.LastLand, s.Whole, s.Probes, s.RedCandidates)
	}
	if _, sim := w.simulate(Shape{}, Score{}); *sim.Landed != float64(row.Landed) || sim.WholeBuilds != float64(row.WholeBuilds) {
		t.Fatalf("landed %v of %d, whole builds %v of %d", *sim.Landed, row.Landed, sim.WholeBuilds, row.WholeBuilds)
	}
	_, w, _ = recordedWindow(t, recs, replayStart.Add(34*time.Minute), replayStart.Add(39*time.Minute))
	if s := runReplay(w); s.Landed != 0 || len(s.Lands) != 0 || s.LastLand != 0 || s.Probes != 0 {
		t.Fatalf("a window landing nothing counted %d landings %v, the last at %v, %d probes", s.Landed, s.Lands, s.LastLand, s.Probes)
	}
}

func TestReplayBrokenBeforeTheWindowScoresNothingButItsFailure(t *testing.T) {
	shape := threeQueued()
	shape.Queue = append(shape.Queue, Queued{At: 14 * time.Minute, Number: 4})
	recs := plannedDay(shape, dials(1, 3, false), 60, func(*Sim) {})
	_, w, _ := recordedWindow(t, recs, replayStart.Add(20*time.Minute), replayStart.Add(60*time.Minute))
	s := NewSim(replayShape(w), w.cfg, 1)
	landed := 0
	s.plan = func(world queue.World, c queue.Config) []queue.Action {
		acts := queue.Plan(world, c)
		if p := s.prs[4]; landed == 0 && s.Landed > 0 && p != nil && p.queued && s.now < 0 {
			landed = s.Landed
			again := slices.Sorted(maps.Keys(s.seen))[0]
			acts = append(acts, queue.Action{Do: "push", Build: queue.Build{SHA: again}})
		}
		return acts
	}
	s.Run()
	if len(s.Broken) == 0 || s.now >= 0 || landed == 0 {
		t.Fatalf("broken %v at %v after %d landings", s.Broken, s.now, landed)
	}
	if !reflect.DeepEqual(s.scored, scored{Ejected: map[string]int{}}) || s.opening != 0 {
		t.Fatalf("a seed broken in the lead-in scored %+v, %d waiting at the open", s.scored, s.opening)
	}
	for _, p := range s.prs {
		if s.leftWaiting(p) {
			t.Fatalf("#%d left waiting by a seed broken in the lead-in", p.Number)
		}
	}
}

func TestReplayRatesNoLandingsAnHourWhereEveryLandingIsAtZero(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-10 * time.Minute)
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(-10, p), replayTick(0, p), replayTick(30, p)}, 30)
	w.times, w.build = []time.Duration{10 * time.Minute}, 10*time.Minute
	if sc, _ := w.simulate(Shape{}, Score{}); sc.Landed != 1 || sc.PerHour != 0 {
		t.Fatalf("%d landed, %v an hour", sc.Landed, sc.PerHour)
	}
}

func TestReplayCountsNoRedCandidateAnsweredBeforeTheWindow(t *testing.T) {
	_, w, row := recordedWindow(t, guiltDay(), replayStart.Add(14*time.Minute), replayStart.Add(80*time.Minute))
	s := runReplay(w)
	if s.RedCandidates != 0 || s.RedPullRequests != 0 || row.RedCandidates != 0 || !s.prs[3].landed {
		t.Fatalf("red candidates %d holding %d, live %d", s.RedCandidates, s.RedPullRequests, row.RedCandidates)
	}
	if _, sim := w.simulate(Shape{}, Score{}); *sim.Landed != float64(row.Landed) || sim.WholeBuilds != float64(row.WholeBuilds) {
		t.Fatalf("landed %v of %d, whole builds %v of %d", *sim.Landed, row.Landed, sim.WholeBuilds, row.WholeBuilds)
	}
	_, w, row = recordedWindow(t, guiltDay(), replayStart.Add(8*time.Minute), replayStart.Add(80*time.Minute))
	s = runReplay(w)
	if s.RedCandidates != 1 || s.RedPullRequests != 3 || row.RedCandidates != 1 || row.RedPullRequests != 3 || s.Whole != 11 || row.WholeBuilds != 11 {
		t.Fatalf("a candidate pushed before the window and red in it: red candidates %d holding %d, live %d holding %d; whole builds %d, live %d", s.RedCandidates, s.RedPullRequests, row.RedCandidates, row.RedPullRequests, s.Whole, row.WholeBuilds)
	}
}

func TestReplayCountsWhatWaitsAtTheWindowsStart(t *testing.T) {
	p, q, r := replayPR(1), replayPR(2), replayPR(3)
	p.Queued, r.Queued = replayStart.Add(-3*time.Minute), replayStart.Add(-3*time.Minute)
	for _, recs := range [][]queue.Recording{
		{replayTick(-3, p, r), replayTick(-1, p), replayTick(1, p, q)},
		{replayTick(-3, p, r), replayTick(-1, p, r), replayTick(0, p), replayTick(1, p, q)},
	} {
		_, w, row := replayWindow(t, durationBuild(recs, 10), 30)
		if _, sim := w.simulate(Shape{}, Score{}); *row.WaitingAtOpen != 1 || sim.WaitingAtOpen != 1 {
			t.Fatalf("waiting at the window's start: live %d, simulated %v", *row.WaitingAtOpen, sim.WaitingAtOpen)
		}
	}
}

func TestReplayCountsTheWindowsOwnActsDeparturesAndHolds(t *testing.T) {
	p, q, r := replayPR(1), replayPR(2), replayPR(3)
	held := q
	held.Note = "waits: it conflicts with a pull request ahead of it"
	recs := []queue.Recording{replayTick(-20, p, held, r), replayTick(-10, p, q), replayTick(0, p, held), replayTick(10, p, held)}
	_, w, row := replayWindow(t, recs, 30)
	_, sim := w.simulate(Shape{}, Score{})
	if *row.Withdrawn != 0 || w.ignoredHolds != 1 || sim.UnattributedHolds != 1 {
		t.Fatalf("withdrawn %d, unattributed holds %d, simulated %d", *row.Withdrawn, w.ignoredHolds, sim.UnattributedHolds)
	}
	if want := map[string]int{"label": 0, "unlabel": 0, "head": 0, "checks": 0, "wait": 0, "conflict": 0, "hold": 1, "ejected": 0, "settings": 0}; !reflect.DeepEqual(sim.Acts, want) {
		t.Fatalf("acts %v, want %v", sim.Acts, want)
	}
	recs = []queue.Recording{replayTick(-20, p, held, r), replayTick(-10, p, q, r), replayTick(0, p, held), replayTick(10, p, held)}
	_, w, row = replayWindow(t, recs, 30)
	_, sim = w.simulate(Shape{}, Score{})
	if *row.Withdrawn != 0 || sim.Acts["unlabel"] != 1 || sim.Acts["label"] != 0 {
		t.Fatalf("a withdrawal first seen at the window's start: withdrawn %d, acts %v", *row.Withdrawn, sim.Acts)
	}
}

func TestReplayLabelsAgainBeforeTheWindowAPullRequestEjectedInError(t *testing.T) {
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(-30, replayPR(1)), replayTick(0, replayPR(1))}, 30)
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: 2 * time.Minute, Flake: 0.5}, w.cfg, 27)
	s.play()
	for s.now < 0 && s.Wrong == 0 {
		s.Step()
	}
	if s.now != -20*time.Minute || s.Wrong != 1 || s.Left[1] != "culprit" || s.prs[1].queued {
		t.Fatalf("at %v %d wrong ejections, #1 left %q, queued %v", s.now, s.Wrong, s.Left[1], s.prs[1].queued)
	}
	for s.now < 0 && !s.prs[1].queued {
		s.Step()
	}
	if s.now != -10*time.Minute || !s.prs[1].queued {
		t.Fatalf("at %v #1 queued %v", s.now, s.prs[1].queued)
	}
}
func TestReplayTreatsLiveWinnersAlike(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	a, b := p, q
	a.Checks = queue.Pending
	b.Checks = queue.Pending
	recs := durationBuild([]queue.Recording{replayTick(0, p, q), replayTick(2, a, b), replayTick(4, p, q), replayTick(20, p, q), replayTick(21, p, q)}, 10)
	recs[3].Actions = []queue.Action{{Do: "land", SHA: "land", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
	recs[4].World.Trunk = "land"
	_, w, _ := replayWindow(t, recs, 60)
	var one, two []Act
	for _, a := range w.acts {
		if a.Number != 1 && a.Number != 2 {
			continue
		}
		n := a.Number
		a.Number = 0
		a.Head = ""
		if n == 1 {
			one = append(one, a)
		} else {
			two = append(two, a)
		}
	}
	if !reflect.DeepEqual(one, two) {
		t.Fatalf("outcomes changed acts:\n%+v\n%+v", one, two)
	}
	recs = append(recs, replayTick(22))
	recs[len(recs)-1].World.Trunk = "land"
	_, w, row := replayWindow(t, recs, 60)
	if slices.Contains(replayKinds(w, 1), "unlabel") || !slices.Contains(replayKinds(w, 2), "unlabel") || *row.Withdrawn != 1 {
		t.Fatal("withdrawal rule did not distinguish the confirmed landing")
	}
}
func TestReplayCulpritMatchesEvidenceHead(t *testing.T) {
	p := replayPR(1)
	next := p
	next.Head = "new"
	b := queue.Build{SHA: "bad", Chain: []queue.Member{{Number: 1, Head: p.Head}}}
	recs := []queue.Recording{replayTick(0, p), replayTick(2, next)}
	recs[1].World.Builds = []queue.Build{b}
	recs[1].Actions = []queue.Action{{Do: "eject", PR: 1, Cause: "culprit", Evidence: []queue.SHA{"bad"}}}
	_, w, _ := replayWindow(t, recs, 30)
	for _, a := range w.acts {
		if a.Do == "head" && a.Breaks != (a.Head == p.Head) {
			t.Fatalf("culprit: %+v", a)
		}
	}
}
func TestReplayBuildTimesDoNotAlias(t *testing.T) {
	var recs []queue.Recording
	for i, d := range []int{10, 50, 60, 20} {
		b := queue.Build{SHA: queue.SHA(string(rune('a' + i))), Pushed: replayStart, Verdict: queue.Success}
		if i == 3 {
			b.Kind = queue.Check
		}
		r := replayTick(d)
		r.World.Builds = []queue.Build{b}
		recs = append(recs, r)
	}
	_, w, _ := replayWindow(t, recs, 90)
	slices.Sort(w.times)
	if !slices.Equal(w.times, []time.Duration{10 * time.Minute, 20 * time.Minute, 50 * time.Minute, 60 * time.Minute}) {
		t.Fatal(w.times)
	}
}
func TestReplayLeavesOutABuildAnsweredAfterAnUnrecordedStretch(t *testing.T) {
	short := queue.Build{SHA: "short", Pushed: replayStart, Verdict: queue.Success}
	long := queue.Build{SHA: "long", Pushed: replayStart, Verdict: queue.Success}
	recs := []queue.Recording{replayTick(0), replayTick(10), replayTick(110)}
	recs[1].World.Builds = []queue.Build{short}
	recs[2].World.Builds = []queue.Build{long}
	_, w, row := replayWindow(t, recs, 120)
	if !slices.Equal(w.times, []time.Duration{10 * time.Minute}) {
		t.Fatalf("pool %v", w.times)
	}
	if row.BuildMedianMinutes == nil || *row.BuildMedianMinutes != 110 || row.WholeBuilds != 2 || w.build != 110*time.Minute {
		t.Fatalf("live build median %v, whole builds %d, window build %v", row.BuildMedianMinutes, row.WholeBuilds, w.build)
	}
}
func TestReplayLeavesOutABuildFirstReadLandedAfterAnUnrecordedStretch(t *testing.T) {
	pending := queue.Build{SHA: "lower", Pushed: replayStart}
	recs := []queue.Recording{replayTick(0), replayTick(10), replayTick(110)}
	recs[1].World.Builds = []queue.Build{pending}
	landed := pending
	landed.Verdict, landed.Landed = queue.Success, recs[2].World.Now
	recs[2].World.Builds = []queue.Build{landed}
	_, w, row := replayWindow(t, recs, 120)
	if len(w.times) != 0 || row.WholeBuilds != 1 {
		t.Fatalf("pool %v, whole builds %d", w.times, row.WholeBuilds)
	}
}
func TestReplayKeepsABuildWhoseLandingWasMarkedInsideAnUnrecordedStretch(t *testing.T) {
	pending := queue.Build{SHA: "lower", Pushed: replayStart}
	recs := []queue.Recording{replayTick(0), replayTick(10), replayTick(110)}
	recs[1].World.Builds = []queue.Build{pending}
	landed := pending
	landed.Verdict, landed.Landed = queue.Success, replayStart.Add(40*time.Minute)
	recs[2].World.Builds = []queue.Build{landed}
	_, w, _ := replayWindow(t, recs, 120)
	if !slices.Equal(w.times, []time.Duration{40 * time.Minute}) {
		t.Fatalf("pool %v", w.times)
	}
}
func TestReplayKeepsABuildAnsweredSoonAfterThePreviousTick(t *testing.T) {
	b := queue.Build{SHA: "b", Pushed: replayStart, Verdict: queue.Success}
	recs := []queue.Recording{replayTick(0), replayTick(30), replayTick(40)}
	recs[2].World.Builds = []queue.Build{b}
	_, w, _ := replayWindow(t, recs, 60)
	if !slices.Equal(w.times, []time.Duration{40 * time.Minute}) {
		t.Fatalf("pool %v", w.times)
	}
}
func TestReplayNoChanceFromShape(t *testing.T) {
	_, w, _ := replayWindow(t, durationBuild([]queue.Recording{replayTick(0, replayPR(1))}, 10), 40)
	_, plain := w.simulate(Shape{}, Score{})
	_, noisy := w.simulate(Shape{Lost: 1, Errored: 1, Flake: 1, ProbeFlake: 1, Moves: 30, Removals: 30, Break: 1, Conflict: 1, Ahead: 1, Clash: 1, Agents: 1, Others: 100, Hold: time.Hour}, Score{})
	if !reflect.DeepEqual(plain, noisy) || *plain.Landed != 1 {
		t.Fatalf("shape chance: %+v / %+v", plain, noisy)
	}
}
func TestReplayFailureAndErrorRates(t *testing.T) {
	recs := []queue.Recording{replayTick(0, replayPR(1))}
	for i, v := range []queue.Verdict{queue.Success, queue.Failure, queue.Error} {
		b := queue.Build{SHA: queue.SHA(string(rune('a' + i))), Pushed: replayStart, Verdict: v, Chain: []queue.Member{{Number: 1, Head: "b", Merge: "tree"}}}
		r := replayTick(10 + i)
		r.World.Builds = []queue.Build{b}
		r.World.PRs = recs[0].World.PRs
		recs = append(recs, r)
	}
	_, w, _ := replayWindow(t, recs, 60)
	if math.Abs(w.flake-1.0/3) > 1e-12 || math.Abs(w.errored-1.0/3) > 1e-12 {
		t.Fatalf("rates %g / %g", w.flake, w.errored)
	}
}
func TestReplayWaitsForNoAgentWhenTheWindowsBuildTimesHoldTheWait(t *testing.T) {
	_, w, _ := replayWindow(t, durationBuild([]queue.Recording{replayTick(0, replayPR(1))}, 10), 40)
	_, sim := w.simulate(Shape{Agents: 1, Others: 60, Burst: 1, Hold: time.Hour}, Score{})
	if *sim.WaitP50Minutes != 10 || *sim.Landed != 1 {
		t.Fatalf("agent wait: %+v", sim.simulatedMeasures)
	}
}

func TestReplayUsesOnlyOpeningLabels(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	p.Queued = replayStart.Add(-8 * time.Hour)
	q.Queued = p.Queued
	recs := []queue.Recording{replayTick(-480, p, q), replayTick(-420, q), replayTick(0, q)}
	recs[0].Actions = []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
	recs[1].World.Trunk = "b"
	recs[2].World.Trunk = "b"
	_, w, _ := replayWindow(t, recs, 30)
	w.build = 10 * time.Minute
	s := runReplay(w)
	if s.prs[1] != nil || s.prs[2] == nil || len(s.prs) != 1 {
		t.Fatalf("opening population: %+v", s.prs)
	}
}
func TestReplayPersonalWaitReleasesAtRecordedTime(t *testing.T) {
	p := replayPR(1)
	p.Wait = "unresolved thread"
	ready := p
	ready.Wait = ""
	recs := durationBuild([]queue.Recording{replayTick(0, p), replayTick(4, ready)}, 10)
	_, w, _ := replayWindow(t, recs, 40)
	s := runReplay(w)
	if s.Landed != 1 || s.prs[1].at != 14*time.Minute || len(s.Waits) != 1 || s.Waits[0] != 1 {
		t.Fatalf("personal wait: %+v", s)
	}
}
func TestReplayNewHeadUsesLiveReadyClock(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-time.Hour)
	p.Checks = queue.Pending
	next := p
	next.Head, next.Checks = "new", queue.Success
	recs := durationBuild([]queue.Recording{replayTick(-60, p), replayTick(5, next), replayTick(15, next), replayTick(16)}, 10)
	recs[2].Actions = []queue.Action{{Do: "land", SHA: "land", Members: []queue.Member{{Number: 1, Head: next.Head}}}}
	recs[3].World.Trunk = "land"
	_, w, row := replayWindow(t, recs, 30)
	_, sim := w.simulate(Shape{}, Score{})
	if row.WaitP50Minutes == nil || sim.WaitP50Minutes == nil || *row.WaitP50Minutes != 10 || *sim.WaitP50Minutes != 10 {
		t.Fatalf("ready clock: live %+v sim %+v", row.WaitP50Minutes, sim.WaitP50Minutes)
	}
	for _, a := range w.acts {
		if a.Do == "head" && a.Head == next.Head && a.At != 5*time.Minute {
			t.Fatalf("new head backdated: %+v", a)
		}
	}
}
func TestReplayBuildWithoutPushRecordProvesClean(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	recs := []queue.Recording{replayTick(0, p, q), replayTick(5, p, q), replayTick(6, p)}
	recs[0].World.Builds = []queue.Build{{SHA: "b", Base: "t", Chain: []queue.Member{{Number: 1, Head: p.Head, Merge: "b"}}}}
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "land", Members: []queue.Member{{Number: 2, Head: q.Head}}}}
	recs[2].World.Trunk = "land"
	recs[2].World.PRs[0].Note = trunkConflict
	_, w, _ := replayWindow(t, recs, 30)
	conflicts := 0
	for _, a := range w.acts {
		if a.Do == "conflict" {
			conflicts++
		}
		if a.Do == "conflict" && (a.At != 6*time.Minute || len(a.After) != 0) {
			t.Fatalf("missing push: %+v", a)
		}
	}
	if conflicts != 1 {
		t.Fatalf("conflicts=%d, want 1", conflicts)
	}
}
func TestReplayHoldLastsAsRecorded(t *testing.T) {
	p, q := replayPR(1), replayPR(2)
	held := q
	held.Note = "waits: it conflicts with a pull request ahead of it"
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(0, p, q), replayTick(5, p, held), replayTick(8, p, q)}, 30)
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Build: time.Minute}, w.cfg, 1)
	s.now = 0
	s.play()
	s.prs[1].landed = true
	s.now += 5 * time.Minute
	s.play()
	if _, ok := s.Merge(s.trunk, q, ""); ok {
		t.Fatal("recorded hold was ignored after another landing")
	}
	s.now += 3 * time.Minute
	s.play()
	if _, ok := s.Merge(s.trunk, q, ""); !ok {
		t.Fatal("recorded hold did not end")
	}
}
func TestReplayProbeWithoutVerdictHasEqualCost(t *testing.T) {
	p := replayPR(1)
	recs := durationBuild([]queue.Recording{replayTick(0, p)}, 10)
	recs[0].Actions = append(recs[0].Actions, queue.Action{Do: "push", Build: queue.Build{SHA: "probe", Kind: queue.Probe}})
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "land", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
	last := replayTick(11)
	last.World.Trunk = "land"
	recs = append(recs, last)
	_, w, row := replayWindow(t, recs, 30)
	if w.probe != w.build || row.BuildsPerLanding == nil || *row.BuildsPerLanding != 2 {
		t.Fatalf("probe fallback: %+v / %+v", w, row)
	}
	s := NewSim(Shape{Recorded: true, Build: w.build, Probe: w.probe}, w.cfg, 1)
	s.push(queue.Build{SHA: "probe", Kind: queue.Probe})
	if s.builds["probe"].end != w.build || s.Probes != 1 {
		t.Fatal("probe did not cost a whole build")
	}
}

func TestReplayConfirmationAtAbsenceWithdrawalRule(t *testing.T) {
	for _, confirmation := range []int{0, 2, 3} {
		t.Run(fmt.Sprint(confirmation), func(t *testing.T) {
			p := replayPR(1)
			recs := []queue.Recording{replayTick(0, p), replayTick(2), replayTick(3)}
			recs[0].Actions = []queue.Action{{Do: "land", SHA: "later", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
			if confirmation != 0 {
				recs[confirmation-1].World.Trunk = "later"
			}
			_, w, row := replayWindow(t, recs, 20)
			withdrawn := confirmation != 2
			if slices.Contains(replayKinds(w, 1), "unlabel") != withdrawn || (*row.Withdrawn == 1) != withdrawn || (*row.DeparturesAfterAnUnconfirmedPlan == 1) != withdrawn {
				t.Fatalf("withdrawal rule: %+v / %+v", row, w.acts)
			}
			w.build = time.Minute
			_, sim := w.simulate(Shape{}, Score{})
			if sim.DeparturesAfterAnUnconfirmedPlan != *row.DeparturesAfterAnUnconfirmedPlan {
				t.Fatal("departure count missing from simulated row")
			}
		})
	}
}

func TestReplayConflictEndsAtNextHeadAfterRequeue(t *testing.T) {
	p := replayPR(1)
	p.Note = trunkConflict
	again := p
	again.Queued = replayStart.Add(4 * time.Minute)
	fixed := again
	fixed.Head, fixed.Note = "fixed", ""
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(0, p), replayTick(2), replayTick(4, again), replayTick(6, fixed)}, 30)
	w.build = time.Minute
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: w.build}, w.cfg, 1)
	s.play()
	if !s.prs[1].conflict {
		t.Fatal("opening conflict missing")
	}
	s.now = 4 * time.Minute
	s.play()
	if !s.prs[1].conflict || !s.prs[1].queued {
		t.Fatal("requeued head lost its conflict")
	}
	s.Run()
	if s.Landed != 1 || s.prs[1].Head != "fixed" || s.prs[1].at != 8*time.Minute {
		t.Fatalf("conflict across requeue: %+v", s.prs[1])
	}
}

func TestReplayRequeueAdmitsReplacementTogether(t *testing.T) {
	p := replayPR(1)
	next := p
	next.Head, next.Queued = "replacement", replayStart.Add(20*time.Minute)
	recs := []queue.Recording{replayTick(0, p), replayTick(2), replayTick(21, next)}
	recs[0].Config.Slots = 0
	_, w, _ := replayWindow(t, recs, 40)
	w.build = 10 * time.Minute
	s := runReplay(w)
	if s.Whole != 1 || s.Landed != 1 || s.prs[1].Head != next.Head || s.prs[1].at != 31*time.Minute {
		t.Fatalf("replacement admission: whole=%d landed=%d pr=%+v", s.Whole, s.Landed, s.prs[1])
	}
}

func TestReplayOldLabelFirstRecordedAfterStartCanLand(t *testing.T) {
	p := replayPR(12046)
	p.Queued, p.Checks = replayStart.Add(-48*time.Hour), queue.Pending
	green := p
	green.Checks = queue.Success
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(3, p), replayTick(20, green)}, 60)
	w.build = 10 * time.Minute
	s := runReplay(w)
	if s.Landed != 1 || s.prs[p.Number].at != 30*time.Minute {
		t.Fatalf("late first observation: %+v", s.prs[p.Number])
	}
}

func TestReplayCulpritWithoutEvidenceBreaksNothing(t *testing.T) {
	p := replayPR(1)
	p.Checks = queue.Pending
	next := p
	next.Head, next.Checks = "new", queue.Success
	recs := []queue.Recording{replayTick(0, p), replayTick(2, next)}
	recs[1].Actions = []queue.Action{{Do: "eject", PR: 1, Cause: "culprit", Evidence: []queue.SHA{"missing"}}}
	_, w, _ := replayWindow(t, recs, 40)
	for _, a := range w.acts {
		if a.Breaks {
			t.Fatalf("guessed culprit: %+v", a)
		}
	}
	w.build = 10 * time.Minute
	s := runReplay(w)
	_, sim := w.simulate(Shape{}, Score{})
	if s.Landed != 1 || s.Red != 0 || s.Ejected["culprit"] != 0 || sim.CulpritsUnattributed != 1 {
		t.Fatalf("unattributed culprit: sim=%+v landed=%d red=%d", sim.simulatedMeasures, s.Landed, s.Red)
	}
}

func TestReplayLiveChargesBuildWithoutPush(t *testing.T) {
	p := replayPR(1)
	b := queue.Build{SHA: "build", Pushed: replayStart.Add(time.Minute), Verdict: queue.Success, Chain: []queue.Member{{Number: 1, Head: p.Head, Merge: "build"}}}
	recs := []queue.Recording{replayTick(0, p), replayTick(11, p), replayTick(12)}
	recs[1].World.Builds = []queue.Build{b}
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "build", Members: b.Chain}}
	recs[2].World.Trunk = "build"
	_, _, row := replayWindow(t, recs, 30)
	if row.BuildsPerLanding == nil || *row.BuildsPerLanding != 1 || row.WholeBuilds != 1 {
		t.Fatalf("missing push charge: %+v", row)
	}
}

func TestReplayConflictDoesNotReturnWithOldHead(t *testing.T) {
	p := replayPR(1)
	conflict, next := p, p
	conflict.Note, next.Head = trunkConflict, "next"
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(0, conflict), replayTick(5, next), replayTick(6, p)}, 30)
	w.build = 10 * time.Minute
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: w.build}, w.cfg, 1)
	s.play()
	if !s.prs[1].conflict {
		t.Fatal("opening conflict missing")
	}
	s.Run()
	if s.Landed != 1 || s.prs[1].conflict || s.prs[1].Head != p.Head {
		t.Fatalf("old conflict returned: %+v", s.prs[1])
	}
}

func TestReplayRequeueKeepsReadyClock(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-time.Hour)
	_, w, _ := replayWindow(t, []queue.Recording{replayTick(0, p)}, 30)
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Build: 10 * time.Minute}, w.cfg, 1)
	s.now = 0
	s.play()
	s.prs[1].queued, s.prs[1].back = false, s.now+2*time.Minute
	s.step(s.now + 2*time.Minute)
	if s.prs[1].ready != -time.Hour {
		t.Fatal("return after a simulated ejection reset the live ready clock")
	}
}

func TestReplayHoldBeginsAtTheRecordWhoseMergeOntoOneAheadFails(t *testing.T) {
	ahead, p := replayPR(1986), replayPR(1987)
	first := replayTick(0, ahead, p)
	first.Commits = map[string]queue.Answer{
		fmt.Sprintf("merge t %d %s %q", ahead.Number, ahead.Head, ""): {SHA: "m1986", OK: true},
		fmt.Sprintf("merge m1986 %d %s %q", p.Number, p.Head, ""):     {OK: false},
	}
	_, w, _ := replayWindow(t, []queue.Recording{first, replayTick(30, ahead, p)}, 60)
	held := 0
	for _, a := range w.acts {
		if a.Number == p.Number && a.Do == "hold" && a.Ahead != 0 {
			if held++; a.At != 0 {
				t.Fatalf("the hold begins at %v, the first record is at %v", a.At, 0)
			}
		}
	}
	if held != 1 {
		t.Fatalf("holds of the pull request behind: %d in %+v", held, w.acts)
	}
}

func TestReplayConflictBeforeRecordedLanding(t *testing.T) {
	p := replayPR(1)
	p.Note = trunkConflict
	fixed := p
	fixed.Head, fixed.Note = "fixed", ""
	recs := []queue.Recording{replayTick(0, p), replayTick(5, fixed), replayTick(15, fixed), replayTick(16)}
	recs[2].Actions = []queue.Action{{Do: "land", SHA: "land", Members: []queue.Member{{Number: p.Number, Head: fixed.Head}}}}
	recs[3].World.Trunk = "land"
	_, w, row := replayWindow(t, recs, 30)
	if row.Landed != 1 || !slices.Contains(replayKinds(w, p.Number), "conflict") {
		t.Fatalf("recorded winner lost conflict: %+v", w.acts)
	}
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Stop: w.stop, Build: 10 * time.Minute}, w.cfg, 1)
	s.play()
	if !s.prs[p.Number].conflict {
		t.Fatal("recorded winner's opening conflict was ignored")
	}
	s.Run()
	if s.Landed != 1 || s.prs[p.Number].Head != fixed.Head || s.prs[p.Number].at < 15*time.Minute {
		t.Fatalf("conflict repair: %+v", s.prs[p.Number])
	}
}

func TestReplayUnblamedCandidateRate(t *testing.T) {
	p := replayPR(1)
	next := p
	next.Head = "replacement"
	recs := []queue.Recording{replayTick(0, p)}
	for i, tc := range []struct {
		kind    queue.Kind
		verdict queue.Verdict
		head    queue.SHA
	}{
		{queue.Candidate, queue.Failure, "sound"},
		{queue.Candidate, queue.Success, "sound"},
		{queue.Candidate, queue.Error, "sound"},
		{queue.Candidate, queue.Failure, p.Head},
		{queue.Candidate, queue.Success, p.Head},
		{queue.Check, queue.Failure, "sound"},
		{queue.Probe, queue.Failure, "sound"},
		{queue.Candidate, queue.Pending, "sound"},
		{queue.Candidate, queue.Failure, "never-passed"},
	} {
		b := queue.Build{SHA: queue.SHA(fmt.Sprintf("b%d", i)), Kind: tc.kind, Pushed: replayStart, Verdict: tc.verdict,
			Chain: []queue.Member{{Number: 1, Head: tc.head, Merge: tc.head}}}
		r := replayTick(10+i, next)
		r.World.Builds = []queue.Build{b}
		if i == 3 {
			r.Actions = []queue.Action{{Do: "eject", PR: 1, Cause: "culprit", Evidence: []queue.SHA{b.SHA}}}
		}
		recs = append(recs, r, r)
	}
	outside := replayTick(31, next)
	outside.World.Builds = []queue.Build{{SHA: "outside", Pushed: replayStart, Verdict: queue.Failure}}
	recs = append(recs, outside)
	was, w, row := replayWindow(t, recs, 30)
	_, sim := w.simulate(Shape{}, was)
	if w.unblamedRedCandidates != 2 || w.candidatesWithoutCulprit != 4 || w.flake != 0.5 || sim.UnblamedRedCandidates != 2 || sim.CandidatesWithoutACulprit != 4 {
		t.Fatalf("unblamed rate: window=%+v simulated=%+v", w, sim.simulatedMeasures)
	}
	if row.GoodBuilds != 6 || row.NonPassingBuilds != 4 || math.Abs(w.errored-1.0/6) > 1e-12 {
		t.Fatalf("live good=%d nonpassing=%d error rate=%g", row.GoodBuilds, row.NonPassingBuilds, w.errored)
	}
}

func TestReplayPerturbedRowKeepsLiveFields(t *testing.T) {
	p := replayPR(1)
	recs := durationBuild([]queue.Recording{replayTick(0, p)}, 10)
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "timing", Members: []queue.Member{{Number: 1, Head: p.Head}}}}
	confirmed := replayTick(11)
	confirmed.World.Trunk = "timing"
	recs = append(recs, confirmed)
	dir := writeLive(t, recs)
	from, to := replayStart.Format(time.RFC3339), replayStart.Add(time.Hour).Format(time.RFC3339)
	plain, err := captureLiveRun(t, dir, "hey2", from, to, true)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := captureLiveRun(t, dir, "hey2", from, to, true, "builds-double")
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal([]byte(plain), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(changed), &b); err != nil {
		t.Fatal(err)
	}
	if _, ok := a["perturbed"]; ok || string(b["perturbed"]) != `"builds-double"` || reflect.DeepEqual(a["simulated"], b["simulated"]) {
		t.Fatalf("perturbation missing: plain=%s changed=%s", plain, changed)
	}
	var over liveComparison
	if err := json.Unmarshal(b["over_live"], &over); err != nil {
		t.Fatal(err)
	}
	if over.WaitP50 == nil || *over.WaitP50 != 1 || over.WaitP90 == nil || *over.WaitP90 != 1 {
		t.Fatalf("doubled wait was hidden by build units: %+v", over)
	}
	var simulated map[string]json.RawMessage
	if err := json.Unmarshal(a["simulated"], &simulated); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"arrivals", "withdrawn"} {
		if _, ok := simulated[key]; ok {
			t.Errorf("simulated contains live %s", key)
		}
	}
	for _, key := range []string{"simulated", "over_live", "perturbed"} {
		delete(a, key)
		delete(b, key)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("live fields changed: %s / %s", plain, changed)
	}
}

func TestReplayPerturbTimesAndSettings(t *testing.T) {
	first, next := dials(3, 12, true), dials(5, 2, false)
	w := window{cfg: first, build: 10 * time.Minute, probe: 3 * time.Minute, times: []time.Duration{8 * time.Minute, 12 * time.Minute},
		failing: map[queue.SHA][]time.Duration{"d": {6 * time.Minute}},
		acts:    []Act{{Do: "settings", Config: &first}, {At: time.Hour, Do: "settings", Config: &next}}}
	for _, name := range []string{"builds-double", "builds-half", "one-slot", "no-batch"} {
		t.Run(name, func(t *testing.T) {
			got, err := w.perturb(name)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "builds-double", "builds-half":
				ratio := 2.0
				if name == "builds-half" {
					ratio = 0.5
				}
				if float64(got.build)/float64(w.build) != ratio || float64(got.probe)/float64(w.probe) != ratio {
					t.Fatal("median build or probe was not scaled")
				}
				for i := range w.times {
					if float64(got.times[i])/float64(w.times[i]) != ratio {
						t.Fatal("sample build time was not scaled")
					}
				}
				if float64(got.failing["d"][0])/float64(w.failing["d"][0]) != ratio {
					t.Fatal("a guilty head's failing time was not scaled")
				}
			case "one-slot", "no-batch":
				for _, a := range got.acts {
					if name == "one-slot" && a.Config.Slots != 1 || name == "no-batch" && a.Config.MaxBatch != 1 {
						t.Fatalf("settings: %+v", *a.Config)
					}
				}
			}
		})
	}
	if first != dials(3, 12, true) || next != dials(5, 2, false) || w.build != 10*time.Minute || !slices.Equal(w.times, []time.Duration{8 * time.Minute, 12 * time.Minute}) || w.failing["d"][0] != 6*time.Minute {
		t.Fatal("perturbation changed source window")
	}
	if _, err := w.perturb("unknown"); err == nil {
		t.Fatal("unknown perturbation accepted")
	}
}

func heldGuilty(w window, n int, head queue.SHA) bool {
	return slices.ContainsFunc(w.acts, func(a Act) bool { return a.Do == "head" && a.Number == n && a.Head == head && a.Breaks })
}

// plannedDay runs the planner tick by tick from replayStart against the simulated CI, and keeps
// every pass as the live queue records it. change acts on the world before each tick.
func plannedDay(shape Shape, cfg queue.Config, minutes int, change func(*Sim)) []queue.Recording {
	s := NewSim(shape, cfg, 1)
	s.epoch = replayStart
	var recs []queue.Recording
	s.plan = func(w queue.World, c queue.Config) []queue.Action {
		rec := queue.Record(w, c)
		recs = append(recs, *rec)
		return rec.Actions
	}
	for s.now < time.Duration(minutes)*time.Minute {
		change(s)
		s.Step()
	}
	return recs
}

// movesHead has the author of a pull request push a new head at a time; the builds of its old
// head fail while they stand, and the new one breaks nothing.
func movesHead(n int, head queue.SHA, at time.Duration) func(*Sim) {
	return func(s *Sim) {
		switch p := s.prs[n]; {
		case s.now == at:
			p.Head = head
			s.byHead[head] = n
		case s.now == at+tick:
			p.breaks = false
		}
	}
}

func threeQueued(breaks ...int) Shape {
	shape := Shape{Build: 10 * time.Minute, Probe: 4 * time.Minute}
	for n := 1; n <= 3; n++ {
		shape.Queue = append(shape.Queue, Queued{At: tick, Number: n, Breaks: slices.Contains(breaks, n)})
	}
	return shape
}

// guiltDay is the planner's own day: #3 breaks the build of #1, #2 and #3, the round on the
// red candidate fails it again, and its author pushes a new head before the fair test that
// would eject it ends.
func guiltDay() []queue.Recording {
	return plannedDay(threeQueued(3), dials(1, 3, false), 70, movesHead(3, "fix3", 32*time.Minute))
}

// readAt is the record read at that time with a pass of its planner.
func readAt(recs []queue.Recording, minutes int) *queue.Recording {
	for i := range recs {
		if recs[i].World.Now.Equal(replayStart.Add(time.Duration(minutes) * time.Minute)) {
			return &recs[i]
		}
	}
	panic(fmt.Sprintf("no record at %dm", minutes))
}

// holdingHead3 is a build of #1, #2 and #3's breaking head on the trunk it opened on.
func holdingHead3(recs []queue.Recording, sha queue.SHA, kind queue.Kind, v queue.Verdict) queue.Build {
	chain := slices.Clone(readAt(recs, 12).World.Builds[0].Chain)
	return queue.Build{SHA: sha, Kind: kind, Base: recs[0].World.Trunk, Chain: chain, Verdict: v}
}

func TestReplayHoldsGuiltyAHeadThatFailedTwiceNeverPassedAndLeft(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	if !heldGuilty(w, 3, "head3") || heldGuilty(w, 3, "fix3") || heldGuilty(w, 1, "head1") || heldGuilty(w, 2, "head2") {
		t.Fatalf("guilt: head3 %v, fix3 %v", heldGuilty(w, 3, "head3"), heldGuilty(w, 3, "fix3"))
	}
	if w.unblamedRedCandidates != 0 || w.candidatesWithoutCulprit != 3 || w.flake != 0 {
		t.Fatalf("unblamed red candidates %d of %d, flake %g", w.unblamedRedCandidates, w.candidatesWithoutCulprit, w.flake)
	}
}

func TestReplayHoldsNoHeadGuiltyWithoutEvidenceAgainstIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		day  func() []queue.Recording
	}{
		{"a candidate holding it passed", func() []queue.Recording {
			recs := guiltDay()
			r := readAt(recs, 30)
			r.World.Builds = append(r.World.Builds, holdingHead3(recs, "green", queue.Candidate, queue.Success))
			return recs
		}},
		{"a check holding it passed", func() []queue.Recording {
			recs := guiltDay()
			r := readAt(recs, 30)
			r.World.Builds = append(r.World.Builds, holdingHead3(recs, "green", queue.Check, queue.Success))
			return recs
		}},
		{"a probe holding it passed", func() []queue.Recording {
			recs := guiltDay()
			r := readAt(recs, 30)
			r.World.Builds = append(r.World.Builds, holdingHead3(recs, "green", queue.Probe, queue.Success))
			return recs
		}},
		{"it failed once", func() []queue.Recording {
			return plannedDay(threeQueued(3), dials(1, 3, false), 70, movesHead(3, "fix3", 12*time.Minute))
		}},
		{"its second failure was a probe", func() []queue.Recording {
			return plannedDay(threeQueued(3), dials(1, 3, true), 70, movesHead(3, "fix3", 20*time.Minute))
		}},
		{"an ejected culprit failed beside it", func() []queue.Recording {
			return plannedDay(threeQueued(2, 3), dials(1, 3, false), 90, movesHead(3, "fix3", 42*time.Minute))
		}},
		{"its landing was confirmed", func() []queue.Recording {
			recs := guiltDay()
			for i, a := range readAt(recs, 32).Actions {
				if a.Do == "land" {
					readAt(recs, 32).Actions[i].Members = append(slices.Clone(a.Members), queue.Member{Number: 3, Head: "head3"})
				}
			}
			return recs
		}},
		{"it was still queued at the last record", func() []queue.Recording {
			return plannedDay(threeQueued(3), dials(1, 3, false), 32, movesHead(3, "fix3", 32*time.Minute))
		}},
		{"a build that failed it was read green first", func() []queue.Recording {
			recs := guiltDay()
			for i, b := range readAt(recs, 30).World.Builds {
				if b.Kind == queue.Check && len(b.Chain) == 3 && b.Round == 2 {
					readAt(recs, 30).World.Builds[i].Verdict = queue.Success
				}
			}
			return recs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, _ := replayWindow(t, tc.day(), 100)
			if heldGuilty(w, 3, "head3") || len(labelled(w, 3)) != 1 {
				t.Fatalf("head3 held guilty %v, labelled at %v", heldGuilty(w, 3, "head3"), labelled(w, 3))
			}
		})
	}
}

func TestReplayHoldsNoHeadGuiltyThatNoRecordQueued(t *testing.T) {
	p := replayPR(1)
	recs := []queue.Recording{replayTick(0, p)}
	for i, minutes := range []int{4, 6} {
		r := replayTick(10*(i+1), p)
		r.World.Builds = []queue.Build{{SHA: queue.SHA(fmt.Sprintf("red%d", i)), Pushed: r.World.Now.Add(-time.Duration(minutes) * time.Minute), Verdict: queue.Failure,
			Chain: []queue.Member{{Number: 9, Head: "gone9", Merge: queue.SHA(fmt.Sprintf("red%d", i))}}}}
		recs = append(recs, r)
	}
	recs = append(recs, replayTick(30, p))
	_, w, _ := replayWindow(t, recs, 40)
	if len(w.failing) != 0 || !slices.Equal(slices.Sorted(slices.Values(w.times)), []time.Duration{4 * time.Minute, 6 * time.Minute}) || w.unblamedRedCandidates != 2 {
		t.Fatalf("failing %v, shared sample %v, unblamed red candidates %d", w.failing, w.times, w.unblamedRedCandidates)
	}
}

func labelled(w window, n int) []time.Duration {
	var at []time.Duration
	for _, a := range w.acts {
		if a.Number == n && a.Do == "label" {
			at = append(at, a.At)
		}
	}
	return at
}

// fastReplay plays the window with every build taking two minutes, so the replay's queue
// ejects a breaking head sooner than the record's would have.
func fastReplay(w window) *Sim {
	shape := replayShape(w)
	shape.Times, shape.Probe = []time.Duration{2 * time.Minute}, 2*time.Minute
	for head := range shape.Failing {
		shape.Failing[head] = []time.Duration{2 * time.Minute}
	}
	s := NewSim(shape, w.cfg, 1)
	s.Run()
	return s
}

func TestReplayRequeuesAtItsNextHeadAPullRequestItEjectedFirst(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	if labels := labelled(w, 3); !slices.Equal(labels, []time.Duration{2 * time.Minute, 34 * time.Minute}) {
		t.Fatalf("labelled at %v", labels)
	}
	for _, a := range w.acts {
		if a.Number == 3 && a.Do == "label" && (a.Queued == nil || *a.Queued != 2*time.Minute) {
			t.Fatalf("#3 labelled at %v as queued at %v, want the record's 2m", a.At, a.Queued)
		}
	}
	s := fastReplay(w)
	if p := s.prs[3]; s.Left[3] != "culprit" || !p.landed || p.Head != "fix3" || len(s.Broken) > 0 {
		t.Fatalf("#3 left for %q, landed %v at head %s: %v", s.Left[3], p.landed, p.Head, s.Broken)
	}
	if p := s.prs[3]; p.arrived != 2*time.Minute || !p.Queued.Equal(s.epoch.Add(2*time.Minute)) {
		t.Fatalf("#3 came back as arrived at %v, queued at %v", p.arrived, p.Queued.Sub(s.epoch))
	}
}

func TestReplayLeavesAPullRequestTheRecordEjectedOutUntilItIsLabelledAgain(t *testing.T) {
	recs := plannedDay(threeQueued(3), dials(1, 3, false), 70, func(s *Sim) {
		switch p := s.prs[3]; s.now {
		case 40 * time.Minute:
			p.Head, p.breaks = "fix3", false
			s.byHead[p.Head] = 3
		case 50 * time.Minute:
			p.queued, p.Queued = true, s.epoch.Add(s.now)
		}
	})
	_, w, _ := replayWindow(t, recs, 80)
	if labels := labelled(w, 3); !heldGuilty(w, 3, "head3") || !slices.Equal(labels, []time.Duration{2 * time.Minute, 52 * time.Minute}) {
		t.Fatalf("guilty %v, labelled at %v", heldGuilty(w, 3, "head3"), labels)
	}
	s := fastReplay(w)
	if p := s.prs[3]; s.Left[3] != "culprit" || !p.landed || p.Head != "fix3" || p.at < 52*time.Minute {
		t.Fatalf("#3 left for %q, landed %v at head %s at %v", s.Left[3], p.landed, p.Head, p.at)
	}
}

func TestReplayRequeuesAtItsNextHeadAPullRequestTheRecordEjectedAndLabelledAgain(t *testing.T) {
	move := movesHead(3, "fix3", 56*time.Minute)
	recs := plannedDay(threeQueued(3), dials(1, 3, false), 100, func(s *Sim) {
		if p := s.prs[3]; s.now == 40*time.Minute {
			p.queued, p.Queued = true, s.epoch.Add(s.now)
		}
		move(s)
	})
	_, w, _ := replayWindow(t, recs, 110)
	if labels := labelled(w, 3); !heldGuilty(w, 3, "head3") || !slices.Equal(labels, []time.Duration{2 * time.Minute, 42 * time.Minute, 58 * time.Minute}) {
		t.Fatalf("guilty %v, labelled at %v", heldGuilty(w, 3, "head3"), labels)
	}
	s := fastReplay(w)
	if p := s.prs[3]; s.Left[3] != "culprit" || !p.landed || p.Head != "fix3" || p.at < 58*time.Minute {
		t.Fatalf("#3 left for %q, landed %v at head %s at %v", s.Left[3], p.landed, p.Head, p.at)
	}
}

// firstTreeHolding opens the replay, lets mark change the simulated queue, runs the planner's
// first tick, and gives the first tree it pushed that holds this pull request.
func firstTreeHolding(t *testing.T, w window, n int, change func(*Shape)) (*Sim, queue.SHA) {
	t.Helper()
	shape := replayShape(w)
	change(&shape)
	s := NewSim(shape, w.cfg, 1)
	s.play()
	for step := s.tick; s.now < w.stop; step = s.Step {
		seen := maps.Clone(s.builds)
		step()
		for _, sha := range s.shas() {
			if _, ok := seen[sha]; !ok && s.holds[sha][n] {
				return s, sha
			}
		}
	}
	t.Fatal("no build holds the pull request")
	return nil, ""
}

func firstBuildHolding(t *testing.T, w window, n int) time.Duration {
	t.Helper()
	s, sha := firstTreeHolding(t, w, n, func(*Shape) {})
	return s.builds[sha].end - s.now
}

// timedGuiltDay is guiltDay with the three builds that fail #3 taking 6, 8 and 4 minutes, the
// first, fourth and ninth pushed, and every other build its 10.
func timedGuiltDay() []queue.Recording {
	shape := threeQueued(3)
	shape.Takes = make([]time.Duration, 9)
	shape.Takes[0], shape.Takes[3], shape.Takes[8] = 6*time.Minute, 8*time.Minute, 4*time.Minute
	return plannedDay(shape, dials(1, 3, false), 70, movesHead(3, "fix3", 32*time.Minute))
}

func TestReplayGuiltyHeadsFailingTimesLeaveTheSharedSample(t *testing.T) {
	_, w, _ := replayWindow(t, timedGuiltDay(), 80)
	failing := slices.Sorted(slices.Values(w.failing["head3"]))
	if !heldGuilty(w, 3, "head3") || !slices.Equal(failing, []time.Duration{4 * time.Minute, 6 * time.Minute, 8 * time.Minute}) || len(w.failing) != 1 {
		t.Fatalf("guilty %v, failing times %v", heldGuilty(w, 3, "head3"), w.failing)
	}
	if len(w.times) == 0 || slices.ContainsFunc(w.times, func(d time.Duration) bool { return d != 10*time.Minute }) {
		t.Fatalf("shared sample %v", w.times)
	}
	if took := firstBuildHolding(t, w, 3); !slices.Contains(failing, took) {
		t.Fatalf("the tree #3 breaks took %v, want one of its recorded %v", took, failing)
	}
}

func TestReplayRowWaitsByTheGuiltyHeadsFailingTimes(t *testing.T) {
	was, w, _ := replayWindow(t, timedGuiltDay(), 80)
	_, timed := w.simulate(Shape{}, was)
	w.failing = nil
	_, shared := w.simulate(Shape{}, was)
	if timed.WaitP50Minutes == nil || shared.WaitP50Minutes == nil || *timed.WaitP50Minutes >= *shared.WaitP50Minutes {
		t.Fatalf("wait p50 %v with #3's failing times, %v with every build its 10m", timed.WaitP50Minutes, shared.WaitP50Minutes)
	}
}

// cleanDay is the planner's own day of eight pull requests that break nothing, its builds taking
// different times, so the simulated half rests on the shared draws alone.
func cleanDay() []queue.Recording {
	shape := Shape{Build: 10 * time.Minute, Probe: 4 * time.Minute}
	for n := 1; n <= 8; n++ {
		shape.Queue = append(shape.Queue, Queued{At: time.Duration(n) * 3 * tick, Number: n})
	}
	shape.Takes = []time.Duration{6 * time.Minute, 14 * time.Minute, 9 * time.Minute, 12 * time.Minute, 7 * time.Minute, 11 * time.Minute, 16 * time.Minute, 5 * time.Minute, 13 * time.Minute, 8 * time.Minute}
	return plannedDay(shape, dials(1, 2, false), 150, func(*Sim) {})
}

// The simulated half of cleanDay's row as the replay before it held guilty a head that left
// unejected, which had no count of those heads.
const cleanDaySimulated = `{"over_live":{"landed":0,"wait_p50":0,"wait_p90":0.1111111111111111,"builds_per_landing":0.13100000000000006},"simulated":{"landed":8,"wait_p50":1.4,"wait_p90":2,"builds_per_landing":0.706875,"unblamed_red_candidates":0,"candidates_without_a_culprit":5,"culprits_unattributed":0,"departures_after_an_unconfirmed_plan":0,"invariant_failures":0,"waiting_at_open":0,"settings_in_window":1,"acts":{"checks":8,"conflict":0,"ejected":0,"head":8,"hold":8,"label":8,"settings":1,"unlabel":0,"wait":8},"unattributed_holds":0,"wait_p50_minutes":14,"wait_p90_minutes":20,"whole_builds":5.655,"probes":0,"red_builds":0,"queue_added_p50_minutes":4,"queue_added_p90_minutes":8,"queue_added_over_30":0,"red_candidates":0,"red_pull_requests":0,"ejected_as_cause":0,"left_waiting":0}}`

func TestReplayLeavesTheSimulatedHalfOfACleanDayAsItWas(t *testing.T) {
	out, err := captureLiveRun(t, writeLive(t, cleanDay()), "hey2", replayStart.Format(time.RFC3339), replayStart.Add(160*time.Minute).Format(time.RFC3339), true)
	if err != nil {
		t.Fatal(err)
	}
	var row, was struct {
		Simulated map[string]any `json:"simulated"`
		OverLive  map[string]any `json:"over_live"`
	}
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(cleanDaySimulated), &was); err != nil {
		t.Fatal(err)
	}
	if row.Simulated["unejected_guilty_heads"] != 0.0 {
		t.Fatalf("unejected guilty heads %v", row.Simulated["unejected_guilty_heads"])
	}
	delete(row.Simulated, "unejected_guilty_heads")
	if !reflect.DeepEqual(row, was) {
		t.Fatalf("simulated half changed:\n%v\n%v", row, was)
	}
}

func TestReplayRowCountsTheGuiltyHeadsTheRecordDidNotEject(t *testing.T) {
	for _, tc := range []struct {
		name string
		day  []queue.Recording
		want int
	}{
		{"one left unejected", guiltDay(), 1},
		{"only an ejected culprit", plannedDay(threeQueued(3), dials(1, 3, false), 70, func(*Sim) {}), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, _ := replayWindow(t, tc.day, 80)
			if _, sim := w.simulate(Shape{}, was); sim.UnejectedGuiltyHeads != tc.want || !heldGuilty(w, 3, "head3") {
				t.Fatalf("unejected guilty heads %d, head3 held guilty %v", sim.UnejectedGuiltyHeads, heldGuilty(w, 3, "head3"))
			}
		})
	}
	was, w, _ := recordedWindow(t, guiltDay(), replayStart.Add(40*time.Minute), replayStart.Add(80*time.Minute))
	if _, sim := w.simulate(Shape{}, was); sim.UnejectedGuiltyHeads != 1 || !slices.Equal(w.failing["head3"], []time.Duration{10 * time.Minute, 10 * time.Minute}) {
		t.Fatalf("a window whose lead-in plays head3 counts %d unejected guilty heads, failing in %v", sim.UnejectedGuiltyHeads, w.failing["head3"])
	}
	if w := leadInGuiltDay(t); w.unejectedGuilty != 1 || !slices.Equal(slices.Sorted(slices.Values(w.failing["head3"])), []time.Duration{6 * time.Minute, 8 * time.Minute}) {
		t.Fatalf("a lead-in whose guilty head fails faster than its green builds counts %d unejected guilty heads, failing in %v", w.unejectedGuilty, w.failing["head3"])
	}
	was, w, _ = recordedWindow(t, guiltDay(), replayStart.Add(leadIn+40*time.Minute), replayStart.Add(leadIn+80*time.Minute))
	if _, sim := w.simulate(Shape{}, was); sim.UnejectedGuiltyHeads != 0 {
		t.Fatalf("a replay opening after head3 left counts %d unejected guilty heads", sim.UnejectedGuiltyHeads)
	}
}

func TestReplayGuiltyHeadsBuildsDrawAmongItsFailingTimes(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	failing := []time.Duration{5 * time.Minute, 3 * time.Minute, 7 * time.Minute}
	s, _ := firstTreeHolding(t, w, 3, withTimes(twoShared, map[queue.SHA][]time.Duration{"head3": failing}))
	took := map[time.Duration]bool{}
	for i := range 8 {
		sha := queue.SHA(fmt.Sprintf("guilty%d", i))
		s.holds[sha] = map[int]bool{3: true}
		s.push(queue.Build{SHA: sha})
		took[s.builds[sha].end-s.now] = true
	}
	if len(took) < 2 || slices.ContainsFunc(slices.Collect(maps.Keys(took)), func(d time.Duration) bool { return !slices.Contains(failing, d) }) {
		t.Fatalf("eight trees #3 breaks took %v, want more than one of its recorded %v", slices.Sorted(maps.Keys(took)), failing)
	}
}

// withTimes is how long builds take in a replay of guiltDay: the shared sample, and the failing
// times of the heads that broke it.
func withTimes(shared []time.Duration, failing map[queue.SHA][]time.Duration) func(*Shape) {
	return func(shape *Shape) { shape.Times, shape.Failing = shared, failing }
}

var twoShared = []time.Duration{5 * time.Minute, 17 * time.Minute}

// leadInGuiltDay is a replay of the planner's own day on which #3 breaks the build of #1, #2 and
// #3 twice, in 6 and then 8 minutes, the round between them passing in 4, and its author pushes
// a clean head at 22m. Every later build takes 30, and the window opens at 30m.
func leadInGuiltDay(t *testing.T) window {
	shape := threeQueued(3)
	shape.Takes = slices.Repeat([]time.Duration{30 * time.Minute}, 40)
	shape.Takes[0], shape.Takes[1], shape.Takes[2], shape.Takes[3] = 6*time.Minute, 4*time.Minute, 4*time.Minute, 8*time.Minute
	recs := plannedDay(shape, dials(1, 3, false), 150, func(s *Sim) {
		if p := s.prs[3]; s.now == 22*time.Minute {
			p.Head, p.breaks = "fix3", false
			s.byHead[p.Head] = 3
		}
	})
	_, w, _ := recordedWindow(t, recs, replayStart.Add(30*time.Minute), replayStart.Add(150*time.Minute))
	return w
}

func TestReplayGuiltyHeadFailsInTheTimesItFailedInTheLeadIn(t *testing.T) {
	w := leadInGuiltDay(t)
	failing := slices.Sorted(slices.Values(w.failing["head3"]))
	if !heldGuilty(w, 3, "head3") || !slices.Equal(failing, []time.Duration{6 * time.Minute, 8 * time.Minute}) || len(w.failing) != 1 {
		t.Fatalf("guilty %v, failing times %v", heldGuilty(w, 3, "head3"), w.failing)
	}
	if !slices.Equal(w.times, []time.Duration{30 * time.Minute, 30 * time.Minute}) {
		t.Fatalf("shared sample %v, want the window's two builds of 30m", w.times)
	}
	if s, sha := firstTreeHolding(t, w, 3, func(*Shape) {}); s.now >= 0 || !slices.Contains(failing, s.builds[sha].end-s.now) {
		t.Fatalf("the tree #3 breaks, pushed at %v, took %v, want one of %v", s.now, s.builds[sha].end-s.now, failing)
	}
}

func TestReplayGuiltyHeadFailsInItsOwnRecordedTime(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	s, sha := firstTreeHolding(t, w, 3, withTimes(twoShared, map[queue.SHA][]time.Duration{"head3": {3 * time.Minute}}))
	if took := s.builds[sha].end - s.now; took != 3*time.Minute {
		t.Fatalf("the tree #3 breaks took %v, want its recorded 3m", took)
	}
}

func TestReplayTreeOfTwoGuiltyHeadsFailsInTheShorterTime(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	for i, a := range w.acts {
		if a.Do == "head" && a.Number == 2 {
			w.acts[i].Breaks = true
		}
	}
	for _, failing := range []map[queue.SHA][]time.Duration{
		{"head2": {3 * time.Minute}, "head3": {8 * time.Minute}},
		{"head2": {8 * time.Minute}, "head3": {3 * time.Minute}},
	} {
		s, sha := firstTreeHolding(t, w, 3, withTimes(twoShared, failing))
		if !s.holds[sha][2] {
			t.Fatalf("the tree does not hold #2: %v", s.holds[sha])
		}
		if took := s.builds[sha].end - s.now; took != 3*time.Minute {
			t.Fatalf("the tree #2 and #3 break took %v with %v, want the shorter 3m", took, failing)
		}
	}
}

func TestReplayBuildKeepsTheGuiltOfTheHeadsItWasPushedWith(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	s, sha := firstTreeHolding(t, w, 3, func(*Shape) {})
	s.act(Act{At: s.now, Number: 3, Do: "head", Head: "clean3"})
	s.plan = func(queue.World, queue.Config) []queue.Action { return nil }
	for s.now < s.builds[sha].end {
		s.Step()
	}
	if b := s.builds[sha]; b.Verdict != queue.Failure || b.flake || b.errored || s.Red != 1 {
		t.Fatalf("the tree pushed with head3 read %d after #3 moved to a clean head (flake %v, errored %v, red builds %d)", b.Verdict, b.flake, b.errored, s.Red)
	}
}

func TestReplayProbeOfAGuiltyHeadKeepsTheProbeTime(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	s, sha := firstTreeHolding(t, w, 3, withTimes(twoShared, map[queue.SHA][]time.Duration{"head3": {3 * time.Minute}}))
	s.holds["probe"] = s.holds[sha]
	s.push(queue.Build{SHA: "probe", Kind: queue.Probe})
	if took := s.builds["probe"].end - s.now; took != w.probe || took == 3*time.Minute || len(s.Broken) > 0 {
		t.Fatalf("the probe took %v, want the probe time %v: %v", took, w.probe, s.Broken)
	}
}

func TestReplayGuiltyHeadWithNoFailingTimeDrawsFromTheSharedSample(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	s, sha := firstTreeHolding(t, w, 3, withTimes(twoShared, map[queue.SHA][]time.Duration{"another": {3 * time.Minute}}))
	plain, same := firstTreeHolding(t, w, 3, withTimes(twoShared, nil))
	if took, want := s.builds[sha].end-s.now, plain.builds[same].end-plain.now; took != want || !slices.Contains(twoShared, took) {
		t.Fatalf("the tree #3 breaks took %v, want the shared sample's %v", took, want)
	}
}

// cleanDraws pushes trees of #1 alone after the replay's first tick, and gives how long each
// takes.
func cleanDraws(s *Sim) []time.Duration {
	var took []time.Duration
	for i := range 8 {
		sha := queue.SHA(fmt.Sprintf("clean%d", i))
		s.holds[sha] = map[int]bool{1: true}
		s.push(queue.Build{SHA: sha})
		took = append(took, s.builds[sha].end-s.now)
	}
	return took
}

func TestReplayGuiltyBuildLeavesTheSharedDrawsAsTheyWere(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	guilty, _ := firstTreeHolding(t, w, 3, withTimes(twoShared, map[queue.SHA][]time.Duration{"head3": {3 * time.Minute}}))
	plain, _ := firstTreeHolding(t, w, 3, withTimes(twoShared, nil))
	if after, was := cleanDraws(guilty), cleanDraws(plain); !slices.Equal(after, was) {
		t.Fatalf("shared draws after a guilty build %v, without one %v", after, was)
	}
}

func TestReplayTakesNothingForFailingTimesWhereNoneIsRecorded(t *testing.T) {
	_, w, _ := replayWindow(t, guiltDay(), 80)
	s, sha := firstTreeHolding(t, w, 3, withTimes(twoShared, nil))
	if allocs := testing.AllocsPerRun(10, func() { s.failing(sha) }); allocs != 0 {
		t.Fatalf("failing with no failing times allocated %v times", allocs)
	}
}

// The live half of guiltDay's row as the replay before it held guilty a head that left
// unejected; only the commit the row names differs.
const guiltDayLive = `{"arrivals":4,"backlog_longest_minutes":8,"build_median_minutes":10,"building_candidates_dropped":0,"builds_per_landing":4,"busiest_hour_landings":3,"busy_minutes":50,"context_ticks":0,"departures_after_an_unconfirmed_plan":0,"dropped_head_moved":0,"dropped_longest_built_minutes":0,"dropped_member_left":0,"dropped_members_still_queued":0,"ejected_p90":null,"ejected_p90_minutes":null,"ejections_as_cause":0,"first_tick":"2026-11-18T12:02:00Z","from":"2026-11-18T12:00:00Z","good_builds":8,"labelled_p50":4,"labelled_p50_minutes":40,"labelled_p90":5,"labelled_p90_minutes":50,"labelled_slowest_20th":5,"labelled_slowest_20th_minutes":50,"landed":3,"landing_build_median_minutes":10,"landings_per_hour":3.5999999999999996,"last_tick":"2026-11-18T13:10:00Z","left_waiting":0,"longest_ready_gap_from":"2026-11-18T12:02:00Z","longest_ready_gap_minutes":30,"longest_ready_gap_to":"2026-11-18T12:32:00Z","non_passing_builds":0,"passing_candidates_thrown_away":0,"queue_added_over_30":0,"queue_added_over_30_sum_minutes":0,"queue_added_p50_minutes":20,"queue_added_p90_minutes":30,"queue_added_p95_minutes":30,"queue_added_sum_minutes":58,"ready_gaps":3,"ready_gaps_over_90":0,"red_candidates":1,"red_pull_requests":3,"red_read_median_minutes":null,"red_read_n":0,"red_read_sum_minutes":null,"settings_in_window":1,"split_landings":3,"split_ready_to_landed_p50_minutes":30,"split_ready_to_landed_p90_minutes":40,"thrown_away_head_moved":0,"thrown_away_member_left":0,"ticks":46,"to":"2026-11-18T13:20:00Z","unchecked":0,"unconfirmed_landing_plans":0,"unplanned_trunk_moves":0,"unreadable":0,"unsplit_landings":0,"wait_p50":3,"wait_p50_minutes":30,"wait_p90":4,"wait_p90_minutes":40,"waiting_at_open":0,"whole_builds":12,"withdrawn":0,"wrong_ejections":0,"wrong_per_1000":0}`

func TestReplayLeavesTheLiveHalfOfTheRowAsItWas(t *testing.T) {
	out, err := captureLiveRun(t, writeLive(t, guiltDay()), "hey2", replayStart.Format(time.RFC3339), replayStart.Add(80*time.Minute).Format(time.RFC3339), true)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &row); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"simulated", "over_live", "commit"} {
		delete(row, key)
	}
	if live, _ := json.Marshal(row); string(live) != guiltDayLive {
		t.Fatalf("live half changed:\n%s\n%s", live, guiltDayLive)
	}
}
