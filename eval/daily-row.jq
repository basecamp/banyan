def n1: if . == null then "not measured" else (. * 10 | round / 10 | tostring) end;
def n2: if . == null then "not measured" else (. * 100 | round / 100 | tostring) end;
def pct: if . == null then "not measured" else ((. * 100 | round) as $p | (if $p > 0 then "+" else "" end) + ($p | tostring) + "%") end;
def hm: if . == null then "?" else .[11:16] + "Z" end;
def commit: if . == null then "unknown" elif endswith("+") then .[0:12] + "+ (not a commit)" else .[0:12] end;
def classes: ["blamed in a candidate", "own check", "no gate", "conflict"];
def over($limit; $what): if . == null then "\($what) was not measured" elif . > $limit then "\($what) is \(. | n2), over \($limit)" else empty end;
.repo as $repo | .live as $l | .takeouts as $t
| ($t.by_class // {}) as $c
| ($t.takeouts // []) as $outs
| ([$outs[] | select(.class as $k | classes | index($k) | not)] | group_by(.attributed) | map({name: .[0].attributed, count: length})) as $others
| ([$outs[] | select(.failed_then_passed == null)] | length) as $checks_unread
| ([$outs[] | select(.landed_same_head == null)] | length) as $pulls_unread
| ([$outs[] | select((.failed_then_passed // []) | length > 0)]) as $later
| ([$outs[] | select(.landed_same_head == true)] | length) as $same
| (($l.unchecked // 0) + ($l.unreadable // 0)) as $unjudged
| ([ ($l.ready_gaps_over_90 | if . == null then "waits of a ready head were not measured" elif . > 0 then "\(.) wait(s) over 90 minutes of a ready head with no landing" else empty end),
     (if $repo == "basecamp/hey-native-apps" and ($l.landed // 0) > 0 then ($l.queue_added_p50_minutes | over(6; "minutes the queue adds at the median")) else empty end),
     (if ($l.landed // 0) > 0 then ($l.builds_per_landing | over(if $repo == "basecamp/hey2" then 1.3 else 0.60 end; "builds per landing")) else empty end),
     ($l.passing_candidates_thrown_away | if . == null then "passing candidates thrown away were not measured" elif . > 3 then "\(.) passing candidates thrown away unlanded, over 3" else empty end),
     ($l.wrong_ejections | if . == null then "wrong ejections were not measured" elif . > 0 then "\(.) wrong ejection(s)" else empty end),
     (if $unjudged > 0 then "\($unjudged) landing(s) after an ejection could not be checked for a wrong one" else empty end),
     (if ($t.twice // [] | length) > 0 then "\($t.twice | length) head(s) taken out twice" else empty end) ]) as $trips
| (if ($l.landed // 0) == 0 then "**\(.repo | split("/")[1]), \(.day)**: no landings in the window."
   else "**\(.repo | split("/")[1]), \(.day)**: \($l.landed) landings. Ready to landed took \($l.wait_p50_minutes | n1) minutes at the median and \($l.wait_p90_minutes | n1) at p90. The queue added \($l.queue_added_p50_minutes | n1) at the median and \($l.queue_added_p90_minutes | n1) at p90." end),
  "",
  "| | |",
  "|---|---|",
  "| Ready to landed, median / p90 | \($l.wait_p50_minutes | n1) / \($l.wait_p90_minutes | n1) min (\($l.wait_p50 | n2) / \($l.wait_p90 | n2) builds of \($l.build_median_minutes | n1) min) |",
  "| Minutes the queue adds, median / p90 / p95 | \($l.queue_added_p50_minutes | n1) / \($l.queue_added_p90_minutes | n1) / \($l.queue_added_p95_minutes | n1) |",
  "| Landings held over 30 minutes | \($l.queue_added_over_30 | n1) of \($l.split_landings) (\($l.unsplit_landings) more could not be split) |",
  "| Builds per landing | \($l.builds_per_landing | n2) |",
  "| Longest wait of a ready head with no landing | \(if $l.ready_gaps == null then "not measured" elif $l.longest_ready_gap_minutes == null then "none" else "\($l.longest_ready_gap_minutes | n1) min, \($l.longest_ready_gap_from | hm) to \($l.longest_ready_gap_to | hm)" end) |",
  "| Red candidates | \(if $l.red_candidates == null then "not measured" elif $l.red_candidates == 0 then "none" else "\($l.red_candidates), holding \($l.red_pull_requests | n1) pull requests; " + (if ($l.red_read_n // 0) > 0 then "failed job seen to read red \($l.red_read_median_minutes | n1) min at the median over \($l.red_read_n)" else "no failed job's time was recorded" end) end) |",
  "| Trunk moves no recorded tick planned | \($l.unplanned_trunk_moves | n1)\(if ($l.unplanned_trunk_moves // 0) > 0 then " (a merge outside the queue, or a tick whose record is missing; its landings are not counted)" else "" end) |",
  "| Passing candidates thrown away unlanded | \(if $l.passing_candidates_thrown_away == null then "not measured" elif $l.passing_candidates_thrown_away == 0 then "none" else "\($l.passing_candidates_thrown_away): \($l.thrown_away_head_moved) with a member's head moved, \($l.thrown_away_member_left) with a member gone from the queue" end) |",
  (if ($l.unconfirmed_landing_plans // 0) > 0 then "| Landing plans in the window the records never confirm | \($l.unconfirmed_landing_plans) (a refused push, or a landing confirmed after the records end) |" else empty end),
  "| Wrong ejections by the engine | \($l.wrong_ejections | n1)\(if $unjudged > 0 then ", with \($unjudged) landing(s) not checked" else "" end) |",
  "| Take-outs | \($outs | length): \([classes[] as $k | "\($c[$k].count // 0) \($k)"] + [$others[] | "\(.count) \(.name)"] | join(", ")) |",
  "| Of those, a failed check passed later on the same commit | \(if $checks_unread > 0 then "at least " else "" end)\($later | length)\(if ($later | length) > 0 then " (" + ($later | group_by(.class) | map("\(length) \(.[0].class)") | join(", ")) + ")" else "" end)\(if $checks_unread > 0 then "; \($checks_unread) not read" else "" end) |",
  "| Of those, the same head has landed since | \(if $pulls_unread > 0 then "at least " else "" end)\($same)\(if $pulls_unread > 0 then "; \($pulls_unread) not read" else "" end) |",
  (if $l.simulated then "| Simulator over live: median, p90, builds per landing, landings | \($l.over_live.wait_p50 | pct), \($l.over_live.wait_p90 | pct), \($l.over_live.builds_per_landing | pct), \($l.over_live.landed | pct) |" else empty end),
  "",
  (if ($trips | length) > 0 then "**Tripwires:** " + ($trips | join("; ")) + "." else "No tripwire breached." end),
  "",
  "The window is 01:17Z to 21:17Z, read at \(.made | hm) from \($l.ticks) tick records by the engine's eval at `\($l.commit | commit)`, and \($t.comments_read) comments."
