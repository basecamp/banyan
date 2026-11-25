package github

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/basecamp/banyan/internal/queue"
)

func TestOnlyTheConfiguredAppsCheckRunsCountForNamedContexts(t *testing.T) {
	if asked := (Names{App: 7}).contextFields(); !strings.Contains(asked, "checkSuite { app { databaseId } }") {
		t.Errorf("with App=7 the queue asks %q, want checkSuite { app { databaseId } }", asked)
	}
	if asked := (Names{}).contextFields(); strings.Contains(asked, "checkSuite") {
		t.Errorf("with no App the queue asks %q, want nothing of a check run's suite", asked)
	}
	var r rollup
	err := json.Unmarshal([]byte(`{"contexts": {"nodes": [
		{"__typename": "StatusContext", "context": "admit-status", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "admit-other", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "CheckRun", "name": "admit-shadowed", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "admit-shadowed", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "admit-shadowed", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:02:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "CheckRun", "name": "admit-app", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "CheckRun", "name": "admit-app", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "admit-app", "state": "FAILURE", "createdAt": "2026-11-14T12:02:00Z"},
		{"__typename": "CheckRun", "name": "merge-queue/build", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "merge-queue/build", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "merge-queue/build", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:02:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "StatusContext", "context": "merge-queue/probe", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "merge-queue/probe", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:02:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "CheckRun", "name": "changes", "status": "IN_PROGRESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "StatusContext", "context": "changes", "state": "SUCCESS", "createdAt": "2026-11-14T12:02:00Z"},
		{"__typename": "StatusContext", "context": "merge-queue", "state": "PENDING", "description": "candidate of #1", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "StatusContext", "context": "linux (ci)", "state": "FAILURE", "createdAt": "2026-11-14T12:01:00Z"}
	]}}`), &r)
	if err != nil {
		t.Fatal(err)
	}

	n := Names{
		Status: "merge-queue",
		Admit:  []string{"admit-status", "admit-other", "admit-shadowed", "admit-app"},
		App:    7,
	}
	st := r.statuses(n)
	for _, absent := range []string{"admit-status", "admit-other", "merge-queue/probe", "changes", "linux (ci)"} {
		if got, ok := st[absent]; ok {
			t.Errorf("with App=7, %s was read as %+v, want unread", absent, got)
		}
	}
	if got := st["admit-shadowed"].State; got != queue.Failure {
		t.Errorf("admit-shadowed = %v, want failure from App 7 unshadowed by newer status or App 8", got)
	}
	if got := st["admit-app"].State; got != queue.Success {
		t.Errorf("admit-app = %v, want success from App 7 unshadowed by newer status", got)
	}
	if got := st["merge-queue/build"].State; got != queue.Failure {
		t.Errorf("merge-queue/build = %v, want failure from App 7", got)
	}
	if got := st["merge-queue"]; got.State != queue.Pending || got.Description != "candidate of #1" {
		t.Errorf("the queue's own status = %+v, want pending %q", got, "candidate of #1")
	}

	n.App = 0
	st0 := r.statuses(n)
	for name, want := range map[string]queue.Verdict{
		"admit-status":      queue.Success,
		"admit-other":       queue.Success,
		"admit-shadowed":    queue.Success,
		"admit-app":         queue.Failure,
		"merge-queue/build": queue.Success,
		"merge-queue/probe": queue.Success,
		"changes":           queue.Success,
		"linux (ci)":        queue.Failure,
	} {
		if got := st0[name].State; got != want {
			t.Errorf("with App=0, %s = %v, want %v", name, got, want)
		}
	}
}

func TestOnlyTheConfiguredAppsCheckRunsCountForUnnamedContexts(t *testing.T) {
	var r rollup
	err := json.Unmarshal([]byte(`{"contexts": {"nodes": [
		{"__typename": "CheckRun", "name": "unit", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "unit", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "StatusContext", "context": "lint", "state": "FAILURE", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "cover", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": {"databaseId": 8}}}
	]}}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	st := r.statuses(Names{Status: "merge-queue", App: 7})
	if got := st["unit"].State; got != queue.Failure {
		t.Errorf("unit = %v, want failure from App 7 unshadowed by newer commit status", got)
	}
	for _, absent := range []string{"lint", "cover"} {
		if got, ok := st[absent]; ok {
			t.Errorf("with App=7, %s was read as %+v, want unread", absent, got)
		}
	}
}

func TestTheQueuesOwnStatusIsReadOnlyFromACommitStatus(t *testing.T) {
	var r rollup
	err := json.Unmarshal([]byte(`{"contexts": {"nodes": [
		{"__typename": "StatusContext", "context": "merge-queue", "state": "PENDING", "description": "in the queue, 0 ahead of it", "createdAt": "2026-11-14T12:00:00Z"},
		{"__typename": "CheckRun", "name": "merge-queue", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": {"databaseId": 7}}}
	]}}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []int{0, 7} {
		got := r.statuses(Names{Status: "merge-queue", App: app})["merge-queue"]
		if got.State != queue.Pending || got.Description != "in the queue, 0 ahead of it" {
			t.Errorf("with App=%d, merge-queue = %+v, want pending %q", app, got, "in the queue, 0 ahead of it")
		}
	}
}

func TestBeginsAndNullAppCheckRunWithConfiguredApp(t *testing.T) {
	var r rollup
	err := json.Unmarshal([]byte(`{"contexts": {"nodes": [
		{"__typename": "CheckRun", "name": "changes", "status": "IN_PROGRESS", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "changes", "state": "FAILURE", "createdAt": "2026-11-14T12:01:00Z"},
		{"__typename": "CheckRun", "name": "changes", "status": "QUEUED", "startedAt": "2026-11-14T12:02:00Z", "checkSuite": {"app": {"databaseId": 8}}},
		{"__typename": "CheckRun", "name": "merge-queue/build", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": null}},
		{"__typename": "CheckRun", "name": "signoff", "status": "COMPLETED", "conclusion": "SUCCESS", "startedAt": "2026-11-14T12:01:00Z", "checkSuite": {"app": null}}
	]}}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	st := r.statuses(Names{Status: "merge-queue", Admit: []string{"signoff"}, App: 7})
	if got := st["changes"]; !got.Running || got.State != queue.Pending {
		t.Errorf("changes = %+v, want in-progress check run from App 7 unshadowed by newer status or App 8", got)
	}
	for _, absent := range []string{"merge-queue/build", "signoff"} {
		if got, ok := st[absent]; ok {
			t.Errorf("with App=7 and null app, %s was read as %+v, want unread", absent, got)
		}
	}
}

func TestAContextWithNoNameAnswersForNothing(t *testing.T) {
	var r rollup
	err := json.Unmarshal([]byte(`{"contexts": {"nodes": [
		null,
		{"__typename": "CheckRun", "name": "", "status": "COMPLETED", "conclusion": "FAILURE", "startedAt": "2026-11-14T12:00:00Z", "checkSuite": {"app": {"databaseId": 7}}},
		{"__typename": "StatusContext", "context": "", "state": "SUCCESS", "createdAt": "2026-11-14T12:01:00Z"}
	]}}`), &r)
	if err != nil {
		t.Fatal(err)
	}
	for _, names := range []Names{{}, {Status: "merge-queue"}, {Status: "merge-queue", App: 7}} {
		if got := r.statuses(names); len(got) != 0 {
			t.Errorf("with %+v, read %+v, want nothing", names, got)
		}
	}
}
