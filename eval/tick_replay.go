package main

import (
	"fmt"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type headKey struct {
	number int
	head   queue.SHA
}

// recorded starts from the last snapshot at or before from, preserving its pull
// requests and their live ready clocks. It does not carry running builds.
func (w *window) recorded(recs []queue.Recording, ready map[recordedHead]time.Time, candidates map[queue.SHA]queue.Build, from, to time.Time) {
	start := from
	w.stop = to.Sub(start)
	confirmed := confirmedMembers(recs)
	builds := map[queue.SHA]queue.Build{}
	broken := map[headKey]bool{}
	for _, r := range recs {
		for _, b := range r.World.Builds {
			if len(b.Chain) > len(builds[b.SHA].Chain) {
				builds[b.SHA] = b
			}
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
	for sha, b := range candidates {
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
	firstIndex := 0
	for i, r := range recs {
		if r.World.Now.After(start) {
			break
		}
		firstIndex = i
	}
	previous := map[int]queue.PR{}
	removed := map[recordedHead]bool{}
	planned := map[headKey]bool{}
	conflicted := map[int]queue.SHA{}
	holding := map[int]queue.SHA{}
	var cfg queue.Config
	for i, r := range recs {
		at := max(r.World.Now.Sub(start), 0)
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
				w.withdrawn++
				if planned[headKey{p.Number, p.Head}] {
					w.unconfirmedDepartures++
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
			held := p.Note == "waits: it conflicts with a pull request ahead of it"
			for k, a := range r.Commits {
				var parent, head string
				var n int
				if _, err := fmt.Sscanf(k, "merge %s %d %s", &parent, &n, &head); err == nil && queue.SHA(parent) != r.World.Trunk && n == p.Number && queue.SHA(head) == p.Head && !a.OK {
					held = true
				}
			}
			if was := exists && holding[p.Number] == p.Head; !exists || old.Head != p.Head || held != was {
				ahead := 0
				if held {
					ahead = -1
					w.ignoredHolds++
				}
				add(p.Number, Act{Do: "hold", Ahead: ahead})
			}
			if held {
				holding[p.Number] = p.Head
			} else {
				delete(holding, p.Number)
			}
			if !exists || old.Queued != p.Queued || old.First != p.First {
				q := p.Queued.Sub(start)
				add(p.Number, Act{Do: "label", First: p.First, Queued: &q})
			}
		}
		previous = present
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
