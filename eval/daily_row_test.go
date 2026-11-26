package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dailyRow runs eval/daily-row for one day with a fetch, an eval and a post that are stand-ins,
// and returns its exit status and how often the post was called.
func dailyRow(t *testing.T, dir string, env ...string) (status, posts int) {
	t.Helper()
	return dailyRowOf(t, dir, "o/app", env...)
}

func dailyRowOf(t *testing.T, dir, repo string, env ...string) (status, posts int) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(dir, "daily-row"), repo, "2026-11-18")
	cmd.Env = append(append(os.Environ(), "T="+dir, "BANYAN_EVAL="+filepath.Join(dir, "eval"), "BANYAN_ROWS="+filepath.Join(dir, "state")), env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		status = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("daily-row: %v\n%s", err, out)
	}
	posted, _ := os.ReadFile(filepath.Join(dir, "posts"))
	return status, strings.Count(string(posted), "\n")
}

func standIns(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	for _, name := range []string{"daily-row", "daily-row.jq"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"fetch-ticks": "#!/bin/sh\nexit \"${FETCH_EXIT:-0}\"\n",
		"eval": "#!/bin/sh\necho \"$*\" >> \"$T/asked\"\ncase \" $* \" in *\" -takeouts \"*) echo '{\"takeouts\":[],\"by_class\":{},\"comments_read\":0}' ;;\n" +
			"*) echo \"{\\\"commit\\\":\\\"$COMMIT\\\",\\\"landed\\\":1,\\\"ticks\\\":2,\\\"red_candidates\\\":0,\\\"red_read_n\\\":0$LIVE}\" ;; esac\n",
		"post": "#!/bin/sh\ncat > /dev/null; echo x >> \"$T/posts\"; [ \"${POST_EXIT:-0}\" = 0 ] || exit \"$POST_EXIT\"; echo posted\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(parts ...string) bool {
	_, err := os.Stat(filepath.Join(parts...))
	return err == nil
}

func TestDailyRowPostsADayOnceAndFailsWhenItCannot(t *testing.T) {
	dir := standIns(t)
	commit, post := "COMMIT="+strings.Repeat("a", 40), "BANYAN_ROW_POST="+filepath.Join(dir, "post")
	row, receipt := []string{dir, "state", "app", "rows", "2026-11-18.json"}, []string{dir, "state", "app", "rows", "2026-11-18.posted"}

	if status, posts := dailyRow(t, dir, commit, post, "FETCH_EXIT=3"); status != 3 || posts != 0 || exists(row...) {
		t.Fatalf("a fetch with a run still going: status %d, posts %d, row %v", status, posts, exists(row...))
	}
	if status, posts := dailyRow(t, dir, commit, post, "POST_EXIT=7"); status != 1 || posts != 1 || !exists(row...) || exists(receipt...) {
		t.Fatalf("a post that fails: status %d, posts %d, row %v, receipt %v", status, posts, exists(row...), exists(receipt...))
	}
	made, _ := os.ReadFile(filepath.Join(row...))
	if status, posts := dailyRow(t, dir, commit, post); status != 0 || posts != 2 || !exists(receipt...) {
		t.Fatalf("the next run: status %d, posts %d, receipt %v", status, posts, exists(receipt...))
	}
	if status, posts := dailyRow(t, dir, commit, post); status != 0 || posts != 2 {
		t.Fatalf("a run after the post: status %d, posts %d", status, posts)
	}
	if again, _ := os.ReadFile(filepath.Join(row...)); string(again) != string(made) {
		t.Fatal("a later run replaced the day's row")
	}
}

func TestDailyRowNeverPostsFromAnEvalThatIsNotACommit(t *testing.T) {
	dir := standIns(t)
	dirty, post := "COMMIT="+strings.Repeat("a", 40)+"+", "BANYAN_ROW_POST="+filepath.Join(dir, "post")
	if status, posts := dailyRow(t, dir, dirty, post); status != 1 || posts != 0 || exists(dir, "state", "app", "rows", "2026-11-18.json") {
		t.Fatalf("asked to post: status %d, posts %d", status, posts)
	}
	if status, posts := dailyRow(t, dir, dirty); status != 0 || posts != 0 {
		t.Fatalf("made without posting: status %d, posts %d", status, posts)
	}
	if status, posts := dailyRow(t, dir, dirty, post); status != 1 || posts != 0 {
		t.Fatalf("the row already made, asked to post: status %d, posts %d", status, posts)
	}
}

func TestDailyRowSetsAsideResultsWithAnotherHeader(t *testing.T) {
	dir := standIns(t)
	app := filepath.Join(dir, "state", "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "rows.tsv"), []byte("day\tcommit\tlanded\n2026-11-17\tabc\t5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, _ := dailyRow(t, dir, "COMMIT="+strings.Repeat("a", 40)); status != 0 {
		t.Fatalf("status %d", status)
	}
	kept, _ := filepath.Glob(filepath.Join(app, "rows.*.tsv"))
	now, _ := os.ReadFile(filepath.Join(app, "rows.tsv"))
	if lines := strings.Split(strings.TrimSpace(string(now)), "\n"); len(kept) != 1 || len(lines) != 2 || !strings.HasPrefix(lines[1], "2026-11-18\t") {
		t.Fatalf("set aside %v, results now:\n%s", kept, now)
	}
}

func TestDailyRowChecksEjectionsAgainstACloneAndSaysNoneForADayWithoutReds(t *testing.T) {
	dir := standIns(t)
	if status, _ := dailyRow(t, dir, "COMMIT="+strings.Repeat("a", 40), "BANYAN_CLONE=/a/clone"); status != 0 {
		t.Fatalf("status %d", status)
	}
	asked, _ := os.ReadFile(filepath.Join(dir, "asked"))
	if live := strings.Split(string(asked), "\n")[0]; !strings.Contains(live, " -clone /a/clone ") || !strings.HasSuffix(live, " -row") {
		t.Fatalf("the live score was asked with: %s", live)
	}
	row, _ := os.ReadFile(filepath.Join(dir, "state", "app", "rows", "2026-11-18.md"))
	if !strings.Contains(string(row), "| Red candidates | none |") {
		t.Fatalf("a day without a red candidate reads:\n%s", row)
	}
}

func TestDailyRowTripsOverThreePassingCandidatesThrownAwayInHeyNativeApps(t *testing.T) {
	for _, c := range []struct {
		repo, thrown, trip string
	}{
		{"basecamp/hey-native-apps", "3", ""},
		{"basecamp/hey-native-apps", "4", "4 passing candidates thrown away unlanded, over 3"},
		{"basecamp/hey2", "4", ""},
	} {
		dir := standIns(t)
		live := `LIVE=,"passing_candidates_thrown_away":` + c.thrown + `,"thrown_away_head_moved":2,"thrown_away_member_left":1`
		if status, _ := dailyRowOf(t, dir, c.repo, "COMMIT="+strings.Repeat("a", 40), live); status != 0 {
			t.Fatalf("status %d", status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "state", filepath.Base(c.repo), "rows", "2026-11-18.md"))
		row := string(data)
		trips := ""
		for _, line := range strings.Split(row, "\n") {
			if strings.HasPrefix(line, "**Tripwires:** ") {
				trips = line
			}
		}
		line := "| Passing candidates thrown away unlanded | " + c.thrown + ": 2 with a member's head moved, 1 with a member gone from the queue |\n"
		if !strings.Contains(row, line) || strings.Contains(trips, "thrown away") != (c.trip != "") || !strings.Contains(trips, c.trip) {
			t.Fatalf("%s, %s thrown away reads:\n%s", c.repo, c.thrown, row)
		}
	}
}

func TestDailyRowHoldsNoLimitOnBuildsPerLanding(t *testing.T) {
	for _, repo := range []string{"basecamp/hey-native-apps", "basecamp/hey2"} {
		dir := standIns(t)
		if status, _ := dailyRowOf(t, dir, repo, "COMMIT="+strings.Repeat("a", 40), `LIVE=,"builds_per_landing":2.5,"passing_candidates_thrown_away":0,"ready_gaps_over_90":0,"queue_added_p50_minutes":1,"wrong_ejections":0`); status != 0 {
			t.Fatalf("status %d", status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "state", filepath.Base(repo), "rows", "2026-11-18.md"))
		if row := string(data); !strings.Contains(row, "| Builds per landing | 2.5 |\n") || !strings.Contains(row, "No tripwire breached") {
			t.Fatalf("%s reads:\n%s", repo, row)
		}
	}
}
