def n1: if . == null then "not measured" else (. * 10 | round / 10 | tostring) end;
def n2: if . == null then "not measured" else (. * 100 | round / 100 | tostring) end;
def pct: if . == null then "not measured" else ((. * 100 | round) as $p | (if $p > 0 then "+" else "" end) + ($p | tostring) + "%") end;
def hm: if . == null then "?" else .[11:16] + "Z" end;
def commit: if . == null then "unknown" elif endswith("+") then .[0:12] + "+ (not a commit)" else .[0:12] end;
def classes: ["blamed in a candidate", "own check", "no gate", "conflict"];
def over($limit; $what): if . == null then "\($what) was not measured" elif . > $limit then "\($what) is \(. | n2), over \($limit)" else empty end;
def summary($title; $none):
  if (.landed // 0) == 0 then "**\($title)**: \($none)."
  else "**\($title)**: \(.landed) landings. Ready to landed took \(.wait_p50_minutes | n1) minutes at the median and \(.wait_p90_minutes | n1) at p90. The queue added \(.queue_added_p50_minutes | n1) at the median and \(.queue_added_p90_minutes | n1) at p90." end;
def ready_row: "| Ready to landed, median / p90 | \(.wait_p50_minutes | n1) / \(.wait_p90_minutes | n1) min (\(.wait_p50 | n2) / \(.wait_p90 | n2) builds of \(.build_median_minutes | n1) min) |";
def held_row: "| Landings held over 30 minutes | \(.queue_added_over_30 | n1) of \(.split_landings) (\(.unsplit_landings) more could not be split) |";
def first_row:
  if .first_candidate_p90_minutes == null or .first_candidate_over_30 == null then empty
  else "| Ready to a first candidate, p90 | \(.first_candidate_p90_minutes | n1) min; \(.first_candidate_over_30) landing\(if .first_candidate_over_30 == 1 then "" else "s" end) waited over 30 min for one |" end;
def first_held_row:
  [.held_first_waited, .held_first_failed, .held_first_dropped, .held_first_overtaken, .held_first_other] as $held
  | if (.queue_added_over_30 // 0) == 0 or ($held | any(. == null)) then empty
    else "| The \(.queue_added_over_30) held over 30 min | \([$held, ["waited over 30 min for any candidate", "after their first candidate failed", "after it was dropped while building", "landed by a later candidate while the first still built", "other"]] | transpose | map(select(.[0] > 0) | "\(.[0]) \(.[1])") | join(", ")) |" end;
def builds_row: "| Builds per landing | \(.builds_per_landing | n2) |";
def gap_row: "| Longest wait of a ready head with no landing | \(if .ready_gaps == null then "not measured" elif .longest_ready_gap_minutes == null then "none" else "\(.longest_ready_gap_minutes | n1) min, \(.longest_ready_gap_from | hm) to \(.longest_ready_gap_to | hm)" end) |";
def red_row: "| Red candidates | \(if .red_candidates == null then "not measured" elif .red_candidates == 0 then "none" else "\(.red_candidates), holding \(.red_pull_requests | n1) pull requests; " + (if (.red_read_n // 0) > 0 then "failed job seen to read red \(.red_read_median_minutes | n1) min at the median over \(.red_read_n)" else "no failed job's time was recorded" end) end) |";
def busiest_target($repo): if $repo == "basecamp/hey-native-apps" then 16 elif $repo == "basecamp/hey2" then 12 else null end;
def backlog_judged: (.backlog_longest_minutes // 0) >= 60;
def busiest_row($target; $day):
  "| Landings in the busiest hour | \(if .busiest_hour_landings == null or .backlog_longest_minutes == null then "not measured"
    elif ($day | not) then "\(.busiest_hour_landings), not judged outside the day's window (the longest backlog stood \(.backlog_longest_minutes | floor) min)"
    elif backlog_judged then (if $target == null then "\(.busiest_hour_landings), with no target for this queue" else "\(.busiest_hour_landings) against a target of \($target), judged: a backlog stood for \(.backlog_longest_minutes | floor) min that day" end)
    else "\(.busiest_hour_landings), not judged: no backlog stood for an hour (the longest stood \(.backlog_longest_minutes | floor) min)" end) |";
def thrown_row: "| Passing candidates thrown away unlanded | \(if .passing_candidates_thrown_away == null then "not measured" elif .passing_candidates_thrown_away == 0 then "none" else "\(.passing_candidates_thrown_away): \(.thrown_away_head_moved) with a member's head moved, \(.thrown_away_member_left) with a member gone from the queue" end) |";
def dropped_row: "| Candidates dropped while building | \(if .building_candidates_dropped == null then "not measured" elif .building_candidates_dropped == 0 then "none"
    elif [.dropped_head_moved, .dropped_member_left, .dropped_members_still_queued, .dropped_longest_built_minutes] | any(. == null) then "\(.building_candidates_dropped); details not measured"
    else "\(.building_candidates_dropped): \(.dropped_head_moved) with a member's head moved, \(.dropped_member_left) with a member gone from the queue; \(.dropped_members_still_queued) of their pull requests stayed queued at the same head\(if .dropped_longest_built_minutes > 0 then ", the longest \(.dropped_longest_built_minutes | floor) min into its build" else "" end)" end) |";
def takeouts_row:
  (.by_class // {}) as $c | (.takeouts // []) as $outs
  | ([$outs[] | select(.class as $k | classes | index($k) | not)] | group_by(.attributed) | map({name: .[0].attributed, count: length})) as $others
  | "| Take-outs | \($outs | length): \([classes[] as $k | "\($c[$k].count // 0) \($k)"] + [$others[] | "\(.count) \(.name)"] | join(", ")) |";
def later_row:
  ([.takeouts // [] | .[] | select(.failed_then_passed == null)] | length) as $unread
  | ([.takeouts // [] | .[] | select((.failed_then_passed // []) | length > 0)]) as $later
  | "| Of those, a failed check passed later on the same commit | \(if $unread > 0 then "at least " else "" end)\($later | length)\(if ($later | length) > 0 then " (" + ($later | group_by(.class) | map("\(length) \(.[0].class)") | join(", ")) + ")" else "" end)\(if $unread > 0 then "; \($unread) not read" else "" end) |";
.repo as $repo | .live as $l | .takeouts as $t
| ($t.takeouts // []) as $outs
| ([$outs[] | select(.landed_same_head == null)] | length) as $pulls_unread
| ([$outs[] | select(.landed_same_head == true)] | length) as $same
| (($l.unchecked // 0) + ($l.unreadable // 0)) as $unjudged
| busiest_target($repo) as $target
| ([ ($l.ready_gaps_over_90 | if . == null then "waits of a ready head were not measured" elif . > 0 then "\(.) wait(s) over 90 minutes of a ready head with no landing" else empty end),
     (if $repo == "basecamp/hey-native-apps" and ($l.landed // 0) > 0 then ($l.queue_added_p50_minutes | over(6; "minutes the queue adds at the median")) else empty end),
     (if $target != null and $l.busiest_hour_landings != null and ($l | backlog_judged) and $l.busiest_hour_landings < $target then "\($l.busiest_hour_landings) landings in the busiest hour of a day a backlog stood an hour, under \($target)" else empty end),
     (if $repo == "basecamp/hey-native-apps" then ($l.passing_candidates_thrown_away | if . == null then "passing candidates thrown away were not measured" elif . > 3 then "\(.) passing candidates thrown away unlanded, over 3" else empty end) else empty end),
     ($l.wrong_ejections | if . == null then "wrong ejections were not measured" elif . > 0 then "\(.) wrong ejection(s)" else empty end),
     (if $unjudged > 0 then "\($unjudged) landing(s) after an ejection could not be checked for a wrong one" else empty end),
     (if ($t.twice // [] | length) > 0 then "\($t.twice | length) head(s) taken out twice" else empty end) ]) as $trips
| "\($repo | split("/")[1]), \(.day)" as $title
| ($l | summary($title; "no landings in the window")),
  "",
  "| | |",
  "|---|---|",
  ($l | ready_row),
  "| Minutes the queue adds, median / p90 / p95 | \($l.queue_added_p50_minutes | n1) / \($l.queue_added_p90_minutes | n1) / \($l.queue_added_p95_minutes | n1) |",
  ($l | held_row, first_row, first_held_row, builds_row, busiest_row($target; true), gap_row, red_row),
  "| Trunk moves no recorded tick planned | \($l.unplanned_trunk_moves | n1)\(if ($l.unplanned_trunk_moves // 0) > 0 then " (a merge outside the queue, or a tick whose record is missing; its landings are not counted)" else "" end) |",
  ($l | thrown_row, dropped_row),
  (if ($l.unconfirmed_landing_plans // 0) > 0 then "| Landing plans in the window the records never confirm | \($l.unconfirmed_landing_plans) (a refused push, or a landing confirmed after the records end) |" else empty end),
  "| Wrong ejections by the engine | \($l.wrong_ejections | n1)\(if $unjudged > 0 then ", with \($unjudged) landing(s) not checked" else "" end) |",
  ($t | takeouts_row, later_row),
  "| Of those, the same head has landed since | \(if $pulls_unread > 0 then "at least " else "" end)\($same)\(if $pulls_unread > 0 then "; \($pulls_unread) not read" else "" end) |",
  (if $l.simulated then "| Simulator over live: median, p90, builds per landing, landings | \($l.over_live.wait_p50 | pct), \($l.over_live.wait_p90 | pct), \($l.over_live.builds_per_landing | pct), \($l.over_live.landed | pct) |" else empty end),
  "",
  (if ($trips | length) > 0 then "**Tripwires:** " + ($trips | join("; ")) + "." else "No tripwire breached." end),
  "",
  "The window is 01:17Z to 21:17Z, read at \(.made | hm) from \($l.ticks) tick records by the engine's eval at `\($l.commit | commit)`, and \($t.comments_read) comments.",
  (.before // empty
   | "The four hours before the window, \(.from | hm) on \(.from[0:10]) to \(.to | hm) on \(.to[0:10])" as $title
   | "",
     (if .live == null or .live.ticks == 0 then "**\($title)**: no records cover them."
      elif (.live.landed // 0) == 0 then .live | summary($title; "no landings"), (if (.passing_candidates_thrown_away // 0) > 0 or (.building_candidates_dropped // 0) > 0 then "", "| | |", "|---|---|", thrown_row, dropped_row else empty end)
      else (.live | summary($title; "no landings"), "", "| | |", "|---|---|", ready_row, held_row, builds_row, busiest_row(null; false), gap_row, red_row, thrown_row, dropped_row), (.takeouts | takeouts_row, later_row) end))
