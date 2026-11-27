package main

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type headKey struct {
	number int
	head   queue.SHA
}

// leadIn is how long before its window a replay opens: a few build times, long enough to
// outlast the builds running at the window's start and let the replay's queue settle by then.
const leadIn = 4 * time.Hour

// recorded opens the replay leadIn before from, at the last snapshot at or before then, or at the
// first one read where none is, preserving its pull requests and their live ready clocks. Acts
// are dated from from, the lead-in's before zero, so the queue the window opens on is the one
// the replay built, builds in flight included. Where no record is before from, the replay opens
// at zero.
func (w *window) recorded(recs []queue.Recording, ready map[recordedHead]time.Time, candidates map[queue.SHA]queue.Build, from, to time.Time, clone string) error {
	start := from
	w.stop = to.Sub(start)
	confirmed := confirmedMembers(recs)
	builds := map[queue.SHA]queue.Build{}
	green, red := map[queue.SHA]bool{}, map[queue.SHA]bool{}
	broken := map[headKey]bool{}
	for _, r := range recs {
		for _, b := range r.World.Builds {
			if len(b.Chain) > len(builds[b.SHA].Chain) {
				builds[b.SHA] = b
			}
			green[b.SHA] = green[b.SHA] || b.Verdict == queue.Success
			red[b.SHA] = red[b.SHA] || b.Verdict == queue.Failure
		}
		for _, a := range r.Actions {
			if a.Do == "push" && len(a.Build.Chain) > len(builds[a.Build.SHA].Chain) {
				builds[a.Build.SHA] = a.Build
			}
		}
	}
	for _, r := range recs {
		for _, a := range r.Actions {
			if a.Do != "eject" || a.Cause != "culprit" {
				continue
			}
			tied := false
			for _, sha := range a.Evidence {
				for _, m := range builds[sha].Chain {
					if m.Number == a.PR {
						broken[headKey{m.Number, m.Head}] = true
						tied = true
					}
				}
			}
			if !tied && !r.World.Now.Before(from) && r.World.Now.Before(to) {
				w.unattributed++
			}
		}
	}
	spans, theirs := redTrunks(recs)
	for sha := range theirs {
		delete(red, sha)
	}
	left := brokeAndLeft(recs, builds, green, red, broken, confirmed)
	for k := range left {
		broken[k] = true
	}
	firstIndex, windowIndex := 0, 0
	for i, r := range recs {
		if r.World.Now.After(start) {
			break
		}
		if windowIndex = i; !r.World.Now.After(start.Add(-leadIn)) {
			firstIndex = i
		}
	}
	opens := min(max(recs[firstIndex].World.Now.Sub(start), -leadIn), 0)
	w.episodes, w.unplayed = nil, 0
	for _, span := range spans {
		at, until := max(span.from.Sub(start), opens), span.until.Sub(start)
		switch {
		case span.fixes == nil && until > 0 && at < w.stop:
			w.unplayed++
		case span.fixes != nil && until > at && at < w.stop:
			w.episodes = append(w.episodes, Episode{At: at, NeedsFix: true, Fixes: span.fixes})
		}
	}
	w.times, w.failing = nil, map[queue.SHA][]time.Duration{}
	for _, b := range w.failedBefore {
		if b.answered.Before(recs[firstIndex].World.Now) {
			continue
		}
		for _, m := range builds[b.sha].Chain {
			if broken[headKey{m.Number, m.Head}] {
				w.failing[m.Head] = append(w.failing[m.Head], b.took)
			}
		}
	}
	for _, b := range w.timed {
		guilty := false
		for _, m := range builds[b.sha].Chain {
			if b.failed && broken[headKey{m.Number, m.Head}] {
				w.failing[m.Head], guilty = append(w.failing[m.Head], b.took), true
			}
		}
		if !guilty {
			w.times = append(w.times, b.took)
		}
	}
	for sha, b := range candidates {
		if theirs[sha] {
			continue
		}
		if len(builds[sha].Chain) > len(b.Chain) {
			b.Chain = builds[sha].Chain
		}
		blamed := false
		for _, m := range b.Chain {
			blamed = blamed || broken[headKey{m.Number, m.Head}]
		}
		if !blamed {
			w.candidatesWithoutCulprit++
			if b.Verdict == queue.Failure {
				w.unblamedRedCandidates++
			}
		}
	}
	if w.candidatesWithoutCulprit > 0 {
		w.flake = float64(w.unblamedRedCandidates) / float64(w.candidatesWithoutCulprit)
	}
	answered := map[queue.SHA]bool{}
	if clone != "" {
		beside := map[queue.SHA]map[queue.SHA]bool{}
		inWindow := recs[firstIndex:]
		for i, r := range inWindow {
			if r.World.Now.Sub(start) >= w.stop {
				inWindow = inWindow[:i]
				break
			}
			for _, p := range r.World.PRs {
				if heldBehind(r, p) {
					beside[p.Head] = map[queue.SHA]bool{}
				}
			}
		}
		for _, r := range inWindow {
			for _, p := range r.World.PRs {
				if others := beside[p.Head]; others != nil {
					for _, o := range r.World.PRs {
						if o.Number != p.Number {
							others[o.Head] = true
						}
					}
				}
			}
		}
		clashing, ok, err := clashingHeads(clone, beside)
		if err != nil {
			return err
		}
		pairs := pairsIn(clashing)
		w.clashing, w.clashingPairs, answered = clashing, &pairs, ok
		for _, p := range recs[firstIndex].World.PRs {
			if heldBehind(recs[firstIndex], p) {
				delete(answered, p.Head)
			}
		}
	}
	previous := map[int]queue.PR{}
	removed := map[recordedHead]bool{}
	planned := map[headKey]bool{}
	conflicted := map[int]queue.SHA{}
	holding := map[int]queue.SHA{}
	var cfg queue.Config
	for i, r := range recs {
		at := max(r.World.Now.Sub(start), -leadIn)
		add := func(n int, a Act) { a.At, a.Number = at, n; w.acts = append(w.acts, a) }
		present := map[int]queue.PR{}
		for _, p := range r.World.PRs {
			present[p.Number] = p
		}
		for _, a := range r.Actions {
			if a.Do == "land" {
				for _, m := range a.Members {
					planned[headKey{m.Number, m.Head}] = true
				}
			}
			if a.Do == "eject" {
				p, ok := present[a.PR]
				if !ok {
					p, ok = previous[a.PR]
				}
				if ok {
					removed[recordedHead{p.Number, p.Head, p.Queued}] = true
				}
			}
		}
		for _, m := range confirmed[i] {
			delete(planned, headKey{m.Number, m.Head})
			p, ok := present[m.Number]
			if !ok {
				p, ok = previous[m.Number]
			}
			if ok && p.Head == m.Head {
				removed[recordedHead{p.Number, p.Head, p.Queued}] = true
			}
		}
		if i < firstIndex {
			previous = present
			continue
		}
		if at >= w.stop {
			break
		}
		if i == firstIndex {
			previous = map[int]queue.PR{}
			w.cfg = r.Config
		}
		if i == firstIndex || cfg != r.Config {
			c := r.Config
			add(0, Act{Do: "settings", Config: &c})
			cfg = c
		}
		for _, p := range recs[max(i-1, 0)].World.PRs {
			if _, existed := previous[p.Number]; !existed {
				continue
			}
			if _, ok := present[p.Number]; !ok && !removed[recordedHead{p.Number, p.Head, p.Queued}] {
				add(p.Number, Act{Do: "unlabel"})
				if i > windowIndex {
					w.withdrawn++
					if planned[headKey{p.Number, p.Head}] {
						w.unconfirmedDepartures++
					}
				}
			}
		}
		for _, p := range r.World.PRs {
			old, exists := previous[p.Number]
			key, h := headKey{p.Number, p.Head}, recordedHead{p.Number, p.Head, p.Queued}
			var since *time.Duration
			if !ready[h].IsZero() {
				d := ready[h].Sub(start)
				since = &d
			}
			if !exists || old.Head != p.Head {
				add(p.Number, Act{Do: "head", Head: p.Head, Breaks: broken[key]})
			}
			if !exists || old.Head != p.Head || old.Checks != p.Checks || old.Queued != p.Queued {
				add(p.Number, Act{Do: "checks", Head: p.Head, Verdict: p.Checks, Ready: since})
			}
			if !exists || old.Head != p.Head || old.Wait != p.Wait || old.Queued != p.Queued {
				add(p.Number, Act{Do: "wait", Wait: p.Wait, Ready: since})
			}
			conflict := p.Note == trunkConflict
			for k, a := range r.Commits {
				var parent, head string
				var n int
				if _, err := fmt.Sscanf(k, "merge %s %d %s", &parent, &n, &head); err == nil && queue.SHA(parent) == r.World.Trunk && n == p.Number && queue.SHA(head) == p.Head {
					conflict = !a.OK
				}
			}
			if conflict || conflicted[p.Number] == p.Head {
				if !exists || old.Head != p.Head || conflicted[p.Number] != p.Head {
					add(p.Number, Act{Do: "conflict"})
				}
				conflicted[p.Number] = p.Head
			} else {
				delete(conflicted, p.Number)
			}
			held := heldBehind(r, p) && !answered[p.Head]
			changed := !exists || old.Head != p.Head || held != (exists && holding[p.Number] == p.Head)
			if changed {
				ahead := 0
				if held {
					ahead = -1
				}
				add(p.Number, Act{Do: "hold", Ahead: ahead})
			}
			if held && (i == windowIndex || i > windowIndex && changed) {
				w.ignoredHolds++
			}
			if held {
				holding[p.Number] = p.Head
			} else {
				delete(holding, p.Number)
			}
			if !exists || old.Queued != p.Queued || old.First != p.First || old.Head != p.Head && broken[headKey{p.Number, old.Head}] {
				q := p.Queued.Sub(start)
				add(p.Number, Act{Do: "label", First: p.First, Queued: &q})
			}
		}
		previous = present
	}
	for k := range left {
		if slices.ContainsFunc(w.acts, func(a Act) bool { return a.Do == "head" && a.Number == k.number && a.Head == k.head }) {
			w.unejectedGuilty++
		}
	}

	// Population follows recorded labels and readiness, including repeated queueings.
	active := map[int]queue.PR{}
	settings := map[queue.Config]bool{}
	for i, r := range recs {
		if !r.World.Now.Before(to) {
			break
		}
		active = map[int]queue.PR{}
		for _, p := range queuedAfter(r, confirmed[i]) {
			active[p.Number] = p
		}
		if !r.World.Now.Before(from) {
			settings[r.Config] = true
		}
	}
	for _, p := range active {
		if waitsOnTheQueue(p) {
			w.left++
		}
	}
	w.settings = len(settings)
	counted := map[recordedHead]bool{}
	for _, r := range recs {
		if !r.World.Now.Before(to) {
			break
		}
		for _, p := range r.World.PRs {
			h := recordedHead{p.Number, p.Head, p.Queued}
			at := ready[h]
			if at.IsZero() || !at.Before(to) || counted[h] {
				continue
			}
			if at.Before(from) {
				continue
			}
			counted[h] = true
			w.arrivals = append(w.arrivals, at.Sub(from))
		}
	}
	var opening []queue.PR
	for i, r := range recs {
		if r.World.Now.After(from) {
			break
		}
		opening = queuedAfter(r, confirmed[i])
	}
	for _, p := range opening {
		h := recordedHead{p.Number, p.Head, p.Queued}
		if waitsOnTheQueue(p) && !ready[h].IsZero() && !counted[h] {
			counted[h] = true
			w.opening++
			w.arrivals = append(w.arrivals, 0)
		}
	}
	return nil
}

// brokeAndLeft is each head, beside those ejected as the cause, that the records hold to have
// broken the build: a record queued it, no build of any kind that held it was ever read green,
// no landing of it was confirmed, two or more whole builds that held it were read red with no
// ejected culprit among their members, and a later record no longer queues it at that head.
//
// red holds no build of a red trunk, which is no evidence against the heads it held.
//
// It leaves two things out. A head with a single failed build is never held guilty, even one
// that failed alone. And it cannot tell a head from what it failed beside unless the record
// ejected the other, so two heads failing in the same builds are both held guilty.
//
// The replay labels the pull request of any guilty head, these and those the record ejected,
// again at the next head the record queues it at, as the replay's queue may have ejected it
// there.
func brokeAndLeft(recs []queue.Recording, builds map[queue.SHA]queue.Build, green, red map[queue.SHA]bool, ejected map[headKey]bool, confirmed [][]queue.Member) map[headKey]bool {
	passed, landed, failed := map[headKey]bool{}, map[headKey]bool{}, map[headKey]int{}
	for _, members := range confirmed {
		for _, m := range members {
			landed[headKey{m.Number, m.Head}] = true
		}
	}
	for sha, b := range builds {
		evidence := b.Kind != queue.Probe && red[sha] &&
			!slices.ContainsFunc(b.Chain, func(m queue.Member) bool { return ejected[headKey{m.Number, m.Head}] })
		for _, m := range b.Chain {
			k := headKey{m.Number, m.Head}
			passed[k] = passed[k] || green[sha]
			if evidence {
				failed[k]++
			}
		}
	}
	last := map[headKey]int{}
	for i, r := range recs {
		for _, p := range r.World.PRs {
			last[headKey{p.Number, p.Head}] = i
		}
	}
	guilty := map[headKey]bool{}
	for k, n := range failed {
		if i, queued := last[k]; n >= 2 && !passed[k] && !landed[k] && !ejected[k] && queued && i+1 < len(recs) {
			guilty[k] = true
		}
	}
	return guilty
}

// redTrunk is a span in which the records read the trunk at one commit failing for its own
// reason. Where fixes is set the span ends when one of them lands, which until gives as the
// record that planned a landing the records confirmed, or the last record where none was.
type redTrunk struct {
	from, until time.Time
	fixes       []int
}

// redTrunks is each span in which the records read the trunk itself red, with the builds that are
// its, each true where a fix ends its span. A build is read green on a base where a record reads it green standing on it, and on the
// base it was pushed on. The trunk at T is red where two probes of the bare trunk on T were read
// red and no build read green on T was pushed between their pushes. It is red from the record
// that made T the trunk, or from the push of the last build read green on T pushed before the
// first of those probes where that is later. Its end is the first build read green on T pushed
// after the first of those probes: a candidate holding pull requests that go first in the record
// that pushed it is their fix, and the span ends when one of them lands; any other ends it at
// its push. Where no build on T was read green the span runs to the last record. A build is the
// trunk's where the record that first read it red stands it on T: one ended by its push where it
// was pushed before that push, one ended by its fix where it holds none of the fixes and was
// pushed before the fix landed. The records rewrite a build's base as the trunk moves, so one
// pushed before T landed can stand on it by then.
func redTrunks(recs []queue.Recording) ([]redTrunk, map[queue.SHA]bool) {
	pushes := pushTimes(recs)
	confirmed := confirmedMembers(recs)
	planned, became := map[queue.SHA]time.Time{}, map[queue.SHA]time.Time{}
	var trunks []queue.SHA
	failed := map[queue.SHA]queue.Build{}
	passedOn := map[queue.SHA]map[queue.SHA]bool{}
	pushedIn := map[queue.SHA]int{}
	pushedOn, shownOn := map[queue.SHA]queue.SHA{}, map[queue.SHA]queue.SHA{}
	pass := func(base, sha queue.SHA) {
		if sha != base {
			if passedOn[base] == nil {
				passedOn[base] = map[queue.SHA]bool{}
			}
			passedOn[base][sha] = true
		}
	}
	var greens []queue.SHA
	for i, r := range recs {
		if _, ok := became[r.World.Trunk]; !ok {
			became[r.World.Trunk] = cmp.Or(planned[r.World.Trunk], r.World.Now)
			trunks = append(trunks, r.World.Trunk)
		}
		for _, b := range r.World.Builds {
			if _, ok := failed[b.SHA]; !ok && b.Verdict == queue.Failure {
				failed[b.SHA] = b
			}
			if _, ok := shownOn[b.SHA]; !ok {
				shownOn[b.SHA] = b.Base
			}
			if b.Verdict == queue.Success {
				pass(b.Base, b.SHA)
				greens = append(greens, b.SHA)
			}
		}
		for _, a := range r.Actions {
			if _, ok := became[a.SHA]; a.Do == "land" && !ok {
				planned[a.SHA] = r.World.Now
			}
			if _, ok := pushedIn[a.Build.SHA]; a.Do == "push" && !ok {
				pushedIn[a.Build.SHA], pushedOn[a.Build.SHA] = i, a.Build.Base
			}
		}
	}
	for _, sha := range greens {
		if base, ok := pushedOn[sha]; ok {
			pass(base, sha)
		} else {
			pass(shownOn[sha], sha)
		}
	}
	var spans []redTrunk
	theirs := map[queue.SHA]bool{}
	for _, t := range trunks {
		var probes []queue.SHA
		for sha, b := range failed {
			if b.Kind == queue.Probe && len(b.Chain) == 0 && b.Base == t {
				probes = append(probes, sha)
			}
		}
		byPush := func(a, b queue.SHA) int { return cmp.Or(pushes[a].Compare(pushes[b]), cmp.Compare(a, b)) }
		slices.SortFunc(probes, byPush)
		passed := func(after, before time.Time) bool {
			for sha := range passedOn[t] {
				if pushes[sha].After(after) && pushes[sha].Before(before) {
					return true
				}
			}
			return false
		}
		for i := 1; i < len(probes); i++ {
			first := pushes[probes[i-1]]
			if passed(first, pushes[probes[i]]) {
				continue
			}
			last := recs[len(recs)-1].World.Now
			span, end := redTrunk{from: became[t], until: last}, last
			var ends []queue.SHA
			for sha := range passedOn[t] {
				if pushes[sha].After(first) && pushes[sha].Before(last) {
					ends = append(ends, sha)
				}
				if pushes[sha].Before(first) && pushes[sha].After(span.from) {
					span.from = pushes[sha]
				}
			}
			if len(ends) > 0 {
				g := slices.MinFunc(ends, byPush)
				end, span.until = pushes[g], pushes[g]
				if at, ok := pushedIn[g]; ok {
					span.fixes = fixesIn(recs, at, g)
					if span.fixes != nil {
						span.until = landedAfter(recs, confirmed, at, span.fixes)
						end = span.until
					}
				}
			}
			spans = append(spans, span)
			for sha, b := range failed {
				fixed := slices.ContainsFunc(b.Chain, func(m queue.Member) bool { return slices.Contains(span.fixes, m.Number) })
				if b.Base == t && pushes[sha].Before(end) && !fixed {
					theirs[sha] = span.fixes != nil
				}
			}
			break
		}
	}
	return spans, theirs
}

// fixesIn is the pull requests going first in the record at that index that the candidate it
// pushes there holds.
func fixesIn(recs []queue.Recording, at int, sha queue.SHA) []int {
	var fixes []int
	for _, a := range recs[at].Actions {
		if a.Do == "push" && a.Build.SHA == sha && a.Build.Kind == queue.Candidate {
			for _, m := range a.Build.Chain {
				if i := slices.IndexFunc(recs[at].World.PRs, func(p queue.PR) bool { return p.Number == m.Number }); i >= 0 && recs[at].World.PRs[i].First {
					fixes = append(fixes, m.Number)
				}
			}
		}
	}
	return fixes
}

// landedAfter is the record from the one at that index on that planned the first landing of any of
// those pull requests the trunk's next observation confirmed, or the last record where none was.
func landedAfter(recs []queue.Recording, confirmed [][]queue.Member, at int, numbers []int) time.Time {
	planned := map[queue.SHA]time.Time{}
	for i, r := range recs[at:] {
		if slices.ContainsFunc(confirmed[at+i], func(m queue.Member) bool { return slices.Contains(numbers, m.Number) }) {
			return cmp.Or(planned[r.World.Trunk], r.World.Now)
		}
		for _, a := range r.Actions {
			if a.Do == "land" {
				planned[a.SHA] = r.World.Now
			}
		}
	}
	return recs[len(recs)-1].World.Now
}

// heldBehind says the record shows this head refused onto a chain of pull requests ahead of it.
func heldBehind(r queue.Recording, p queue.PR) bool {
	held := p.Note == "waits: it conflicts with a pull request ahead of it"
	for k, a := range r.Commits {
		var parent, head string
		var n int
		if _, err := fmt.Sscanf(k, "merge %s %d %s", &parent, &n, &head); err == nil && queue.SHA(parent) != r.World.Trunk && n == p.Number && queue.SHA(head) == p.Head && !a.OK {
			held = true
		}
	}
	return held
}

// confirmedMembers reports landings only once the trunk observation confirms them.
func confirmedMembers(recs []queue.Recording) [][]queue.Member {
	confirmed := make([][]queue.Member, len(recs))
	pending := map[queue.SHA][]queue.Member{}
	seen := map[queue.SHA]bool{}
	for i, r := range recs {
		for _, a := range r.Actions {
			if a.Do == "land" {
				pending[a.SHA] = a.Members
			}
		}
		if members := pending[r.World.Trunk]; !seen[r.World.Trunk] && len(members) > 0 {
			confirmed[i] = members
			seen[r.World.Trunk] = true
		}
	}
	return confirmed
}

// queuedAfter retains the labels that the recorded queue did not remove at this tick.
func queuedAfter(r queue.Recording, landed []queue.Member) []queue.PR {
	removed := map[int]bool{}
	for _, a := range r.Actions {
		if a.Do == "eject" {
			removed[a.PR] = true
		}
	}
	for _, m := range landed {
		for _, p := range r.World.PRs {
			if p.Number == m.Number && p.Head == m.Head {
				removed[p.Number] = true
			}
		}
	}
	var prs []queue.PR
	for _, p := range r.World.PRs {
		if !removed[p.Number] {
			prs = append(prs, p)
		}
	}
	return prs
}
