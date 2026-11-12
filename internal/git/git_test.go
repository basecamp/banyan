package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basecamp/banyan/internal/queue"
)

// origin is a bare repository with a trunk and pull requests, each a branch off the trunk
// that writes one file, and a clone of it for the queue to work in.
type origin struct {
	t     *testing.T
	bare  string
	work  string
	Repo  *Repo
	heads map[int]queue.SHA
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newOrigin(t *testing.T) *origin {
	dir := t.TempDir()
	o := &origin{t: t, bare: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "author"), heads: map[int]queue.SHA{}}
	sh(t, dir, "git", "init", "--quiet", "--bare", "--initial-branch=main", o.bare)
	sh(t, dir, "git", "clone", "--quiet", o.bare, o.work)
	os.WriteFile(filepath.Join(o.work, "README"), []byte("trunk\n"), 0o644)
	sh(t, o.work, "git", "add", ".")
	sh(t, o.work, "git", "commit", "--quiet", "-m", "trunk")
	sh(t, o.work, "git", "push", "--quiet", "origin", "HEAD:main")
	clone := filepath.Join(dir, "queue")
	sh(t, dir, "git", "clone", "--quiet", o.bare, clone)
	o.Repo = &Repo{Dir: clone, Remote: "origin", Trunk: "main", Lane: "merge-queue", Name: "Banyan", Email: "banyan@example.com"}
	return o
}

// pr opens pull request n, which writes content to file, and returns it queued.
func (o *origin) pr(n int, file, content string) queue.PR {
	sh(o.t, o.work, "git", "checkout", "--quiet", "-B", fmt.Sprintf("pr%d", n), "origin/main")
	os.WriteFile(filepath.Join(o.work, file), []byte(content), 0o644)
	sh(o.t, o.work, "git", "add", ".")
	sh(o.t, o.work, "git", "commit", "--quiet", "-m", fmt.Sprintf("pr %d", n))
	head := sh(o.t, o.work, "git", "rev-parse", "HEAD")
	sh(o.t, o.work, "git", "push", "--quiet", "--force", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", n))
	o.heads[n] = head
	return queue.PR{Number: n, Head: head, Pinned: head, Checks: queue.Success}
}

func (o *origin) fetch() queue.SHA {
	var ns []int
	for n := range o.heads {
		ns = append(ns, n)
	}
	trunk, heads, err := o.Repo.Fetch(ns)
	if err != nil {
		o.t.Fatal(err)
	}
	for n, h := range heads {
		if h != o.heads[n] {
			o.t.Fatalf("#%d fetched at %s, pushed at %s", n, h, o.heads[n])
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
	sh(t, o.work, "git", "checkout", "--quiet", "-B", "main", "origin/main")
	os.WriteFile(filepath.Join(o.work, "other"), []byte("x\n"), 0o644)
	sh(t, o.work, "git", "add", ".")
	sh(t, o.work, "git", "commit", "--quiet", "-m", "outside")
	sh(t, o.work, "git", "push", "--quiet", "origin", "HEAD:main")
	if err := o.Repo.Land(one); err == nil {
		t.Error("a candidate landed on a trunk that had moved past its base")
	}
}
