package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IssueWeek is a GitHub snapshot for one repository and ISO week. A file with
// zero counts still establishes that the week was observed.
type IssueWeek struct {
	Week        string         `json:"week"`
	Opened      map[string]int `json:"opened"`
	Closed      int            `json:"closed"`
	CoveredFrom time.Time      `json:"covered_from"`
	CoveredTo   time.Time      `json:"covered_to"`
	Events      []IssueEvent   `json:"events"`
}

// IssueEvent preserves event time so partial summary windows stay exact.
// Events contain no issue number or login.
type IssueEvent struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Kind   string    `json:"kind,omitempty"`
}

func IssueWeekPath(root, repo, week string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
		owner == "." || owner == ".." || name == "." || name == ".." ||
		strings.ContainsAny(owner+name, `\/`) {
		return "", fmt.Errorf("invalid repository %q", repo)
	}
	if len(week) != 8 || week[4:6] != "-W" {
		return "", fmt.Errorf("invalid ISO week %q", week)
	}
	for _, c := range week[:4] + week[6:] {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid ISO week %q", week)
		}
	}
	year, _ := strconv.Atoi(week[:4])
	number, _ := strconv.Atoi(week[6:])
	jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
	start := jan4.AddDate(0, 0, -(int(jan4.Weekday())+6)%7+(number-1)*7)
	if y, w := start.ISOWeek(); y != year || w != number {
		return "", fmt.Errorf("invalid ISO week %q", week)
	}
	return filepath.Join(root, owner, name, "issues", week+".json"), nil
}

func WriteIssueWeek(root, repo string, week IssueWeek) error {
	path, err := IssueWeekPath(root, repo, week.Week)
	if err != nil {
		return err
	}
	data, err := json.Marshal(week)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".issue-week-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func ReadIssueWeek(root, repo, week string) (IssueWeek, error) {
	path, err := IssueWeekPath(root, repo, week)
	if err != nil {
		return IssueWeek{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return IssueWeek{}, err
	}
	var out IssueWeek
	if err := json.Unmarshal(data, &out); err != nil {
		return IssueWeek{}, err
	}
	if out.Week != week || out.Opened == nil || out.Closed < 0 || !out.CoveredTo.After(out.CoveredFrom) {
		return IssueWeek{}, fmt.Errorf("invalid issue week %q", path)
	}
	for _, kind := range []string{AuthorHuman, AuthorAgent, AuthorBot} {
		if out.Opened[kind] < 0 {
			return IssueWeek{}, fmt.Errorf("invalid issue week %q", path)
		}
	}
	return out, nil
}
