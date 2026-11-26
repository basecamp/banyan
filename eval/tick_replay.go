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
	w.episodes = nil
	for _, span := range spans {
		if at, until := max(span.from.Sub(start), opens), span.until.Sub(start); until > at && at < w.stop {
			w.episodes = append(w.episodes, Episode{At: at, For: until - at})
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
// reason.
type redTrunk struct {
	from, until time.Time
}

// redTrunks is each span in which the records read the trunk itself red, with the builds that are
// its. The trunk at T is red where two probes of the bare trunk on T were read red and no build
// on T pushed between their pushes was read green. It is red from the record that made T the
// trunk until the push of the first build on T read green that was pushed after the first of
// those probes, or the last record where none was. A build is the trunk's where the record that
// first read it red stands it on T, and it was pushed before the span ends: the records rewrite
// a build's base as the trunk moves, so one pushed before T landed can stand on it by then.
func redTrunks(recs []queue.Recording) ([]redTrunk, map[queue.SHA]bool) {
	pushes := pushTimes(recs)
	planned, became := map[queue.SHA]time.Time{}, map[queue.SHA]time.Time{}
	var trunks []queue.SHA
	failed := map[queue.SHA]queue.Build{}
	passedOn := map[queue.SHA]map[queue.SHA]bool{}
	for _, r := range recs {
		if _, ok := became[r.World.Trunk]; !ok {
			became[r.World.Trunk] = cmp.Or(planned[r.World.Trunk], r.World.Now)
			trunks = append(trunks, r.World.Trunk)
		}
		for _, b := range r.World.Builds {
			if _, ok := failed[b.SHA]; !ok && b.Verdict == queue.Failure {
				failed[b.SHA] = b
			}
			if b.Verdict == queue.Success && b.SHA != b.Base {
				if passedOn[b.Base] == nil {
					passedOn[b.Base] = map[queue.SHA]bool{}
				}
				passedOn[b.Base][b.SHA] = true
			}
		}
		for _, a := range r.Actions {
			if _, ok := became[a.SHA]; a.Do == "land" && !ok {
				planned[a.SHA] = r.World.Now
			}
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
		slices.SortFunc(probes, func(a, b queue.SHA) int { return cmp.Or(pushes[a].Compare(pushes[b]), cmp.Compare(a, b)) })
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
			until := recs[len(recs)-1].World.Now
			for sha := range passedOn[t] {
				if pushes[sha].After(first) && pushes[sha].Before(until) {
					until = pushes[sha]
				}
			}
			spans = append(spans, redTrunk{became[t], until})
			for sha, b := range failed {
				if b.Base == t && pushes[sha].Before(until) {
					theirs[sha] = true
				}
			}
			break
		}
	}
	return spans, theirs
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
