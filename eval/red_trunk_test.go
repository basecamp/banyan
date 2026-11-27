package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// redTrunkDay is a record of a trunk that fails by itself: #1 lands as T at 10m, probes of the
// bare trunk on T pushed at 20m and 35m are read red at 30m and 45m, and #2's candidate on T,
// pushed at 50m, is read green at 70m and lands. #3 and #4 wait throughout.
func redTrunkDay() []queue.Recording {
	p := []queue.PR{replayPR(1), replayPR(2), replayPR(3), replayPR(4)}
	at := func(minutes int) time.Time { return replayStart.Add(time.Duration(minutes) * time.Minute) }
	first := queue.Build{SHA: "T", Pushed: at(0), Chain: []queue.Member{{Number: 1, Head: p[0].Head, Merge: "T"}}}
	probe := func(sha queue.SHA, pushed int) queue.Build {
		return queue.Build{SHA: sha, Kind: queue.Probe, Base: "T", Round: 1, Of: "red", Pushed: at(pushed)}
	}
	fix := queue.Build{SHA: "g", Base: "T", Pushed: at(50), Chain: []queue.Member{{Number: 2, Head: p[1].Head, Merge: "g"}}}
	tick := func(minutes int, trunk queue.SHA, prs []queue.PR, builds ...queue.Build) queue.Recording {
		r := replayTick(minutes, prs...)
		r.World.Trunk, r.World.Builds, r.Config = trunk, builds, dials(1, 1, true)
		return r
	}
	green, red := func(b queue.Build) queue.Build { b.Verdict = queue.Success; return b }, func(b queue.Build) queue.Build { b.Verdict = queue.Failure; return b }
	first.Base = "t0"
	recs := []queue.Recording{
		tick(0, "t0", p),
		tick(10, "t0", p, green(first)),
		tick(12, "T", p[1:]),
		tick(20, "T", p[1:]),
		tick(30, "T", p[1:], red(probe("probe1", 20))),
		tick(35, "T", p[1:]),
		tick(45, "T", p[1:], red(probe("probe2", 35))),
		tick(50, "T", p[1:]),
		tick(60, "T", p[1:], fix),
		tick(70, "T", p[1:], green(fix)),
		tick(72, "g", p[2:]),
		tick(100, "g", p[2:]),
	}
	recs[0].Actions = []queue.Action{{Do: "push", Build: first}}
	recs[1].Actions = []queue.Action{{Do: "land", SHA: "T", Members: first.Chain}}
	recs[3].Actions = []queue.Action{{Do: "push", Build: probe("probe1", 20)}}
	recs[5].Actions = []queue.Action{{Do: "push", Build: probe("probe2", 35)}}
	recs[7].Actions = []queue.Action{{Do: "push", Build: fix}}
	recs[9].Actions = []queue.Action{{Do: "land", SHA: "g", Members: fix.Chain}}
	return recs
}

// foundSpan is a red trunk the records show, by its minutes from the replay's start.
type foundSpan struct {
	from, until time.Duration
	fixes       []int
}

func foundSpans(recs []queue.Recording) []foundSpan {
	spans, _ := redTrunks(recs)
	var found []foundSpan
	for _, span := range spans {
		found = append(found, foundSpan{span.from.Sub(replayStart), span.until.Sub(replayStart), span.fixes})
	}
	return found
}

func TestReplayPlaysNoEpisodeForARedTrunkNoFixEnds(t *testing.T) {
	if found, want := foundSpans(redTrunkDay()), []foundSpan{{10 * time.Minute, 50 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("spans %v, want %v", found, want)
	}
	was, w, _ := replayWindow(t, redTrunkDay(), 100)
	if _, sim := w.simulate(Shape{}, was); len(w.episodes) != 0 || sim.RedTrunkSpans != 0 {
		t.Fatalf("episodes %v, red trunk spans %d", w.episodes, sim.RedTrunkSpans)
	}
}

func TestReplayPlaysNoRedTrunkTheRecordsClear(t *testing.T) {
	for _, tc := range []struct {
		name string
		day  func() []queue.Recording
	}{
		{"one red probe, then a green one", func() []queue.Recording {
			recs := redTrunkDay()
			readAt(recs, 45).World.Builds[0].Verdict = queue.Success
			return recs
		}},
		{"two red probes of the trunk with members", func() []queue.Recording {
			recs := redTrunkDay()
			for _, minutes := range []int{30, 45} {
				readAt(recs, minutes).World.Builds[0].Chain = []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: "m"}}
			}
			return recs
		}},
		{"two red checks of the trunk", func() []queue.Recording {
			recs := redTrunkDay()
			for _, minutes := range []int{30, 45} {
				readAt(recs, minutes).World.Builds[0].Kind = queue.Check
			}
			return recs
		}},
		{"two red probes with a green build on the trunk pushed between them", func() []queue.Recording {
			recs := redTrunkDay()
			check := queue.Build{SHA: "check", Kind: queue.Check, Base: "T", Pushed: replayStart.Add(25 * time.Minute), Verdict: queue.Success}
			r := readAt(recs, 45)
			r.World.Builds = append(r.World.Builds, check)
			return recs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if found := foundSpans(tc.day()); len(found) != 0 {
				t.Fatalf("spans %v", found)
			}
		})
	}
}

func TestReplayRedTrunkIsNotClearedByTheLandedCandidatesOwnBuild(t *testing.T) {
	recs := redTrunkDay()
	recs[0].Actions = nil
	for i := range recs {
		for j, b := range recs[i].World.Builds {
			if b.SHA == "T" {
				recs[i].World.Builds[j].Pushed = replayStart.Add(25 * time.Minute)
			}
		}
	}
	own := queue.Build{SHA: "T", Base: "T", Pushed: replayStart.Add(25 * time.Minute), Verdict: queue.Success}
	r := readAt(recs, 30)
	r.World.Builds = append(r.World.Builds, own)
	if found, want := foundSpans(recs), []foundSpan{{10 * time.Minute, 50 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("spans %v, want %v", found, want)
	}
}

// recordAt is a record read at minutes, holding the queue of the record before it and one build.
func recordAt(recs []queue.Recording, minutes int, b queue.Build) queue.Recording {
	var r queue.Recording
	for _, rec := range recs {
		if !rec.World.Now.After(replayStart.Add(time.Duration(minutes) * time.Minute)) {
			r = rec
		}
	}
	r.World.Now, r.World.Builds, r.Actions = replayStart.Add(time.Duration(minutes)*time.Minute), []queue.Build{b}, nil
	return r
}

func sortedByTime(recs []queue.Recording) []queue.Recording {
	slices.SortStableFunc(recs, func(a, b queue.Recording) int { return a.World.Now.Compare(b.World.Now) })
	return recs
}

func TestReplayCountsTheCandidatesOfARedTrunkNoFixEndsTowardsTheChanceOfRed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		day     func() []queue.Recording
		red, of int
	}{
		{"no fix ends it", redTrunkDay, 2, 4},
		{"its fix ends it", fixedRedTrunkDay, 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := tc.day()
			early := queue.Build{SHA: "early", Base: "T", Pushed: replayStart.Add(5 * time.Minute), Verdict: queue.Failure,
				Chain: []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: "early"}}}
			before := queue.Build{SHA: "before", Base: "t0", Pushed: replayStart.Add(2 * time.Minute), Verdict: queue.Failure,
				Chain: []queue.Member{{Number: 4, Head: replayPR(4).Head, Merge: "before"}}}
			recs = sortedByTime(append(recs, recordAt(recs, 15, early), recordAt(recs, 8, before)))
			_, w, _ := replayWindow(t, recs, 100)
			if w.unblamedRedCandidates != tc.red || w.candidatesWithoutCulprit != tc.of || w.flake != float64(tc.red)/float64(tc.of) {
				t.Fatalf("unblamed red candidates %d of %d, chance of red %g", w.unblamedRedCandidates, w.candidatesWithoutCulprit, w.flake)
			}
		})
	}
}

// leavesAfterRedBuilds is redTrunkDay with two red builds of #3 stacked on #1, pushed before T
// landed and read red on T, two more of #3 read red on the trunk before T where onAnother says
// so, and #3 leaving at 60m.
func leavesAfterRedBuilds(onAnother bool) []queue.Recording {
	recs := redTrunkDay()
	three := func(sha queue.SHA, base queue.SHA, pushed int) queue.Build {
		return queue.Build{SHA: sha, Kind: queue.Check, Base: base, Pushed: replayStart.Add(time.Duration(pushed) * time.Minute), Verdict: queue.Failure,
			Chain: []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: sha}}}
	}
	recs = append(recs, recordAt(recs, 15, three("early1", "T", 5)), recordAt(recs, 16, three("early2", "T", 6)))
	if onAnother {
		recs = append(recs, recordAt(recs, 7, three("old1", "t0", 1)), recordAt(recs, 8, three("old2", "t0", 2)))
	}
	recs = sortedByTime(recs)
	for i := range recs {
		if recs[i].World.Now.After(replayStart.Add(60 * time.Minute)) {
			recs[i].World.PRs = slices.DeleteFunc(slices.Clone(recs[i].World.PRs), func(p queue.PR) bool { return p.Number == 3 })
		}
	}
	return recs
}

func TestReplayHoldsNoHeadGuiltyForTheRedTrunksBuilds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		onAnother bool
	}{
		{"all its red builds the trunk's", false},
		{"two more red on another trunk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, _ := replayWindow(t, leavesAfterRedBuilds(tc.onAnother), 100)
			if guilty := heldGuilty(w, 3, replayPR(3).Head); guilty != tc.onAnother {
				t.Fatalf("#3 held guilty %v", guilty)
			}
		})
	}
}

// behindRedBuildsAlone is redTrunkDay with two red builds of #3 and #4 read red on the trunk
// before T, two red probes of #4 alone on aloneOn pushed before T landed and read red after,
// and #3 leaving at 60m. Where redTrunk is false the second probe of the bare trunk is read
// green, so T is not red and no probe of #4 is the trunk's.
func behindRedBuildsAlone(redTrunk bool, aloneOn queue.SHA) []queue.Recording {
	day := redTrunkDay()
	if !redTrunk {
		readAt(day, 45).World.Builds[0].Verdict = queue.Success
	}
	build := func(sha queue.SHA, kind queue.Kind, base queue.SHA, pushed int, numbers ...int) queue.Build {
		b := queue.Build{SHA: sha, Kind: kind, Base: base, Pushed: replayStart.Add(time.Duration(pushed) * time.Minute), Verdict: queue.Failure}
		for _, n := range numbers {
			b.Chain = append(b.Chain, queue.Member{Number: n, Head: replayPR(n).Head, Merge: sha})
		}
		return b
	}
	recs := append(slices.Clone(day),
		recordAt(day, 7, build("both1", queue.Check, "t0", 1, 3, 4)), recordAt(day, 8, build("both2", queue.Check, "t0", 2, 3, 4)),
		recordAt(day, 15, build("alone1", queue.Probe, aloneOn, 5, 4)), recordAt(day, 16, build("alone2", queue.Probe, aloneOn, 6, 4)))
	recs = sortedByTime(recs)
	for i := range recs {
		if recs[i].World.Now.After(replayStart.Add(60 * time.Minute)) {
			recs[i].World.PRs = slices.DeleteFunc(slices.Clone(recs[i].World.PRs), func(p queue.PR) bool { return p.Number == 3 })
		}
	}
	return recs
}

func TestReplayCountsNoRedTrunksBuildAsAHeadFailingAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redTrunk bool
		aloneOn  queue.SHA
		spans    int
		guilty   bool
	}{
		{"its builds alone the red trunk's", true, "T", 1, true},
		{"its builds alone on a trunk the records clear", false, "T", 0, false},
		{"its builds alone on another trunk than the red one", true, "t0", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := behindRedBuildsAlone(tc.redTrunk, tc.aloneOn)
			if found := foundSpans(recs); len(found) != tc.spans {
				t.Fatalf("spans %v", found)
			}
			_, w, _ := replayWindow(t, recs, 100)
			if guilty := heldGuilty(w, 3, replayPR(3).Head); guilty != tc.guilty {
				t.Fatalf("#3 held guilty %v", guilty)
			}
		})
	}
}

// reversedRedTrunkDay is redTrunkDay with its probes pushed at 2m and 5m, before T landed, and a
// check on T pushed at 7m read green at 46m, so the span the records show ends before it begins.
func reversedRedTrunkDay() []queue.Recording {
	recs := redTrunkDay()
	recs[3].Actions, recs[5].Actions = nil, nil
	readAt(recs, 30).World.Builds[0].Pushed = replayStart.Add(2 * time.Minute)
	readAt(recs, 45).World.Builds[0].Pushed = replayStart.Add(5 * time.Minute)
	check := queue.Build{SHA: "check", Kind: queue.Check, Base: "T", Pushed: replayStart.Add(7 * time.Minute), Verdict: queue.Success}
	return sortedByTime(append(recs, recordAt(recs, 46, check)))
}

func TestReplayCountsTheRedTrunksNoFixEndsThatOverlapTheWindow(t *testing.T) {
	if found, want := foundSpans(reversedRedTrunkDay()), []foundSpan{{10 * time.Minute, 7 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("the reversed day's spans %v, want %v", found, want)
	}
	for _, tc := range []struct {
		name          string
		day           func() []queue.Recording
		from, minutes time.Duration
		want          int
		inside        float64
	}{
		{"inside the window", redTrunkDay, 0, 100 * time.Minute, 1, 40},
		{"straddling its opening", redTrunkDay, 30 * time.Minute, 40 * time.Minute, 1, 20},
		{"ending exactly at its opening", redTrunkDay, 50 * time.Minute, 40 * time.Minute, 0, 0},
		{"wholly in the lead-in", redTrunkDay, 60 * time.Minute, 40 * time.Minute, 0, 0},
		{"starting exactly at its stop", redTrunkDay, -30 * time.Minute, 40 * time.Minute, 0, 0},
		{"ending before it begins", reversedRedTrunkDay, 0, 100 * time.Minute, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, _ := recordedWindow(t, tc.day(), replayStart.Add(tc.from), replayStart.Add(tc.from+tc.minutes))
			if _, sim := w.simulate(Shape{}, was); sim.UnplayedRedTrunkSpans != tc.want || sim.UnplayedRedTrunkMinutes != tc.inside || sim.RedTrunkSpans != 0 {
				t.Fatalf("unplayed red trunk spans %d over %v min, want %d over %v; played %d", sim.UnplayedRedTrunkSpans, sim.UnplayedRedTrunkMinutes, tc.want, tc.inside, sim.RedTrunkSpans)
			}
		})
	}
}

func TestReplayFindsARedTrunkNoGreenBuildEndsRunningToTheLastRecord(t *testing.T) {
	recs := redTrunkDay()[:9]
	recs = append(recs, recordAt(recs, 100, recs[8].World.Builds[0]))
	if found, want := foundSpans(recs), []foundSpan{{10 * time.Minute, 100 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("spans %v, want %v", found, want)
	}
	was, w, _ := replayWindow(t, recs, 80)
	if _, sim := w.simulate(Shape{}, was); sim.UnplayedRedTrunkSpans != 1 || sim.UnplayedRedTrunkMinutes != 70 || sim.RedTrunkSpans != 0 {
		t.Fatalf("unplayed red trunk spans %d over %v min; played %d", sim.UnplayedRedTrunkSpans, sim.UnplayedRedTrunkMinutes, sim.RedTrunkSpans)
	}
}

// The simulated half of redTrunkDay's row with its second probe read green, by the replay before
// it played a red trunk.
const clearedRedTrunkSimulated = `{"over_live":{"landed":1,"wait_p50":-0.42857142857142855,"wait_p90":-0.14285714285714285,"builds_per_landing":-0.3333333333333333},"simulated":{"landed":4,"wait_p50":2,"wait_p90":3,"builds_per_landing":1,"unblamed_red_candidates":0,"candidates_without_a_culprit":2,"culprits_unattributed":0,"unejected_guilty_heads":0,"departures_after_an_unconfirmed_plan":0,"invariant_failures":0,"waiting_at_open":4,"settings_in_window":1,"acts":{"checks":4,"conflict":0,"ejected":0,"head":4,"hold":4,"label":4,"settings":1,"unlabel":0,"wait":4},"unattributed_holds":0,"wait_p50_minutes":40,"wait_p90_minutes":60,"whole_builds":4,"probes":0,"red_builds":0,"queue_added_p50_minutes":20,"queue_added_p90_minutes":50,"queue_added_over_30":1.18,"red_candidates":0,"red_pull_requests":0,"ejected_as_cause":0,"left_waiting":0}}`

func TestReplayLeavesTheSimulatedHalfOfADayWithNoRedTrunkAsItWas(t *testing.T) {
	recs := redTrunkDay()
	readAt(recs, 45).World.Builds[0].Verdict = queue.Success
	out, err := captureLiveRun(t, writeLive(t, recs), "hey2", replayStart.Format(time.RFC3339), replayStart.Add(100*time.Minute).Format(time.RFC3339), true)
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
	if err := json.Unmarshal([]byte(clearedRedTrunkSimulated), &was); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if row.Simulated["red_trunk_spans"] != 0.0 || row.Simulated["unfixed_red_trunk_spans"] != 0.0 || row.Simulated["unplayed_red_trunk_spans"] != 0.0 || row.Simulated["unplayed_red_trunk_minutes"] != 0.0 {
		t.Fatalf("red trunk spans %v, unfixed %v, unplayed %v over %v min", row.Simulated["red_trunk_spans"], row.Simulated["unfixed_red_trunk_spans"], row.Simulated["unplayed_red_trunk_spans"], row.Simulated["unplayed_red_trunk_minutes"])
	}
	delete(row.Simulated, "red_trunk_spans")
	delete(row.Simulated, "unfixed_red_trunk_spans")
	delete(row.Simulated, "unplayed_red_trunk_spans")
	delete(row.Simulated, "unplayed_red_trunk_minutes")
	if !reflect.DeepEqual(row, was) {
		t.Fatalf("simulated half changed:\n%v\n%v", row, was)
	}
}

// fixedRedTrunkDay is redTrunkDay with #2 queued at 40m to go first, so the candidate that ends
// the red trunk at 50m is its fix, which the records land at 70m.
func fixedRedTrunkDay() []queue.Recording {
	recs := redTrunkDay()
	for i := range recs {
		recs[i].World.PRs = slices.Clone(recs[i].World.PRs)
		for j := range recs[i].World.PRs {
			recs[i].World.PRs[j].First = recs[i].World.PRs[j].Number == 2
		}
		if recs[i].World.Now.Before(replayStart.Add(40 * time.Minute)) {
			recs[i].World.PRs = slices.DeleteFunc(recs[i].World.PRs, func(p queue.PR) bool { return p.Number == 2 })
		}
	}
	return recs
}

// fixedBuild is a build a replay read a verdict for, by when it began and whether its tree held
// the fix.
type fixedBuild struct {
	start   time.Duration
	fixed   bool
	verdict queue.Verdict
}

func TestReplayEndsARedTrunkItsFixEndsWhenItsQueueLandsTheFix(t *testing.T) {
	_, w, _ := replayWindow(t, fixedRedTrunkDay(), 100)
	if want := []Episode{{At: 10 * time.Minute, NeedsFix: true, Fixes: []int{2}}}; !reflect.DeepEqual(w.episodes, want) {
		t.Fatalf("episodes %v, want %v", w.episodes, want)
	}
	s := NewSim(replayShape(w), w.cfg, 1)
	var answered []fixedBuild
	seen := map[queue.SHA]bool{}
	plan := s.plan
	s.plan = func(world queue.World, cfg queue.Config) []queue.Action {
		for _, b := range world.Builds {
			if sb := s.builds[b.SHA]; sb != nil && b.Verdict != queue.Pending && !seen[b.SHA] {
				seen[b.SHA] = true
				answered = append(answered, fixedBuild{sb.start, s.holds[b.SHA][2], b.Verdict})
			}
		}
		return plan(world, cfg)
	}
	s.play()
	for step := s.tick; s.now < w.stop; step = s.Step {
		step()
	}
	fix, landed := never, s.prs[2].at
	for _, b := range answered {
		if b.fixed {
			fix = min(fix, b.start)
		}
	}
	if !s.prs[2].landed || fix == never {
		t.Fatalf("the fix began at %v and landed %v", fix, s.prs[2].landed)
	}
	for _, b := range answered {
		if b.start >= 10*time.Minute && (b.verdict == queue.Success) != b.fixed {
			t.Fatalf("a tree begun at %v holding the fix %v read %d; the fix began at %v and landed at %v", b.start, b.fixed, b.verdict, fix, landed)
		}
	}
	for n, p := range s.prs {
		if p.landed && n != 2 && p.at > fix && p.at < landed {
			t.Fatalf("#%d landed at %v, between the fix's start at %v and its landing at %v", n, p.at, fix, landed)
		}
	}
	if !slices.ContainsFunc(answered, func(b fixedBuild) bool { return !b.fixed && b.start > fix && b.verdict == queue.Failure }) {
		t.Fatalf("no tree without the fix began after it: %v", answered)
	}
}

func TestReplayQueuesNoFixOfItsOwnForARedTrunkARecordedOneFixes(t *testing.T) {
	_, w, _ := replayWindow(t, fixedRedTrunkDay(), 100)
	s := runReplay(w)
	for n := range s.prs {
		if n > 4 {
			t.Fatalf("the replay queued #%d of its own", n)
		}
	}
}

func TestRedTrunkPassesEveryTreeHoldingTheFixAndEveryOneOnceItLands(t *testing.T) {
	shape := Shape{Build: 10 * time.Minute, Probe: 4 * time.Minute, Recorded: true, Stop: 2 * time.Hour, Instant: true,
		Episodes: []Episode{{At: 5 * time.Minute, NeedsFix: true, Fixes: []int{2}}}}
	s := NewSim(shape, dials(2, 1, false), 1)
	s.plan = func(queue.World, queue.Config) []queue.Action { return nil }
	for n := 1; n <= 2; n++ {
		p := &simPR{back: never, queued: true}
		p.Number, p.Head, p.Checks = n, queue.SHA(rune('a'+n)), queue.Success
		s.prs[n], s.byHead[p.Head] = p, n
	}
	bare := s.commit(s.trunk, "", "check", 0)
	s.push(queue.Build{SHA: bare, Kind: queue.Probe})
	for s.now < 10*time.Minute {
		s.Step()
	}
	fix := s.commit(s.trunk, "b", "", 2)
	s.push(queue.Build{SHA: fix, Chain: []queue.Member{{Number: 2, Head: "b", Merge: fix}}})
	for s.now < 40*time.Minute {
		s.Step()
	}
	one := s.commit(s.trunk, "a", "", 1)
	s.push(queue.Build{SHA: one, Chain: []queue.Member{{Number: 1, Head: "a", Merge: one}}})
	for s.now < 60*time.Minute {
		s.Step()
	}
	if s.builds[bare].Verdict != queue.Success || s.builds[fix].Verdict != queue.Success || s.builds[one].Verdict != queue.Failure {
		t.Fatalf("before the episode %d, the fix %d, without it after it %d", s.builds[bare].Verdict, s.builds[fix].Verdict, s.builds[one].Verdict)
	}
	if s.unfixed() != 1 {
		t.Fatalf("unfixed %d before the fix lands", s.unfixed())
	}
	s.land(queue.Action{Do: "land", SHA: fix, Members: []queue.Member{{Number: 2, Head: "b", Merge: fix}}})
	after := s.commit(s.trunk, "a", "", 1)
	s.push(queue.Build{SHA: after, Chain: []queue.Member{{Number: 1, Head: "a", Merge: after}}})
	for s.now < 90*time.Minute {
		s.Step()
	}
	if s.builds[after].Verdict != queue.Success || s.unfixed() != 0 {
		t.Fatalf("a tree on the trunk after the fix landed read %d; unfixed %d", s.builds[after].Verdict, s.unfixed())
	}
}

func TestReplayMakesNoFixOfAFirstMemberAfterTheFirstGreenBuild(t *testing.T) {
	recs := fixedRedTrunkDay()
	check := queue.Build{SHA: "check", Kind: queue.Check, Base: "T", Pushed: replayStart.Add(40 * time.Minute), Verdict: queue.Success}
	recs = sortedByTime(append(recs, recordAt(recs, 46, check)))
	if found, want := foundSpans(recs), []foundSpan{{10 * time.Minute, 40 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("spans %v, want %v", found, want)
	}
}

func TestReplayCountsTheRedTrunksItsQueueLeftUnfixed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		minutes int
		want    float64
	}{
		{"the fix lands", 100, 0},
		{"the window ends before it can", 52, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, w, _ := replayWindow(t, fixedRedTrunkDay(), tc.minutes)
			if _, sim := w.simulate(Shape{}, was); sim.UnfixedRedTrunkSpans != tc.want || sim.RedTrunkSpans != 1 {
				t.Fatalf("unfixed red trunk spans %v of %d", sim.UnfixedRedTrunkSpans, sim.RedTrunkSpans)
			}
		})
	}
}

func TestReplayPlaysAFixedRedTrunkOnlyWhereItsFixLandsAfterTheReplayOpens(t *testing.T) {
	for _, tc := range []struct {
		name string
		from time.Duration
		want []Episode
	}{
		{"the fix pushed, not landed", leadIn + 55*time.Minute, []Episode{{At: -leadIn, NeedsFix: true, Fixes: []int{2}}}},
		{"the fix landed", leadIn + 75*time.Minute, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, w, _ := recordedWindow(t, fixedRedTrunkDay(), replayStart.Add(tc.from), replayStart.Add(tc.from+40*time.Minute))
			if !reflect.DeepEqual(w.episodes, tc.want) {
				t.Fatalf("episodes %v, want %v", w.episodes, tc.want)
			}
		})
	}
}

func TestReplayHoldsTheRedBuildsWithoutTheFixTheTrunksUntilItLands(t *testing.T) {
	recs := fixedRedTrunkDay()
	at := func(minutes int) time.Time { return replayStart.Add(time.Duration(minutes) * time.Minute) }
	late := queue.Build{SHA: "late", Base: "T", Pushed: at(55), Verdict: queue.Failure,
		Chain: []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: "late"}}}
	holding := queue.Build{SHA: "holding", Base: "T", Pushed: at(45), Verdict: queue.Failure,
		Chain: []queue.Member{{Number: 2, Head: replayPR(2).Head, Merge: "two"}, {Number: 3, Head: replayPR(3).Head, Merge: "holding"}}}
	after := queue.Build{SHA: "after", Base: "T", Pushed: at(75), Verdict: queue.Failure,
		Chain: []queue.Member{{Number: 3, Head: replayPR(3).Head, Merge: "after"}}}
	recs = sortedByTime(append(recs, recordAt(recs, 65, late), recordAt(recs, 48, holding)))
	if _, theirs := redTrunks(sortedByTime(append(slices.Clone(recs), recordAt(recs, 80, after)))); !theirs["late"] || theirs["holding"] || theirs["after"] {
		t.Fatalf("the trunk's: pushed after the fix's push without it %v, holding it %v, pushed after its landing %v", theirs["late"], theirs["holding"], theirs["after"])
	}
	_, w, _ := replayWindow(t, recs, 100)
	if w.unblamedRedCandidates != 1 || w.candidatesWithoutCulprit != 3 {
		t.Fatalf("unblamed red candidates %d of %d", w.unblamedRedCandidates, w.candidatesWithoutCulprit)
	}
}

func TestReplayPlaysARedTrunkWhoseFixsLandingTheRecordsRefused(t *testing.T) {
	recs := fixedRedTrunkDay()
	queued := slices.Clone(readAt(recs, 70).World.PRs)
	for i := range recs {
		if recs[i].World.Now.After(replayStart.Add(70 * time.Minute)) {
			recs[i].World.Trunk, recs[i].World.PRs = "T", slices.Clone(queued)
		}
	}
	_, w, _ := recordedWindow(t, recs, replayStart.Add(leadIn+75*time.Minute), replayStart.Add(leadIn+115*time.Minute))
	if want := []Episode{{At: -leadIn, NeedsFix: true, Fixes: []int{2}}}; !reflect.DeepEqual(w.episodes, want) {
		t.Fatalf("episodes %v, want %v", w.episodes, want)
	}
}

func TestReplayPlaysNoRedTrunkWhereAGreenBuildPushedOnItIsReadOnALaterBase(t *testing.T) {
	recs := redTrunkDay()
	check := queue.Build{SHA: "check", Kind: queue.Check, Base: "T", Pushed: replayStart.Add(25 * time.Minute)}
	pushed := recordAt(recs, 25, queue.Build{})
	pushed.World.Builds, pushed.Actions = nil, []queue.Action{{Do: "push", Build: check}}
	check.Base, check.Verdict = "g", queue.Success
	recs = sortedByTime(append(recs, pushed, recordAt(recs, 74, check)))
	if found := foundSpans(recs); len(found) != 0 {
		t.Fatalf("spans %v", found)
	}
}

func TestReplayStartsARedTrunkAtTheLastGreenBuildOnItBeforeItsProbes(t *testing.T) {
	recs := redTrunkDay()
	check := queue.Build{SHA: "check", Kind: queue.Check, Base: "T", Pushed: replayStart.Add(15 * time.Minute), Verdict: queue.Success}
	recs = sortedByTime(append(recs, recordAt(recs, 18, check)))
	if found, want := foundSpans(recs), []foundSpan{{15 * time.Minute, 50 * time.Minute, nil}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("spans %v, want %v", found, want)
	}
}

func TestRedTrunkFailsATreeWithoutTheFixStillRunningWhenTheFixLands(t *testing.T) {
	shape := Shape{Build: 10 * time.Minute, Probe: 4 * time.Minute, Recorded: true, Stop: 2 * time.Hour, Instant: true,
		Episodes: []Episode{{At: 5 * time.Minute, NeedsFix: true, Fixes: []int{2}}}}
	s := NewSim(shape, dials(2, 1, false), 1)
	s.plan = func(queue.World, queue.Config) []queue.Action { return nil }
	for n := 1; n <= 2; n++ {
		p := &simPR{back: never, queued: true}
		p.Number, p.Head, p.Checks = n, queue.SHA(rune('a'+n)), queue.Success
		s.prs[n], s.byHead[p.Head] = p, n
	}
	s.step(6 * time.Minute)
	fix := s.commit(s.trunk, "b", "", 2)
	s.push(queue.Build{SHA: fix, Chain: []queue.Member{{Number: 2, Head: "b", Merge: fix}}})
	s.Step()
	one := s.commit(s.trunk, "a", "", 1)
	s.push(queue.Build{SHA: one, Chain: []queue.Member{{Number: 1, Head: "a", Merge: one}}})
	for s.builds[fix].Verdict == queue.Pending && s.now < time.Hour {
		s.Step()
	}
	if s.builds[fix].Verdict != queue.Success || s.builds[one].Verdict != queue.Pending {
		t.Fatalf("the fix read %d; the tree without it read %d before the fix landed", s.builds[fix].Verdict, s.builds[one].Verdict)
	}
	s.land(queue.Action{Do: "land", SHA: fix, Members: []queue.Member{{Number: 2, Head: "b", Merge: fix}}})
	for s.builds[one].Verdict == queue.Pending && s.now < time.Hour {
		s.Step()
	}
	if s.builds[one].Verdict != queue.Failure {
		t.Fatalf("a tree without the fix pushed before it landed read %d", s.builds[one].Verdict)
	}
}
