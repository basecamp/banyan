package main

import (
	"crypto/sha1"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// Shape is a repository and its traffic, as the simulator plays them. Rates are per hour;
// chances are per pull request or per build.
type Shape struct {
	Name         string
	Build, Probe time.Duration
	Flake        float64 // a whole build fails though its tree is good
	ProbeFlake   float64
	Break        float64 // a pull request passes its own checks and fails the whole build
	Clash        float64 // a pull request fails only with one queued ahead of it
	Conflict     float64 // it conflicts with the trunk
	Ahead        float64 // it conflicts with one queued ahead of it
	Lost         float64 // a build never reports
	Errored      float64 // a build ends with no verdict
	Backlog      int
	Arrivals     float64
	Hours        float64 // arrivals stop after this long
	Moves        float64 // heads pushed while queued
	Removals     float64 // labels taken off
	Outside      float64 // trunk commits that did not come through the queue
	Episodes     []Episode
	Crash        float64         // a tick dies after one of its actions
	Race         float64         // a head moves between a tick's read and its landing
	Script       []bool          // exhaustive cases: whether the nth build flakes, in place of Flake
	Queue        []Queued        // a case's pull requests, each queued at its own time
	Takes        []time.Duration // a case's builds: how long the nth takes, in place of Build

	// The CI's pool: how many builds run at once, 0 for no limit. A build waits for an agent
	// behind every build that came before it, the queue's or not.
	Agents int
	// The other builds that share the pool: Others come an hour, Burst of them together on
	// average, and each holds an agent for Hold.
	Others, Burst float64
	Hold          time.Duration
	// Cancels says the CI stops a build whose branch the queue deleted, and frees its agent.
	// The queue is always configured to match.
	Cancels bool
	// Silent says the CI does not report when a build started.
	Silent bool
}

// Queued is a pull request a case queues by hand, with its hidden facts.
type Queued struct {
	Number int
	At     time.Duration
	Breaks bool
	Clash  int
}

// Episode turns the trunk red from At. A plain one heals by itself after For, as a clock or
// an outside service does; one that NeedsFix stays red until a fix, queued Fix later, lands.
type Episode struct {
	At, For  time.Duration
	NeedsFix bool
	Fix      time.Duration
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
	firstRed    time.Duration
	checksGreen time.Duration
}

type simBuild struct {
	queue.Build
	start, end time.Duration
	lost       bool
	flake      bool
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
	seen    map[queue.SHA]bool
	started int
	next    int
	pool    *pool // the CI's agents, nil where they are unlimited
	alone   *pool // the same pool and the same other builds, with no queue

	Trace    io.Writer
	Broken   []string // invariants broken, each with what happened
	Landed   int
	Wrong    int
	Ejected  map[string]int
	Left     map[int]string // the cause each ejected pull request last left for
	Whole    int
	Probes   int
	Waits    []float64 // queue to landed, in build times
	Blames   []float64 // first red to ejection, in build times
	LastLand time.Duration
}

const tick = 2 * time.Minute

func NewSim(shape Shape, cfg queue.Config, seed int64) *Sim {
	cfg.Cancels = shape.Cancels
	s := &Sim{shape: shape, cfg: cfg, plan: queue.Plan, verify: queue.Verify, rng: rand.New(rand.NewSource(seed)), epoch: time.Unix(1_800_000_000, 0).UTC(),
		trunk: "trunk0", prs: map[int]*simPR{}, byHead: map[queue.SHA]int{}, builds: map[queue.SHA]*simBuild{},
		holds: map[queue.SHA]map[int]bool{"trunk0": {}}, seen: map[queue.SHA]bool{}, Ejected: map[string]int{}, Left: map[int]string{}}
	if shape.Agents > 0 {
		s.pool, s.alone = newPool(shape, seed), newPool(shape, seed)
	}
	for i := 0; i < shape.Backlog; i++ {
		s.arrive()
	}
	s.queue()
	return s
}

// queue labels the case's pull requests whose time has come. The queue has posted nothing on
// them yet, so each is pinned by the tick that first reads it.
func (s *Sim) queue() {
	for _, q := range s.shape.Queue {
		if q.At <= s.now-tick || q.At > s.now {
			continue
		}
		p := &simPR{arrived: s.now, queued: true, breaks: q.Breaks, clash: q.Clash}
		p.Number, p.Head = q.Number, fmt.Sprintf("head%d", q.Number)
		p.Queued, p.Checks = s.epoch.Add(s.now), queue.Success
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
		s.holds[sha] = in
	}
	return sha
}

func (s *Sim) Merge(parent queue.SHA, pr queue.PR, note string) (queue.SHA, bool) {
	p := s.prs[pr.Number]
	if p.conflict || (p.ahead > 0 && s.holds[parent][p.ahead]) {
		return "", false
	}
	return s.commit(parent, pr.Head, note, pr.Number), true
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
	p.Pinned, p.Queued, p.Checks = p.Head, s.epoch.Add(s.now), queue.Success
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
	s.prs[p.Number], s.byHead[p.Head] = p, p.Number
	return p
}

// red is the episode that fails a build of this tree begun at this time, if any.
func (s *Sim) red(tree map[int]bool, at time.Duration) bool {
	for i, e := range s.shape.Episodes {
		if at < e.At {
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

// culprit says whether ejecting this pull request for a red build is right: it breaks the
// build alone, or it is one of a pair that clash and the other has landed or is still queued.
func (s *Sim) culprit(p *simPR) bool {
	if p.breaks {
		return true
	}
	for _, o := range s.prs {
		if (o.Number == p.clash || o.clash == p.Number) && (o.landed || o.queued) {
			return true
		}
	}
	return false
}

// flakes says whether any build of this world may fail on a good tree.
func (s *Sim) flakes() bool {
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
	case s.red(tree, b.start), s.guilty(tree):
		return queue.Failure
	case b.flake:
		return queue.Failure
	}
	return queue.Success
}

// Step advances the world one tick: people and CI act, then the queue takes its tick.
func (s *Sim) Step() {
	s.now += tick
	if s.pool != nil {
		s.pool.advance(s.now)
		s.alone.advance(s.now)
	}
	if s.now.Hours() < s.shape.Hours && s.hourly(s.shape.Arrivals) {
		s.arrive()
	}
	s.queue()
	for i, e := range s.shape.Episodes {
		if e.NeedsFix && s.now >= e.At+e.Fix && s.now < e.At+e.Fix+tick {
			p := s.arrive()
			p.First, p.fix, p.breaks, p.clash, p.conflict, p.ahead = true, i+1, false, 0, false, 0
			s.say("the fix for the red trunk is queued as #%d", p.Number)
			p.Checks = queue.Success
		}
	}
	for _, n := range s.numbers() {
		p := s.prs[n]
		if p.Checks == queue.Pending && s.now >= p.checksGreen {
			p.Checks = queue.Success
		}
		if !p.queued && !p.landed && !p.gone && p.back > 0 && s.now >= p.back {
			p.queued, p.back, p.Pinned, p.Note = true, 0, p.Head, ""
			p.Queued = s.epoch.Add(s.now)
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
		s.trunk = s.commit(s.trunk, "", fmt.Sprintf("outside %d", s.now), 0)
	}
	for _, sha := range s.shas() {
		if b := s.builds[sha]; b.Verdict == queue.Pending && !b.lost && s.now >= b.end {
			if s.chance(s.shape.Errored) {
				b.Verdict = queue.Error
				s.say("  %.7s errored", sha)
				continue
			}
			b.Verdict = s.verdict(b)
			s.say("  %.7s verdict %d (trunk red %v, guilty %v, flake %v)", sha, b.Verdict, s.red(s.holds[sha], b.start), s.guilty(s.holds[sha]), b.flake)
			if b.Verdict == queue.Failure {
				for _, m := range b.Chain {
					if p := s.prs[m.Number]; p.firstRed == 0 {
						p.firstRed = s.now
					}
				}
			}
		}
	}
	s.tick()
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

// move pushes a new commit to a queued pull request, which its author queues again later.
func (s *Sim) move() {
	if p := s.pick(); p != nil && p.fix == 0 {
		p.Head = fmt.Sprintf("%s'", p.Head)
		s.byHead[p.Head] = p.Number
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
		b := sb.Build
		if sb.end != never && !s.shape.Silent {
			b.Started = s.epoch.Add(sb.start)
		}
		if b.Verdict != queue.Pending {
			b.Finished = s.epoch.Add(sb.end)
		}
		w.Builds = append(w.Builds, b)
	}
	return w
}

func (s *Sim) broke(n int, format string, args ...any) {
	s.Broken = append(s.Broken, fmt.Sprintf("invariant %d: %s", n, fmt.Sprintf(format, args...)))
}

// tick runs the planner until it has nothing more to do, as the executor does, and applies
// what it asks for. A crash drops the rest of one pass.
func (s *Sim) tick() {
	for pass := 0; ; pass++ {
		acts := s.plan(s.World(), s.cfg)
		if len(acts) == 0 {
			return
		}
		if pass == 6 {
			s.broke(5, "the tick still had %d actions after six passes, the first %s", len(acts), acts[0].Do)
			return
		}
		crashed := s.chance(s.shape.Crash)
		if crashed {
			acts = acts[:s.rng.Intn(len(acts)+1)]
		}
		for _, a := range acts {
			s.apply(a)
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
	case "note", "pin":
	default:
		s.say("%s %.7s #%d %s %s %.7s", a.Do, a.SHA, a.PR, a.Cause, a.Text, a.Evidence)
	}
	switch a.Do {
	case "pin":
		s.prs[a.PR].Pinned = s.prs[a.PR].Head
	case "note":
		s.prs[a.PR].Note = a.Text
	case "delete":
		if b := s.builds[a.SHA]; b != nil && s.pool != nil && s.shape.Cancels {
			s.pool.cancel(b, s.now)
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
	if s.seen[b.SHA] {
		s.broke(7, "%.12s was pushed for a second build", b.SHA)
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
	if s.started < len(s.shape.Takes) && s.shape.Takes[s.started] > 0 {
		d = s.shape.Takes[s.started]
	}
	sb := &simBuild{Build: b, start: s.now, end: s.now + d, lost: s.chance(s.shape.Lost)}
	if s.pool != nil {
		sb.end = never
		s.pool.add(sb, d, s.now)
	}
	if s.shape.Script != nil {
		sb.flake = s.started < len(s.shape.Script) && s.shape.Script[s.started]
	} else {
		sb.flake = s.chance(flake)
	}
	s.started++
	s.builds[b.SHA] = sb
}

func (s *Sim) eject(a queue.Action) {
	p := s.prs[a.PR]
	p.queued = false
	s.Ejected[a.Cause]++
	s.Left[a.PR] = a.Cause
	switch a.Cause {
	case "moved":
		p.back = s.now + 10*time.Minute
	case "conflict":
		if !p.conflict && !(p.ahead > 0 && s.holds[s.trunk][p.ahead]) {
			s.broke(3, "#%d was ejected for a conflict it does not have", a.PR)
		}
		// Its author merges the trunk in and queues it again.
		p.conflict, p.ahead, p.back = false, 0, s.now+30*time.Minute
		p.Head = fmt.Sprintf("%s+", p.Head)
		s.byHead[p.Head] = p.Number
	case "culprit":
		if s.culprit(p) {
			p.gone = true
			s.Blames = append(s.Blames, float64(s.now-p.firstRed)/float64(s.shape.Build))
			return
		}
		s.Wrong++
		p.back = s.now + 10*time.Minute
		// The failures it was ejected for all began while the trunk's own tip failed.
		under := s.red(s.holds[s.trunk], s.now)
		for _, sha := range a.Evidence {
			if b := s.builds[sha]; b != nil && b.Verdict == queue.Failure {
				under = s.red(s.holds[s.trunk], b.start)
				if !under {
					break
				}
			}
		}
		if under {
			s.broke(8, "#%d was ejected while the trunk's own tip fails", a.PR)
		}
		if !s.flakes() {
			s.broke(3, "#%d is innocent and was ejected with no flake in play", a.PR)
		}
	}
}

func (s *Sim) land(a queue.Action) {
	b := s.builds[a.SHA]
	if b == nil || b.Kind != queue.Candidate || b.Verdict != queue.Success {
		s.broke(1, "the trunk was moved to %.12s, which no whole build passed", a.SHA)
		return
	}
	for _, m := range a.Members {
		p := s.prs[m.Number]
		if !p.queued || p.Head != m.Head || p.Pinned != m.Head {
			s.broke(2, "#%d landed at %.12s, not the head it is queued at", m.Number, m.Head)
		}
		if p.landed {
			continue
		}
		p.landed, p.queued = true, false
		s.Landed++
		s.Waits = append(s.Waits, float64(s.now-p.arrived)/float64(s.shape.Build))
	}
	s.trunk, s.LastLand = a.SHA, s.now
	delete(s.builds, a.SHA)
}

// Run plays the shape to its end: until the arrivals have stopped and the queue is empty, or
// far longer than any queue should take, which breaks the fourth invariant.
func (s *Sim) Run() {
	limit := time.Duration(s.shape.Hours*float64(time.Hour)) + time.Duration(60+2*len(s.prs)+int(4*s.shape.Arrivals*s.shape.Hours))*s.shape.Build
	for _, e := range s.shape.Episodes {
		limit += e.For + e.Fix
	}
	for s.now < limit && len(s.Broken) == 0 {
		s.Step()
		if s.now.Hours() >= s.shape.Hours && s.idle() {
			return
		}
	}
	if len(s.Broken) > 0 {
		return
	}
	for _, n := range s.numbers() {
		if p := s.prs[n]; p.queued && p.Wait == "" && p.Checks == queue.Success {
			s.broke(4, "#%d was still queued after %s", n, limit)
			return
		}
	}
}

// idle says nothing is queued or coming back, and no episode is still to come.
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
