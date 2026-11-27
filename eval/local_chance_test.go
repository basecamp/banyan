package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// tenTwentyThirty is a recorded window's candidates: red ones pushed at 10m and 20m, a green one
// at 30m.
var tenTwentyThirty = []answeredCandidate{{10 * time.Minute, true}, {20 * time.Minute, true}, {30 * time.Minute, false}}

func TestReplayTakesTheRedShareOfTheCandidatesPushedWithinABuildsTime(t *testing.T) {
	for _, tc := range []struct {
		at, d   time.Duration
		share   float64
		present bool
	}{
		{15 * time.Minute, 10 * time.Minute, 1, true},
		{15 * time.Minute, 20 * time.Minute, 2.0 / 3, true},
		{15 * time.Minute, 4 * time.Minute, 0, false},
		{40 * time.Minute, 10 * time.Minute, 0, true},
		{0, 10 * time.Minute, 1, true},
	} {
		if share, present := nearbyShare(tenTwentyThirty, tc.at, tc.d); share != tc.share || present != tc.present {
			t.Errorf("pushed at %v taking %v: %g, %v; want %g, %v", tc.at, tc.d, share, present, tc.share, tc.present)
		}
	}
}

// pushedAt is whether a build of that kind, pushed at that time into a recorded replay of
// tenTwentyThirty whose builds take build, fails though its tree is good.
func pushedAt(kind queue.Kind, at, build time.Duration, flake float64) bool {
	return pushedInto(Shape{Build: build, Probe: build, Flake: flake, Nearby: tenTwentyThirty}, 1, kind, at).builds["pushed0"].flake
}

func TestReplayFailsACandidateOrCheckByTheCandidatesPushedNearIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      queue.Kind
		at, build time.Duration
		flake     float64
		want      bool
	}{
		{"a candidate among reds", queue.Candidate, 15 * time.Minute, 10 * time.Minute, 0, true},
		{"a check among reds", queue.Check, 15 * time.Minute, 10 * time.Minute, 0, true},
		{"a candidate among greens", queue.Candidate, 35 * time.Minute, 5 * time.Minute, 1, false},
		{"a candidate with none near, by the day's share", queue.Candidate, 50 * time.Minute, 4 * time.Minute, 1, true},
		{"a probe among reds", queue.Probe, 15 * time.Minute, 10 * time.Minute, 0, false},
	} {
		if got := pushedAt(tc.kind, tc.at, tc.build, tc.flake); got != tc.want {
			t.Errorf("%s: flaked %v, want %v", tc.name, got, tc.want)
		}
	}
}

// nearbyDay is a window from 5h to 6h with one build of each kind pushed in it, at their minutes
// past 5h, and a red candidate answered in the lead-in. Every build but the blamed one holds a
// head that passed.
func nearbyDay() []queue.Recording {
	p := replayPR(1)
	at := func(minutes int) time.Time { return replayStart.Add(time.Duration(minutes) * time.Minute) }
	sound := []queue.Member{{Number: 1, Head: "sound", Merge: "sound"}}
	build := func(sha queue.SHA, kind queue.Kind, pushed int, v queue.Verdict, chain []queue.Member) queue.Build {
		return queue.Build{SHA: sha, Kind: kind, Pushed: at(pushed), Verdict: v, Chain: chain}
	}
	tick := func(minutes int, builds ...queue.Build) queue.Recording {
		r := replayTick(minutes, p)
		r.World.Builds = builds
		return r
	}
	blamed := build("blamed", queue.Candidate, 318, queue.Failure, []queue.Member{{Number: 1, Head: p.Head, Merge: "blamed"}})
	recs := []queue.Recording{
		tick(60),
		tick(90, build("lead", queue.Candidate, 90, queue.Pending, sound)),
		tick(100, build("lead", queue.Candidate, 90, queue.Failure, sound)),
		tick(290),
		tick(300, build("pending", queue.Candidate, 300, queue.Pending, sound)),
		tick(305),
		tick(310, build("green", queue.Candidate, 302, queue.Success, sound)),
		tick(312, build("errored", queue.Candidate, 304, queue.Error, sound)),
		tick(314, build("check", queue.Check, 306, queue.Failure, sound)),
		tick(316, build("probe", queue.Probe, 308, queue.Failure, sound)),
		tick(320, blamed),
		tick(330, build("red", queue.Candidate, 312, queue.Failure, sound)),
		tick(360),
	}
	recs[10].Actions = []queue.Action{{Do: "eject", PR: 1, Cause: "culprit", Evidence: []queue.SHA{"blamed"}}}
	return recs
}

func TestReplayDrawsItsChanceFromTheAnsweredCandidatesWithNoCulprit(t *testing.T) {
	_, w, _ := recordedWindow(t, nearbyDay(), replayStart.Add(5*time.Hour), replayStart.Add(6*time.Hour))
	want := []answeredCandidate{{-210 * time.Minute, true}, {2 * time.Minute, false}, {12 * time.Minute, true}}
	got := map[answeredCandidate]bool{}
	for _, c := range w.nearby {
		got[c] = true
	}
	if len(w.nearby) != len(want) || !reflect.DeepEqual(got, map[answeredCandidate]bool{want[0]: true, want[1]: true, want[2]: true}) {
		t.Fatalf("nearby %v, want %v", w.nearby, want)
	}
	if w.unblamedRedCandidates != 1 || w.candidatesWithoutCulprit != 3 {
		t.Fatalf("the day's share: %d of %d", w.unblamedRedCandidates, w.candidatesWithoutCulprit)
	}
}

func TestReplayKeepsTheCandidatesOfARedTrunkNoFixEndsNearby(t *testing.T) {
	for _, tc := range []struct {
		name string
		day  func() []queue.Recording
		want bool
	}{
		{"no fix ends it", redTrunkDay, true},
		{"its fix ends it", fixedRedTrunkDay, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := tc.day()
			early := queue.Build{SHA: "early", Base: "T", Pushed: replayStart.Add(5 * time.Minute), Verdict: queue.Failure,
				Chain: []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: "early"}}}
			_, w, _ := replayWindow(t, sortedByTime(append(recs, recordAt(recs, 15, early))), 100)
			kept := false
			for _, c := range w.nearby {
				kept = kept || c == answeredCandidate{5 * time.Minute, true}
			}
			if kept != tc.want || len(w.nearby) != 2+btoi(tc.want) {
				t.Fatalf("nearby %v, the trunk's red candidate kept %v", w.nearby, kept)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pushedInto is a recorded replay of the shape with Recorded set, whose seed pushed one build of
// that kind at each time.
func pushedInto(shape Shape, seed int64, kind queue.Kind, at ...time.Duration) *Sim {
	shape.Recorded = true
	s := NewSim(shape, dials(1, 1, false), seed)
	for i, t := range at {
		s.now = t
		s.push(queue.Build{SHA: queue.SHA(fmt.Sprint("pushed", i)), Kind: kind})
	}
	return s
}

func TestReplayPlaysAFlakeOnceABuildTookAChanceAboveZeroFromTheSample(t *testing.T) {
	near := Shape{Build: 10 * time.Minute, Probe: 10 * time.Minute, Nearby: tenTwentyThirty}
	greens := Shape{Build: 5 * time.Minute, Nearby: tenTwentyThirty}
	theDays := Shape{Build: 10 * time.Minute, Flake: 0.1, Nearby: tenTwentyThirty}
	scripted := Shape{Build: 10 * time.Minute, Script: []bool{}, Nearby: tenTwentyThirty}
	for _, tc := range []struct {
		name  string
		shape Shape
		kind  queue.Kind
		at    []time.Duration
		want  bool
	}{
		{"before any build drew from it", near, queue.Candidate, nil, false},
		{"after a candidate drew among reds", near, queue.Candidate, []time.Duration{15 * time.Minute}, true},
		{"after a check drew among reds", near, queue.Check, []time.Duration{15 * time.Minute}, true},
		{"after a probe among reds", near, queue.Probe, []time.Duration{15 * time.Minute}, false},
		{"after candidates drew zero among greens", greens, queue.Candidate, []time.Duration{35 * time.Minute, 34 * time.Minute}, false},
		{"after one drew among reds and the next drew zero", greens, queue.Candidate, []time.Duration{15 * time.Minute, 35 * time.Minute}, true},
		{"after a candidate drew among reds in a scripted shape", scripted, queue.Candidate, []time.Duration{15 * time.Minute}, true},
		{"by the day's share before any build", theDays, queue.Candidate, nil, true},
	} {
		if got := pushedInto(tc.shape, 1, tc.kind, tc.at...).flakes(); got != tc.want {
			t.Errorf("%s: a flake in play %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReplayPlaysAFlakeOnceABuildTookAChanceFromTheSampleWhetherItFailedOrNot(t *testing.T) {
	failed := map[bool]int{}
	for seed := int64(1); seed <= 20; seed++ {
		s := pushedInto(Shape{Build: 20 * time.Minute, Nearby: tenTwentyThirty}, seed, queue.Candidate, 15*time.Minute)
		failed[s.builds["pushed0"].flake]++
		if !s.flakes() {
			t.Fatalf("seed %d: no flake in play after a build took 2 in 3", seed)
		}
	}
	if failed[true] == 0 || failed[false] == 0 {
		t.Fatalf("builds failed %v", failed)
	}
}

func TestReplayTakesTheShareWithinTheBuildsDrawnTime(t *testing.T) {
	s := pushedInto(Shape{Build: 5 * time.Minute, Times: []time.Duration{20 * time.Minute}, Nearby: []answeredCandidate{{20 * time.Minute, true}}}, 1, queue.Candidate, 0)
	if !s.builds["pushed0"].flake || !s.flakes() {
		t.Fatalf("a build drawn to take 20m beside a red at 20m: failed %v, a flake in play %v", s.builds["pushed0"].flake, s.flakes())
	}
}

// nearRedDay is the day of TestReplayOfADayWithNoRedCandidateIsUnchanged with one more candidate,
// pushed at 5m and read v at 70m, after the window, so the day's share leaves it out.
func nearRedDay(v queue.Verdict) []queue.Recording {
	recs := durationBuild([]queue.Recording{replayTick(0, replayPR(1), replayPR(2))}, 10)
	second := queue.Build{SHA: "second", Pushed: replayStart.Add(20 * time.Minute), Verdict: queue.Success}
	errored := queue.Build{SHA: "errored", Kind: queue.Check, Pushed: replayStart.Add(22 * time.Minute), Verdict: queue.Error}
	late := replayTick(70)
	late.World.Builds = []queue.Build{{SHA: "late", Pushed: replayStart.Add(5 * time.Minute), Verdict: v}}
	return append(recs, recordAt(recs, 32, second), recordAt(recs, 34, errored), replayTick(60), late)
}

func TestReplayOfADayFailsItsBuildsByARedCandidateNearThem(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    queue.Verdict
		want string
	}{
		{"red", queue.Failure, `{"landed":1.675,"wait_p50":3,"wait_p90":4.5,"builds_per_landing":4.534328358208955,"unblamed_red_candidates":0,"candidates_without_a_culprit":2,"culprits_unattributed":0,"unejected_guilty_heads":0,"red_trunk_spans":0,"unfixed_red_trunk_spans":0,"unplayed_red_trunk_spans":0,"unplayed_red_trunk_minutes":0,"departures_after_an_unconfirmed_plan":0,"invariant_failures":0,"waiting_at_open":2,"settings_in_window":1,"acts":{"checks":2,"conflict":0,"ejected":0,"head":2,"hold":2,"label":2,"settings":1,"unlabel":0,"wait":2},"unattributed_holds":0,"wait_p50_minutes":36,"wait_p90_minutes":54,"whole_builds":7.595,"probes":0,"red_builds":1.39,"queue_added_p50_minutes":24,"queue_added_p90_minutes":44,"queue_added_over_30":0.65,"red_candidates":0.83,"red_pull_requests":0.83,"ejected_as_cause":0.025,"left_waiting":0.325}`},
		{"green", queue.Success, quietDay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, _ := replayWindow(t, nearRedDay(tc.v), 60)
			_, sim := w.simulate(Shape{}, was)
			if row, _ := json.Marshal(sim); string(row) != tc.want || w.flake != 0 {
				t.Fatalf("day's share %g, row %s", w.flake, row)
			}
		})
	}
}

// quietDay is the simulated half of a recorded day with no red candidate anywhere in its records.
const quietDay = `{"landed":1.93,"wait_p50":1.8333333333333333,"wait_p90":3.6666666666666665,"builds_per_landing":1.5129533678756477,"unblamed_red_candidates":0,"candidates_without_a_culprit":2,"culprits_unattributed":0,"unejected_guilty_heads":0,"red_trunk_spans":0,"unfixed_red_trunk_spans":0,"unplayed_red_trunk_spans":0,"unplayed_red_trunk_minutes":0,"departures_after_an_unconfirmed_plan":0,"invariant_failures":0,"waiting_at_open":2,"settings_in_window":1,"acts":{"checks":2,"conflict":0,"ejected":0,"head":2,"hold":2,"label":2,"settings":1,"unlabel":0,"wait":2},"unattributed_holds":0,"wait_p50_minutes":22,"wait_p90_minutes":44,"whole_builds":2.92,"probes":0,"red_builds":0,"queue_added_p50_minutes":12,"queue_added_p90_minutes":32,"queue_added_over_30":0.23,"red_candidates":0,"red_pull_requests":0,"ejected_as_cause":0,"left_waiting":0.07}`

func TestReplayOfADayWithNoRedCandidateIsUnchanged(t *testing.T) {
	recs := durationBuild([]queue.Recording{replayTick(0, replayPR(1), replayPR(2))}, 10)
	second := queue.Build{SHA: "second", Pushed: replayStart.Add(20 * time.Minute), Verdict: queue.Success}
	errored := queue.Build{SHA: "errored", Kind: queue.Check, Pushed: replayStart.Add(22 * time.Minute), Verdict: queue.Error}
	recs = append(recs, recordAt(recs, 32, second), recordAt(recs, 34, errored), replayTick(60))
	was, w, _ := replayWindow(t, recs, 60)
	_, sim := w.simulate(Shape{}, was)
	row, _ := json.Marshal(sim)
	if string(row) != quietDay {
		t.Fatalf("row %s", row)
	}
}
