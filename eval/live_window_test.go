package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

func windowRow(t *testing.T, recs []queue.Recording, from, to time.Time) liveRow {
	t.Helper()
	_, _, row, err := scoreLive(writeLive(t, recs), "", liveOptions{from: &from, to: &to, row: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.SplitLandings+row.UnsplitLandings != row.Landed {
		t.Fatalf("split population: %+v", row)
	}
	return row
}

func TestLiveAttributesOutcomesUsingContext(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	pr := queue.PR{Number: 1, Head: "h", Queued: at(-20), Checks: queue.Pending}
	b := queue.Build{SHA: "b", Pushed: at(-5), Chain: []queue.Member{{Number: 1, Head: "h", Merge: "b"}}}
	recs := []queue.Recording{{World: queue.World{Now: at(-20), Trunk: "t", PRs: []queue.PR{pr}}}}
	pr.Checks = queue.Success
	recs = append(recs,
		queue.Recording{World: queue.World{Now: at(-10), Trunk: "t", PRs: []queue.PR{pr}}},
		queue.Recording{World: queue.World{Now: at(-5), Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}})
	b.Verdict = queue.Success
	recs = append(recs, queue.Recording{World: queue.World{Now: at(5), Trunk: "t", PRs: []queue.PR{pr}, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}, {Do: "eject", PR: 1, Cause: "culprit"}}})
	b.Landed = at(10)
	recs = append(recs, queue.Recording{World: queue.World{Now: at(11), Trunk: "b", Builds: []queue.Build{b}}})
	first := windowRow(t, recs, at(0), at(10))
	second := windowRow(t, recs, at(10), at(20))
	if first.Landed != 1 || first.SplitLandings != 1 || *first.WaitP50Minutes != 15 || *first.QueueAddedP50Minutes != 5 || *first.LandingBuildMedianMinutes != 10 || first.BuildMedianMinutes == nil || *first.BuildMedianMinutes != 10 || first.EjectionsAsCause != 1 {
		t.Fatalf("first: %+v", first)
	}
	if second.Landed != 0 || second.SplitLandings != 0 || second.WaitP50Minutes != nil || second.QueueAddedP50Minutes != nil || second.LandingBuildMedianMinutes != nil || second.BuildMedianMinutes != nil || second.EjectionsAsCause != 0 {
		t.Fatalf("second: %+v", second)
	}
	if first.Ticks != 1 || first.BusyMinutes != 10 || second.Ticks != 1 || second.BusyMinutes != 1 {
		t.Fatalf("ticks: %+v / %+v", first, second)
	}
	if first.LandingsPerHour == nil || *first.LandingsPerHour != 6 || second.LandingsPerHour == nil || *second.LandingsPerHour != 0 {
		t.Fatalf("landings per hour: %v / %v", first.LandingsPerHour, second.LandingsPerHour)
	}
	data, _ := json.Marshal(second)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if fields["context_ticks"] != float64(4) {
		t.Fatalf("context: %s", data)
	}
}

func TestLiveSortsContextByWorldTime(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dir := writeLive(t, []queue.Recording{
		{World: queue.World{Now: start.Add(time.Minute), PRs: []queue.PR{{Number: 1}}}},
		{World: queue.World{Now: start, PRs: []queue.PR{{Number: 1}}}},
	})
	end := start.Add(2 * time.Minute)
	_, _, row, err := scoreLive(dir, "", liveOptions{from: &start, to: &end, row: true})
	if err != nil || !row.FirstTick.Equal(start) || !row.LastTick.Equal(start.Add(time.Minute)) || row.BusyMinutes != 1 {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestLiveKeepsReadyReadFromThreeDayContext(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	pr := queue.PR{Number: 1, Head: "h", Queued: start.Add(-96 * time.Hour), Checks: queue.Pending}
	b := queue.Build{SHA: "b", Pushed: start, Chain: []queue.Member{{Number: 1, Head: "h"}}}
	recs := []queue.Recording{{World: queue.World{Now: start.Add(-48 * time.Hour), Trunk: "t", PRs: []queue.PR{pr}}}}
	pr.Checks = queue.Success
	recs = append(recs,
		queue.Recording{World: queue.World{Now: start.Add(-30 * time.Hour), Trunk: "t", PRs: []queue.PR{pr}}},
		queue.Recording{World: queue.World{Now: start, Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}})
	b.Verdict = queue.Success
	recs = append(recs,
		queue.Recording{World: queue.World{Now: start.Add(5 * time.Minute), Trunk: "t", PRs: []queue.PR{pr}, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}}},
		queue.Recording{World: queue.World{Now: start.Add(6 * time.Minute), Trunk: "b"}})
	row := windowRow(t, recs, start, start.Add(time.Hour))
	if row.ContextTicks != 2 || row.Landed != 1 || row.WaitP50Minutes == nil || *row.WaitP50Minutes != 1805 || row.QueueAddedP90Minutes == nil || *row.QueueAddedP90Minutes != 1800 {
		t.Fatalf("ready clock: %+v", row)
	}
}

func TestLiveClipsBusyIntervalsToWindow(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	for _, test := range []struct {
		name    string
		minutes []int
		opts    liveOptions
		busy    float64
	}{
		{"both bounds", []int{-5, 5, 15}, liveOptions{from: &start, to: &end, row: true}, 10},
		{"whole window between ticks", []int{-5, 15}, liveOptions{from: &start, to: &end, row: true}, 10},
		{"from only", []int{-10, -5, 5}, liveOptions{from: &start, row: true}, 5},
		{"to only", []int{5, 15, 20}, liveOptions{to: &end, row: true}, 5},
		{"unwindowed", []int{-5, 5, 15}, liveOptions{row: true}, 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			var recs []queue.Recording
			for _, minute := range test.minutes {
				recs = append(recs, queue.Recording{World: queue.World{Now: start.Add(time.Duration(minute) * time.Minute), PRs: []queue.PR{{Number: 1}}}})
			}
			_, _, row, err := scoreLive(writeLive(t, recs), "", test.opts)
			if err != nil || row.BusyMinutes != test.busy {
				t.Fatalf("busy minutes=%g, want %g; err=%v", row.BusyMinutes, test.busy, err)
			}
		})
	}
}

func TestLiveContextBoundsSkipOnlyOutsideFiles(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 500000000, time.UTC)
	end := start.Add(time.Hour)
	dir := t.TempDir()
	times := []time.Time{start.Add(-72*time.Hour - time.Nanosecond), start.Add(-72 * time.Hour), start, end, end.Add(time.Hour - time.Nanosecond), end.Add(time.Hour)}
	for i, at := range times {
		data, _ := json.Marshal(queue.Recording{World: queue.World{Now: at}})
		name := at.Format("20060102T150405Z") + "-" + string(rune('a'+i)) + ".json"
		os.WriteFile(filepath.Join(dir, name), data, 0600)
	}
	os.WriteFile(filepath.Join(dir, "20261115T115959Z-bad.json"), []byte("bad"), 0600)
	os.WriteFile(filepath.Join(dir, "20261118T140001Z-bad.json"), []byte("bad"), 0600)
	_, _, row, err := scoreLive(dir, "", liveOptions{from: &start, to: &end, row: true})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(row)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	if row.Ticks != 1 || fields["context_ticks"] != float64(3) {
		t.Fatalf("row: %s", data)
	}
}

func TestLiveRedFirstReadAndMissingFailure(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	red := queue.Build{SHA: "old", Verdict: queue.Failure, Failed: []string{"job"}}
	late := queue.Build{SHA: "late", Verdict: queue.Failure}
	recs := []queue.Recording{
		{World: queue.World{Now: start.Add(-time.Minute), Builds: []queue.Build{red}}},
		{World: queue.World{Now: start, Builds: []queue.Build{red, late}}},
	}
	late.Failed = []string{"job"}
	recs = append(recs, queue.Recording{World: queue.World{Now: start.Add(time.Minute), Builds: []queue.Build{late}}})
	row := windowRow(t, recs, start, start.Add(time.Hour))
	if row.RedCandidates != 1 || row.RedReadN != 0 || row.RedReadMedianMinutes != nil {
		t.Fatalf("reds: %+v", row.liveMeasures)
	}
}

func TestLiveSplitUsesQueueingReadyClock(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	for _, state := range []string{"requeued", "held", "pending", "first ready"} {
		t.Run(state, func(t *testing.T) {
			pr := queue.PR{Number: 1, Head: "h", Queued: at(-10), Checks: queue.Success}
			b := queue.Build{SHA: "b", Pushed: at(30), Chain: []queue.Member{{Number: 1, Head: "h", Merge: "b"}}}
			recs := []queue.Recording{{World: queue.World{Now: start, Trunk: "t", PRs: []queue.PR{pr}}}}
			if state == "requeued" {
				pr.Queued = at(10)
				pr.Wait = "hold"
				recs = append(recs, queue.Recording{World: queue.World{Now: at(10), Trunk: "t", PRs: []queue.PR{pr}}})
				pr.Wait = ""
				recs = append(recs, queue.Recording{World: queue.World{Now: at(20), Trunk: "t", PRs: []queue.PR{pr}}})
			}
			if state == "held" {
				pr.Wait = "hold"
				recs[0].World.PRs = []queue.PR{pr}
			}
			if state == "pending" {
				pr.Checks = queue.Pending
				recs[0].World.PRs = []queue.PR{pr}
			}
			recs = append(recs, queue.Recording{World: queue.World{Now: at(30), Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}})
			b.Verdict = queue.Success
			recs = append(recs, queue.Recording{World: queue.World{Now: at(40), Trunk: "t", PRs: []queue.PR{pr}, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}}})
			b.Landed = at(46)
			recs = append(recs, queue.Recording{World: queue.World{Now: at(50), Trunk: "b", Builds: []queue.Build{b}}})
			row := windowRow(t, recs, start, at(60))
			if state == "held" || state == "pending" {
				if row.UnsplitLandings != 1 || row.QueueAddedP50Minutes != nil {
					t.Fatalf("unready split: %+v", row.liveMeasures)
				}
			} else {
				added, wait := 40.0, 50.0
				if state == "requeued" {
					added, wait = 10, 20
				}
				if row.SplitLandings != 1 || *row.QueueAddedP50Minutes != added || *row.WaitP50Minutes != wait || *row.SplitReadyToLandedP50Minutes != wait {
					t.Fatalf("ready split: %+v", row)
				}
			}
		})
	}
}

func TestLiveSplitCountsConfirmedLandingsOnce(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	pr := queue.PR{Number: 1, Head: "h", Queued: start, Checks: queue.Success}
	b := queue.Build{SHA: "b", Pushed: start, Chain: []queue.Member{{Number: 1, Head: "h", Merge: "b"}}}
	recs := []queue.Recording{{World: queue.World{Now: start, Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}}}
	b.Verdict = queue.Success
	for _, minute := range []int{10, 11} {
		recs = append(recs, queue.Recording{World: queue.World{Now: at(minute), Trunk: "t", PRs: []queue.PR{pr}, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}, {Do: "land", SHA: "refused", Members: b.Chain}}})
	}
	b.Landed = at(12)
	recs = append(recs, queue.Recording{World: queue.World{Now: at(13), Trunk: "b", Builds: []queue.Build{b}}})
	row := windowRow(t, recs, start, at(60))
	if row.Landed != 1 || row.SplitLandings != 1 || *row.SplitReadyToLandedP50Minutes != 10 {
		t.Fatalf("population: %+v", row)
	}
}

func TestLiveSplitUsesLandingWhenVerdictIsLaterOrAbsent(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, verdict := range []queue.Verdict{queue.Pending, queue.Success} {
		pr := queue.PR{Number: 1, Head: "h", Queued: start, Checks: queue.Success}
		b := queue.Build{SHA: "b", Pushed: start, Chain: []queue.Member{{Number: 1, Head: "h", Merge: "b"}}}
		recs := []queue.Recording{
			{World: queue.World{Now: start, Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}},
			{World: queue.World{Now: start.Add(9 * time.Minute), Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}}},
		}
		b.Landed, b.Verdict = start.Add(10*time.Minute), verdict
		recs = append(recs, queue.Recording{World: queue.World{Now: start.Add(11 * time.Minute), Trunk: "b", Builds: []queue.Build{b}}})
		row := windowRow(t, recs, start, start.Add(time.Hour))
		if row.SplitLandings != 1 || *row.LandingBuildMedianMinutes != 9 || *row.QueueAddedP50Minutes != 0 {
			t.Fatalf("verdict fallback: %+v", row.liveMeasures)
		}
	}
}

func TestLiveRedUsesFractionalReadTimeAtFrom(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 500000000, time.UTC)
	b := queue.Build{SHA: "b", Failed: []string{"job"}}
	recs := []queue.Recording{{World: queue.World{Now: start.Add(-time.Minute), Builds: []queue.Build{b}}}}
	b.Verdict = queue.Failure
	recs = append(recs, queue.Recording{World: queue.World{Now: start, Builds: []queue.Build{b}}})
	row := windowRow(t, recs, start, start.Add(time.Minute))
	if row.RedCandidates != 1 || row.RedReadN != 1 || *row.RedReadMedianMinutes != 1 {
		t.Fatalf("fractional read: %+v", row.liveMeasures)
	}
}

func TestLiveGapBelongsToEndingLandingWindow(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recs := []queue.Recording{
		{World: queue.World{Now: start.Add(-time.Minute), Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start, Trunk: "a", PRs: []queue.PR{{Number: 1, Checks: queue.Success}}}},
		{World: queue.World{Now: start.Add(10 * time.Minute), Trunk: "a"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 2}}}}},
		{World: queue.World{Now: start.Add(11 * time.Minute), Trunk: "b"}},
	}
	first := windowRow(t, recs, start, start.Add(10*time.Minute))
	second := windowRow(t, recs, start.Add(10*time.Minute), start.Add(20*time.Minute))
	if first.ReadyGaps != 0 || second.ReadyGaps != 1 || *second.LongestReadyGapMinutes != 11 {
		t.Fatalf("gaps: %+v / %+v", first.liveMeasures, second.liveMeasures)
	}
}

func TestLiveWaitAfterAnEmptyQueueStartsAtTheFirstReadyTick(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	ready := []queue.PR{{Number: 1, Queued: start, Checks: queue.Success, Note: "in the queue, waiting"}}
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "t", PRs: ready}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start.Add(time.Minute), Trunk: "a"}},
		{World: queue.World{Now: start.Add(50 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(100 * time.Minute), Trunk: "a", PRs: ready}, Actions: []queue.Action{{Do: "land", SHA: "c", Members: []queue.Member{{Number: 2}}}}},
		{World: queue.World{Now: start.Add(101 * time.Minute), Trunk: "c"}},
	}
	row := windowRow(t, recs, start, start.Add(102*time.Minute))
	if row.ReadyGaps != 1 || *row.LongestReadyGapMinutes != 50 || !row.LongestReadyGapFrom.Equal(start.Add(50*time.Minute)) || row.ReadyGapsOver90 != 0 {
		t.Fatalf("wait after an empty queue: %+v", row.liveMeasures)
	}
}

func TestLiveWaitOutlastsTicksThatWereNotRecorded(t *testing.T) {
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	at := func(h, m, s int) time.Time {
		return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
	}
	ready := []queue.PR{{Number: 2, Queued: at(9, 0, 0), Checks: queue.Success}}
	held := []queue.PR{{Number: 2, Queued: at(9, 0, 0), Checks: queue.Success, Wait: "hold"}}
	recs := []queue.Recording{
		{World: queue.World{Now: at(9, 38, 45), Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: at(9, 40, 0), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: at(9, 42, 0), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: at(11, 33, 0), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: at(12, 6, 50), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: at(12, 12, 10), Trunk: "a", PRs: held}},
		{World: queue.World{Now: at(13, 0, 0), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: at(13, 30, 0), Trunk: "a", PRs: held}},
		{World: queue.World{Now: at(14, 25, 25), Trunk: "a"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 3}}}}},
		{World: queue.World{Now: at(14, 26, 0), Trunk: "b"}},
	}
	row := windowRow(t, recs, at(1, 17, 0), at(21, 17, 0))
	if row.ReadyGaps != 1 || row.ReadyGapsOver90 != 1 || *row.LongestReadyGapMinutes != at(12, 6, 50).Sub(at(9, 38, 45)).Minutes() ||
		!row.LongestReadyGapFrom.Equal(at(9, 38, 45)) || !row.LongestReadyGapTo.Equal(at(12, 6, 50)) {
		t.Fatalf("wait across unrecorded ticks: %+v", row.liveMeasures)
	}
}

func TestLiveWaitStartsWhenAHeadTurnsReadyAndEndsAtTheLanding(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	pending := []queue.PR{{Number: 2, Queued: start, Checks: queue.Pending}}
	ready := []queue.PR{{Number: 2, Queued: start, Checks: queue.Success}}
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start.Add(10 * time.Minute), Trunk: "a", PRs: pending}},
		{World: queue.World{Now: start.Add(30 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(60 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(70 * time.Minute), Trunk: "a"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 2}}}}},
		{World: queue.World{Now: start.Add(71 * time.Minute), Trunk: "b"}},
	}
	row := windowRow(t, recs, start, start.Add(72*time.Minute))
	if row.ReadyGaps != 1 || *row.LongestReadyGapMinutes != 40 || !row.LongestReadyGapFrom.Equal(start.Add(30*time.Minute)) || !row.LongestReadyGapTo.Equal(start.Add(70*time.Minute)) {
		t.Fatalf("wait of a head that turned ready: %+v", row.liveMeasures)
	}
}

func TestLiveCountsEachWaitBetweenTwoLandings(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	ready := []queue.PR{{Number: 2, Queued: start, Checks: queue.Success}}
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start.Add(5 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(20 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(25 * time.Minute), Trunk: "a"}},
		{World: queue.World{Now: start.Add(40 * time.Minute), Trunk: "a", PRs: ready}},
		{World: queue.World{Now: start.Add(80 * time.Minute), Trunk: "a"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 2}}}}},
		{World: queue.World{Now: start.Add(81 * time.Minute), Trunk: "b"}},
	}
	row := windowRow(t, recs, start, start.Add(82*time.Minute))
	if row.ReadyGaps != 2 || *row.LongestReadyGapMinutes != 40 || !row.LongestReadyGapFrom.Equal(start.Add(40*time.Minute)) {
		t.Fatalf("two waits between landings: %+v", row.liveMeasures)
	}
}

func TestLiveNoGapWhileTheOnlyHeadConflictsWithTheTrunk(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	conflicting := []queue.PR{{Number: 3, Queued: start, Checks: queue.Success, Note: "waits: it conflicts with the trunk"}}
	behind := []queue.PR{{Number: 3, Queued: start, Checks: queue.Success, Note: "waits: it conflicts with a pull request ahead of it"}}
	day := func(between []queue.PR) []queue.Recording {
		return []queue.Recording{
			{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
			{World: queue.World{Now: start.Add(time.Minute), Trunk: "a", PRs: between}},
			{World: queue.World{Now: start.Add(100 * time.Minute), Trunk: "a", PRs: between}, Actions: []queue.Action{{Do: "land", SHA: "c", Members: []queue.Member{{Number: 2}}}}},
			{World: queue.World{Now: start.Add(101 * time.Minute), Trunk: "c"}},
		}
	}
	if row := windowRow(t, day(conflicting), start, start.Add(102*time.Minute)); row.ReadyGaps != 0 || row.ReadyGapsOver90 != 0 {
		t.Fatalf("a head that conflicts with the trunk made a gap: %+v", row.liveMeasures)
	}
	if row := windowRow(t, day(behind), start, start.Add(102*time.Minute)); row.ReadyGaps != 1 || row.ReadyGapsOver90 != 1 || *row.LongestReadyGapMinutes != 100 {
		t.Fatalf("a head waiting on one ahead of it is waiting on the queue: %+v", row.liveMeasures)
	}
}

func TestLiveCountsTrunkMovesNoRecordedPlanLanded(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start.Add(time.Minute), Trunk: "a"}},
		{World: queue.World{Now: start.Add(2 * time.Minute), Trunk: "merged-by-hand"}},
		{World: queue.World{Now: start.Add(3 * time.Minute), Trunk: "merged-by-hand"}},
	}
	row := windowRow(t, recs, start, start.Add(4*time.Minute))
	if row.Landed != 1 || row.UnplannedTrunkMoves != 1 {
		t.Fatalf("landed %d, unplanned trunk moves %d", row.Landed, row.UnplannedTrunkMoves)
	}
	if later := windowRow(t, recs, start.Add(3*time.Minute), start.Add(4*time.Minute)); later.UnplannedTrunkMoves != 0 {
		t.Fatalf("a move before the window counted in it: %d", later.UnplannedTrunkMoves)
	}
}

func TestLiveGapsIgnoreMarksOnLowerBuilds(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "a", Members: []queue.Member{{Number: 1}}}}},
		{World: queue.World{Now: start.Add(50 * time.Minute), Trunk: "a", PRs: []queue.PR{{Number: 1, Checks: queue.Success}}, Builds: []queue.Build{{SHA: "lower", Landed: start.Add(40 * time.Minute)}}}},
		{World: queue.World{Now: start.Add(100 * time.Minute), Trunk: "a"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: []queue.Member{{Number: 2}}}}},
		{World: queue.World{Now: start.Add(101 * time.Minute), Trunk: "b"}},
	}
	row := windowRow(t, recs, start, start.Add(102*time.Minute))
	if row.ReadyGaps != 1 || row.LongestReadyGapMinutes == nil || *row.LongestReadyGapMinutes != 100 {
		t.Fatalf("gap split by lower build: %+v", row.liveMeasures)
	}
}

func TestLiveFullWindowMatchesUnwindowed(t *testing.T) {
	start := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprint(keep), func(t *testing.T) {
			var recs []queue.Recording
			trunk := queue.SHA("t")
			for i := range 24 {
				at := start.Add(time.Duration(i) * time.Hour)
				pr := queue.PR{Number: i + 1, Head: queue.SHA(fmt.Sprintf("h%d", i)), Queued: at.Add(-5 * time.Minute), Checks: queue.Success}
				b := queue.Build{SHA: queue.SHA(fmt.Sprintf("b%d", i)), Pushed: at, Chain: []queue.Member{{Number: pr.Number, Head: pr.Head}}}
				probe := queue.Build{SHA: queue.SHA(fmt.Sprintf("p%d", i)), Kind: queue.Probe, Pushed: at}
				recs = append(recs, queue.Recording{World: queue.World{Now: at, Trunk: trunk, PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}, {Do: "push", Build: probe}}})
				b.Verdict, probe.Verdict = queue.Success, queue.Success
				for _, minute := range []int{10, 11} {
					recs = append(recs, queue.Recording{World: queue.World{Now: at.Add(time.Duration(minute) * time.Minute), Trunk: trunk, PRs: []queue.PR{pr}, Builds: []queue.Build{b, probe}}, Actions: []queue.Action{{Do: "land", SHA: b.SHA, Members: b.Chain}, {Do: "land", SHA: "refused", Members: b.Chain}}})
				}
				if keep {
					b.Landed = at.Add(11 * time.Minute)
					b.Pushed = b.Landed
				}
				trunk = b.SHA
				recs = append(recs, queue.Recording{World: queue.World{Now: at.Add(12 * time.Minute), Trunk: trunk, Builds: []queue.Build{b}}})
			}
			last := &recs[len(recs)-1].World
			last.PRs = []queue.PR{{Number: 25, Head: "last", Queued: last.Now, Checks: queue.Success}}
			dir := writeLive(t, recs)
			var rows []liveRow
			var scores []Score
			var windows []window
			for _, opts := range []liveOptions{{row: true}, {from: &start, to: &end, row: true}} {
				score, win, row, err := scoreLive(dir, "", opts)
				if err != nil {
					t.Fatal(err)
				}
				if row.Landed != 24 || row.SplitLandings != 24 || row.UnsplitLandings != 0 || row.WaitP50Minutes == nil || *row.WaitP50Minutes != 15 || row.WaitP90Minutes == nil || *row.WaitP90Minutes != 15 || row.BuildsPerLanding == nil || *row.BuildsPerLanding != 2 {
					t.Fatalf("day: %+v", row)
				}
				scores, rows = append(scores, score), append(rows, row)
				windows = append(windows, win)
			}
			if len(windows[0].arrivals) != 25 || !reflect.DeepEqual(windows[0].arrivals, windows[1].arrivals) || windows[0].build != windows[1].build || windows[0].cfg != windows[1].cfg {
				t.Fatalf("full window arrivals differ: %+v / %+v", windows[0], windows[1])
			}
			if !reflect.DeepEqual(scores[0], scores[1]) || !reflect.DeepEqual(rows[0].WaitP50, rows[1].WaitP50) || !reflect.DeepEqual(rows[0].WaitP90, rows[1].WaitP90) || !reflect.DeepEqual(rows[0].liveMeasures, rows[1].liveMeasures) {
				t.Fatalf("full window differs: %+v / %+v; %+v / %+v", scores[0], scores[1], rows[0], rows[1])
			}
		})
	}
}

func TestLivePlanBelongsToItsWindowWhenConfirmedAfterTo(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	pr := queue.PR{Number: 1, Head: "h", Queued: start, Checks: queue.Success}
	b := queue.Build{SHA: "b", Pushed: start, Chain: []queue.Member{{Number: 1, Head: "h"}}}
	recs := []queue.Recording{{World: queue.World{Now: start, Trunk: "t", PRs: []queue.PR{pr}}, Actions: []queue.Action{{Do: "push", Build: b}}}}
	b.Verdict = queue.Success
	for _, minute := range []int{9, 10} {
		recs = append(recs, queue.Recording{World: queue.World{Now: start.Add(time.Duration(minute) * time.Minute), Trunk: "t", PRs: []queue.PR{pr}, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: b.Chain}}})
	}
	recs = append(recs, queue.Recording{World: queue.World{Now: end.Add(30 * time.Minute), Trunk: "b"}})
	first := windowRow(t, recs, start, end)
	second := windowRow(t, recs, end, end.Add(time.Hour))
	if first.Landed != 1 || first.SplitLandings != 1 || first.UnsplitLandings != 0 || *first.WaitP50Minutes != 9 || *first.LabelledP50Minutes != 9 || *first.SplitReadyToLandedP50Minutes != 9 || second.Landed != 0 || second.SplitLandings != 0 || second.UnsplitLandings != 0 {
		t.Fatalf("landing windows: %+v / %+v", first, second)
	}
}

func TestLiveCountsUnconfirmedLandingPlans(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	members := []queue.Member{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2"}}
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			recs := []queue.Recording{
				{World: queue.World{Now: start.Add(-time.Minute), Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "before", Members: members}}},
				{World: queue.World{Now: start.Add(time.Minute), Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: members}}},
				{World: queue.World{Now: start.Add(2 * time.Minute), Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "b", Members: members}}},
				{World: queue.World{Now: end, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "after", Members: members}}},
			}
			wantPlans, wantLanded := 1.0, 0
			if confirmed {
				recs = append(recs, queue.Recording{World: queue.World{Now: end.Add(30 * time.Minute), Trunk: "b"}})
				wantPlans, wantLanded = 0, len(members)
			}
			row := windowRow(t, recs, start, end)
			data, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["unconfirmed_landing_plans"] != wantPlans || row.Landed != wantLanded {
				t.Fatalf("unconfirmed plans=%v landed=%d, want %g and %d", fields["unconfirmed_landing_plans"], row.Landed, wantPlans, wantLanded)
			}
		})
	}
}

func TestLiveUnwindowedOmitsUnconfirmedLandingPlans(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dir := writeLive(t, []queue.Recording{{World: queue.World{Now: start, Trunk: "t"}, Actions: []queue.Action{{Do: "land", SHA: "unconfirmed"}}}})
	out, err := captureLiveRun(t, dir, "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["unconfirmed_landing_plans"]; ok {
		t.Fatalf("unwindowed row added a field: %s", out)
	}
}

func TestLiveMarksWithoutPlansDoNotCount(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	recs := []queue.Recording{
		{World: queue.World{Now: start, Trunk: "a", Builds: []queue.Build{{SHA: "a", Landed: start}}}},
		{World: queue.World{Now: start.Add(time.Minute), Trunk: "a", PRs: []queue.PR{{Number: 1, Checks: queue.Success}}}},
		{World: queue.World{Now: start.Add(10 * time.Minute), Trunk: "b", Builds: []queue.Build{{SHA: "b", Landed: start.Add(9 * time.Minute)}}}},
	}
	row := windowRow(t, recs, start, start.Add(time.Hour))
	if row.Landed != 0 || row.SplitLandings != 0 || row.UnsplitLandings != 0 || row.ReadyGaps != 0 || row.LongestReadyGapMinutes != nil {
		t.Fatalf("unplanned marks: %+v", row)
	}
}

func TestLiveCountsPassingCandidatesThrownAwayUnlanded(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	candidate := func(sha queue.SHA, verdict queue.Verdict, chain ...queue.Member) queue.Build {
		return queue.Build{SHA: sha, Kind: queue.Candidate, Verdict: verdict, Chain: chain}
	}
	one, two := queue.Member{Number: 1, Head: "h1", Merge: "m1"}, queue.Member{Number: 2, Head: "h2", Merge: "m2"}
	deleted := func(m int, prs []queue.PR, b queue.Build) queue.Recording {
		return queue.Recording{World: queue.World{Now: at(m), Trunk: "t", PRs: prs, Builds: []queue.Build{b}}, Actions: []queue.Action{{Do: "delete", SHA: b.SHA}}}
	}
	both := []queue.PR{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2"}}
	moved := []queue.PR{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2-pushed-again"}}
	for _, c := range []struct {
		name                    string
		recs                    []queue.Recording
		from, to                int
		thrown, headMoved, left int
	}{
		{"a member's head moved", []queue.Recording{deleted(1, moved, candidate("c", queue.Success, one, two))}, 0, 10, 1, 1, 0},
		{"a member left", []queue.Recording{deleted(1, both[:1], candidate("c", queue.Success, one, two))}, 0, 10, 1, 0, 1},
		{"one member moved and another left", []queue.Recording{deleted(1, moved[1:], candidate("c", queue.Success, one, two))}, 0, 10, 1, 1, 1},
		{"deleted twice, counted once", []queue.Recording{deleted(1, both[:1], candidate("c", queue.Success, one, two)), deleted(2, both[:1], candidate("c", queue.Success, one, two))}, 0, 10, 1, 0, 1},
		{"landed, then deleted", []queue.Recording{
			{World: queue.World{Now: at(1), Trunk: "t", PRs: both, Builds: []queue.Build{candidate("c", queue.Success, one, two)}}, Actions: []queue.Action{{Do: "land", SHA: "c", Members: []queue.Member{one, two}}}},
			deleted(2, nil, candidate("c", queue.Success, one, two))}, 0, 10, 0, 0, 0},
		{"landed after the window", []queue.Recording{
			deleted(1, both[:1], candidate("c", queue.Success, one, two)),
			{World: queue.World{Now: at(20), Trunk: "t", PRs: both, Builds: []queue.Build{candidate("c", queue.Success, one, two)}}, Actions: []queue.Action{{Do: "land", SHA: "c", Members: []queue.Member{one, two}}}}}, 0, 10, 0, 0, 0},
		{"failed", []queue.Recording{deleted(1, nil, candidate("c", queue.Failure, one, two))}, 0, 10, 0, 0, 0},
		{"pending", []queue.Recording{deleted(1, nil, candidate("c", queue.Pending, one, two))}, 0, 10, 0, 0, 0},
		{"passed with an empty chain", []queue.Recording{deleted(1, nil, candidate("c", queue.Success))}, 0, 10, 0, 0, 0},
		{"a passing check, not a candidate", []queue.Recording{deleted(1, nil, queue.Build{SHA: "c", Kind: queue.Check, Verdict: queue.Success, Chain: []queue.Member{one}})}, 0, 10, 0, 0, 0},
		{"deleted before the window", []queue.Recording{deleted(1, nil, candidate("c", queue.Success, one, two)), {World: queue.World{Now: at(5), Trunk: "t"}}}, 2, 10, 0, 0, 0},
		{"deleted at the window's end", []queue.Recording{{World: queue.World{Now: at(1), Trunk: "t"}}, deleted(10, nil, candidate("c", queue.Success, one, two))}, 0, 10, 0, 0, 0},
		{"a later entry for the same commit passed", []queue.Recording{{World: queue.World{Now: at(1), Trunk: "t", PRs: []queue.PR{{Number: 1, Head: "h1-pushed-again"}},
			Builds: []queue.Build{candidate("c", queue.Failure, one, two), candidate("c", queue.Success, one, two)}}, Actions: []queue.Action{{Do: "delete", SHA: "c"}}}}, 0, 10, 1, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := windowRow(t, c.recs, at(c.from), at(c.to)).liveMeasures
			if m.PassingCandidatesThrownAway != c.thrown || m.ThrownAwayHeadMoved != c.headMoved || m.ThrownAwayMemberLeft != c.left {
				t.Fatalf("thrown away %d, head moved %d, member left %d; want %d, %d, %d",
					m.PassingCandidatesThrownAway, m.ThrownAwayHeadMoved, m.ThrownAwayMemberLeft, c.thrown, c.headMoved, c.left)
			}
		})
	}
}

func TestLiveCountsCandidatesDroppedWhileBuildingForAMember(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	candidate := func(sha queue.SHA, verdict queue.Verdict, pushed int, chain ...queue.Member) queue.Build {
		return queue.Build{SHA: sha, Kind: queue.Candidate, Verdict: verdict, Pushed: at(pushed), Chain: chain}
	}
	one, two, three := queue.Member{Number: 1, Head: "h1"}, queue.Member{Number: 2, Head: "h2"}, queue.Member{Number: 3, Head: "h3"}
	deleted := func(m int, prs []queue.PR, builds ...queue.Build) queue.Recording {
		rec := queue.Recording{World: queue.World{Now: at(m), Trunk: "t", PRs: prs, Builds: builds}}
		for _, b := range builds {
			rec.Actions = append(rec.Actions, queue.Action{Do: "delete", SHA: b.SHA})
		}
		return rec
	}
	unpushed := func(b queue.Build) queue.Build {
		b.Pushed = time.Time{}
		return b
	}
	read := func(m int, prs []queue.PR, builds []queue.Build, actions ...queue.Action) queue.Recording {
		return queue.Recording{World: queue.World{Now: at(m), Trunk: "t", PRs: prs, Builds: builds}, Actions: actions}
	}
	all := []queue.PR{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2"}, {Number: 3, Head: "h3"}}
	moved := []queue.PR{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2-pushed-again"}, {Number: 3, Head: "h3"}}
	building := candidate("c", queue.Pending, 0, one, two)
	for _, c := range []struct {
		name                                  string
		recs                                  []queue.Recording
		from, to                              int
		dropped, headMoved, left, stillQueued int
		longest                               float64
	}{
		{"a member's head moved", []queue.Recording{deleted(22, moved, candidate("c", queue.Pending, 0, one, two))}, 0, 30, 1, 1, 0, 1, 22},
		{"a member left", []queue.Recording{deleted(9, all[:1], candidate("c", queue.Pending, 2, one, two))}, 0, 30, 1, 0, 1, 1, 7},
		{"stacked candidates count each member still queued once", []queue.Recording{deleted(22, moved,
			candidate("c", queue.Pending, 0, one, two), candidate("d", queue.Pending, 6, one, two, three))}, 0, 30, 2, 2, 0, 2, 22},
		{"every member still queued at its head", []queue.Recording{deleted(22, all, candidate("c", queue.Pending, 0, one, two))}, 0, 30, 0, 0, 0, 0, 0},
		{"passed before it was dropped", []queue.Recording{deleted(22, moved, candidate("c", queue.Success, 0, one, two))}, 0, 30, 0, 0, 0, 0, 0},
		{"failed", []queue.Recording{deleted(22, moved, candidate("c", queue.Failure, 0, one, two))}, 0, 30, 0, 0, 0, 0, 0},
		{"an empty chain", []queue.Recording{deleted(22, moved, candidate("c", queue.Pending, 0))}, 0, 30, 0, 0, 0, 0, 0},
		{"deleted twice, counted once", []queue.Recording{deleted(22, moved, candidate("c", queue.Pending, 0, one, two)), deleted(23, moved, candidate("c", queue.Pending, 0, one, two))}, 0, 30, 1, 1, 0, 1, 22},
		{"deleted before the window", []queue.Recording{deleted(1, moved, candidate("c", queue.Pending, 0, one, two)), {World: queue.World{Now: at(5), Trunk: "t"}}}, 2, 30, 0, 0, 0, 0, 0},
		{"a member the queue ejected as the culprit", []queue.Recording{
			read(10, all, []queue.Build{building}, queue.Action{Do: "eject", PR: 1, Cause: "culprit"}),
			deleted(12, all[1:], building)}, 0, 30, 0, 0, 0, 0, 0},
		{"a member that landed in another candidate", []queue.Recording{
			read(10, all, []queue.Build{candidate("d", queue.Success, 0, one), building}, queue.Action{Do: "land", SHA: "d", Members: []queue.Member{one}}),
			read(12, all[1:], nil),
			deleted(14, all[1:], building)}, 0, 30, 0, 0, 0, 0, 0},
		{"a member whose label was removed", []queue.Recording{
			read(10, all, []queue.Build{building}),
			deleted(12, all[1:], building)}, 0, 30, 1, 0, 1, 1, 12},
		{"a member ejected, queued again and gone again", []queue.Recording{
			read(10, all, []queue.Build{building}, queue.Action{Do: "eject", PR: 1, Cause: "culprit"}),
			read(12, all, []queue.Build{building}),
			deleted(14, all[1:], building)}, 0, 30, 1, 0, 1, 1, 14},
		{"a member ejected before the window", []queue.Recording{
			read(1, all, []queue.Build{building}, queue.Action{Do: "eject", PR: 1, Cause: "culprit"}),
			deleted(12, all[1:], building)}, 5, 30, 0, 0, 0, 0, 0},
		{"a push time read by an earlier record", []queue.Recording{
			read(5, all, []queue.Build{building}),
			deleted(20, moved, unpushed(building))}, 0, 30, 1, 1, 0, 1, 20},
		{"an ejection cleared by the pull request queued again at a new head", []queue.Recording{
			read(10, all, []queue.Build{building}, queue.Action{Do: "eject", PR: 1, Cause: "culprit"}),
			read(12, []queue.PR{{Number: 1, Head: "h1-pushed-again"}, {Number: 2, Head: "h2"}, {Number: 3, Head: "h3"}}, []queue.Build{building}),
			deleted(14, all[1:], building)}, 0, 30, 1, 0, 1, 1, 14},
		{"no push time, no age", []queue.Recording{deleted(22, moved, unpushed(building))}, 0, 30, 1, 1, 0, 1, 0},
		{"an age only from a push time", []queue.Recording{deleted(22, moved, unpushed(building), candidate("d", queue.Pending, 6, one, two, three))}, 0, 30, 2, 2, 0, 2, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := windowRow(t, c.recs, at(c.from), at(c.to)).liveMeasures
			got := [4]int{m.BuildingCandidatesDropped, m.DroppedHeadMoved, m.DroppedMemberLeft, m.DroppedMembersStillQueued}
			if want := [4]int{c.dropped, c.headMoved, c.left, c.stillQueued}; got != want || m.DroppedLongestBuiltMinutes != c.longest {
				t.Fatalf("dropped, head moved, member left, still queued %v, longest built %v min; want %v, %v", got, m.DroppedLongestBuiltMinutes, want, c.longest)
			}
		})
	}
}

func TestLiveCountsACandidateDroppedWhileBuildingThatPassedAndWasThrownAway(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	one, two := queue.Member{Number: 1, Head: "h1"}, queue.Member{Number: 2, Head: "h2"}
	moved := []queue.PR{{Number: 1, Head: "h1"}, {Number: 2, Head: "h2-pushed-again"}}
	c := queue.Build{SHA: "c", Kind: queue.Candidate, Verdict: queue.Pending, Pushed: at(0), Chain: []queue.Member{one, two}}
	recs := []queue.Recording{{World: queue.World{Now: at(10), Trunk: "t", PRs: moved, Builds: []queue.Build{c}}, Actions: []queue.Action{{Do: "delete", SHA: "c"}}}}
	c.Verdict = queue.Success
	recs = append(recs, queue.Recording{World: queue.World{Now: at(12), Trunk: "t", PRs: moved, Builds: []queue.Build{c}}, Actions: []queue.Action{{Do: "delete", SHA: "c"}}})
	m := windowRow(t, recs, at(0), at(30)).liveMeasures
	thrown := [3]int{m.PassingCandidatesThrownAway, m.ThrownAwayHeadMoved, m.ThrownAwayMemberLeft}
	dropped := [4]int{m.BuildingCandidatesDropped, m.DroppedHeadMoved, m.DroppedMemberLeft, m.DroppedMembersStillQueued}
	if thrown != [3]int{1, 1, 0} || dropped != [4]int{1, 1, 0, 1} || m.DroppedLongestBuiltMinutes != 10 {
		t.Fatalf("thrown away %v, dropped %v, longest built %v min", thrown, dropped, m.DroppedLongestBuiltMinutes)
	}
}

func TestLiveBusiestHourCountsEachMemberLandedInTheWindow(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	landings := func(members map[int]int) []queue.Recording {
		var recs []queue.Recording
		trunk := queue.SHA("t")
		for _, m := range slices.Sorted(maps.Keys(members)) {
			var landed []queue.Member
			for n := range members[m] {
				landed = append(landed, queue.Member{Number: 100*m + n})
			}
			sha := queue.SHA(fmt.Sprintf("l%d", m))
			recs = append(recs, queue.Recording{World: queue.World{Now: at(m), Trunk: trunk}, Actions: []queue.Action{{Do: "land", SHA: sha, Members: landed}}})
			trunk = sha
		}
		return append(recs, queue.Recording{World: queue.World{Now: at(125), Trunk: trunk}})
	}
	for _, c := range []struct {
		name            string
		members         map[int]int
		busiest, landed int
	}{
		{"the best 60 minutes is not a clock hour, and one exactly 60 minutes on is outside it", map[int]int{30: 1, 45: 3, 80: 1, 90: 1}, 5, 6},
		{"landings before and after the window do not count", map[int]int{-10: 2, 30: 1, 45: 3, 80: 1, 90: 1, 120: 4}, 5, 6},
		{"the busiest hour is not the first", map[int]int{0: 1, 70: 3}, 3, 4},
		{"no landing", map[int]int{}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := windowRow(t, landings(c.members), at(0), at(120))
			if row.BusiestHourLandings != c.busiest || row.Landed != c.landed {
				t.Fatalf("busiest hour %d of %d landed, want %d of %d", row.BusiestHourLandings, row.Landed, c.busiest, c.landed)
			}
		})
	}
}

func TestLiveBacklogStandsWhileAReadyPullRequestWaitsForAFullQueue(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	ready := func(n int) queue.PR {
		return queue.PR{Number: n, Head: queue.SHA(fmt.Sprintf("h%d", n)), Checks: queue.Success}
	}
	member := func(n int) queue.Member { return queue.Member{Number: n, Head: queue.SHA(fmt.Sprintf("h%d", n))} }
	candidate := func(sha queue.SHA, verdict queue.Verdict, members ...int) queue.Build {
		b := queue.Build{SHA: sha, Kind: queue.Candidate, Verdict: verdict}
		for _, n := range members {
			b.Chain = append(b.Chain, member(n))
		}
		return b
	}
	probe := func(verdict queue.Verdict) queue.Build {
		return queue.Build{SHA: "p", Kind: queue.Probe, Verdict: verdict, Chain: []queue.Member{member(1)}}
	}
	record := func(m, slots int, prs []queue.PR, builds []queue.Build, actions ...queue.Action) queue.Recording {
		return queue.Recording{World: queue.World{Now: at(m), Trunk: "t", PRs: prs, Builds: builds}, Config: queue.Config{Slots: slots}, Actions: actions}
	}
	full := func(m int) queue.Recording {
		return record(m, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)})
	}
	free := func(m int) queue.Recording {
		return record(m, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)})
	}
	run := func(ms ...int) []queue.Recording {
		var recs []queue.Recording
		for _, m := range ms {
			if m < 0 {
				recs = append(recs, free(-m))
			} else {
				recs = append(recs, full(m))
			}
		}
		return recs
	}
	for _, c := range []struct {
		name    string
		recs    []queue.Recording
		minutes float64
	}{
		{"every slot held", []queue.Recording{full(0), full(7)}, 7},
		{"one slot free", []queue.Recording{free(0), free(7)}, 0},
		{"the record's own push takes the last ready pull request", []queue.Recording{
			record(0, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), probe(queue.Pending)}),
			record(7, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "push", Build: candidate("d", queue.Pending, 1, 2)})}, 0},
		{"the record's own push takes the last free slot", []queue.Recording{
			record(0, 2, []queue.PR{ready(1), ready(2), ready(3)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "push", Build: candidate("d", queue.Pending, 1, 2)}),
			record(7, 2, []queue.PR{ready(1), ready(2), ready(3)}, []queue.Build{candidate("c", queue.Pending, 1), candidate("d", queue.Pending, 1, 2)})}, 7},
		{"a pending probe holds no slot", func() []queue.Recording {
			var recs []queue.Recording
			for m := 0; m <= 60; m += 10 {
				recs = append(recs, record(m, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), probe(queue.Pending)}))
			}
			return recs
		}(), 0},
		{"the record's own probe holds no slot", []queue.Recording{
			record(0, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "push", Build: probe(queue.Pending)}),
			record(7, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "push", Build: probe(queue.Pending)})}, 0},
		{"two pending candidates fill two slots", []queue.Recording{
			record(0, 2, []queue.PR{ready(1), ready(2), ready(3)}, []queue.Build{candidate("c", queue.Pending, 1), candidate("d", queue.Pending, 1, 2)}),
			record(7, 2, []queue.PR{ready(1), ready(2), ready(3)}, []queue.Build{candidate("c", queue.Pending, 1), candidate("d", queue.Pending, 1, 2)})}, 7},
		{"a deleted candidate's members are not started", []queue.Recording{
			full(0),
			record(7, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), candidate("d", queue.Pending, 1, 2)}, queue.Action{Do: "delete", SHA: "d"})}, 7},
		{"a probe with a verdict holds none, nor a candidate with one", []queue.Recording{
			record(0, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), probe(queue.Failure), candidate("e", queue.Success, 3)}),
			record(7, 2, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), probe(queue.Failure), candidate("e", queue.Success, 3)})}, 0},
		{"a pull request held by a wait or by its checks is not ready", []queue.Recording{
			record(0, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Wait: "it is labelled merge-queue-hold"}, {Number: 3, Head: "h3", Checks: queue.Pending}}, []queue.Build{candidate("c", queue.Pending, 1)}),
			record(7, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Wait: "it is labelled merge-queue-hold"}, {Number: 3, Head: "h3", Checks: queue.Pending}}, []queue.Build{candidate("c", queue.Pending, 1)})}, 0},
		{"a pull request that conflicts with the trunk cannot be built", []queue.Recording{
			record(0, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Note: trunkConflict}}, []queue.Build{candidate("c", queue.Pending, 1)}),
			record(7, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Note: trunkConflict}}, []queue.Build{candidate("c", queue.Pending, 1)})}, 0},
		{"a pull request at a head no chain holds is not started", []queue.Recording{
			record(0, 1, []queue.PR{{Number: 1, Head: "pushed-again", Checks: queue.Success}}, []queue.Build{candidate("c", queue.Pending, 1)}),
			record(7, 1, []queue.PR{{Number: 1, Head: "pushed-again", Checks: queue.Success}}, []queue.Build{candidate("c", queue.Pending, 1)})}, 7},
		{"the record's own delete frees the slot", []queue.Recording{
			full(0),
			record(7, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "delete", SHA: "c"})}, 0},
		{"the record's own landing takes the waiter", []queue.Recording{
			full(0),
			record(7, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1), candidate("e", queue.Success, 2)}, queue.Action{Do: "land", SHA: "e", Members: []queue.Member{member(2)}})}, 0},
		{"the record's own ejection takes the waiter", []queue.Recording{
			full(0),
			record(7, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "eject", PR: 2, Cause: "culprit"})}, 0},
		{"the record's own note that a pull request conflicts with the trunk", []queue.Recording{
			record(0, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "note", PR: 2, Text: trunkConflict}),
			record(7, 1, []queue.PR{ready(1), ready(2)}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "note", PR: 2, Text: trunkConflict})}, 0},
		{"the record's own note replaces an old note that it conflicts with the trunk", []queue.Recording{
			record(0, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Note: trunkConflict}}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "note", PR: 2, Text: "in the queue, waiting"}),
			record(7, 1, []queue.PR{ready(1), {Number: 2, Head: "h2", Checks: queue.Success, Note: trunkConflict}}, []queue.Build{candidate("c", queue.Pending, 1)}, queue.Action{Do: "note", PR: 2, Text: "in the queue, waiting"})}, 7},
		{"no slots, no backlog", func() []queue.Recording {
			var recs []queue.Recording
			for m := 0; m <= 60; m += 10 {
				recs = append(recs, record(m, 0, []queue.PR{ready(1)}, nil))
			}
			return recs
		}(), 0},
		{"a run broken by one record with no backlog restarts", run(0, 5, -10, 15, 20, 25, 30), 15},
		{"more than 10 minutes with no record ends the run", run(0, 10, 21, 25), 10},
		{"a run lasts from its first record to its last", run(0, 4, -9), 4},
		{"records before the window do not count", append([]queue.Recording{full(-5)}, run(0, 5)...), 5},
		{"records at and after the window's end do not count", run(110, 115, 120, 125), 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := windowRow(t, c.recs, at(0), at(120))
			if row.BacklogLongestMinutes != c.minutes {
				t.Fatalf("longest backlog %v minutes, want %v", row.BacklogLongestMinutes, c.minutes)
			}
		})
	}
}

func TestLiveSaysWhatBecameOfALandingsFirstCandidate(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }
	type read struct {
		m       int
		builds  []queue.Build
		actions []queue.Action
	}
	type story struct {
		ready  int
		reads  func(n int, first, last queue.Build) []read
		heldAt string
	}
	push := func(b queue.Build) []queue.Action { return []queue.Action{{Do: "push", Build: b}} }
	with := func(b queue.Build, v queue.Verdict) queue.Build {
		b.Verdict = v
		return b
	}
	land := func(b queue.Build) []queue.Action { return []queue.Action{{Do: "land", SHA: b.SHA, Members: b.Chain}} }
	tell := func(n, base int, trunk queue.SHA, s story, firstHead string) ([]queue.Recording, queue.SHA) {
		head := queue.SHA(fmt.Sprintf("h%d", n))
		if firstHead == "" {
			firstHead = string(head)
		}
		first := queue.Build{SHA: queue.SHA(fmt.Sprintf("f%d", n)), Chain: []queue.Member{{Number: n, Head: queue.SHA(firstHead)}}}
		last := queue.Build{SHA: queue.SHA(fmt.Sprintf("l%d", n)), Chain: []queue.Member{{Number: n, Head: head}}}
		var recs []queue.Recording
		for _, r := range s.reads(n, first, last) {
			pr := queue.PR{Number: n, Head: head, Queued: at(base), Checks: queue.Pending}
			if r.m >= s.ready {
				pr.Checks = queue.Success
			}
			builds := slices.Clone(r.builds)
			actions := slices.Clone(r.actions)
			for i := range builds {
				builds[i].Pushed = builds[i].Pushed.Add(time.Duration(base) * time.Minute)
			}
			for i := range actions {
				if actions[i].Do == "push" {
					actions[i].Build.Pushed = at(base + r.m)
				}
			}
			recs = append(recs, queue.Recording{World: queue.World{Now: at(base + r.m), Trunk: trunk, PRs: []queue.PR{pr}, Builds: builds}, Actions: actions})
		}
		recs = append(recs, queue.Recording{World: queue.World{Now: recs[len(recs)-1].World.Now.Add(2 * time.Minute), Trunk: last.SHA}})
		return recs, last.SHA
	}
	pushed := func(b queue.Build, m int) queue.Build {
		b.Pushed = at(m)
		return b
	}
	failed := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 25, builds: []queue.Build{with(pushed(f, 5), queue.Failure)}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "failed"}
	dropped := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10, builds: []queue.Build{pushed(f, 5)}},
			{m: 20, builds: []queue.Build{pushed(f, 5)}, actions: []queue.Action{{Do: "delete", SHA: f.SHA}}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "dropped"}
	overtaken := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10, builds: []queue.Build{pushed(f, 5)}},
			{m: 40, builds: []queue.Build{pushed(f, 5)}, actions: push(l)},
			{m: 50, builds: []queue.Build{pushed(f, 5), with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "overtaken"}
	waited := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 20}, {m: 35, actions: push(l)}, {m: 45, builds: []queue.Build{with(pushed(l, 35), queue.Success)}, actions: land(l)}}
	}, "waited"}
	redThenDeleted := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10, builds: []queue.Build{pushed(f, 5)}},
			{m: 25, builds: []queue.Build{with(pushed(f, 5), queue.Failure)}},
			{m: 30, builds: []queue.Build{pushed(f, 5)}, actions: []queue.Action{{Do: "delete", SHA: f.SHA}}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "failed"}
	beforeReady := story{10, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10}, {m: 25, builds: []queue.Build{with(pushed(f, 5), queue.Failure)}},
			{m: 50, actions: push(l)}, {m: 60, builds: []queue.Build{with(pushed(l, 50), queue.Success)}, actions: land(l)}}
	}, "failed"}
	thrownAway := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 20, builds: []queue.Build{with(pushed(f, 5), queue.Success)}, actions: []queue.Action{{Do: "delete", SHA: f.SHA}}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "passed"}
	passed := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 25, builds: []queue.Build{with(pushed(f, 5), queue.Success)}},
			{m: 40, builds: []queue.Build{with(pushed(f, 5), queue.Success)}, actions: push(l)},
			{m: 50, builds: []queue.Build{with(pushed(f, 5), queue.Success), with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "passed"}
	passedAtTheLanding := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10, builds: []queue.Build{pushed(f, 5)}},
			{m: 40, builds: []queue.Build{pushed(f, 5)}, actions: push(l)},
			{m: 50, builds: []queue.Build{with(pushed(f, 5), queue.Success), with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "overtaken"}
	redThenPassed := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 25, builds: []queue.Build{with(pushed(f, 5), queue.Failure)}},
			{m: 30, builds: []queue.Build{with(pushed(f, 5), queue.Success)}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "failed"}
	other := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 10, builds: []queue.Build{pushed(f, 5)}},
			{m: 40, actions: push(l)}, {m: 50, builds: []queue.Build{with(pushed(l, 40), queue.Success)}, actions: land(l)}}
	}, "other"}
	quick := story{0, func(n int, f, l queue.Build) []read {
		return []read{{m: 0}, {m: 5, actions: push(f)}, {m: 15, builds: []queue.Build{with(pushed(f, 5), queue.Failure)}},
			{m: 20, actions: push(l)}, {m: 30, builds: []queue.Build{with(pushed(l, 20), queue.Success)}, actions: land(l)}}
	}, ""}
	held := func(m liveMeasures) map[string]int {
		return map[string]int{"waited": m.HeldFirstWaited, "failed": m.HeldFirstFailed, "dropped": m.HeldFirstDropped, "passed": m.HeldFirstPassed, "overtaken": m.HeldFirstOvertaken, "other": m.HeldFirstOther}
	}
	for _, c := range []struct {
		name         string
		s            story
		firstHead    string
		added, first float64
		over30       int
	}{
		{"the first candidate failed and a later one landed", failed, "", 40, 5, 0},
		{"the first candidate was dropped while building", dropped, "", 40, 5, 0},
		{"a later candidate landed while the first still built", overtaken, "", 40, 5, 0},
		{"no candidate for 35 minutes", waited, "", 35, 35, 1},
		{"read red, built again and deleted is failed, not dropped", redThenDeleted, "", 40, 5, 0},
		{"a first candidate pushed before the pull request was ready", beforeReady, "", 40, 0, 0},
		{"a candidate at an older head is not the first", failed, "h1-old", 40, 40, 1},
		{"the first candidate passed and was thrown away", thrownAway, "", 40, 5, 0},
		{"the first candidate passed before the landing record and could not land", passed, "", 40, 5, 0},
		{"the first candidate's first pass read in the landing record is overtaken", passedAtTheLanding, "", 40, 5, 0},
		{"read red, then passing, is failed, not passed", redThenPassed, "", 40, 5, 0},
		{"the first candidate was never read again", other, "", 40, 5, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			recs, _ := tell(1, 0, "t", c.s, c.firstHead)
			m := windowRow(t, recs, at(0), at(100)).liveMeasures
			want := map[string]int{"waited": 0, "failed": 0, "dropped": 0, "passed": 0, "overtaken": 0, "other": 0}
			if c.firstHead != "" {
				want["waited"] = 1
			} else {
				want[c.s.heldAt] = 1
			}
			if m.SplitLandings != 1 || m.QueueAddedOver30 != 1 || *m.QueueAddedP90Minutes != c.added || m.FirstCandidateP90Minutes == nil || *m.FirstCandidateP90Minutes != c.first ||
				m.FirstCandidateOver30 != c.over30 || !reflect.DeepEqual(held(m), want) {
				t.Fatalf("queue adds %s, to the first candidate %s, over 30 %d, held %v; want %v, %v, %d, %v",
					measured(m.QueueAddedP90Minutes), measured(m.FirstCandidateP90Minutes), m.FirstCandidateOver30, held(m), c.added, c.first, c.over30, want)
			}
		})
	}
	t.Run("a day mixing all of them", func(t *testing.T) {
		var recs []queue.Recording
		trunk := queue.SHA("t")
		for i, s := range []story{failed, dropped, overtaken, waited, redThenDeleted, passed, passedAtTheLanding, other, quick} {
			var day []queue.Recording
			day, trunk = tell(i+1, 100*i, trunk, s, "")
			recs = append(recs, day...)
		}
		m := windowRow(t, recs, at(0), at(900)).liveMeasures
		counts := held(m)
		sum := 0
		for _, n := range counts {
			sum += n
		}
		if m.SplitLandings != 9 || m.QueueAddedOver30 != 8 || sum != m.QueueAddedOver30 || m.FirstCandidateOver30 != 1 ||
			!reflect.DeepEqual(counts, map[string]int{"waited": 1, "failed": 2, "dropped": 1, "passed": 1, "overtaken": 2, "other": 1}) {
			t.Fatalf("split %d, held over 30 %d, to the first candidate over 30 %d, held %v", m.SplitLandings, m.QueueAddedOver30, m.FirstCandidateOver30, counts)
		}
	})
}
