package github

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/basecamp/banyan/internal/queue"
)

func TestOnlyTheConfiguredAppsCheckRunsCountForNamedContexts(t *testing.T) {
	if !strings.Contains(contextFields, "checkSuite { app { databaseId } }") {
		t.Errorf("contextFields = %q, want checkSuite { app { databaseId } }", contextFields)
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
		Build:  []string{"merge-queue/build"},
		Probe:  "merge-queue/probe",
		Admit:  []string{"admit-status", "admit-other", "admit-shadowed", "admit-app"},
		Begins: "changes",
		App:    7,
	}
	st := r.statuses(n)
	for _, absent := range []string{"admit-status", "admit-other", "merge-queue/probe", "changes"} {
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
	if got := st["linux (ci)"].State; got != queue.Failure {
		t.Errorf("unnamed context linux (ci) = %v, want failure", got)
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
	} {
		if got := st0[name].State; got != want {
			t.Errorf("with App=0, %s = %v, want %v", name, got, want)
		}
	}
}
