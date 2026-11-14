---
name: hillclimb
description: One iteration of Banyan's improvement loop. Use when asked to improve the merge queue, close a gap to a charter target, work an escape (a live stall or an ejection later overturned), or run the loop on a schedule.
---

# Hillclimb

One iteration changes one thing and leaves one row on the scoreboard. Run it by hand, or again and again.

Read `CHARTER.md` first. It holds the invariants and the targets. Do not change it: that is a person's call.

## One iteration

1. **Pick the gap.** Run `go run ./eval -baselines` and read the challenger rows against the charter's targets. Take the newest escape in `eval/cases/` if one is unfixed. Otherwise take the indicator furthest from its target, as a ratio.
2. **Look before guessing.** Replay a world where the gap shows: `go run ./eval -trace "<shape> <slots> <batch> <probes> <seed>"`. Read what the queue did and say in one sentence why the indicator is where it is. To see what a dial would do before you spend a run on it: `go run ./eval -try "<shape> <slots> <batch> <probes>; ..."`, which measures and records nothing.
3. **State one hypothesis and its change.** One mechanism or one dial, small enough to explain in a sentence. Prefer removing code to adding it. A change to the dials goes in `challenger` in `eval/main.go`; a change to the rule goes in `internal/queue/`.
4. **Run the eval:** `go run ./eval -judge "<hypothesis>"`. It records the run with the verdict step 5 gives it, kept or rejected and why, so the note cannot say something the numbers don't. Whether the indicator you picked got better is yours to read; when it didn't, revert the code and say so in the commit. `-record "<note>"` writes a note of your own, for baselines. A rejected idea on the scoreboard is one nobody tries twice.
5. **Keep it only if** every invariant holds, every mutant is still caught, the one you picked got better, and no indicator of either shape is more than a twentieth worse than the best it has been on a kept row since the last fresh baselines. The eval compares with that best row, not the newest, so small losses don't add up. Otherwise revert the code and keep the row. A change that trades one indicator past that for another is a person's call: revert it, record it as rejected with `trade for Jeremy` in the note, and give both sides' numbers.
6. **Commit** the change and its row together. The commit's message is the hypothesis and what the numbers said.

## Rules

- **Never weaken the eval to pass it.** A broken invariant is fixed in the planner. If the simulator is wrong about the world, say how you know, fix the simulator in its own commit, and record fresh baselines: `go run ./eval -baselines -record "baseline: <what changed>"`. A note that begins with `baseline` is where the best kept row starts again.
- **A new failure becomes a case first.** When a search or a live queue finds a world that breaks an invariant, add it to the eval before fixing it, and watch it fail.
- **Add a mutant with each new invariant,** so the eval is shown to see what it claims to see.
- **Mind the size.** The engine's line count is an indicator. A mechanism that buys less than a twentieth on its indicator doesn't stay.
- **The shapes are estimates.** `go run ./eval -live <ticks> [-clone <repo>] -like <shape>` scores a real queue from the ticks it recorded (reading `<repo>` to check whether pull requests ejected at an earlier head later landed the same change), then plays the same arrivals on the shape's CI with builds as long and as flaky as the real ones, and prints the two rows together. Where they are more than a fifth apart, find out why before trusting either. When a rate in a shape is wrong, change it in its own commit and record fresh baselines.

## What the numbers mean

A pull request's ready clock starts when it is labelled and its own checks have passed, whichever is later. `wait` is from then to landed and `eject` from then to ejected as the cause, both in whole-build times. `label50`, `label90` and `slow20th` are from the first time the pull request was labelled for the queue to its landing, across new heads and re-labelling: median, p90 and the mean of the slowest twentieth, in whole-build times. `builds/pr` counts a probe as its share of a whole build. `per hour` is landings an hour while a backlog stands: 120 queued with nothing arriving, timed to the landing that leaves a quarter of them. `wrong/1k` is innocent pull requests ejected per thousand landed; in `-live` without `-clone`, landings at a newer head after an ejection are printed as unchecked.

hey2's builds run on the 13 agents that keep its compiles warm, shared with the other builds of its pipeline and with hey-native-apps' jobs. Nothing waits for one: a build that finds them busy starts on a cold agent and takes five minutes longer, which is in every number. `go run ./eval -fit` shows the share that start cold beside what the pipeline measured. Under hey2's row the eval prints how many of the other builds start cold with the queue running and with none. That is what the queue costs everyone else; it is not an indicator. hey-native-apps has no pool fitted until its warm agents are measured.
