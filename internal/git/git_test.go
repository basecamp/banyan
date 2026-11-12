package git

import (
	"fmt"
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
	return queue.PR{Number: n, Head: head, Pinned: head, Checks: queue.Success}
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
