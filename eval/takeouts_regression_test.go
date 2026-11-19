package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const takeoutStatusesPath = "repos/owner/repo/commits/judged/statuses?per_page=100&page=1"

func TestTakeoutCheckIdentityAndRerunStart(t *testing.T) {
	for _, test := range []struct {
		name, success string
		want          []string
	}{
		{"rerun before comment", `{"name":"build","app":{"id":1},"status":"completed","conclusion":"success","started_at":"2026-11-18T10:30:00Z","completed_at":"2026-11-18T11:30:00Z"}`, []string{"build"}},
		{"different app", `{"name":"build","app":{"id":2},"status":"completed","conclusion":"success","started_at":"2026-11-18T13:00:00Z","completed_at":"2026-11-18T14:00:00Z"}`, []string{}},
		{"older overlapping run", `{"name":"build","app":{"id":1},"status":"completed","conclusion":"success","started_at":"2026-11-18T09:00:00Z","completed_at":"2026-11-18T14:00:00Z"}`, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bodies := takeoutBodies()
			bodies[takeoutChecksPath] = `{"check_runs":[{"name":"build","app":{"id":1},"status":"completed","conclusion":"failure","started_at":"2026-11-18T10:00:00Z","completed_at":"2026-11-18T11:00:00Z"},` + test.success + `]}`
			out := takeoutRead(t, bodies).Takeouts[0]
			if !reflect.DeepEqual(out.FailedThenPassed, test.want) {
				t.Fatalf("out: %+v", out)
			}
		})
	}
}

func TestTakeoutFencesAndSpacedName(t *testing.T) {
	for _, fence := range []string{"```", "~~~", "````go", "   ~~~text"} {
		bodies := takeoutBodies()
		var comment takeoutComment
		json.Unmarshal([]byte(takeoutCommentJSON), &comment)
		close := strings.TrimLeft(fence, " ")[:3]
		if strings.HasPrefix(fence, "````") {
			close = "````"
		}
		comment.Body = fence + "\n<!-- merge queue eject abc attributed=checks -->\n" + close
		data, _ := json.Marshal([]takeoutComment{comment})
		bodies[takeoutCommentsPath] = string(data)
		if report := takeoutRead(t, bodies); len(report.Takeouts) != 0 {
			t.Fatalf("fence %q: %+v", fence, report)
		}
		comment.Body += "\n<!-- merge queue eject judged attributed=checks -->"
		data, _ = json.Marshal([]takeoutComment{comment})
		bodies[takeoutCommentsPath] = string(data)
		if report := takeoutRead(t, bodies); len(report.Takeouts) != 1 {
			t.Fatalf("spaced name: %+v", report)
		}
	}
}

func TestTakeoutUnreadCountsTakeoutsOnce(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutPullPath], bodies[takeoutChecksPath] = "", ""
	report := takeoutRead(t, bodies)
	data, _ := json.Marshal(report)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if report.Unread != 1 || fields["reads_failed"] != float64(2) {
		t.Fatalf("report: %s", data)
	}
}

func TestTakeoutStatusesAcrossPages(t *testing.T) {
	bodies := takeoutBodies()
	success := `{"context":"build","state":"success","created_at":"2026-11-18T11:30:00Z"}`
	bodies[takeoutStatusesPath] = "[" + strings.TrimSuffix(strings.Repeat(success+",", 100), ",") + "]"
	bodies[strings.Replace(takeoutStatusesPath, "&page=1", "&page=2", 1)] = `[
 {"context":"build","state":"failure","created_at":"2026-11-18T11:00:00Z"},
 {"context":"error","state":"error","created_at":"2026-11-18T10:00:00Z"},
 {"context":"error","state":"success","created_at":"2026-11-18T10:30:00Z"},
 {"context":"late","state":"failure","created_at":"2026-11-18T12:00:01Z"},
 {"context":"late","state":"success","created_at":"2026-11-18T13:00:00Z"},
 {"context":"equal","state":"error","created_at":"2026-11-18T12:00:00Z"},
 {"context":"equal","state":"success","created_at":"2026-11-18T12:00:00Z"},
 {"context":"older","state":"error","created_at":"2026-11-18T11:00:00Z"},
 {"context":"older","state":"success","created_at":"2026-11-18T10:00:00Z"} ]`
	out := takeoutRead(t, bodies).Takeouts[0]
	if !reflect.DeepEqual(out.Failed, []string{"build", "equal", "error", "older"}) || !reflect.DeepEqual(out.FailedThenPassed, []string{"build", "error"}) {
		t.Fatalf("statuses: %+v", out)
	}
}

func TestTakeoutUnreadStatusesNullBothLists(t *testing.T) {
	for _, body := range []string{"", "invalid JSON"} {
		bodies := takeoutBodies()
		bodies[takeoutChecksPath] = `{"check_runs":[{"name":"build","status":"completed","conclusion":"failure","completed_at":"2026-11-18T11:00:00Z"}]}`
		bodies[takeoutStatusesPath] = body
		report := takeoutRead(t, bodies)
		out := report.Takeouts[0]
		if report.Unread != 1 || out.Failed != nil || out.FailedThenPassed != nil {
			t.Fatalf("report: %+v", report)
		}
	}
}

func TestTakeoutUnreadCountsAllThreeSources(t *testing.T) {
	bodies := takeoutBodies()
	bodies[takeoutPullPath], bodies[takeoutChecksPath], bodies[takeoutStatusesPath] = "", "invalid JSON", "invalid JSON"
	report := takeoutRead(t, bodies)
	data, _ := json.Marshal(report)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if report.Unread != 1 || fields["reads_failed"] != float64(3) {
		t.Fatalf("report: %s", data)
	}
}

func TestTakeoutStillOut(t *testing.T) {
	for _, pr := range []string{takeoutOpenJSON,
		`{"merged":true,"state":"closed","head":{"sha":"judged"}}`,
		`{"merged":false,"state":"open","head":{"sha":"moved"}}`,
		`{"merged":false,"state":"closed","head":{"sha":"judged"}}`, ""} {
		bodies := takeoutBodies()
		bodies[takeoutPullPath] = pr
		report := takeoutRead(t, bodies)
		data, _ := json.Marshal(report)
		var fields map[string]json.RawMessage
		json.Unmarshal(data, &fields)
		var still []takeout
		if err := json.Unmarshal(fields["still_out"], &still); err != nil {
			t.Fatal(err)
		}
		want := 0
		if pr == takeoutOpenJSON {
			want = 1
		}
		if len(still) != want {
			t.Fatalf("pull %s: still_out=%s", pr, fields["still_out"])
		}
	}
}

func TestTakeoutIgnoresFencedMarkers(t *testing.T) {
	for _, body := range []string{
		"```\n<!-- banyan eject judged attributed=checks -->\n```",
		"~~~\n<!-- banyan eject judged attributed=checks -->\n~~~",
		"````\n```\n<!-- banyan eject judged attributed=checks -->\n````",
	} {
		bodies := takeoutBodies()
		var comment takeoutComment
		json.Unmarshal([]byte(takeoutCommentJSON), &comment)
		comment.Body = body
		data, _ := json.Marshal([]takeoutComment{comment})
		bodies[takeoutCommentsPath] = string(data)
		if report := takeoutRead(t, bodies); len(report.Takeouts) != 0 {
			t.Fatalf("body %q: %+v", body, report.Takeouts)
		}
	}
}

func TestTakeoutReadsFromOneSecondBeforeWindow(t *testing.T) {
	var paths []string
	_, err := readTakeouts("owner/repo", takeoutFrom, takeoutTo, func(path string) ([]byte, error) {
		paths = append(paths, path)
		return []byte("[]"), nil
	})
	want := "repos/owner/repo/issues/comments?since=2026-11-17T23%3A59%3A59Z&sort=created&direction=asc&per_page=100&page=1"
	if err != nil || !reflect.DeepEqual(paths, []string{want}) {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
}
