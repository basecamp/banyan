---
name: hillclimb
description: One iteration of Banyan's improvement loop. Use when asked to improve the merge queue, close a gap to a charter target, work an escape (a live stall or an ejection later overturned), or run the loop on a schedule.
---

# Hillclimb

One iteration changes one thing and leaves one row on the scoreboard. Run it by hand, or again and again.

Read `CHARTER.md` first. It holds the invariants and the targets. Do not change it: that is a person's call.

## One iteration

1. **Pick the gap.** Run `go run ./eval -baselines` and read the challenger rows against the charter's targets. Take the newest escape in `eval/cases/` if one is unfixed. Otherwise take the indicator furthest from its target, as a ratio.
2. **Look before guessing.** Replay a world where the gap shows: `go run ./eval -trace "<shape> <slots> <batch> <probes> <seed>"`. Read what the queue did and say in one sentence why the indicator is where it is.
3. **State one hypothesis and its change.** One mechanism or one dial, small enough to explain in a sentence. Prefer removing code to adding it. A change to the dials goes in `challenger` in `eval/main.go`; a change to the rule goes in `internal/queue/`.
4. **Run the eval:** `go run ./eval -record "<hypothesis>: <kept or rejected, and why>"`. Record it either way. A rejected idea on the scoreboard is one nobody tries twice.
5. **Keep it only if** every invariant holds, every mutant is still caught, the one you picked got better, and no indicator of either shape is more than a twentieth worse than the best it has been on a kept row since the last fresh baselines. The eval compares with that best row, not the newest, so small losses don't add up. Otherwise revert the code and keep the row. A change that trades one indicator past that for another is a person's call: revert it, record it as rejected with `trade for Jeremy` in the note, and give both sides' numbers.
6. **Commit** the change and its row together. The commit's message is the hypothesis and what the numbers said.

## Rules

- **Never weaken the eval to pass it.** A broken invariant is fixed in the planner. If the simulator is wrong about the world, say how you know, fix the simulator in its own commit, and record fresh baselines: `go run ./eval -baselines -record "baseline: <what changed>"`. A note that begins with `baseline` is where the best kept row starts again.
- **A new failure becomes a case first.** When a search or a live queue finds a world that breaks an invariant, add it to the eval before fixing it, and watch it fail.
- **Add a mutant with each new invariant,** so the eval is shown to see what it claims to see.
- **Mind the size.** The engine's line count is an indicator. A mechanism that buys less than a twentieth on its indicator doesn't stay.
- **The shapes are estimates** until `banyan report` measures the real rates. When it does, change the shape in its own commit and record fresh baselines.

## What the numbers mean

`wait` is queue to landed and `blame` is first red to ejection, both in whole-build times. `builds/pr` counts a probe as its share of a whole build. `per hour` is landings an hour on a backlog of forty with nothing arriving. `wrong/1k` is innocent pull requests ejected per thousand landed.

hey2's builds share a pool of agents with the other builds of its pipeline, first come first served, so a queue build's wait for an agent is in every number. `go run ./eval -fit` shows the pool beside what the pipeline measured. Under hey2's row the eval prints how much longer the other builds wait for an agent with the queue running. That is what the queue costs everyone else; it is not an indicator. hey-native-apps has no limit until its CI is measured.
