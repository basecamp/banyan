# Banyan v3: design

Banyan is a merge queue. Pull requests are queued by a label. Banyan merges them one onto the next on the trunk's tip, has CI test those merges, and fast-forwards the trunk to the newest one that passed. A red merge is narrowed down to the pull request that caused it, and that pull request leaves the queue with a comment saying why.

v3 is a rewrite of hey-native-apps' `tools/banyan` (v2, 5,500 lines of bash) as a small Go binary. It works with any CI that GitHub hears from, and it's built to be open-sourced. It is proved first in two repositories: hey2, a Rust server on Buildkite, and hey-native-apps, Kotlin, Swift and C# on GitHub Actions moving to Buildkite.

## What it keeps from v2

- **The trunk only advances to a commit CI has seen pass.** A landing is `git push <candidate>:<trunk>`, a fast-forward to the exact SHA that was tested. Once the trunk's rules require the queue's status, a bug in Banyan stalls landing; it never lands an untested tree.
- **A tick is a pure function of what it reads,** and it stores nothing. It reads the trunk, the labelled pull requests, the candidate branches and the commit statuses on them, then takes its actions and exits. Any number of ticks may run, one at a time, from anywhere.
- **Candidates are deterministic.** Each pull request is merged onto the one before it with `git merge-tree`, using a fixed author and dates and the first parent's attributes. The same queue always builds the same SHAs, so a later tick finds the verdicts an earlier one started.
- **A red main holds the queue instead of ejecting PRs.** A red the trunk carries is nobody's in the queue.
- **Every ejection is a comment** that names the head it judged, the cause and the evidence, with a hidden trailer that tools can read.

## What it drops

v2's size comes from two things v3 doesn't do: talking to one CI provider's API, and parsing logs to guess a culprit. Its scope narrowing, compile, fixture, catalog and signature judges, the second culprit, orphans, the remainder, preflights and proofs all exist to skip a slow serial bisection. v3 has no serial bisection, so it needs none of them. Repair in place, express and the watch become separate tools that read Banyan's comments, or wait until a repository shows it needs them.

## The repository contract

Banyan speaks git and GitHub alone. It never calls a CI provider's API and never reads a log. A repository offers two things:

1. **Whole builds of the candidate branches.** A push to `banyan/<trunk>/**` runs the whole battery, with no subset a pull request would skip, and posts one commit status or check run, `banyan` by default, on that SHA. The repository's other CI systems can post their own on the same SHA, and Banyan waits for every context its config names. That covers hey-native-apps, where the Apple jobs stay on GitHub Actions and the rest moves to Buildkite.
2. **Probes (optional).** A commit whose message carries `Banyan-Probe-Of: <sha>` runs only what failed in the repository's own build of `<sha>`. It reports `success` when nothing fails again or none of those tests exists here, `failure` when a failure reproduces, and `error` when it can't tell. A repository without probes still gets correct blame, but each probe is then a whole build.

The repository's CI reads its own failure report to decide what a probe runs, so the report's format stays the repository's own.

- **hey2:** about 30 lines in `bin/dev ci`. It downloads the red build's `junit.xml` and runs those cases through a nextest filter, or the failed step alone when no case is named.
- **hey-native-apps:** already has `tools/ci --only` and v2's probe trailers.

## Candidates and blame

One dial, `slots`, sets how many candidates are under test at once. A new candidate stacks on the newest one whenever a slot is free and at least `step` pull requests wait, up to `max_batch` in one candidate.

- **Green lands itself and everything under it.** A red below a green was a flake by construction, and the tree that lands was the one tested.
- **A red candidate holding one new pull request names its culprit by construction.**
- **Any other red fans out one round of probes,** each at most `fanout`:
  - one on each prefix of the red candidate's new pull requests;
  - one on the trunk's tip alone, prefix 0;
  - one on the red SHA itself, as the control.

  Then:
  - **The lowest red probe's newest pull request is the culprit.** It is ejected with the probe as evidence.
  - **If the trunk's tip is the lowest red, the trunk is red,** whether from the clock, the environment or an outside change. Nobody is blamed, and the queue holds until the trunk's tip passes again. This is the re-run of the last green commit: the tip is the last commit Banyan saw pass.
  - **If every probe is green, the red was a flake.** A green above it lands it anyway. Otherwise the candidate's SHA is pushed again under a second branch name, `<sha12>-again`, so the CI builds the same SHA once more with no provider API. The trunk's rule only lets through a SHA whose own whole build passed, so a red build never lands.
  - **A probe that errors is pushed again once,** then run as a whole build.

`slots=1` is v2's one chain with bisection. `step=1` with many slots is Zuul's every-prefix queue. Under load the dial drifts to fewer, larger candidates on its own.

Cost per landed pull request, in whole builds, as Fable 5.2 estimated it (the inputs are assumptions until measured):

| | hey2 (N≈6, 10% red) | hey-native-apps (N≈24, 40% red) |
|---|---|---|
| Every prefix (Zuul) | 1.02–1.15 builds, blame at once | 1.10–1.57 builds |
| v2: top of the chain, serial bisection | 0.21 builds, blame in about 100 min | 0.11 builds, about 180 min |
| **v3: top, plus one round of probes** | **0.20 builds, blame in about 8 min** | **0.13 builds, about 10 min** |

The window N may not need to adapt at all. Under v3 a red costs about the same whatever N is, so a fixed `max_batch` replaces v2's AIMD window, until a red-main storm shows otherwise.

## The queue

- **`banyan add <pr>`** labels the pull request and posts a pending `banyan/queue` status on its head. That status pins the head the pull request was queued at, and makes adding it twice harmless.
  - **A head that moves** has no such status, so the pull request leaves the queue with a comment.
  - **The same status reports the outcome:** success with "landed as \<sha\>", or failure with the cause and the evidence's URL. That's the signal a landing loop reads.
- **Admission:** the pull request's base is the trunk, it isn't a draft, the required statuses pass on its head, its review threads are resolved, it has no hold label, and it merges cleanly onto the candidate below it.
  - **A conflict** ejects it with a comment, at no build cost. Repair is a separate tool that reads that comment.
- **The repository's own gates,** such as review, evidence lines or a person's word, run before `banyan add`. Banyan doesn't know about them.
- **`banyan status [<pr>] --json`** gives each pull request's state (`queued`, `candidate`, `landed`, `ejected`, `held`), its position, its candidate's SHA and build URL, and for an ejection the cause and the evidence. It reads all of this from GitHub; nothing is stored.
- **`banyan tick [--shadow]`** takes one tick's actions. `--shadow` pushes candidates and probes but never the trunk, and comments on and labels nothing.

## Configuration

`.github/banyan.yml` on the trunk holds:
- the label names;
- the candidate status context and the other required contexts;
- `slots`, `step`, `max_batch` and `fanout`;
- the merge author;
- the hold label.

The environment provides a GitHub App installation token, and nothing else is secret.

## Where the tick runs

Anywhere that can run the binary with the App's token, on the trunk's code alone, never a pull request's:
- **On GitHub Actions,** a workflow triggered by schedule, by label, and by `status` events on candidate SHAs.
- **On Buildkite,** a pipeline that builds the trunk alone, with a schedule, and a last step in each candidate build that triggers it.

The repository picks. Ticks never overlap: GitHub Actions uses a concurrency group, and Buildkite uses `concurrency: 1`.

## Proving it

1. **hey2 first, in shadow.** It builds and probes real candidates from the real queue. The factory's `land` keeps landing.
   - Compare the two over about 50 pull requests: what v3 would have landed and when, and whom it would have ejected.
   - Replay #615 with #599, #621 with #629, and #1617 as fixtures.
2. **hey2 live,** on Jeremy's word. The App bypasses the trunk's pull-request rule. A ruleset then requires `banyan` on the trunk with no bypass, so only a tested SHA can land.
3. **hey-native-apps in shadow beside v2.** Both see the same queue, and v3's probes go on their own branch prefix. The comparison covers verdicts, blame time and builds spent, with v2's stall corpus as the record of what really caused each stall.
4. **hey-native-apps live,** with v2 retired.

Rollback in either repository: disable the ruleset and stop the tick. Nothing is stored, so nothing needs migrating back.

## Open

- On Buildkite, a push to `banyan/**` starts a build only once the pipeline's filter takes that prefix. For hey2 that's a change to the pipeline's settings.
- How long a hey2 probe of a few nextest cases takes on a warm agent. That decides whether probes beat a whole build for hey2.
- Cancelling superseded candidates needs a provider API. v3 leaves them to finish, or to the CI's own "cancel on new push" setting, until the cost shows.
- How v2's stall corpus maps onto v3, which blames by probes rather than by reading logs. The corpus records v2's runs, not v3's probes, so it tests the stacking, flake and red-trunk rules, and not the probe outcomes.
