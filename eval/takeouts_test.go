package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const takeoutFrom = "2026-11-18T00:00:00Z"
const takeoutTo = "2026-11-19T00:00:00Z"
const takeoutCommentsPath = "repos/owner/repo/issues/comments?since=2026-11-17T23%3A59%3A59Z&sort=created&direction=asc&per_page=100&page=1"
const takeoutPullPath = "repos/owner/repo/pulls/7"
const takeoutChecksPath = "repos/owner/repo/commits/judged/check-runs?per_page=100&page=1&filter=all"
const takeoutCommentJSON = `{"created_at":"2026-11-18T12:00:00Z","issue_url":"https://api.github.com/repos/owner/repo/issues/7","user":{"type":"Bot","login":"queue[bot]"},"body":"Taken out.\n<!-- banyan eject judged attributed=checks -->"}`
const takeoutOpenJSON = `{"merged":false,"state":"open","head":{"sha":"judged"}}`
const takeoutEmptyChecksJSON = `{"total_count":0,"check_runs":[]}`

func takeoutAPI(t *testing.T, bodies map[string]string) (func(string) ([]byte, error), *[]string) {
	t.Helper()
	var paths []string
	return func(path string) ([]byte, error) {
		paths = append(paths, path)
		body, ok := bodies[path]
		if !ok {
			t.Errorf("unexpected read: %s", path)
		}
		if body == "" {
			return nil, fmt.Errorf("unread: %s", path)
		}
		return []byte(body), nil
	}, &paths
}

func takeoutBodies() map[string]string {
	return map[string]string{
		takeoutCommentsPath: "[" + takeoutCommentJSON + "]",
		takeoutPullPath:     takeoutOpenJSON,
		takeoutChecksPath:   takeoutEmptyChecksJSON,
		takeoutStatusesPath: "[]",
	}
}

func takeoutRead(t *testing.T, bodies map[string]string) takeoutReport {
	t.Helper()
	read, _ := takeoutAPI(t, bodies)
	report, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, read)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestTakeoutClasses(t *testing.T) {
	for _, test := range []struct{ attributed, class, counted string }{
		{"culprit", "blamed in a candidate", "blamed in a candidate"},
		{"checks", "own check", "own check"},
		{"nogate", "no gate", "no gate"},
		{"conflict", "conflict", "conflict"},
		{"new-reason", "new-reason", "other"},
	} {
		t.Run(test.attributed, func(t *testing.T) {
			bodies := takeoutBodies()
			bodies[takeoutCommentsPath] = "[" + strings.ReplaceAll(takeoutCommentJSON, "attributed=checks", "attributed="+test.attributed) + "]"
			report := takeoutRead(t, bodies)
			if len(report.Takeouts) != 1 {
				t.Fatalf("take-outs: %+v", report.Takeouts)
			}
			out := report.Takeouts[0]
			if out.At.Format("2006-01-02T15:04:05Z07:00") != "2026-11-18T12:00:00Z" || out.Number != 7 || out.Head != "judged" || out.Attributed != test.attributed || out.Class != test.class || out.By != "queue[bot]" {
				t.Fatalf("take-out: %+v", out)
			}
			want := map[string]takeoutCounts{"blamed in a candidate": {}, "own check": {}, "no gate": {}, "conflict": {}}
			want[test.counted] = takeoutCounts{Count: 1}
			if !reflect.DeepEqual(report.ByClass, want) {
				t.Fatalf("classes = %+v, want %+v", report.ByClass, want)
			}
		})
	}
}

func TestTakeoutIgnoresQuotedInlineAndPersonalMarkers(t *testing.T) {
	for _, comment := range []string{
		strings.ReplaceAll(takeoutCommentJSON, "<!--", "> <!--"),
		strings.ReplaceAll(takeoutCommentJSON, "<!--", "Some text <!--"),
		strings.ReplaceAll(takeoutCommentJSON, `"type":"Bot"`, `"type":"User"`),
	} {
		report := takeoutRead(t, map[string]string{takeoutCommentsPath: "[" + comment + "]"})
		if len(report.Takeouts) != 0 || report.CommentsRead != 1 || report.Unread != 0 {
			t.Fatalf("report: %+v", report)
		}
	}
}

func TestTakeoutAcceptsRepositoryMarkers(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutCommentsPath] = "[" + strings.ReplaceAll(takeoutCommentJSON, "banyan eject", "merge-queue eject") + "]"
	if report := takeoutRead(t, bodies); len(report.Takeouts) != 1 || report.Takeouts[0].Head != "judged" {
		t.Fatalf("take-outs: %+v", report.Takeouts)
	}
}

func TestTakeoutWindowUsesCreationTimeAndExcludesTo(t *testing.T) {
	bodies := takeoutBodies()
	var comments []string
	for _, at := range []string{"2026-11-17T23:59:59Z", takeoutFrom, "2026-11-18T23:59:59.999Z", takeoutTo} {
		comment := strings.ReplaceAll(takeoutCommentJSON, "2026-11-18T12:00:00Z", at)
		if at == "2026-11-17T23:59:59Z" {
			comment = strings.Replace(comment, `"created_at":`, `"updated_at":"2026-11-18T13:00:00Z","created_at":`, 1)
		}
		comments = append(comments, comment)
	}
	bodies[takeoutCommentsPath] = "[" + strings.Join(comments, ",") + "]"
	report := takeoutRead(t, bodies)
	var ats []string
	for _, out := range report.Takeouts {
		ats = append(ats, out.At.Format("2006-01-02T15:04:05.999Z07:00"))
	}
	if !reflect.DeepEqual(ats, []string{takeoutFrom, "2026-11-18T23:59:59.999Z"}) || report.CommentsRead != 4 {
		t.Fatalf("times = %v, comments read = %d", ats, report.CommentsRead)
	}
}

func TestTakeoutReadsASecondCommentPage(t *testing.T) {
	bodies := takeoutBodies()
	unmarked := strings.ReplaceAll(takeoutCommentJSON, "eject", "unmarked")
	bodies[takeoutCommentsPath] = "[" + strings.TrimSuffix(strings.Repeat(unmarked+",", 100), ",") + "]"
	second := strings.ReplaceAll(takeoutCommentsPath, "&page=1", "&page=2")
	bodies[second] = "[" + takeoutCommentJSON + "]"
	read, paths := takeoutAPI(t, bodies)
	report, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, read)
	want := []string{takeoutCommentsPath, second, takeoutPullPath, takeoutChecksPath, takeoutStatusesPath}
	if err != nil || report.CommentsRead != 101 || len(report.Takeouts) != 1 || !reflect.DeepEqual(*paths, want) {
		t.Fatalf("report=%+v paths=%v err=%v", report, *paths, err)
	}
}

func TestTakeoutStopsWhenAPageStartsAtTo(t *testing.T) {
	comment := strings.ReplaceAll(takeoutCommentJSON, "2026-11-18T12:00:00Z", takeoutTo)
	report := takeoutRead(t, map[string]string{takeoutCommentsPath: "[" + strings.TrimSuffix(strings.Repeat(comment+",", 100), ",") + "]"})
	if report.CommentsRead != 100 || len(report.Takeouts) != 0 {
		t.Fatalf("report: %+v", report)
	}
}

func TestTakeoutPullOutcomes(t *testing.T) {
	for _, test := range []struct {
		name, pr                    string
		landed, moved, merged, open bool
	}{
		{"landed unchanged", `{"merged":true,"state":"closed","head":{"sha":"judged"}}`, true, false, true, false},
		{"head moved", `{"merged":false,"state":"open","head":{"sha":"new"}}`, false, true, false, true},
		{"merged after moving", `{"merged":true,"state":"closed","head":{"sha":"new"}}`, false, true, true, false},
		{"closed", `{"merged":false,"state":"closed","head":{"sha":"judged"}}`, false, false, false, false},
		{"open", takeoutOpenJSON, false, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			bodies := takeoutBodies()
			bodies[takeoutPullPath] = test.pr
			report := takeoutRead(t, bodies)
			out := report.Takeouts[0]
			if out.LandedSameHead == nil || out.HeadMoved == nil || out.Merged == nil || out.Open == nil || *out.LandedSameHead != test.landed || *out.HeadMoved != test.moved || *out.Merged != test.merged || *out.Open != test.open {
				t.Fatalf("outcome: %+v", out)
			}
			want := takeoutCounts{Count: 1}
			if test.landed {
				want.LandedSameHead = 1
			}
			if report.ByClass["own check"] != want {
				t.Fatalf("counts: %+v", report.ByClass)
			}
		})
	}
}

func TestTakeoutChecksFailedByTheComment(t *testing.T) {
	for _, conclusion := range []string{"failure", "timed_out", "cancelled"} {
		t.Run(conclusion, func(t *testing.T) {
			bodies := takeoutBodies()
			bodies[takeoutChecksPath] = fmt.Sprintf(`{"check_runs":[{"name":"build","status":"completed","conclusion":%q,"completed_at":"2026-11-18T12:00:00Z"}]}`, conclusion)
			report := takeoutRead(t, bodies)
			out := report.Takeouts[0]
			if !reflect.DeepEqual(out.Failed, []string{"build"}) || !reflect.DeepEqual(out.FailedThenPassed, []string{}) || report.ByClass["own check"] != (takeoutCounts{Count: 1}) {
				t.Fatalf("outcome=%+v counts=%+v", out, report.ByClass)
			}
		})
	}
}

func TestTakeoutChecksThatFailedLaterAreExcluded(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutChecksPath] = `{"check_runs":[
		{"name":"late","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T12:00:01Z"},
		{"name":"late","status":"completed","conclusion":"success","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"}
	]}`
	out := takeoutRead(t, bodies).Takeouts[0]
	if !reflect.DeepEqual(out.Failed, []string{}) || !reflect.DeepEqual(out.FailedThenPassed, []string{}) {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestTakeoutChecksASecondFailureIsNotAPass(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutChecksPath] = `{"check_runs":[
		{"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"},
		{"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"}
	]}`
	out := takeoutRead(t, bodies).Takeouts[0]
	if !reflect.DeepEqual(out.Failed, []string{"build"}) || !reflect.DeepEqual(out.FailedThenPassed, []string{}) {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestTakeoutChecksASecondPageCanPassAnEarlierFailure(t *testing.T) {
	bodies := takeoutBodies()
	failed := `{"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"}`
	bodies[takeoutChecksPath] = `{"total_count":101,"check_runs":[` + strings.TrimSuffix(strings.Repeat(failed+",", 100), ",") + "]}"
	second := strings.ReplaceAll(takeoutChecksPath, "&page=1", "&page=2")
	bodies[second] = `{"total_count":101,"check_runs":[{"name":"build","status":"completed","conclusion":"success","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"}]}`
	read, paths := takeoutAPI(t, bodies)
	report, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, read)
	if err != nil {
		t.Fatal(err)
	}
	out := report.Takeouts[0]
	if !reflect.DeepEqual(out.Failed, []string{"build"}) || !reflect.DeepEqual(out.FailedThenPassed, []string{"build"}) || report.ByClass["own check"] != (takeoutCounts{Count: 1, FailedThenPassed: 1}) || !reflect.DeepEqual(*paths, []string{takeoutCommentsPath, takeoutPullPath, takeoutChecksPath, second, takeoutStatusesPath}) {
		t.Fatalf("outcome=%+v counts=%+v paths=%v", out, report.ByClass, *paths)
	}
}

func TestTakeoutChecksRerunsStartedBeforeTheCommentCount(t *testing.T) {
	for _, started := range []string{"2026-11-18T11:30:00Z", "2026-11-18T12:00:00Z"} {
		bodies := takeoutBodies()
		bodies[takeoutChecksPath] = fmt.Sprintf(`{"check_runs":[
			{"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"},
			{"name":"build","status":"completed","conclusion":"success","started_at":%q,"completed_at":"2026-11-18T13:00:00Z"}
		]}`, started)
		out := takeoutRead(t, bodies).Takeouts[0]
		if !reflect.DeepEqual(out.FailedThenPassed, []string{"build"}) {
			t.Fatalf("started=%s outcome=%+v", started, out)
		}
	}
}

func TestTakeoutChecksNamesAreSortedAndCountedOnce(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutChecksPath] = `{"check_runs":[
		{"name":"z","status":"completed","conclusion":"success","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"},
		{"name":"a","status":"completed","conclusion":"success","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"},
		{"name":"z","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"},
		{"name":"a","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"},
		{"name":"z","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T15:00:00Z"}
	]}`
	report := takeoutRead(t, bodies)
	out := report.Takeouts[0]
	if !reflect.DeepEqual(out.Failed, []string{"a", "z"}) || !reflect.DeepEqual(out.FailedThenPassed, []string{"a", "z"}) || report.ByClass["own check"] != (takeoutCounts{Count: 1, FailedThenPassed: 1}) {
		t.Fatalf("outcome=%+v counts=%+v", out, report.ByClass)
	}
}

func TestTakeoutUnreadPullLeavesItsFieldsNull(t *testing.T) {
	for _, body := range []string{"", "invalid JSON"} {
		bodies := takeoutBodies()
		bodies[takeoutPullPath] = body
		report := takeoutRead(t, bodies)
		out := report.Takeouts[0]
		if report.Unread != 1 || out.LandedSameHead != nil || out.HeadMoved != nil || out.Merged != nil || out.Open != nil || !reflect.DeepEqual(out.Failed, []string{}) || !reflect.DeepEqual(out.FailedThenPassed, []string{}) || report.ByClass["own check"] != (takeoutCounts{Count: 1}) {
			t.Fatalf("report: %+v, outcome: %+v", report, out)
		}
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"landed_same_head", "head_moved", "merged", "open"} {
			if !strings.Contains(string(data), `"`+field+`":null`) {
				t.Errorf("%s is not null: %s", field, data)
			}
		}
	}
}

func TestTakeoutUnreadCheckPageLeavesAllChecksNull(t *testing.T) {
	bodies := takeoutBodies()
	failed := `{"name":"build","status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"}`
	bodies[takeoutChecksPath] = `{"check_runs":[` + strings.TrimSuffix(strings.Repeat(failed+",", 100), ",") + "]}"
	bodies[strings.ReplaceAll(takeoutChecksPath, "&page=1", "&page=2")] = ""
	report := takeoutRead(t, bodies)
	data, err := json.Marshal(report.Takeouts[0])
	if err != nil {
		t.Fatal(err)
	}
	if report.Unread != 1 || !strings.Contains(string(data), `"failed":null,"failed_then_passed":null`) || report.Takeouts[0].Open == nil || !*report.Takeouts[0].Open || report.ByClass["own check"] != (takeoutCounts{Count: 1}) {
		t.Fatalf("unread=%d outcome=%s counts=%+v", report.Unread, data, report.ByClass)
	}
}

func TestTakeoutCountsBothUnreadSources(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutPullPath], bodies[takeoutChecksPath] = "", ""
	report := takeoutRead(t, bodies)
	if report.Unread != 1 || report.ReadsFailed != 2 || len(report.Takeouts) != 1 || report.ByClass["own check"] != (takeoutCounts{Count: 1}) {
		t.Fatalf("report: %+v", report)
	}
}

func TestTakeoutTwiceUsesBothNumberAndHead(t *testing.T) {
	bodies := takeoutBodies()
	newHead := strings.ReplaceAll(takeoutCommentJSON, "eject judged", "eject changed")
	newNumber := strings.ReplaceAll(takeoutCommentJSON, "issues/7", "issues/8")
	bodies[takeoutCommentsPath] = "[" + strings.Join([]string{takeoutCommentJSON, takeoutCommentJSON, takeoutCommentJSON, newHead, newNumber}, ",") + "]"
	bodies["repos/owner/repo/pulls/8"] = takeoutOpenJSON
	bodies[strings.ReplaceAll(takeoutChecksPath, "commits/judged", "commits/changed")] = takeoutEmptyChecksJSON
	bodies[strings.ReplaceAll(takeoutStatusesPath, "commits/judged", "commits/changed")] = "[]"
	read, paths := takeoutAPI(t, bodies)
	report, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, read)
	if err != nil || !reflect.DeepEqual(report.Twice, []takeoutHead{{Number: 7, Head: "judged"}}) || len(report.Takeouts) != 5 || len(*paths) != 16 || report.ByClass["own check"] != (takeoutCounts{Count: 5}) {
		t.Fatalf("report=%+v paths=%v err=%v", report, *paths, err)
	}
}

func TestTakeoutCommentsMustBeReadable(t *testing.T) {
	for _, body := range []string{"", "invalid JSON"} {
		read, _ := takeoutAPI(t, map[string]string{takeoutCommentsPath: body})
		if _, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, read); err == nil {
			t.Fatalf("unread comments %q accepted", body)
		}
	}
}

func TestTakeoutRejectsInvalidArgumentsBeforeReading(t *testing.T) {
	for _, args := range [][3]string{
		{"repo", takeoutFrom, takeoutTo},
		{"owner/repo", "", takeoutTo},
		{"owner/repo", takeoutFrom, ""},
		{"owner/repo", "yesterday", takeoutTo},
		{"owner/repo", takeoutFrom, "tomorrow"},
		{"owner/repo", takeoutFrom, takeoutFrom},
		{"owner/repo", takeoutTo, takeoutFrom},
	} {
		read, paths := takeoutAPI(t, nil)
		if _, err := readTakeouts(args[0], args[1], args[2], read); err == nil || len(*paths) != 0 {
			t.Fatalf("args=%v paths=%v err=%v", args, *paths, err)
		}
	}
}

func TestTakeoutSinceEscapesTimezoneOffsets(t *testing.T) {
	from := "2026-11-18T00:00:00+08:00"
	path := strings.ReplaceAll(takeoutCommentsPath, "2026-11-17T23%3A59%3A59Z", "2026-11-17T23%3A59%3A59%2B08%3A00")
	read, paths := takeoutAPI(t, map[string]string{path: "[]"})
	report, err := readTakeouts("owner/repo", from, takeoutTo, read)
	if err != nil || report.From.Format("2006-01-02T15:04:05Z07:00") != from || !reflect.DeepEqual(*paths, []string{path}) {
		t.Fatalf("report=%+v paths=%v err=%v", report, *paths, err)
	}
}

func TestTakeoutGHUsesTheSelectedBinary(t *testing.T) {
	for _, selected := range []bool{true, false} {
		t.Run(fmt.Sprint(selected), func(t *testing.T) {
			dir := t.TempDir()
			bin := dir + "/gh"
			if selected {
				bin = dir + "/chosen gh"
				t.Setenv("BANYAN_GH", bin)
			} else {
				t.Setenv("BANYAN_GH", "")
				t.Setenv("PATH", dir)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = api ] && [ \"$2\" = 'a/path?x=1&y=2' ] || exit 1\nprintf '[]'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			body, err := ghAPI("a/path?x=1&y=2")
			if err != nil || string(body) != "[]" {
				t.Fatalf("body=%s err=%v", body, err)
			}
		})
	}
}

func TestTakeoutRunPrintsOneJSONLine(t *testing.T) {
	bin := t.TempDir() + "/gh"
	script := "#!/bin/sh\n[ \"$#\" -eq 2 ] && [ \"$1\" = api ] && [ \"$2\" = '" + takeoutCommentsPath + "' ] || exit 1\nprintf '[]'\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BANYAN_GH", bin)
	f, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	err = runTakeouts("owner/repo", takeoutFrom, takeoutTo)
	os.Stdout = old
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"repo":"owner/repo","from":"2026-11-18T00:00:00Z","to":"2026-11-19T00:00:00Z","eval_commit":` + strconv.Quote(evalRevision()) + `,"comments_read":0,"takeouts":[],"by_class":{"blamed in a candidate":{"count":0,"landed_same_head":0,"failed_then_passed":0},"conflict":{"count":0,"landed_same_head":0,"failed_then_passed":0},"no gate":{"count":0,"landed_same_head":0,"failed_then_passed":0},"own check":{"count":0,"landed_same_head":0,"failed_then_passed":0}},"twice":[],"unread":0,"reads_failed":0,"still_out":[]}` + "\n"
	if string(data) != want {
		t.Fatalf("row = %s, want %s", data, want)
	}
}
