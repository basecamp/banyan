package queue

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type commits struct{}

func (commits) Merge(parent SHA, pr PR, note string) (SHA, bool) {
	return fmt.Sprintf("merge(%s,%d,%q)", parent, pr.Number, note), true
}
func (commits) Mark(parent SHA, note string) SHA { return fmt.Sprintf("mark(%s,%q)", parent, note) }
func (commits) Seen(SHA) bool                    { return false }

// #1 and #2 failed together. The second round's build of #1 alone passed, and its build of the
// trunk has yet to report. The third round has reported whole: both failed again, and so did
// #1 alone, which had just passed. Read in order, that last failure means nothing the rounds
// said holds, so #2 is not blamed on the three builds that would otherwise be its fair test.
func TestNobodyIsBlamedOnARoundReadBeforeTheOneBeforeItIsWhole(t *testing.T) {
	now := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	both := []Member{{1, "h1", "m1"}, {2, "h2", "m2"}}
	build := func(sha SHA, chain []Member, round int, verdict Verdict) Build {
		b := Build{SHA: sha, Kind: Check, Base: "trunk", Chain: chain, Round: round, Of: "m2", Verdict: verdict, Pushed: now, Started: now}
		if round == 0 {
			b.Kind, b.Of = Candidate, ""
		}
		return b
	}
	w := World{Now: now.Add(time.Hour), Trunk: "trunk", Repo: commits{},
		PRs: []PR{{Number: 1, Head: "h1", Queued: now, Checks: Success}, {Number: 2, Head: "h2", Queued: now, Checks: Success}},
		Builds: []Build{
			build("m2", both, 0, Failure),
			build("both1", both, 1, Failure), build("one1", both[:1], 1, Failure),
			build("one2", both[:1], 2, Success), build("trunk2", nil, 2, Pending),
			build("both3", both, 3, Failure), build("one3", both[:1], 3, Failure),
		}}
	for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 2, Cancels: true, Recheck: 10 * time.Minute}) {
		if a.Do == "eject" {
			t.Errorf("ejected #%d as %s on %v, with the second round still running", a.PR, a.Cause, a.Evidence)
		}
	}

	// Once the trunk's build passes, the rounds are whole, and the third round's failure of #1
	// alone starts them over: still nobody is blamed, and a fourth round begins, though each
	// pull request's three builds are all there in the rounds that no longer hold.
	w.Builds[4].Verdict = Success
	fourth := 0
	for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 2, Cancels: true, Recheck: 10 * time.Minute}) {
		if a.Do == "eject" {
			t.Errorf("ejected #%d as %s on %v, though #1 alone failed after it had passed", a.PR, a.Cause, a.Evidence)
		}
		if a.Do == "push" && a.Build.Round == 4 {
			fourth++
		}
	}
	if fourth != 2 {
		t.Errorf("the rounds started over with %d builds of a fourth round, where both pull requests' tests begin again", fourth)
	}
}

// Three of five queued pull requests are labelled to go first, and the trunk passes. They go
// ahead of the rest, and one candidate holds all five: a batch ends at a pull request labelled
// first only while the trunk is red, when that pull request is its fix.
func TestPullRequestsLabelledFirstBatchWithTheRestWhileTheTrunkPasses(t *testing.T) {
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	w := World{Now: now, Trunk: "trunk", Repo: commits{}}
	for n := 1; n <= 5; n++ {
		w.PRs = append(w.PRs, PR{Number: n, Head: fmt.Sprintf("h%d", n), Queued: now.Add(time.Duration(n) * time.Minute), First: n >= 3, Checks: Success})
	}
	var pushed [][]int
	for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 12, Recheck: 10 * time.Minute}) {
		if a.Do == "push" {
			var ns []int
			for _, m := range a.Build.Chain {
				ns = append(ns, m.Number)
			}
			pushed = append(pushed, ns)
		}
	}
	if want := [][]int{{3, 4, 5, 1, 2}}; fmt.Sprint(pushed) != fmt.Sprint(want) {
		t.Errorf("pushed candidates of %v, want one of %v", pushed, want)
	}
}

func member(n int, merge SHA) Member { return Member{n, fmt.Sprintf("h%d", n), merge} }

func queuedAt(now time.Time, numbers ...int) (prs []PR) {
	for _, n := range numbers {
		prs = append(prs, PR{Number: n, Head: fmt.Sprintf("h%d", n), Queued: now.Add(time.Duration(n) * time.Minute), Checks: Success})
	}
	return prs
}

func ejections(acts []Action) (numbers []int, evidence [][]SHA) {
	for _, a := range acts {
		if a.Do == "eject" {
			numbers, evidence = append(numbers, a.PR), append(evidence, a.Evidence)
		}
	}
	return numbers, evidence
}

// #1 breaks the build and #2 does not. The second round lost its build of #1 alone, which was
// the pass #2's test wanted, and its build of the trunk passed, which was the pass #1's test
// wanted. A round that lost a build still says what its other builds said: #1 failed, the trunk
// then passed, and #1 failed again, so #1 leaves. It used to wait with no end, its test whole
// and the trunk's pass unread.
func TestARoundThatLostABuildStillCountsTheBuildsThatReported(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	both := []Member{member(1, "m1"), member(2, "m2")}
	check := func(sha SHA, chain []Member, round int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Check, Base: "trunk", Chain: chain, Round: round, Of: "m2", Verdict: verdict, Pushed: now, Started: now}
	}
	w := World{Now: now.Add(time.Hour), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2),
		Builds: []Build{
			{SHA: "m2", Base: "trunk", Chain: both, Verdict: Failure, Pushed: now, Started: now},
			check("both1", both, 1, Failure), check("one1", both[:1], 1, Failure),
			check("one2", both[:1], 2, Error), check("trunk2", nil, 2, Success),
			check("one3", both[:1], 3, Failure),
		}}
	numbers, evidence := ejections(Plan(w, Config{Slots: 2, MaxBatch: 2, Recheck: 10 * time.Minute}))
	if fmt.Sprint(numbers) != "[1]" || fmt.Sprint(evidence) != "[[trunk2 one1 one3]]" {
		t.Errorf("ejected %v on %v, where #1 leaves on the trunk's pass between its two failures", numbers, evidence)
	}
}

// #2 clashes with #1, and #3 with #2. A probe of #1 failed while the trunk was red, so #2 and
// #3 were built without it and failed too. Now both tests are whole: #2 fails on #1, and #3
// fails on #2. Only #2 leaves. #3's test passed on a tree holding #2, which says nothing of the
// queue once #2 is gone, and #3 lands with #1.
func TestNobodyIsBlamedAgainstAPullRequestThatLeavesInTheSameTick(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	three := []Member{member(1, "a1"), member(2, "a2"), member(3, "a3")}
	two := []Member{member(2, "b2"), member(3, "b3")}
	probe := func(sha, of SHA, chain []Member, round int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Probe, Base: "trunk", Chain: chain, Round: round, Of: of, Verdict: verdict, Pushed: now, Started: now}
	}
	w := World{Now: now.Add(time.Hour), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2, 3),
		Builds: []Build{
			{SHA: "a3", Base: "trunk", Chain: three, Verdict: Failure, Pushed: now, Started: now},
			probe("a-two-1", "a3", three[:2], 1, Failure), probe("a-one-1", "a3", three[:1], 1, Success), probe("a-trunk-1", "a3", nil, 1, Success),
			probe("a-one-2", "a3", three[:1], 2, Success),
			probe("a-two-3", "a3", three[:2], 3, Failure),
			{SHA: "b3", Base: "trunk", Chain: two, Verdict: Failure, Pushed: now, Started: now},
			probe("b-one-1", "b3", two[:1], 1, Success), probe("b-trunk-1", "b3", nil, 1, Success),
			probe("b-two-2", "b3", two, 2, Failure),
			probe("b-one-3", "b3", two[:1], 3, Success),
			probe("b-two-4", "b3", two, 4, Failure),
		}}
	numbers, _ := ejections(Plan(w, Config{Slots: 2, MaxBatch: 3, Probes: true, Recheck: 10 * time.Minute}))
	if fmt.Sprint(numbers) != "[2]" {
		t.Errorf("ejected %v, where only #2 leaves and #3's test against it proves nothing once it is gone", numbers)
	}
}

// #1 and #2 failed together, and #2 breaks the build. #1 was cleared and has landed, so the
// red candidate now stands on the trunk that holds #1, and its builds of #1 alone are builds
// of that trunk. The second round's was one of them, and failed. That round also built the
// old trunk, without #1, and it passed: a build of a tree from below the landing, which is no
// pass between #2's two failures. So #2 is not blamed on it, and the rounds go on.
func TestABuildFromBelowALandingIsNoPartOfARedCandidatesRounds(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	two := []Member{member(2, "m2")}
	check := func(sha, base SHA, chain []Member, round int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Check, Base: base, Chain: chain, Round: round, Of: "m2", Verdict: verdict, Pushed: now, Started: now}
	}
	w := World{Now: now.Add(time.Hour), Trunk: "m1", Repo: commits{}, PRs: queuedAt(now, 2),
		Builds: []Build{
			{SHA: "m2", Base: "m1", Chain: two, Verdict: Failure, Pushed: now, Started: now},
			check("both1", "m1", two, 1, Failure), check("one1", "m1", nil, 1, Success),
			check("one2", "m1", nil, 2, Failure), check("old2", "old", nil, 2, Success),
			check("both3", "m1", two, 3, Failure), check("one3", "m1", nil, 3, Success),
		}}
	acts := Plan(w, Config{Slots: 2, MaxBatch: 2, Recheck: 10 * time.Minute})
	if numbers, evidence := ejections(acts); len(numbers) > 0 {
		t.Errorf("ejected %v on %v, where the pass between the failures was a build of the old trunk", numbers, evidence)
	}
	fourth := false
	for _, a := range acts {
		fourth = fourth || (a.Do == "push" && a.Build.Round == 4)
	}
	if !fourth {
		t.Errorf("no fourth round began, where #2's test starts again after the trunk's own failure: %+v", acts)
	}
}

// #1 and #2 failed together, and the first round's build of both outlived the timeout before
// CI began it. It may yet begin and fail after the rounds pushed since, and read by its round
// that failure would stand before their pass. So the round pushed past it skips a number, and
// the late failure is in no test: a CI broken for a while after the pass would fail both
// builds of both. Where CI had begun the build, the next round is the next number. Where the
// rounds are in a row, #2 leaves, however long the tick took to push each: on a CI that does
// not say when it begins a build, each round waited for the last to report.
func TestARoundPushedPastABuildCIHadNotBegunSkipsANumberThatNoTestSpans(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return now.Add(time.Duration(minutes) * time.Minute) }
	both := []Member{member(1, "m1"), member(2, "m2")}
	check := func(sha SHA, chain []Member, round int, verdict Verdict, pushed int) Build {
		return Build{SHA: sha, Kind: Check, Base: "trunk", Chain: chain, Round: round, Of: "m2", Verdict: verdict, Pushed: at(pushed)}
	}
	cfg := Config{Slots: 2, MaxBatch: 2, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}
	pushed := func(started time.Time) (rounds []int) {
		slow := check("both1", both, 1, Pending, 0)
		slow.Started = started
		w := World{Now: at(91), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2),
			Builds: []Build{{SHA: "m2", Base: "trunk", Chain: both, Verdict: Failure, Pushed: now}, slow, check("one1", both[:1], 1, Success, 0)}}
		for _, a := range Plan(w, cfg) {
			if a.Do == "push" && a.Build.Of == "m2" {
				rounds = append(rounds, a.Build.Round)
			}
		}
		return rounds
	}
	if rounds := pushed(time.Time{}); len(rounds) == 0 || slices.Max(rounds) != 3 || slices.Min(rounds) != 3 {
		t.Errorf("pushed rounds %v past a build CI had not begun, where the round skips to 3", rounds)
	}
	if rounds := pushed(at(1)); len(rounds) == 0 || slices.Max(rounds) != 2 || slices.Min(rounds) != 2 {
		t.Errorf("pushed rounds %v past a build CI had begun, where the next round is 2", rounds)
	}

	tested := func(pass int) []int {
		w := World{Now: at(300), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2),
			Builds: []Build{
				{SHA: "m2", Base: "trunk", Chain: both, Verdict: Failure, Pushed: now},
				check("both1", both, 1, Failure, 0), check("one1", both[:1], 1, Success, 0),
				check("one", both[:1], pass, Success, 91), check("both", both, pass+1, Failure, 182),
			}}
		numbers, _ := ejections(Plan(w, cfg))
		return numbers
	}
	if numbers := tested(3); len(numbers) > 0 {
		t.Errorf("ejected %v across a skipped round, where the first failure may have begun after the pass", numbers)
	}
	if numbers := tested(2); fmt.Sprint(numbers) != "[2]" {
		t.Errorf("ejected %v, where #2's test is three rounds in a row and it leaves", numbers)
	}
}

// A candidate is building. One more would add #2 and #3 alone, so with gather at three it
// waits for a third, and is built once #4 is ready too. With nothing building it takes what
// is ready, and a pull request labelled to go first is never held back.
func TestACandidateWaitsToGatherWhileAnotherBuilds(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	cfg := Config{Slots: 3, MaxBatch: 8, Gather: 3, Recheck: 10 * time.Minute}
	building := Build{SHA: "m1", Base: "trunk", Chain: []Member{member(1, "m1")}, Verdict: Pending, Pushed: now, Started: now}
	added := func(builds []Build, first int, numbers ...int) (sizes []int) {
		w := World{Now: now.Add(time.Minute), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, numbers...), Builds: builds}
		for i := range w.PRs {
			w.PRs[i].First = w.PRs[i].Number == first
		}
		for _, a := range Plan(w, cfg) {
			if a.Do == "push" {
				sizes = append(sizes, len(a.Build.Chain))
			}
		}
		return sizes
	}
	if sizes := added([]Build{building}, 0, 1, 2, 3); len(sizes) > 0 {
		t.Errorf("pushed candidates of %v, where two pull requests wait for a third while #1 builds", sizes)
	}
	if sizes := added([]Build{building}, 0, 1, 2, 3, 4); fmt.Sprint(sizes) != "[4]" {
		t.Errorf("pushed candidates of %v, where three have gathered and are built on #1", sizes)
	}
	if sizes := added(nil, 0, 2); fmt.Sprint(sizes) != "[1]" {
		t.Errorf("pushed candidates of %v, where nothing is building and #2 is built alone", sizes)
	}
	if sizes := added([]Build{building}, 2, 1, 2); fmt.Sprint(sizes) != "[2]" {
		t.Errorf("pushed candidates of %v, where #2 goes first and is built at once", sizes)
	}
}

// Probes pass on #1's red tree, a whole build of it is late and then fails, and probes pushed
// meanwhile pass on it again in a later round. Once a whole build has failed where probes
// passed, their passing says nothing: the rounds go on to #1's test as whole builds, and it
// leaves, where the queue once built the red tree whole again after every failure.
func TestProbesThatPassAfterAWholeBuildFailedDoNotAskForItAgain(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	one := []Member{member(1, "m1")}
	round := func(sha SHA, kind Kind, chain []Member, n int, verdict Verdict) Build {
		at := now.Add(time.Duration(n) * time.Minute)
		return Build{SHA: sha, Kind: kind, Base: "trunk", Chain: chain, Round: n, Of: "m1", Verdict: verdict, Pushed: at, Started: at}
	}
	builds := []Build{
		{SHA: "m1", Base: "trunk", Chain: one, Verdict: Failure, Pushed: now, Started: now},
		round("p1", Probe, one, 1, Success), round("p2", Probe, nil, 2, Failure),
		round("c4", Check, one, 4, Failure),
		round("p6a", Probe, nil, 6, Success), round("p6b", Probe, one, 6, Success),
		round("c7", Check, one, 7, Failure),
	}
	cfg := Config{Slots: 1, MaxBatch: 1, Probes: true, Recheck: 10 * time.Minute}
	w := World{Now: now.Add(time.Hour), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1), Builds: builds}
	var pushed []string
	for _, a := range Plan(w, cfg) {
		if a.Do == "push" && a.Build.Of == "m1" {
			pushed = append(pushed, fmt.Sprintf("%d of %d", a.Build.Round, len(a.Build.Chain)))
		}
	}
	if fmt.Sprint(pushed) != "[8 of 0]" {
		t.Errorf("pushed rounds %v, where round 8 builds the trunk alone, the pass of #1's test", pushed)
	}
	w.Builds = append(builds, round("c8", Check, nil, 8, Success), round("c9", Check, one, 9, Failure))
	if numbers, _ := ejections(Plan(w, cfg)); fmt.Sprint(numbers) != "[1]" {
		t.Errorf("ejected %v, where #1 failed, the trunk passed without it and it failed again", numbers)
	}
}

// The trunk broke under #1's candidate: probes of the trunk alone and of #1 on it both failed.
// A round later both pass. The trunk no longer fails, so the queue builds #1's tree whole at
// once and says so, where it once held for the recheck and went on saying the trunk failed.
func TestATrunkThatPassesAgainUnderProbesIsNoLongerHeld(t *testing.T) {
	now := time.Date(2026, 11, 14, 22, 0, 0, 0, time.UTC)
	one := []Member{member(1, "m1")}
	probe := func(sha SHA, chain []Member, n int, verdict Verdict) Build {
		at := now.Add(time.Duration(n) * time.Minute)
		return Build{SHA: sha, Kind: Probe, Base: "trunk", Chain: chain, Round: n, Of: "m1", Verdict: verdict, Pushed: at, Started: at}
	}
	w := World{Now: now.Add(3 * time.Minute), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1),
		Builds: []Build{
			{SHA: "m1", Base: "trunk", Chain: one, Verdict: Failure, Pushed: now, Started: now},
			probe("p1a", nil, 1, Failure), probe("p1b", one, 1, Failure),
			probe("p2a", nil, 2, Success), probe("p2b", one, 2, Success),
		}}
	var pushed, notes []string
	for _, a := range Plan(w, Config{Slots: 1, MaxBatch: 1, Probes: true, Recheck: 10 * time.Minute}) {
		switch a.Do {
		case "push":
			pushed = append(pushed, fmt.Sprintf("kind %d, round %d of %d", a.Build.Kind, a.Build.Round, len(a.Build.Chain)))
		case "note":
			notes = append(notes, a.Text)
		}
	}
	if want := fmt.Sprintf("[kind %d, round 3 of 1]", Check); fmt.Sprint(pushed) != want {
		t.Errorf("pushed %v, want %s: a whole build of #1's tree, as probes passed on it", pushed, want)
	}
	if slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, redTrunk) }) {
		t.Errorf("notes %q say the trunk is failing, where its probe passed", notes)
	}
}

// The trunk fails by itself: #1's candidate failed, and so did the probes of the trunk alone and
// of #1 on it. #3, labelled first as #1 is, was then built alone and failed too. #1 on #3 is a
// tree no build has tried, so it is built next, where #1 once waited on its own red candidate
// and two fixes that pass only together never met. #1 on the trunk is not built again, and #2,
// which nobody labelled as a fix, waits.
func TestAFixARedCandidateHoldsIsStackedOnAnotherWhileTheTrunkFails(t *testing.T) {
	now := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	m1, m3 := SHA(`merge(trunk,1,"")`), SHA(`merge(trunk,3,"")`)
	one, three := []Member{member(1, m1)}, []Member{member(3, m3)}
	probe := func(sha SHA, chain []Member) Build {
		at := now.Add(time.Minute)
		return Build{SHA: sha, Kind: Probe, Base: "trunk", Chain: chain, Round: 1, Of: m1, Verdict: Failure, Pushed: at, Started: at}
	}
	prs := queuedAt(now, 1, 2, 3)
	prs[0].First, prs[2].First = true, true
	w := World{Now: now.Add(3 * time.Minute), Trunk: "trunk", Repo: commits{}, PRs: prs,
		Builds: []Build{
			{SHA: m1, Base: "trunk", Chain: one, Verdict: Failure, Pushed: now, Started: now},
			probe("p1a", nil), probe("p1b", one),
			{SHA: m3, Base: "trunk", Chain: three, Verdict: Failure, Pushed: now, Started: now},
		}}
	var pushed [][]int
	for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 8, Probes: true, Recheck: 10 * time.Minute}) {
		if a.Do == "push" {
			var ns []int
			for _, m := range a.Build.Chain {
				ns = append(ns, m.Number)
			}
			pushed = append(pushed, ns)
		}
	}
	if fmt.Sprint(pushed) != "[[3 1]]" {
		t.Errorf("pushed %v, want [[3 1]]: the two fixes together, and nothing else while the trunk fails", pushed)
	}
}

// While the trunk fails, a fix is never built again as the tree its red candidate tested. #1
// alone on the trunk has failed, so nothing is pushed for it. And where #1 and #2 failed
// together and #1 is being built again alone on a new commit, #2 on that commit is the same
// two pull requests in the same order, so it is not built either: a candidate stacked on it
// would read as stacked on the red one.
func TestAFixIsNotBuiltAgainAsTheTreeItsRedCandidateTested(t *testing.T) {
	now := time.Date(2026, 11, 14, 23, 30, 0, 0, time.UTC)
	m1 := SHA(`merge(trunk,1,"")`)
	m12 := SHA(fmt.Sprintf("merge(%s,2,%q)", m1, ""))
	one, both := []Member{member(1, m1)}, []Member{member(1, m1), member(2, m12)}
	held := func(red []Member, extra ...Build) World {
		of := red[len(red)-1].Merge
		builds := []Build{{SHA: of, Base: "trunk", Chain: red, Verdict: Failure, Pushed: now, Started: now}}
		for j := 0; j <= len(red); j++ {
			at := now.Add(time.Minute)
			builds = append(builds, Build{SHA: fmt.Sprintf("p%d", j), Kind: Probe, Base: "trunk", Chain: red[:j], Round: 1, Of: of, Verdict: Failure, Pushed: at, Started: at})
		}
		prs := queuedAt(now, 1, 2)[:len(red)]
		for i := range prs {
			prs[i].First = true
		}
		return World{Now: now.Add(3 * time.Minute), Trunk: "trunk", Repo: commits{}, PRs: prs, Builds: append(builds, extra...)}
	}
	again := Build{SHA: "a2", Base: "trunk", Chain: []Member{member(1, "a2")}, Verdict: Pending, Pushed: now, Started: now}
	for name, w := range map[string]World{"#1 alone": held(one), "#2 on #1 built again": held(both, again)} {
		for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 8, Probes: true, Recheck: 10 * time.Minute}) {
			if a.Do == "push" {
				t.Errorf("%s: pushed %+v, where the trunk fails and that tree has been tried", name, a.Build.Chain)
			}
		}
	}
}

// #1 and #2 pass alone and fail together, and two red candidates hold them in opposite orders,
// each with its test whole: one blames #2 on a tree holding #1, the other #1 on a tree holding
// #2. Each stands on the other, so the first leaves, and the queue does not wait on both with
// no end.
func TestOneLeavesWhereTwoTestsStandOnEachOther(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	ab := []Member{member(1, "a"), member(2, "ab")}
	ba := []Member{member(2, "b"), member(1, "ba")}
	check := func(sha, of SHA, chain []Member, round int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Check, Base: "trunk", Chain: chain, Round: round, Of: of, Verdict: verdict, Pushed: now, Started: now}
	}
	w := World{Now: now.Add(time.Hour), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2),
		Builds: []Build{
			{SHA: "ab", Base: "trunk", Chain: ab, Verdict: Failure, Pushed: now, Started: now},
			{SHA: "ba", Base: "trunk", Chain: ba, Verdict: Failure, Pushed: now, Started: now},
			check("a1", "ab", ab, 1, Failure), check("a2", "ab", ab[:1], 2, Success), check("a3", "ab", ab, 3, Failure),
			check("b1", "ba", ba, 1, Failure), check("b2", "ba", ba[:1], 2, Success), check("b3", "ba", ba, 3, Failure),
		}}
	if numbers, _ := ejections(Plan(w, Config{Slots: 2, MaxBatch: 2, Recheck: 10 * time.Minute})); len(numbers) != 1 {
		t.Errorf("ejected %v, where exactly one of the two leaves", numbers)
	}
}

func TestAProbeNamesTheContextsThatFailedOnTheRedBuild(t *testing.T) {
	now := time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)
	chain := []Member{member(1, "m1"), member(2, "m2")}
	red := Build{SHA: "m2", Base: "trunk", Chain: chain, Verdict: Failure, Failed: []string{"apple (ci)", "linux (ci)"}, Pushed: now, Started: now}
	w := World{Now: now.Add(time.Minute), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2), Builds: []Build{red}}
	probes, whole := 0, 0
	for _, a := range Plan(w, Config{Slots: 2, MaxBatch: 2, Probes: true}) {
		if a.Do == "push" && a.Build.Kind == Probe {
			probes++
			if !strings.Contains(a.Build.SHA, `Banyan-Probe-Of: m2\nBanyan-Probe-Failed: apple (ci)\nBanyan-Probe-Failed: linux (ci)`) {
				t.Errorf("a probe of the red build is marked %s, where it names the two contexts that failed on it", a.Build.SHA)
			}
		}
	}
	for _, a := range Plan(w, Config{Slots: 2, MaxBatch: 2}) {
		if a.Do == "push" && a.Build.Round > 0 {
			whole++
			if strings.Contains(a.Build.SHA, "Banyan-Probe") {
				t.Errorf("a whole build is marked %s, where it runs every job and names none", a.Build.SHA)
			}
		}
	}
	if probes == 0 || whole == 0 {
		t.Errorf("pushed %d probes and %d whole builds of a red candidate, where each plan pushes a round", probes, whole)
	}
}

func TestKeepLandedExpiryUsesOnlyTheMark(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for _, kind := range []Kind{Candidate, Check, Probe} {
		for _, verdict := range []Verdict{Pending, Success, Failure, Error} {
			for _, tc := range []struct {
				name string
				mark time.Time
				due  bool
			}{
				{"recent", now.Add(-time.Minute), false}, {"boundary", now.Add(-3 * time.Minute), true},
				{"old", now.Add(-time.Hour), true}, {"future", now.Add(time.Minute), false},
			} {
				t.Run(fmt.Sprintf("%d/%d/%s", kind, verdict, tc.name), func(t *testing.T) {
					b := Build{SHA: "landed", Kind: kind, Landed: tc.mark, Verdict: verdict, Pushed: now.Add(-24 * time.Hour)}
					w := World{Now: now, Trunk: "trunk", Repo: commits{}, Builds: []Build{b}}
					acts := Plan(w, Config{Keep: 3 * time.Minute, Timeout: time.Minute})
					var want []Action
					if tc.due {
						want = []Action{{Do: "delete", SHA: b.SHA}}
					}
					if !reflect.DeepEqual(acts, want) {
						t.Fatalf("got %+v, want %+v", acts, want)
					}
				})
			}
		}
	}
}

func TestKeepLandedPendingCandidateKeepsSlotWithoutCancellation(t *testing.T) {
	now := time.Now()
	for _, cancels := range []bool{false, true} {
		t.Run(fmt.Sprint(cancels), func(t *testing.T) {
			w := World{Now: now, Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1), Builds: []Build{{SHA: "landed", Base: "landed", Verdict: Pending, Started: now}}}
			c := Config{Cancels: cancels, Slots: 1, MaxBatch: 1}
			want := Plan(w, c)
			c.Keep = time.Minute
			acts := Plan(w, c)
			if !reflect.DeepEqual(acts, want) {
				t.Fatalf("got %+v, want keep unset %+v", acts, want)
			}
			pushed := slices.ContainsFunc(acts, func(a Action) bool { return a.Do == "push" && a.Build.Kind == Candidate })
			deleted := slices.ContainsFunc(acts, func(a Action) bool { return a.Do == "delete" && a.SHA == "landed" })
			if pushed != cancels || deleted != cancels {
				t.Fatalf("cancels=%v: pushed=%v deleted=%v: %+v", cancels, pushed, deleted, acts)
			}
		})
	}
}

func TestKeepUnmarkedBuildMatchesWithoutKeep(t *testing.T) {
	now := time.Now()
	for _, kind := range []Kind{Candidate, Check, Probe} {
		for _, verdict := range []Verdict{Pending, Success, Failure, Error} {
			for _, cancels := range []bool{false, true} {
				for _, timeout := range []time.Duration{0, time.Minute} {
					t.Run(fmt.Sprintf("%d/%d/%v/%s", kind, verdict, cancels, timeout), func(t *testing.T) {
						b := Build{SHA: "landed", Base: "landed", Kind: kind, Verdict: verdict, Pushed: now.Add(-time.Hour)}
						w := World{Now: now, Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1), Builds: []Build{b}}
						c := Config{Slots: 1, MaxBatch: 1, Cancels: cancels, Timeout: timeout}
						want := Plan(w, c)
						c.Keep = time.Hour
						if got := Plan(w, c); !reflect.DeepEqual(got, want) {
							t.Fatalf("got %+v, want keep unset %+v", got, want)
						}
					})
				}
			}
		}
	}
}

func TestKeepLandedBuildJoinsNoRedGroupOrRound(t *testing.T) {
	now := time.Now()
	chain := []Member{member(1, "m1")}
	for _, round := range []int{0, 1} {
		w := World{Now: now, Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1), Builds: []Build{
			{SHA: "m1", Base: "trunk", Chain: chain, Verdict: Failure},
			{SHA: "round", Kind: Check, Base: "trunk", Chain: chain, Round: 1, Of: "m1", Verdict: Pending, Pushed: now},
		}}
		if round == 0 {
			w.Builds[0].Landed = now
		} else {
			w.Builds[1].Landed = now
			w.Builds[1].Verdict = Success
		}
		c := Config{Keep: time.Minute, Slots: 1, MaxBatch: 1}
		p := &plan{w: w, c: c, open: map[int]PR{}, waits: map[int]string{}}
		p.admit()
		p.read()
		if round == 0 && len(p.reds) != 0 {
			t.Fatalf("landed candidate joined red group: %+v", p.reds)
		}
		for _, r := range p.reds {
			for _, b := range append(r.built, r.builds...) {
				if !b.Landed.IsZero() {
					t.Fatalf("landed build read as round: %+v", b)
				}
			}
		}
		without := w
		without.Builds = slices.DeleteFunc(slices.Clone(w.Builds), func(b Build) bool { return !b.Landed.IsZero() })
		if got, want := Plan(w, c), Plan(without, c); !reflect.DeepEqual(got, want) {
			t.Fatalf("round=%d got %+v, want %+v", round, got, want)
		}
	}
}

func TestKeepLandedBuildIsNeverStackedOnLandedOrPushedAgain(t *testing.T) {
	now := time.Now()
	for _, verdict := range []Verdict{Pending, Success, Failure} {
		w := World{Now: now, Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2), Builds: []Build{{SHA: "landed", Base: "trunk", Chain: []Member{member(1, "m1")}, Landed: now, Verdict: verdict}}}
		c := Config{Keep: time.Minute, Slots: 2, MaxBatch: 1}
		acts := Plan(w, c)
		w.Builds = nil
		if want := Plan(w, c); !reflect.DeepEqual(acts, want) {
			t.Fatalf("verdict=%d got %+v, want %+v", verdict, acts, want)
		}
		for _, a := range acts {
			if a.Do == "land" || a.Do == "push" && strings.Contains(a.Build.SHA, "landed") {
				t.Fatalf("reused landed commit: %+v", acts)
			}
		}
	}
}

func TestKeepVerifyRefusesAnAlreadyLandedCandidate(t *testing.T) {
	w := World{Trunk: "m", Builds: []Build{{SHA: "m", Base: "m", Verdict: Success}}}
	if err := Verify(w, Action{Do: "land", SHA: "m"}); err == nil {
		t.Fatal("already landed candidate was approved")
	}
}

func TestKeepUnlandedDroppedBuildsGoImmediately(t *testing.T) {
	now := time.Now()
	for _, kind := range []Kind{Candidate, Check, Probe} {
		for _, verdict := range []Verdict{Pending, Success, Failure, Error} {
			for _, cancels := range []bool{false, true} {
				b := Build{SHA: "old", Kind: kind, Base: "trunk", Chain: []Member{member(1, "m1")}, Verdict: verdict}
				w := World{Now: now, Trunk: "trunk", Repo: commits{}, Builds: []Build{b}}
				c := Config{Cancels: cancels}
				want := Plan(w, c)
				c.Keep = time.Hour
				got := Plan(w, c)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("kind=%d verdict=%d cancels=%v got %+v want %+v", kind, verdict, cancels, got, want)
				}
				retained := kind == Candidate && verdict == Pending && !cancels
				if !retained && (len(got) != 1 || got[0].Do != "delete") {
					t.Fatalf("unlanded discarded build survived: %+v", got)
				}
			}
		}
	}
}

func TestKeepUnlandedLifecyclesMatchWithoutKeep(t *testing.T) {
	now := time.Now()
	for _, kind := range []Kind{Check, Probe} {
		for _, event := range []string{"eject and relabel", "clear then pass", "end with running probe"} {
			t.Run(fmt.Sprintf("%d/%s", kind, event), func(t *testing.T) {
				red := Build{SHA: "m1", Base: "trunk", Chain: []Member{member(1, "m1")}, Verdict: Failure}
				w := World{Now: now, Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1), Builds: []Build{red}}
				for round := 1; round <= 3; round++ {
					chain, v := red.Chain, Failure
					if round == 2 {
						chain, v = nil, Success
					}
					w.Builds = append(w.Builds, Build{SHA: fmt.Sprint("round", round), Kind: kind, Base: "trunk", Chain: chain, Round: round, Of: "m1", Verdict: v})
				}
				if event != "eject and relabel" {
					w.Builds[1].Verdict = Success
					if kind == Probe {
						w.Builds[1].Kind = Check
					}
				}
				if event == "end with running probe" {
					w.Builds[3].Kind = Probe
					w.Builds[3].Verdict = Pending
				}
				c := Config{Slots: 1, MaxBatch: 1, Probes: kind == Probe}
				compare := func() []Action {
					t.Helper()
					want := Plan(w, c)
					kept := c
					kept.Keep = time.Hour
					got := Plan(w, kept)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("got %+v want %+v", got, want)
					}
					return got
				}
				apply := func(acts []Action) {
					t.Helper()
					for _, a := range acts {
						switch a.Do {
						case "delete":
							w.Builds = slices.DeleteFunc(w.Builds, func(b Build) bool { return b.SHA == a.SHA })
						case "eject":
							w.PRs = nil
						case "push":
							w.Builds = append(w.Builds, a.Build)
						case "note":
							for i := range w.PRs {
								w.PRs[i].Note = a.Text
							}
						}
					}
				}
				first := compare()
				if event == "eject and relabel" {
					ns, _ := ejections(first)
					if len(ns) != 1 {
						t.Fatalf("no first ejection: %+v", first)
					}
					apply(first)
					apply(compare())
					if len(w.Builds) != 0 {
						t.Fatalf("ended evidence survived: %+v", w.Builds)
					}
					w.PRs = queuedAt(now, 1)
					if ns, _ := ejections(compare()); len(ns) > 0 {
						t.Fatal("old evidence ejected relabelled head")
					}
				} else {
					for _, b := range w.Builds {
						if !slices.ContainsFunc(first, func(a Action) bool { return a.Do == "delete" && a.SHA == b.SHA }) {
							t.Fatalf("ended build %s survived: %+v", b.SHA, first)
						}
					}
					apply(first)
					for i := range w.Builds {
						w.Builds[i].Verdict = Success
					}
					acts := compare()
					if !slices.ContainsFunc(acts, func(a Action) bool { return a.Do == "land" }) {
						t.Fatalf("fresh candidate did not land: %+v", acts)
					}
				}
			})
		}
	}
}

func TestKeepZeroAddsNothingToARecording(t *testing.T) {
	b, err := json.Marshal(Build{SHA: "m"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"SHA":"m","Kind":0,"Base":"","Chain":null,"Round":0,"Of":"","Verdict":0,"Failed":null,"Pushed":"0001-01-01T00:00:00Z","Started":"0001-01-01T00:00:00Z"}`
	if string(b) != want {
		t.Fatalf("zero build JSON = %s, want %s", b, want)
	}
	c, err := json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"Slots":0,"MaxBatch":0,"Gather":0,"Probes":false,"Cancels":false,"Timeout":0,"Recheck":0}`
	if string(c) != want {
		t.Fatalf("zero config JSON = %s, want %s", c, want)
	}
}

// played is a queue played a minute a tick: each world is the last with the planner's own
// actions applied, and CI begins each build as it is pushed and answers it once it has run
// for as many ticks as takes says, failing it where fails says.
type played struct {
	t      *testing.T
	w      World
	c      Config
	seen   map[SHA]bool
	takes  func(Build) int
	fails  func(Build) bool
	acts   [][]Action
	pushed []Build
}

func (q *played) Merge(parent SHA, pr PR, note string) (SHA, bool) {
	return commits{}.Merge(parent, pr, note)
}
func (q *played) Mark(parent SHA, note string) SHA { return commits{}.Mark(parent, note) }
func (q *played) Seen(sha SHA) bool                { return q.seen[sha] }

func newPlayed(t *testing.T, c Config, takes func(Build) int, fails func(Build) bool, numbers ...int) *played {
	now := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)
	q := &played{t: t, c: c, seen: map[SHA]bool{}, takes: takes, fails: fails}
	q.w = World{Now: now, Trunk: "trunk", Repo: q, PRs: queuedAt(now.Add(-time.Hour), numbers...)}
	return q
}

// arrive queues pull requests now, behind those already queued.
func (q *played) arrive(numbers ...int) {
	for _, n := range numbers {
		q.w.PRs = append(q.w.PRs, PR{Number: n, Head: fmt.Sprintf("h%d", n), Queued: q.w.Now, Checks: Success})
	}
}

func (q *played) tick() []Action {
	q.w.Now = q.w.Now.Add(time.Minute)
	for i, b := range q.w.Builds {
		if b.Verdict == Pending && q.w.Now.Sub(b.Started) >= time.Duration(q.takes(b))*time.Minute {
			q.w.Builds[i].Verdict = Success
			if q.fails(b) {
				q.w.Builds[i].Verdict = Failure
			}
		}
	}
	acts := Plan(q.w, q.c)
	for _, a := range acts {
		switch a.Do {
		case "push":
			b := a.Build
			b.Started = q.w.Now
			q.seen[b.SHA] = true
			q.w.Builds = append(q.w.Builds, b)
			q.pushed = append(q.pushed, b)
		case "delete":
			q.w.Builds = slices.DeleteFunc(q.w.Builds, func(b Build) bool { return b.SHA == a.SHA })
		case "land":
			q.w.Trunk = a.SHA
			q.w.Builds = slices.DeleteFunc(q.w.Builds, func(b Build) bool { return b.SHA == a.SHA })
			q.w.PRs = slices.DeleteFunc(q.w.PRs, func(pr PR) bool {
				return slices.ContainsFunc(a.Members, func(m Member) bool { return m.Number == pr.Number })
			})
		case "eject":
			q.w.PRs = slices.DeleteFunc(q.w.PRs, func(pr PR) bool { return pr.Number == a.PR })
		}
	}
	q.acts = append(q.acts, acts)
	return acts
}

func (q *played) ticks(n int) {
	for range n {
		q.tick()
	}
}

func numbers(chain []Member) (ns []int) {
	for _, m := range chain {
		ns = append(ns, m.Number)
	}
	return ns
}

// log is every tick's pushed candidates, landings and ejections, one line a tick that took any.
func (q *played) log() string {
	var lines []string
	for i, acts := range q.acts {
		var did []string
		for _, a := range acts {
			switch {
			case a.Do == "push" && a.Build.Round == 0:
				did = append(did, fmt.Sprintf("push %v", numbers(a.Build.Chain)))
			case a.Do == "push":
				did = append(did, fmt.Sprintf("round %d of %d", a.Build.Round, len(a.Build.Chain)))
			case a.Do == "land":
				did = append(did, fmt.Sprintf("land %v", numbers(a.Members)))
			case a.Do == "eject":
				did = append(did, fmt.Sprintf("eject %d", a.PR))
			}
		}
		if len(did) > 0 {
			lines = append(lines, fmt.Sprintf("%d: %s", i+1, strings.Join(did, ", ")))
		}
	}
	return strings.Join(lines, "\n")
}

func holds(b Build, n int) bool {
	return slices.ContainsFunc(b.Chain, func(m Member) bool { return m.Number == n })
}

// when is the first tick, counted from 1, whose actions hold one that match matches, or 0.
func (q *played) when(match func(Action) bool) int {
	for i, acts := range q.acts {
		if slices.ContainsFunc(acts, match) {
			return i + 1
		}
	}
	return 0
}

// pushedAgain matches a candidate that holds pull request n pushed after the first tick.
func (q *played) pushedAgain(n int) func(Action) bool {
	first := q.pushed[0].Pushed
	return func(a Action) bool {
		return a.Do == "push" && a.Build.Round == 0 && holds(a.Build, n) && a.Build.Pushed.After(first)
	}
}

func ejected(n int) func(Action) bool {
	return func(a Action) bool { return a.Do == "eject" && a.PR == n }
}

// whole counts the whole builds the queue began: candidates, and rounds' builds of a tree.
func (q *played) whole() (n int) {
	for _, b := range q.pushed {
		if b.Kind != Probe {
			n++
		}
	}
	return n
}

// #1, #2 and #3 failed together, and #2 breaks the build. #4's candidate stacked on theirs
// builds slowly and holds a slot, and #4 and #5 arrive while the first round runs. With spare,
// the third slot stays free, and the round's read at tick 19 clears #1 and #3, which are
// pushed in that tick. Without it, #4 and #5 take the third slot and #1 and #3 wait for #4 to
// land. Either way #2 leaves at tick 21 on the same builds.
func TestWithSpareASlotWaitsForThePullRequestsARedCandidatesFirstRoundClears(t *testing.T) {
	play := func(spare bool) *played {
		var slow SHA
		takes := func(b Build) int {
			switch {
			case b.SHA == slow:
				return 40
			case b.Round > 0:
				return 8
			}
			return 10
		}
		c := Config{Slots: 3, MaxBatch: 3, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute, Spare: spare}
		q := newPlayed(t, c, takes, func(b Build) bool { return holds(b, 2) }, 1, 2, 3, 4)
		q.tick()
		slow = q.pushed[1].SHA
		q.ticks(11)
		q.arrive(5)
		q.ticks(30)
		return q
	}
	evidence := func(q *played) string {
		for _, acts := range q.acts {
			for _, a := range acts {
				if a.Do == "eject" {
					return fmt.Sprintf("#%d on %q", a.PR, a.Evidence)
				}
			}
		}
		return "nobody"
	}
	held, plain := play(true), play(false)
	if got := held.when(held.pushedAgain(5)); got != 19 {
		t.Errorf("with spare #5 was pushed at tick %d, where the free slot waits for the read at 19:\n%s", got, held.log())
	}
	if got := held.when(held.pushedAgain(1)); got != 19 {
		t.Errorf("with spare #1 was pushed at tick %d, where the round clears it at 19:\n%s", got, held.log())
	}
	if got := plain.when(plain.pushedAgain(5)); got != 13 {
		t.Errorf("without spare #5 was pushed at tick %d, where it takes the third slot at 13:\n%s", got, plain.log())
	}
	if got := plain.when(plain.pushedAgain(1)); got != 22 {
		t.Errorf("without spare #1 was pushed at tick %d, where it waits for #4 to land at 21, and the tick after:\n%s", got, plain.log())
	}
	if a, b := evidence(held), evidence(plain); a != b || held.when(ejected(2)) != 21 || plain.when(ejected(2)) != 21 {
		t.Errorf("with spare ejected %s at tick %d, and without it %s at tick %d, where #2 leaves at 21 on the same builds", a, held.when(ejected(2)), b, plain.when(ejected(2)))
	}
}

// Spare keeps no slot for a red candidate of one pull request, which nothing can clear, for one
// whose base the trunk has moved past, for a red trunk, for a flake, nor with one slot. Each
// world pushes the same candidates with spare as without.
func TestSpareKeepsNoSlotWhereNoFirstRoundOnTheTrunkIsAboutToClearAnybody(t *testing.T) {
	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	two := []Member{member(1, "m1"), member(2, "m2")}
	probe := func(sha, base SHA, chain []Member, round int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Probe, Base: base, Chain: chain, Round: round, Of: two[len(two)-1].Merge, Verdict: verdict, Pushed: now, Started: now}
	}
	red := func(base SHA) Build {
		return Build{SHA: "m2", Base: base, Chain: two, Verdict: Failure, Pushed: now, Started: now}
	}
	fixes := queuedAt(now, 1, 2, 4, 5)
	fixes[2].First, fixes[3].First = true, true
	cfg := Config{Slots: 3, MaxBatch: 1, Probes: true, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}
	one := cfg
	one.Slots = 1
	for _, tc := range []struct {
		name   string
		prs    []PR
		builds []Build
		c      Config
		want   int
	}{
		{"a red of one pull request", queuedAt(now, 1, 4, 5, 6), []Build{{SHA: "m1", Base: "trunk", Chain: two[:1], Verdict: Failure, Pushed: now, Started: now}}, cfg, 3},
		{"a red on a base below the trunk", queuedAt(now, 1, 2, 4, 5, 6), []Build{red("old"), probe("p0", "old", nil, 1, Pending), probe("p1", "old", two[:1], 1, Pending)}, cfg, 3},
		{"a red trunk", fixes, []Build{red("trunk"), probe("p0", "trunk", nil, 1, Failure), probe("p1", "trunk", two[:1], 1, Failure)}, cfg, 2},
		{"a flake", queuedAt(now, 1, 2, 4, 5), []Build{red("trunk"), probe("p0", "trunk", nil, 1, Success), probe("p1", "trunk", two[:1], 1, Success), probe("p2", "trunk", two, 2, Success)}, cfg, 3},
		{"one slot", queuedAt(now, 1, 2, 4), []Build{red("trunk"), probe("p0", "trunk", nil, 1, Pending), probe("p1", "trunk", two[:1], 1, Pending)}, one, 1},
	} {
		w := World{Now: now.Add(time.Minute), Trunk: "trunk", Repo: commits{}, PRs: tc.prs, Builds: tc.builds}
		count := func(c Config) (n int) {
			for _, a := range Plan(w, c) {
				if a.Do == "push" && a.Build.Round == 0 {
					n++
				}
			}
			return n
		}
		spare := tc.c
		spare.Spare = true
		if got, plain := count(spare), count(tc.c); got != tc.want || plain != tc.want {
			t.Errorf("%s: pushed %d candidates with spare and %d without, where both push %d", tc.name, got, plain, tc.want)
		}
	}
}

// The slot spare keeps for #1, #2 and #3 is given back when the red candidate goes, as #3's
// label comes off mid-round, and when its round outlives the timeout: the round then reads as
// lost, so the hold lasts no longer than the timeout after the round was pushed.
func TestTheSlotSpareKeepsIsGivenBackWhenTheRedGoesOrItsRoundOutlivesTheTimeout(t *testing.T) {
	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	three := []Member{member(1, "m1"), member(2, "m2"), member(3, "m3")}
	cfg := Config{Slots: 3, MaxBatch: 1, Probes: true, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute, Spare: true}
	count := func(after time.Duration, numbers ...int) (n int) {
		w := World{Now: now.Add(after), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, numbers...),
			Builds: []Build{{SHA: "m3", Base: "trunk", Chain: three, Verdict: Failure, Pushed: now, Started: now}}}
		for j := range 3 {
			w.Builds = append(w.Builds, Build{SHA: fmt.Sprint("p", j), Kind: Probe, Base: "trunk", Chain: three[:j], Round: 1, Of: "m3", Pushed: now})
		}
		for _, a := range Plan(w, cfg) {
			if a.Do == "push" && a.Build.Round == 0 {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		name    string
		after   time.Duration
		numbers []int
		want    int
	}{
		{"the round running", 5 * time.Minute, []int{1, 2, 3, 4, 5, 6}, 2},
		{"#3 gone", 5 * time.Minute, []int{1, 2, 4, 5, 6}, 3},
		{"the round at the timeout", 90 * time.Minute, []int{1, 2, 3, 4, 5, 6}, 2},
		{"the round past the timeout", 91 * time.Minute, []int{1, 2, 3, 4, 5, 6}, 3},
	} {
		if got := count(tc.after, tc.numbers...); got != tc.want {
			t.Errorf("%s: pushed %d candidates, want %d", tc.name, got, tc.want)
		}
	}
}

// #1, #2 and #3 failed together, and #4's candidate stacked on theirs is building. Probes
// will say within a fraction of a build whether it is doomed, so nothing is pushed while the
// first round runs, with spare or without. Once the round has read, clearing #1 and #3,
// spare builds them beside the stacked candidate, where the queue otherwise waits for it.
func TestWithSpareTheChainGoesOnBesideAStackedCandidateOnceTheFirstRoundReads(t *testing.T) {
	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	three := []Member{member(1, "m1"), member(2, "m2"), member(3, "m3")}
	probe := func(sha SHA, j int, verdict Verdict) Build {
		return Build{SHA: sha, Kind: Probe, Base: "trunk", Chain: three[:j], Round: 1, Of: "m3", Verdict: verdict, Pushed: now, Started: now}
	}
	stacked := Build{SHA: "m4", Base: "trunk", Chain: append(slices.Clone(three), member(4, "m4")), Verdict: Pending, Pushed: now, Started: now}
	pushed := func(spare bool, verdicts ...Verdict) (chains [][]int) {
		w := World{Now: now.Add(5 * time.Minute), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2, 3, 4, 5),
			Builds: []Build{{SHA: "m3", Base: "trunk", Chain: three, Verdict: Failure, Pushed: now, Started: now}, stacked,
				probe("p0", 0, verdicts[0]), probe("p1", 1, verdicts[1]), probe("p2", 2, verdicts[2])}}
		for _, a := range Plan(w, Config{Slots: 3, MaxBatch: 3, Probes: true, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute, Spare: spare}) {
			if a.Do == "push" && a.Build.Round == 0 {
				chains = append(chains, numbers(a.Build.Chain))
			}
		}
		return chains
	}
	for _, spare := range []bool{false, true} {
		if got := pushed(spare, Pending, Pending, Pending); len(got) > 0 {
			t.Errorf("spare %v: pushed %v while the first round runs, where the chain waits for it", spare, got)
		}
	}
	if got := pushed(false, Success, Success, Failure); len(got) > 0 {
		t.Errorf("without spare pushed %v once the round read, where the chain waits for the stacked candidate", got)
	}
	if got := pushed(true, Success, Success, Failure); fmt.Sprint(got) != "[[1 3 4] [1 3 4 5]]" {
		t.Errorf("with spare pushed %v once the round read, where #1 and #3 are built beside the stacked candidate", got)
	}
}

// #1, #2 and #3's candidate failed by chance, and #4's stacked on it will land all four. With
// spare, the first round's read clears #1 and #2, and they are built with #4 beside the stacked
// candidate. Where the stacked one reports first, it lands all four at the same tick as without
// spare, and the build of #1, #2 and #4 is one whole build more, thrown away before it reports.
// Where it builds slowly, #1, #2 and #4 land at tick 23 and #3 at 34, where all four would
// have landed at 36: two whole builds more, and the stacked candidate, which would have
// passed, is thrown away.
func TestWithSpareAFlakeBuildsTheClearedPullRequestsBesideTheCandidateThatLandsThem(t *testing.T) {
	play := func(spare bool, slow int) *played {
		var red, stacked SHA
		takes := func(b Build) int {
			switch {
			case b.Kind == Probe:
				return 2
			case b.SHA == stacked:
				return slow
			}
			return 10
		}
		c := Config{Slots: 2, MaxBatch: 3, Probes: true, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute, Spare: spare}
		q := newPlayed(t, c, takes, func(b Build) bool { return b.SHA == red }, 1, 2, 3)
		q.tick()
		red = q.pushed[0].SHA
		q.ticks(4)
		q.arrive(4)
		q.tick()
		stacked = q.pushed[1].SHA
		q.ticks(60)
		return q
	}
	landings := func(q *played) string {
		var out []string
		for i, acts := range q.acts {
			for _, a := range acts {
				if a.Do == "land" {
					out = append(out, fmt.Sprintf("%v at %d", numbers(a.Members), i+1))
				}
			}
		}
		return strings.Join(out, ", ")
	}
	for _, tc := range []struct {
		spare          bool
		slow, whole    int
		landed, thrown string
	}{
		{false, 10, 2, "[1 2 3 4] at 16", ""},
		{true, 10, 3, "[1 2 3 4] at 16", "[1 2 4]"},
		{false, 30, 3, "[1 2 3 4] at 36", ""},
		{true, 30, 5, "[1 2 4] at 23, [3] at 34", "[1 2 3 4]"},
	} {
		q := play(tc.spare, tc.slow)
		var thrown []string
		for _, b := range q.pushed {
			if b.Kind == Candidate && !slices.ContainsFunc(q.acts, func(acts []Action) bool {
				return slices.ContainsFunc(acts, func(a Action) bool { return a.Do == "land" && a.SHA == b.SHA })
			}) && b.SHA != q.pushed[0].SHA {
				thrown = append(thrown, fmt.Sprint(numbers(b.Chain)))
			}
		}
		if got := landings(q); got != tc.landed || q.whole() != tc.whole || strings.Join(thrown, " ") != tc.thrown {
			t.Errorf("spare %v, stacked candidate %d ticks: landed %s on %d whole builds, throwing away %v, where it lands %s on %d, throwing away %q:\n%s",
				tc.spare, tc.slow, got, q.whole(), thrown, tc.landed, tc.whole, tc.thrown, q.log())
		}
	}
}

// #2 breaks the build, and the first round of #1 to #4's red candidate narrows the suspects to
// #2 alone. #1, #3 and #4 go back on the chain at once and land at tick 31, while the third
// build of #2's fair test is still running. A pull request under the red candidate has left,
// so the red candidate goes with its evidence, and #2 is free again: it is built into the next
// candidate at tick 32, fails again, and leaves at 54 on a fair test begun over.
func TestASuspectWalksBackInWhenAPullRequestBelowItLandsBeforeItsFairTestEnds(t *testing.T) {
	c := Config{Slots: 3, MaxBatch: 4, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}
	q := newPlayed(t, c, func(Build) int { return 10 }, func(b Build) bool { return holds(b, 2) }, 1, 2, 3, 4)
	q.ticks(60)
	want := `1: push [1 2 3 4]
11: round 1 of 1, round 1 of 2, round 1 of 0
21: round 2 of 1, push [1 3 4]
22: round 3 of 2
31: land [1 3 4]
32: push [2]
42: round 1 of 1
43: round 2 of 0
44: round 3 of 1
54: eject 2`
	if got := q.log(); got != want {
		t.Errorf("the queue did\n%s\nwhere #2 walks back in at 32 and leaves at 54:\n%s", got, want)
	}
}

// As above, with #5, #6 and #7 arriving while the first round runs. Without spare they fill
// the slots, #1, #3 and #4 are pushed only at tick 24, and #2's fair test ends first: it leaves
// at 32. With spare the third slot waits for them, they are pushed at 21 and land at 31, and
// #2 walks back in at 32 and leaves at 54.
func TestSpareLetsTheClearedLandBeforeTheSuspectsFairTestEnds(t *testing.T) {
	play := func(spare bool) *played {
		c := Config{Slots: 3, MaxBatch: 4, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute, Spare: spare}
		q := newPlayed(t, c, func(Build) int { return 10 }, func(b Build) bool { return holds(b, 2) }, 1, 2, 3, 4)
		q.ticks(12)
		q.arrive(5)
		q.ticks(2)
		q.arrive(6)
		q.ticks(2)
		q.arrive(7)
		q.ticks(50)
		return q
	}
	for _, tc := range []struct {
		spare                bool
		cleared, again, left int
	}{{false, 24, 0, 32}, {true, 21, 32, 54}} {
		q := play(tc.spare)
		again := q.when(func(a Action) bool { return a.Do == "push" && fmt.Sprint(numbers(a.Build.Chain)) == "[2]" })
		if q.when(q.pushedAgain(1)) != tc.cleared || again != tc.again || q.when(ejected(2)) != tc.left {
			t.Errorf("spare %v: #1 pushed at %d, #2 pushed again at %d and ejected at %d, where %d, %d and %d:\n%s",
				tc.spare, q.when(q.pushedAgain(1)), again, q.when(ejected(2)), tc.cleared, tc.again, tc.left, q.log())
		}
	}
}
