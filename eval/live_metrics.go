package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type liveOptions struct {
	from, to *time.Time
	row      bool
}

func liveBounds(from, to string, row bool) (liveOptions, error) {
	opts := liveOptions{row: row}
	for _, bound := range []struct {
		name, text string
		at         **time.Time
	}{{"from", from, &opts.from}, {"to", to, &opts.to}} {
		if bound.text != "" {
			at, err := time.Parse(time.RFC3339, bound.text)
			if err != nil {
				return opts, fmt.Errorf("-%s: %w", bound.name, err)
			}
			*bound.at = &at
		}
	}
	if opts.from != nil && opts.to != nil && !opts.from.Before(*opts.to) {
		return opts, fmt.Errorf("-from must be before -to")
	}
	return opts, nil
}

type liveRow struct {
	From                       *time.Time `json:"from"`
	To                         *time.Time `json:"to"`
	FirstTick                  time.Time  `json:"first_tick"`
	LastTick                   time.Time  `json:"last_tick"`
	Ticks                      int        `json:"ticks"`
	Commit                     string     `json:"commit"`
	BusyMinutes                float64    `json:"busy_minutes"`
	Landed                     int        `json:"landed"`
	BuildMedianMinutes         *float64   `json:"build_median_minutes"`
	WaitP50                    *float64   `json:"wait_p50"`
	WaitP90                    *float64   `json:"wait_p90"`
	WaitP50Minutes             *float64   `json:"wait_p50_minutes"`
	WaitP90Minutes             *float64   `json:"wait_p90_minutes"`
	LabelledP50                *float64   `json:"labelled_p50"`
	LabelledP90                *float64   `json:"labelled_p90"`
	LabelledSlowest20th        *float64   `json:"labelled_slowest_20th"`
	LabelledP50Minutes         *float64   `json:"labelled_p50_minutes"`
	LabelledP90Minutes         *float64   `json:"labelled_p90_minutes"`
	LabelledSlowest20thMinutes *float64   `json:"labelled_slowest_20th_minutes"`
	EjectedP90                 *float64   `json:"ejected_p90"`
	EjectedP90Minutes          *float64   `json:"ejected_p90_minutes"`
	BuildsPerLanding           *float64   `json:"builds_per_landing"`
	LandingsPerHour            *float64   `json:"landings_per_hour"`
	WrongEjections             int        `json:"wrong_ejections"`
	WrongPer1000               *float64   `json:"wrong_per_1000"`
	Unchecked                  int        `json:"unchecked"`
	Unreadable                 int        `json:"unreadable"`
	GoodBuilds                 int        `json:"good_builds"`
	NonPassingBuilds           int        `json:"non_passing_builds"`
	EjectionsAsCause           int        `json:"ejections_as_cause"`
	liveMeasures
	Simulated *liveComparison `json:"simulated,omitempty"`
	OverLive  *liveComparison `json:"over_live,omitempty"`
	busy      time.Duration
}

type liveComparison struct {
	Landed           *float64 `json:"landed"`
	WaitP50          *float64 `json:"wait_p50"`
	WaitP90          *float64 `json:"wait_p90"`
	BuildsPerLanding *float64 `json:"builds_per_landing"`
}

type liveMeasures struct {
	SplitLandings                int        `json:"split_landings"`
	UnsplitLandings              int        `json:"unsplit_landings"`
	QueueAddedP50Minutes         *float64   `json:"queue_added_p50_minutes"`
	QueueAddedP90Minutes         *float64   `json:"queue_added_p90_minutes"`
	QueueAddedP95Minutes         *float64   `json:"queue_added_p95_minutes"`
	QueueAddedSumMinutes         *float64   `json:"queue_added_sum_minutes"`
	QueueAddedOver30             int        `json:"queue_added_over_30"`
	QueueAddedOver30SumMinutes   *float64   `json:"queue_added_over_30_sum_minutes"`
	LandingBuildMedianMinutes    *float64   `json:"landing_build_median_minutes"`
	SplitReadyToLandedP50Minutes *float64   `json:"split_ready_to_landed_p50_minutes"`
	SplitReadyToLandedP90Minutes *float64   `json:"split_ready_to_landed_p90_minutes"`
	ReadyGaps                    int        `json:"ready_gaps"`
	LongestReadyGapMinutes       *float64   `json:"longest_ready_gap_minutes"`
	LongestReadyGapFrom          *time.Time `json:"longest_ready_gap_from"`
	LongestReadyGapTo            *time.Time `json:"longest_ready_gap_to"`
	ReadyGapsOver90              int        `json:"ready_gaps_over_90"`
	RedCandidates                int        `json:"red_candidates"`
	RedReadN                     int        `json:"red_read_n"`
	RedReadMedianMinutes         *float64   `json:"red_read_median_minutes"`
	RedReadSumMinutes            *float64   `json:"red_read_sum_minutes"`
	RedPullRequests              int        `json:"red_pull_requests"`
}

type recordedHead struct {
	number int
	head   queue.SHA
}

type landingParts struct {
	first, again, build, toLand, total float64
}

// splitLive splits each planned landing from observed heads, candidate pushes and verdicts.
func splitLive(recs []queue.Recording) ([]landingParts, int) {
	type push struct {
		at    time.Time
		heads map[recordedHead]bool
	}
	type landing struct {
		at   time.Time
		head recordedHead
		sha  queue.SHA
	}
	ready, verdicts := map[recordedHead]time.Time{}, map[queue.SHA]time.Time{}
	pushes := map[queue.SHA]push{}
	var landings []landing
	for _, rec := range recs {
		now := rec.World.Now.Truncate(time.Second)
		for _, pr := range rec.World.PRs {
			h := recordedHead{pr.Number, pr.Head}
			if pr.Checks == queue.Success && ready[h].IsZero() {
				ready[h] = now
			}
		}
		for _, b := range rec.World.Builds {
			if b.Verdict != queue.Pending && verdicts[b.SHA].IsZero() {
				verdicts[b.SHA] = now
			}
		}
		for _, a := range rec.Actions {
			switch {
			case a.Do == "push" && a.Build.Kind == queue.Candidate:
				p := push{at: now, heads: map[recordedHead]bool{}}
				for _, m := range a.Build.Chain {
					p.heads[recordedHead{m.Number, m.Head}] = true
				}
				pushes[a.Build.SHA] = p
			case a.Do == "land":
				for _, m := range a.Members {
					landings = append(landings, landing{now, recordedHead{m.Number, m.Head}, a.SHA})
				}
			}
		}
	}
	var rows []landingParts
	skipped := 0
	for _, l := range landings {
		var first time.Time
		for _, p := range pushes {
			if p.heads[l.head] && !p.at.After(l.at) && (first.IsZero() || p.at.Before(first)) {
				first = p.at
			}
		}
		r, lp, v := ready[l.head], pushes[l.sha].at, verdicts[l.sha]
		if r.IsZero() || first.IsZero() || lp.IsZero() || v.IsZero() || r.After(first) || first.After(lp) || lp.After(v) || v.After(l.at) {
			skipped++
			continue
		}
		rows = append(rows, landingParts{first.Sub(r).Minutes(), lp.Sub(first).Minutes(), v.Sub(lp).Minutes(), l.at.Sub(v).Minutes(), l.at.Sub(r).Minutes()})
	}
	return rows, skipped
}

func measureLive(recs []queue.Recording) liveMeasures {
	rows, skipped := splitLive(recs)
	m := liveMeasures{SplitLandings: len(rows), UnsplitLandings: skipped}
	var added, build, total []float64
	for _, r := range rows {
		added, build, total = append(added, r.first+r.again), append(build, r.build), append(total, r.total)
	}
	slices.Sort(added)
	var sum, over30 float64
	for _, a := range added {
		sum += a
		if a > 30 {
			m.QueueAddedOver30++
			over30 += a
		}
	}
	m.QueueAddedP50Minutes, m.QueueAddedP90Minutes, m.QueueAddedP95Minutes = median(added), quantile(added, 90), quantile(added, 95)
	m.LandingBuildMedianMinutes = median(build)
	m.SplitReadyToLandedP50Minutes, m.SplitReadyToLandedP90Minutes = median(total), quantile(total, 90)
	if len(rows) > 0 {
		m.QueueAddedSumMinutes, m.QueueAddedOver30SumMinutes = number(sum), number(over30)
	}

	var prev queue.SHA
	var lastLand time.Time
	readyAll := true
	for i, rec := range recs {
		now := rec.World.Now.Truncate(time.Second)
		ready := false
		for _, pr := range rec.World.PRs {
			if strings.HasPrefix(pr.Note, "in the queue") {
				ready = true
			}
		}
		if i > 0 && rec.World.Trunk != prev {
			if !lastLand.IsZero() && readyAll {
				gap := now.Sub(lastLand).Minutes()
				m.ReadyGaps++
				if m.LongestReadyGapMinutes == nil || gap >= *m.LongestReadyGapMinutes {
					m.LongestReadyGapMinutes, m.LongestReadyGapFrom, m.LongestReadyGapTo = number(gap), timeValue(lastLand), timeValue(now)
				}
				if gap > 90 {
					m.ReadyGapsOver90++
				}
			}
			lastLand, readyAll = now, true
		} else {
			readyAll = readyAll && ready
		}
		prev = rec.World.Trunk
	}

	failed, red := map[queue.SHA]time.Time{}, map[queue.SHA]time.Time{}
	kinds, chains := map[queue.SHA]queue.Kind{}, map[queue.SHA][]queue.Member{}
	var redOrder []queue.SHA
	for _, rec := range recs {
		now := rec.World.Now.Truncate(time.Second)
		for _, b := range rec.World.Builds {
			kinds[b.SHA] = b.Kind
			if _, ok := chains[b.SHA]; !ok {
				chains[b.SHA] = b.Chain
			}
			if len(b.Failed) > 0 && failed[b.SHA].IsZero() {
				failed[b.SHA] = now
			}
			if b.Verdict == queue.Failure && red[b.SHA].IsZero() {
				red[b.SHA] = now
				redOrder = append(redOrder, b.SHA)
			}
		}
	}
	var latencies []float64
	var redSum float64
	slices.SortStableFunc(redOrder, func(a, b queue.SHA) int { return red[a].Compare(red[b]) })
	for _, sha := range redOrder {
		at := red[sha]
		if kinds[sha] != queue.Candidate {
			continue
		}
		m.RedCandidates++
		m.RedPullRequests += len(chains[sha])
		if !failed[sha].IsZero() {
			value, _ := strconv.ParseFloat(strconv.FormatFloat(at.Sub(failed[sha]).Minutes(), 'f', 1, 64), 64)
			latencies = append(latencies, value)
			redSum += value
		}
	}
	m.RedReadN, m.RedReadMedianMinutes = len(latencies), median(latencies)
	if len(latencies) > 0 {
		m.RedReadSumMinutes = number(math.RoundToEven(redSum))
	}
	return m
}

func number(x float64) *float64 { return &x }

func timeValue(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

func median(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	xs = slices.Sorted(slices.Values(xs))
	n := len(xs)
	return number((xs[(n-1)/2] + xs[n/2]) / 2)
}

func quantile(xs []float64, p int) *float64 {
	if len(xs) == 0 {
		return nil
	}
	return number(percentile(slices.Clone(xs), p))
}

func durationQuantile(ds []time.Duration, p int) *float64 {
	var xs []float64
	for _, d := range ds {
		xs = append(xs, d.Minutes())
	}
	return quantile(xs, p)
}

func evalRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return buildRevision(info)
}

func buildRevision(info *debug.BuildInfo) string {
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if modified {
		revision += "+"
	}
	return revision
}

func measured(x *float64) string {
	if x == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f", *x)
}

func measuredTime(at *time.Time) string {
	if at == nil {
		return "unknown"
	}
	return at.Format(time.RFC3339)
}

func printLive(sc Score, row liveRow) {
	whole := *row.BuildMedianMinutes
	fmt.Printf("%d ticks over %s with something queued; a whole build took %.1f min at the median, as the ticks saw it\n",
		row.Ticks, row.busy.Round(time.Minute), whole)
	fmt.Printf("ready to landed: %.0f min at the median, %.0f at p90\n", sc.WaitP50*whole, sc.WaitP90*whole)
	fmt.Printf("labelled to landed: %.0f min at the median, %.0f at p90, %.0f at the mean of the slowest twentieth\n",
		sc.LabelP50*whole, sc.LabelP90*whole, sc.LabelSlow*whole)
	fmt.Printf("wrong ejections: %d in %d landings (%.2f per 1,000)", row.WrongEjections, sc.Landed, sc.Wrong)
	if sc.Unchecked > 0 {
		fmt.Printf(", %d at a newer head unchecked", sc.Unchecked)
	}
	if sc.Unreadable > 0 {
		fmt.Printf(", %d at a newer head unreadable", sc.Unreadable)
	}
	fmt.Println()
	fmt.Printf("of %d whole builds of trees that passed, alone or under more, %d did not pass\n", sc.Good, sc.Flaked)
	fmt.Printf("landings split: %d; could not be split: %d (ready, candidate or verdict missing, or out of order)\n", row.SplitLandings, row.UnsplitLandings)
	fmt.Printf("minutes the queue adds: %s at the median, %s at p90, %s at p95, %s in all; over 30 min: %d, %s min in all\n",
		measured(row.QueueAddedP50Minutes), measured(row.QueueAddedP90Minutes), measured(row.QueueAddedP95Minutes), measured(row.QueueAddedSumMinutes), row.QueueAddedOver30, measured(row.QueueAddedOver30SumMinutes))
	fmt.Printf("landing candidate's build: %s min at the median\n", measured(row.LandingBuildMedianMinutes))
	fmt.Printf("ready to landed, split landings: %s min at the median, %s at p90\n", measured(row.SplitReadyToLandedP50Minutes), measured(row.SplitReadyToLandedP90Minutes))
	fmt.Printf("gaps between landings with a ready head waiting: %d; longest %s min, %s to %s; over 90 min: %d\n",
		row.ReadyGaps, measured(row.LongestReadyGapMinutes), measuredTime(row.LongestReadyGapFrom), measuredTime(row.LongestReadyGapTo), row.ReadyGapsOver90)
	fmt.Printf("red candidates: %d, holding %d pull requests; failed job seen to read red: n=%d, %s min at the median, %s in all\n",
		row.RedCandidates, row.RedPullRequests, row.RedReadN, measured(row.RedReadMedianMinutes), measured(row.RedReadSumMinutes))
}

func fractionOver(simulated, live *float64) *float64 {
	if simulated == nil || live == nil || *live == 0 {
		return nil
	}
	return number((*simulated - *live) / *live)
}

func runLive(dir, clone, like, from, to string, rowOnly bool) error {
	opts, err := liveBounds(from, to, rowOnly)
	if err != nil {
		return err
	}
	var shape *Shape
	if like != "" {
		for i := range shapes {
			if shapes[i].Name == like {
				shape = &shapes[i]
			}
		}
		if shape == nil {
			return fmt.Errorf("unknown shape %q", like)
		}
	}
	sc, win, row, err := scoreLive(dir, clone, opts)
	if err != nil {
		return err
	}
	var simulated *Score
	if shape != nil && win.build > 0 {
		sim, comparison := win.simulate(*shape, sc)
		simulated, row.Simulated = &sim, &comparison
		row.OverLive = &liveComparison{
			Landed: fractionOver(row.Simulated.Landed, number(float64(row.Landed))), WaitP50: fractionOver(row.Simulated.WaitP50, row.WaitP50),
			WaitP90: fractionOver(row.Simulated.WaitP90, row.WaitP90), BuildsPerLanding: fractionOver(row.Simulated.BuildsPerLanding, row.BuildsPerLanding),
		}
	} else if shape != nil {
		row.Simulated, row.OverLive = &liveComparison{}, &liveComparison{}
	}
	if rowOnly {
		return json.NewEncoder(os.Stdout).Encode(row)
	}
	printLive(sc, row)
	fmt.Printf(columns, "shape", "config", "landed", "wrong/1k", "wait50", "wait90", "label50", "label90", "slow20th", "eject90", "builds/pr", "per hour")
	sc.print(Shape{})
	if simulated != nil {
		simulated.print(Shape{})
	}
	return nil
}
