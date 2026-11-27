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

// ticks is a queue played tick by tick: each world is the last one with the planner's own
// pushes, deletes, landings and ejections applied, and whatever CI said since. Its git
// remembers what was pushed, so a build begun again is a new commit.
type ticks struct {
	t     *testing.T
	w     World
	c     Config
	seen  map[SHA]bool
	acts  [][]Action
	built []Build
}

func (s *ticks) Merge(parent SHA, pr PR, note string) (SHA, bool) {
	return commits{}.Merge(parent, pr, note)
}
func (s *ticks) Mark(parent SHA, note string) SHA { return commits{}.Mark(parent, note) }
func (s *ticks) Seen(sha SHA) bool                { return s.seen[sha] }

func newTicks(t *testing.T, c Config, numbers ...int) *ticks {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	s := &ticks{t: t, c: c, seen: map[SHA]bool{}}
	s.w = World{Now: now, Trunk: "trunk", Repo: s, PRs: queuedAt(now.Add(-time.Hour), numbers...)}
	return s
}

// tick plans the world a minute on, and applies what the plan says. CI begins every build
// it is pushed at once.
func (s *ticks) tick() []Action {
	s.w.Now = s.w.Now.Add(time.Minute)
	acts := Plan(s.w, s.c)
	for _, a := range acts {
		switch a.Do {
		case "push":
			b := a.Build
			b.Started = s.w.Now
			s.seen[b.SHA] = true
			s.w.Builds = append(s.w.Builds, b)
			s.built = append(s.built, b)
		case "delete":
			s.w.Builds = slices.DeleteFunc(s.w.Builds, func(b Build) bool { return b.SHA == a.SHA })
		case "land":
			s.w.Trunk = a.SHA
			s.w.Builds = slices.DeleteFunc(s.w.Builds, func(b Build) bool { return b.SHA == a.SHA })
			s.w.PRs = slices.DeleteFunc(s.w.PRs, func(pr PR) bool {
				return slices.ContainsFunc(a.Members, func(m Member) bool { return m.Number == pr.Number })
			})
		case "eject":
			s.w.PRs = slices.DeleteFunc(s.w.PRs, func(pr PR) bool { return pr.Number == a.PR })
		}
	}
	s.acts = append(s.acts, acts)
	return acts
}

// ci says what CI has said of each standing build that answer judges, by its SHA.
func (s *ticks) ci(answer func(b Build) (Verdict, []string, bool)) {
	for i, b := range s.w.Builds {
		if v, failed, ok := answer(b); ok {
			s.w.Builds[i].Verdict, s.w.Builds[i].Failed = v, failed
		}
	}
}

func holds(b Build, n int) bool {
	return slices.ContainsFunc(b.Chain, func(m Member) bool { return m.Number == n })
}

// ci is a CI that answers every build pushed so far but the candidate the test speaks for,
// failing those fails says fail, with job, and passing the rest.
func ci(fails func(Build) bool, job string, except SHA) func(Build) (Verdict, []string, bool) {
	return func(b Build) (Verdict, []string, bool) {
		if b.SHA == except || b.Verdict != Pending {
			return 0, nil, false
		}
		if fails(b) {
			return Failure, []string{job}, true
		}
		return Success, nil, true
	}
}

// breaks says a build fails because it holds culprit: a whole build always, and a probe where
// it runs job. A probe that names no job runs none.
func breaks(culprit int, job string) func(Build) bool {
	return func(b Build) bool {
		return holds(b, culprit) && (b.Kind != Probe || strings.Contains(b.SHA, "Banyan-Probe-Failed: "+job))
	}
}

func (s *ticks) set(sha SHA, v Verdict, failed ...string) {
	s.ci(func(b Build) (Verdict, []string, bool) { return v, failed, b.SHA == sha })
}

// landed is the pull requests the trunk took, in the order it took them, and ejected those
// that left.
func (s *ticks) outcome() (landed [][]int, ejected []int) {
	for _, acts := range s.acts {
		for _, a := range acts {
			switch a.Do {
			case "land":
				landed = append(landed, chainNumbers(a.Members))
			case "eject":
				ejected = append(ejected, a.PR)
			}
		}
	}
	return landed, ejected
}

// when is the first tick that took an action a matches, or -1.
func (s *ticks) when(match func(Action) bool) int {
	for i, acts := range s.acts {
		if slices.ContainsFunc(acts, match) {
			return i
		}
	}
	return -1
}

func chainNumbers(chain []Member) (ns []int) {
	for _, m := range chain {
		ns = append(ns, m.Number)
	}
	return ns
}

var early = Config{Slots: 2, MaxBatch: 2, Probes: true, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}

// #2 breaks linux. Read at its gate, the candidate of #1 and #2 goes red ten ticks after it
// was pushed. Read early, linux's failure dooms it at the first. From the tick each reads it
// red, the two queues act alike, tick for tick, and the gate's own failure changes nothing in
// the early one: #1 lands, and #2 leaves nine ticks sooner. The early queue deletes the
// candidate's branch as #2 leaves, before its gate has reported.
func TestACandidateReadFailedEarlyIsNarrowedAsItWouldBeAtItsGate(t *testing.T) {
	play := func(red int) *ticks {
		s := newTicks(t, early, 1, 2)
		s.tick()
		c := s.candidate(1, 2)
		for i := 1; i < 30; i++ {
			if i == red || i == 10 {
				s.set(c.SHA, Failure, "linux")
			}
			s.ci(ci(breaks(2, "linux"), "linux", c.SHA))
			s.tick()
		}
		return s
	}
	gate, doomed := play(10), play(1)
	same := func(acts []Action) string {
		var out []Action
		for _, a := range acts {
			a.Build.Pushed = time.Time{}
			out = append(out, a)
		}
		data, _ := json.Marshal(out)
		return string(data)
	}
	for i := 0; i < 20; i++ {
		if a, b := same(doomed.acts[1+i]), same(gate.acts[10+i]); a != b {
			t.Errorf("%d ticks after the red was read, the early queue did\n%s\nwhere the one that read it at its gate did\n%s", i, a, b)
		}
	}
	for _, s := range []*ticks{gate, doomed} {
		if landed, ejected := s.outcome(); fmt.Sprint(landed, ejected) != "[[1]] [2]" {
			t.Errorf("landed %v and ejected %v, where #1 lands and #2 leaves", landed, ejected)
		}
	}
	left := func(s *ticks) int { return s.when(func(a Action) bool { return a.Do == "eject" }) }
	if left(gate)-left(doomed) != 9 {
		t.Errorf("#2 left at tick %d read early and %d read at the gate, where it leaves nine ticks sooner", left(doomed), left(gate))
	}
	c := doomed.built[0].SHA
	if gone := doomed.when(func(a Action) bool { return a.Do == "delete" && a.SHA == c }); gone < 0 || gone >= 10 {
		t.Errorf("the early queue deleted the candidate's branch at tick %d, where it goes as #2 leaves, before the gate reports at 10", gone)
	}
}

func (s *ticks) candidate(numbers ...int) Build {
	s.t.Helper()
	for _, b := range s.w.Builds {
		if b.Kind == Candidate && b.Round == 0 && fmt.Sprint(chainNumbers(b.Chain)) == fmt.Sprint(numbers) {
			return b
		}
	}
	s.t.Fatalf("no candidate of %v stands: %+v", numbers, s.w.Builds)
	return Build{}
}

// noted is how a commit of the fake git spells a line of its note.
func noted(line string) string { return strings.Trim(fmt.Sprintf("%q", line), `"`) }

// emptyProbes is the probes pushed at or after tick from that name no build to probe and no
// job to run.
func (s *ticks) emptyProbes(from int) (n int) {
	for _, acts := range s.acts[from:] {
		for _, a := range acts {
			if a.Do == "push" && a.Build.Kind == Probe && strings.HasSuffix(a.Build.SHA, `Banyan-Probe-Of: ")`) {
				n++
			}
		}
	}
	return n
}

// #1 and #2's candidate read failed on linux, and its first round of probes began. Then
// linux was retried by hand and the candidate's whole build passed: nothing is broken. Nothing
// reads that pass. The candidate never lands, and its rounds go on as though it had failed,
// every probe pushed since naming no failed build and no job. Where those probes pass, #1 and
// #2 land one at a time, each built again. Where the probes of #1 fail, as a flaky job's may,
// #1 leaves, though every whole build that held it passed.
func TestACandidateThatPassesAfterReadingFailedIsNarrowedAndNeverLands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fails   func(Build) bool
		landed  string
		ejected string
	}{
		{"its probes pass", func(Build) bool { return false }, "[[1] [2]]", "[]"},
		{"its probes of #1 fail", func(b Build) bool { return b.Kind == Probe && holds(b, 1) }, "[[2]]", "[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTicks(t, early, 1, 2)
			s.tick()
			c := s.candidate(1, 2)
			s.set(c.SHA, Failure, "linux")
			s.tick()
			s.set(c.SHA, Success)
			for i := 0; i < 20; i++ {
				s.ci(ci(tc.fails, "linux", c.SHA))
				s.tick()
			}
			if s.when(func(a Action) bool { return a.Do == "land" && a.SHA == c.SHA }) >= 0 {
				t.Errorf("the candidate landed on its pass")
			}
			if landed, ejected := s.outcome(); fmt.Sprint(landed) != tc.landed || fmt.Sprint(ejected) != tc.ejected {
				t.Errorf("landed %v and ejected %v, want %s and %s", landed, ejected, tc.landed, tc.ejected)
			}
			if s.emptyProbes(2) == 0 {
				t.Errorf("no probe pushed after the pass names nothing, where each does")
			}
		})
	}
}

// #1 and #2's candidate read failed on linux, and its first round of probes began. Then
// linux was retried, and the candidate reads pending again a tick later. It stays the red
// candidate: its branch stands, it is not built again, and its rounds go on with probes that
// name no failed build and no job, while the pull request its first round cleared is built
// on its own.
//
// Where #1 breaks linux and the gate fails at tick 3, the probes pushed after it name the
// candidate and linux again, and #1 leaves on the candidate's own rounds: the pass between its
// two failures is a probe of the trunk pushed while the candidate read pending, which ran no
// job at all. Where the gate fails
// only at tick 8, a probe of #1 that ran nothing has passed by then: the rounds read #1 as
// cleared and #2, which has landed, as the suspect, and the candidate and its rounds go. #1
// is built again, goes red again, and leaves on that candidate's rounds. Where nothing is
// broken and the gate passes, nobody leaves and both land, each built again.
func TestACandidateWhoseFailedJobClearsStaysRedWhileItsRoundsGoOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      int
		gate    Verdict
		fails   func(Build) bool
		landed  string
		ejected string
		own     bool
		empty   bool
	}{
		{"#1 breaks linux, the gate fails at 3", 3, Failure, breaks(1, "linux"), "[[2]]", "[1]", true, true},
		{"#1 breaks linux, the gate fails at 8", 8, Failure, breaks(1, "linux"), "[[2]]", "[1]", false, false},
		{"nothing is broken", 8, Success, func(Build) bool { return false }, "[[1] [2]]", "[]", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTicks(t, early, 1, 2)
			s.tick()
			c := s.candidate(1, 2)
			s.set(c.SHA, Failure, "linux")
			s.tick()
			s.set(c.SHA, Pending)
			for i := 2; i < 30; i++ {
				if i == tc.at {
					s.set(c.SHA, tc.gate, "linux")
				}
				s.ci(ci(tc.fails, "linux", c.SHA))
				s.tick()
			}
			if gone := s.when(func(a Action) bool { return a.Do == "delete" && a.SHA == c.SHA }); gone >= 0 && gone < tc.at {
				t.Errorf("the candidate's branch was deleted at tick %d, while it read pending", gone)
			}
			for _, b := range s.built[1:] {
				if b.Kind == Candidate && fmt.Sprint(chainNumbers(b.Chain)) == "[1 2]" {
					t.Errorf("the candidate's tree was built again as %s", b.SHA)
				}
			}
			if landed, ejected := s.outcome(); fmt.Sprint(landed) != tc.landed || fmt.Sprint(ejected) != tc.ejected {
				t.Errorf("landed %v and ejected %v, want %s and %s", landed, ejected, tc.landed, tc.ejected)
			}
			if s.emptyProbes(2)-s.emptyProbes(tc.at) == 0 {
				t.Errorf("no probe pushed while the candidate read pending names nothing, where each does")
			}
			for _, acts := range s.acts[tc.at:] {
				for _, a := range acts {
					if a.Do == "push" && a.Build.Kind == Probe && a.Build.Of == c.Tests() && !strings.Contains(a.Build.SHA, noted("Banyan-Probe-Of: "+c.SHA+"\nBanyan-Probe-Failed: linux")) {
						t.Errorf("a probe pushed after the gate failed is %s, where it names the candidate and linux", a.Build.SHA)
					}
				}
			}
			for _, acts := range s.acts {
				for _, a := range acts {
					if a.Do == "eject" && strings.Contains(a.Evidence[0], noted("Banyan-Of: "+c.Tests())) != tc.own {
						t.Errorf("#%d left on %v, where it leaves on the candidate's own rounds: %v", a.PR, a.Evidence, tc.own)
					}
					if a.Do == "eject" && strings.HasSuffix(a.Evidence[0], `Banyan-Probe-Of: ")`) != tc.empty {
						t.Errorf("#%d left on a pass without it of %s, where that pass ran no job: %v", a.PR, a.Evidence[0], tc.empty)
					}
				}
			}
		})
	}
}

// #1 breaks mac, which the candidate of #1 and #2 reports only at its gate. It read failed
// early on linux, which flaked, so its first rounds probe linux alone. They clear #1 and put
// #2 to the test. Where linux flakes once more on #2's first probe, #2's test reads: #1 and #2
// failed, on linux; #1 alone passed, running linux alone; #1 and #2 failed again, on mac, once
// the gate had named it. #2 leaves, though it broke nothing, and #1 leaves later on a
// candidate of its own. Where linux does not flake again, the probes of #1 and #2 pass, a
// whole build of the red tree decides, and only #1 leaves.
func TestAGateThatFailsMoreJobsLaterCanBlameAnInnocentOnAPassThatRanFewer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flakes  int
		ejected string
	}{
		{"linux flakes once more", 1, "[2 1]"},
		{"linux does not flake again", 0, "[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTicks(t, early, 1, 2)
			s.tick()
			c := s.candidate(1, 2)
			s.set(c.SHA, Failure, "linux")
			flakes := tc.flakes
			mac := func(b Build) bool {
				return holds(b, 1) && (b.Kind != Probe || strings.Contains(b.SHA, noted("Banyan-Probe-Failed: mac")))
			}
			for i := 1; i < 30; i++ {
				s.tick()
				if i == 3 {
					s.set(c.SHA, Failure, "linux", "mac")
				}
				s.ci(ci(func(b Build) bool {
					if b.Kind == Probe && len(b.Chain) == 2 && flakes > 0 && !mac(b) {
						flakes--
						return true
					}
					return mac(b)
				}, "mac", c.SHA))
			}
			if _, ejected := s.outcome(); fmt.Sprint(ejected) != tc.ejected {
				t.Errorf("ejected %v, want %s", ejected, tc.ejected)
			}
			ranMac := noted("Banyan-Probe-Failed: mac")
			for _, acts := range s.acts {
				for _, a := range acts {
					if a.Do == "eject" && a.PR == 2 && (strings.Contains(a.Evidence[0], ranMac) || !strings.Contains(a.Evidence[2], ranMac)) {
						t.Errorf("#2 left on %v, where the pass without it ran linux alone and the failure after it ran mac", a.Evidence)
					}
				}
			}
		})
	}
}
