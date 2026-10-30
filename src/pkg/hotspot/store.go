package hotspot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// IssueRecord remembers the modularize issue filed for one file so the
// advisor never opens a second one while it is open and waits the refile
// window after it closes.
type IssueRecord struct {
	Repo     string    `json:"repo"`
	File     string    `json:"file"`
	Number   int       `json:"number"`
	URL      string    `json:"url,omitempty"`
	FiledAt  time.Time `json:"filed_at"`
	ClosedAt time.Time `json:"closed_at,omitempty"`
}

type storeState struct {
	PRs       map[string]PRFiles   `json:"prs"`
	Conflicts []Conflict           `json:"conflicts"`
	Issues    []IssueRecord        `json:"issues"`
	Results   map[string][]Hotspot `json:"results,omitempty"`
}

// Store holds the PR file lists, conflict events and filed issues the
// advisor derives from, persisted as one JSON file.
type Store struct {
	mu    sync.Mutex
	path  string
	state storeState
	now   func() time.Time
}

// NewStore loads the store at path (an empty store if it does not exist).
// An empty path keeps the store in memory only.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	s.state.PRs = map[string]PRFiles{}
	s.state.Results = map[string][]Hotspot{}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, err
	}
	if s.state.PRs == nil {
		s.state.PRs = map[string]PRFiles{}
	}
	if s.state.Results == nil {
		s.state.Results = map[string][]Hotspot{}
	}
	return s, nil
}

// SetClock injects the clock (tests).
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

func prKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// ObservePR records (or refreshes) one open PR's file list. OpenedAt is
// kept from the first observation when the caller passes zero.
func (s *Store) ObservePR(repo string, number int, files []string, openedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	k := prKey(repo, number)
	pr, ok := s.state.PRs[k]
	if !ok {
		pr = PRFiles{Repo: repo, Number: number, OpenedAt: openedAt}
	}
	if pr.OpenedAt.IsZero() {
		if openedAt.IsZero() {
			openedAt = now
		}
		pr.OpenedAt = openedAt
	}
	pr.Files = append([]string(nil), files...)
	pr.SeenAt = now
	// A PR seen open again reopens its interval.
	pr.ClosedAt = time.Time{}
	pr.Merged = false
	s.state.PRs[k] = pr
	s.dirtySave()
}

// ClosePR ends a PR's open interval.
func (s *Store) ClosePR(repo string, number int, merged bool, closedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := prKey(repo, number)
	pr, ok := s.state.PRs[k]
	if !ok || !pr.ClosedAt.IsZero() {
		return
	}
	if closedAt.IsZero() {
		closedAt = s.now()
	}
	pr.ClosedAt = closedAt
	pr.Merged = merged
	s.state.PRs[k] = pr
	s.dirtySave()
}

// ReconcileOpen marks every recorded PR of repo that is not in stillOpen
// as closed now (merged unknown → true, since the hive only tracks PRs it
// was trying to land). Called after each sweep with the current open set.
func (s *Store) ReconcileOpen(repo string, stillOpen map[int]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := false
	for k, pr := range s.state.PRs {
		if pr.Repo != repo || !pr.ClosedAt.IsZero() || stillOpen[pr.Number] {
			continue
		}
		pr.ClosedAt = now
		pr.Merged = true
		s.state.PRs[k] = pr
		changed = true
	}
	if changed {
		s.dirtySave()
	}
}

// TouchOpen refreshes SeenAt for PRs known to be open right now (from the
// actionable listing the eval cycle already holds), so ExpireUnseen keeps
// them even when the merge train has no train to put them in.
func (s *Store) TouchOpen(repo string, numbers []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := false
	for _, n := range numbers {
		k := prKey(repo, n)
		pr, ok := s.state.PRs[k]
		if !ok || !pr.ClosedAt.IsZero() {
			continue
		}
		pr.SeenAt = now
		s.state.PRs[k] = pr
		changed = true
	}
	if changed {
		s.dirtySave()
	}
}

// ExpireUnseen closes the interval of every open PR not observed for
// staleAfter, at its last sighting. The hive only has the PRs' file lists
// while the merge train watches them, so "not seen" is the close signal.
func (s *Store) ExpireUnseen(staleAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-staleAfter)
	changed := false
	for k, pr := range s.state.PRs {
		if !pr.ClosedAt.IsZero() || pr.SeenAt.IsZero() || !pr.SeenAt.Before(cutoff) {
			continue
		}
		pr.ClosedAt = pr.SeenAt
		pr.Merged = true
		s.state.PRs[k] = pr
		changed = true
	}
	if changed {
		s.dirtySave()
	}
}

// ObserveConflict records a conflict/rebase-failure event for a PR and the
// files blamed for it.
func (s *Store) ObserveConflict(repo string, number int, files []string) {
	if len(files) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Conflicts = append(s.state.Conflicts, Conflict{
		Repo: repo, Number: number, Files: append([]string(nil), files...), At: s.now(),
	})
	s.dirtySave()
}

// RecordIssue remembers the issue filed for a file.
func (s *Store) RecordIssue(rec IssueRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.state.Issues {
		if r.Repo == rec.Repo && r.File == rec.File {
			s.state.Issues[i] = rec
			s.dirtySave()
			return
		}
	}
	s.state.Issues = append(s.state.Issues, rec)
	s.dirtySave()
}

// MarkIssueClosed records when a filed issue closed.
func (s *Store) MarkIssueClosed(repo, file string, closedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.state.Issues {
		if r.Repo == repo && r.File == file && r.ClosedAt.IsZero() {
			s.state.Issues[i].ClosedAt = closedAt
			s.dirtySave()
			return
		}
	}
}

// Issue returns the record for a file, if any.
func (s *Store) Issue(repo, file string) (IssueRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.state.Issues {
		if r.Repo == repo && r.File == file {
			return r, true
		}
	}
	return IssueRecord{}, false
}

// Prune drops PRs closed and conflicts older than window, and issue
// records closed longer ago than refile. It keeps the store bounded.
func (s *Store) Prune(window, refile time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	since := now.Add(-window)
	for k, pr := range s.state.PRs {
		if !pr.ClosedAt.IsZero() && pr.ClosedAt.Before(since) {
			delete(s.state.PRs, k)
		}
	}
	kept := s.state.Conflicts[:0]
	for _, c := range s.state.Conflicts {
		if !c.At.Before(since) {
			kept = append(kept, c)
		}
	}
	s.state.Conflicts = kept
	refileSince := now.Add(-refile)
	issues := s.state.Issues[:0]
	for _, r := range s.state.Issues {
		if r.ClosedAt.IsZero() || !r.ClosedAt.Before(refileSince) {
			issues = append(issues, r)
		}
	}
	s.state.Issues = issues
	s.dirtySave()
}

// Repos lists the repos with any recorded PR.
func (s *Store) Repos() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, pr := range s.state.PRs {
		set[pr.Repo] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Snapshot returns copies of the PR and conflict records for repo.
func (s *Store) Snapshot(repo string) ([]PRFiles, []Conflict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var prs []PRFiles
	for _, pr := range s.state.PRs {
		if pr.Repo == repo {
			prs = append(prs, pr)
		}
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	var cs []Conflict
	for _, c := range s.state.Conflicts {
		if c.Repo == repo {
			cs = append(cs, c)
		}
	}
	return prs, cs
}

// SetResults caches the latest analysis for repo so the dashboard serves
// it without recomputing.
func (s *Store) SetResults(repo string, hs []Hotspot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range hs {
		for _, r := range s.state.Issues {
			if r.Repo == repo && r.File == hs[i].File {
				hs[i].IssueNumber = r.Number
				hs[i].IssueURL = r.URL
			}
		}
	}
	s.state.Results[repo] = hs
	s.dirtySave()
}

// Results returns the cached analysis, all repos when repo is empty, sorted
// by score descending.
func (s *Store) Results(repo string) []Hotspot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Hotspot
	for r, hs := range s.state.Results {
		if repo != "" && r != repo {
			continue
		}
		out = append(out, hs...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].File < out[j].File
	})
	return out
}

// Counts summarises the cache for /api/status.
func (s *Store) Counts() (total, actionable, filed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hs := range s.state.Results {
		for _, h := range hs {
			total++
			if h.Actionable() {
				actionable++
			}
			if h.IssueNumber > 0 {
				filed++
			}
		}
	}
	return
}

// dirtySave writes the state; callers hold s.mu. Errors are swallowed: the
// store is advisory and rebuilds from the next sweeps.
func (s *Store) dirtySave() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
