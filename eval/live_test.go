package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// Two pull requests are queued at noon. #1's build fails at 12:10, a build of the same tree
// passes at 12:30, and #1 is ejected all the same, then queued again as it was and landed at
// 13:00 with #2: an ejection later overturned, on a failure that was CI's own.
func TestLiveScoresRecordedTicksByTheEvalsDefinitions(t *testing.T) {
	noon := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return noon.Add(time.Duration(m) * time.Minute) }
	one, two := queue.PR{Number: 1, Head: "h1", Queued: noon, Checks: queue.Success}, queue.PR{Number: 2, Head: "h2", Queued: noon, Checks: queue.Success}
	chain := []queue.Member{{Number: 1, Head: "h1", Merge: "m1"}, {Number: 2, Head: "h2", Merge: "m2"}}
	red := queue.Build{SHA: "m1", Chain: chain[:1], Pushed: noon}
	green := queue.Build{SHA: "m2b", Chain: chain, Pushed: at(50)}
	failed, passed := red, green
	failed.Verdict, passed.Verdict = queue.Failure, queue.Success
	again := queue.Build{SHA: "c1", Kind: queue.Check, Chain: chain[:1], Round: 1, Of: "m1", Pushed: at(20), Verdict: queue.Success}
	recs := []queue.Recording{
		{World: queue.World{Now: noon, Trunk: "t0", PRs: []queue.PR{one, two}}, Actions: []queue.Action{{Do: "push", Build: red}}},
		{World: queue.World{Now: at(10), Trunk: "t0", PRs: []queue.PR{one, two}, Builds: []queue.Build{failed}}},
		{World: queue.World{Now: at(30), Trunk: "t0", PRs: []queue.PR{one, two}, Builds: []queue.Build{failed, again}},
			Actions: []queue.Action{{Do: "eject", PR: 1, Cause: "culprit"}}},
		{World: queue.World{Now: at(50), Trunk: "t0", PRs: []queue.PR{one, two}}, Actions: []queue.Action{{Do: "push", Build: green}}},
		{World: queue.World{Now: at(60), Trunk: "t0", PRs: []queue.PR{one, two}, Builds: []queue.Build{passed}},
			Actions: []queue.Action{{Do: "land", SHA: "m2b", Members: chain}, {Do: "land", SHA: "never", Members: chain}}},
		// The trunk refused the push once, so the same tick planned the landing again.
		{World: queue.World{Now: at(60), Trunk: "t0", PRs: []queue.PR{one, two}, Builds: []queue.Build{passed}},
			Actions: []queue.Action{{Do: "land", SHA: "m2b", Members: chain}}},
		{World: queue.World{Now: at(61), Trunk: "m2b"}, Config: dials(2, 2, false)},
	}
	dir := t.TempDir()
	for i, rec := range recs {
		data, _ := json.Marshal(rec)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.json", i)), data, 0o644)
	}
	sc, win, err := live(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	// A whole build took 10 minutes, so an hour to land is 6 builds and half an hour to an ejection is 3.
	want := Score{Shape: filepath.Base(dir), Config: "live", Landed: 2, Wrong: 500, WaitP50: 6, WaitP90: 6, LabelP50: 6, LabelP90: 6, LabelSlow: 6, EjectP90: 3, Builds: 1, PerHour: 2 / (61.0 / 60),
		Good: 3, Flaked: 1}
	if fmt.Sprintf("%.3v", sc) != fmt.Sprintf("%.3v", want) {
		t.Errorf("scored\n %+v\nwant\n %+v", sc, want)
	}

	// The same hour played again on a CI that never fails: both were ready at noon, so one
	// build of the two lands them.
	if len(win.arrivals) != 2 || win.arrivals[1] != 0 || win.build != 10*time.Minute {
		t.Errorf("the window is %+v, where two were ready at noon and a build took 10 minutes", win)
	}
	if again := win.simulated(Shape{Name: "quiet"}, Score{}); again.Landed != 2 || again.Builds != 0.5 || again.WaitP90 > 1.5 {
		t.Errorf("simulated %+v, where both land on one build", again)
	}
}

func captureLive(t *testing.T, recs []queue.Recording, clone string) (Score, window, string) {
	t.Helper()
	dir := t.TempDir()
	for i, rec := range recs {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.json", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	sc, win, lerr := live(dir, clone)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	r.Close()
	if lerr != nil {
		t.Fatal(lerr)
	}
	return sc, win, string(out)
}

func TestLiveLabelledToLandedSpansNewHeadsAndRelabelling(t *testing.T) {
	noon := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return noon.Add(time.Duration(m) * time.Minute) }

	var prs []queue.PR
	var chain18 []queue.Member
	for n := 1; n <= 18; n++ {
		h := fmt.Sprintf("h%d", n)
		prs = append(prs, queue.PR{Number: n, Head: h, Queued: noon, Checks: queue.Success})
		chain18 = append(chain18, queue.Member{Number: n, Head: h, Merge: fmt.Sprintf("m%d", n)})
	}
	pr19a := queue.PR{Number: 19, Head: "h19a", Queued: noon, Checks: queue.Pending}
	pr19bPending := queue.PR{Number: 19, Head: "h19b", Queued: noon, Checks: queue.Pending}
	pr19bReady := queue.PR{Number: 19, Head: "h19b", Queued: noon, Checks: queue.Success}
	pr20a := queue.PR{Number: 20, Head: "h20a", Queued: noon, Checks: queue.Success}
	pr20b := queue.PR{Number: 20, Head: "h20b", Queued: at(50), Checks: queue.Success}

	b18 := queue.Build{SHA: "m18", Chain: chain18, Pushed: noon}
	passed18 := b18
	passed18.Verdict = queue.Success

	recs := []queue.Recording{
		{World: queue.World{Now: noon, Trunk: "t0", PRs: append(slices.Clone(prs), pr19a, pr20a)}, Actions: []queue.Action{{Do: "push", Build: b18}}},
		{World: queue.World{Now: at(10), Trunk: "t0", PRs: append(slices.Clone(prs), pr19a, pr20a), Builds: []queue.Build{passed18}},
			Actions: []queue.Action{{Do: "land", SHA: "m18", Members: chain18}}},
		{World: queue.World{Now: at(35), Trunk: "m18", PRs: []queue.PR{pr19bPending}}},
		{World: queue.World{Now: at(40), Trunk: "m18", PRs: []queue.PR{pr19bReady}}},
		{World: queue.World{Now: at(50), Trunk: "m18", PRs: []queue.PR{pr19bReady, pr20b}},
			Actions: []queue.Action{{Do: "land", SHA: "m19", Members: []queue.Member{{Number: 19, Head: "h19b", Merge: "m19"}}}}},
		{World: queue.World{Now: at(60), Trunk: "m19", PRs: []queue.PR{pr20b}},
			Actions: []queue.Action{{Do: "land", SHA: "m20", Members: []queue.Member{{Number: 20, Head: "h20b", Merge: "m20"}}}}},
		{World: queue.World{Now: at(61), Trunk: "m20"}, Config: dials(2, 2, false)},
	}

	sc, _, out := captureLive(t, recs, "")
	if sc.WaitP50 != 1 || sc.WaitP90 != 1 {
		t.Errorf("ready to landed: wait50=%v wait90=%v, want 1 and 1", sc.WaitP50, sc.WaitP90)
	}
	if sc.LabelP50 != 1 || sc.LabelP90 != 5 || sc.LabelSlow != 6 {
		t.Errorf("labelled to landed: label50=%v label90=%v slow20th=%v, want 1, 5, 6", sc.LabelP50, sc.LabelP90, sc.LabelSlow)
	}
	wantLine := "labelled to landed: 10 min at the median, 50 at p90, 60 at the mean of the slowest twentieth"
	if !strings.Contains(out, wantLine) {
		t.Errorf("output missing %q\ngot:\n%s", wantLine, out)
	}
}

func TestLiveWrongEjectionsAcrossNewHeads(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-b", "main")
	commitFile := func(name, body, msg string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-m", msg)
		return git("rev-parse", "HEAD")
	}

	t0 := commitFile("base.txt", "base\n", "t0")
	git("checkout", "-b", "pr1", t0)
	h1a := commitFile("pr1.txt", "one\n", "pr1a")
	git("checkout", "-b", "pr2", t0)
	h2a := commitFile("pr2.txt", "two\n", "pr2a")
	git("checkout", "-b", "pr3", t0)
	h3a := commitFile("pr3.txt", "before\n", "pr3a")

	git("checkout", "main")
	t1 := commitFile("trunk.txt", "trunk\n", "t1")

	git("checkout", "-b", "pr1b", t1)
	git("cherry-pick", h1a)
	h1b := git("rev-parse", "HEAD")

	git("checkout", "-b", "pr2b", h2a)
	git("merge", "--no-edit", t1)
	h2b := git("rev-parse", "HEAD")

	git("checkout", "-b", "pr3b", t1)
	h3b := commitFile("pr3.txt", "after\n", "pr3b")

	git("checkout", "-b", "pr4b", t1)
	h4b := commitFile("pr4.txt", "four\n", "pr4b")

	git("checkout", "main")
	git("merge", "--no-ff", "--no-edit", h1b, h2b, h3b, h4b)
	h4a := "0000000000000000000000000000000000000004"

	chmodTree := func(dirWritable bool) {
		var dirs []string
		filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				dirs = append(dirs, p)
				if dirWritable {
					os.Chmod(p, 0o755)
				}
			} else {
				if dirWritable {
					os.Chmod(p, 0o644)
				} else {
					os.Chmod(p, 0o444)
				}
			}
			return nil
		})
		if !dirWritable {
			for i := len(dirs) - 1; i >= 0; i-- {
				os.Chmod(dirs[i], 0o555)
			}
		}
	}
	t.Cleanup(func() { chmodTree(true) })
	chmodTree(false)

	snapshot := func() []string {
		var entries []string
		filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			info, err := d.Info()
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(repo, p)
			entries = append(entries, fmt.Sprintf("%s|%v|%d|%d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano()))
			return nil
		})
		slices.Sort(entries)
		return entries
	}
	before := snapshot()

	noon := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return noon.Add(time.Duration(m) * time.Minute) }
	ejected := []queue.PR{
		{Number: 1, Head: queue.SHA(h1a), Queued: noon, Checks: queue.Success},
		{Number: 2, Head: queue.SHA(h2a), Queued: noon, Checks: queue.Success},
		{Number: 3, Head: queue.SHA(h3a), Queued: noon, Checks: queue.Success},
		{Number: 4, Head: queue.SHA(h4a), Queued: noon, Checks: queue.Success},
	}
	landing := []queue.PR{
		{Number: 1, Head: queue.SHA(h1b), Queued: noon, Checks: queue.Success},
		{Number: 2, Head: queue.SHA(h2b), Queued: noon, Checks: queue.Success},
		{Number: 3, Head: queue.SHA(h3b), Queued: noon, Checks: queue.Success},
		{Number: 4, Head: queue.SHA(h4b), Queued: noon, Checks: queue.Success},
	}
	chain := []queue.Member{
		{Number: 1, Head: queue.SHA(h1b), Merge: "m1"},
		{Number: 2, Head: queue.SHA(h2b), Merge: "m2"},
		{Number: 3, Head: queue.SHA(h3b), Merge: "m3"},
		{Number: 4, Head: queue.SHA(h4b), Merge: "m4"},
	}
	b := queue.Build{SHA: "m1234", Chain: chain, Pushed: noon}
	passed := b
	passed.Verdict = queue.Success

	recs := []queue.Recording{
		{
			World: queue.World{Now: noon, Trunk: queue.SHA(t0), PRs: ejected},
			Actions: []queue.Action{
				{Do: "push", Build: b},
				{Do: "eject", PR: 1, Cause: "culprit"},
				{Do: "eject", PR: 2, Cause: "culprit"},
				{Do: "eject", PR: 3, Cause: "culprit"},
				{Do: "eject", PR: 4, Cause: "culprit"},
			},
		},
		{
			World:   queue.World{Now: at(10), Trunk: queue.SHA(t1), PRs: landing, Builds: []queue.Build{passed}},
			Actions: []queue.Action{{Do: "land", SHA: "m1234", Members: chain}},
		},
		{World: queue.World{Now: at(11), Trunk: "m1234"}, Config: dials(2, 4, false)},
	}

	scNoClone, _, outNoClone := captureLive(t, recs, "")
	if scNoClone.Wrong != 0 || scNoClone.Unchecked != 4 || scNoClone.Unreadable != 0 {
		t.Errorf("without clone: wrong=%v unchecked=%d unreadable=%d, want 0, 4, 0", scNoClone.Wrong, scNoClone.Unchecked, scNoClone.Unreadable)
	}
	if !strings.Contains(outNoClone, "4 at a newer head unchecked") {
		t.Errorf("without clone output missing unchecked note:\n%s", outNoClone)
	}

	scClone, _, outClone := captureLive(t, recs, repo)
	if scClone.Wrong != 500 || scClone.Unchecked != 0 || scClone.Unreadable != 1 {
		t.Errorf("with clone: wrong=%v unchecked=%d unreadable=%d, want 500, 0, 1", scClone.Wrong, scClone.Unchecked, scClone.Unreadable)
	}
	if !strings.Contains(outClone, "1 at a newer head unreadable") {
		t.Errorf("with clone output missing unreadable note:\n%s", outClone)
	}
	if after := snapshot(); !slices.Equal(before, after) {
		t.Errorf("reading the clone modified it")
	}
}
