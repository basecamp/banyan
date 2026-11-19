package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type takeout struct {
	At               time.Time `json:"at"`
	Number           int       `json:"number"`
	Head             string    `json:"head"`
	Attributed       string    `json:"attributed"`
	Class            string    `json:"class"`
	By               string    `json:"by"`
	LandedSameHead   *bool     `json:"landed_same_head"`
	HeadMoved        *bool     `json:"head_moved"`
	Merged           *bool     `json:"merged"`
	Open             *bool     `json:"open"`
	Failed           []string  `json:"failed"`
	FailedThenPassed []string  `json:"failed_then_passed"`
}

type takeoutCounts struct {
	Count            int `json:"count"`
	LandedSameHead   int `json:"landed_same_head"`
	FailedThenPassed int `json:"failed_then_passed"`
}

type takeoutHead struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
}

type takeoutReport struct {
	Repo         string                   `json:"repo"`
	From         time.Time                `json:"from"`
	To           time.Time                `json:"to"`
	EvalCommit   string                   `json:"eval_commit"`
	CommentsRead int                      `json:"comments_read"`
	Takeouts     []takeout                `json:"takeouts"`
	ByClass      map[string]takeoutCounts `json:"by_class"`
	Twice        []takeoutHead            `json:"twice"`
	Unread       int                      `json:"unread"`
	ReadsFailed  int                      `json:"reads_failed"`
	StillOut     []takeout                `json:"still_out"`
}

type takeoutComment struct {
	CreatedAt time.Time `json:"created_at"`
	Body      string    `json:"body"`
	IssueURL  string    `json:"issue_url"`
	User      struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

type takeoutCheck struct {
	Name string `json:"name"`
	App  struct {
		ID int64 `json:"id"`
	} `json:"app"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

var takeoutMarker = regexp.MustCompile(`^<!-- [^\r\n]+ eject (\S+) attributed=(\S+) -->$`)
var takeoutRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// findTakeoutMarker returns the first marker outside fenced code blocks.
func findTakeoutMarker(body string) []string {
	var fence byte
	length := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					if trimmed[0] != '`' || !strings.ContainsRune(trimmed[n:], '`') {
						fence, length = trimmed[0], n
					}
				} else if trimmed[0] == fence && n >= length && strings.TrimSpace(trimmed[n:]) == "" {
					fence, length = 0, 0
				}
				continue
			}
		}
		if fence == 0 {
			if marker := takeoutMarker.FindStringSubmatch(line); marker != nil {
				return marker
			}
		}
	}
	return nil
}

func takeoutClass(attributed string) string {
	switch attributed {
	case "culprit":
		return "blamed in a candidate"
	case "checks":
		return "own check"
	case "nogate":
		return "no gate"
	case "conflict":
		return "conflict"
	default:
		return attributed
	}
}

func ghAPI(path string) ([]byte, error) {
	bin := os.Getenv("BANYAN_GH")
	if bin == "" {
		bin = "gh"
	}
	body, err := exec.Command(bin, "api", path).Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return body, nil
}

func runTakeouts(repo, from, to string) error {
	report, err := readTakeouts(repo, from, to, ghAPI)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

// readTakeouts keeps every marked take-out in the window, with unknown outcomes left null.
func readTakeouts(repo, from, to string, read func(string) ([]byte, error)) (takeoutReport, error) {
	report := takeoutReport{Repo: repo, EvalCommit: evalRevision(), Takeouts: []takeout{}, Twice: []takeoutHead{}, StillOut: []takeout{}, ByClass: map[string]takeoutCounts{}}
	if !takeoutRepo.MatchString(repo) {
		return report, fmt.Errorf("-takeouts requires owner/repo")
	}
	if from == "" || to == "" {
		return report, fmt.Errorf("-takeouts requires -from and -to")
	}
	bounds, err := liveBounds(from, to, false)
	if err != nil {
		return report, err
	}
	report.From, report.To = *bounds.from, *bounds.to
	for _, class := range []string{"blamed in a candidate", "own check", "no gate", "conflict"} {
		report.ByClass[class] = takeoutCounts{}
	}
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/issues/comments?since=%s&sort=created&direction=asc&per_page=100&page=%d", repo, url.QueryEscape(report.From.Add(-time.Second).Format(time.RFC3339Nano)), page)
		body, err := read(path)
		if err != nil {
			return report, err
		}
		var comments []takeoutComment
		if err := json.Unmarshal(body, &comments); err != nil {
			return report, fmt.Errorf("%s: %w", path, err)
		}
		report.CommentsRead += len(comments)
		if len(comments) > 0 && !comments[0].CreatedAt.Before(report.To) {
			break
		}
		for _, comment := range comments {
			if comment.CreatedAt.Before(report.From) || !comment.CreatedAt.Before(report.To) || comment.User.Type != "Bot" {
				continue
			}
			marker := findTakeoutMarker(comment.Body)
			if marker == nil {
				continue
			}
			number, err := strconv.Atoi(comment.IssueURL[strings.LastIndex(comment.IssueURL, "/")+1:])
			if err != nil || number <= 0 {
				return report, fmt.Errorf("invalid issue_url %q", comment.IssueURL)
			}
			report.Takeouts = append(report.Takeouts, takeout{At: comment.CreatedAt, Number: number, Head: marker[1], Attributed: marker[2], Class: takeoutClass(marker[2]), By: comment.User.Login})
		}
		if len(comments) < 100 {
			break
		}
	}
	heads := map[takeoutHead]int{}
	for i := range report.Takeouts {
		out := &report.Takeouts[i]
		failed := 0
		if err := out.readPull(repo, read); err != nil {
			failed++
		}
		failed += out.readChecks(repo, read)
		if failed > 0 {
			report.Unread++
		}
		report.ReadsFailed += failed
		if out.Open != nil && *out.Open && !*out.Merged && !*out.HeadMoved {
			report.StillOut = append(report.StillOut, *out)
		}
		class := out.Class
		switch out.Attributed {
		case "culprit", "checks", "nogate", "conflict":
		default:
			class = "other"
		}
		counts := report.ByClass[class]
		counts.Count++
		if out.LandedSameHead != nil && *out.LandedSameHead {
			counts.LandedSameHead++
		}
		if len(out.FailedThenPassed) > 0 {
			counts.FailedThenPassed++
		}
		report.ByClass[class] = counts
		head := takeoutHead{out.Number, out.Head}
		heads[head]++
		if heads[head] == 2 {
			report.Twice = append(report.Twice, head)
		}
	}
	return report, nil
}

func (out *takeout) readPull(repo string, read func(string) ([]byte, error)) error {
	body, err := read(fmt.Sprintf("repos/%s/pulls/%d", repo, out.Number))
	if err != nil {
		return err
	}
	var pr struct {
		Merged bool   `json:"merged"`
		State  string `json:"state"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return err
	}
	landed, moved, open := pr.Merged && pr.Head.SHA == out.Head, pr.Head.SHA != out.Head, pr.State == "open"
	out.LandedSameHead, out.HeadMoved, out.Merged, out.Open = &landed, &moved, &pr.Merged, &open
	return nil
}

type takeoutStatus struct {
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

func readTakeoutChecks(repo, head string, read func(string) ([]byte, error)) ([]takeoutCheck, error) {
	var runs []takeoutCheck
	for page := 1; ; page++ {
		body, err := read(fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100&page=%d&filter=all", repo, head, page))
		if err != nil {
			return nil, err
		}
		var checks struct {
			Runs []takeoutCheck `json:"check_runs"`
		}
		if err := json.Unmarshal(body, &checks); err != nil {
			return nil, err
		}
		runs = append(runs, checks.Runs...)
		if len(checks.Runs) < 100 {
			return runs, nil
		}
	}
}

func readTakeoutStatuses(repo, head string, read func(string) ([]byte, error)) ([]takeoutStatus, error) {
	var statuses []takeoutStatus
	for page := 1; ; page++ {
		body, err := read(fmt.Sprintf("repos/%s/commits/%s/statuses?per_page=100&page=%d", repo, head, page))
		if err != nil {
			return nil, err
		}
		var pageStatuses []takeoutStatus
		if err := json.Unmarshal(body, &pageStatuses); err != nil {
			return nil, err
		}
		statuses = append(statuses, pageStatuses...)
		if len(pageStatuses) < 100 {
			return statuses, nil
		}
	}
}

// readChecks leaves both lists unknown if either source cannot be read.
func (out *takeout) readChecks(repo string, read func(string) ([]byte, error)) int {
	runs, checksErr := readTakeoutChecks(repo, out.Head, read)
	statuses, statusesErr := readTakeoutStatuses(repo, out.Head, read)
	failures := 0
	if checksErr != nil {
		failures++
	}
	if statusesErr != nil {
		failures++
	}
	if failures > 0 {
		return failures
	}
	failed, passed := map[string]bool{}, map[string]bool{}
	for _, run := range runs {
		if run.Status != "completed" || run.CompletedAt.IsZero() || run.CompletedAt.After(out.At) {
			continue
		}
		switch run.Conclusion {
		case "failure", "timed_out", "cancelled":
			failed[run.Name] = true
			for _, next := range runs {
				if next.App.ID == run.App.ID && next.Name == run.Name && next.Status == "completed" && next.Conclusion == "success" && !run.StartedAt.IsZero() && next.StartedAt.After(run.StartedAt) && !next.CompletedAt.IsZero() && !next.CompletedAt.Before(next.StartedAt) {
					passed[run.Name] = true
				}
			}
		}
	}
	for _, status := range statuses {
		if (status.State != "failure" && status.State != "error") || status.CreatedAt.IsZero() || status.CreatedAt.After(out.At) {
			continue
		}
		failed[status.Context] = true
		for _, next := range statuses {
			if next.Context == status.Context && next.State == "success" && next.CreatedAt.After(status.CreatedAt) {
				passed[status.Context] = true
			}
		}
	}
	out.Failed, out.FailedThenPassed = []string{}, []string{}
	for name := range failed {
		out.Failed = append(out.Failed, name)
	}
	for name := range passed {
		out.FailedThenPassed = append(out.FailedThenPassed, name)
	}
	slices.Sort(out.Failed)
	slices.Sort(out.FailedThenPassed)
	return 0
}
