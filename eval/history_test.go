package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

// A recorded hour: #1 and #2 are labelled with their checks passed, #2 comes to conflict when
// #1 lands and a bot mends it seven minutes on, #3 breaks the build until its author pushes a
// fix on being ejected, and #4 is labelled before its own checks have passed.
const recordedHour = `{
	"window": {"from": "2026-11-04T00:00:00Z", "to": "2026-11-04T06:00:00Z"},
	"prs": [
		{"number": 1, "labels": [{"at": 60, "label": "land", "on": true, "by": "person"}], "heads": [{"at": 0, "checks_done": 30, "checks": "success"}], "merged": 2000},
		{"number": 2, "labels": [{"at": 120, "label": "land", "on": true, "by": "person"}], "heads": [{"at": 0, "checks_done": 30, "checks": "success"}, {"at": 2500, "checks_done": 3400, "checks": "success"}],
			"conflicts": [{"at": 2100, "kind": "repair-ask", "onto": "t1", "mended": 2520, "mended_by": "bot"}], "merged": 6000},
		{"number": 3, "labels": [{"at": 180, "label": "land", "on": true, "by": "person"}, {"at": 5000, "label": "land", "on": false, "by": "queue"}, {"at": 5700, "label": "land", "on": true, "by": "person"}],
			"heads": [{"at": 0, "checks_done": 30, "checks": "success"}, {"at": 5600, "checks_done": 6500, "checks": "success"}],
			"ejections": [{"at": 5000, "cause": "culprit", "attributed": "bisection"}], "breaks": true, "merged": 9000},
		{"number": 4, "labels": [{"at": 240, "label": "land", "on": true, "by": "person"}], "heads": [{"at": 200, "checks_done": 1200, "checks": "success"}], "merged": 4000}
	],
	"trunk": [{"sha": "t1", "prs": [1]}],
	"candidates": [{"attempts": [{"started": 0, "ended": 1500, "conclusion": "success"}, {"started": 0, "ended": 1200, "conclusion": "failure"}]}]
}`

func TestARecordedWindowIsPlayedToThePlanner(t *testing.T) {
	var tr trace
	if err := json.Unmarshal([]byte(recordedHour), &tr); err != nil {
		t.Fatal(err)
	}
	if o := tr.recorded(); o.landed != 4 || len(o.clean) != 2 || o.ejected != 1 || o.builds != 2 {
		t.Errorf("the record reads as %d landed, %d on the clean path, %d ejected and %d builds, where it holds 4, 2, 1 and 2", o.landed, len(o.clean), o.ejected, o.builds)
	}
	passed, fails := tr.times()
	shape := Shape{Name: "history", Build: passed[0], Times: passed, Whole: passed[0], Fails: fails, Acts: tr.acts(true), Cancels: true}
	s := NewSim(shape, dials(1, 1, false), 0)
	admitted := map[int]time.Duration{}
	for s.now < 6*time.Hour && len(s.Broken) == 0 {
		s.Step()
		for n, p := range s.prs {
			if _, ok := admitted[n]; !ok && p.queued && p.Checks == queue.Success {
				admitted[n] = s.now
			}
		}
	}
	if len(s.Broken) > 0 {
		t.Fatal(s.Broken)
	}
	for n := 1; n <= 4; n++ {
		if !s.prs[n].landed {
			t.Errorf("#%d did not land", n)
		}
	}
	if admitted[4] < 1200*time.Second {
		t.Errorf("#4 was ready at %s, before its own checks passed at 20m", admitted[4])
	}
	if two := s.prs[2]; !s.history.waited[2] || two.Head != "head2.2" || two.at < s.prs[1].at+7*time.Minute {
		t.Errorf("#2 landed at %s on %s, where it conflicts once #1 has landed at %s and lands on the head pushed seven minutes on", two.at, two.Head, s.prs[1].at)
	}
	if three := s.prs[3]; s.Left[3] != "culprit" || s.Wrong != 0 || three.Head != "head3.2" {
		t.Errorf("#3 left as %q with %d wrong ejections and landed on %s, where it is ejected as the cause and lands on its fix", s.Left[3], s.Wrong, three.Head)
	}
	if first := fmt.Sprint(s.history.labelled); first != "map[1:1m0s 2:2m0s 3:3m0s 4:4m0s]" {
		t.Errorf("first labels read as %s", first)
	}
}

// With no repair, the conflict the record's bot mended in seven minutes is mended as late as
// a person would have.
func TestAConflictIsMendedByItsAuthorWhereNothingRepairs(t *testing.T) {
	var tr trace
	if err := json.Unmarshal([]byte(recordedHour), &tr); err != nil {
		t.Fatal(err)
	}
	pushed := func(repair bool) (at time.Duration) {
		for _, a := range tr.acts(repair) {
			if a.Number == 2 && a.Do == "head" && a.Head == "head2.2" {
				at = a.At
			}
		}
		return at
	}
	if with, without := pushed(true), pushed(false); with != 2500*time.Second || without != with+authorMends-7*time.Minute {
		t.Errorf("the mending head is pushed at %s with repair and %s without, where a person takes %s from the conflict", with, without, authorMends)
	}
}

func TestTheRecordCountsTheWindowsEjectionsAlone(t *testing.T) {
	var tr trace
	carried := `{"window": {"from": "2026-11-04T00:00:00Z", "to": "2026-11-04T01:00:00Z"}, "prs": [
		{"number": 1, "labels": [{"at": -7200, "label": "land", "on": true, "by": "person"}],
			"ejections": [{"at": -3600, "cause": "conflict"}, {"at": 600, "cause": "conflict"}, {"at": 4000, "cause": "conflict"}]}]}`
	if err := json.Unmarshal([]byte(carried), &tr); err != nil {
		t.Fatal(err)
	}
	if o := tr.recorded(); o.ejected != 1 {
		t.Errorf("a pull request ejected before, in and after the window reads as %d ejections, where the window holds 1", o.ejected)
	}
}

func TestAnEjectionSoonerThanTheRecordsIsTimedWhereItFellAndItsAuthorActsSooner(t *testing.T) {
	var tr trace
	late := `{"window": {"from": "2026-11-04T00:00:00Z", "to": "2026-11-04T08:00:00Z"}, "prs": [
		{"number": 1, "labels": [{"at": 60, "label": "land", "on": true, "by": "person"}, {"at": 18000, "label": "land", "on": false, "by": "queue"}, {"at": 18700, "label": "land", "on": true, "by": "person"}],
			"heads": [{"at": 0, "checks_done": 30, "checks": "success"}, {"at": 18600, "checks_done": 18660, "checks": "success"}],
			"ejections": [{"at": 18000, "cause": "culprit", "attributed": "bisection"}], "breaks": true, "merged": 21000}],
		"candidates": [{"attempts": [{"started": 0, "ended": 1500, "conclusion": "success"}, {"started": 0, "ended": 1200, "conclusion": "failure"}]}]}`
	if err := json.Unmarshal([]byte(late), &tr); err != nil {
		t.Fatal(err)
	}
	passed, fails := tr.times()
	s := NewSim(Shape{Name: "history", Build: passed[0], Times: passed, Whole: passed[0], Fails: fails, Acts: tr.acts(true), Cancels: true}, dials(2, 1, false), 0)
	for s.now < 8*time.Hour && len(s.Broken) == 0 {
		s.Step()
	}
	if len(s.Broken) > 0 {
		t.Fatal(s.Broken)
	}
	recorded := 18000 * time.Second
	if at, ok := s.history.blamed[1]; !ok || at >= recorded {
		t.Errorf("#1 is timed as ejected at %s, where this queue ejected it before the record's did at %s", at, recorded)
	}
	if one := s.prs[1]; !one.landed || one.Head != "head1.2" || one.at >= recorded {
		t.Errorf("#1 landed %v at %s on %s, where its author pushes the fix ten minutes after this queue's ejection and it lands before the record's ejection", one.landed, one.at, one.Head)
	}
}
