---
name: operate
description: Run a Banyan merge queue in a repository. Use when asked to set a queue up, queue or take out a pull request, say where one stands or why it waits, explain an ejection, stop or resume the queue, move it to a new release, or score how it is doing from the ticks it recorded.
---

# Operate

The queue is `banyan tick`, run by the repository's own workflow. It stores nothing: everything it knows is in git and in GitHub's statuses, so everything here is read from those.

`README.md` lists the commands and the settings. `doc/design.md` says why the queue did what it did.

## Where a pull request stands

- **One pull request:** the `merge-queue` status on its head. It says its place, what it waits on by name, that it landed, or that it left and why.
- **The queue:** the open pull requests that carry the label, each with that status. `banyan tick --dry-run`, in a clone of the repository, prints what the next tick would do and does none of it.
- **Its builds:** the branches under `merge-queue/`, each with its CI's status on the commit.
- **Why it left:** the comment on it names the head judged, the cause and the builds the verdict rests on. A culprit's comment links three builds: failed with it, passed without it, failed with it again.

A status that begins "the queue is stopped:" is the queue's own failure, not the pull request's. The rest of the line is git's or GitHub's refusal. Fix that, and the next tick writes each place back.

## Queue, take out, stop

- **Queue:** label it. Where it can't join yet, its status says what it waits on.
- **Take out:** take the label off. The next tick drops the builds that hold it, and a landing rereads the labels just before it moves the trunk.
- **Stop:** disable the tick's workflow and cancel the runs still waiting. A tick already running may land what it read as passed. Enable the workflow to resume: nothing is lost, since nothing was stored.

A push to a queued pull request keeps it queued: the builds that hold it are dropped, and the new head lands once its own checks pass. Take the label off first where that isn't wanted.

## What a tick did

Each tick records what it read and what it planned, where the workflow passes `--record <dir>` and uploads the folder.

- **Plan one again:** `go run ./eval -replay <recording>` in Banyan's repository prints the actions that world calls for. It is the same function the tick ran, so a surprise replays exactly on the release that recorded it.
- **Score a stretch of them:** `go run ./eval -live <dir>` in Banyan's repository, with every recording in one folder. The row has the simulator's columns, so it sits beside `eval/scoreboard.tsv`.
- **A day's row:** `eval/daily-row <owner/repo> <YYYY-MM-DD> [<shape>]` fetches the day's recordings (`eval/fetch-ticks`), scores 01:17Z to 21:17Z (`go run ./eval -live <dir> -from <time> -to <time> -row`, one JSON object, with the simulated row beside it where a shape is named) and reads the day's take-outs from the comments that carry the eject marker (`go run ./eval -takeouts <owner/repo> -from <time> -to <time>`): who made each, whether the same head landed since, and whether a check that had failed passed later on the same commit. It reads the same two for the four hours before the window, 21:17Z the day before to 01:17Z, with no simulated row, so no landing falls outside every row: they go under `before` in the JSON and in a block after the day's tripwires, and judge no tripwire and add no column to `rows.tsv`. The row's `busiest_hour_landings` is the most pull requests landed in the window within 60 minutes of one of its landings. Its `backlog_longest_minutes` is the longest run of records, none more than 10 minutes after the last, after whose own actions a pull request that could be built (ready, and not in conflict with the trunk) is in no pending candidate or push and every slot holds a pending candidate or a candidate's push (a round's probes and checks run beside the slots and hold none, as do a candidate marked landed while landed builds are kept and one pending past the timeout, as the engine counts them). The busiest hour is judged against the charter's landings an hour only on a day that backlog stood 60 minutes, wherever in the day the hour falls (on 19 and 25 Nov 2026 it was the hour after the backlog cleared); `landings_per_hour` is landings over every hour anything was queued, which is not the charter's measure. Its `building_candidates_dropped` counts the candidates a tick deleted while they were still building because a member's head had moved or a member had left the queue without landing or being ejected, with `dropped_members_still_queued`, how many of their pull requests stayed queued at the same head (a lower candidate may still land one without a new build), and the longest any had built; it is shown and trips nothing. Its `first_candidate_p90_minutes` and `first_candidate_over_30` run from a landing's ready time to the push of the earliest candidate that held its pull request at the head that landed (the landing candidate itself when none was pushed sooner), at most the minutes the queue added, and its row line gives them beside the queue-added lines. For the landings held over 30 minutes, `held_first_waited`, `held_first_failed`, `held_first_dropped`, `held_first_passed`, `held_first_overtaken` and `held_first_other` say, in that order of precedence, whether that first candidate came over 30 minutes after ready, was read `Failure` or `Error` by the record that landed the pull request, was deleted while building by that record, was read passing before it, was still building at it or first read passing in it, or none of those; they sum to `queue_added_over_30`, and their line is shown only on a day that held one, and judges nothing. It keeps the row as JSON and Markdown with a line in `rows.tsv`, and hands the Markdown to `$BANYAN_ROW_POST` once a day. Reads go through `$BANYAN_GH`. With `$BANYAN_CLONE` naming a clone that holds the blamed heads, the row checks a landing after an ejection as the cause; without it, the row says how many it could not check.

A surprise from a real queue becomes a case in `eval/cases/` before its fix. The hillclimb skill says how.

## Set one up

Do these in order, and prove each before the next.

1. **CI builds every `merge-queue/**` branch whole** and reports one commit status, `merge-queue/build` unless `build` names others. Push a branch there by hand and watch the status arrive. The queue itself reads only branches named `merge-queue/<trunk>/<sha12>`. With `keep` set, one pushed by hand under that name at the trunk's tip is marked and stands at least `keep` before the queue deletes it; at an older trunk commit it goes as a candidate beneath the tip does.
2. **No branch is named `merge-queue` itself.** Git can't hold it beside the queue's branches, and every push is refused while it exists.
3. **The tick's identity** can push to the trunk and to `merge-queue/**`, write statuses, comments and labels, and change workflow files if pull requests do. Where the trunk requires pull requests, that identity bypasses the rule and nobody else does. Where the trunk requires a status, CI posts it on the queue's builds too, or that identity bypasses it: GitHub takes a push of one merge on its pull request's statuses, and refuses a push of two.
4. **The tick runs one at a time,** on a schedule and when: one of the queue's labels goes on or comes off, the trunk moves, a build begins or reports, and a status in `admit` reports. GitHub runs a five-minute schedule every six to fifteen, so the events carry the queue and the schedule only catches what they miss.
5. **`.github/merge-queue.json`** holds what differs from the defaults. Set `started` where the CI's pending status says a build is running, and `cancels` where it stops the build of a deleted branch.
6. **The binary is pinned** by release and checksum, and a new release is a pull request that changes both.

Then rehearse before the real repository: a clash (one pull request that passes alone and fails behind another), a flake, a trunk that fails by itself, and two pull requests that land in one push under the trunk's real protection. The queue must take out only the first's culprit, and nobody in the others.

## What to know about CI

- **A cancelled build may report as failed.** Buildkite does. The queue deletes a branch before its build is cancelled, so it never reads that verdict. A build cancelled by hand while its branch stands reads as red, and costs the builds that clear it.
- **A lost agent may report as failed.** Buildkite does, as exit status -1 with nothing to tell it from a red test. The queue clears it as it clears a flake, at about a build time and three builds each. Have the pipeline retry a step whose agent was lost, so the queue only reads a red the tests gave.
- **An open lane pays for every merge made beside it.** A merge by hand moves the trunk under the running builds, and each is built again on the new tip. The queue is quick once most landings go through it.
- **What a repository admits on is its own policy.** `admit` names statuses on the pull request's head, and the queue reads only their state. Decide there who may post them.
