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

// IssueSeriesDirName is the per-repo directory under <root>/<owner>/<repo>
// that holds one IssueWeek file per ISO week (<YYYY-Www>.json). Review-bundle
// walkers skip it, like complexity.DirName.
const IssueSeriesDirName = "issues"

// IssueEvent.Action values.
const (
	IssueActionOpened = "opened"
	IssueActionClosed = "closed"
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

// IssueSeriesWeek is one IssueWeek file together with the repository whose
// directory it was read from.
type IssueSeriesWeek struct {
	Repo string
	IssueWeek
}

// Covers reports whether t falls inside the week's ISO week and its
// covered_from/covered_to range (end exclusive). Readers and aggregators share
// this gate so an event is never counted outside the window its file claims.
func (w IssueWeek) Covers(t time.Time) bool {
	return !t.IsZero() && IssueWeekKey(t) == w.Week && !t.Before(w.CoveredFrom) && t.Before(w.CoveredTo)
}

// Valid reports whether the event carries a known action and, for opened
// events, a known author kind.
func (e IssueEvent) Valid() bool {
	switch e.Action {
	case IssueActionOpened:
		return e.Kind == AuthorHuman || e.Kind == AuthorAgent || e.Kind == AuthorBot
	case IssueActionClosed:
		return true
	}
	return false
}

// IssueWeekKey is the ISO week of t in UTC, formatted as IssueWeek.Week
// (<YYYY-Www>).
func IssueWeekKey(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// IssueWeekPath is <root>/<owner>/<repo>/issues/<week>.json after validating
// the repository slug and ISO week so neither can escape root.
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
	return filepath.Join(root, owner, name, IssueSeriesDirName, week+".json"), nil
}

// WriteIssueWeek atomically replaces the week's file under root.
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

// ReadIssueWeek loads one week's file and rejects structurally invalid
// snapshots (mismatched week, negative counts, empty coverage).
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

// LoadIssueSeries reads every IssueWeek file under root. Weeks that are
// malformed, lack an events array, or carry an event outside their own
// coverage are skipped so a caller's fallback stays available for them.
func LoadIssueSeries(root string) []IssueSeriesWeek {
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", IssueSeriesDirName, "*.json"))
	if err != nil {
		return nil
	}
	var out []IssueSeriesWeek
	for _, path := range paths {
		repoDir := filepath.Dir(filepath.Dir(path))
		repo := filepath.Base(filepath.Dir(repoDir)) + "/" + filepath.Base(repoDir)
		week := strings.TrimSuffix(filepath.Base(path), ".json")
		w, err := ReadIssueWeek(root, repo, week)
		if err != nil || w.Events == nil {
			continue
		}
		valid := true
		for _, event := range w.Events {
			if !w.Covers(event.At) || !event.Valid() {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, IssueSeriesWeek{Repo: repo, IssueWeek: w})
		}
	}
	return out
}
