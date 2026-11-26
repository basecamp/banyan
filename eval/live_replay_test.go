package main

import (
	"encoding/json"
	"fmt"
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
	red := queue.Build{SHA: "red", Pushed: replayStart, Chain: []queue.Member{{Number: 1, Head: p.Head, Merge: "p"}, {Number: 2, Head: q.Head, Merge: "red"}}}
	green := queue.Build{SHA: "green", Pushed: replayStart.Add(40 * time.Minute), Verdict: queue.Success, Chain: []queue.Member{{Number: 2, Head: q.Head, Merge: "green"}}}
	recs := []queue.Recording{replayTick(0, p, q), replayTick(32, p, q), replayTick(40, next, q), replayTick(72, next, q), replayTick(73, next)}
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
	for _, tc := range []struct {
		name     string
		from, to int
		landed   float64
		red      float64
		added    *float64
	}{
		{"whole window", 0, 80, 1, 1, number(40)},
		{"opens during red build", 33, 80, 0, 0, nil},
		{"landing after window", 0, 70, 0, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, row := recordedWindow(t, recs, replayStart.Add(time.Duration(tc.from)*time.Minute), replayStart.Add(time.Duration(tc.to)*time.Minute))
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
			if tc.from == 0 && (!reflect.DeepEqual(sim.QueueAddedP90Minutes, row.QueueAddedP90Minutes) || sim.QueueAddedOver30 != float64(row.QueueAddedOver30) || sim.RedCandidates != float64(row.RedCandidates) || sim.RedPullRequests != float64(row.RedPullRequests)) {
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
func TestReplayOpeningWaitAndBuild(t *testing.T) {
	p := replayPR(1)
	p.Queued = replayStart.Add(-60 * time.Minute)
	b := queue.Build{SHA: "b", Pushed: p.Queued, Chain: []queue.Member{{Number: 1, Head: p.Head, Merge: "b"}}}
	recs := []queue.Recording{replayTick(-60, p), replayTick(12, p), replayTick(13)}
	recs[0].Actions = []queue.Action{{Do: "push", Build: b}}
	b.Verdict = queue.Success
	recs[1].World.Builds = []queue.Build{b}
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}}
	recs[2].World.Trunk = "b"
	_, w, row := replayWindow(t, recs, 30)
	s := NewSim(Shape{Recorded: true, Acts: w.acts, Build: w.build}, w.cfg, 1)
	s.play()
	if len(s.prs) != 1 || s.prs[1].ready != -time.Hour || !s.prs[1].queued || len(s.builds) != 0 {
		t.Fatalf("opening snapshot: pr=%+v builds=%+v", s.prs[1], s.builds)
	}
	_, sim := w.simulate(Shape{}, Score{})
	if *row.WaitP50Minutes != 72 || sim.WaitP50Minutes != nil || sim.WholeBuilds != 1 || *sim.Landed != 0 || *row.BuildsPerLanding != 0 || sim.WaitingAtOpen != 1 {
		t.Fatalf("opening: live %+v sim %+v / %+v", row, sim, sim.simulatedMeasures)
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

// breakersDay has a candidate of #1 and #2 building at the open that lands them, then #3,
// whose head d fails two candidates in 8 and 6 minutes, no build passes and nothing ejects,
// until its author pushes a new head that lands.
func breakersDay() []queue.Recording {
	p, q, r := replayPR(1), replayPR(2), replayPR(3)
	for _, pr := range []*queue.PR{&p, &q, &r} {
		pr.Queued = replayStart.Add(-time.Hour)
	}
	fixed := r
	fixed.Head = "fix"
	build := func(sha, base queue.SHA, pushed int, chain ...queue.Member) queue.Build {
		return queue.Build{SHA: sha, Base: base, Pushed: replayStart.Add(time.Duration(pushed) * time.Minute), Chain: chain}
	}
	answered := func(b queue.Build, v queue.Verdict) []queue.Build { b.Verdict = v; return []queue.Build{b} }
	open := build("A", "t", -5, queue.Member{Number: 1, Head: p.Head, Merge: "m1"}, queue.Member{Number: 2, Head: q.Head, Merge: "A"})
	first := build("F1", "A", 11, queue.Member{Number: 3, Head: r.Head, Merge: "F1"})
	second := build("F2", "A", 19, queue.Member{Number: 3, Head: r.Head, Merge: "F2"})
	mended := build("G", "A", 30, queue.Member{Number: 3, Head: fixed.Head, Merge: "G"})
	recs := []queue.Recording{
		replayTick(-5, p, q, r), replayTick(10, p, q, r), replayTick(11, r), replayTick(19, r),
		replayTick(25, r), replayTick(30, fixed), replayTick(48, fixed), replayTick(49),
	}
	for i := range recs {
		recs[i].Config = dials(2, 2, false)
		if i >= 2 {
			recs[i].World.Trunk = "A"
		}
	}
	recs[0].Actions = []queue.Action{{Do: "push", Build: open}}
	recs[1].World.Builds = answered(open, queue.Success)
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "A", Members: open.Chain}}
	recs[2].Actions = []queue.Action{{Do: "push", Build: first}}
	recs[3].World.Builds = answered(first, queue.Failure)
	recs[3].Actions = []queue.Action{{Do: "push", Build: second}}
	recs[4].World.Builds = answered(second, queue.Failure)
	recs[5].Actions = []queue.Action{{Do: "push", Build: mended}}
	recs[6].World.Builds = answered(mended, queue.Success)
	recs[6].Actions = []queue.Action{{Do: "land", SHA: "G", Members: mended.Chain}}
	recs[7].World.Trunk = "G"
	return recs
}

func heldGuilty(w window, n int, head queue.SHA) bool {
	return slices.ContainsFunc(w.acts, func(a Act) bool { return a.Do == "head" && a.Number == n && a.Head == head && a.Breaks })
}

func TestReplayHoldsGuiltyAHeadThatFailedTwiceNeverPassedAndLeft(t *testing.T) {
	_, w, _ := replayWindow(t, breakersDay(), 60)
	if !heldGuilty(w, 3, "d") || heldGuilty(w, 3, "fix") || heldGuilty(w, 1, "b") {
		t.Fatalf("guilt: d %v, fix %v, b %v", heldGuilty(w, 3, "d"), heldGuilty(w, 3, "fix"), heldGuilty(w, 1, "b"))
	}
	if w.unblamedRedCandidates != 0 || w.candidatesWithoutCulprit != 2 || w.flake != 0 {
		t.Fatalf("unblamed red candidates %d of %d, flake %g", w.unblamedRedCandidates, w.candidatesWithoutCulprit, w.flake)
	}
}

func TestReplayHoldsNoHeadGuiltyThatTheRecordClears(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(recs []queue.Recording)
	}{
		{"a whole build passed it", func(recs []queue.Recording) {
			recs[4].World.Builds = append(recs[4].World.Builds, queue.Build{SHA: "C", Kind: queue.Check, Base: "A", Verdict: queue.Success, Chain: []queue.Member{{Number: 3, Head: "d", Merge: "C"}}})
		}},
		{"it failed once", func(recs []queue.Recording) {
			recs[3].Actions, recs[4].World.Builds = nil, nil
		}},
		{"it failed once and in a probe", func(recs []queue.Recording) {
			recs[3].Actions = nil
			recs[4].World.Builds = []queue.Build{{SHA: "P", Kind: queue.Probe, Base: "A", Verdict: queue.Failure, Chain: []queue.Member{{Number: 3, Head: "d", Merge: "P"}}}}
		}},
		{"an ejected culprit failed beside it", func(recs []queue.Recording) {
			for _, r := range recs[3:5] {
				r.World.Builds[0].Chain = append([]queue.Member{{Number: 4, Head: "e", Merge: "m4"}}, r.World.Builds[0].Chain...)
			}
			recs[4].Actions = []queue.Action{{Do: "eject", PR: 4, Cause: "culprit", Evidence: []queue.SHA{"F1", "F2"}}}
		}},
		{"it landed", func(recs []queue.Recording) {
			recs[4].Actions = []queue.Action{{Do: "land", SHA: "L", Members: []queue.Member{{Number: 3, Head: "d"}}}}
			recs[5].World.Trunk = "L"
		}},
		{"it was still queued at the last record", func(recs []queue.Recording) {
			for i := 5; i < len(recs); i++ {
				recs[i].World.PRs = recs[4].World.PRs
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := breakersDay()
			tc.change(recs)
			_, w, _ := replayWindow(t, recs, 60)
			if heldGuilty(w, 3, "d") {
				t.Fatal("head d held guilty")
			}
		})
	}
}

// firstTreeHolding opens the replay, lets mark change the simulated queue, runs the planner's
// first tick, and gives the first tree it pushed that holds this pull request.
func firstTreeHolding(t *testing.T, w window, n int, mark func(*Sim)) (*Sim, queue.SHA) {
	t.Helper()
	s := NewSim(replayShape(w), w.cfg, 1)
	s.play()
	mark(s)
	s.tick()
	for _, sha := range s.shas() {
		if b := s.builds[sha]; b.start >= 0 && s.holds[sha][n] {
			return s, sha
		}
	}
	t.Fatal("no build holds the pull request")
	return nil, ""
}

func firstBuildHolding(t *testing.T, w window, n int) time.Duration {
	t.Helper()
	s, sha := firstTreeHolding(t, w, n, func(*Sim) {})
	return s.builds[sha].end - s.now
}

// ejectedOnItsFirstFailure is breakersDay with #3 ejected on F1, so its one failing time is 8m.
func ejectedOnItsFirstFailure() []queue.Recording {
	recs := breakersDay()
	recs[3].Actions, recs[4].World.Builds = []queue.Action{{Do: "eject", PR: 3, Cause: "culprit", Evidence: []queue.SHA{"F1"}}}, nil
	return recs
}

func TestReplayGuiltyHeadFailsInItsOwnRecordedTime(t *testing.T) {
	_, w, _ := replayWindow(t, ejectedOnItsFirstFailure(), 60)
	if slices.Sort(w.times); !slices.Equal(w.times, []time.Duration{15 * time.Minute, 18 * time.Minute}) {
		t.Fatalf("shared sample %v", w.times)
	}
	if took := firstBuildHolding(t, w, 3); took != 8*time.Minute {
		t.Fatalf("the tree #3 breaks took %v, want its recorded 8m", took)
	}
}

func TestReplayTreeOfTwoGuiltyHeadsFailsInTheShorterTime(t *testing.T) {
	_, w, _ := replayWindow(t, ejectedOnItsFirstFailure(), 60)
	s, sha := firstTreeHolding(t, w, 3, func(s *Sim) {
		s.prs[2].breaks = true
		s.shape.Failing = map[queue.SHA][]time.Duration{"d": {8 * time.Minute}, s.prs[2].Head: {3 * time.Minute}}
	})
	if !s.holds[sha][2] {
		t.Fatalf("the tree does not hold #2: %v", s.holds[sha])
	}
	if took := s.builds[sha].end - s.now; took != 3*time.Minute {
		t.Fatalf("the tree #2 and #3 break took %v, want the shorter 3m", took)
	}
}

func TestReplayProbeOfAGuiltyHeadKeepsTheProbeTime(t *testing.T) {
	_, w, _ := replayWindow(t, ejectedOnItsFirstFailure(), 60)
	s, sha := firstTreeHolding(t, w, 3, func(*Sim) {})
	s.holds["probe"] = s.holds[sha]
	s.push(queue.Build{SHA: "probe", Kind: queue.Probe})
	if took := s.builds["probe"].end - s.now; took != w.probe || took == 8*time.Minute || len(s.Broken) > 0 {
		t.Fatalf("the probe took %v, want the probe time %v: %v", took, w.probe, s.Broken)
	}
}

func TestReplayGuiltyHeadsFailingTimesLeaveTheSharedSample(t *testing.T) {
	_, w, _ := replayWindow(t, breakersDay(), 60)
	if slices.Sort(w.times); !slices.Equal(w.times, []time.Duration{15 * time.Minute, 18 * time.Minute}) {
		t.Fatalf("shared sample %v", w.times)
	}
	if took := firstBuildHolding(t, w, 3); took != 6*time.Minute && took != 8*time.Minute {
		t.Fatalf("the tree #3 breaks took %v, want one of its recorded 8m and 6m", took)
	}
}

func TestReplayGuiltyHeadWithNoFailingTimeDrawsFromTheSharedSample(t *testing.T) {
	recs := breakersDay()
	probe := queue.Build{SHA: "P", Kind: queue.Probe, Base: "A", Pushed: recs[2].World.Now, Verdict: queue.Failure, Chain: []queue.Member{{Number: 3, Head: "d", Merge: "P"}}}
	recs[2].Actions, recs[3].World.Builds = nil, []queue.Build{probe}
	recs[3].Actions, recs[4].World.Builds = []queue.Action{{Do: "eject", PR: 3, Cause: "culprit", Evidence: []queue.SHA{"P"}}}, nil
	recs[5].Actions, recs[6].World.Builds, recs[6].Actions, recs[7].World.Trunk = nil, nil, nil, "A"
	_, w, _ := replayWindow(t, recs, 60)
	if !heldGuilty(w, 3, "d") || !slices.Equal(w.times, []time.Duration{15 * time.Minute}) {
		t.Fatalf("guilty %v, shared sample %v", heldGuilty(w, 3, "d"), w.times)
	}
	if took := firstBuildHolding(t, w, 3); took != 15*time.Minute {
		t.Fatalf("the tree #3 breaks took %v, want the shared 15m", took)
	}
}

// The live half of breakersDay's row as the replay before it carried running candidates, held
// guilty a head that left unejected, or timed a guilty head's builds by its own.
const breakersDayLive = `{"arrivals":4,"backlog_longest_minutes":0,"build_median_minutes":15,"building_candidates_dropped":0,"builds_per_landing":1,"busiest_hour_landings":3,"busy_minutes":49,"context_ticks":1,"departures_after_an_unconfirmed_plan":0,"dropped_head_moved":0,"dropped_longest_built_minutes":0,"dropped_member_left":0,"dropped_members_still_queued":0,"ejected_p90":null,"ejected_p90_minutes":null,"ejections_as_cause":0,"first_tick":"2026-11-18T12:10:00Z","from":"2026-11-18T12:00:00Z","good_builds":2,"labelled_p50":4.666666666666667,"labelled_p50_minutes":70,"labelled_p90":7.2,"labelled_p90_minutes":108,"labelled_slowest_20th":7.2,"labelled_slowest_20th_minutes":108,"landed":3,"landing_build_median_minutes":15,"landings_per_hour":3.673469387755102,"last_tick":"2026-11-18T12:49:00Z","left_waiting":0,"longest_ready_gap_from":"2026-11-18T12:10:00Z","longest_ready_gap_minutes":38,"longest_ready_gap_to":"2026-11-18T12:48:00Z","non_passing_builds":0,"passing_candidates_thrown_away":0,"queue_added_over_30":2,"queue_added_over_30_sum_minutes":110,"queue_added_p50_minutes":55,"queue_added_p90_minutes":55,"queue_added_p95_minutes":55,"queue_added_sum_minutes":110,"ready_gaps":2,"ready_gaps_over_90":0,"red_candidates":2,"red_pull_requests":2,"red_read_median_minutes":null,"red_read_n":0,"red_read_sum_minutes":null,"settings_in_window":1,"split_landings":3,"split_ready_to_landed_p50_minutes":70,"split_ready_to_landed_p90_minutes":70,"thrown_away_head_moved":0,"thrown_away_member_left":0,"ticks":7,"to":"2026-11-18T13:00:00Z","unchecked":0,"unconfirmed_landing_plans":0,"unplanned_trunk_moves":0,"unreadable":0,"unsplit_landings":0,"wait_p50":4.666666666666667,"wait_p50_minutes":70,"wait_p90":4.666666666666667,"wait_p90_minutes":70,"waiting_at_open":3,"whole_builds":3,"withdrawn":0,"wrong_ejections":0,"wrong_per_1000":0}`

func TestReplayLeavesTheLiveHalfOfTheRowAsItWas(t *testing.T) {
	out, err := captureLiveRun(t, writeLive(t, breakersDay()), "hey2", replayStart.Format(time.RFC3339), replayStart.Add(time.Hour).Format(time.RFC3339), true)
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
	if live, _ := json.Marshal(row); string(live) != breakersDayLive {
		t.Fatalf("live half changed:\n%s\n%s", live, breakersDayLive)
	}
}
