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
	cmd := exec.Command("bash", filepath.Join(dir, "daily-row"), "o/app", "2026-11-18")
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
			"*) echo \"{\\\"commit\\\":\\\"$COMMIT\\\",\\\"landed\\\":1,\\\"ticks\\\":2,\\\"red_candidates\\\":0,\\\"red_read_n\\\":0}\" ;; esac\n",
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
