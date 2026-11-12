package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/basecamp/banyan/internal/queue"
)

//go:embed cases/*.json
var caseFiles embed.FS

// A Case is a world built by hand from something that happened to a real queue, and what the
// queue has to make of it. Times are in minutes. No tick crashes, and a build flakes or runs
// long only where the case says so, so a case plays the same way every time.
type Case struct {
	Name  string `json:"-"`
	Story string `json:"story"`
	Build int    `json:"build"`
	// Agents is how many builds the CI runs at once, 0 for no limit. A deleted branch's build
	// runs to its end.
	Agents int `json:"agents"`
	PRs    []struct {
		Number  int  `json:"number"`
		At      int  `json:"at"`
		Breaks  bool `json:"breaks"`  // it passes its own checks and fails the whole build
		Clashes int  `json:"clashes"` // it fails the whole build only together with this one
	} `json:"prs"`
	// Builds is what happens to the first builds the queue begins, in the order it begins them.
	Builds []struct {
		Takes  int  `json:"takes"`  // minutes, in place of the usual
		Flakes bool `json:"flakes"` // it fails though its tree is good
	} `json:"builds"`
	// Red is when the trunk fails by the clock, whatever is built on it.
	Red []struct {
		At  int `json:"at"`
		For int `json:"for"`
	} `json:"red"`
	// Ejected is the pull requests that must leave as culprits and never reach the trunk.
	// Every other one must land, and must never be ejected for any cause.
	Ejected []int `json:"ejected"`
	// Whole is the most whole builds the queue may begin, 0 for any number.
	Whole int `json:"whole"`
}

// cases reads every case in eval/cases, in the order of their names.
func cases() []Case {
	files, err := caseFiles.ReadDir("cases")
	if err != nil {
		panic(err)
	}
	var all []Case
	for _, f := range files {
		data, _ := caseFiles.ReadFile(path.Join("cases", f.Name()))
		c := Case{Name: strings.TrimSuffix(f.Name(), ".json")}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			panic(fmt.Sprintf("eval/cases/%s: %v", f.Name(), err))
		}
		all = append(all, c)
	}
	return all
}

func (c Case) shape() Shape {
	minutes := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	s := Shape{Name: c.Name, Build: minutes(c.Build), Probe: minutes(c.Build) / 3, Agents: c.Agents}
	for _, pr := range c.PRs {
		s.Queue = append(s.Queue, Queued{Number: pr.Number, At: minutes(pr.At), Breaks: pr.Breaks, Clash: pr.Clashes})
		s.Hours = max(s.Hours, minutes(pr.At).Hours())
	}
	for _, b := range c.Builds {
		s.Script, s.Takes = append(s.Script, b.Flakes), append(s.Takes, minutes(b.Takes))
	}
	for _, r := range c.Red {
		s.Episodes = append(s.Episodes, Episode{At: minutes(r.At), For: minutes(r.For)})
	}
	return s
}

// play runs the case under one configuration and says what the queue got wrong.
func (c Case) play(m mutant, cfg queue.Config) []string {
	s := NewSim(c.shape(), cfg, 1)
	s.plan, s.verify = m.plan, m.verify
	s.Run()
	wrong := slices.Clone(s.Broken)
	if c.Whole > 0 && s.Whole > c.Whole {
		wrong = append(wrong, fmt.Sprintf("it began %d whole builds, where %d are enough", s.Whole, c.Whole))
	}
	for _, pr := range c.PRs {
		n, left := pr.Number, s.Left[pr.Number]
		switch culprit := slices.Contains(c.Ejected, n); {
		case len(s.Broken) > 0:
		case culprit && s.holds[s.trunk][n]:
			wrong = append(wrong, fmt.Sprintf("the trunk holds #%d", n))
		case culprit && left != "culprit":
			wrong = append(wrong, fmt.Sprintf("#%d was not ejected as the culprit", n))
		case !culprit && left != "":
			wrong = append(wrong, fmt.Sprintf("#%d was ejected, as %s", n, left))
		case !culprit && !s.prs[n].landed:
			wrong = append(wrong, fmt.Sprintf("#%d did not land", n))
		}
	}
	return wrong
}

// history plays every case under the configuration each shape ships with and under each kind
// the invariants are held in. Each failure starts with its case's name.
func history(m mutant) []string {
	var failed []string
	for _, c := range cases() {
		for _, cfg := range append([]queue.Config{challenger["hey2"], challenger["hey-native-apps"]}, kinds...) {
			for _, w := range c.play(m, cfg) {
				failed = append(failed, fmt.Sprintf("%s, %d slots of %d, probes %v: %s", c.Name, cfg.Slots, cfg.MaxBatch, cfg.Probes, w))
			}
		}
	}
	return failed
}
