package queue

import (
	"fmt"
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

// #1 and #2 failed together, and the first round has waited past the timeout. CI never began
// its build of both, so that one goes and is built again: left in place it would read as
// begun, later rounds would start, and a failure CI began after their pass would stand as the
// failure before it. CI did begin its build of #1 alone, which stays and reads as lost until
// it reports: it began before any round after it, and deleting a build that is running would
// lose every verdict of a CI slower than the timeout.
func TestARoundsBuildThatNeverBeganInsideTheTimeoutGoes(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	both := []Member{member(1, "m1"), member(2, "m2")}
	w := World{Now: now.Add(2 * time.Hour), Trunk: "trunk", Repo: commits{}, PRs: queuedAt(now, 1, 2),
		Builds: []Build{
			{SHA: "m2", Base: "trunk", Chain: both, Verdict: Failure, Pushed: now, Started: now},
			{SHA: "both1", Kind: Check, Base: "trunk", Chain: both, Round: 1, Of: "m2", Pushed: now},
			{SHA: "one1", Kind: Check, Base: "trunk", Chain: both[:1], Round: 1, Of: "m2", Pushed: now, Started: now},
		}}
	var deleted []SHA
	for _, a := range Plan(w, Config{Slots: 2, MaxBatch: 2, Timeout: 90 * time.Minute, Recheck: 10 * time.Minute}) {
		if a.Do == "delete" {
			deleted = append(deleted, a.SHA)
		}
	}
	if fmt.Sprint(deleted) != "[both1]" {
		t.Errorf("deleted %v, where only the build CI never began goes", deleted)
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
