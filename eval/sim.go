package main

import (
	"crypto/sha1"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// Shape is a repository and its traffic, as the simulator plays them. Rates are per hour;
// chances are per pull request or per build.
type Shape struct {
	Name         string
	Build, Probe time.Duration
	Spread       float64 // a build takes this share of its time more or less, evenly
	Flake        float64 // a whole build fails though its tree is good
	ProbeFlake   float64
	Break        float64 // a pull request passes its own checks and fails the whole build
	Fails        float64 // a whole build of a tree a pull request breaks reports after this share of its time, 0 for all of it
	Clash        float64 // a pull request fails only with one queued ahead of it
	Conflict     float64 // it conflicts with the trunk
	Ahead        float64 // it conflicts with one queued ahead of it
	Lost         float64 // a build never reports
	Errored      float64 // a build ends with no verdict
	Backlog      int
	Arrivals     float64
	Hours        float64       // arrivals stop after this long
	Recorded     bool          // tick replay: facts and settings come from acts
	Stop         time.Duration // where positive, no tick at or after this time runs
	Moves        float64       // heads pushed while queued
	Removals     float64       // labels taken off
	Outside      float64       // trunk commits that did not come through the queue
	Episodes     []Episode
	Crash        float64         // a tick dies after one of its actions
	Race         float64         // a head moves between a tick's read and its landing
	Script       []bool          // exhaustive cases: whether the nth build flakes, in place of Flake
	Queue        []Queued        // a case's pull requests, each queued at its own time
	Takes        []time.Duration // a case's builds: how long the nth takes, in place of Build

	// A recorded window, played in place of arrivals drawn by chance: what its people, their
	// checks and its trunk did to each pull request, how long its CI's builds took, and how
	// long a build takes that runs every job whatever the change reaches.
	Acts  []Act
	Times []time.Duration
	Whole time.Duration
	// Clashing holds each head git was asked about, with the heads git cannot merge it with.
	Clashing map[queue.SHA]map[queue.SHA]bool
	// Failing holds how long each recorded whole build that a guilty head failed took, push to
	// answered as in Times, by the head. A whole build of a tree that holds such a head takes one
	// of its times in place of one from Times.
	Failing map[queue.SHA][]time.Duration
	// Rerun says the CI runs a red build's failed jobs once more before it reports, which
	// takes this much longer, and Rescued is the share of flakes that pass then.
	Rerun   time.Duration
	Rescued float64

	// The CI's pool: how many builds run at once, 0 for no limit. A build waits for an agent
	// behind every build that came before it, the queue's or not.
	Agents int
	// Cold makes the pool elastic: a build that finds every agent busy starts at once on a
	// cold one, and takes this much longer.
	Cold time.Duration
	// The other builds that share the pool: Others come an hour, Burst of them together on
	// average, and each holds an agent for Hold.
	Others, Burst float64
	Hold          time.Duration
	// Cancels says the CI stops a build whose branch the queue deleted, and frees its agent.
	// The queue is always configured to match.
	Cancels bool
	// Silent says the CI does not report when a build started.
	Silent bool
	// Instant says the CI begins every build the moment it is pushed, which no real one does.
	// Otherwise it takes a build up a few seconds after the push, and builds pushed together
	// in no order: that is the order they join the line for an agent in.
	Instant bool
}

// Queued is a pull request a case queues by hand, with its hidden facts.
type Queued struct {
	Number   int
	At       time.Duration
	Breaks   bool
	Clash    int
	Conflict bool
}

// Episode turns the trunk red from At. A plain one heals by itself after For, as a clock or
// an outside service does; one that NeedsFix stays red until a fix, queued Fix later, lands.
type Episode struct {
	At, For  time.Duration
	NeedsFix bool
	Fix      time.Duration
	// Reach, where set, is the one pull request whose change reaches the job that fails: on a
	// CI that runs a build's jobs by what its change reaches, only a tree that holds this pull
	// request ahead of the trunk runs that job, and every other build passes by skipping it.
	Reach int
	// Share, where set, is the share of the agents the episode breaks: a build that lands on
	// one fails whatever its tree, and the rest are untouched. 0 is all of them.
	Share float64
}

type simPR struct {
	queue.PR
	arrived     time.Duration
	breaks      bool
	clash       int // fails together with this pull request, 0 for none
	conflict    bool
	ahead       int // conflicts with this pull request, 0 for none
	fix         int // 1 + the episode it fixes
	queued      bool
	landed      bool
	gone        bool // ejected with cause, or removed by its author, and not coming back
	back        time.Duration
	mend        time.Duration // when its author pushes the head that mends its conflict with the trunk
	ready       time.Duration // when this head became ready, which starts its clock
	checksGreen time.Duration
	at          time.Duration // when it landed
}

type simBuild struct {
	queue.Build
	start, end time.Duration
	lost       bool
	flake      bool
	errored    bool
}

// Sim is one simulated world: the repository, its CI and the hidden facts that decide
// verdicts, which the planner never sees.
type Sim struct {
	shape   Shape
	cfg     queue.Config
	rng     *rand.Rand
	plan    func(queue.World, queue.Config) []queue.Action
	verify  func(queue.World, queue.Action) error
	now     time.Duration
	epoch   time.Time
	trunk   queue.SHA
	prs     map[int]*simPR
	byHead  map[queue.SHA]int
	builds  map[queue.SHA]*simBuild
	holds   map[queue.SHA]map[int]bool // the pull requests in each commit's tree
	under   map[queue.SHA]queue.SHA    // each commit's parent
	merges  map[queue.SHA]queue.Member // the pull request each merge commit merges
	trunks  map[queue.SHA]bool         // the commits the trunk holds
	seen    map[queue.SHA]bool
	started int
	next    int
	pool    *pool // the CI's agents, nil where they are unlimited
	alone   *pool // the same pool and the same other builds, with no queue

	crashAfterPush bool

	Trace    io.Writer
	Broken   []string      // invariants broken, each with what happened
	Stopped  time.Duration // when a queue that did not keep up was stopped
	Landed   int
	Wrong    int
	Ejected  map[string]int
	Left     map[int]string // the cause each ejected pull request last left for
	Whole    int
	Probes   int
	Red      int       // whole builds that reported failure
	Waits    []float64 // ready to landed, in build times
	Labelled []float64 // arrival to landed, in build times
	Ejects   []float64 // ready to ejected as the cause, in build times
	LastLand time.Duration
	Lands    []time.Duration // when each pull request landed
	Minutes  float64         // the time its builds took, each to its end
	opening  int
	history  *script

	RedCandidates   int
	RedPullRequests int
	QueueAdded      []float64 // ready to the landing candidate's push, in minutes
}

const tick = 2 * time.Minute

func NewSim(shape Shape, cfg queue.Config, seed int64) *Sim {
	if shape.Recorded {
		shape.Cancels = cfg.Cancels
	} else {
		cfg.Cancels = shape.Cancels
	}
	s := &Sim{shape: shape, cfg: cfg, plan: queue.Plan, verify: queue.Verify, rng: rand.New(rand.NewSource(seed)), epoch: time.Unix(1_800_000_000, 0).UTC(),
		trunk: "trunk0", prs: map[int]*simPR{}, byHead: map[queue.SHA]int{}, builds: map[queue.SHA]*simBuild{},
		holds: map[queue.SHA]map[int]bool{"trunk0": {}}, under: map[queue.SHA]queue.SHA{}, merges: map[queue.SHA]queue.Member{}, trunks: map[queue.SHA]bool{"trunk0": true}, seen: map[queue.SHA]bool{}, Ejected: map[string]int{}, Left: map[int]string{}}
	if shape.Agents > 0 {
		s.pool, s.alone = newPool(shape, seed), newPool(shape, seed)
	}
	for i := 0; i < shape.Backlog; i++ {
		s.arrive()
	}
	if shape.Acts != nil {
		s.history = newScript(shape.Acts)
	}
	s.queue()
	return s
}

// queue labels the case's pull requests whose time has come.
func (s *Sim) queue() {
	for _, q := range s.shape.Queue {
		if q.At <= s.now-tick || q.At > s.now {
			continue
		}
		p := &simPR{arrived: s.now, ready: s.now, queued: true, breaks: q.Breaks, clash: q.Clash, conflict: q.Conflict}
		p.Number, p.Head = q.Number, fmt.Sprintf("head%d", q.Number)
		p.Queued, p.Checks = s.epoch.Add(s.now), queue.Success
		if s.shape.Stop > 0 {
			p.arrived, p.ready, p.Queued = q.At, q.At, s.epoch.Add(q.At)
		}
		s.prs[p.Number], s.byHead[p.Head] = p, p.Number
		s.next = max(s.next, q.Number)
	}
}

// The Repo the planner merges with: SHAs are a hash of what went in, as git's are.

func (s *Sim) commit(parent queue.SHA, head queue.SHA, note string, adds int) queue.SHA {
	sha := fmt.Sprintf("%x", sha1.Sum([]byte(parent+"|"+head+"|"+note)))
	if _, ok := s.holds[sha]; !ok {
		in := map[int]bool{}
		for n := range s.holds[parent] {
			in[n] = true
		}
		if adds > 0 {
			in[adds] = true
		}
		s.holds[sha], s.under[sha] = in, parent
		if adds > 0 {
			s.merges[sha] = queue.Member{Number: adds, Head: head, Merge: sha}
		}
	}
	return sha
}

// advance puts the trunk at sha, which holds every commit under it.
func (s *Sim) advance(sha queue.SHA) {
	s.trunk = sha
	for at := sha; !s.trunks[at]; at = s.under[at] {
		s.trunks[at] = true
	}
}

// read gives a build its base and chain as git reads them: the base is the first commit
// under the build that the trunk holds, and the chain is the merges above it. A candidate's
// own merge keeps the name its chain gave it, which is the same whatever attempt built it.
func (s *Sim) read(b queue.Build) queue.Build {
	at := b.SHA
	if b.Kind != queue.Candidate {
		at = s.under[at]
	}
	var chain []queue.Member
	for ; !s.trunks[at] && s.merges[at].Number > 0; at = s.under[at] {
		chain = append([]queue.Member{s.merges[at]}, chain...)
	}
	if n := len(chain); n > 0 && b.Kind == queue.Candidate {
		chain[n-1].Merge = b.Chain[len(b.Chain)-1].Merge
	}
	b.Base, b.Chain = at, chain
	return b
}

func (s *Sim) Merge(parent queue.SHA, pr queue.PR, note string) (queue.SHA, bool) {
	p := s.prs[pr.Number]
	if p.conflict || s.shape.Recorded && p.ahead != 0 || p.ahead > 0 && s.holds[parent][p.ahead] || s.clashes(parent, pr.Head) {
		return "", false
	}
	return s.commit(parent, pr.Head, note, pr.Number), true
}

// clashes says the parent's tree holds a pull request, at the head it was merged at, that git
// cannot merge with this head.
func (s *Sim) clashes(parent, head queue.SHA) bool {
	others := s.shape.Clashing[head]
	for at := parent; len(others) > 0 && at != ""; at = s.under[at] {
		if others[s.merges[at].Head] {
			return true
		}
	}
	return false
}

// leftWaiting says a pull request ends the run waiting on the queue. A refusal onto the trunk
// that only a clashing head this simulation landed makes is the simulated order's, not its
// author's, so it still waits on the queue.
func (s *Sim) leftWaiting(p *simPR) bool {
	pr := p.PR
	if !p.conflict && p.ahead == 0 && s.clashes(s.trunk, p.Head) {
		pr.Note = ""
	}
	return p.queued && !p.landed && waitsOnTheQueue(pr)
}

func (s *Sim) Mark(parent queue.SHA, note string) queue.SHA { return s.commit(parent, "", note, 0) }
func (s *Sim) Seen(sha queue.SHA) bool                      { return s.seen[sha] }

func (s *Sim) chance(p float64) bool { return p > 0 && s.rng.Float64() < p }

// hourly turns a rate per hour into a chance per tick.
func (s *Sim) hourly(rate float64) bool { return s.chance(rate * tick.Hours()) }

func (s *Sim) arrive() *simPR {
	s.next++
	p := &simPR{arrived: s.now, queued: true}
	p.Number, p.Head = s.next, fmt.Sprintf("head%d", s.next)
	p.Queued, p.Checks = s.epoch.Add(s.now), queue.Success
	var ahead []int
	for n, o := range s.prs {
		if o.queued {
			ahead = append(ahead, n)
		}
	}
	sort.Ints(ahead)
	switch {
	case s.chance(s.shape.Break):
		p.breaks = true
	case len(ahead) > 0 && s.chance(s.shape.Clash):
		p.clash = ahead[s.rng.Intn(len(ahead))]
	case s.chance(s.shape.Conflict):
		p.conflict = true
	case len(ahead) > 0 && s.chance(s.shape.Ahead):
		p.ahead = ahead[s.rng.Intn(len(ahead))]
	}
	if s.chance(0.2) {
		p.Checks, p.checksGreen = queue.Pending, s.now+10*time.Minute
	}
	if p.Checks == queue.Success {
		p.ready = s.now
	}
	s.prs[p.Number], s.byHead[p.Head] = p, p.Number
	return p
}

// red is the episode that fails this build of this tree begun at this time, if any.
func (s *Sim) red(sha queue.SHA, tree map[int]bool, at time.Duration) bool {
	for i, e := range s.shape.Episodes {
		if at < e.At || (e.Reach > 0 && (!tree[e.Reach] || s.prs[e.Reach].landed)) || (e.Share > 0 && agent(sha) >= e.Share) {
			continue
		}
		if !e.NeedsFix && at < e.At+e.For {
			return true
		}
		if e.NeedsFix {
			fixed := false
			for n := range tree {
				fixed = fixed || s.prs[n].fix == i+1
			}
			if !fixed {
				return true
			}
		}
	}
	return false
}

// guilty says the tree fails for a reason that is a pull request's own: one that breaks the
// whole build, or the later of a pair that clash.
func (s *Sim) guilty(tree map[int]bool) bool {
	for n := range tree {
		if p := s.prs[n]; p.breaks || (p.clash > 0 && tree[p.clash]) {
			return true
		}
	}
	return false
}

func (s *Sim) checkedPR(n int) checkedPR {
	p := s.prs[n]
	if p == nil {
		return checkedPR{number: n}
	}
	return checkedPR{number: p.Number, head: p.Head, breaks: p.breaks, clash: p.clash, queued: p.queued, landed: p.landed, checks: p.Checks, wait: p.Wait}
}

func (s *Sim) checkedPRs() []checkedPR {
	ns := s.numbers()
	prs := make([]checkedPR, 0, len(ns))
	for _, n := range ns {
		prs = append(prs, s.checkedPR(n))
	}
	return prs
}

func (s *Sim) checkedBuild(sha queue.SHA) (checkedBuild, bool) {
	b := s.builds[sha]
	if b == nil {
		return checkedBuild{}, false
	}
	var heads map[int]queue.SHA
	if len(b.Chain) > 0 {
		heads = make(map[int]queue.SHA, len(b.Chain))
		for _, m := range b.Chain {
			heads[m.Number] = m.Head
		}
	}
	return checkedBuild{kind: b.Kind, verdict: b.Verdict, start: int64(b.start), holds: s.holds[sha], heads: heads}, true
}

// culprit says whether ejecting this pull request for a red build is right: it breaks the
// build alone, or it is one of a pair that clash and the other has landed or is still queued.
func (s *Sim) culprit(p *simPR) bool {
	return culpritPR(p.Number, s.checkedPRs())
}

// agent is where in the pool a build lands, from 0 to 1, by the commit alone.
func agent(sha queue.SHA) float64 {
	var n int64
	fmt.Sscanf(sha[min(len(sha), 4):min(len(sha), 10)], "%x", &n)
	return float64(n%10000) / 10000
}

// flakes says whether any build of this world may fail on a good tree.
func (s *Sim) flakes() bool {
	for _, e := range s.shape.Episodes {
		if e.Share > 0 {
			return true
		}
	}
	for _, f := range s.shape.Script {
		if f {
			return true
		}
	}
	return s.shape.Script == nil && (s.shape.Flake > 0 || s.shape.ProbeFlake > 0)
}

func (s *Sim) verdict(b *simBuild) queue.Verdict {
	tree := s.holds[b.SHA]
	switch {
	case s.red(b.SHA, tree, b.start), s.guilty(tree):
		return queue.Failure
	case b.flake:
		return queue.Failure
	}
	return queue.Success
}

// Step advances the world one tick: people and CI act, then the queue takes its tick.
func (s *Sim) Step() {
	s.step(s.now + tick)
}

func (s *Sim) step(at time.Duration) {
	s.now = at
	if s.pool != nil {
		s.pool.advance(s.now)
		s.alone.advance(s.now)
	}
	if s.now.Hours() < s.shape.Hours && s.hourly(s.shape.Arrivals) {
		s.arrive()
	}
	s.queue()
	s.play()
	for i, e := range s.shape.Episodes {
		if e.NeedsFix && s.now >= e.At+e.Fix && s.now < e.At+e.Fix+tick {
			p := s.arrive()
			p.First, p.fix, p.breaks, p.clash, p.conflict, p.ahead = true, i+1, false, 0, false, 0
			s.say("the fix for the red trunk is queued as #%d", p.Number)
			p.Checks, p.ready = queue.Success, s.now
		}
	}
	for _, n := range s.numbers() {
		p := s.prs[n]
		if p.Checks == queue.Pending && s.now >= p.checksGreen {
			p.Checks, p.ready = queue.Success, s.now
		}
		if !p.queued && !p.landed && !p.gone && p.back > 0 && s.now >= p.back {
			p.queued, p.back, p.Note = true, 0, ""
			if !s.shape.Recorded {
				p.ready = s.now
			}
			p.Queued = s.epoch.Add(s.now)
		}
	}
	if !s.shape.Recorded {
		for _, n := range s.numbers() {
			s.mend(s.prs[n])
		}
	}
	if s.hourly(s.shape.Moves) {
		s.move()
	}
	if s.hourly(s.shape.Removals) {
		if p := s.pick(); p != nil && p.fix == 0 {
			p.queued, p.gone = false, true
		}
	}
	if s.hourly(s.shape.Outside) {
		s.advance(s.commit(s.trunk, "", fmt.Sprintf("outside %d", s.now), 0))
	}
	for _, sha := range s.shas() {
		if b := s.builds[sha]; b.Verdict == queue.Pending && !b.lost && s.now >= b.end {
			if b.errored || !s.shape.Recorded && s.chance(s.shape.Errored) {
				b.Verdict = queue.Error
				s.say("  %.7s errored", sha)
				continue
			}
			b.Verdict = s.verdict(b)
			if b.Kind != queue.Probe && b.Verdict == queue.Failure {
				s.Red++
				if b.Kind == queue.Candidate {
					s.RedCandidates++
					s.RedPullRequests += len(b.Chain)
				}
			}
			s.say("  %.7s verdict %d (trunk red %v, guilty %v, flake %v)", sha, b.Verdict, s.red(sha, s.holds[sha], b.start), s.guilty(s.holds[sha]), b.flake)
		}
	}
	s.tick()
	if s.cfg.Keep > 0 {
		for _, b := range s.builds {
			if !b.Landed.IsZero() && s.epoch.Add(s.now).Sub(b.Landed) > s.cfg.Keep+10*tick {
				s.Broken = append(s.Broken, fmt.Sprintf("keep expiry: landed build %.12s still stands past keep", b.SHA))
			}
		}
	}
}

func (s *Sim) numbers() []int {
	var ns []int
	for n := range s.prs {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	return ns
}

func (s *Sim) shas() []queue.SHA {
	var all []queue.SHA
	for sha := range s.builds {
		all = append(all, sha)
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := s.builds[all[i]], s.builds[all[j]]
		if a.start != b.start {
			return a.start < b.start
		}
		return all[i] < all[j]
	})
	return all
}

// pick is a queued pull request at random.
func (s *Sim) pick() *simPR {
	var in []int
	for _, n := range s.numbers() {
		if s.prs[n].queued {
			in = append(in, n)
		}
	}
	if len(in) == 0 {
		return nil
	}
	return s.prs[in[s.rng.Intn(len(in))]]
}

// move pushes a new commit to a queued pull request. It stays queued, and the new head's own
// checks take as long as a first head's.
func (s *Sim) move() {
	if p := s.pick(); p != nil && p.fix == 0 {
		p.Head = fmt.Sprintf("%s'", p.Head)
		s.byHead[p.Head] = p.Number
		p.Checks, p.checksGreen, p.Note = queue.Pending, s.now+10*time.Minute, ""
	}
}

// mend is a pull request's author merging the trunk in once it conflicts with it: the new head
// is pushed 50 minutes after the conflict began, and its own checks take as long as any.
func (s *Sim) mend(p *simPR) {
	switch conflicts := p.queued && (p.conflict || (p.ahead > 0 && s.holds[s.trunk][p.ahead])); {
	case !conflicts:
		p.mend = 0
	case p.mend == 0:
		p.mend = s.now + 50*time.Minute
	case s.now >= p.mend:
		p.conflict, p.ahead, p.mend = false, 0, 0
		p.Head = fmt.Sprintf("%s+", p.Head)
		s.byHead[p.Head] = p.Number
		p.Checks, p.checksGreen, p.Note = queue.Pending, s.now+10*time.Minute, ""
	}
}

// World is what a tick reads: no hidden fact is in it.
func (s *Sim) World() queue.World {
	w := queue.World{Now: s.epoch.Add(s.now), Trunk: s.trunk, Repo: s}
	for _, n := range s.numbers() {
		if p := s.prs[n]; p.queued {
			w.PRs = append(w.PRs, p.PR)
		}
	}
	for _, sha := range s.shas() {
		sb := s.builds[sha]
		if s.cfg.Keep > 0 && sb.Landed.IsZero() && sb.Kind == queue.Candidate && sb.Round == 0 && s.trunks[sha] && sb.Verdict != queue.Pending {
			sb.Landed = w.Now
		}
		b := s.read(sb.Build)
		if sb.end != never && sb.start <= s.now && !s.shape.Silent {
			b.Started = s.epoch.Add(sb.start)
		}
		w.Builds = append(w.Builds, b)
	}
	return w
}

// tick runs the planner until it has nothing more to do, as the executor does, and applies
// what it asks for. A crash drops the rest of one pass.
func (s *Sim) tick() {
	for pass := 0; ; pass++ {
		acts := s.plan(s.World(), s.cfg)
		if len(acts) == 0 {
			return
		}
		if msg := checkPass(pass, acts); msg != "" {
			s.Broken = append(s.Broken, msg)
			return
		}
		crashed := s.chance(s.shape.Crash)
		if crashed {
			acts = acts[:s.rng.Intn(len(acts)+1)]
		}
		for i, a := range acts {
			s.crashAfterPush = crashed && i == len(acts)-1
			s.apply(a)
			if len(s.Broken) > 0 {
				return
			}
		}
		if crashed {
			return
		}
	}
}

func (s *Sim) say(format string, args ...any) {
	if s.Trace != nil {
		fmt.Fprintf(s.Trace, "%6s  %s\n", s.now, fmt.Sprintf(format, args...))
	}
}

func (s *Sim) apply(a queue.Action) {
	switch a.Do {
	case "push":
		var ns []int
		for _, m := range a.Build.Chain {
			ns = append(ns, m.Number)
		}
		s.say("push %.7s kind %d round %d of %.7s: %v", a.Build.SHA, a.Build.Kind, a.Build.Round, a.Build.Of, ns)
	case "note":
	default:
		s.say("%s %.7s #%d %s %s %.7s", a.Do, a.SHA, a.PR, a.Cause, a.Text, a.Evidence)
	}
	switch a.Do {
	case "note":
		s.prs[a.PR].Note = a.Text
	case "delete":
		if b := s.builds[a.SHA]; b != nil && s.cfg.Keep > 0 && !b.Landed.IsZero() && s.epoch.Add(s.now).Sub(b.Landed) < s.cfg.Keep {
			s.Broken = append(s.Broken, fmt.Sprintf("keep retention: landed build %.12s deleted before keep", b.SHA))
			return
		}
		if b := s.builds[a.SHA]; b != nil && s.pool != nil && s.shape.Cancels {
			s.pool.cancel(b, s.now)
		}
		if b := s.builds[a.SHA]; b != nil && s.shape.Cancels && b.end != never && b.end > s.now {
			s.Minutes -= (b.end - s.now).Minutes()
		}
		delete(s.builds, a.SHA)
	case "push":
		s.push(a.Build)
	case "eject":
		s.eject(a)
	case "land":
		if s.chance(s.shape.Race) {
			s.move()
		}
		if s.verify(s.World(), a) == nil {
			s.land(a)
		}
	}
}

func (s *Sim) push(b queue.Build) {
	if msg := checkPush(s.seen[b.SHA], b.SHA); msg != "" {
		s.Broken = append(s.Broken, msg)
		return
	}
	s.seen[b.SHA] = true
	d, flake := s.shape.Build, s.shape.Flake
	if b.Kind == queue.Probe {
		flake = s.shape.ProbeFlake
		if s.shape.Probe > 0 {
			d = s.shape.Probe
		}
		s.Probes++
	} else {
		s.Whole++
	}
	if s.shape.Spread > 0 {
		d += time.Duration(float64(d) * s.shape.Spread * (2*s.rng.Float64() - 1))
	}
	if s.shape.Fails > 0 && b.Kind != queue.Probe && s.guilty(s.holds[b.SHA]) {
		d = time.Duration(float64(d) * s.shape.Fails)
	}
	if s.started < len(s.shape.Takes) && s.shape.Takes[s.started] > 0 {
		d = s.shape.Takes[s.started]
	}
	if b.Kind != queue.Probe {
		if fails, ok := s.failing(b.SHA); ok {
			d = fails
		} else if len(s.shape.Times) > 0 {
			d = s.shape.Times[s.rng.Intn(len(s.shape.Times))]
			if b.Kind == queue.Check && s.shape.Whole > 0 {
				d = s.shape.Whole
			}
		}
	}

	sb := &simBuild{Build: b, start: s.now + s.pickup(b.SHA), end: s.now + d, lost: s.chance(s.shape.Lost)}
	s.Minutes += d.Minutes()
	if s.pool != nil {
		sb.end = never
		s.pool.add(sb, d-s.pickup(b.SHA), sb.start)
	}
	if s.shape.Recorded {
		fate := s.rng.Float64()
		sb.errored = fate < s.shape.Errored
		sb.flake = !sb.errored && fate < s.shape.Errored+flake
	} else if s.shape.Script != nil {
		sb.flake = s.started < len(s.shape.Script) && s.shape.Script[s.started]
	} else {
		sb.flake = s.chance(flake)
	}
	if s.shape.Rerun > 0 && b.Kind != queue.Probe && (sb.flake || s.guilty(s.holds[b.SHA])) {
		sb.end, sb.flake = sb.end+s.shape.Rerun, sb.flake && !s.chance(s.shape.Rescued)
		s.Minutes += s.shape.Rerun.Minutes()
	}
	s.started++
	s.builds[b.SHA] = sb
}

// failing is how long a whole build of this tree takes to fail by the recorded failing times of
// the guilty heads it holds, where it holds one the record timed. It takes one draw in place of
// the shared sample's, the shortest of the times it picks where the tree holds more than one.
// It leaves two things as they were: where two guilty heads failed in the same recorded build,
// that build's time is each of theirs, and a failing time runs from push to answered.
func (s *Sim) failing(sha queue.SHA) (time.Duration, bool) {
	if len(s.shape.Failing) == 0 {
		return 0, false
	}
	var timed [][]time.Duration
	for _, n := range slices.Sorted(maps.Keys(s.holds[sha])) {
		if times := s.shape.Failing[s.prs[n].Head]; s.prs[n].breaks && len(times) > 0 {
			timed = append(timed, times)
		}
	}
	if len(timed) == 0 {
		return 0, false
	}
	d, draw := never, s.rng.Int63()
	for _, times := range timed {
		d = min(d, times[draw%int64(len(times))])
	}
	return d, true
}

// pickup is how long after its push the CI takes a build up: one to ten seconds, by the commit
// alone, so a world replays exactly. It is part of the build's time, not added to it.
func (s *Sim) pickup(sha queue.SHA) time.Duration {
	if s.shape.Instant {
		return 0
	}
	var n int64
	fmt.Sscanf(sha[:min(len(sha), 4)], "%x", &n)
	return time.Second + time.Duration(n%9000)*time.Millisecond
}

func (s *Sim) eject(a queue.Action) {
	p := s.prs[a.PR]
	p.queued = false
	s.Ejected[a.Cause]++
	s.Left[a.PR] = a.Cause
	culprit := a.Cause == "culprit" && s.culprit(p)
	s.Broken = append(s.Broken, checkEject(a, culprit, s.flakes(), s.checkedBuild)...)
	if a.Cause != "culprit" {
		return
	}
	if culprit {
		p.gone = s.history == nil
		s.history.blame(a.PR, s.now)
		s.Ejects = append(s.Ejects, float64(s.now-p.ready)/float64(s.shape.Build))
		return
	}
	s.Wrong++
	p.back = s.now + 10*time.Minute
}

func (s *Sim) land(a queue.Action) {
	broken, ok := checkLand(a, s.checkedBuild, s.checkedPR)
	s.Broken = append(s.Broken, broken...)
	if !ok {
		return
	}
	pushed := s.builds[a.SHA].Pushed.Sub(s.epoch)
	for _, m := range a.Members {
		p := s.prs[m.Number]
		if p.landed {
			continue
		}
		p.landed, p.queued, p.at = true, false, s.now
		s.Landed++
		s.Lands = append(s.Lands, s.now)
		s.Waits = append(s.Waits, float64(s.now-p.ready)/float64(s.shape.Build))
		s.Labelled = append(s.Labelled, float64(s.now-p.arrived)/float64(s.shape.Build))
		if pushed >= p.ready {
			s.QueueAdded = append(s.QueueAdded, (pushed - p.ready).Minutes())
		}
	}
	s.advance(a.SHA)
	s.LastLand = s.now
	if s.cfg.Keep <= 0 {
		delete(s.builds, a.SHA)
	} else if !s.crashAfterPush {
		s.builds[a.SHA].Landed = s.epoch.Add(s.now)
	}
}

// Run plays ticks before Stop, or drains an unbounded shape and checks that it finishes.
func (s *Sim) Run() {
	if s.shape.Stop > 0 {
		if s.shape.Recorded {
			s.play()
			for _, p := range s.prs {
				if p.queued && waitsOnTheQueue(p.PR) {
					s.opening++
				}
			}
			s.tick()
		}
		for len(s.Broken) == 0 {
			next := s.now + tick
			if s.shape.Recorded {
				for _, acts := range s.history.acts {
					if len(acts) > 0 && acts[0].At > s.now {
						next = min(next, acts[0].At)
					}
				}
			}
			if next >= s.shape.Stop {
				break
			}
			s.step(next)
		}
		return
	}
	limit := time.Duration(s.shape.Hours*float64(time.Hour)) + time.Duration(60+2*len(s.prs)+int(4*s.shape.Arrivals*s.shape.Hours))*s.shape.Build
	for _, e := range s.shape.Episodes {
		limit += e.For + e.Fix
	}
	if s.cfg.Keep > 0 {
		limit += s.cfg.Keep + 11*tick
	}
	for s.now < limit && len(s.Broken) == 0 {
		s.Step()
		if s.now.Hours() >= s.shape.Hours && s.idle() && !s.kept() {
			return
		}
		if len(s.Broken) == 0 && s.overloaded() {
			s.Stopped = s.now
			return
		}
	}
	if len(s.Broken) > 0 {
		return
	}
	if msg := checkDrain(s.checkedPRs(), limit.String()); msg != "" {
		s.Broken = append(s.Broken, msg)
	}
}

// overloaded says the queue is not keeping up and will not recover: more builds are alive
// than ten rounds of every slot would leave, or more pull requests are queued than arrive in
// twelve builds, over what it began with. A queue with nothing building is left to run, so a
// planner that has stalled still breaks invariant 4.
func (s *Sim) overloaded() bool {
	building, queued, live := false, 0, 0
	for _, b := range s.builds {
		if s.cfg.Keep > 0 && !b.Landed.IsZero() {
			continue
		}
		building = building || (b.Verdict == queue.Pending && !b.lost)
		live++
	}
	for _, p := range s.prs {
		if p.queued {
			queued++
		}
	}
	behind := s.shape.Arrivals > 0 && building && queued > max(80, s.shape.Backlog+int(12*s.shape.Arrivals*s.shape.Build.Hours()))
	return behind || live > max(300, 10*s.cfg.Slots*s.cfg.MaxBatch)
}

func (s *Sim) kept() bool {
	if s.cfg.Keep > 0 {
		for _, b := range s.builds {
			if !b.Landed.IsZero() || s.trunks[b.SHA] {
				return true
			}
		}
	}
	return false
}

// idle says no pull request or episode remains.
func (s *Sim) idle() bool {
	for _, p := range s.prs {
		if p.queued || (p.back > 0 && !p.gone && !p.landed) {
			return false
		}
	}
	for _, e := range s.shape.Episodes {
		if s.now < e.At+e.Fix+tick {
			return false
		}
	}
	return true
}
