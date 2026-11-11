# Charter

Banyan is a merge queue. This page says what it is for, what must always hold, and how we tell whether it is getting better. `go run ./eval` checks all of it. Changing this page is a person's call.

## Purpose

Land every ready pull request as soon as CI can prove it. Never land an unproven tree. Never cost an author a round trip they didn't cause.

## Principles

- **A tick is a pure function of the world it reads.** It stores nothing, so any tick can be replayed.
- **Git and GitHub only.** The queue reads commit statuses; it never calls a CI provider and never parses a log.
- **Evidence before blame.** A pull request leaves the queue for a red build only when a build without it passed at the same time.
- **Waiting is free; ejecting is not.** Anything that may clear by itself waits in place.
- **Small.** One planner, two dials. A change that adds a mechanism has to pay for it on the scoreboard.

## Invariants

Each is a check in `eval/`. One failure in any simulated world fails the eval.

1. The trunk only fast-forwards to a SHA whose whole build passed.
2. A pull request lands only at the head it was queued at.
3. With no flakes, no innocent pull request is ejected.
4. Every queue drains, given a CI that eventually answers.
5. A tick on an unchanged world does nothing.
6. A crash after any side effect leaves a world the next tick finishes correctly.
7. No SHA is built twice.
8. Nobody is ejected while the trunk's own tip fails.

## Indicators

Each is measured in simulation and live, by the same definition. A build time is one whole build of the repository.

| Indicator | Target | Baseline |
|---|---|---|
| Wrong ejections per 1,000 landings | ≤ 1 | see scoreboard |
| Queue to landed, p50, in build times | ≤ 1.2 | see scoreboard |
| Queue to landed, p90, in build times | ≤ 2.5 | see scoreboard |
| Red to blame, p90, in build times | ≤ 1.5 without probes, ≤ 0.5 with | see scoreboard |
| Whole builds per landing | ≤ 1.3 one at a time, ≤ 0.3 batched | see scoreboard |
| Landings an hour under backlog | ≥ 12 (hey2), ≥ 16 (hey-native-apps) | see scoreboard |
| Simulated against live | within 20% | none yet |
| Engine size, non-test Go lines | ≤ 2,000 | v2: 5,500 of bash |

The targets are provisional until Jeremy fixes them against the first baseline rows in `eval/scoreboard.tsv`.

## Loop

`.agents/skills/hillclimb/SKILL.md` is one iteration: pick the indicator furthest from its target or the newest escape, state one hypothesis, run the eval, and keep the change only if the scoreboard says so. Every run is recorded, kept or not.

Every live stall and every ejection later overturned becomes a case in `eval/cases/` before it is fixed.
