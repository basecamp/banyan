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
