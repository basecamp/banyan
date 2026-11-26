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
3. With no flakes, and a trunk that fails at most once during a test, no innocent pull request is ejected. A pull request is innocent when the trunk with it and the pull requests ahead of it passes.
4. Every pull request that is ready lands or leaves as the cause, given a CI that eventually answers. One that waits on its own checks, a conflict or a hold is not ready, and waits until that clears.
5. A tick on an unchanged world does nothing.
6. A crash after any side effect leaves a world the next tick finishes correctly.
7. No commit is pushed for a build twice, so a verdict answers for one build. A tree is built again as a new commit.
8. A pull request is blamed only after a fair test. The build failed with it, passed without it, and failed with it again, with the passing build started between the two failures. A trunk or a CI that is broken for a while fails the build without it too, so it blames nobody. That holds only where the build without it runs every job the failed build ran, which the repository's CI owes the queue (`doc/design.md`, What a repository offers).

CI that breaks, recovers and breaks again inside one test can still blame an innocent pull request. So can a break that starts inside a test, after its passing build finished and before a build begun earlier ran the job that fails, and so can two random failures in a row. Those are wrong ejections, and the first indicator caps them. The cap holds while under about a fifth of the builds of good trees fail. Past that it does not, and mending CI is the repository's (`doc/design.md`, Blame).

## Indicators

Each is measured in simulation and live, by the same definition. A build time is one whole build of the repository. A pull request's clock starts when it is labelled and its own checks have passed, whichever is later, and stops when it lands or is ejected. The minutes the queue adds to a landing run from that start to the push of the build that lands it: the build itself is CI's time.

The baseline is the live rows of 16, 17 and 18 Nov 2026, hey-native-apps first, then hey2. A build took 31 to 34 minutes in hey-native-apps and 23 to 34 in hey2.

| Indicator | Target | Baseline |
|---|---|---|
| Wrong ejections per 1,000 landings | ≤ 1 | 0 in 409 landings; 0 in 122 |
| Time to an answer, landed, p50, in build times | ≤ 1.2 | 1.46, 1.35, 1.15 (45, 43, 39 min); 1.04, 1.01, 1.03 (24, 25, 35 min) |
| Time to an answer, landed, p90, in build times | ≤ 2.0 | 2.14, 2.29, 2.16 (66, 74, 74 min); 1.35, 1.93, 1.43 (31, 48, 49 min) |
| Minutes the queue adds to a landing, p50 | ≤ 6 | 11.2, 7.9, 5.6; 0.2, 0.6, 0.6 |
| Minutes the queue adds to a landing, p90 | ≤ 20 | 25, 31, 36; 4.1, 0.8, 4.9 |
| Landings the queue adds over 30 minutes to | ≤ 1 in 20 | 6 of 64, 16 of 153, 24 of 192; 0 of 58, 1 of 37, 0 of 27 |
| Pull requests in candidates that went red, per 100 landings | none: recorded | 5, 25, 34 (3 of 64, 38 of 153, 66 of 192); 28, 3, 26 (16 of 58, 1 of 37, 7 of 27) |
| Labelled to landed, p50, in build times | ≤ 2.2 | 2.1, 2.4, 1.9 (65, 76, 66 min); 2.4, 1.5, 1.1 (54, 36, 38 min) |
| Labelled to landed, p90 and mean of the slowest twentieth, in build times | none: recorded | p90 5.6, 7.5, 3.6; 15.2, 7.9, 13.1 |
| Time to an answer, ejected as the cause, p90, in build times | ≤ 3.0 | 2.5, none, 3.1 (77, 105 min) over 3 ejections; 2.6 (59 min) over 2 |
| Whole builds per landing | ≤ 1.3 one at a time, ≤ 0.5 batched | 1.17, 1.05, 1.07 one at a time; 0.52, 0.42, 0.43 batched |
| Landings an hour while a backlog stands | ≥ 12 (hey2), ≥ 16 (hey-native-apps) | untested in hey2: no day held a backlog, 8 in its busiest hour; 22, 20 and 18 in hey-native-apps' busiest hours |
| Simulated against live: landings, p50, p90 and whole builds per landing | each within 20% | hey-native-apps within 12% on the three days it was built against; no day it had not seen |
| Engine size, non-test Go lines | none: recorded | v2: 5,500 of bash |

Labelled to landed has a target at the median only. Past it, the wait belongs to a head whose own checks stayed red after it was labelled, which the queue can't shorten: hey2's slowest twentieth was 188 build times on 16 Nov, from heads labelled days earlier. The simulated row is given the day's arrivals and stops at the window's end, so a slower planner lands fewer and its waits read shorter for it. Landings are compared with the other three numbers for that reason, and the replay records what each side left waiting. The minutes the queue adds, the landings it holds over 30 minutes and the pull requests in red candidates are defined on a recorded day: they are read from the daily row and its replay, not from the scoreboard's seeded worlds. `go run ./eval -live <ticks> -like <shape> -from <time> -to <time> -row` prints one JSON row: the live measures at the top, the replay's under `simulated`, and the four comparisons under `over_live`. Pull requests in candidates that went red are counted once for each such candidate, and have no target: the count says how much of a day's wait began with a red build, which CI owns until the queue spreads it. The engine's size is on every scoreboard row so that growth shows, and has no target: a count can be met by cutting what earns its place.

## Loop

`.agents/skills/hillclimb/SKILL.md` is one iteration: pick the indicator furthest from its target or the newest escape, state one hypothesis, run the eval, and keep the change only if the scoreboard says so. Every run is recorded, kept or not.

An indicator short of its target with no change to the engine or its settings sized to move it is not an iteration. It gets a card to whoever owns the cause, with the numbers, and the loop takes the next.

Every live stall and every ejection later overturned becomes a case in `eval/cases/` before it is fixed.
