# Design

Banyan is a merge queue. Pull requests are queued by a label. Banyan merges them one onto the next on the trunk's tip, has CI build those merges, and fast-forwards the trunk to the highest one that passed. A red merge is narrowed to the pull request that caused it, which leaves the queue with a comment saying why.

`CHARTER.md` says what must hold and how it is scored. This page says how the current challenger works. The scoreboard, not this page, says whether it is good.

## The world and the tick

A tick is `queue.Plan`: a pure function from the world it reads to the actions it takes. It stores nothing.

The world is the trunk's tip, the labelled pull requests, and the builds: every commit Banyan pushed whose branch still stands, with CI's verdict on it. Everything a later tick needs is in git and in GitHub's statuses, so a tick can run anywhere, and a recorded world replays exactly.

Commits are deterministic. A merge is built from its parents, the head it merges and a note, with fixed author and dates, so the same queue always gives the same SHAs and a later tick finds the builds an earlier one pushed.

## The chain

The chain is the admitted pull requests merged one onto the next on the tip. Prefix j is the tip plus the first j of them; prefix 0 is the tip alone.

A candidate is a whole build of a prefix. A new candidate stacks on the last whenever a slot is free, adding up to `max_batch` pull requests. With `max_batch: 1` every prefix is built; with a large one only every Nth is.

## Landing

The highest prefix whose whole build passed lands, with everything under it. A red below a green was a flake, or a break that a later pull request fixed, and the tree that lands is the one that was built.

The landing is one atomic push: the trunk moves to the candidate, and the candidate's branch is deleted on the condition that it still points there. Just before the push the queue is read again, and the landing is dropped unless every pull request in it is still queued at the head that was merged.

## Blame

A red candidate that is the lowest on the chain starts a round: builds of some of its prefixes, begun together. The first round is the tip and the candidate itself.

- **The candidate passes this time:** the red was a flake, and it lands.
- **Nothing passes, the tip included:** the trunk is red. Nobody is blamed. The queue holds and tries a round again after `recheck`. If a pull request labelled as a fix is in the chain, the prefix that ends at it is built too, since that is where a green would show. If one waits outside the chain, the chain is dropped so that the fix is built on the tip.
- **Prefix g passes and a prefix above it fails:** the next round narrows between them. When they are next to each other, the pull request between them is the suspect. It leaves once its prefix has failed twice, one of those in the same round as the pass below it.

"Begun together" is what makes blame safe against the clock. A commit that passed an hour ago proves nothing about a red now.

A round's builds are whole builds by default. Where a repository's CI offers probes, a round uses them instead: a probe is an empty commit on a prefix that asks CI to run only what failed in the red build.

Only `failure` is evidence. A build that was lost, cancelled or timed out is built again.

## Who leaves the queue

- **A culprit,** by the rule above.
- **A conflict with the trunk itself.**
- **A head that moved** after it was queued.

Everything else waits in place: a conflict with a pull request ahead, the pull request's own checks running or red, a hold label, a red trunk.

Every ejection is a comment that names the head judged, the cause and the builds it rests on, with the marker `<!-- banyan eject <head> attributed=<cause> -->`.

## No SHA is built twice

A verdict belongs to one commit for good. A retry is the same merge carrying `Banyan-Try: <n>`, which is a new commit. The planner takes the first n whose commit it has never pushed, so a flake's retry, a lost build, and a pull request queued again all get a fresh build and a fresh verdict.

## What a repository offers

1. **A whole build of every push to the queue's branches,** reported as one commit status. Whatever decides that status is the repository's own: a gate that counts a job skipped by design as passed is the repository's gate. Several CI systems may each report, and the queue waits for every context its config names.
2. **Probes, optionally.** A commit carrying `Banyan-Probe-Of: <sha>` runs only what failed in the build of that SHA, and reports under the probe context, which the trunk's rule never accepts.
3. **Cancelling, optionally.** The queue deletes the branch of a build it no longer needs. The repository's CI may cancel a build whose branch is gone.

The queue never calls a CI provider's API and never reads a log.

## Statuses

| Context | On | Says |
|---|---|---|
| `merge-queue` | a pull request's head | It pins the head that was queued, shows its place, and ends as landed or ejected with the cause. |
| `merge-queue/build` | a candidate | CI's whole-build verdict. The trunk's rule requires it. |
| `merge-queue/probe` | a probe | CI's verdict on what the red build failed. |

The queue posts `pending` on everything it pushes, which gives each wait a start. A build with no verdict after `timeout` is built again.

## Dials

`slots` is how many builds are under test at once. `max_batch` is how many pull requests a candidate adds. `eval/main.go` holds the values each shape ships with, and the hillclimb moves them.

## Not in the engine

- **A repository's own gates:** review, evidence, a person's word. They run before a pull request is queued.
- **Repair of a trunk conflict.** A separate tool can read the eject comment and merge the trunk in. A pull request kept on a person's word is never pushed to.
- **A watch.** A stall alarm reads `banyan report`.
- **A stop switch.** A person stops the queue by stopping its tick.

## Going live in a repository

1. A scratch repository, with the real rulesets and CI on a stub build.
2. An open lane: the queue lands the pull requests labelled for it, and the merge button still works for everyone else.
3. Exclusive: a ruleset requires `merge-queue/build` on the trunk, and only Banyan's App updates it. Rollback is disabling that ruleset.

Each stage is scored by `banyan report` against the simulated row.
