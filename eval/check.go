package main

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

type checkedPR struct {
	number int
	head   queue.SHA
	breaks bool
	clash  int
	needs  int
	queued bool
	landed bool
	first  bool
	order  uint8
	checks queue.Verdict
	wait   string
}

type checkedBuild struct {
	kind    queue.Kind
	verdict queue.Verdict
	start   int64
	holds   map[int]bool
	heads   map[int]queue.SHA
}

func brokeMsg(n int, format string, args ...any) string {
	return fmt.Sprintf("invariant %d: %s", n, fmt.Sprintf(format, args...))
}

func culpritPR(number int, prs []checkedPR, failed []checkedBuild) bool {
	idx := slices.IndexFunc(prs, func(p checkedPR) bool { return p.number == number })
	if idx < 0 {
		return false
	}
	pr := prs[idx]
	if pr.breaks {
		return true
	}
	if pr.needs > 0 {
		needed := slices.IndexFunc(prs, func(p checkedPR) bool { return p.number == pr.needs })
		if needed < 0 || !(prs[needed].landed || readyAhead(prs[needed], pr)) {
			return true
		}
	}
	for _, o := range prs {
		if (o.number == pr.clash || o.clash == number) && (o.landed || o.queued && slices.ContainsFunc(failed, func(b checkedBuild) bool { return b.holds[o.number] })) {
			return true
		}
	}
	return false
}

func readyAhead(a, b checkedPR) bool {
	switch {
	case !a.queued || a.checks != queue.Success || a.wait != "":
		return false
	case a.first != b.first:
		return a.first
	case a.order != b.order:
		return a.order < b.order
	}
	return a.number < b.number
}

// fairTest says an ejection's evidence is a fair test of its pull request, by when CI really began
// each build: the first is a build that passed without it, and among the rest are two builds
// of that tree with it that failed, one begun no later than the pass and one no earlier.
func fairTest(a queue.Action, built func(queue.SHA) (checkedBuild, bool)) bool {
	pass, failed, ok := fairEvidence(a, built)
	var before, after int
	for _, b := range failed {
		if b.start <= pass.start {
			before++
		}
		if b.start >= pass.start {
			after++
		}
	}
	return ok && pass.verdict == queue.Success && !pass.holds[a.PR] && len(failed) >= 2 && before >= 1 && after >= 1
}

// fairEvidence splits an ejection's evidence into its first build, the pass without it, and
// the builds after that failed holding it beside exactly the pass's pull requests.
func fairEvidence(a queue.Action, built func(queue.SHA) (checkedBuild, bool)) (checkedBuild, []checkedBuild, bool) {
	if len(a.Evidence) == 0 {
		return checkedBuild{}, nil, false
	}
	pass, ok := built(a.Evidence[0])
	if !ok {
		return checkedBuild{}, nil, false
	}
	without := pass.holds
	var failed []checkedBuild
	for _, sha := range a.Evidence[1:] {
		b, ok := built(sha)
		with := b.holds
		same := ok && b.verdict == queue.Failure && with[a.PR] && len(with) == len(without)+1
		for n := range without {
			same = same && with[n]
		}
		if same {
			failed = append(failed, b)
		}
	}
	return pass, failed, true
}

func checkPass(pass int, acts []queue.Action) string {
	if pass == 6 && len(acts) > 0 {
		return brokeMsg(5, "the tick still had %d actions after six passes, the first %s", len(acts), acts[0].Do)
	}
	return ""
}

func checkStill(acts []queue.Action) string {
	if len(acts) > 0 {
		return brokeMsg(5, "a tick on an unchanged world acted with %s", acts[0].Do)
	}
	return ""
}

func checkKeep(w queue.World, c queue.Config, acts []queue.Action) string {
	if c.Keep <= 0 {
		return ""
	}
	without := w
	without.Builds = nil
	landed, due := map[queue.SHA]bool{}, map[queue.SHA]bool{}
	for _, b := range w.Builds {
		if b.Landed.IsZero() {
			without.Builds = append(without.Builds, b)
			continue
		}
		landed[b.SHA] = true
		if w.Now.Sub(b.Landed) >= c.Keep {
			due[b.SHA] = true
		}
	}
	var rest []queue.Action
	for _, a := range acts {
		if a.Do == "delete" && landed[a.SHA] {
			if !due[a.SHA] {
				return fmt.Sprintf("keep inertness: unexpected delete of landed build %.12s", a.SHA)
			}
			delete(due, a.SHA)
		} else {
			rest = append(rest, a)
		}
	}
	if len(due) > 0 {
		var shas []string
		for sha := range due {
			shas = append(shas, sha)
		}
		slices.Sort(shas)
		return fmt.Sprintf("keep inertness: missing due deletes %v", shas)
	}
	unset := c
	unset.Keep = 0
	want := queue.Plan(without, unset)
	equal := func(a, b []queue.Action) bool {
		return slices.EqualFunc(a, b, func(a, b queue.Action) bool { return reflect.DeepEqual(a, b) })
	}
	if !equal(rest, want) {
		format := func(acts []queue.Action) []string {
			var out []string
			for _, a := range acts {
				out = append(out, formatAction(a))
			}
			return out
		}
		return fmt.Sprintf("keep inertness: actions %v, want %v with marked builds removed and keep unset", format(rest), format(want))
	}
	return ""
}

func checkPush(seen bool, sha queue.SHA) string {
	if seen {
		return brokeMsg(7, "%.12s was pushed for a second build", sha)
	}
	return ""
}

func checkEject(a queue.Action, culprit, flaked bool, built func(queue.SHA) (checkedBuild, bool)) []string {
	if a.Cause != "culprit" {
		return []string{brokeMsg(3, "#%d was ejected as %q, which is no cause to leave", a.PR, a.Cause)}
	}
	var broken []string
	if !fairTest(a, built) {
		broken = append(broken, brokeMsg(8, "#%d was blamed with no fair test in %.7s", a.PR, a.Evidence))
	}
	if !culprit && !flaked {
		broken = append(broken, brokeMsg(3, "#%d is innocent and was ejected with no flake in play", a.PR))
	}
	return broken
}

func checkLand(a queue.Action, built func(queue.SHA) (checkedBuild, bool), pr func(int) checkedPR) ([]string, bool) {
	b, ok := built(a.SHA)
	if !ok || b.kind != queue.Candidate || b.verdict != queue.Success {
		return []string{brokeMsg(1, "the trunk was moved to %.12s, which no whole build passed", a.SHA)}, false
	}
	var broken []string
	for _, m := range a.Members {
		p := pr(m.Number)
		if !p.queued || p.head != m.Head || p.checks != queue.Success || (len(b.heads) > 0 && b.heads[m.Number] != m.Head) {
			broken = append(broken, brokeMsg(2, "#%d landed at %.12s, which is not its head with its checks passed", m.Number, m.Head))
		}
	}
	return broken, true
}

func checkDrain(prs []checkedPR, after string) string {
	for _, p := range prs {
		if p.queued && p.wait == "" && p.checks == queue.Success {
			return brokeMsg(4, "#%d was still queued after %s", p.number, after)
		}
	}
	return ""
}

type pullSpec struct {
	Breaks, Conflict, Unready, First, Hold bool
	Clash, Needs, Ahead                    int
}

type bounds struct {
	Flakes, Lost, Timeouts, Episodes, Moves, Outside, Crashes, Stale, MaxStates int
}

type checkWorld struct {
	Name            string
	Config          queue.Config
	Pulls           []pullSpec
	Silent, Instant bool
	Bounds          bounds
}

type stepKind uint8

const (
	stepNone stepKind = iota
	stepTick
	stepDrain
	stepCrash
	stepBegin
	stepPass
	stepFail
	stepFlake
	stepError
	stepLost
	stepRecheck
	stepTimeout
	stepKeep
	stepTrunkRed
	stepTrunkGreen
	stepSync
	stepMove
	stepArriving
	stepUnlabel
	stepRelabel
	stepChecks
	stepHold
	stepFirst
	stepOutside
)

var stepNames = [...]string{
	stepTick:       "tick",
	stepDrain:      "drain",
	stepBegin:      "ci begin",
	stepPass:       "ci pass",
	stepFail:       "ci fail",
	stepFlake:      "ci flake",
	stepError:      "ci error",
	stepLost:       "ci lost",
	stepRecheck:    "time recheck",
	stepTimeout:    "time timeout",
	stepKeep:       "time keep",
	stepTrunkRed:   "trunk red",
	stepTrunkGreen: "trunk green",
	stepSync:       "hub sync",
	stepMove:       "move head",
	stepArriving:   "push arriving",
	stepUnlabel:    "unlabel",
	stepRelabel:    "relabel",
	stepChecks:     "checks",
	stepHold:       "hold",
	stepFirst:      "first",
	stepOutside:    "outside trunk",
}

type step struct {
	kind    stepKind
	stale   bool
	after   string
	at      int
	pull    int
	sha     queue.SHA
	verdict queue.Verdict
	on      bool
}

func (s step) String() string {
	if s.stale {
		s.stale = false
		return "stale tick after " + s.String()
	}
	name := stepNames[s.kind]
	switch {
	case s.kind == stepCrash && s.sha != "":
		return fmt.Sprintf("crash after %s %.7s", s.after, s.sha)
	case s.kind == stepCrash:
		return fmt.Sprintf("crash after %s #%d", s.after, s.pull)
	case s.kind == stepChecks:
		return fmt.Sprintf("checks #%d=%d", s.pull, s.verdict)
	case s.kind == stepHold, s.kind == stepFirst:
		return fmt.Sprintf("%s #%d=%v", name, s.pull, s.on)
	case s.sha != "":
		return fmt.Sprintf("%s %.7s", name, s.sha)
	case s.pull != 0:
		return fmt.Sprintf("%s #%d", name, s.pull)
	default:
		return name
	}
}

type checkResult struct {
	States, Depth int
	Capped        bool
	Broken        string
	Trace         []step
}

type checkCommit struct {
	parent, head, of, plain queue.SHA
	pull, round             int
	kind                    queue.Kind
	holds                   uint16
}

type checker struct {
	world       checkWorld
	plan        func(queue.World, queue.Config) []queue.Action
	verify      func(queue.World, queue.Action) error
	commits     map[queue.SHA]checkCommit
	markRunning bool
	settled     map[string]string
}

func newChecker(w checkWorld, m mutant) *checker {
	return &checker{
		world:       w,
		markRunning: m.markRunning,
		plan:        m.plan,
		verify:      m.verify,
		commits:     map[queue.SHA]checkCommit{"trunk0": {}},
		settled:     map[string]string{},
	}
}

func pullBit(n int) uint16 {
	if n <= 0 || n > 16 {
		return 0
	}
	return 1 << uint(n-1)
}

func trailers(note string) (kind queue.Kind, round int, of queue.SHA, isTry bool) {
	kind = queue.Candidate
	for _, line := range strings.Split(note, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Banyan-Round":
			round, _ = strconv.Atoi(v)
		case "Banyan-Of":
			of = queue.SHA(v)
		case "Banyan-Check":
			kind = queue.Check
		case "Banyan-Probe-Of":
			kind = queue.Probe
		case "Banyan-Try":
			isTry = true
		}
	}
	return kind, round, of, isTry
}

func (c *checker) addCommit(parent, head queue.SHA, pull int, note string) queue.SHA {
	sha := queue.SHA(fmt.Sprintf("%x", sha1.Sum([]byte(parent+"|"+head+"|"+note))))
	if _, ok := c.commits[sha]; ok {
		return sha
	}
	kind, round, of, isTry := trailers(note)
	var plain queue.SHA
	if isTry && pull > 0 && kind == queue.Candidate {
		plain = c.addCommit(parent, head, pull, "")
	}
	c.commits[sha] = checkCommit{
		parent: parent, head: head, pull: pull, kind: kind,
		round: round, of: of, plain: plain, holds: c.commits[parent].holds | pullBit(pull),
	}
	return sha
}

func (c *checker) holds(trunk, sha queue.SHA) bool {
	if sha == "" {
		return false
	}
	for cur := trunk; cur != ""; {
		if cur == sha {
			return true
		}
		cm, ok := c.commits[cur]
		if !ok {
			return false
		}
		if cm.head == sha {
			return true
		}
		cur = cm.parent
	}
	return false
}

func (c *checker) build(sha, trunk queue.SHA) queue.Build {
	top := c.commits[sha]
	b := queue.Build{SHA: sha, Kind: top.kind, Round: top.round, Of: top.of}
	at := sha
	switch {
	case top.kind == queue.Probe || top.kind == queue.Check:
		at = top.parent
	case top.plain != "":
		b.Chain = []queue.Member{{Number: top.pull, Head: top.head, Merge: top.plain}}
		at = top.parent
	}
	for !c.holds(trunk, at) {
		cm, ok := c.commits[at]
		if !ok || cm.pull == 0 || cm.head == "" {
			break
		}
		b.Chain = append([]queue.Member{{Number: cm.pull, Head: cm.head, Merge: at}}, b.Chain...)
		at = cm.parent
	}
	b.Base = at
	return b
}

func (c *checker) guilty(sha queue.SHA) bool {
	mask := c.commits[sha].holds
	for i, p := range c.world.Pulls {
		if mask&pullBit(i+1) != 0 && (p.Breaks || (p.Clash > 0 && mask&pullBit(p.Clash) != 0) || (p.Needs > 0 && mask&pullBit(p.Needs) == 0)) {
			return true
		}
	}
	return false
}

func (c *checker) tree(sha queue.SHA) map[int]bool {
	mask := c.commits[sha].holds
	out := make(map[int]bool)
	for i := 1; i <= len(c.world.Pulls); i++ {
		if mask&pullBit(i) != 0 {
			out[i] = true
		}
	}
	return out
}

type pullRequest struct {
	gitHead, githubHead, order, ahead, statusHead uint8
	queued, first, hold, merged, conflict         bool
	commented                                     bool
	checks, status                                queue.Verdict
	statusText                                    string
}

func (p pullRequest) head(number int, gen uint8) queue.SHA {
	if gen == 0 {
		return queue.SHA(fmt.Sprintf("head%d", number))
	}
	return queue.SHA(fmt.Sprintf("head%d_%d", number, gen))
}

func (p pullRequest) waitReason() string {
	switch {
	case p.hold:
		return "it is labelled hold"
	case p.gitHead != p.githubHead:
		return "its newest push is still arriving"
	default:
		return ""
	}
}

type pushedBuild struct {
	sha                                           queue.SHA
	branch, marked, begun, flaked, lost, redTrunk bool
	verdict                                       queue.Verdict
	pushed, started, landed, past                 uint16
}

type worldState struct {
	trunk                                                                                            queue.SHA
	outsideGen, episodes, nextOrder                                                                  uint8
	trunkRed, flaked, crashed                                                                        bool
	flakesLeft, lostLeft, timeoutsLeft, episodesLeft, movesLeft, outsideLeft, crashesLeft, staleLeft uint8
	nextSeq, rechecked, timedOut, kept                                                               uint16
	pulls                                                                                            []pullRequest
	builds                                                                                           []pushedBuild
	seen                                                                                             []queue.SHA
}

func (c *checker) initState() *worldState {
	prs := make([]pullRequest, len(c.world.Pulls))
	var nextOrder uint8 = 1
	for i, p := range c.world.Pulls {
		checks := queue.Success
		if p.Unready {
			checks = queue.Pending
		}
		prs[i] = pullRequest{
			queued: true, order: nextOrder, first: p.First, hold: p.Hold,
			checks: checks, conflict: p.Conflict, ahead: uint8(p.Ahead),
		}
		nextOrder++
	}
	b := c.world.Bounds
	return &worldState{
		trunk: "trunk0", pulls: prs,
		flakesLeft: uint8(b.Flakes), lostLeft: uint8(b.Lost), timeoutsLeft: uint8(b.Timeouts),
		episodesLeft: uint8(b.Episodes), movesLeft: uint8(b.Moves), outsideLeft: uint8(b.Outside),
		crashesLeft: uint8(b.Crashes), staleLeft: uint8(b.Stale), nextSeq: 1, nextOrder: nextOrder,
	}
}

func (s *worldState) clone() *worldState {
	c := *s
	c.pulls = slices.Clone(s.pulls)
	c.builds = slices.Clone(s.builds)
	c.seen = slices.Clone(s.seen)
	return &c
}

func (s *worldState) sort() {
	sort.Slice(s.builds, func(i, j int) bool { return s.builds[i].sha < s.builds[j].sha })
	sort.Slice(s.seen, func(i, j int) bool { return s.seen[i] < s.seen[j] })
}

func (s *worldState) seenSHA(sha queue.SHA) bool {
	for _, b := range s.builds {
		if b.sha == sha && (b.branch || b.marked) {
			return true
		}
	}
	_, ok := slices.BinarySearch(s.seen, sha)
	return ok
}

func (s *worldState) findBuild(sha queue.SHA) int {
	return slices.IndexFunc(s.builds, func(b pushedBuild) bool { return b.sha == sha })
}

func (s *worldState) markSeen(sha queue.SHA) {
	if idx, ok := slices.BinarySearch(s.seen, sha); !ok {
		s.seen = slices.Insert(s.seen, idx, sha)
	}
}

func (s *worldState) nextEvent() uint16 {
	v := s.nextSeq
	s.nextSeq++
	return v
}

func (s *worldState) begin(b *pushedBuild) {
	if !b.begun {
		b.begun = true
		b.started = s.nextEvent()
		b.redTrunk = s.trunkRed
	}
}

func rank(vals []uint16) []uint16 {
	var unique []uint16
	for _, v := range vals {
		if v > 0 && !slices.Contains(unique, v) {
			unique = append(unique, v)
		}
	}
	slices.Sort(unique)
	out := make([]uint16, len(vals))
	for i, v := range vals {
		if v > 0 {
			idx, _ := slices.BinarySearch(unique, v)
			out[i] = uint16(idx + 1)
		}
	}
	return out
}

func cut(pushes []uint16, seq uint16) uint16 {
	idx, ok := slices.BinarySearch(pushes, seq)
	if ok {
		return uint16(idx + 1)
	}
	return uint16(idx)
}

func (s *worldState) clockRanks() (qRank, pRank, sRank, aRank, pushes, marks []uint16) {
	qo := make([]uint16, len(s.pulls))
	for i, p := range s.pulls {
		if p.queued {
			qo[i] = uint16(p.order)
		}
	}
	events := make([]uint16, 3*len(s.builds))
	for i, b := range s.builds {
		if b.branch {
			events[i] = b.pushed
			pushes = append(pushes, b.pushed)
			if b.landed > 0 {
				events[2*len(s.builds)+i] = b.landed
				marks = append(marks, b.landed)
			}
			if b.begun {
				events[len(s.builds)+i] = b.started
			}
		}
	}
	slices.Sort(pushes)
	slices.Sort(marks)
	er := rank(events)
	n := len(s.builds)
	return rank(qo), er[:n], er[n : 2*n], er[2*n:], pushes, marks
}

func packFlags(bits ...bool) byte {
	var b byte
	for i, on := range bits {
		if on {
			b |= 1 << i
		}
	}
	return b
}

// key identifies a state up to queue order, the order of build pushes, starts and marks,
// and elapsed rechecks, timeouts and keeps.
func (s *worldState) key() string {
	s.sort()
	qRank, pRank, sRank, aRank, pushes, marks := s.clockRanks()
	var buf []byte
	buf = append(buf, s.trunk...)
	buf = append(buf, 0, s.outsideGen)
	flags := packFlags(s.trunkRed, s.flaked, s.crashed)
	buf = append(buf, flags, s.episodes, s.flakesLeft, s.lostLeft, s.timeoutsLeft, s.episodesLeft, s.movesLeft, s.outsideLeft, s.crashesLeft, s.staleLeft)
	buf = binary.AppendUvarint(buf, uint64(cut(pushes, s.rechecked)))
	buf = binary.AppendUvarint(buf, uint64(cut(pushes, s.timedOut)))
	buf = binary.AppendUvarint(buf, uint64(cut(marks, s.kept)))
	buf = append(buf, byte(len(s.pulls)))
	for i, p := range s.pulls {
		pf := packFlags(p.queued, p.first, p.hold, p.merged, p.conflict, p.commented)
		buf = append(buf, pf, p.gitHead, p.githubHead, byte(qRank[i]), byte(p.checks), p.ahead, p.statusHead, byte(p.status))
		buf = binary.AppendVarint(buf, int64(len(p.statusText)))
		buf = append(buf, p.statusText...)
	}
	buf = binary.AppendVarint(buf, int64(len(s.builds)))
	for i, b := range s.builds {
		bf := packFlags(b.branch, b.marked, b.begun, b.flaked, b.redTrunk, b.lost)
		buf = append(buf, bf, byte(b.verdict))
		buf = binary.AppendUvarint(buf, uint64(cut(pushes, b.past)))
		buf = binary.AppendUvarint(buf, uint64(pRank[i]))
		buf = binary.AppendUvarint(buf, uint64(sRank[i]))
		buf = binary.AppendUvarint(buf, uint64(aRank[i]))
		buf = append(buf, b.sha...)
		buf = append(buf, 0)
	}
	buf = binary.AppendVarint(buf, int64(len(s.seen)))
	for _, sha := range s.seen {
		buf = append(buf, sha...)
		buf = append(buf, 0)
	}
	return string(buf)
}

type worldRepo struct {
	c *checker
	s *worldState
}

func (r worldRepo) Merge(parent queue.SHA, pr queue.PR, note string) (queue.SHA, bool) {
	if pr.Number <= 0 || pr.Number > len(r.s.pulls) {
		return "", false
	}
	p := r.s.pulls[pr.Number-1]
	if p.conflict || (p.ahead > 0 && r.c.commits[parent].holds&pullBit(int(p.ahead)) != 0) {
		return "", false
	}
	return r.c.addCommit(parent, pr.Head, pr.Number, note), true
}

func (r worldRepo) Mark(parent queue.SHA, note string) queue.SHA {
	return r.c.addCommit(parent, "", 0, note)
}

func (r worldRepo) Seen(sha queue.SHA) bool {
	return r.s.seenSHA(sha)
}

var checkEpoch = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func (c *checker) buildTimes(s *worldState, pushSeqs []uint16) (x []time.Duration, nodeOf map[uint16]int, nowNode int) {
	var pushes []pushedBuild
	var events []uint16
	for _, b := range s.builds {
		if !b.branch {
			continue
		}
		pushes = append(pushes, b)
		events = append(events, b.pushed)
		if b.begun && !c.world.Silent {
			events = append(events, b.started)
		}
		if b.landed > 0 {
			events = append(events, b.landed)
		}
	}
	sort.Slice(pushes, func(i, j int) bool { return pushes[i].pushed < pushes[j].pushed })
	slices.Sort(events)
	events = slices.Compact(events)
	m := len(events)
	nodeOf = make(map[uint16]int, m)
	for i, ev := range events {
		nodeOf[ev] = i
	}
	k := len(pushes)
	q := make([]int, k+1)
	for r, b := range pushes {
		q[r] = nodeOf[b.pushed]
	}
	q[k] = m

	recheck, timeout, keep := c.world.Config.Recheck, c.world.Config.Timeout, c.world.Config.Keep
	step := time.Second
	for _, d := range []time.Duration{recheck, timeout, keep, timeout - recheck, recheck - timeout, keep - recheck, recheck - keep, keep - timeout, timeout - keep} {
		if d > 0 && time.Duration(4*(m+2))*step > d {
			step = max(d/time.Duration(4*(m+2)), time.Nanosecond)
		}
	}

	type edge struct {
		u, v int
		w    time.Duration
	}
	var edges []edge
	for i := 0; i < m; i++ {
		edges = append(edges, edge{i + 1, i, -step})
	}
	if timeout > 0 {
		for r := 0; r <= k; r++ {
			seq := s.timedOut
			if r < k {
				seq = pushes[r].past
			}
			tc := int(cut(pushSeqs, seq))
			if tc > 0 {
				edges = append(edges, edge{q[r], q[tc-1], -(timeout + step)})
			}
			if tc < r {
				for j := tc; j < r; j++ {
					if pushes[j].verdict == queue.Pending {
						edges = append(edges, edge{q[j], q[r], timeout})
					}
				}
			}
		}
	}
	if recheck > 0 {
		rc := int(cut(pushSeqs, s.rechecked))
		if rc > 0 {
			edges = append(edges, edge{m, q[rc-1], -recheck})
		}
		if rc < k {
			edges = append(edges, edge{q[rc], m, recheck - step})
		}
	}

	if keep > 0 {
		for _, b := range pushes {
			if b.landed == 0 {
				continue
			}
			a := nodeOf[b.landed]
			if b.landed <= s.kept {
				edges = append(edges, edge{m, a, -keep})
			} else {
				edges = append(edges, edge{a, m, keep - step})
			}
		}
	}
	x = make([]time.Duration, m+1)
	for pass := 0; pass <= m+1; pass++ {
		changed := false
		for _, e := range edges {
			if cand := x[e.u] + e.w; cand < x[e.v] {
				if pass == m+1 {
					return nil, nil, 0
				}
				x[e.v] = cand
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return x, nodeOf, m
}

func (c *checker) read(s *worldState) (queue.World, map[int]pullRequest) {
	s.sort()
	qRank, _, _, _, pushSeqs, _ := s.clockRanks()
	now := checkEpoch.Add(24 * time.Hour)

	pulls := map[int]pullRequest{}
	w := queue.World{Now: now, Trunk: s.trunk, Repo: worldRepo{c: c, s: s}}
	for i, p := range s.pulls {
		num := i + 1
		if !p.queued || p.merged || c.holds(s.trunk, p.head(num, p.gitHead)) {
			continue
		}
		qpr := queue.PR{
			Number: num, Head: p.head(num, p.githubHead),
			Queued: checkEpoch.Add(time.Duration(qRank[i]) * time.Minute),
			First:  p.first, Checks: p.checks, Wait: p.waitReason(),
		}
		if p.statusHead == p.githubHead && p.status == queue.Pending {
			qpr.Note = p.statusText
		}
		w.PRs = append(w.PRs, qpr)
		pulls[num] = p
	}
	if len(pushSeqs) == 0 {
		return w, pulls
	}
	x, nodeOf, nowNode := c.buildTimes(s, pushSeqs)
	if x == nil {
		panic("infeasible clock constraints")
	}
	for _, b := range s.builds {
		if !b.branch {
			continue
		}
		qb := c.build(b.sha, s.trunk)
		qb.Verdict = b.verdict
		qb.Pushed = now.Add(x[nodeOf[b.pushed]] - x[nowNode])
		if b.landed > 0 {
			qb.Landed = now.Add(x[nodeOf[b.landed]] - x[nowNode])
		}
		if b.begun && !c.world.Silent {
			qb.Started = now.Add(x[nodeOf[b.started]] - x[nowNode])
		}
		w.Builds = append(w.Builds, qb)
	}
	return w, pulls
}

func (c *checker) checkedBuild(s *worldState) func(queue.SHA) (checkedBuild, bool) {
	return func(sha queue.SHA) (checkedBuild, bool) {
		idx := s.findBuild(sha)
		if idx < 0 || !s.builds[idx].branch {
			return checkedBuild{}, false
		}
		b := s.builds[idx]
		qb := c.build(sha, s.trunk)
		heads := make(map[int]queue.SHA, len(qb.Chain))
		for _, m := range qb.Chain {
			heads[m.Number] = m.Head
		}
		return checkedBuild{kind: qb.Kind, verdict: b.verdict, start: int64(b.started), holds: c.tree(sha), heads: heads}, true
	}
}

func (c *checker) checkedPull(s *worldState, number int) checkedPR {
	if number <= 0 || number > len(s.pulls) {
		return checkedPR{number: number}
	}
	p := s.pulls[number-1]
	landed := c.holds(s.trunk, p.head(number, p.gitHead))
	return checkedPR{
		number: number, head: p.head(number, p.githubHead),
		breaks: c.world.Pulls[number-1].Breaks, clash: c.world.Pulls[number-1].Clash, needs: c.world.Pulls[number-1].Needs,
		queued: p.queued && !landed && !p.merged, landed: landed, first: p.first, order: p.order,
		checks: p.checks, wait: p.waitReason(),
	}
}

func (c *checker) checkedPulls(s *worldState) []checkedPR {
	out := make([]checkedPR, len(s.pulls))
	for i := range s.pulls {
		out[i] = c.checkedPull(s, i+1)
	}
	return out
}

type transition struct {
	step  step
	state *worldState
}

type tickRead struct {
	world   queue.World
	actions []queue.Action
}

type tickOutcome struct {
	final   *worldState
	changed bool
	crashes []transition
	broken  string
	reads   []tickRead
}

type checkTick struct {
	c          *checker
	state      *worldState
	stale      *queue.World
	stalePulls map[int]pullRequest
	crash      bool
	trace      bool
	stopAt     int

	origKey string
	crashes []transition
	reads   []tickRead
	refused queue.SHA
	effects int
}

func (t *checkTick) run() tickOutcome {
	orig := t.state
	t.origKey = orig.key()
	t.state = orig.clone()
	if !t.settle() {
		return t.done()
	}
	for pass := 0; ; pass++ {
		var w queue.World
		var pulls map[int]pullRequest
		if pass == 0 && t.stale != nil {
			w, pulls = *t.stale, t.stalePulls
		} else {
			var ok bool
			if w, pulls, ok = t.read(); !ok {
				return t.done()
			}
		}
		acts := t.c.plan(w, t.c.world.Config)
		if msg := checkKeep(w, t.c.world.Config, acts); msg != "" {
			return tickOutcome{broken: msg, reads: append(t.reads, tickRead{world: w, actions: acts})}
		}
		if t.trace {
			t.reads = append(t.reads, tickRead{world: w, actions: slices.Clone(acts)})
		}
		if msg := checkPass(pass, acts); msg != "" {
			return tickOutcome{broken: msg, reads: t.reads}
		}
		if len(acts) == 0 {
			break
		}
		for _, a := range acts {
			var broken string
			var stop bool
			switch a.Do {
			case "note":
				stop = t.note(pulls, a)
			case "push":
				broken, stop = t.push(a)
			case "delete":
				stop = t.delete(a)
			case "eject":
				broken, stop = t.eject(pulls, a)
			case "land":
				broken, stop = t.land(a)
			}
			if broken != "" {
				return tickOutcome{broken: broken, reads: t.reads}
			}
			if stop {
				return t.done()
			}
		}
	}
	if t.stale == nil {
		finalW, _ := t.c.read(t.state)
		if still := t.c.plan(finalW, t.c.world.Config); len(still) > 0 {
			if t.trace {
				t.reads = append(t.reads, tickRead{world: finalW, actions: slices.Clone(still)})
			}
			if msg := checkStill(still); msg != "" {
				return tickOutcome{broken: msg, reads: t.reads}
			}
		}
	}
	return t.done()
}

func (t *checkTick) effect(after string, pull int, sha queue.SHA) bool {
	t.effects++
	if t.stopAt > 0 && t.effects == t.stopAt {
		t.state.crashesLeft--
		t.state.crashed = true
		return false
	}
	if t.crash && t.state.crashesLeft > 0 {
		snap := t.state.clone()
		snap.crashesLeft--
		snap.crashed = true
		s := step{kind: stepCrash, after: after, at: t.effects, pull: pull, sha: sha}
		t.crashes = append(t.crashes, transition{step: s, state: snap})
	}
	return true
}

func (t *checkTick) done() tickOutcome {
	finalKey := t.state.key()
	var kept []transition
	seen := map[string]bool{}
	for _, br := range t.crashes {
		k := br.state.key()
		if k != t.origKey && k != finalKey && !seen[k] {
			seen[k] = true
			kept = append(kept, br)
		}
	}
	return tickOutcome{final: t.state, changed: finalKey != t.origKey, crashes: kept, reads: t.reads}
}

func (t *checkTick) settle() bool {
	for i := range t.state.pulls {
		p := &t.state.pulls[i]
		if p.queued && p.merged && !(p.statusHead == p.githubHead && p.status == queue.Success) {
			p.statusHead, p.status, p.statusText = p.githubHead, queue.Success, "landed"
			if !t.effect("settle", i+1, "") {
				return false
			}
		}
	}
	return true
}

func (t *checkTick) read() (queue.World, map[int]pullRequest, bool) {
	for i := range t.state.builds {
		if b := &t.state.builds[i]; b.branch && !b.marked {
			b.marked, b.pushed, b.past = true, t.state.nextEvent(), t.state.timedOut
			if !t.effect("read marked", 0, b.sha) {
				return queue.World{}, nil, false
			}
		}
	}
	if t.c.world.Config.Keep > 0 {
		for i := range t.state.builds {
			b := &t.state.builds[i]
			cm := t.c.commits[b.sha]
			if b.branch && b.landed == 0 && cm.kind == queue.Candidate && cm.round == 0 && t.c.holds(t.state.trunk, b.sha) && (b.verdict != queue.Pending || t.c.markRunning) {
				b.landed = t.state.nextEvent()
				if !t.effect("read landed", 0, b.sha) {
					return queue.World{}, nil, false
				}
			}
		}
	}
	w, pulls := t.c.read(t.state)
	return w, pulls, true
}

func (t *checkTick) note(pulls map[int]pullRequest, a queue.Action) bool {
	pr, ok := pulls[a.PR]
	if !ok || a.PR <= 0 || a.PR > len(t.state.pulls) {
		return false
	}
	pull := &t.state.pulls[a.PR-1]
	if pr.githubHead != pull.githubHead {
		return false
	}
	pull.statusHead, pull.status, pull.statusText = pr.githubHead, queue.Pending, a.Text
	return !t.effect("note", a.PR, "")
}

func (t *checkTick) push(a queue.Action) (string, bool) {
	if msg := checkPush(t.state.seenSHA(a.Build.SHA), a.Build.SHA); msg != "" {
		return msg, false
	}
	b := pushedBuild{sha: a.Build.SHA, branch: true, verdict: queue.Pending, pushed: t.state.nextEvent(), past: t.state.timedOut}
	if t.c.world.Instant {
		t.state.begin(&b)
	}
	t.state.builds = append(t.state.builds, b)
	if !t.c.world.Config.Cancels {
		running := 0
		underTrunk := false
		for _, live := range t.state.builds {
			if live.branch && live.verdict == queue.Pending && live.pushed > t.state.timedOut && t.c.commits[live.sha].kind == queue.Candidate {
				running++
				underTrunk = underTrunk || t.c.holds(t.state.trunk, live.sha)
			}
		}
		if underTrunk && running > t.c.world.Config.Slots {
			return fmt.Sprintf("slots: %d candidates running with %d slots", running, t.c.world.Config.Slots), false
		}
	}
	if !t.effect("git push", 0, a.Build.SHA) {
		return "", true
	}
	t.state.builds[t.state.findBuild(a.Build.SHA)].marked = true
	return "", !t.effect("hub mark", 0, a.Build.SHA)
}

func (t *checkTick) delete(a queue.Action) bool {
	idx := t.state.findBuild(a.SHA)
	if idx < 0 || !t.state.builds[idx].branch {
		return false
	}
	t.state.builds[idx].branch = false
	if t.state.builds[idx].marked {
		t.state.markSeen(a.SHA)
	}
	t.state.builds = slices.Delete(t.state.builds, idx, idx+1)
	return !t.effect("delete", 0, a.SHA)
}

func (t *checkTick) eject(pulls map[int]pullRequest, a queue.Action) (string, bool) {
	built := t.c.checkedBuild(t.state)
	_, failed, _ := fairEvidence(a, built)
	culprit := a.Cause == "culprit" && culpritPR(a.PR, t.c.drainPulls(t.state), failed)
	flaked := t.state.flaked || t.state.episodes > 1
	if broken := checkEject(a, culprit, flaked, built); len(broken) > 0 {
		return strings.Join(broken, "; "), false
	}
	pr, ok := pulls[a.PR]
	if !ok || a.PR <= 0 || a.PR > len(t.state.pulls) {
		return "", false
	}
	pull := &t.state.pulls[a.PR-1]
	if !pull.commented || pr.githubHead != pull.githubHead {
		pull.commented = true
		if !t.effect("eject comment", a.PR, "") {
			return "", true
		}
	}
	if pr.githubHead == pull.githubHead {
		pull.statusHead, pull.status, pull.statusText = pr.githubHead, queue.Failure, "left the queue: "+a.Text
		if !t.effect("eject status", a.PR, "") {
			return "", true
		}
	}
	if pull.queued {
		pull.queued, pull.order, pull.commented = false, 0, false
		if !t.effect("eject unlabel", a.PR, "") {
			return "", true
		}
	}
	return "", false
}

func (t *checkTick) land(a queue.Action) (string, bool) {
	reread, _, ok := t.read()
	if !ok {
		return "", true
	}
	if err := t.c.verify(reread, a); err != nil {
		return "", false
	}
	if !t.c.holds(a.SHA, t.state.trunk) {
		if t.refused != a.SHA {
			t.refused = a.SHA
			return "", false
		}
		return "", true
	}
	broken, ok := checkLand(a, t.c.checkedBuild(t.state), func(n int) checkedPR {
		return t.c.checkedPull(t.state, n)
	})
	if len(broken) > 0 {
		return strings.Join(broken, "; "), false
	}
	if !ok {
		return "", false
	}
	t.state.trunk = a.SHA
	if idx := t.state.findBuild(a.SHA); idx >= 0 && t.c.world.Config.Keep <= 0 {
		if t.state.builds[idx].marked {
			t.state.markSeen(a.SHA)
		}
		t.state.builds = slices.Delete(t.state.builds, idx, idx+1)
	}
	for _, m := range a.Members {
		if m.Number > 0 && m.Number <= len(t.state.pulls) {
			pull := &t.state.pulls[m.Number-1]
			if pull.head(m.Number, pull.gitHead) == m.Head {
				pull.merged = true
			}
		}
	}
	if !t.effect("git land", 0, a.SHA) {
		return "", true
	}
	t.state.markSeen(a.SHA)
	if idx := t.state.findBuild(a.SHA); idx >= 0 && t.c.world.Config.Keep > 0 {
		t.state.builds[idx].landed = t.state.nextEvent()
	}
	if !t.effect("land status", 0, a.SHA) {
		return "", true
	}
	for _, m := range a.Members {
		if m.Number > 0 && m.Number <= len(t.state.pulls) {
			pull := &t.state.pulls[m.Number-1]
			if pull.head(m.Number, pull.githubHead) == m.Head {
				pull.statusHead, pull.status = pull.githubHead, queue.Success
				pull.statusText = fmt.Sprintf("landed as %.12s", a.SHA)
				if !t.effect("land pr status", m.Number, "") {
					return "", true
				}
			}
		}
	}
	return "", false
}

type drainTickTrace struct {
	tick  int
	reads []tickRead
}

const maxDrainTicks = 40

// drain runs the world forward with no new hazards until nothing moves, checking invariant 4,
// and attributes a stall after a crashed tick to invariant 6.
func (c *checker) drain(s *worldState, trace bool) (string, []drainTickTrace) {
	d := s.clone()
	d.trunkRed, d.crashed = false, false
	d.flakesLeft, d.lostLeft, d.timeoutsLeft, d.episodesLeft, d.movesLeft, d.outsideLeft, d.crashesLeft, d.staleLeft = 0, 0, 0, 0, 0, 0, 0, 0
	for i := range d.pulls {
		d.pulls[i].githubHead = d.pulls[i].gitHead
	}

	var keys []string
	var traces []drainTickTrace
	finish := func(raw string) (string, []drainTickTrace) {
		if !trace {
			for _, k := range keys {
				c.settled[k] = raw
			}
		}
		if raw != "" && s.crashed {
			raw = strings.Replace(raw, "invariant 4:", "invariant 6:", 1)
		}
		return raw, traces
	}

	for n := 0; n < maxDrainTicks; n++ {
		dk := d.key()
		if !trace {
			if cached, ok := c.settled[dk]; ok {
				return finish(cached)
			}
		}
		if slices.Contains(keys, dk) {
			break
		}
		keys = append(keys, dk)

		out := (&checkTick{c: c, state: d, trace: trace}).run()
		if trace && len(out.reads) > 0 {
			traces = append(traces, drainTickTrace{tick: n + 1, reads: out.reads})
		}
		if out.broken != "" {
			return finish(out.broken)
		}
		d = out.final
		if !c.drainCI(d) {
			if msg := checkDrain(c.drainPulls(d), fmt.Sprintf("%d drain ticks", maxDrainTicks)); msg != "" {
				return finish(msg)
			}
			if c.world.Config.Keep > 0 {
				for _, b := range d.builds {
					if b.branch && b.landed != 0 && c.holds(d.trunk, b.sha) {
						return finish(fmt.Sprintf("keep expiry: landed build %.12s still stands after drain", b.sha))
					}
				}
			}
			return finish("")
		}
	}
	if msg := checkDrain(c.drainPulls(d), fmt.Sprintf("%d drain ticks", maxDrainTicks)); msg != "" {
		return finish(msg)
	}
	return finish(brokeMsg(4, "the queue did not settle after %d drain ticks", maxDrainTicks))
}

func (c *checker) drainCI(d *worldState) bool {
	changed := false
	expire := d.kept
	if c.world.Config.Keep > 0 {
		for _, b := range d.builds {
			if b.branch && b.landed != 0 && c.holds(d.trunk, b.sha) {
				expire = max(expire, b.landed)
			}
		}
	}
	var idxs []int
	for i := range d.builds {
		if b := d.builds[i]; b.branch && b.verdict == queue.Pending && !b.lost {
			idxs = append(idxs, i)
		}
	}
	sort.Slice(idxs, func(a, b int) bool {
		ba, bb := d.builds[idxs[a]], d.builds[idxs[b]]
		if ba.begun != bb.begun {
			return ba.begun
		}
		if ba.started != bb.started {
			return ba.started < bb.started
		}
		return ba.pushed < bb.pushed
	})
	for _, idx := range idxs {
		b := &d.builds[idx]
		d.begin(b)
		if b.redTrunk || c.guilty(b.sha) {
			b.verdict = queue.Failure
		} else {
			b.verdict = queue.Success
		}
		changed = true
	}
	if changed && c.world.Config.Keep > 0 {
		return true
	}
	for _, b := range d.builds {
		if !b.branch {
			continue
		}
		if b.lost && c.world.Config.Timeout > 0 && b.pushed > d.timedOut {
			d.timedOut = b.pushed
			changed = true
		}
		if c.world.Config.Recheck > 0 && b.pushed > d.rechecked {
			d.rechecked = b.pushed
			changed = true
		}
	}
	if expire > d.kept {
		d.kept = expire
		changed = true
	}
	if c.world.Config.Keep > 0 {
		_, _, _, _, pushes, marks := d.clockRanks()
		for _, mark := range marks {
			if x, _, _ := c.buildTimes(d, pushes); x != nil {
				break
			}
			if mark > d.kept {
				d.kept = mark
				changed = true
			}
		}
	}
	return changed
}

func (c *checker) drainPulls(s *worldState) []checkedPR {
	list := c.checkedPulls(s)
	repo := worldRepo{c: c, s: s}
	for i := range list {
		if list[i].queued && list[i].wait == "" && list[i].checks == queue.Success {
			if _, ok := repo.Merge(s.trunk, queue.PR{Number: list[i].number, Head: list[i].head}, ""); !ok {
				list[i].wait = "it conflicts with the trunk"
			}
		}
	}
	return list
}

func (c *checker) envMoves(s *worldState) []transition {
	var out []transition
	out = append(out, c.ciMoves(s)...)
	out = append(out, c.clockMoves(s)...)
	if !s.trunkRed && s.episodesLeft > 0 {
		ns := s.clone()
		ns.trunkRed = true
		ns.episodesLeft--
		ns.episodes++
		out = append(out, transition{step{kind: stepTrunkRed}, ns})
	}
	if s.trunkRed {
		ns := s.clone()
		ns.trunkRed = false
		out = append(out, transition{step{kind: stepTrunkGreen}, ns})
	}
	out = append(out, c.pullMoves(s)...)
	if s.outsideLeft > 0 {
		ns := s.clone()
		ns.outsideGen++
		ns.trunk = c.addCommit(ns.trunk, "", 0, fmt.Sprintf("outside %d", ns.outsideGen))
		ns.outsideLeft--
		out = append(out, transition{step{kind: stepOutside}, ns})
	}
	return out
}

func (c *checker) ciMoves(s *worldState) []transition {
	var out []transition
	if !c.world.Silent {
		for i, b := range s.builds {
			if !b.branch || b.verdict != queue.Pending || b.begun || b.lost {
				continue
			}
			ns := s.clone()
			ns.begin(&ns.builds[i])
			out = append(out, transition{step{kind: stepBegin, sha: b.sha}, ns})
		}
	}
	for i, b := range s.builds {
		if !b.branch || b.verdict != queue.Pending || b.lost {
			continue
		}
		truthFail := b.redTrunk || (!b.begun && s.trunkRed) || c.guilty(b.sha)
		addVerdict := func(v queue.Verdict, flaked bool, kind stepKind) {
			ns := s.clone()
			nb := &ns.builds[i]
			ns.begin(nb)
			nb.verdict = v
			if flaked {
				nb.flaked, ns.flaked = true, true
				ns.flakesLeft--
			}
			if v == queue.Error {
				ns.lostLeft--
			}
			out = append(out, transition{step{kind: kind, sha: b.sha}, ns})
		}
		if truthFail {
			addVerdict(queue.Failure, false, stepFail)
		} else {
			addVerdict(queue.Success, false, stepPass)
			if s.flakesLeft > 0 {
				addVerdict(queue.Failure, true, stepFlake)
			}
		}
		if s.lostLeft > 0 {
			addVerdict(queue.Error, false, stepError)
			nsLost := s.clone()
			nsLost.builds[i].lost = true
			nsLost.lostLeft--
			out = append(out, transition{step{kind: stepLost, sha: b.sha}, nsLost})
		}
	}
	return out
}

func (c *checker) clockMoves(s *worldState) []transition {
	if c.world.Config.Keep > 0 {
		return c.keepClockMoves(s)
	}
	return c.baseClockMoves(s)
}

// keepClockMoves cuts keep independently of recheck. Some cuts of one clock force cuts
// of the other; the same difference constraints used by read discard impossible pairs.
func (c *checker) keepClockMoves(s *worldState) []transition {
	_, _, _, _, pushes, marks := s.clockRanks()
	rechecks, keeps := []uint16{s.rechecked}, []uint16{s.kept}
	if c.world.Config.Recheck > 0 {
		for _, p := range pushes {
			if p > s.rechecked {
				rechecks = append(rechecks, p)
			}
		}
	}
	for _, a := range marks {
		if a > s.kept {
			keeps = append(keeps, a)
		}
	}
	seeds := append([]transition{{step: step{kind: stepKeep}, state: s}}, c.baseClockMoves(s)...)
	var out []transition
	seen := map[string]bool{s.key(): true}
	for _, seed := range seeds {
		for _, rc := range rechecks {
			if rc < seed.state.rechecked {
				continue
			}
			for _, kc := range keeps {
				ns := seed.state.clone()
				ns.rechecked, ns.kept = rc, kc
				if x, _, _ := c.buildTimes(ns, pushes); x == nil {
					continue
				}
				key := ns.key()
				if seen[key] {
					continue
				}
				seen[key] = true
				st := seed.step
				if kc == s.kept && st.kind == stepKeep {
					st.kind = stepRecheck
				}
				out = append(out, transition{st, ns})
			}
		}
	}
	return out
}

func (c *checker) baseClockMoves(s *worldState) []transition {
	var out []transition
	oldest := func(keep func(pushedBuild) bool) int {
		best := -1
		for i, b := range s.builds {
			if b.branch && keep(b) && (best < 0 || b.pushed < s.builds[best].pushed) {
				best = i
			}
		}
		return best
	}
	if c.world.Config.Recheck > 0 {
		if i := oldest(func(b pushedBuild) bool { return b.pushed > s.rechecked }); i >= 0 {
			ns := s.clone()
			ns.rechecked = s.builds[i].pushed
			out = append(out, transition{step{kind: stepRecheck}, ns})
		}
	}
	if c.world.Config.Timeout > 0 {
		if i := oldest(func(b pushedBuild) bool { return b.verdict == queue.Pending && b.pushed > s.timedOut }); i >= 0 {
			b := s.builds[i]
			if b.lost || s.timeoutsLeft > 0 {
				ns := s.clone()
				ns.timedOut = b.pushed
				if c.world.Config.Recheck > 0 && c.world.Config.Recheck <= c.world.Config.Timeout {
					ns.rechecked = max(ns.rechecked, b.pushed)
				}
				if !b.lost {
					ns.timeoutsLeft--
				}
				out = append(out, transition{step{kind: stepTimeout}, ns})
			}
		}
	}
	return out
}

func (c *checker) pullMoves(s *worldState) []transition {
	var out []transition
	for i, p := range s.pulls {
		if p.githubHead != p.gitHead {
			ns := s.clone()
			ns.pulls[i].githubHead = ns.pulls[i].gitHead
			out = append(out, transition{step{kind: stepSync, pull: i + 1}, ns})
		}
	}
	if s.movesLeft == 0 {
		return out
	}
	for i, p := range s.pulls {
		num := i + 1
		if c.holds(s.trunk, p.head(num, p.gitHead)) || p.merged {
			continue
		}
		move := func(st step, edit func(*worldState, *pullRequest)) {
			ns := s.clone()
			edit(ns, &ns.pulls[i])
			ns.movesLeft--
			out = append(out, transition{st, ns})
		}
		if !p.queued {
			move(step{kind: stepRelabel, pull: num}, func(ns *worldState, np *pullRequest) {
				np.queued, np.order, np.commented = true, ns.nextOrder, false
				ns.nextOrder++
			})
			continue
		}
		move(step{kind: stepMove, pull: num}, func(ns *worldState, np *pullRequest) {
			np.gitHead++
			np.githubHead, np.commented, np.checks, np.conflict = np.gitHead, false, queue.Pending, false
			if c.commits[ns.trunk].holds&pullBit(int(np.ahead)) != 0 {
				np.ahead = 0
			}
		})
		if p.gitHead == p.githubHead {
			move(step{kind: stepArriving, pull: num}, func(_ *worldState, np *pullRequest) {
				np.gitHead++
				np.commented = false
			})
		}
		move(step{kind: stepUnlabel, pull: num}, func(_ *worldState, np *pullRequest) {
			np.queued, np.order, np.commented = false, 0, false
		})
		for _, v := range []queue.Verdict{queue.Success, queue.Pending, queue.Failure} {
			if v != p.checks {
				move(step{kind: stepChecks, pull: num, verdict: v}, func(_ *worldState, np *pullRequest) {
					np.checks = v
				})
			}
		}
		move(step{kind: stepHold, pull: num, on: !p.hold}, func(_ *worldState, np *pullRequest) {
			np.hold = !p.hold
		})
		move(step{kind: stepFirst, pull: num, on: !p.first}, func(_ *worldState, np *pullRequest) {
			np.first = !p.first
		})
	}
	return out
}

type reached struct {
	state       *worldState
	from, depth int
	step        step
}

func explore(w checkWorld, m mutant) checkResult {
	c := newChecker(w, m)
	init := c.initState()
	maxStates := w.Bounds.MaxStates
	if maxStates <= 0 {
		maxStates = 100000
	}

	visited := map[string]bool{init.key(): true}
	nodes := []reached{{state: init, from: -1, depth: 0}}
	maxDepth := 0
	var stopped *checkResult

	fail := func(idx int, last step, broken string) checkResult {
		var steps []step
		for i := idx; i >= 0; i = nodes[i].from {
			if nodes[i].step.kind != stepNone {
				steps = append(steps, nodes[i].step)
			}
		}
		slices.Reverse(steps)
		if last.kind != stepNone {
			steps = append(steps, last)
		}
		return checkResult{States: len(nodes), Depth: nodes[idx].depth + 1, Broken: broken, Trace: steps}
	}

	if c.world.Config.Keep > 0 {
		w, _ := c.read(init)
		if msg := checkKeep(w, c.world.Config, c.plan(w, c.world.Config)); msg != "" {
			return fail(0, step{kind: stepTick}, msg)
		}
	}
	if msg, _ := c.drain(init, false); msg != "" {
		return fail(0, step{kind: stepDrain}, msg)
	}

	visit := func(from int, st step, ns *worldState) bool {
		k := ns.key()
		if visited[k] {
			return true
		}
		if len(visited) >= maxStates {
			stopped = &checkResult{States: len(visited), Depth: maxDepth, Capped: true}
			return false
		}
		visited[k] = true
		d := nodes[from].depth + 1
		maxDepth = max(maxDepth, d)
		nodes = append(nodes, reached{state: ns, from: from, step: st, depth: d})
		if c.world.Config.Keep > 0 {
			w, _ := c.read(ns)
			if msg := checkKeep(w, c.world.Config, c.plan(w, c.world.Config)); msg != "" {
				res := fail(len(nodes)-1, step{kind: stepTick}, msg)
				stopped = &res
				return false
			}
		}
		if msg, _ := c.drain(ns, false); msg != "" {
			res := fail(len(nodes)-1, step{kind: stepDrain}, msg)
			stopped = &res
			return false
		}
		return true
	}

	for head := 0; head < len(nodes); head++ {
		cur := nodes[head]

		out := (&checkTick{c: c, state: cur.state, crash: cur.state.crashesLeft > 0}).run()
		if out.broken != "" {
			return fail(head, step{kind: stepTick}, out.broken)
		}
		if out.changed && !visit(head, step{kind: stepTick}, out.final) {
			return *stopped
		}
		for _, br := range out.crashes {
			if !visit(head, br.step, br.state) {
				return *stopped
			}
		}

		var staleW *queue.World
		var stalePulls map[int]pullRequest
		if cur.state.staleLeft > 0 {
			rw, rp := c.read(cur.state)
			staleW, stalePulls = &rw, rp
		}
		for _, mv := range c.envMoves(cur.state) {
			if !visit(head, mv.step, mv.state) {
				return *stopped
			}
			if cur.state.staleLeft > 0 {
				after := mv.state.clone()
				after.staleLeft--
				outStale := (&checkTick{c: c, state: after, stale: staleW, stalePulls: stalePulls}).run()
				staleStep := mv.step
				staleStep.stale = true
				if outStale.broken != "" {
					return fail(head, staleStep, outStale.broken)
				}
				if outStale.changed && !visit(head, staleStep, outStale.final) {
					return *stopped
				}
			}
		}
	}

	return checkResult{States: len(visited), Depth: maxDepth}
}

var (
	good     = pullSpec{}
	breaks   = pullSpec{Breaks: true}
	conflict = pullSpec{Conflict: true}
	hold     = pullSpec{Hold: true}
)

func clash(n int) pullSpec            { return pullSpec{Clash: n} }
func needs(n int) pullSpec            { return pullSpec{Needs: n} }
func ahead(n int) pullSpec            { return pullSpec{Ahead: n} }
func pulls(ps ...pullSpec) []pullSpec { return ps }

func cancels(slots, batch int) queue.Config {
	c := dials(slots, batch, false)
	c.Cancels = true
	return c
}

func defaultWorlds() []checkWorld {
	cfg1, cfg2, cfg2c := dials(2, 1, false), dials(2, 2, false), cancels(2, 2)
	cfg32, cfg3p := dials(3, 2, false), dials(2, 3, true)
	gathers := dials(2, 2, false)
	gathers.Gather = 2

	bCI := bounds{Flakes: 1, Lost: 1, Timeouts: 1, Episodes: 1, MaxStates: 6000}
	bCrash := bounds{Flakes: 1, Crashes: 1, Stale: 1, MaxStates: 5000}
	bMove := bounds{Moves: 1, Outside: 1, Stale: 1, MaxStates: 5000}
	bSolo := bounds{Flakes: 1, Lost: 1, Timeouts: 1, Episodes: 1, Moves: 1, Outside: 1, Crashes: 1, Stale: 1, MaxStates: 6000}
	b4 := bounds{Flakes: 1, Lost: 1, Timeouts: 1, Episodes: 1, MaxStates: 4000}

	worlds := []checkWorld{
		{"1 PR all hazards (good)", cfg1, pulls(good), false, false, bSolo},
		{"1 PR all hazards (breaks)", cfg1, pulls(breaks), false, false, bSolo},
		{"1 PR conflict", cfg1, pulls(conflict), false, false, bMove},
		{"1 PR silent CI (good)", cfg1, pulls(good), true, false, bCI},
		{"1 PR cancels (good)", cfg2c, pulls(good), false, false, bCrash},
	}

	pairs := []struct {
		name string
		prs  []pullSpec
	}{
		{"good+good", pulls(good, good)},
		{"good+breaks", pulls(good, breaks)},
		{"breaks+good", pulls(breaks, good)},
		{"clash", pulls(good, clash(1))},
		{"ahead", pulls(good, ahead(1))},
	}
	for _, p := range pairs {
		worlds = append(worlds,
			checkWorld{"2 PRs CI (" + p.name + ", 2 of 2)", cfg2, p.prs, false, false, bCI},
			checkWorld{"2 PRs crashes+stale (" + p.name + ", 2 of 2)", cfg2, p.prs, false, false, bCrash},
			checkWorld{"2 PRs moves+outside (" + p.name + ", 2 of 2)", cfg2, p.prs, false, false, bMove},
			checkWorld{"2 PRs CI (" + p.name + ", 2 of 1)", cfg1, p.prs, false, false, bCI},
			checkWorld{"2 PRs probes CI (" + p.name + ")", cfg3p, p.prs, false, false, bCI},
			checkWorld{"2 PRs gather CI (" + p.name + ")", gathers, p.prs, false, false, bCI},
			checkWorld{"2 PRs gather moves+outside (" + p.name + ")", gathers, p.prs, false, false, bMove},
		)
	}
	worlds = append(worlds,
		checkWorld{"2 PRs 3 slots (good+breaks)", cfg32, pulls(good, breaks), false, false, bCI},
		checkWorld{"2 PRs silent CI (good+breaks)", cfg2, pulls(good, breaks), true, false, bCI},
		checkWorld{"2 PRs cancels (good+breaks)", cfg2c, pulls(good, breaks), false, false, bCrash},
	)

	worlds = append(worlds, keepWorlds()...)

	specs := []func(int) pullSpec{
		func(int) pullSpec { return good },
		func(int) pullSpec { return breaks },
		clash,
		ahead,
	}
	for _, cfg := range []queue.Config{cfg1, dials(2, 3, false), cfg3p} {
		for a := range 2 {
			for b := range 4 {
				for c := range 4 {
					worlds = append(worlds, checkWorld{
						fmt.Sprintf("3 PRs %d%d%d (%d of %d, probes %v)", a, b, c, cfg.Slots, cfg.MaxBatch, cfg.Probes),
						cfg, pulls(specs[a](0), specs[b](1), specs[c](2)), false, false, bounds{Flakes: 1, Episodes: 1, MaxStates: 2500},
					})
				}
			}
		}
	}

	return append(append(worlds,
		checkWorld{"3 PRs crashes+lost (good+breaks+good)", dials(2, 3, false), pulls(good, breaks, good), false, false, bCrash},
		checkWorld{"3 PRs moves+stale (good+breaks+ahead)", dials(2, 3, false), pulls(good, breaks, ahead(2)), false, false, bMove},
		checkWorld{"4 PRs batch (good*4)", dials(2, 4, false), pulls(good, good, good, good), false, false, b4},
		checkWorld{"4 PRs batch (good*3+breaks)", dials(2, 4, false), pulls(good, good, good, breaks), false, false, b4},
		checkWorld{"4 PRs batch (clash+ahead+breaks)", dials(2, 4, false), pulls(good, clash(1), ahead(2), breaks), false, false, b4},
		checkWorld{"4 PRs 3x2 (breaks+good*2+clash)", cfg32, pulls(breaks, good, good, clash(2)), false, false, b4},
		checkWorld{"4 PRs probes (good+clash+good+breaks)", dials(2, 4, true), pulls(good, clash(1), good, breaks), false, false, b4},
	), needsWorlds()...)
}

func needsWorlds() []checkWorld {
	cfg := dials(2, 2, true)
	return []checkWorld{
		{Name: "3 PRs #2 and #3 need each other (2 of 2, probes)", Config: cfg, Pulls: pulls(good, needs(3), needs(2)), Instant: true, Bounds: bounds{MaxStates: 5000}},
		{Name: "3 PRs #3 needs #2 (2 of 2, probes)", Config: cfg, Pulls: pulls(good, good, needs(2)), Instant: true, Bounds: bounds{MaxStates: 5000}},
		{Name: "3 PRs #3 needs #2 and is ready later (2 of 2, probes)", Config: cfg, Pulls: pulls(good, good, late(needs(2))), Bounds: bounds{Moves: 1, MaxStates: 5000}},
	}
}

func needsASuspectWorld() checkWorld {
	return checkWorld{
		Name:   "3 PRs #1 breaks and #3 needs #2 (2 of 2, probes)",
		Config: dials(2, 2, true), Pulls: pulls(breaks, good, needs(2)), Bounds: bounds{MaxStates: 5000},
	}
}

func late(p pullSpec) pullSpec {
	p.Unready = true
	return p
}

func smallKeepWorlds() []checkWorld {
	cfg := queue.Config{Slots: 1, MaxBatch: 1, Keep: 3 * time.Minute}
	cancels, stacked := cfg, cfg
	cancels.Cancels = true
	stacked.Slots = 2
	return []checkWorld{
		{Name: "Keep whole one PR", Config: cfg, Pulls: pulls(good), Bounds: bounds{MaxStates: 10000}},
		{Name: "Keep whole one PR cancels", Config: cancels, Pulls: pulls(good), Bounds: bounds{MaxStates: 10000}},
		{Name: "Keep whole two stacked", Config: stacked, Pulls: pulls(good, good), Bounds: bounds{MaxStates: 10000}},
		{Name: "Keep whole flaky retry", Config: cfg, Pulls: pulls(good), Bounds: bounds{Flakes: 1, MaxStates: 10000}},
		{Name: "Keep whole culprit relabelled", Config: cfg, Pulls: pulls(breaks), Bounds: bounds{Moves: 1, MaxStates: 10000}},
	}
}

func addedKeepWorlds() []checkWorld {
	cfg := queue.Config{Slots: 1, MaxBatch: 1, Keep: 3 * time.Minute}
	stacked := cfg
	stacked.Slots = 2
	long := cfg
	long.Timeout = time.Minute
	longStacked := long
	longStacked.Slots = 2
	lost := long
	lost.Recheck = 3 * time.Minute
	probes := stacked
	probes.Probes = true
	return []checkWorld{
		{Name: "Keep whole four PRs two slots no cancelling", Config: stacked, Pulls: pulls(good, good, good, good), Bounds: bounds{MaxStates: 100000}},
		{Name: "Keep whole lower flakes after landing", Config: stacked, Pulls: pulls(good, good), Bounds: bounds{Flakes: 1, MaxStates: 100000}},
		{Name: "Keep whole longer than timeout", Config: long, Pulls: pulls(good, good, good), Bounds: bounds{Timeouts: 1, MaxStates: 100000}},
		{Name: "Keep whole stacked longer than timeout", Config: longStacked, Pulls: pulls(good, good), Bounds: bounds{Timeouts: 1, MaxStates: 100000}},
		{Name: "Keep whole error then recheck", Config: lost, Pulls: pulls(good), Bounds: bounds{Lost: 1, MaxStates: 100000}},
		{Name: "Keep whole stacked probes", Config: probes, Pulls: pulls(good, breaks), Bounds: bounds{MaxStates: 100000}},
		{Name: "Keep whole crash before mark", Config: cfg, Pulls: pulls(good), Bounds: bounds{Crashes: 1, MaxStates: 100000}},
	}
}

func keepWorlds() []checkWorld {
	worlds := append(smallKeepWorlds(), addedKeepWorlds()...)
	cases := []struct {
		name         string
		slots, batch int
		probes       bool
		prs          []pullSpec
	}{
		{"serial good+good", 1, 1, false, pulls(good, good)},
		{"stacked good+good", 3, 1, false, pulls(good, good)},
		{"blame whole", 2, 2, false, pulls(good, breaks)},
		{"blame probes", 2, 2, true, pulls(good, breaks)},
	}
	for _, tc := range cases {
		cfg := dials(tc.slots, tc.batch, tc.probes)
		cfg.Keep = 3 * time.Minute
		for _, crash := range []bool{false, true} {
			b := bounds{Flakes: 1, Lost: 1, Timeouts: 1, Episodes: 1, MaxStates: 6000}
			if crash {
				b = bounds{Flakes: 1, Crashes: 1, Stale: 1, MaxStates: 5000}
			}
			worlds = append(worlds, checkWorld{Name: fmt.Sprintf("Keep %s crashes=%v", tc.name, crash), Config: cfg, Pulls: tc.prs, Bounds: b})
		}
	}
	for _, tc := range []struct {
		name   string
		probes bool
		prs    []pullSpec
		bounds bounds
	}{
		{"ejection then same head relabelled", true, pulls(breaks), bounds{Moves: 1, MaxStates: 6000}},
		{"cleared round then fresh candidate passes", false, pulls(good), bounds{Flakes: 1, MaxStates: 6000}},
		{"round ends while probe runs", true, pulls(good, breaks), bounds{Flakes: 1, MaxStates: 6000}},
	} {
		cfg := dials(2, 2, tc.probes)
		cfg.Keep = 3 * time.Minute
		worlds = append(worlds, checkWorld{Name: "Keep " + tc.name, Config: cfg, Pulls: tc.prs, Bounds: tc.bounds})
	}
	for _, rc := range []time.Duration{0, time.Minute, 10 * time.Minute} {
		cfg := dials(1, 1, false)
		cfg.Keep, cfg.Recheck = 3*time.Minute, rc
		worlds = append(worlds, checkWorld{Name: fmt.Sprintf("Keep removal+relabel recheck=%s", rc), Config: cfg, Pulls: pulls(good, good), Bounds: bounds{Moves: 2, Crashes: 1, Stale: 1, MaxStates: 6000}})
	}
	return worlds
}

func deepWorlds() []checkWorld {
	bDeep := bounds{Flakes: 2, Lost: 2, Timeouts: 2, Episodes: 1, Moves: 1, Outside: 1, Crashes: 2, Stale: 2, MaxStates: 25000}
	b4 := bounds{Flakes: 2, Lost: 1, Timeouts: 1, Episodes: 1, Crashes: 1, Stale: 1, MaxStates: 20000}
	return append(defaultWorlds(),
		checkWorld{"2 PRs deep lost=2 (good+good)", dials(2, 2, false), pulls(good, good), false, false, bDeep},
		checkWorld{"2 PRs deep lost=2 (good+breaks)", dials(2, 2, false), pulls(good, breaks), false, false, bDeep},
		checkWorld{"2 PRs deep probes (good+breaks)", dials(2, 2, true), pulls(good, breaks), false, false, bDeep},
		checkWorld{"3 PRs deep (good+breaks+good)", dials(2, 3, false), pulls(good, breaks, good), false, false, bDeep},
		checkWorld{"4 PRs deep (good*3+breaks)", dials(2, 4, false), pulls(good, good, good, breaks), false, false, b4},
		checkWorld{"4 PRs deep probes (clash+breaks)", dials(2, 4, true), pulls(good, clash(1), good, breaks), false, false, b4},
	)
}

type checkSample struct {
	world  checkWorld
	result checkResult
}

type checkSummary struct {
	worlds, whole, capped, states, brokenWorlds int
	brokenInvs                                  []string
	bestByInv                                   map[string]checkSample
}

func checkSuite(worlds []checkWorld, m mutant) checkSummary {
	results := each(len(worlds), func(i int) checkResult { return explore(worlds[i-1], m) })
	sum := checkSummary{worlds: len(worlds), bestByInv: map[string]checkSample{}}
	better := func(a, b checkResult) bool {
		return b.Broken == "" || len(a.Trace) < len(b.Trace) || (len(a.Trace) == len(b.Trace) && a.States < b.States)
	}
	for i, res := range results {
		sum.states += res.States
		if res.Capped {
			sum.capped++
		} else {
			sum.whole++
		}
		if res.Broken == "" {
			continue
		}
		sum.brokenWorlds++
		for _, part := range strings.Split(res.Broken, "; ") {
			inv, _, _ := strings.Cut(part, ":")
			if prev, ok := sum.bestByInv[inv]; !ok || better(res, prev.result) {
				if !ok {
					sum.brokenInvs = append(sum.brokenInvs, inv)
				}
				sum.bestByInv[inv] = checkSample{world: worlds[i], result: res}
			}
		}
	}
	slices.Sort(sum.brokenInvs)
	return sum
}

func firstBroken(worlds []checkWorld, m mutant) (states int, worldName string, res checkResult) {
	for _, w := range worlds {
		r := explore(w, m)
		states += r.States
		if r.Broken != "" {
			return states, w.Name, r
		}
	}
	return states, "", checkResult{}
}

func reportCheck(worlds []checkWorld, m mutant) checkSummary {
	sum := checkSuite(worlds, m)
	fmt.Printf("checker: %d of %d worlds explored whole and %d stopped at the cap, %d states, %d broken\n",
		sum.whole, sum.worlds, sum.capped, sum.states, sum.brokenWorlds)
	for _, inv := range sum.brokenInvs {
		sample := sum.bestByInv[inv]
		fmt.Printf("  shortest %s in %s:\n%s", inv, sample.world.Name, formatTrace(sample.result))
	}
	return sum
}

type checkFlag struct {
	on   bool
	name string
}

func (f *checkFlag) String() string   { return f.name }
func (f *checkFlag) IsBoolFlag() bool { return true }
func (f *checkFlag) Set(s string) error {
	switch s {
	case "false":
		f.on, f.name = false, ""
	case "true":
		f.on = true
	default:
		f.on, f.name = true, s
	}
	return nil
}

func (f checkFlag) run(args []string) {
	name := f.name
	if name == "" && len(args) > 0 {
		name = args[0]
	}
	if (f.name != "" && len(args) > 0) || len(args) > 1 {
		fmt.Fprintln(os.Stderr, `usage: go run ./eval -check ["<world name>"]`)
		os.Exit(2)
	}
	if !runCheck(name) {
		os.Exit(1)
	}
}

func runCheck(name string) bool {
	real := mutant{plan: queue.Plan, verify: queue.Verify}
	if name != "" {
		w, ok := findWorld(name)
		if !ok {
			fmt.Printf("unknown world %q\n", name)
			return false
		}
		res := explore(w, real)
		if res.Broken == "" {
			status := "explored whole"
			if res.Capped {
				status = "stopped at the cap"
			}
			fmt.Printf("%s: %d states, %s, 0 broken\n", w.Name, res.States, status)
			return true
		}
		fmt.Print(formatCounterexample(w, real, res))
		return false
	}

	sum := reportCheck(deepWorlds(), real)
	ok := sum.brokenWorlds == 0
	for _, suite := range []struct {
		worlds  []checkWorld
		mutants []checkMutant
	}{
		{mutantWorlds(), checkerMutants()},
		{keepMutantWorlds(), keepMutants()},
	} {
		for _, cm := range suite.mutants {
			_, worldName, res := firstBroken(suite.worlds, cm.mutant)
			if res.Broken == "" {
				fmt.Printf("  mutant %-46s: NOT CAUGHT\n", cm.name)
				ok = false
				continue
			}
			fmt.Printf("  mutant %-46s: %s (%s, %d states)\n", cm.name, res.Broken, worldName, res.States)
		}
	}
	return ok
}

func findWorld(name string) (checkWorld, bool) {
	for _, list := range [][]checkWorld{deepWorlds(), mutantWorlds(), keepMutantWorlds(), {needsASuspectWorld()}} {
		for _, w := range list {
			if w.Name == name {
				return w, true
			}
		}
	}
	return checkWorld{}, false
}

func formatTrace(res checkResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "    %s (%d states, depth %d):\n", res.Broken, res.States, res.Depth)
	for i, st := range res.Trace {
		fmt.Fprintf(&b, "      %2d. %s\n", i+1, st)
	}
	return b.String()
}

func formatCounterexample(w checkWorld, m mutant, res checkResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (%d states, %d steps)\n", w.Name, res.Broken, res.States, len(res.Trace))
	newChecker(w, m).replay(res.Trace,
		func(i int, st step) { fmt.Fprintf(&b, "  %2d. %s\n", i+1, st) },
		func(at string, r tickRead) {
			fmt.Fprintf(&b, "    %s:\n", at)
			writeRead(&b, "      ", w.Config, r)
		})
	return b.String()
}

// replay plays a trace from the initial state, calling stepped before each step and read for each
// pass of a tick the step runs, with where in the step the pass falls.
func (c *checker) replay(trace []step, stepped func(int, step), read func(string, tickRead)) {
	cur := c.initState()
	play := func(t *checkTick) {
		out := t.run()
		for p, r := range out.reads {
			read(fmt.Sprintf("pass %d", p), r)
		}
		if out.final != nil {
			cur = out.final
			cur.sort()
		}
	}
	for i, st := range trace {
		stepped(i, st)
		switch {
		case st.kind == stepTick:
			play(&checkTick{c: c, state: cur, trace: true})
		case st.kind == stepCrash:
			play(&checkTick{c: c, state: cur, trace: true, stopAt: st.at})
		case st.stale:
			staleW, stalePulls := c.read(cur)
			base := st
			base.stale = false
			after := moveState(c.envMoves(cur), base).clone()
			after.staleLeft--
			play(&checkTick{c: c, state: after, stale: &staleW, stalePulls: stalePulls, trace: true})
		case st.kind == stepDrain:
			_, ticks := c.drain(cur, true)
			for _, dt := range ticks {
				for p, r := range dt.reads {
					read(fmt.Sprintf("drain tick %d, pass %d", dt.tick, p), r)
				}
			}
		default:
			cur = moveState(c.envMoves(cur), st)
			cur.sort()
		}
	}
}

func moveState(moves []transition, target step) *worldState {
	for _, mv := range moves {
		if mv.step == target {
			return mv.state
		}
	}
	panic(fmt.Sprintf("missing transition %s", target))
}

var (
	kindNames    = map[queue.Kind]string{queue.Candidate: "candidate", queue.Check: "check", queue.Probe: "probe"}
	verdictNames = map[queue.Verdict]string{queue.Pending: "pending", queue.Success: "success", queue.Failure: "failure", queue.Error: "error"}
)

func writeRead(b *strings.Builder, indent string, cfg queue.Config, r tickRead) {
	w := r.world
	fmt.Fprintf(b, "%strunk: %.7s\n", indent, w.Trunk)
	if len(w.PRs) == 0 {
		fmt.Fprintf(b, "%spull requests: none\n", indent)
	} else {
		var parts []string
		for _, pr := range w.PRs {
			s := fmt.Sprintf("#%d %s %s", pr.Number, pr.Head, verdictNames[pr.Checks])
			if pr.First {
				s += " first"
			}
			if pr.Wait != "" {
				s += fmt.Sprintf(" wait=%q", pr.Wait)
			}
			if pr.Note != "" {
				s += fmt.Sprintf(" note=%q", pr.Note)
			}
			parts = append(parts, s)
		}
		fmt.Fprintf(b, "%spull requests: %s\n", indent, strings.Join(parts, ", "))
	}
	if len(w.Builds) == 0 {
		fmt.Fprintf(b, "%sbuilds: none\n", indent)
	} else {
		fmt.Fprintf(b, "%sbuilds:\n", indent)
		for _, build := range w.Builds {
			fmt.Fprintf(b, "%s  %s\n", indent, formatBuild(build, w.Now, cfg))
		}
	}
	if len(r.actions) == 0 {
		fmt.Fprintf(b, "%sactions: none\n", indent)
	} else {
		fmt.Fprintf(b, "%sactions:\n", indent)
		for _, a := range r.actions {
			fmt.Fprintf(b, "%s  %s\n", indent, formatAction(a))
		}
	}
}

func formatBuildCore(b queue.Build) string {
	of := "-"
	if b.Of != "" {
		of = fmt.Sprintf("%.7s", b.Of)
	}
	return fmt.Sprintf("%.7s %s base=%.7s chain=%s round=%d of=%s",
		b.SHA, kindNames[b.Kind], b.Base, formatChain(b.Chain), b.Round, of)
}

func formatBuild(build queue.Build, now time.Time, cfg queue.Config) string {
	begun := "false"
	if !build.Started.IsZero() {
		begun = fmt.Sprintf("-%s", now.Sub(build.Started))
	}
	s := fmt.Sprintf("%s verdict=%s pushed=-%s begun=%s",
		formatBuildCore(build), verdictNames[build.Verdict], now.Sub(build.Pushed), begun)
	switch age := now.Sub(build.Pushed); {
	case cfg.Timeout > 0 && age > cfg.Timeout:
		s += " aged=timeout"
	case cfg.Recheck > 0 && age >= cfg.Recheck:
		s += " aged=recheck"
	}
	return s
}

func formatAction(a queue.Action) string {
	switch a.Do {
	case "push":
		return "push " + formatBuildCore(a.Build)
	case "delete":
		return fmt.Sprintf("delete %.7s", a.SHA)
	case "land":
		return fmt.Sprintf("land %.7s members=%s", a.SHA, formatChain(a.Members))
	case "eject":
		evs := make([]string, len(a.Evidence))
		for i, sha := range a.Evidence {
			evs[i] = fmt.Sprintf("%.7s", sha)
		}
		return fmt.Sprintf("eject #%d cause=%s evidence=[%s] text=%q", a.PR, a.Cause, strings.Join(evs, " "), a.Text)
	case "note":
		return fmt.Sprintf("note #%d text=%q", a.PR, a.Text)
	default:
		return a.Do
	}
}

func formatChain(chain []queue.Member) string {
	parts := make([]string, len(chain))
	for i, m := range chain {
		parts[i] = fmt.Sprintf("#%d@%s:%.7s", m.Number, m.Head, m.Merge)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
