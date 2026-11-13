# Design

Banyan is a merge queue. Pull requests are queued by a label. Banyan merges them one onto the next on the trunk's tip, has CI build those merges, and fast-forwards the trunk to the highest one that passed. A red merge is narrowed to the pull request that caused it, which leaves the queue with a comment saying why.

`CHARTER.md` says what must hold and how it is scored. This page says how the current challenger works. The scoreboard, not this page, says whether it is good.

## The world and the tick

A tick is `queue.Plan`: a pure function from the world it reads to the actions it takes. It stores nothing.

The world is the trunk's tip, the labelled pull requests, and the builds: every commit Banyan pushed whose branch still stands, with CI's verdict on it. Everything a later tick needs is in git and in GitHub's statuses, so a tick can run anywhere, and a recorded world replays exactly.

Commits are deterministic. A merge is built from its parents, the head it merges and a note, with fixed author and dates, so the same queue always gives the same SHAs and a later tick finds the builds an earlier one pushed.

## The chain

The chain is the admitted pull requests merged one onto the next on the tip. Prefix j is the tip plus the first j of them; prefix 0 is the tip alone.

A candidate is a whole build of a prefix. A new candidate stacks on the last whenever a slot is free, adding up to `max_batch` pull requests. It is merged onto the newest build of the prefix below that may still pass, so a pull request queued behind a build that was begun again is built on the commit that can land. With `max_batch: 1` every prefix is built; with a large one only every Nth is.

## Landing

The highest prefix whose whole build passed lands, with everything under it. A red below a green was a flake, or a break that a later pull request fixed, and the tree that lands is the one that was built.

The landing is one atomic push: the trunk moves to the candidate, and the candidate's branch is deleted on the condition that it still points there. Just before the push the queue is read again, and the landing is dropped unless every pull request in it is still queued at the head that was merged. A push the remote refuses is planned again, since the trunk may have moved under it. One it refuses twice in a tick lost no race: the queue stops, and says the remote's reason on every pull request it holds.

## Blame

A red candidate at the bottom of the chain is set aside with the pull requests it added, and the chain goes on without them, built on the tip. A candidate stacked on the red one is dropped once it fails too. Until then it may yet land them all. Where rounds are probes, which answer in a fraction of a build, the chain waits for that answer. Where they are whole builds it does not wait: it is built beside the candidates still running.

The red candidate is narrowed by rounds: builds of some of its prefixes. A round's build never lands. What the rounds have said so far is a span, from the highest prefix that passed to the lowest above it that failed. The culprit is in it. A round is read once every build in it has reported, and the rounds are read in order.

- **The candidate's tree passes:** the red was a flake, and its pull requests go back on the chain. Where probes passed, a whole build of the tree decides. The build that passed is a round's and never lands, so a flake costs one build more than it has to. Letting it land would save that build on about three candidates in a hundred, which is under a twentieth on every indicator, so it is left as it is until a repository's own flake rate says otherwise.
- **The tip fails:** the trunk is red. Nobody is blamed. The queue holds, and builds the tip and the candidate's tree again after `recheck`. If a pull request labelled as a fix has yet to be built, the chain goes on for it though the trunk is red. A pull request labelled as a fix ends the batch it is in, so its candidate holds nothing queued after it.
- **A prefix that had passed fails:** nothing the rounds have said so far holds, and they start over. No test counts a build from before then.
- **The span is wide:** a round narrows it. A round of probes builds every prefix in it, so one round finds the pair. A round of whole builds splits it into as many parts as there are slots.
- **The span is one pull request, or one round of whole builds reaches all of it:** its pull requests are put to the fair test.

### The fair test

A pull request leaves on three builds in three rounds in a row: its prefix failed, the prefix below it passed, and its own failed again. Each round is pushed as soon as CI has begun every build of the round before, whatever that round will say. So the three run side by side, and each began after the last. No more than three rounds are begun past the last one read, since a test is three: a round whose builds report minutes apart starts no test over on its half. A trunk or a CI that was broken for a while and failed both failures would have failed the pass between them too.

Where the span holds several pull requests, each round is the next build of every one's test, so the test finds the culprit wherever it is. A pass of the candidate's own tree clears it, as above.

The planner never compares two times. It reads only whether CI has begun a build, and the order comes from the order of the pushes. Where CI does not say when a build began, a round is pushed once the round before has reported, and the three builds run one after another.

The red candidate's own failure is not one of the three. It began a build time or more before the test, so a trunk that was red then and is red again can explain both, and a random failure of the candidate needs only one more to blame somebody. With both failures inside the test, the simulator ejects 1 innocent pull request in 184,000 landings on hey2, against 25 where the candidate's failure counts.

A build that is lost breaks the test, and the next round starts it again. So the three builds of a test always begin close together.

A pull request leaves as soon as the third build of its test reports, though another build of that round, or of the round before, may still be running. Should that build fail a prefix that had passed, the rounds would have started over and nobody would have left. On a good tree that takes four random failures and a verdict that arrives late. Waiting for those builds was tried: it moved wrong ejections from 0.49 to 0.44 in 1,000 simulated landings at hey2's 18% flakes, which is inside their scatter, and made ejections 7% later, so the scoreboard rejected it. The simulator's flakes are independent, so a repository whose builds fail together would want it back.

Only the pull requests the rounds have not ruled out stay off the chain. The rest go back on it at once. One whose merge there would be the red candidate's own tree waits with them, since a build of that tree is an attempt of the red candidate and not a candidate on the chain.

A red candidate and its rounds stand while the trunk moves under them, since they prove what they prove about the commits they were built on. They go when a pull request in the trees they build leaves the queue, and when their base is no longer the tip and fails by itself. So several red candidates are narrowed at once, each beside a chain that keeps landing.

A round's builds are whole builds by default. Where a repository's CI offers probes, a round uses them instead: a probe is an empty commit on a prefix that asks CI to run only what failed in the red build.

Only `failure` is evidence. A build that was lost, cancelled or timed out is built again.

## Who leaves the queue

- **A culprit,** by the rule above.
- **A conflict with the trunk itself.**

Everything else waits in place: a conflict with a pull request ahead, the pull request's own checks running or red, a hold label, a red trunk, a red candidate being narrowed.

A push to a queued pull request keeps it queued. The label says to land the pull request, and the checks named in `admit` say which head may land. The builds that merged the old head are dropped, the new head waits for its own checks, and it joins behind the builds already running. So where `admit` is empty, whatever is pushed to a labelled pull request lands once the queue's build passes.

Every ejection is a comment that names the head judged, the cause and the builds it rests on, with the marker `<!-- banyan eject <head> attributed=<cause> -->`.

## No SHA is built twice

A verdict belongs to one commit for good. A retry is the same merge carrying `Banyan-Try: <n>`, which is a new commit. The planner takes the first n whose commit it has never pushed, so a flake's retry, a lost build, and a pull request queued again all get a fresh build and a fresh verdict.

## What a repository offers

1. **A whole build of every push to the queue's branches,** reported as one commit status. Whatever decides that status is the repository's own: a gate that counts a job skipped by design as passed is the repository's gate. Several CI systems may each report, and the queue waits for every context its config names.
2. **Probes, optionally.** A commit carrying `Banyan-Probe-Of: <sha>` runs only what failed in the build of that SHA, and reports under the probe context, which the trunk's rule never accepts.
3. **Cancelling, optionally.** The queue deletes the branch of a build it no longer needs. The repository's CI may cancel a build whose branch is gone. `cancels` says whether it does. Where it does not, the build runs to its end and holds an agent, so a candidate the queue no longer wants is left until it reports and holds its slot. Otherwise every head that moves leaves a chain's worth of builds behind, and on a small pool the queue's own leavings outrun the agents.

4. **When a build began, optionally.** Where the statuses say CI has begun a build, a fair test's rounds overlap and it answers in about one build time. Where they do not, it takes three. A check run says so itself. A commit status says so in words, and `started` names a word its pending status carries once the build runs: Buildkite's reads "Build #7 started".

The queue never calls a CI provider's API and never reads a log.

## Statuses

| Context | On | Says |
|---|---|---|
| `merge-queue` | a pull request's head | It pins the head that was queued, shows its place, and ends as landed or ejected with the cause. |
| `merge-queue/build` | a candidate | CI's whole-build verdict. The trunk's rule requires it. |
| `merge-queue/probe` | a probe | CI's verdict on what the red build failed. |

The queue posts `pending` on everything it pushes, which gives each wait a start. A build with no verdict after `timeout` is built again.

## Dials

`slots` is how many candidates are under test at once. A round's builds are beside them and are not counted. `max_batch` is how many pull requests a candidate adds. `eval/main.go` holds the values each shape ships with, and the hillclimb moves them.

## Not in the engine

- **A repository's own gates:** review, evidence, a person's word. They run before a pull request is queued.
- **Repair of a trunk conflict.** A separate tool can read the eject comment and merge the trunk in. A pull request kept on a person's word is never pushed to.
- **A watch.** A stall alarm reads the `merge-queue` statuses.
- **Commands to queue, take out or list.** The label is how a pull request joins and leaves, and the status on its head is where it stands. `gh` does both.
- **A stop switch.** A person stops the queue by stopping its tick.

## Going live in a repository

1. A scratch repository, with the real rulesets and CI on a stub build.
2. An open lane: the queue lands the pull requests labelled for it, and the merge button still works for everyone else.
3. Exclusive: a ruleset requires `merge-queue/build` on the trunk, and only Banyan's App updates it. Rollback is disabling that ruleset.

Each stage is scored against the simulated row, by the same indicators read from GitHub.
