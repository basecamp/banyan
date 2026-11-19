def n1: if . == null then "not read" else (. * 10 | round / 10 | tostring) end;
def n2: if . == null then "not read" else (. * 100 | round / 100 | tostring) end;
def pct: if . == null then "not read" else ((. * 100 | round) as $p | (if $p > 0 then "+" else "" end) + ($p | tostring) + "%") end;
def hm: if . == null then "?" else .[11:16] + "Z" end;
def classes: ["blamed in a candidate", "own check", "no gate", "conflict"];
.live as $l | .takeouts as $t
| ($t.by_class // {}) as $c
| ([$t.takeouts[]? | select(.class as $k | classes | index($k) | not)] | group_by(.attributed) | map({name: .[0].attributed, count: length, later: ([.[] | select((.failed_then_passed // []) | length > 0)] | length)})) as $others
| ([$t.takeouts[]? | select((.failed_then_passed // []) | length > 0)] | length) as $later
| ([$t.takeouts[]? | select(.landed_same_head == true)] | length) as $same
| [ (if ($l.ready_gaps_over_90 // 0) > 0 then "\($l.ready_gaps_over_90) gap(s) over 90 minutes between landings with a ready head waiting" else empty end),
    (if .repo == "basecamp/hey-native-apps" and ($l.queue_added_p50_minutes // 0) > 6 then "the queue added \($l.queue_added_p50_minutes | n1) minutes at the median, over 6" else empty end),
    (if .repo == "basecamp/hey-native-apps" and ($l.builds_per_landing // 0) > 0.60 then "builds per landing \($l.builds_per_landing | n2), over 0.60" else empty end),
    (if .repo == "basecamp/hey2" and ($l.builds_per_landing // 0) > 1.3 then "builds per landing \($l.builds_per_landing | n2), over 1.3" else empty end),
    (if ($l.wrong_ejections // 0) > 0 then "\($l.wrong_ejections) wrong ejection(s)" else empty end),
    (if ($t.twice // [] | length) > 0 then "\($t.twice | length) head(s) taken out twice" else empty end) ] as $trips
| "**\(.repo | split("/")[1]), \(.day)**: \($l.landed) landings. Ready to landed took \($l.wait_p50_minutes | n1) minutes at the median and \($l.wait_p90_minutes | n1) at p90. The queue added \($l.queue_added_p50_minutes | n1) and \($l.queue_added_p90_minutes | n1) of those.",
  "",
  "| | |",
  "|---|---|",
  "| Ready to landed, median / p90 | \($l.wait_p50_minutes | n1) / \($l.wait_p90_minutes | n1) min (\($l.wait_p50 | n2) / \($l.wait_p90 | n2) builds of \($l.build_median_minutes | n1) min) |",
  "| Minutes the queue adds, median / p90 / p95 | \($l.queue_added_p50_minutes | n1) / \($l.queue_added_p90_minutes | n1) / \($l.queue_added_p95_minutes | n1) |",
  "| Landings held over 30 minutes | \($l.queue_added_over_30) of \($l.split_landings) (\($l.unsplit_landings) more could not be split) |",
  "| Builds per landing | \($l.builds_per_landing | n2) |",
  "| Longest gap between landings with a ready head waiting | \($l.longest_ready_gap_minutes | n1) min, \($l.longest_ready_gap_from | hm) to \($l.longest_ready_gap_to | hm) |",
  "| Red candidates | \($l.red_candidates), holding \($l.red_pull_requests) pull requests; failed job seen to read red \($l.red_read_median_minutes | n1) min at the median |",
  "| Wrong ejections by the engine | \($l.wrong_ejections) |",
  "| Take-outs | \($t.takeouts | length): \([classes[] as $k | "\($c[$k].count // 0) \($k)"] + [$others[] | "\(.count) \(.name)"] | join(", ")) |",
  "| Of those, a failed check passed later on the same commit | \($later) \([classes[] as $k | select(($c[$k].failed_then_passed // 0) > 0) | "\($c[$k].failed_then_passed) \($k)"] + [$others[] | select(.later > 0) | "\(.later) \(.name)"] | if length > 0 then "(" + join(", ") + ")" else "" end) |",
  "| Of those, the same head has landed since | \($same) |",
  (if ($t.unread // 0) > 0 then "| Take-outs not read | \($t.unread) |" else empty end),
  (if $l.simulated then "| Simulator over live: median, p90, builds per landing, landings | \($l.over_live.wait_p50 | pct), \($l.over_live.wait_p90 | pct), \($l.over_live.builds_per_landing | pct), \($l.over_live.landed | pct) |" else empty end),
  "",
  (if ($trips | length) > 0 then "**Tripwires:** " + ($trips | join("; ")) + "." else "No tripwire breached." end),
  "",
  "The window is 01:17Z to 21:17Z, read from \($l.ticks) tick records by the engine's eval at `\($l.commit[0:12])`, and \($t.comments_read) comments."
