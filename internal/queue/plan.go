package queue

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Plan is one tick: the actions this world calls for, in the order to take them.
//
// The chain is the admitted pull requests merged one onto the next on the trunk's tip. A
// prefix of it is the trunk plus its first j pull requests; prefix 0 is the tip alone.
//
//   - The highest prefix whose whole build passed lands, with everything under it.
//   - A red candidate is narrowed by rounds of builds of its prefixes, begun together. A pull
//     request leaves only when, in one round, the prefix below it passed and its own failed,
//     and its own has failed twice.
//   - A round in which nothing passed, the tip included, is a red trunk: nobody is blamed.
func Plan(w World, c Config) []Action {
	p := &plan{w: w, c: c, open: map[int]PR{}, waits: map[int]string{}}
	p.admit()
	p.read()
	if p.land() {
		return p.acts
	}
	p.retry()
	if !p.blame() {
		p.fill()
	}
	p.notes()
	return p.acts
}

type plan struct {
	w       World
	c       Config
	acts    []Action
	open    map[int]PR     // admitted pull requests, by number
	order   []PR           // admitted, in the order they join the chain
	waits   map[int]string // why a queued pull request stays out of the chain
	chain   []Member       // the chain above the trunk
	builds  []Build        // live builds on the chain, each standing on the trunk
	pending int
}

func (p *plan) act(a Action) { p.acts = append(p.acts, a) }

// admit sorts the queue into those that may join the chain, those that wait in place, and
// the two that leave: a head that moved since it was queued, and a conflict with the trunk.
func (p *plan) admit() {
	prs := slices.Clone(p.w.PRs)
	sort.SliceStable(prs, func(i, j int) bool {
		a, b := prs[i], prs[j]
		if a.First != b.First {
			return a.First
		}
		if !a.Queued.Equal(b.Queued) {
			return a.Queued.Before(b.Queued)
		}
		return a.Number < b.Number
	})
	for _, pr := range prs {
		switch {
		case pr.Pinned == "":
			p.act(Action{Do: "pin", PR: pr.Number})
			p.waits[pr.Number] = "queued"
		case pr.Pinned != pr.Head:
			p.act(Action{Do: "eject", PR: pr.Number, Cause: "moved",
				Text: fmt.Sprintf("its head moved from %.12s to %.12s after it was queued", pr.Pinned, pr.Head)})
		case pr.Hold:
			p.waits[pr.Number] = "held by its label"
		case pr.Draft:
			p.waits[pr.Number] = "it is a draft"
		case pr.Stacked:
			p.waits[pr.Number] = "its base is not the trunk"
		case pr.Checks == Pending:
			p.waits[pr.Number] = "its own checks are running"
		case pr.Checks != Success:
			p.waits[pr.Number] = "its own checks failed"
		default:
			if _, ok := p.w.Repo.Merge(p.w.Trunk, pr, ""); !ok {
				p.act(Action{Do: "eject", PR: pr.Number, Cause: "conflict", Text: "it conflicts with the trunk"})
				continue
			}
			p.open[pr.Number] = pr
			p.order = append(p.order, pr)
		}
	}
}

// read keeps the builds that still stand on the trunk and hold only admitted heads, takes the
// longest stacked candidate as the chain, and deletes every other branch.
func (p *plan) read() {
	var live []Build
	for _, b := range p.w.Builds {
		rest, ok := b.above(p.w.Trunk)
		for _, m := range rest {
			if pr, in := p.open[m.Number]; !in || pr.Head != m.Head {
				ok = false
			}
		}
		if !ok {
			p.act(Action{Do: "delete", SHA: b.SHA})
			continue
		}
		b.Base, b.Chain = p.w.Trunk, rest
		if b.Verdict == Pending && p.c.Timeout > 0 && p.w.Now.Sub(b.Pushed) > p.c.Timeout {
			b.Verdict = Error
		}
		if b.Kind == Candidate && b.Round == 0 && len(rest) > len(p.chain) {
			p.chain = slices.Clone(rest)
		}
		live = append(live, b)
	}
	of := ""
	if ks := p.stacked(live); len(ks) > 0 {
		of = p.chain[ks[0]-1].Merge
	}
	for _, b := range live {
		on := len(b.Chain) <= len(p.chain)
		for i := 0; on && i < len(b.Chain); i++ {
			on = b.Chain[i].Merge == p.chain[i].Merge
		}
		if !on || (b.Round > 0 && b.Of != of) {
			p.act(Action{Do: "delete", SHA: b.SHA})
			continue
		}
		if b.Verdict == Pending {
			p.pending++
		}
		p.builds = append(p.builds, b)
	}
}

// stacked is the lengths of the prefixes that hold a stacked candidate, lowest first.
func (p *plan) stacked(builds []Build) []int {
	var ks []int
	for _, b := range builds {
		if b.Kind == Candidate && b.Round == 0 && !slices.Contains(ks, len(b.Chain)) {
			ks = append(ks, len(b.Chain))
		}
	}
	sort.Ints(ks)
	return ks
}

// at is the candidate builds of prefix k, oldest first, and whether one passed.
func (p *plan) at(k int) (all []Build, passed bool) {
	for _, b := range p.builds {
		if b.Kind == Candidate && len(b.Chain) == k {
			all = append(all, b)
			passed = passed || b.Verdict == Success
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Pushed.Before(all[j].Pushed) })
	return all, passed
}

// last is the newest stacked attempt of prefix k.
func (p *plan) last(k int) (Build, bool) {
	all, _ := p.at(k)
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Round == 0 {
			return all[i], true
		}
	}
	return Build{}, false
}

// red says prefix k's newest stacked attempt failed and no build of it passed.
func (p *plan) red(k int) bool {
	_, passed := p.at(k)
	b, ok := p.last(k)
	return ok && !passed && b.Verdict == Failure
}

func (p *plan) prefix(j int) SHA {
	if j == 0 {
		return p.w.Trunk
	}
	return p.chain[j-1].Merge
}

// land moves the trunk to the highest prefix whose whole build passed.
func (p *plan) land() bool {
	var best *Build
	for i, b := range p.builds {
		if b.Kind == Candidate && b.Verdict == Success && (best == nil || len(b.Chain) > len(best.Chain)) {
			best = &p.builds[i]
		}
	}
	if best == nil {
		return false
	}
	p.act(Action{Do: "land", SHA: best.SHA, Members: best.Chain})
	return true
}

// fresh is the first commit of a numbered series that the queue has never pushed, so no SHA
// is built twice and an old verdict never answers for a new build.
func (p *plan) fresh(commit func(n int) SHA) SHA {
	for n := 1; ; n++ {
		if sha := commit(n); !p.w.Repo.Seen(sha) {
			return sha
		}
	}
}

// candidate is a whole build of prefix j that may land: the chain's own merge the first time,
// and after that the same merge carrying a try number.
func (p *plan) candidate(j, round int, of SHA) Build {
	b := Build{Kind: Candidate, Base: p.w.Trunk, Chain: slices.Clone(p.chain[:j]), Round: round, Of: of}
	m := p.chain[j-1]
	b.SHA = p.fresh(func(n int) SHA {
		if n == 1 && round == 0 {
			return m.Merge
		}
		sha, _ := p.w.Repo.Merge(p.prefix(j-1), p.open[m.Number], note(Candidate, round, of, "", n))
		return sha
	})
	return b
}

func (p *plan) push(b Build) {
	b.Pushed = p.w.Now
	p.act(Action{Do: "push", Build: b})
	p.pending++
}

// retry builds again a stacked candidate whose newest attempt ended with no verdict.
func (p *plan) retry() {
	for _, k := range p.stacked(p.builds) {
		b, _ := p.last(k)
		if _, passed := p.at(k); !passed && b.Verdict == Error && p.w.Now.Sub(b.Pushed) >= p.c.Recheck {
			p.push(p.candidate(k, 0, ""))
		}
	}
}

// blame works on the lowest stacked candidate when it is red, and says whether it is.
func (p *plan) blame() bool {
	ks := p.stacked(p.builds)
	if len(ks) == 0 || !p.red(ks[0]) {
		return false
	}
	k := ks[0]
	of := p.chain[k-1].Merge
	all, _ := p.at(k)
	fails := map[int]int{}
	redSHA := ""
	for _, b := range all {
		if b.Round == 0 && b.Verdict == Failure {
			fails[k]++
			redSHA = b.SHA
		}
	}
	// After three whole reds that probes called flakes, the probes are missing something.
	whole := !p.c.Probes || fails[k] >= 3

	rounds := map[int][]Build{}
	last := 0
	for _, b := range p.builds {
		if b.Round > 0 {
			rounds[b.Round] = append(rounds[b.Round], b)
			last = max(last, b.Round)
		}
	}
	lo, hi := 0, k
	var confirmed, held, flake, void bool
	var pushed time.Time
	var proof []SHA
	for r := 1; r <= last; r++ {
		g, low := -1, -1
		void = false
		var lowSHA, gSHA SHA
		for _, b := range rounds[r] {
			if b.Pushed.After(pushed) {
				pushed = b.Pushed
			}
			switch j := len(b.Chain); b.Verdict {
			case Pending:
				return true
			case Error:
				void = true
			case Success:
				if j > g {
					g, gSHA = j, b.SHA
				}
			}
		}
		if void || len(rounds[r]) == 0 {
			continue
		}
		if g < 0 {
			held, flake, confirmed, lo, hi = true, false, false, 0, k
			continue
		}
		held, flake = false, g >= k
		if flake {
			continue
		}
		for _, b := range rounds[r] {
			if j := len(b.Chain); b.Verdict == Failure && j > g && (low < 0 || j < low) {
				low, lowSHA = j, b.SHA
			}
		}
		lo = g
		if confirmed = low > 0; confirmed {
			hi = low
			fails[hi]++
			proof = []SHA{gSHA, lowSHA}
		} else if hi <= lo {
			hi = k
		}
	}

	switch {
	case flake:
		// The probes passed on the red tree itself. Build it whole again, on fresh evidence.
		for _, b := range p.builds {
			if b.Round > 0 {
				p.act(Action{Do: "delete", SHA: b.SHA})
			}
		}
		p.push(p.candidate(k, 0, ""))
		return true
	case (held || void) && p.w.Now.Sub(pushed) < p.c.Recheck:
		return true
	case held:
		if p.fixWaits() {
			return true
		}
	case hi == lo+1 && confirmed && fails[hi] >= 2:
		m := p.chain[hi-1]
		p.act(Action{Do: "eject", PR: m.Number, Cause: "culprit", Evidence: append(proof, redSHA),
			Text: fmt.Sprintf("the build of the queue up to it failed twice, and the build without it passed (%.12s)", proof[0])})
		return true
	}

	var points []int
	switch span := hi - lo; {
	case held:
		// A red trunk: the tip, the top, and the prefix that ends at a pull request labelled
		// as its fix, which is where a green would show.
		points = []int{0, k}
		for j := k - 1; j > 0; j-- {
			if p.open[p.chain[j-1].Number].First {
				points = []int{0, j, k}
				break
			}
		}
	case span == 1:
		points = []int{lo, hi}
	default:
		points = []int{lo}
		n := min(max(p.c.Slots, 2)-1, span-1)
		for i := 1; i <= n; i++ {
			points = append(points, lo+i*span/(n+1))
		}
	}
	for _, j := range points {
		p.push(p.evidence(j, last+1, of, redSHA, whole))
	}
	return true
}

// evidence is a round's build of prefix j: a probe where the repository offers them, else a
// whole build, which for a prefix above the tip is a candidate that lands if it passes.
func (p *plan) evidence(j, round int, of, red SHA, whole bool) Build {
	if whole && j > 0 {
		return p.candidate(j, round, of)
	}
	b := Build{Kind: Probe, Base: p.w.Trunk, Chain: slices.Clone(p.chain[:j]), Round: round, Of: of}
	if whole {
		b.Kind, red = Check, ""
	}
	b.SHA = p.fresh(func(n int) SHA { return p.w.Repo.Mark(p.prefix(j), note(b.Kind, round, of, red, n)) })
	return b
}

// fixWaits drops the chain while the trunk is red and a pull request labelled first waits
// outside it, so the next tick builds the fix on the tip, ahead of everything.
func (p *plan) fixWaits() bool {
	in := map[int]bool{}
	for _, m := range p.chain {
		in[m.Number] = true
	}
	for _, pr := range p.order {
		if pr.First && !in[pr.Number] {
			for _, b := range p.builds {
				p.act(Action{Do: "delete", SHA: b.SHA})
			}
			return true
		}
	}
	return false
}

// fill stacks new candidates on the chain while slots are free and none of it is red.
func (p *plan) fill() {
	in := map[int]bool{}
	for _, m := range p.chain {
		in[m.Number] = true
	}
	for _, k := range p.stacked(p.builds) {
		if p.red(k) {
			return
		}
	}
	for p.pending < p.c.Slots {
		parent, added := p.prefix(len(p.chain)), 0
		for _, pr := range p.order {
			if in[pr.Number] || p.waits[pr.Number] != "" {
				continue
			}
			sha, ok := p.w.Repo.Merge(parent, pr, "")
			if !ok {
				p.waits[pr.Number] = "it conflicts with a pull request ahead of it"
				continue
			}
			p.chain = append(p.chain, Member{pr.Number, pr.Head, sha})
			in[pr.Number], parent = true, sha
			if added++; added == max(p.c.MaxBatch, 1) {
				break
			}
		}
		if added == 0 {
			return
		}
		p.push(p.candidate(len(p.chain), 0, ""))
	}
}

// notes writes each pull request's place in the queue on its head, when it changed.
func (p *plan) notes() {
	place := map[int]string{}
	for i, m := range p.chain {
		place[m.Number] = fmt.Sprintf("in the queue, %d ahead of it", i)
	}
	for _, pr := range p.w.PRs {
		text := place[pr.Number]
		if why := p.waits[pr.Number]; text == "" && why != "" {
			text = "waits: " + why
		} else if text == "" {
			text = "in the queue, waiting for a build"
		}
		if _, admitted := p.open[pr.Number]; (admitted || p.waits[pr.Number] != "") && pr.Pinned != "" && text != pr.Note {
			p.act(Action{Do: "note", PR: pr.Number, Text: text})
		}
	}
}

// note is the trailers a commit carries, which a later tick reads the build back from.
func note(kind Kind, round int, of, red SHA, try int) string {
	s := fmt.Sprintf("Banyan-Try: %d", try)
	if round > 0 {
		s += fmt.Sprintf("\nBanyan-Round: %d\nBanyan-Of: %s", round, of)
	}
	switch kind {
	case Check:
		s += "\nBanyan-Check: whole"
	case Probe:
		s += "\nBanyan-Probe-Of: " + red
	}
	return s
}

// Verify says whether a landing still holds in a world read just before the push: the build
// passed, stands on the trunk, and every pull request it lands is still queued at that head.
func Verify(w World, a Action) error {
	prs := map[int]PR{}
	for _, pr := range w.PRs {
		prs[pr.Number] = pr
	}
	for _, b := range w.Builds {
		if b.SHA != a.SHA {
			continue
		}
		rest, ok := b.above(w.Trunk)
		if !ok || b.Kind != Candidate || b.Verdict != Success {
			return fmt.Errorf("%.12s is no passed candidate on the trunk", a.SHA)
		}
		for _, m := range rest {
			if pr, in := prs[m.Number]; !in || pr.Head != m.Head || pr.Pinned != m.Head || pr.Hold || pr.Draft {
				return fmt.Errorf("#%d is no longer queued at %.12s", m.Number, m.Head)
			}
		}
		return nil
	}
	return fmt.Errorf("%.12s is gone", a.SHA)
}
