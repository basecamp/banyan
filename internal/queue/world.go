// Package queue plans a merge queue's next actions from the world it reads.
//
// Plan is a pure function: the world in, actions out. Nothing is stored between ticks, so
// the same world always gives the same actions, and a recorded world replays exactly.
package queue

import "time"

// SHA names a commit.
type SHA = string

// Verdict is what CI said of a commit.
type Verdict int

const (
	Pending Verdict = iota
	Success
	Failure
	// Error is a build that ended with no verdict on the code: cancelled, lost, timed out.
	Error
)

// Kind says what a build is for.
type Kind int

const (
	// Candidate is a whole build of a commit the trunk may fast-forward to.
	Candidate Kind = iota
	// Check is a whole build of a tree that never lands: evidence about the trunk's tip, or
	// about a prefix of a red candidate.
	Check
	// Probe reruns only what failed in a red candidate, on one prefix of it.
	Probe
)

// PR is a pull request labelled for the queue.
type PR struct {
	Number int
	Head   SHA
	// Branch is the head branch as GitHub's own merge commits name it, owner/branch.
	Branch string
	// Pinned is the head the queue's status was posted on, empty before the first tick.
	Pinned SHA
	Queued time.Time
	First  bool
	// Wait is why the pull request is not ready to join the chain, empty when it is: a hold
	// label, a draft, a base that is not the trunk, a review thread left open.
	Wait   string
	Checks Verdict
	// Note is the description of the queue's status on its head, as last written.
	Note string
}

// Member is one pull request merged into a chain: the head merged, and the merge commit that
// has no attempt number, which names this prefix of the chain whatever attempt built it.
type Member struct {
	Number int
	Head   SHA
	Merge  SHA
}

// Build is a commit the queue pushed for CI, read back from its branch and statuses.
type Build struct {
	SHA  SHA
	Kind Kind
	// Base is the trunk commit the chain stands on, and Chain the prefix this build tests.
	Base  SHA
	Chain []Member
	// Round is the blame round a build belongs to, 0 for a candidate stacked on the chain.
	Round int
	// Of names the red candidate a round's build is evidence about, by its prefix.
	Of      SHA
	Verdict Verdict
	Pushed  time.Time
	// Started is when CI began running the build, if it made the build wait for an agent:
	// zero while it waits, zero where CI began it as soon as it was pushed, and always zero
	// where the repository's CI does not say. The planner compares these times exactly, and
	// takes builds pushed together with no Started to have begun together.
	Started time.Time
}

// Tests is the prefix whose tree this build has.
func (b Build) Tests() SHA {
	if len(b.Chain) == 0 {
		return b.Base
	}
	return b.Chain[len(b.Chain)-1].Merge
}

// began is when CI began running the build, or its push where CI does not say.
func (b Build) began() time.Time {
	if b.Started.IsZero() {
		return b.Pushed
	}
	return b.Started
}

// above is the part of the build's chain above the trunk, and whether the trunk is on it.
func (b Build) above(trunk SHA) ([]Member, bool) {
	if b.Base == trunk {
		return b.Chain, true
	}
	for i, m := range b.Chain {
		if m.Merge == trunk {
			return b.Chain[i+1:], true
		}
	}
	return nil, false
}

// Repo is the git the planner builds commits with. Each answer depends on its arguments
// alone, so the same queue always gives the same SHAs.
type Repo interface {
	// Merge is the commit that merges the pull request's head onto parent, carrying note,
	// or false when they conflict.
	Merge(parent SHA, pr PR, note string) (SHA, bool)
	// Mark is an empty commit on parent that carries note.
	Mark(parent SHA, note string) SHA
	// Seen says whether the queue ever pushed this commit for CI.
	Seen(SHA) bool
}

// World is everything a tick reads.
type World struct {
	Now    time.Time
	Trunk  SHA
	PRs    []PR
	Builds []Build
	Repo   Repo `json:"-"`
}

// Config holds the dials.
type Config struct {
	// Slots is how many candidates the queue keeps under test at once.
	Slots int
	// MaxBatch is how many pull requests one candidate adds to the chain.
	MaxBatch int
	// Probes says the repository's CI answers a probe commit by running only what failed.
	Probes bool
	// Cancels says the repository's CI stops a build whose branch is deleted. Where it does
	// not, a build the queue no longer wants holds its slot until it reports.
	Cancels bool
	// Timeout is how long a build may go without a verdict before it is built again.
	Timeout time.Duration
	// Recheck is the wait before a red trunk, or a CI that errored, is tried again.
	Recheck time.Duration
}

// Action is one thing a tick does. The executor applies them in order.
type Action struct {
	// Do is pin, push, land, delete, eject or note.
	Do    string `json:"do"`
	Build Build  `json:"build,omitzero"`  // push
	SHA   SHA    `json:"sha,omitempty"`   // land, delete
	PR    int    `json:"pr,omitempty"`    // pin, eject, note
	Cause string `json:"cause,omitempty"` // eject: culprit, conflict or moved
	Text  string `json:"text,omitempty"`
	// Evidence is the builds an ejection rests on, and Members the pull requests a landing lands.
	Evidence []SHA    `json:"evidence,omitempty"`
	Members  []Member `json:"members,omitempty"`
}
