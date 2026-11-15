package git

import (
	"fmt"
	"strings"
	"testing"

	"github.com/basecamp/banyan/internal/gittest"
	"github.com/basecamp/banyan/internal/queue"
)

type origin struct {
	*gittest.Origin
	t    *testing.T
	Repo *Repo
}

func newOrigin(t *testing.T) *origin {
	o := gittest.New(t)
	return &origin{Origin: o, t: t, Repo: &Repo{Dir: o.Clone, Remote: "origin", Trunk: "main", Lane: "merge-queue", Name: "Banyan", Email: "banyan@example.com"}}
}

// pr opens pull request n, which writes content to file, and returns it queued.
func (o *origin) pr(n int, file, content string) queue.PR {
	head := o.PR(n, file, content)
	return queue.PR{Number: n, Head: head, Checks: queue.Success}
}

func (o *origin) fetch() queue.SHA {
	var ns []int
	for n := range o.Heads {
		ns = append(ns, n)
	}
	trunk, heads, err := o.Repo.Fetch(ns)
	if err != nil {
		o.t.Fatal(err)
	}
	for n, h := range heads {
		if h != o.Heads[n] {
			o.t.Fatalf("#%d fetched at %s, pushed at %s", n, h, o.Heads[n])
		}
	}
	return trunk
}

func TestTheSameQueueBuildsTheSameCommits(t *testing.T) {
	o := newOrigin(t)
	a, b := o.pr(1, "a", "a\n"), o.pr(2, "b", "b\n")
	trunk := o.fetch()
	one, ok := o.Repo.Merge(trunk, a, "")
	two, ok2 := o.Repo.Merge(one, b, "")
	if !ok || !ok2 {
		t.Fatal("clean merges were called conflicts")
	}

	again := *o.Repo
	again.commits, again.made = nil, nil
	if x, _ := again.Merge(trunk, a, ""); x != one {
		t.Errorf("a second merge of the same parents gave %s, not %s", x, one)
	}
	if x, _ := o.Repo.Merge(trunk, a, "Banyan-Try: 2"); x == one {
		t.Error("a second try is the same commit as the first")
	}
	if x := o.Repo.Mark(two, "Banyan-Try: 1\nBanyan-Check: whole"); x == two || x == "" {
		t.Errorf("a mark on %s is %q", two, x)
	}
}

func TestAConflictIsNoMerge(t *testing.T) {
	o := newOrigin(t)
	a, b := o.pr(1, "same", "one\n"), o.pr(2, "same", "two\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	if _, ok := o.Repo.Merge(one, b, ""); ok {
		t.Error("two pull requests that write one file differently merged")
	}
	if _, ok := o.Repo.Merge(trunk, b, ""); !ok {
		t.Error("the second merges onto the trunk alone, and was called a conflict")
	}
}

func TestBuildsAreReadBackAsPushed(t *testing.T) {
	o := newOrigin(t)
	a, b := o.pr(1, "a", "a\n"), o.pr(2, "b", "b\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	two, _ := o.Repo.Merge(one, b, "")
	retry, _ := o.Repo.Merge(one, b, "Banyan-Try: 2\nBanyan-Round: 1\nBanyan-Of: "+two)
	check := o.Repo.Mark(trunk, "Banyan-Try: 1\nBanyan-Round: 1\nBanyan-Of: "+two+"\nBanyan-Check: whole")
	probe := o.Repo.Mark(one, "Banyan-Try: 1\nBanyan-Round: 1\nBanyan-Of: "+two+"\nBanyan-Probe-Of: "+two)
	for _, sha := range []queue.SHA{two, retry, check, probe} {
		if o.Repo.Seen(sha) {
			t.Errorf("%s is seen before it is pushed", sha)
		}
		if err := o.Repo.Push(sha); err != nil {
			t.Fatal(err)
		}
	}
	o.fetch()
	builds, err := o.Repo.Builds(trunk)
	if err != nil {
		t.Fatal(err)
	}
	got := map[queue.SHA]queue.Build{}
	for _, b := range builds {
		got[b.SHA] = b
	}
	chain := []queue.Member{{Number: 1, Head: a.Head, Merge: one}, {Number: 2, Head: b.Head, Merge: two}}
	want := []queue.Build{
		{SHA: two, Kind: queue.Candidate, Base: trunk, Chain: chain},
		{SHA: retry, Kind: queue.Candidate, Base: trunk, Chain: chain, Round: 1, Of: two},
		{SHA: check, Kind: queue.Check, Base: trunk, Round: 1, Of: two},
		{SHA: probe, Kind: queue.Probe, Base: trunk, Chain: chain[:1], Round: 1, Of: two},
	}
	for _, w := range want {
		if g := got[w.SHA]; fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("read back\n %+v\nwant\n %+v", g, w)
		}
		if !o.Repo.Seen(w.SHA) {
			t.Errorf("%s is pushed and not seen", w.SHA)
		}
	}
	if len(builds) != len(want) {
		t.Errorf("%d builds read back, %d pushed", len(builds), len(want))
	}
}

func TestLandingMovesTheTrunkAndTakesTheBranch(t *testing.T) {
	o := newOrigin(t)
	a, b := o.pr(1, "a", "a\n"), o.pr(2, "b", "b\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	two, _ := o.Repo.Merge(one, b, "")
	o.Repo.Push(one)
	o.Repo.Push(two)

	// A removal deletes the branch, and the landing that read the queue before it is refused.
	if err := o.Repo.Delete(two); err != nil {
		t.Fatal(err)
	}
	if err := o.Repo.Land(two); err == nil {
		t.Fatal("a candidate whose branch was deleted landed")
	}
	if now := o.fetch(); now != trunk {
		t.Fatalf("the refused landing moved the trunk to %s", now)
	}

	if err := o.Repo.Land(one); err != nil {
		t.Fatal(err)
	}
	if now := o.fetch(); now != one {
		t.Errorf("the trunk is at %s after landing %s", now, one)
	}
	if builds, _ := o.Repo.Builds(one); len(builds) != 0 {
		t.Errorf("the landed candidate's branch still stands: %+v", builds)
	}
	if err := o.Repo.Delete(one); err != nil {
		t.Errorf("deleting a branch that is gone failed: %v", err)
	}
}

func TestATrunkThatMovedRefusesTheLanding(t *testing.T) {
	o := newOrigin(t)
	a := o.pr(1, "a", "a\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	o.Repo.Push(one)
	o.Outside("other", "x\n")
	if err := o.Repo.Land(one); err == nil {
		t.Error("a candidate landed on a trunk that had moved past its base")
	}
}

func TestABuildKeepsItsBaseOnceTheTrunkHasMovedPastIt(t *testing.T) {
	o := newOrigin(t)
	a, b, c := o.pr(1, "a", "a\n"), o.pr(2, "b", "b\n"), o.pr(3, "c", "c\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	o.Repo.Push(one)
	if err := o.Repo.Land(one); err != nil {
		t.Fatal(err)
	}

	// On a trunk that is itself a landed merge, a red candidate is set aside with its round,
	// and the chain goes on beside it and lands.
	two, _ := o.Repo.Merge(one, b, "")
	check := o.Repo.Mark(two, "Banyan-Try: 1\nBanyan-Round: 1\nBanyan-Of: "+two+"\nBanyan-Check: whole")
	tip := o.Repo.Mark(one, "Banyan-Try: 1\nBanyan-Round: 1\nBanyan-Of: "+two+"\nBanyan-Check: whole")
	three, _ := o.Repo.Merge(one, c, "")
	for _, sha := range []queue.SHA{two, check, tip, three} {
		if err := o.Repo.Push(sha); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Repo.Land(three); err != nil {
		t.Fatal(err)
	}
	if now := o.fetch(); now != three {
		t.Fatalf("the trunk is at %s after landing %s", now, three)
	}

	builds, err := o.Repo.Builds(three)
	if err != nil {
		t.Fatal(err)
	}
	got := map[queue.SHA]queue.Build{}
	for _, b := range builds {
		got[b.SHA] = b
	}
	chain := []queue.Member{{Number: 2, Head: b.Head, Merge: two}}
	for _, w := range []queue.Build{
		{SHA: two, Kind: queue.Candidate, Base: one, Chain: chain},
		{SHA: check, Kind: queue.Check, Base: one, Chain: chain, Round: 1, Of: two},
		{SHA: tip, Kind: queue.Check, Base: one, Round: 1, Of: two},
	} {
		if g := got[w.SHA]; fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("read back\n %+v\nwant\n %+v", g, w)
		}
	}
}

func TestAShallowCloneIsNoConflict(t *testing.T) {
	o := newOrigin(t)
	a, b := o.pr(1, "a", "a\n"), o.pr(2, "b", "b\n")
	trunk := o.fetch()
	one, _ := o.Repo.Merge(trunk, a, "")
	o.Repo.Push(one)
	if err := o.Repo.Land(one); err != nil {
		t.Fatal(err)
	}

	// A CI checkout of the trunk's tip alone has none of the history a merge base is found in.
	shallow := *o.Repo
	shallow.Dir, shallow.commits, shallow.made = t.TempDir(), nil, nil
	gittest.Sh(t, shallow.Dir, "git", "clone", "--quiet", "--depth=1", "--branch=main", "file://"+o.Bare, ".")
	tip, _, err := shallow.Fetch([]int{2})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shallow.Merge(tip, b, ""); !ok {
		t.Error("a pull request that merges cleanly was called a conflict in a shallow clone")
	}
}

func TestAMergeThatGitCannotAttemptIsNoConflict(t *testing.T) {
	o := newOrigin(t)
	trunk := o.fetch()
	defer func() {
		if recover() == nil {
			t.Error("a merge of a commit the repository does not hold was called a conflict")
		}
	}()
	o.Repo.Merge(trunk, queue.PR{Number: 9, Head: "0123456789012345678901234567890123456789"}, "")
}

func TestAMergeEndsWithWhatItMergesAndMarksCarryNone(t *testing.T) {
	o := newOrigin(t)
	a := o.pr(12, "a", "a\n")
	a.Branch = "o/pr12"
	trunk := o.fetch()

	plain, _ := o.Repo.Merge(trunk, a, "")
	if got, want := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", plain), "Merge pull request #12 from o/pr12\n\nMerges #12"; got != want {
		t.Errorf("plain merge message:\n%s\nwant:\n%s", got, want)
	}

	retry, _ := o.Repo.Merge(trunk, a, "Banyan-Try: 2\nBanyan-Round: 1\nBanyan-Of: "+plain)
	wantRetry := "Merge pull request #12 from o/pr12\n\nBanyan-Try: 2\nBanyan-Round: 1\nBanyan-Of: " + plain + "\nMerges #12"
	if got := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", retry); got != wantRetry {
		t.Errorf("retry merge message:\n%s\nwant:\n%s", got, wantRetry)
	}

	mark := o.Repo.Mark(plain, "Banyan-Try: 1\nBanyan-Round: 1\nBanyan-Of: "+plain+"\nBanyan-Check: whole")
	if got := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", mark); strings.Contains(got, "Merges #") || strings.Contains(got, "Banyan-Merges") {
		t.Errorf("mark message carries a merge line:\n%s", got)
	}

	c, err := o.Repo.read(plain)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := o.Repo.commitTree(c.tree, []string{trunk, a.Head}, "Merge pull request #12 from o/pr12\n\nBanyan-Merges: 12")
	if err != nil {
		t.Fatal(err)
	}
	legacyTry, err := o.Repo.commitTree(c.tree, []string{trunk, a.Head}, "Merge pull request #12 from o/pr12\n\nBanyan-Merges: 12\nBanyan-Try: 2")
	if err != nil {
		t.Fatal(err)
	}

	for _, sha := range []queue.SHA{plain, retry, mark, legacy, legacyTry} {
		if err := o.Repo.Push(sha); err != nil {
			t.Fatal(err)
		}
	}
	o.fetch()
	builds, err := o.Repo.Builds(trunk)
	if err != nil {
		t.Fatal(err)
	}
	got := map[queue.SHA]queue.Build{}
	for _, b := range builds {
		got[b.SHA] = b
	}
	chain := []queue.Member{{Number: 12, Head: a.Head, Merge: plain}}
	for _, w := range []queue.Build{
		{SHA: plain, Kind: queue.Candidate, Base: trunk, Chain: chain},
		{SHA: retry, Kind: queue.Candidate, Base: trunk, Chain: chain, Round: 1, Of: plain},
		{SHA: mark, Kind: queue.Check, Base: trunk, Chain: chain, Round: 1, Of: plain},
	} {
		if g := got[w.SHA]; fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("read back\n %+v\nwant\n %+v", g, w)
		}
	}
	older := []queue.Member{{Number: 12, Head: a.Head, Merge: legacy}}
	for _, old := range []queue.SHA{legacy, legacyTry} {
		if g, w := got[old], (queue.Build{SHA: old, Kind: queue.Candidate, Base: trunk, Chain: older}); fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("a merge an older release wrote reads back as\n %+v\nwant\n %+v", g, w)
		}
	}
}

func TestAMergeIsReadBackByItsLastParagraphWhateverItsDescriptionQuotes(t *testing.T) {
	o := newOrigin(t)
	a := o.pr(12, "a", "a\n")
	a.Message = "A title\n\nIt quotes the queue:\n\nBanyan-Probe-Of: abc\nBanyan-Try: 9\nMerges #99"
	trunk := o.fetch()

	plain, _ := o.Repo.Merge(trunk, a, "")
	if got, want := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", plain), a.Message+"\n\nMerges #12"; got != want {
		t.Errorf("plain merge message:\n%s\nwant:\n%s", got, want)
	}
	retry, _ := o.Repo.Merge(trunk, a, "Banyan-Try: 2")
	for _, sha := range []queue.SHA{plain, retry} {
		if err := o.Repo.Push(sha); err != nil {
			t.Fatal(err)
		}
	}
	o.fetch()
	builds, err := o.Repo.Builds(trunk)
	if err != nil {
		t.Fatal(err)
	}
	chain := []queue.Member{{Number: 12, Head: a.Head, Merge: plain}}
	for _, b := range builds {
		if w := (queue.Build{SHA: b.SHA, Kind: queue.Candidate, Base: trunk, Chain: chain}); fmt.Sprint(b) != fmt.Sprint(w) {
			t.Errorf("read back\n %+v\nwant\n %+v", b, w)
		}
	}
	if len(builds) != 2 {
		t.Errorf("read back %d builds, want the merge and its second try", len(builds))
	}
}

func TestAMergeTheQueueHoldsKeepsItsWordsWhenThePullRequestIsReworded(t *testing.T) {
	o := newOrigin(t)
	a := o.pr(1, "a", "a\n")
	a.Message = "As queued"
	trunk := o.fetch()
	first, _ := o.Repo.Merge(trunk, a, "")
	if err := o.Repo.Push(first); err != nil {
		t.Fatal(err)
	}

	later := func() {
		o.Repo = &Repo{Dir: o.Clone, Remote: "origin", Trunk: "main", Lane: "merge-queue", Name: "Banyan", Email: "banyan@example.com"}
		o.fetch()
		if _, err := o.Repo.Builds(trunk); err != nil {
			t.Fatal(err)
		}
	}
	later()
	a.Message = "As reworded since"
	if again, _ := o.Repo.Merge(trunk, a, ""); again != first {
		t.Errorf("merged again as %s, which says %q, while the queue holds %s", again, gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", again), first)
	}
	try, _ := o.Repo.Merge(trunk, a, "Banyan-Try: 2")
	if got, want := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", try), "As queued\n\nBanyan-Try: 2\nMerges #1"; got != want {
		t.Errorf("its second try says:\n%s\nwant:\n%s", got, want)
	}
	if err := o.Repo.Push(try); err != nil {
		t.Fatal(err)
	}
	o.fetch()
	builds, err := o.Repo.Builds(trunk)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		if b.Tests() != first {
			t.Errorf("%s is read back as a build of %s, and both are builds of %s", b.SHA, b.Tests(), first)
		}
	}

	// The same queue, reading again once it holds no merge of the head, says the new words.
	for _, sha := range []queue.SHA{first, try} {
		if err := o.Repo.Delete(sha); err != nil {
			t.Fatal(err)
		}
	}
	o.fetch()
	if _, err := o.Repo.Builds(trunk); err != nil {
		t.Fatal(err)
	}
	fresh, _ := o.Repo.Merge(trunk, a, "")
	if got, want := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", fresh), "As reworded since\n\nMerges #1"; got != want {
		t.Errorf("with no merge of it held, it says:\n%s\nwant:\n%s", got, want)
	}
}

func TestADescriptionLongerThanAnArgumentMayBeIsSaidWhole(t *testing.T) {
	o := newOrigin(t)
	a := o.pr(1, "a", "a\n")
	a.Message = "A title\n\n" + strings.TrimSpace(strings.Repeat("words and more words\n", 12000))
	trunk := o.fetch()
	merge, ok := o.Repo.Merge(trunk, a, "")
	if got := gittest.Sh(t, o.Clone, "git", "show", "--no-patch", "--format=%B", merge); !ok || got != a.Message+"\n\nMerges #1" {
		t.Errorf("merged %v, saying %d bytes of the %d queued", ok, len(got), len(a.Message))
	}
}
