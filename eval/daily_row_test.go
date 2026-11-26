package main

import (
	"encoding/json"
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
	return dailyRowWith(t, dir, []string{repo, "2026-11-18"}, env...)
}

func dailyRowWith(t *testing.T, dir string, args []string, env ...string) (status, posts int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(dir, "daily-row")}, args...)...)
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

const (
	dayTakeouts    = `{"takeouts":[],"by_class":{},"comments_read":0}`
	beforeTakeouts = `{"takeouts":[],"by_class":{},"comments_read":7}`
)

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
		"fetch-ticks": "#!/bin/sh\necho \"$*\" >> \"$T/fetched\"\nexit \"${FETCH_EXIT:-0}\"\n",
		"eval": "#!/bin/sh\necho \"$*\" >> \"$T/asked\"\nBEFORE_NONE='" + beforeTakeouts + "'\ncase \" $* \" in\n" +
			"*\" -takeouts \"*\" -from 2026-11-17T21:17:00Z \"*) [ \"${BEFORE_TAKEOUTS_EXIT:-0}\" = 0 ] || exit \"$BEFORE_TAKEOUTS_EXIT\"; echo \"${BEFORE_TAKEOUTS:-$BEFORE_NONE}\" ;;\n" +
			"*\" -takeouts \"*) echo '" + dayTakeouts + "' ;;\n" +
			"*\" -from 2026-11-17T21:17:00Z \"*) [ \"${BEFORE_EXIT:-0}\" = 0 ] || exit \"$BEFORE_EXIT\"; echo \"{\\\"commit\\\":\\\"$COMMIT\\\",\\\"ticks\\\":${BEFORE_TICKS:-1}$BEFORE_LIVE}\" ;;\n" +
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

const (
	beforeLive = `,"landed":3,"wait_p50_minutes":40,"wait_p90_minutes":80,"wait_p50":1.25,"wait_p90":2.5,"build_median_minutes":32,` +
		`"queue_added_p50_minutes":9,"queue_added_p90_minutes":20,"queue_added_over_30":1,"split_landings":3,"unsplit_landings":0,"builds_per_landing":0.5,` +
		`"ready_gaps":2,"longest_ready_gap_minutes":95,"longest_ready_gap_from":"2026-11-17T22:00:00Z","longest_ready_gap_to":"2026-11-17T23:35:00Z","ready_gaps_over_90":1,` +
		`"red_candidates":1,"red_pull_requests":4,"red_read_n":1,"red_read_median_minutes":12,` +
		`"passing_candidates_thrown_away":5,"thrown_away_head_moved":4,"thrown_away_member_left":1,"wrong_ejections":3,"unchecked":2,` +
		`"busiest_hour_landings":9,"backlog_longest_minutes":75`
	beforeOuts = `{"takeouts":[{"at":"2026-11-17T23:00:00Z","number":7,"head":"abc","attributed":"checks","class":"own check","failed_then_passed":["build"],"landed_same_head":false},` +
		`{"at":"2026-11-17T23:30:00Z","number":7,"head":"abc","attributed":"bob","class":"bob","failed_then_passed":[],"landed_same_head":false}],` +
		`"by_class":{"own check":{"count":1}},"twice":[{"number":7,"head":"abc"}],"comments_read":40}`
	beforeTitle = "**The four hours before the window, 21:17Z on 2026-11-17 to 01:17Z on 2026-11-18**"
	beforeBlock = beforeTitle + ": 3 landings. Ready to landed took 40 minutes at the median and 80 at p90. The queue added 9 at the median and 20 at p90.\n" +
		"\n" +
		"| | |\n" +
		"|---|---|\n" +
		"| Ready to landed, median / p90 | 40 / 80 min (1.25 / 2.5 builds of 32 min) |\n" +
		"| Landings held over 30 minutes | 1 of 3 (0 more could not be split) |\n" +
		"| Builds per landing | 0.5 |\n" +
		"| Landings in the busiest hour | 9, not judged outside the day's window (the longest backlog stood 75 min) |\n" +
		"| Longest wait of a ready head with no landing | 95 min, 22:00Z to 23:35Z |\n" +
		"| Red candidates | 1, holding 4 pull requests; failed job seen to read red 12 min at the median over 1 |\n" +
		"| Passing candidates thrown away unlanded | 5: 4 with a member's head moved, 1 with a member gone from the queue |\n" +
		"| Take-outs | 2: 0 blamed in a candidate, 1 own check, 0 no gate, 0 conflict, 1 bob |\n" +
		"| Of those, a failed check passed later on the same commit | 1 (1 own check) |\n"
)

func recordBefore(t *testing.T, dir, repo string) {
	t.Helper()
	ticks := filepath.Join(dir, "state", filepath.Base(repo), "ticks")
	if err := os.MkdirAll(ticks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ticks, "20261117T220000Z-0.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sameJSON(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("%s: %v", want, err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestDailyRowGivesTheFourHoursBeforeItsWindow(t *testing.T) {
	dir := standIns(t)
	recordBefore(t, dir, "o/app")
	commit := strings.Repeat("a", 40)
	env := []string{"COMMIT=" + commit, "BANYAN_CLONE=/a/clone", "BEFORE_LIVE=" + beforeLive, "BEFORE_TAKEOUTS=" + beforeOuts, "BANYAN_ROW_POST=" + filepath.Join(dir, "post")}
	rows := filepath.Join(dir, "state", "app", "rows")
	if status, posts := dailyRowWith(t, dir, []string{"o/app", "2026-11-18", "hey2"}, env...); status != 0 || posts != 1 {
		t.Fatalf("status %d, posts %d", status, posts)
	}
	made, _ := os.ReadFile(filepath.Join(rows, "2026-11-18.json"))
	var row struct {
		Made     string          `json:"made"`
		Live     json.RawMessage `json:"live"`
		Takeouts json.RawMessage `json:"takeouts"`
		Before   struct {
			From     string          `json:"from"`
			To       string          `json:"to"`
			Live     json.RawMessage `json:"live"`
			Takeouts json.RawMessage `json:"takeouts"`
		} `json:"before"`
	}
	if err := json.Unmarshal(made, &row); err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, row.Live, `{"commit":"`+commit+`","landed":1,"ticks":2,"red_candidates":0,"red_read_n":0}`) || !sameJSON(t, row.Takeouts, dayTakeouts) {
		t.Fatalf("the day's own reads changed:\n%s", made)
	}
	if row.Before.From != "2026-11-17T21:17:00Z" || row.Before.To != "2026-11-18T01:17:00Z" ||
		!sameJSON(t, row.Before.Live, `{"commit":"`+commit+`","ticks":1`+beforeLive+`}`) || !sameJSON(t, row.Before.Takeouts, beforeOuts) {
		t.Fatalf("the four hours before read:\n%s", made)
	}
	for name, want := range map[string]string{"2026-11-18.before.live.json": string(row.Before.Live), "2026-11-18.before.takeouts.json": beforeOuts} {
		if kept, _ := os.ReadFile(filepath.Join(rows, name)); !sameJSON(t, kept, want) {
			t.Fatalf("%s holds %s", name, kept)
		}
	}

	asked, _ := os.ReadFile(filepath.Join(dir, "asked"))
	lines := strings.Split(strings.TrimSpace(string(asked)), "\n")
	ticks := filepath.Join(dir, "state", "app", "ticks")
	if len(lines) != 4 || !strings.Contains(lines[0], " -like hey2 -clone /a/clone -row") ||
		lines[2] != "-live "+ticks+" -from 2026-11-17T21:17:00Z -to 2026-11-18T01:17:00Z -clone /a/clone -row" ||
		lines[3] != "-takeouts o/app -from 2026-11-17T21:17:00Z -to 2026-11-18T01:17:00Z" {
		t.Fatalf("the eval was asked:\n%s", asked)
	}

	md, _ := os.ReadFile(filepath.Join(rows, "2026-11-18.md"))
	if !strings.HasSuffix(string(md), "`aaaaaaaaaaaa`, and 0 comments.\n\n"+beforeBlock) || !strings.Contains(string(md), "\n**Tripwires:** waits of a ready head were not measured; wrong ejections were not measured.\n") {
		t.Fatalf("the row reads:\n%s", md)
	}

	tsv, _ := os.ReadFile(filepath.Join(dir, "state", "app", "rows.tsv"))
	want := "day\tmade\tcommit\tlanded\tready_p50_min\tready_p90_min\tadded_p50_min\tadded_p90_min\tover_30\tsplit\tbuilds_per_landing\ttakeouts\tpassed_later\tnot_read\tlongest_gap_min\twrong\tunchecked\tsim_over_live\n" +
		"2026-11-18\t" + row.Made + "\taaaaaaaaaaaa\t1\t\t\t\t\t\t\t\t0\t0\t0\t\t\t0\t///\n"
	if string(tsv) != want {
		t.Fatalf("rows.tsv reads:\n%q", tsv)
	}

	if status, posts := dailyRowWith(t, dir, []string{"o/app", "2026-11-18", "hey2"}, env...); status != 0 || posts != 1 {
		t.Fatalf("a second run: status %d, posts %d", status, posts)
	}
	if again, _ := os.ReadFile(filepath.Join(rows, "2026-11-18.json")); string(again) != string(made) {
		t.Fatal("a second run replaced the day's row")
	}
	if again, _ := os.ReadFile(filepath.Join(rows, "2026-11-18.md")); string(again) != string(md) {
		t.Fatal("a second run replaced the day's Markdown")
	}
}

func TestDailyRowFetchesTheRecordsBothWindowsReadBackOver(t *testing.T) {
	dir := standIns(t)
	if status, _ := dailyRow(t, dir, "COMMIT="+strings.Repeat("a", 40)); status != 0 {
		t.Fatalf("status %d", status)
	}
	fetched, _ := os.ReadFile(filepath.Join(dir, "fetched"))
	want := "o/app " + filepath.Join(dir, "state", "app") + " 2026-11-14T21:17:00Z 2026-11-18T21:22:00Z\n"
	if string(fetched) != want {
		t.Errorf("fetched %q, want one fetch from 72 hours before the four hours open: %q", fetched, want)
	}
}

func TestDailyRowTripsOnTheDayAloneNotTheFourHoursBefore(t *testing.T) {
	clean := `,"builds_per_landing":0.5,"passing_candidates_thrown_away":0,"ready_gaps_over_90":0,"queue_added_p50_minutes":1,"wrong_ejections":0`
	breach := `,"builds_per_landing":0.5,"passing_candidates_thrown_away":5,"ready_gaps_over_90":0,"queue_added_p50_minutes":9,"wrong_ejections":0`
	for _, c := range []struct {
		day, trips string
	}{
		{clean, "No tripwire breached."},
		{breach, "**Tripwires:** minutes the queue adds at the median is 9, over 6; 5 passing candidates thrown away unlanded, over 3."},
	} {
		dir := standIns(t)
		recordBefore(t, dir, "basecamp/hey-native-apps")
		if status, _ := dailyRowOf(t, dir, "basecamp/hey-native-apps", "COMMIT="+strings.Repeat("a", 40), "LIVE="+c.day, "BEFORE_LIVE="+beforeLive, "BEFORE_TAKEOUTS="+beforeOuts); status != 0 {
			t.Fatalf("status %d", status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "state", "hey-native-apps", "rows", "2026-11-18.md"))
		row := string(data)
		if !strings.Contains(row, "\n"+c.trips+"\n") || strings.Count(row, "Tripwires") > 1 || !strings.HasSuffix(row, beforeBlock) {
			t.Fatalf("a day of %s reads:\n%s", c.day, row)
		}
	}
}

func TestDailyRowSaysWhenTheFourHoursBeforeHoldNoRecordsOrNoLanding(t *testing.T) {
	for _, c := range []struct {
		name    string
		records bool
		env     []string
		line    string
	}{
		{"no tick in the four hours", false, nil, beforeTitle + ": no records cover them.\n"},
		{"no tick the eval counts", true, []string{"BEFORE_TICKS=0"}, beforeTitle + ": no records cover them.\n"},
		{"ticks and no landing", true, []string{`BEFORE_LIVE=,"landed":0,"red_candidates":2`}, beforeTitle + ": no landings.\n"},
	} {
		dir := standIns(t)
		if c.records {
			recordBefore(t, dir, "o/app")
		}
		if status, _ := dailyRow(t, dir, append([]string{"COMMIT=" + strings.Repeat("a", 40)}, c.env...)...); status != 0 {
			t.Fatalf("%s: status %d", c.name, status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "state", "app", "rows", "2026-11-18.md"))
		row := string(data)
		if !strings.HasSuffix(row, " comments.\n\n"+c.line) || strings.Count(row, "| | |") != 1 {
			t.Fatalf("%s reads:\n%s", c.name, row)
		}
		asked, _ := os.ReadFile(filepath.Join(dir, "asked"))
		if strings.Contains(string(asked), "-live "+filepath.Join(dir, "state", "app", "ticks")+" -from 2026-11-17T21:17:00Z") != c.records {
			t.Fatalf("%s: the eval was asked:\n%s", c.name, asked)
		}
	}
}

func TestDailyRowIsNotMadeWhenAReadOfTheFourHoursBeforeFails(t *testing.T) {
	for _, failing := range []string{"BEFORE_EXIT=4", "BEFORE_TAKEOUTS_EXIT=5"} {
		dir := standIns(t)
		recordBefore(t, dir, "o/app")
		status, posts := dailyRow(t, dir, "COMMIT="+strings.Repeat("a", 40), "BANYAN_ROW_POST="+filepath.Join(dir, "post"), failing)
		if rows := filepath.Join(dir, "state", "app", "rows"); status == 0 || posts != 0 || exists(rows, "2026-11-18.json") || exists(rows, "2026-11-18.md") || exists(dir, "state", "app", "rows.tsv") {
			t.Fatalf("%s: status %d, posts %d", failing, status, posts)
		}
	}
}

func TestDailyRowJudgesTheBusiestHourOnlyOnADayABacklogStoodAnHour(t *testing.T) {
	clean := `,"passing_candidates_thrown_away":0,"ready_gaps_over_90":0,"queue_added_p50_minutes":1,"wrong_ejections":0`
	for _, c := range []struct {
		repo, busiest, backlog, line, trips string
	}{
		{"basecamp/hey-native-apps", "22", "179", "22 against a target of 16, judged: a backlog stood for 179 min that day", "No tripwire breached."},
		{"basecamp/hey-native-apps", "12", "67.4", "12 against a target of 16, judged: a backlog stood for 67 min that day", "**Tripwires:** 12 landings in the busiest hour of a day a backlog stood an hour, under 16."},
		{"basecamp/hey2", "11", "60", "11 against a target of 12, judged: a backlog stood for 60 min that day", "**Tripwires:** 11 landings in the busiest hour of a day a backlog stood an hour, under 12."},
		{"basecamp/hey-native-apps", "16", "90", "16 against a target of 16, judged: a backlog stood for 90 min that day", "No tripwire breached."},
		{"basecamp/hey2", "12", "90", "12 against a target of 12, judged: a backlog stood for 90 min that day", "No tripwire breached."},
		{"o/app", "2", "65", "2, with no target for this queue", "No tripwire breached."},
		{"basecamp/hey2", "5", "59.96", "5, not judged: no backlog stood for an hour (the longest stood 59 min)", "No tripwire breached."},
		{"basecamp/hey-native-apps", "0", "0", "0, not judged: no backlog stood for an hour (the longest stood 0 min)", "No tripwire breached."},
	} {
		dir := standIns(t)
		live := "LIVE=" + clean + `,"busiest_hour_landings":` + c.busiest + `,"backlog_longest_minutes":` + c.backlog
		if status, _ := dailyRowOf(t, dir, c.repo, "COMMIT="+strings.Repeat("a", 40), live); status != 0 {
			t.Fatalf("status %d", status)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "state", filepath.Base(c.repo), "rows", "2026-11-18.md"))
		row := string(data)
		want := "| Builds per landing | not measured |\n| Landings in the busiest hour | " + c.line + " |\n| Longest wait"
		if !strings.Contains(row, want) || !strings.Contains(row, "\n"+c.trips+"\n") {
			t.Fatalf("%s, %s in the busiest hour with a backlog of %s minutes, reads:\n%s", c.repo, c.busiest, c.backlog, row)
		}
	}
}
