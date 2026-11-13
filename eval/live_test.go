package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		{World: queue.World{Now: at(61), Trunk: "m2b"}},
	}
	dir := t.TempDir()
	for i, rec := range recs {
		data, _ := json.Marshal(rec)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.json", i)), data, 0o644)
	}
	sc, err := live(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A whole build took 10 minutes, so an hour to land is 6 builds and half an hour to an ejection is 3.
	want := Score{Shape: filepath.Base(dir), Config: "live", Landed: 2, Wrong: 500, WaitP50: 6, WaitP90: 6, EjectP90: 3, Builds: 1, PerHour: 2 / (61.0 / 60),
		Good: 3, Flaked: 1}
	if fmt.Sprintf("%.3v", sc) != fmt.Sprintf("%.3v", want) {
		t.Errorf("scored\n %+v\nwant\n %+v", sc, want)
	}
}
