# Banyan

A merge queue for GitHub that works with any CI.

Label a pull request and Banyan merges it onto the ones queued before it, has your CI build that merge, and fast-forwards your trunk to it once it passes. When a merge fails, Banyan finds the pull request that caused it and takes that one out, with a comment saying why. The trunk only ever moves to a commit your CI has passed.

Banyan talks to git and GitHub and nothing else. Your CI builds the branches Banyan pushes and reports a commit status, as it does for any branch. That is the whole integration, so GitHub Actions, Buildkite and anything else that posts a status work alike, and can be mixed.

## Use

```
banyan add <pr> [--first]        # queue a pull request, or say in one line why not
banyan status [<pr>] [--json]    # the queue, or one pull request's place or outcome
banyan remove <pr> <who> <why>   # take one out, saying who and why on it
banyan tick [--dry-run]          # land what passed, build what is next
banyan replay <recording>        # plan a recorded tick again
```

`banyan tick` is the queue. It stores nothing: it reads the trunk, the labelled pull requests and the branches it pushed, acts, and exits. Run it on a schedule and whenever a build begins or finishes, one at a time, from the trunk's own checkout.

## Set up

1. **CI builds `merge-queue/**` branches whole,** and reports the commit status `merge-queue/build`. No branch may be named `merge-queue` itself: git can't hold it beside them, and the queue's pushes are refused while it exists.
2. **A tick runs every few minutes,** with a token that can push to the trunk and to `merge-queue/**`, and write statuses, comments and labels.
3. **`.github/merge-queue.json`** holds whatever differs from the defaults:

```json
{
  "trunk": "main",
  "label": "merge-queue",
  "admit": ["ci"],
  "threads": true,
  "slots": 4,
  "max_batch": 1
}
```

`admit` names the statuses or check runs a pull request's own head must pass before it joins the queue (`"review|security-review"` passes on either), and `threads` keeps one out while a review thread is unresolved (the queue reads its first hundred, and one with more waits). `slots` is how many candidates build at once, and `max_batch` how many pull requests each adds. The builds that find a failure's cause run beside them: three for each pull request the red build added, all at once where `started` is set. Set `started` to a word your CI's pending status carries once a build is running (Buildkite's says "started"), so the queue can find a failure's cause in one build time, not three; check runs need no setting. Set `cancels` where your CI stops a build whose branch is deleted; otherwise a build the queue no longer wants keeps its slot until it reports. `run.Config` in `internal/run/run.go` lists every setting.

To make the queue the only way in, require `merge-queue/build` on the trunk and let only Banyan's token update it.

## How it is built

[CHARTER.md](CHARTER.md) says what must always hold and how the queue is scored. [doc/design.md](doc/design.md) says how it works. `go run ./eval` plays the planner against thousands of simulated worlds, checks every invariant, and scores it against `eval/scoreboard.tsv`. A change is kept only if the scoreboard says so.

```
go test ./...     # the planner, git and GitHub adapters, end to end against a fake GitHub
go run ./eval     # the invariants and the indicators
```
