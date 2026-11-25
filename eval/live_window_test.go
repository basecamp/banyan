package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
