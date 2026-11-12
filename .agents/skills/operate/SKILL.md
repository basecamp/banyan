---
name: operate
description: Run a Banyan merge queue in a repository. Use when asked to set a queue up, queue or take out a pull request, say where one stands or why it waits, explain an ejection, stop or resume the queue, move it to a new release, or score how it is doing from the ticks it recorded.
---

# Operate

The queue is `banyan tick`, run by the repository's own workflow. It stores nothing: everything it knows is in git and in GitHub's statuses, so everything here is read from those.

`README.md` lists the commands and the settings. `doc/design.md` says why the queue did what it did.

## Where a pull request stands

- **One pull request:** the `merge-queue` status on its head. It says its place, what it waits on by name, that it landed, or that it left and why. `banyan status <pr> --json` reads the same.
- **The queue:** `banyan status`, in a clone of the repository.
- **Its builds:** the branches under `merge-queue/`, each with its CI's status on the commit.
- **Why it left:** the comment on it names the head judged, the cause and the builds the verdict rests on. A culprit's comment links three builds: failed with it, passed without it, failed with it again.

A status that begins "the queue is stopped:" is the queue's own failure, not the pull request's. The rest of the line is git's or GitHub's refusal. Fix that, and the next tick writes each place back.

## Queue, take out, stop

- **Queue:** label it, or `banyan add <pr>`, which says in one line why not where it can't.
- **Take out:** take the label off, or `banyan remove <pr> <who> <why>`, which says so on the pull request.
- **Stop:** disable the tick's workflow and cancel the runs still waiting. A tick already running may land what it read as passed. Enable the workflow to resume: nothing is lost, since nothing was stored.

Never push to a queued pull request to fix it in place. A push after the label takes it out.

## What a tick did

Each tick records what it read and what it planned, where the workflow passes `--record <dir>` and uploads the folder.

- **Plan one again:** `banyan replay <recording>` prints the actions that world calls for. It is the same function the tick ran, so a surprise replays exactly.
- **Score a stretch of them:** `go run ./eval -live <dir>` in Banyan's repository, with every recording in one folder. The row has the simulator's columns, so it sits beside `eval/scoreboard.tsv`.

A surprise from a real queue becomes a case in `eval/cases/` before its fix. The hillclimb skill says how.

## Set one up

Do these in order, and prove each before the next.

1. **CI builds every `merge-queue/**` branch whole** and reports one commit status, `merge-queue/build` unless `build` names others. Push a branch there by hand and watch the status arrive.
2. **No branch is named `merge-queue` itself.** Git can't hold it beside the queue's branches, and every push is refused while it exists.
3. **The tick's identity** can push to the trunk and to `merge-queue/**`, write statuses, comments and labels, and change workflow files if pull requests do. Where the trunk requires pull requests, that identity bypasses the rule and nobody else does.
4. **The tick runs one at a time,** on a schedule and when: one of the queue's labels goes on or comes off, the trunk moves, a build begins or reports, and a status in `admit` reports. GitHub runs a five-minute schedule every six to fifteen, so the events carry the queue and the schedule only catches what they miss.
5. **`.github/merge-queue.json`** holds what differs from the defaults. Set `started` where the CI's pending status says a build is running, and `cancels` where it stops the build of a deleted branch.
6. **The binary is pinned** by release and checksum, and a new release is a pull request that changes both.

Then rehearse before the real repository: a clash (one pull request that passes alone and fails behind another), a flake, and a trunk that fails by itself. The queue must take out only the first's culprit, and nobody in the other two.

## What to know about CI

- **A cancelled build may report as failed.** Buildkite does. The queue deletes a branch before its build is cancelled, so it never reads that verdict. A build cancelled by hand while its branch stands reads as red, and costs the builds that clear it.
- **A lost agent may report as failed.** Buildkite does, as exit status -1 with nothing to tell it from a red test. The queue clears it as it clears a flake, at about a build time and three builds each. Have the pipeline retry a step whose agent was lost, so the queue only reads a red the tests gave.
- **An open lane pays for every merge made beside it.** A merge by hand moves the trunk under the running builds, and each is built again on the new tip. The queue is quick once most landings go through it.
- **What a repository admits on is its own policy.** `admit` names statuses on the pull request's head, and the queue reads only their state. Decide there who may post them.
