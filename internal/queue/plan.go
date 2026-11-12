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
//   - A red candidate at the bottom of the chain is set aside with its pull requests, and the
//     chain goes on without them. It is narrowed by rounds of builds of its prefixes, begun
//     together. A pull request leaves only when, in one round, the prefix below it passed and
//     its own failed, and its own has failed twice.
//   - A round in which nothing passed, the tip included, is a red trunk: nobody is blamed.
func Plan(w World, c Config) []Action {
	p := &plan{w: w, c: c, open: map[int]PR{}, waits: map[int]string{}}
	p.admit()
	p.read()
	ejected := p.judge()
	if p.land() || ejected {
		return p.acts
	}
	p.retry()
	p.rounds()
	p.fill()
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
	builds  []Build        // live candidates on the chain, each standing on the trunk
	above   []Build        // candidates stacked on a red one that may still land it
	reds    []*red
	pushed  []SHA // every build this tick begins
	pending int   // candidates with no verdict yet, which is what the slots count
	held    bool  // a round on the trunk's tip found the trunk red
	wait    bool  // probes are about to say whether candidates stacked on a red one are doomed
}

// red is a red candidate set aside, with the pull requests it added to its base. The suspects
// are those the rounds have not yet ruled out, and only they stay out of the chain.
type red struct {
	of     SHA // the prefix it tests, which names it whatever attempt was built
	base   SHA
	chain  []Member
	builds []Build // its attempts, and the rounds run on it
	sha    SHA     // the attempt that failed last

	// What the rounds say so far: the culprit is in prefixes lo+1 to hi.
	lo, hi    int
	fails     map[int]int
	last      int // the newest round
	pushed    time.Time
	proof     []SHA
	running   bool // a round's build has no verdict yet
	confirmed bool // the last round had a pass below a failure
	none, tip bool // nothing passed in the last round, and the tip failed in it
	flake     bool // probes passed on the red tree itself
	cleared   bool // a whole build of the red tree passed
	void      bool // the last round lost a build
	whole     bool // rounds are whole builds, not probes
	passed    bool // a candidate stacked on it passed, so it lands
	backed    bool // a candidate stacked on it failed too, as this tick found
}

func (r *red) suspects() []Member { return r.chain[r.lo:r.hi] }

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
		case pr.Wait != "":
			p.waits[pr.Number] = pr.Wait
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

// queued says every member is admitted at the head that was merged.
func (p *plan) queued(chain []Member) bool {
	for _, m := range chain {
		if pr, in := p.open[m.Number]; !in || pr.Head != m.Head {
			return false
		}
	}
	return true
}

func (p *plan) drop(builds ...Build) {
	for _, b := range builds {
		p.act(Action{Do: "delete", SHA: b.SHA})
	}
}

// read sorts the builds into the red candidates set aside with their rounds, and the
// candidates stacked on the trunk, whose longest is the chain. It deletes every branch that
// is neither. A red candidate and its rounds stand wherever the trunk has moved to since.
func (p *plan) read() {
	trunk := p.w.Trunk
	byOf := map[SHA]*red{}
	var reds []*red
	for _, b := range p.w.Builds {
		if b.Round > 0 {
			if byOf[b.Of] == nil {
				byOf[b.Of] = &red{of: b.Of, base: b.Base}
				reds = append(reds, byOf[b.Of])
			}
		}
	}
	var stacked []Build
	for _, b := range p.w.Builds {
		if b.Verdict == Pending && p.c.Timeout > 0 && p.w.Now.Sub(b.Pushed) > p.c.Timeout {
			b.Verdict = Error
		}
		if b.Round > 0 {
			byOf[b.Of].builds = append(byOf[b.Of].builds, b)
			continue
		}
		if r := byOf[b.Tests()]; r != nil {
			if chain, ok := b.above(r.base); ok {
				r.chain, r.builds = chain, append(r.builds, b)
				continue
			}
		}
		if rest, ok := b.above(trunk); ok && len(rest) > 0 && p.queued(rest) {
			b.Base, b.Chain = trunk, rest
			stacked = append(stacked, b)
		} else {
			p.drop(b)
		}
	}
	for _, r := range reds {
		p.weigh(r)
		// It goes when a pull request in the trees its rounds build has left, when a whole
		// build of the red tree passed, and when its base is no longer the tip and fails by itself.
		if len(r.chain) == 0 || !p.queued(r.chain[:r.hi]) || r.cleared || (r.none && r.tip && r.base != trunk) {
			p.drop(r.builds...)
			continue
		}
		p.reds = append(p.reds, r)
		p.held = p.held || (r.none && r.tip)
	}

	var free []Build
	for _, b := range stacked {
		switch r := p.under(b); {
		case r != nil:
			p.stackedOn(r, b)
		case p.held && (b.Verdict == Failure || b.Verdict == Error):
			p.drop(b)
		default:
			free = append(free, b)
			if len(b.Chain) > len(p.chain) {
				p.chain = slices.Clone(b.Chain)
			}
		}
	}
	for _, b := range free {
		on := true
		for i := range b.Chain {
			on = on && b.Chain[i].Merge == p.chain[i].Merge
		}
		if !on {
			p.drop(b)
			continue
		}
		p.builds = append(p.builds, b)
	}
	if ks := p.stacked(); !p.held && len(ks) > 0 && p.red(ks[0]) {
		r := &red{of: p.chain[ks[0]-1].Merge, base: trunk, chain: p.chain[:ks[0]]}
		r.builds, _ = p.at(ks[0])
		p.weigh(r)
		for _, b := range p.builds {
			if len(b.Chain) > ks[0] {
				p.stackedOn(r, b)
			}
		}
		p.reds, p.chain, p.builds = append(p.reds, r), nil, nil
	}

	for _, r := range p.reds {
		for _, m := range r.suspects() {
			p.waits[m.Number] = "a build that holds it failed, and the queue is finding the cause"
		}
	}
	for _, b := range append(p.builds, p.above...) {
		if b.Verdict == Pending {
			p.pending++
		}
	}
}

// under is the red candidate on the trunk that this candidate is stacked on, if any.
func (p *plan) under(b Build) *red {
	for _, r := range p.reds {
		if k := len(r.chain); r.base == p.w.Trunk && len(b.Chain) > k && b.Chain[k-1].Merge == r.of {
			return r
		}
	}
	return nil
}

// stackedOn keeps a candidate stacked on a red one only while it may still land it: not once
// it has failed, and not once a round has shown the red to be real. Where probes will say
// which within a fraction of a build, the chain waits for them before it goes on beside it.
func (p *plan) stackedOn(r *red, b Build) {
	if b.Verdict == Failure || b.Verdict == Error || (b.Verdict == Pending && r.confirmed) {
		r.backed = r.backed || b.Verdict == Failure
		p.drop(b)
		return
	}
	r.passed = r.passed || b.Verdict == Success
	p.wait = p.wait || (b.Verdict == Pending && !r.whole)
	p.above = append(p.above, b)
}

// weigh reads a red candidate's rounds in order, and leaves what they say on it.
func (p *plan) weigh(r *red) {
	k := len(r.chain)
	r.lo, r.hi, r.fails, r.whole = 0, k, map[int]int{}, !p.c.Probes
	rounds := map[int][]Build{}
	for _, b := range r.builds {
		switch {
		case b.Round > 0:
			rounds[b.Round] = append(rounds[b.Round], b)
			r.last = max(r.last, b.Round)
			r.whole = r.whole || (b.Kind == Check && b.Verdict == Failure)
		case b.Verdict == Failure:
			r.fails[k]++
			r.sha = b.SHA
		}
	}
	for n := 1; n <= r.last; n++ {
		g, low := -1, -1
		tip := false
		r.void = false
		var lowSHA, gSHA SHA
		var by Kind
		for _, b := range rounds[n] {
			if b.Pushed.After(r.pushed) {
				r.pushed = b.Pushed
			}
			switch j := len(b.Chain); b.Verdict {
			case Pending:
				r.running = true
				return
			case Error:
				r.void = true
			case Failure:
				tip = tip || j == 0
			case Success:
				if j > g {
					g, gSHA, by = j, b.SHA, b.Kind
				}
			}
		}
		if r.void || len(rounds[n]) == 0 {
			continue
		}
		if g < 0 {
			r.none, r.tip, r.flake, r.confirmed, r.lo, r.hi = true, tip, false, false, 0, k
			continue
		}
		r.none, r.tip, r.flake, r.cleared = false, false, g >= k && by == Probe, g >= k && by != Probe
		if g >= k {
			continue
		}
		for _, b := range rounds[n] {
			if j := len(b.Chain); b.Verdict == Failure && j > g && (low < 0 || j < low) {
				low, lowSHA = j, b.SHA
			}
		}
		r.lo = g
		if r.confirmed = low > 0; r.confirmed {
			r.hi = low
			for _, b := range rounds[n] {
				if len(b.Chain) == low && b.Verdict == Failure {
					r.fails[low]++
				}
			}
			r.proof = []SHA{gSHA, lowSHA}
		} else if r.hi <= r.lo {
			r.hi = k
		}
	}
}

// stacked is the lengths of the prefixes of the chain that hold a candidate, lowest first.
func (p *plan) stacked() []int {
	var ks []int
	for _, b := range p.builds {
		if !slices.Contains(ks, len(b.Chain)) {
			ks = append(ks, len(b.Chain))
		}
	}
	sort.Ints(ks)
	return ks
}

// at is the candidates of prefix k, oldest first, and whether one passed.
func (p *plan) at(k int) (all []Build, passed bool) {
	for _, b := range p.builds {
		if len(b.Chain) == k {
			all = append(all, b)
			passed = passed || b.Verdict == Success
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Pushed.Before(all[j].Pushed) })
	return all, passed
}

// red says prefix k's newest attempt failed and no build of it passed.
func (p *plan) red(k int) bool {
	all, passed := p.at(k)
	return len(all) > 0 && !passed && all[len(all)-1].Verdict == Failure
}

func (p *plan) prefix(j int) SHA {
	if j == 0 {
		return p.w.Trunk
	}
	return p.chain[j-1].Merge
}

// judge ejects the pull request a red candidate's rounds have proved the culprit, and says
// whether it ejected any.
func (p *plan) judge() bool {
	ejected := false
	for _, r := range p.reds {
		if r.running || r.passed || r.hi != r.lo+1 || !r.confirmed || r.fails[r.hi] < 2 {
			continue
		}
		p.act(Action{Do: "eject", PR: r.chain[r.hi-1].Number, Cause: "culprit", Evidence: append(r.proof, r.sha),
			Text: fmt.Sprintf("the build of the queue up to it failed twice, and the build without it passed (%.12s)", r.proof[0])})
		ejected = true
	}
	return ejected
}

// land moves the trunk to the highest prefix whose whole build passed.
func (p *plan) land() bool {
	var best *Build
	all := append(slices.Clone(p.builds), p.above...)
	for i, b := range all {
		if b.Verdict == Success && (best == nil || len(b.Chain) > len(best.Chain)) {
			best = &all[i]
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
		if sha := commit(n); !p.w.Repo.Seen(sha) && !slices.Contains(p.pushed, sha) {
			p.pushed = append(p.pushed, sha)
			return sha
		}
	}
}

// candidate is a whole build of prefix j that may land: the chain's own merge the first time,
// and after that the same merge carrying a try number.
func (p *plan) candidate(j int) Build {
	b := Build{Kind: Candidate, Base: p.w.Trunk, Chain: slices.Clone(p.chain[:j])}
	m := p.chain[j-1]
	b.SHA = p.fresh(func(n int) SHA {
		if n == 1 {
			return m.Merge
		}
		sha, _ := p.w.Repo.Merge(p.prefix(j-1), p.open[m.Number], note(Candidate, 0, "", "", n))
		return sha
	})
	return b
}

func (p *plan) push(b Build) {
	b.Pushed = p.w.Now
	p.act(Action{Do: "push", Build: b})
	if b.Kind == Candidate {
		p.pending++
	}
}

// retry builds again a candidate on the chain whose newest attempt ended with no verdict.
func (p *plan) retry() {
	for _, k := range p.stacked() {
		all, passed := p.at(k)
		if b := all[len(all)-1]; !passed && b.Verdict == Error && p.w.Now.Sub(b.Pushed) >= p.c.Recheck {
			p.push(p.candidate(k))
		}
	}
}

// rounds begins the next round of each red candidate that is ready for one.
func (p *plan) rounds() {
	for _, r := range p.reds {
		wait := ((r.none && r.tip) || r.void) && p.w.Now.Sub(r.pushed) < p.c.Recheck
		if r.running || r.passed || wait {
			continue
		}
		k, whole := len(r.chain), r.whole
		var points []int
		switch span := r.hi - r.lo; {
		case r.flake:
			// The probes passed on the red tree itself, so a whole build of it decides.
			points, whole = []int{k}, true
		case r.none:
			// The tip, the top, and the prefix that ends at a pull request labelled as the
			// trunk's fix, which is where a green would show.
			points = []int{0, k}
			for j := k - 1; j > 0; j-- {
				if p.open[r.chain[j-1].Number].First {
					points = []int{0, j, k}
					break
				}
			}
		default:
			points = []int{r.lo}
			n := min(max(p.c.Slots, 2)-1, span-1)
			if !whole {
				n = span - 1
			}
			for i := 1; i <= n; i++ {
				points = append(points, r.lo+i*span/(n+1))
			}
			// A round that can convict builds the top of the span, which has failed before.
			// With no failed build stacked on the red candidate to bear it out, it builds
			// the top twice, so that one more flake does not convict it. A round of whole
			// builds that reaches every prefix builds each of them twice, and so convicts
			// by itself wherever the culprit is.
			if whole && n == span-1 {
				points = append(append(points, points[1:]...), r.hi)
			} else if span == 1 {
				points = append(points, r.hi)
			}
			if !r.backed && points[len(points)-1] == r.hi {
				points = append(points, r.hi)
			}
		}
		for _, j := range points {
			p.push(p.evidence(r, j, whole))
		}
	}
}

// evidence is a round's build of prefix j of a red candidate, which never lands: a probe
// where the repository offers them, else a whole build.
func (p *plan) evidence(r *red, j int, whole bool) Build {
	b := Build{Kind: Probe, Base: r.base, Chain: slices.Clone(r.chain[:j]), Round: r.last + 1, Of: r.of}
	parent, red := r.base, r.sha
	if j > 0 {
		parent = r.chain[j-1].Merge
	}
	if whole {
		b.Kind, red = Check, ""
	}
	b.SHA = p.fresh(func(n int) SHA { return p.w.Repo.Mark(parent, note(b.Kind, b.Round, r.of, red, n)) })
	return b
}

// fill stacks new candidates on the chain while slots are free. It waits while the trunk is
// red, unless a pull request labelled as its fix has yet to be built, while probes are
// deciding whether a red candidate is real, and while any of the chain is red above a build
// still running.
func (p *plan) fill() {
	in := map[int]bool{}
	for _, m := range p.chain {
		in[m.Number] = true
	}
	fix := slices.ContainsFunc(p.order, func(pr PR) bool { return pr.First && !in[pr.Number] && p.waits[pr.Number] == "" })
	if p.wait || (p.held && !fix) {
		return
	}
	for _, k := range p.stacked() {
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
			if added++; added == max(p.c.MaxBatch, 1) || pr.First {
				break
			}
		}
		if added == 0 {
			return
		}
		p.push(p.candidate(len(p.chain)))
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
			if pr, in := prs[m.Number]; !in || pr.Head != m.Head || pr.Pinned != m.Head || pr.Wait != "" {
				return fmt.Errorf("#%d is no longer queued at %.12s", m.Number, m.Head)
			}
		}
		return nil
	}
	return fmt.Errorf("%.12s is gone", a.SHA)
}
