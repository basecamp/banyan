package main

import (
	"encoding/json"
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
	s := NewSim(Shape{Recorded: true, Build: build, Probe: build, Flake: flake, Nearby: tenTwentyThirty}, dials(1, 1, false), 1)
	s.now = at
	s.push(queue.Build{SHA: "pushed", Kind: kind})
	return s.builds["pushed"].flake
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

// flakeDay is a window of an hour from minute from, with a green candidate pushed as it opens, and
// a red candidate pushed at minute red where red is above zero.
func flakeDay(red, from int) []queue.Recording {
	at := func(minutes int) time.Time { return replayStart.Add(time.Duration(minutes) * time.Minute) }
	tick := func(minutes int, builds ...queue.Build) queue.Recording {
		r := replayTick(minutes, replayPR(1))
		r.World.Builds = builds
		return r
	}
	candidate := func(sha queue.SHA, pushed int, v queue.Verdict) queue.Build {
		return queue.Build{SHA: sha, Pushed: at(pushed), Verdict: v, Chain: []queue.Member{{Number: 1, Head: "sound", Merge: sha}}}
	}
	recs := []queue.Recording{tick(0)}
	if red > 0 {
		recs = append(recs, tick(red, candidate("red", red, queue.Pending)), tick(red+10, candidate("red", red, queue.Failure)))
	}
	return append(recs, tick(from-60), tick(from, candidate("green", from, queue.Pending)), tick(from+10, candidate("green", from, queue.Success)), tick(from+60))
}

func TestReplayPlaysAFlakeWhereARedCandidateIsNearTheHoursItRuns(t *testing.T) {
	for _, tc := range []struct {
		name      string
		red, from int
		want      bool
	}{
		{"a red in the lead-in", 90, 300, true},
		{"greens only", 0, 300, false},
		{"a red a day away", 90, 300 + 24*60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := replayStart.Add(time.Duration(tc.from) * time.Minute)
			_, w, _ := recordedWindow(t, flakeDay(tc.red, tc.from), from, from.Add(time.Hour))
			s := NewSim(Shape{Recorded: true, Flake: w.flake, NearbyRed: w.nearbyRed}, w.cfg, 1)
			if w.flake != 0 || len(w.nearby) != 1+btoi(tc.red > 0) || s.flakes() != tc.want {
				t.Fatalf("day's share %g, nearby %v, a flake in play %v", w.flake, w.nearby, s.flakes())
			}
		})
	}
}

func TestReplayOfADayWithNoRedCandidateIsUnchanged(t *testing.T) {
	recs := durationBuild([]queue.Recording{replayTick(0, replayPR(1), replayPR(2))}, 10)
	second := queue.Build{SHA: "second", Pushed: replayStart.Add(20 * time.Minute), Verdict: queue.Success}
	errored := queue.Build{SHA: "errored", Kind: queue.Check, Pushed: replayStart.Add(22 * time.Minute), Verdict: queue.Error}
	recs = append(recs, recordAt(recs, 32, second), recordAt(recs, 34, errored), replayTick(60))
	was, w, _ := replayWindow(t, recs, 60)
	_, sim := w.simulate(Shape{}, was)
	row, _ := json.Marshal(sim)
	if want := `{"landed":1.93,"wait_p50":1.8333333333333333,"wait_p90":3.6666666666666665,"builds_per_landing":1.5129533678756477,"unblamed_red_candidates":0,"candidates_without_a_culprit":2,"culprits_unattributed":0,"unejected_guilty_heads":0,"red_trunk_spans":0,"unfixed_red_trunk_spans":0,"unplayed_red_trunk_spans":0,"unplayed_red_trunk_minutes":0,"departures_after_an_unconfirmed_plan":0,"invariant_failures":0,"waiting_at_open":2,"settings_in_window":1,"acts":{"checks":2,"conflict":0,"ejected":0,"head":2,"hold":2,"label":2,"settings":1,"unlabel":0,"wait":2},"unattributed_holds":0,"wait_p50_minutes":22,"wait_p90_minutes":44,"whole_builds":2.92,"probes":0,"red_builds":0,"queue_added_p50_minutes":12,"queue_added_p90_minutes":32,"queue_added_over_30":0.23,"red_candidates":0,"red_pull_requests":0,"ejected_as_cause":0,"left_waiting":0.07}`; string(row) != want {
		t.Fatalf("row %s", row)
	}
}
