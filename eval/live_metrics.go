package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type liveOptions struct {
	from, to *time.Time
	row      bool
	replays  bool // a shape replays the window, so git is asked which heads clash
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
	WholeBuilds                      int        `json:"whole_builds"`
	DeparturesAfterAnUnconfirmedPlan *int       `json:"departures_after_an_unconfirmed_plan,omitempty"`
	From                             *time.Time `json:"from"`
	To                               *time.Time `json:"to"`
	FirstTick                        time.Time  `json:"first_tick"`
	LastTick                         time.Time  `json:"last_tick"`
	Ticks                            int        `json:"ticks"`
	ContextTicks                     int        `json:"context_ticks"`
	Commit                           string     `json:"commit"`
	BusyMinutes                      float64    `json:"busy_minutes"`
	Landed                           int        `json:"landed"`
	Arrivals                         *int       `json:"arrivals,omitempty"`
	WaitingAtOpen                    *int       `json:"waiting_at_open,omitempty"`
	SettingsInWindow                 *int       `json:"settings_in_window,omitempty"`
	LeftWaiting                      *int       `json:"left_waiting,omitempty"`
	Withdrawn                        *int       `json:"withdrawn,omitempty"`
	LandingsWithoutAWait             *int       `json:"landings_without_a_wait,omitempty"`
	UnconfirmedLandingPlans          *int       `json:"unconfirmed_landing_plans,omitempty"`
	BuildMedianMinutes               *float64   `json:"build_median_minutes"`
	WaitP50                          *float64   `json:"wait_p50"`
	WaitP90                          *float64   `json:"wait_p90"`
	WaitP50Minutes                   *float64   `json:"wait_p50_minutes"`
	WaitP90Minutes                   *float64   `json:"wait_p90_minutes"`
	LabelledP50                      *float64   `json:"labelled_p50"`
	LabelledP90                      *float64   `json:"labelled_p90"`
	LabelledSlowest20th              *float64   `json:"labelled_slowest_20th"`
	LabelledP50Minutes               *float64   `json:"labelled_p50_minutes"`
	LabelledP90Minutes               *float64   `json:"labelled_p90_minutes"`
	LabelledSlowest20thMinutes       *float64   `json:"labelled_slowest_20th_minutes"`
	EjectedP90                       *float64   `json:"ejected_p90"`
	EjectedP90Minutes                *float64   `json:"ejected_p90_minutes"`
	BuildsPerLanding                 *float64   `json:"builds_per_landing"`
	LandingsPerHour                  *float64   `json:"landings_per_hour"`
	WrongEjections                   int        `json:"wrong_ejections"`
	WrongPer1000                     *float64   `json:"wrong_per_1000"`
	Unchecked                        int        `json:"unchecked"`
	Unreadable                       int        `json:"unreadable"`
	GoodBuilds                       int        `json:"good_builds"`
	NonPassingBuilds                 int        `json:"non_passing_builds"`
	EjectionsAsCause                 int        `json:"ejections_as_cause"`
	liveMeasures
	Perturbed string          `json:"perturbed,omitempty"`
	Simulated *liveComparison `json:"simulated,omitempty"`
	OverLive  *liveComparison `json:"over_live,omitempty"`
	busy      time.Duration
}

type liveComparison struct {
	Landed           *float64 `json:"landed"`
	WaitP50          *float64 `json:"wait_p50"`
	WaitP90          *float64 `json:"wait_p90"`
	BuildsPerLanding *float64 `json:"builds_per_landing"`
	*simulatedMeasures
}

type simulatedMeasures struct {
	UnblamedRedCandidates            int            `json:"unblamed_red_candidates"`
	CandidatesWithoutACulprit        int            `json:"candidates_without_a_culprit"`
	CulpritsUnattributed             int            `json:"culprits_unattributed"`
	UnejectedGuiltyHeads             int            `json:"unejected_guilty_heads"`
	RedTrunkSpans                    int            `json:"red_trunk_spans"`
	UnfixedRedTrunkSpans             float64        `json:"unfixed_red_trunk_spans"`
	UnplayedRedTrunkSpans            int            `json:"unplayed_red_trunk_spans"`
	UnplayedRedTrunkMinutes          float64        `json:"unplayed_red_trunk_minutes"`
	DeparturesAfterAnUnconfirmedPlan int            `json:"departures_after_an_unconfirmed_plan"`
	InvariantFailures                int            `json:"invariant_failures"`
	WaitingAtOpen                    float64        `json:"waiting_at_open"`
	SettingsInWindow                 int            `json:"settings_in_window"`
	Acts                             map[string]int `json:"acts"`
	UnattributedHolds                int            `json:"unattributed_holds"`
	ClashingPairs                    *int           `json:"clashing_pairs,omitempty"`
	WaitP50Minutes                   *float64       `json:"wait_p50_minutes"`
	WaitP90Minutes                   *float64       `json:"wait_p90_minutes"`
	WholeBuilds                      float64        `json:"whole_builds"`
	Probes                           float64        `json:"probes"`
	RedBuilds                        float64        `json:"red_builds"`
	QueueAddedP50Minutes             *float64       `json:"queue_added_p50_minutes"`
	QueueAddedP90Minutes             *float64       `json:"queue_added_p90_minutes"`
	QueueAddedOver30                 float64        `json:"queue_added_over_30"`
	RedCandidates                    float64        `json:"red_candidates"`
	RedPullRequests                  float64        `json:"red_pull_requests"`
	EjectedAsCause                   float64        `json:"ejected_as_cause"`
	LeftWaiting                      float64        `json:"left_waiting"`
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
	FirstCandidateP90Minutes     *float64   `json:"first_candidate_p90_minutes"`
	FirstCandidateOver30         int        `json:"first_candidate_over_30"`
	HeldFirstWaited              int        `json:"held_first_waited"`
	HeldFirstFailed              int        `json:"held_first_failed"`
	HeldFirstDropped             int        `json:"held_first_dropped"`
	HeldFirstPassed              int        `json:"held_first_passed"`
	HeldFirstOvertaken           int        `json:"held_first_overtaken"`
	HeldFirstOther               int        `json:"held_first_other"`
	LandingBuildMedianMinutes    *float64   `json:"landing_build_median_minutes"`
	SplitReadyToLandedP50Minutes *float64   `json:"split_ready_to_landed_p50_minutes"`
	SplitReadyToLandedP90Minutes *float64   `json:"split_ready_to_landed_p90_minutes"`
	ReadyGaps                    int        `json:"ready_gaps"`
	LongestReadyGapMinutes       *float64   `json:"longest_ready_gap_minutes"`
	LongestReadyGapFrom          *time.Time `json:"longest_ready_gap_from"`
	LongestReadyGapTo            *time.Time `json:"longest_ready_gap_to"`
	ReadyGapsOver90              int        `json:"ready_gaps_over_90"`
	// BusiestHourLandings is the most landings in the window in 60 minutes from one of them.
	// BacklogLongestMinutes is the longest run of records in the window, none more than 10
	// minutes after the one before, after whose own actions a pull request that could be built
	// is in no pending candidate or push and no slot is free, from the run's first record to its last.
	BusiestHourLandings   int     `json:"busiest_hour_landings"`
	BacklogLongestMinutes float64 `json:"backlog_longest_minutes"`
	// UnplannedTrunkMoves counts the ticks in the window that read the trunk at a commit the tick
	// before did not and no recorded plan landed: a merge made outside the queue, or a tick whose
	// record is missing, whose landings the score cannot count.
	UnplannedTrunkMoves int `json:"unplanned_trunk_moves"`
	// PassingCandidatesThrownAway counts the candidates a tick in the window deleted while they
	// had passed with members on them, and that no recorded plan landed, each once. Of those,
	// ThrownAwayHeadMoved counts the ones with a member queued at another head by then, and
	// ThrownAwayMemberLeft the ones with a member no longer queued; one can be both, or neither.
	PassingCandidatesThrownAway int `json:"passing_candidates_thrown_away"`
	ThrownAwayHeadMoved         int `json:"thrown_away_head_moved"`
	ThrownAwayMemberLeft        int `json:"thrown_away_member_left"`
	// BuildingCandidatesDropped counts the candidates a tick in the window deleted while they were
	// still building, with a member queued at another head by then (DroppedHeadMoved) or no longer
	// queued, though it neither landed nor was ejected by the queue (DroppedMemberLeft), each once.
	// DroppedMembersStillQueued counts their members still queued at the head that was building,
	// each once, and DroppedLongestBuiltMinutes is the longest any with a push time had built, 0
	// when none had one.
	BuildingCandidatesDropped  int      `json:"building_candidates_dropped"`
	DroppedHeadMoved           int      `json:"dropped_head_moved"`
	DroppedMemberLeft          int      `json:"dropped_member_left"`
	DroppedMembersStillQueued  int      `json:"dropped_members_still_queued"`
	DroppedLongestBuiltMinutes float64  `json:"dropped_longest_built_minutes"`
	RedCandidates              int      `json:"red_candidates"`
	RedReadN                   int      `json:"red_read_n"`
	RedReadMedianMinutes       *float64 `json:"red_read_median_minutes"`
	RedReadSumMinutes          *float64 `json:"red_read_sum_minutes"`
	RedPullRequests            int      `json:"red_pull_requests"`
}

type recordedHead struct {
	number int
	head   queue.SHA
	queued time.Time
}

type liveLanding struct {
	at, ready time.Time
	sha       queue.SHA
	member    headKey
	record    int // the index of the record that planned it
}

type landingParts struct {
	added, build, toLand, total float64
	landing                     liveLanding
}

func (opts liveOptions) contains(at time.Time) bool {
	return (opts.from == nil || !at.Before(*opts.from)) && (opts.to == nil || at.Before(*opts.to))
}

func (opts liveOptions) context() liveOptions {
	if opts.from != nil {
		at := opts.from.Add(-72 * time.Hour)
		opts.from = &at
	}
	if opts.to != nil {
		at := opts.to.Add(time.Hour)
		opts.to = &at
	}
	return opts
}

// splitLive splits counted landings only when all of their clocks are known and ordered.
func splitLive(recs []queue.Recording, landings []liveLanding, opts liveOptions) ([]landingParts, int) {
	pushes, verdicts := pushTimes(recs), map[queue.SHA]time.Time{}
	for _, rec := range recs {
		for _, b := range rec.World.Builds {
			if b.Verdict != queue.Pending && verdicts[b.SHA].IsZero() {
				verdicts[b.SHA] = rec.World.Now
			}
		}
	}
	var rows []landingParts
	skipped := 0
	for _, l := range landings {
		if !opts.contains(l.at) || l.at.IsZero() {
			continue
		}
		r, p, v := l.ready, pushes[l.sha], verdicts[l.sha]
		end := l.at
		if v.IsZero() || end.Before(v) {
			v = end
		}
		if r.IsZero() || p.IsZero() || v.IsZero() || end.IsZero() || r.After(p) || p.After(v) || v.After(end) {
			skipped++
			continue
		}
		rows = append(rows, landingParts{p.Sub(r).Minutes(), v.Sub(p).Minutes(), end.Sub(v).Minutes(), end.Sub(r).Minutes(), l})
	}
	return rows, skipped
}

// pushTimes is each build's first recorded push time, or the tick that planned its push.
func pushTimes(recs []queue.Recording) map[queue.SHA]time.Time {
	pushes := map[queue.SHA]time.Time{}
	for _, rec := range recs {
		for _, b := range rec.World.Builds {
			if !b.Pushed.IsZero() && pushes[b.SHA].IsZero() {
				pushes[b.SHA] = b.Pushed
			}
		}
		for _, a := range rec.Actions {
			if a.Do == "push" && pushes[a.Build.SHA].IsZero() {
				pushes[a.Build.SHA] = rec.World.Now
			}
		}
	}
	return pushes
}

func measureLive(recs []queue.Recording, opts liveOptions, landings []liveLanding) liveMeasures {
	rows, skipped := splitLive(recs, landings, opts)
	m := liveMeasures{SplitLandings: len(rows), UnsplitLandings: skipped,
		BusiestHourLandings: busiestHour(landings, opts), BacklogLongestMinutes: longestBacklog(recs, opts).Minutes()}
	planned := map[queue.SHA]bool{}
	for _, rec := range recs {
		for _, a := range rec.Actions {
			if a.Do == "land" {
				planned[a.SHA] = true
			}
		}
	}
	for i := 1; i < len(recs); i++ {
		if trunk := recs[i].World.Trunk; trunk != recs[i-1].World.Trunk && !planned[trunk] && opts.contains(recs[i].World.Now) {
			m.UnplannedTrunkMoves++
		}
	}
	thrown, dropped, stillQueued := map[queue.SHA]bool{}, map[queue.SHA]bool{}, map[headKey]bool{}
	landedHeads, ejectedSinceQueued, pushes := map[headKey]bool{}, map[int]bool{}, pushTimes(recs)
	for _, rec := range recs {
		for _, pr := range rec.World.PRs {
			delete(ejectedSinceQueued, pr.Number)
		}
		for _, a := range rec.Actions {
			switch a.Do {
			case "land":
				for _, member := range a.Members {
					landedHeads[headKey{member.Number, member.Head}] = true
				}
			case "eject":
				ejectedSinceQueued[a.PR] = true
			}
		}
		if !opts.contains(rec.World.Now) {
			continue
		}
		for _, a := range rec.Actions {
			if a.Do != "delete" || planned[a.SHA] {
				continue
			}
			for _, b := range rec.World.Builds {
				if b.SHA != a.SHA || b.Kind != queue.Candidate || len(b.Chain) == 0 {
					continue
				}
				if b.Verdict == queue.Success && thrown[a.SHA] || b.Verdict == queue.Pending && dropped[a.SHA] {
					break
				}
				if b.Verdict == queue.Success {
					thrown[a.SHA] = true
					m.PassingCandidatesThrownAway++
					moved, left := throwReasons(rec.World.PRs, b.Chain)
					if moved {
						m.ThrownAwayHeadMoved++
					}
					if left {
						m.ThrownAwayMemberLeft++
					}
					break
				}
				if b.Verdict == queue.Pending {
					if moved, left := dropReasons(rec.World.PRs, b.Chain, landedHeads, ejectedSinceQueued); moved || left {
						dropped[a.SHA] = true
						m.BuildingCandidatesDropped++
						if moved {
							m.DroppedHeadMoved++
						}
						if left {
							m.DroppedMemberLeft++
						}
						for _, pr := range rec.World.PRs {
							if slices.ContainsFunc(b.Chain, func(member queue.Member) bool { return member.Number == pr.Number && member.Head == pr.Head }) {
								stillQueued[headKey{pr.Number, pr.Head}] = true
							}
						}
						if pushed := cmp.Or(b.Pushed, pushes[b.SHA]); !pushed.IsZero() {
							m.DroppedLongestBuiltMinutes = max(m.DroppedLongestBuiltMinutes, rec.World.Now.Sub(pushed).Minutes())
						}
						break
					}
				}
			}
		}
	}
	m.DroppedMembersStillQueued = len(stillQueued)
	var added, build, total []float64
	for _, r := range rows {
		added, build, total = append(added, r.added), append(build, r.build), append(total, r.total)
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
	reads := readCandidates(recs)
	var firsts []float64
	for _, r := range rows {
		first, minutes := reads.first(r, pushes)
		firsts = append(firsts, minutes)
		if minutes > 30 {
			m.FirstCandidateOver30++
		}
		if r.added <= 30 {
			continue
		}
		red, wasRed := reads.red[first]
		deleted, wasDropped := reads.dropped[first]
		passed, didPass := reads.passed[first]
		building := slices.ContainsFunc(recs[r.landing.record].World.Builds, func(b queue.Build) bool {
			return b.Kind == queue.Candidate && b.SHA == first && b.Verdict == queue.Pending
		})
		switch {
		case minutes > 30:
			m.HeldFirstWaited++
		case wasRed && red <= r.landing.record:
			m.HeldFirstFailed++
		case wasDropped && deleted <= r.landing.record:
			m.HeldFirstDropped++
		case didPass && passed < r.landing.record:
			m.HeldFirstPassed++
		case didPass && passed == r.landing.record || building:
			m.HeldFirstOvertaken++
		default:
			m.HeldFirstOther++
		}
	}
	m.FirstCandidateP90Minutes = quantile(firsts, 90)

	var times []time.Time
	for _, l := range landings {
		times = append(times, l.at)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	times = slices.CompactFunc(times, func(a, b time.Time) bool { return a.Equal(b) })
	type tick struct {
		at    time.Time
		ready bool
	}
	var ticks []tick
	for _, rec := range recs {
		ready := false
		for _, pr := range rec.World.PRs {
			ready = ready || waitsOnTheQueue(pr)
		}
		ticks = append(ticks, tick{rec.World.Now, ready})
	}
	slices.SortStableFunc(ticks, func(a, b tick) int { return a.at.Compare(b.at) })
	wait := func(from, to time.Time) {
		if !to.After(from) || !opts.contains(to) {
			return
		}
		minutes := to.Sub(from).Minutes()
		m.ReadyGaps++
		if m.LongestReadyGapMinutes == nil || minutes >= *m.LongestReadyGapMinutes {
			m.LongestReadyGapMinutes, m.LongestReadyGapFrom, m.LongestReadyGapTo = number(minutes), timeValue(from), timeValue(to)
		}
		if minutes > 90 {
			m.ReadyGapsOver90++
		}
	}
	var start, lastReady, landed time.Time
	waiting, next := false, 0
	land := func(at time.Time) {
		if waiting {
			wait(start, at)
		}
		waiting, landed = false, at
	}
	for _, t := range ticks {
		for ; next < len(times) && !times[next].After(t.at); next++ {
			land(times[next])
		}
		switch {
		case next > 0 && t.at.Equal(times[next-1]):
		case t.ready && !waiting:
			waiting, start = true, landed
			if start.IsZero() {
				start = t.at
			}
			landed, lastReady = time.Time{}, t.at
		case t.ready:
			lastReady = t.at
		case waiting:
			wait(start, lastReady)
			waiting, landed = false, time.Time{}
		default:
			landed = time.Time{}
		}
	}
	for ; next < len(times); next++ {
		land(times[next])
	}

	failed, red := map[queue.SHA]time.Time{}, map[queue.SHA]time.Time{}
	kinds, chains := map[queue.SHA]queue.Kind{}, map[queue.SHA][]queue.Member{}
	var redOrder []queue.SHA
	for _, rec := range recs {
		now := rec.World.Now
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
		if kinds[sha] != queue.Candidate || !opts.contains(at) {
			continue
		}
		m.RedCandidates++
		m.RedPullRequests += len(chains[sha])
		if !failed[sha].IsZero() && !failed[sha].After(at) {
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

// candidateReads holds, for the candidates the records show, which pull requests at which heads
// each chain holds, in the order the records first show them, the first record that read each
// red, the first that read each passing, and the first record that deleted each while the last
// world to show it as a candidate read it pending.
type candidateReads struct {
	holders map[headKey][]queue.SHA
	red     map[queue.SHA]int
	passed  map[queue.SHA]int
	dropped map[queue.SHA]int
}

func readCandidates(recs []queue.Recording) candidateReads {
	c := candidateReads{holders: map[headKey][]queue.SHA{}, red: map[queue.SHA]int{}, passed: map[queue.SHA]int{}, dropped: map[queue.SHA]int{}}
	held := map[headKey]map[queue.SHA]bool{}
	hold := func(b queue.Build) {
		if b.Kind != queue.Candidate {
			return
		}
		for _, m := range b.Chain {
			key := headKey{m.Number, m.Head}
			if held[key] == nil {
				held[key] = map[queue.SHA]bool{}
			}
			if !held[key][b.SHA] {
				held[key][b.SHA] = true
				c.holders[key] = append(c.holders[key], b.SHA)
			}
		}
	}
	pending := map[queue.SHA]bool{}
	for i, rec := range recs {
		read := map[queue.SHA]bool{}
		for _, b := range rec.World.Builds {
			if b.Kind != queue.Candidate {
				continue
			}
			hold(b)
			if _, ok := c.red[b.SHA]; !ok && (b.Verdict == queue.Failure || b.Verdict == queue.Error) {
				c.red[b.SHA] = i
			}
			if _, ok := c.passed[b.SHA]; !ok && b.Verdict == queue.Success {
				c.passed[b.SHA] = i
			}
			pending[b.SHA] = read[b.SHA] && pending[b.SHA] || b.Verdict == queue.Pending
			read[b.SHA] = true
		}
		for _, a := range rec.Actions {
			switch a.Do {
			case "push":
				hold(a.Build)
			case "delete":
				if _, ok := c.dropped[a.SHA]; !ok && pending[a.SHA] {
					c.dropped[a.SHA] = i
				}
			}
		}
	}
	return c
}

// first is a split landing's first candidate: of the candidates whose chain holds the pull
// request at the head that landed, the one pushed earliest and no later than the landing
// candidate, which it is itself when no other was pushed sooner. Its minutes run from ready to
// that push, between 0 and the minutes the queue added.
func (c candidateReads) first(r landingParts, pushes map[queue.SHA]time.Time) (queue.SHA, float64) {
	first, at := r.landing.sha, pushes[r.landing.sha]
	for _, sha := range c.holders[r.landing.member] {
		if pushed := pushes[sha]; !pushed.IsZero() && pushed.Before(at) {
			first, at = sha, pushed
		}
	}
	return first, min(max(at.Sub(r.landing.ready).Minutes(), 0), r.added)
}

func throwReasons(prs []queue.PR, chain []queue.Member) (moved, left bool) {
	heads := map[int]queue.SHA{}
	for _, pr := range prs {
		heads[pr.Number] = pr.Head
	}
	for _, member := range chain {
		head, queued := heads[member.Number]
		moved, left = moved || queued && head != member.Head, left || !queued
	}
	return moved, left
}

// dropReasons is throwReasons for a candidate still building, where a member no longer queued
// counts as left only when it neither landed at that head nor was ejected since it was last queued.
func dropReasons(prs []queue.PR, chain []queue.Member, landed map[headKey]bool, ejected map[int]bool) (moved, left bool) {
	heads := map[int]queue.SHA{}
	for _, pr := range prs {
		heads[pr.Number] = pr.Head
	}
	for _, member := range chain {
		head, queued := heads[member.Number]
		gone := !queued && !landed[headKey{member.Number, member.Head}] && !ejected[member.Number]
		moved, left = moved || queued && head != member.Head, left || gone
	}
	return moved, left
}

func busiestHour(landings []liveLanding, opts liveOptions) int {
	var times []time.Time
	for _, l := range landings {
		if opts.contains(l.at) {
			times = append(times, l.at)
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	most, end := 0, 0
	for i, at := range times {
		for end < len(times) && times[end].Before(at.Add(time.Hour)) {
			end++
		}
		most = max(most, end-i)
	}
	return most
}

func longestBacklog(recs []queue.Recording, opts liveOptions) time.Duration {
	var longest time.Duration
	var first, last time.Time
	standing := false
	for _, rec := range recs {
		if !opts.contains(rec.World.Now) {
			continue
		}
		if standing && rec.World.Now.Sub(last) > 10*time.Minute {
			standing = false
		}
		switch {
		case !backlogged(rec):
			standing = false
		case standing:
			last = rec.World.Now
		default:
			standing, first, last = true, rec.World.Now, rec.World.Now
		}
		if standing {
			longest = max(longest, last.Sub(first))
		}
	}
	return longest
}

// backlogged says that after the record's own actions a pull request that could be built is in
// no pending candidate or push, and every slot is held by a pending candidate or a candidate's
// push. A round's probes and checks run beside the slots, and so does a candidate the engine
// reads as no longer building: one marked landed while landed builds are kept, and one pending
// longer than the timeout, which it builds again.
func backlogged(rec queue.Recording) bool {
	deleted, taken, ejected, notes := map[queue.SHA]bool{}, map[headKey]bool{}, map[int]bool{}, map[int]string{}
	held, started := 0, map[headKey]bool{}
	for _, a := range rec.Actions {
		switch a.Do {
		case "push":
			if a.Build.Kind == queue.Candidate {
				held++
			}
			for _, m := range a.Build.Chain {
				started[headKey{m.Number, m.Head}] = true
			}
		case "delete":
			deleted[a.SHA] = true
		case "land":
			for _, m := range a.Members {
				taken[headKey{m.Number, m.Head}] = true
			}
		case "eject":
			ejected[a.PR] = true
		case "note":
			notes[a.PR] = a.Text
		}
	}
	for _, b := range rec.World.Builds {
		kept := rec.Config.Keep > 0 && !b.Landed.IsZero()
		late := rec.Config.Timeout > 0 && rec.World.Now.Sub(b.Pushed) > rec.Config.Timeout
		if b.Verdict == queue.Pending && b.Kind == queue.Candidate && !deleted[b.SHA] && !kept && !late {
			held++
			for _, m := range b.Chain {
				started[headKey{m.Number, m.Head}] = true
			}
		}
	}
	return rec.Config.Slots > 0 && held >= rec.Config.Slots && slices.ContainsFunc(rec.World.PRs, func(pr queue.PR) bool {
		if note, ok := notes[pr.Number]; ok {
			pr.Note = note
		}
		key := headKey{pr.Number, pr.Head}
		return waitsOnTheQueue(pr) && !started[key] && !taken[key] && !ejected[pr.Number]
	})
}

// waitsOnTheQueue says a recorded pull request could be built: its own checks passed, nothing of
// its own holds it, and the queue's last note on it is not that it conflicts with the trunk, which
// only its author can change.
func waitsOnTheQueue(pr queue.PR) bool {
	return pr.Wait == "" && pr.Checks == queue.Success && pr.Note != trunkConflict
}

const trunkConflict = "waits: it conflicts with the trunk"

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
	fmt.Printf("waits of a ready head with no landing: %d; longest %s min, %s to %s; over 90 min: %d\n",
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

func runLive(dir, clone, like, from, to string, rowOnly bool, perturb string) error {
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
	opts.replays = shape != nil
	sc, win, row, err := scoreLive(dir, clone, opts)
	if err != nil {
		return err
	}
	if perturb != "" && (shape == nil || opts.from == nil || opts.to == nil) {
		return fmt.Errorf("-perturb requires -like, -from and -to")
	}
	win, err = win.perturb(perturb)
	if err != nil {
		return err
	}
	row.Perturbed = perturb
	var simulated *Score
	if shape != nil && win.build > 0 {
		sim, comparison := win.simulate(*shape, sc)
		simulated, row.Simulated = &sim, &comparison
		row.OverLive = &liveComparison{
			Landed: fractionOver(row.Simulated.Landed, number(float64(row.Landed))), WaitP50: fractionOver(row.Simulated.WaitP50, row.WaitP50),
			WaitP90: fractionOver(row.Simulated.WaitP90, row.WaitP90), BuildsPerLanding: fractionOver(row.Simulated.BuildsPerLanding, row.BuildsPerLanding),
		}
		if win.stop > 0 {
			row.OverLive.WaitP50 = fractionOver(row.Simulated.WaitP50Minutes, row.WaitP50Minutes)
			row.OverLive.WaitP90 = fractionOver(row.Simulated.WaitP90Minutes, row.WaitP90Minutes)
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
