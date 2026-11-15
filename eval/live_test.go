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

func TestKeepLiveOmitsBuildsFirstSeenMarked(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	recs := []queue.Recording{{World: queue.World{Now: now, Trunk: "t", Builds: []queue.Build{
		{SHA: "built", Pushed: now.Add(-10 * time.Minute), Verdict: queue.Success},
		{SHA: "landed1", Landed: now.Add(-time.Minute), Pushed: now.Add(-time.Minute), Verdict: queue.Success},
		{SHA: "landed2", Landed: now.Add(-time.Minute), Pushed: now.Add(-2 * time.Minute), Verdict: queue.Failure},
		{SHA: "landed3", Landed: now, Pushed: now.Add(-3 * time.Minute), Verdict: queue.Success},
	}}}}
	sc, win, _ := captureLive(t, recs, "")
	if win.build != 10*time.Minute {
		t.Fatalf("build duration = %s, want 10m", win.build)
	}
	if sc.Good != 1 || sc.Flaked != 0 {
		t.Fatalf("pre-window builds entered tallies: good=%d flaked=%d", sc.Good, sc.Flaked)
	}
}

func TestKeepLiveCountsTheFirstVerdictUnderALandingOnce(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	b := queue.Build{SHA: "lower", Pushed: now}
	finished := b
	finished.Verdict, finished.Landed = queue.Success, now.Add(20*time.Minute)
	finished.Pushed = finished.Landed
	later := finished
	later.Pushed = finished.Landed
	recs := []queue.Recording{
		{World: queue.World{Now: now, Trunk: "t", Builds: []queue.Build{b}}},
		{World: queue.World{Now: now.Add(20 * time.Minute), Trunk: "upper", Builds: []queue.Build{finished}}},
		{World: queue.World{Now: now.Add(21 * time.Minute), Trunk: "upper", Builds: []queue.Build{later}}},
	}
	sc, win, _ := captureLive(t, recs, "")
	if win.build != 20*time.Minute || sc.Good != 1 {
		t.Fatalf("first verdict duration=%s good=%d, want 20m and one", win.build, sc.Good)
	}
}

func TestKeepLiveIgnoresVerdictsAfterAnUnrecordedMark(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	b := queue.Build{SHA: "b", Pushed: now}
	recs := []queue.Recording{{World: queue.World{Now: now, Trunk: "t", Builds: []queue.Build{
		{SHA: "reference", Pushed: now.Add(-5 * time.Minute), Verdict: queue.Success},
	}}, Actions: []queue.Action{{Do: "push", Build: b}}}}
	b.Landed = now.Add(20 * time.Minute)
	b.Pushed = b.Landed
	for minute := 21; minute <= 28; minute++ {
		if minute == 28 {
			b.Verdict = queue.Failure
		}
		recs = append(recs, queue.Recording{World: queue.World{Now: now.Add(time.Duration(minute) * time.Minute), Trunk: "b", Builds: []queue.Build{b}}})
	}
	sc, win, _ := captureLive(t, recs, "")
	if win.build != 5*time.Minute || sc.Good != 1 || sc.Flaked != 0 {
		t.Fatalf("post-mark verdict counted: duration=%s good=%d flaked=%d", win.build, sc.Good, sc.Flaked)
	}
	recs = append(recs, queue.Recording{World: queue.World{Now: now.Add(29 * time.Minute), Trunk: "newer", Builds: []queue.Build{b}}})
	sc, win, _ = captureLive(t, recs, "")
	if win.build != 5*time.Minute || sc.Good != 1 || sc.Flaked != 0 {
		t.Fatalf("former tip verdict counted: duration=%s good=%d flaked=%d", win.build, sc.Good, sc.Flaked)
	}
}

func TestKeepLiveCountsAnUnrecordedLowerMark(t *testing.T) {
	for _, source := range []string{"world", "push"} {
		t.Run(source, func(t *testing.T) {
			now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
			members := []queue.Member{{Number: 1, Head: "ha", Merge: "a"}, {Number: 2, Head: "hb", Merge: "b"}}
			a := queue.Build{SHA: "a", Chain: members[:1], Pushed: now}
			b := queue.Build{SHA: "b", Chain: members, Pushed: now, Verdict: queue.Success}
			c := queue.Build{SHA: "c", Chain: []queue.Member{{Number: 3, Head: "hc", Merge: "c"}}, Pushed: at(6), Verdict: queue.Success}
			first := queue.Recording{World: queue.World{Now: now, Trunk: "t"}, Actions: []queue.Action{{Do: "push", Build: b}}}
			if source == "world" {
				first.World.Builds = []queue.Build{a}
			} else {
				first.Actions = append(first.Actions, queue.Action{Do: "push", Build: a})
			}
			recs := []queue.Recording{
				first,
				{World: queue.World{Now: at(5), Trunk: "t", Builds: []queue.Build{a, b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: members}}},
				{World: queue.World{Now: at(6), Trunk: "b", Builds: []queue.Build{a}}, Actions: []queue.Action{{Do: "push", Build: c}}},
				{World: queue.World{Now: at(16), Trunk: "b", Builds: []queue.Build{a, c}}, Actions: []queue.Action{{Do: "land", SHA: "c", Members: c.Chain}}},
			}
			a.Verdict, a.Landed, a.Pushed = queue.Failure, at(17), at(17)
			recs = append(recs,
				queue.Recording{World: queue.World{Now: at(18), Trunk: "c", Builds: []queue.Build{a}}},
				queue.Recording{World: queue.World{Now: at(19), Trunk: "c", Builds: []queue.Build{a}}},
			)
			if source == "push" {
				for i := range recs {
					recs[i].World.Builds = slices.DeleteFunc(recs[i].World.Builds, func(b queue.Build) bool {
						return b.SHA == a.SHA && b.Landed.IsZero()
					})
				}
			}
			sc, _, _ := captureLive(t, recs, "")
			if sc.Good != 3 || sc.Flaked != 1 {
				t.Errorf("lower mark: good=%d flaked=%d, want 3 and 1", sc.Good, sc.Flaked)
			}
			durationRecs := make([]queue.Recording, len(recs))
			for i, rec := range recs {
				durationRecs[i].World = queue.World{Now: rec.World.Now, Trunk: rec.World.Trunk}
				for _, build := range rec.World.Builds {
					if build.SHA == a.SHA {
						durationRecs[i].World.Builds = append(durationRecs[i].World.Builds, build)
					}
				}
				for _, action := range rec.Actions {
					if action.Do == "push" && action.Build.SHA == a.SHA {
						durationRecs[i].Actions = append(durationRecs[i].Actions, action)
					}
				}
			}
			durationRecs[0].World.Builds = append(durationRecs[0].World.Builds, queue.Build{SHA: "reference", Kind: queue.Check, Of: "a", Pushed: at(-1), Verdict: queue.Success})
			_, win, _ := captureLive(t, durationRecs, "")
			if win.build != 17*time.Minute {
				t.Errorf("lower mark duration=%s, want 17m from first push to mark", win.build)
			}
			trunk := queue.SHA("c")
			for n := 4; n <= 50; n++ {
				build := queue.Build{SHA: fmt.Sprintf("m%d", n), Chain: []queue.Member{{Number: n, Head: fmt.Sprintf("h%d", n), Merge: fmt.Sprintf("m%d", n)}}, Pushed: at(n * 20)}
				recs = append(recs, queue.Recording{World: queue.World{Now: build.Pushed, Trunk: trunk}, Actions: []queue.Action{{Do: "push", Build: build}}})
				build.Verdict = queue.Success
				recs = append(recs, queue.Recording{World: queue.World{Now: build.Pushed.Add(10 * time.Minute), Trunk: trunk, Builds: []queue.Build{build}}, Actions: []queue.Action{{Do: "land", SHA: build.SHA, Members: build.Chain}}})
				trunk = build.SHA
			}
			recs = append(recs, queue.Recording{World: queue.World{Now: at(1011), Trunk: trunk}})
			sc, _, _ = captureLive(t, recs, "")
			if sc.Landed != 50 || sc.Good != 50 || sc.Flaked != 1 {
				t.Errorf("fifty landings: landed=%d good=%d flaked=%d, want 50, 50, 1", sc.Landed, sc.Good, sc.Flaked)
			}
		})
	}
}

func TestKeepLiveCountsTheTopBeforeLandingOnce(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	b := queue.Build{SHA: "top", Pushed: now, Verdict: queue.Success}
	recs := []queue.Recording{
		{World: queue.World{Now: now, Trunk: "t"}, Actions: []queue.Action{{Do: "push", Build: b}}},
		{World: queue.World{Now: now.Add(10 * time.Minute), Trunk: "t", Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "top"}}},
	}
	b.Landed, b.Pushed, b.Verdict = now.Add(11*time.Minute), now.Add(11*time.Minute), queue.Failure
	recs = append(recs,
		queue.Recording{World: queue.World{Now: b.Landed, Trunk: "top", Builds: []queue.Build{b}}},
		queue.Recording{World: queue.World{Now: now.Add(12 * time.Minute), Trunk: "newer", Builds: []queue.Build{b}}},
	)
	sc, win, _ := captureLive(t, recs, "")
	if sc.Good != 1 || sc.Flaked != 0 || win.build != 10*time.Minute {
		t.Fatalf("top counted again: good=%d flaked=%d duration=%s, want 1, 0, 10m", sc.Good, sc.Flaked, win.build)
	}
}
