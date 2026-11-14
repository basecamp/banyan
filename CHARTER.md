# Charter

Banyan is a merge queue. This page says what it is for, what must always hold, and how we tell whether it is getting better. `go run ./eval` checks all of it. Changing this page is a person's call.

## Purpose

Land every ready pull request as soon as CI can prove it. Never land an unproven tree. Never cost an author a round trip they didn't cause.

## Principles

- **A tick is a pure function of the world it reads.** It stores nothing, so any tick can be replayed.
- **Git and GitHub only.** The queue reads commit statuses; it never calls a CI provider and never parses a log.
- **Blame takes a fair test.** A red build alone ejects nobody. Invariant 8 says what does.
- **Waiting is free; ejecting is not.** Anything that may clear by itself waits in place.
- **Small.** One planner, two dials. A change that adds a mechanism has to pay for it on the scoreboard.

## Invariants

Each is a check in `eval/`. One failure in any simulated world fails the eval.

1. The trunk only fast-forwards to a SHA whose whole build passed.
2. A pull request lands only at its head, as the build that lands it merged it, and only once that head passed its own checks.
3. With no flakes, and a trunk that fails at most once during a test, no innocent pull request is ejected.
4. Every queue drains, given a CI that eventually answers.
5. A tick on an unchanged world does nothing.
6. A crash after any side effect leaves a world the next tick finishes correctly.
7. No SHA is built twice.
8. A pull request is blamed only after a fair test. The build failed with it, passed without it, and failed with it again, with the passing build started between the two failures. A trunk or a CI that is broken for a while fails the build without it too, so it blames nobody. That holds only where the build without it runs every job the failed build ran, which the repository's CI owes the queue (`doc/design.md`, What a repository offers).

CI that breaks, recovers and breaks again inside one test can still blame an innocent pull request, and so can two random failures in a row. Those are wrong ejections, and the first indicator caps them.

## Indicators

Each is measured in simulation and live, by the same definition. A build time is one whole build of the repository. A pull request's clock starts when it is labelled and its own checks have passed, whichever is later, and stops when it lands or is ejected.

| Indicator | Target | Baseline |
|---|---|---|
| Wrong ejections per 1,000 landings | ≤ 1 | see scoreboard |
| Time to an answer, landed, p50, in build times | ≤ 1.2 | see scoreboard |
| Time to an answer, landed, p90, in build times | ≤ 2.5 | see scoreboard |
| Time to an answer, ejected as the cause, p90, in build times | to be set | see scoreboard |
| Whole builds per landing | ≤ 1.3 one at a time, ≤ 0.3 batched | see scoreboard |
| Landings an hour while a backlog stands | ≥ 12 (hey2), ≥ 16 (hey-native-apps) | see scoreboard |
| Simulated against live | within 20% | none yet |
| Engine size, non-test Go lines | none: recorded | v2: 5,500 of bash |

The targets are provisional until hey2's first live rows, and are set from those. The engine's size is on every scoreboard row so that growth shows, and has no target: a count can be met by cutting what earns its place.

## Loop

`.agents/skills/hillclimb/SKILL.md` is one iteration: pick the indicator furthest from its target or the newest escape, state one hypothesis, run the eval, and keep the change only if the scoreboard says so. Every run is recorded, kept or not.

Every live stall and every ejection later overturned becomes a case in `eval/cases/` before it is fixed.
