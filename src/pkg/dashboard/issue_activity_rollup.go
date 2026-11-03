package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Panel 6 (#12366, #12517, RFC #12062): the work-balance panel needs issue
// opens and closes for EVERY author, not just the writes the hive mediated.
// The metrics collector asks GitHub for every issue updated in the last
// issueActivityLookback (opening and closing both bump updated_at) and folds
// the answer into a durable rollup under /data, so the panel reads a file,
// never GitHub, and an issue seen once stays known after it ages out of the
// lookback. Record-only.

// issueActivityPath is the durable rollup. Under /data (the PVC) so it
// survives restarts, next to the write counters. A var so tests can point
// the summary at a fixture.
var issueActivityPath = "/data/hive-metrics/issue-activity.json"

const (
	// issueActivityLookback is how far back each GitHub fetch reaches; the
	// first fetch therefore covers issues opened or closed this far back and
	// the rollup's Since is first fetch minus this.
	issueActivityLookback = 42 * 24 * time.Hour
	// issueActivityRefresh bounds GitHub calls: the collector ticks every
	// five minutes, the rollup is refreshed at most this often.
	issueActivityRefresh = time.Hour
	// issueActivityRetention prunes issues closed longer ago than this; open
	// issues are kept regardless of age.
	issueActivityRetention = 180 * 24 * time.Hour
)

// IssueActivityRollup is the on-disk schema of issueActivityPath.
type IssueActivityRollup struct {
	// Since is the earliest instant the rollup covers completely: every issue
	// opened or closed at or after it has been seen (first fetch minus the
	// lookback). Weeks before it fall back to the audit log.
	Since time.Time `json:"since"`
	// FetchedAt is the last successful fetch; Repos the repos it covered.
	FetchedAt time.Time `json:"fetched_at"`
	Repos     []string  `json:"repos,omitempty"`
	// Issues is keyed by ghpkg.IssueActivityKey.
	Issues map[string]ghpkg.IssueActivity `json:"issues"`
}

// loadIssueActivityRollup reads the rollup at path; a missing or unreadable
// file is an empty rollup, never an error (the panel degrades to audit-only).
func loadIssueActivityRollup(path string) IssueActivityRollup {
	r := IssueActivityRollup{Issues: map[string]ghpkg.IssueActivity{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return r
	}
	var f IssueActivityRollup
	if json.Unmarshal(data, &f) != nil {
		return r
	}
	if f.Issues == nil {
		f.Issues = map[string]ghpkg.IssueActivity{}
	}
	return f
}

// mergeIssueActivity folds a fresh fetch into the rollup: newer records win
// per issue, Since is set on the first fetch and only ever moves earlier,
// and closed issues older than the retention are pruned.
func mergeIssueActivity(r IssueActivityRollup, fetched []ghpkg.IssueActivity, repos []string, now time.Time) IssueActivityRollup {
	if r.Issues == nil {
		r.Issues = map[string]ghpkg.IssueActivity{}
	}
	coverage := now.Add(-issueActivityLookback).UTC()
	if r.Since.IsZero() || coverage.Before(r.Since) {
		r.Since = coverage
	}
	for _, a := range fetched {
		r.Issues[ghpkg.IssueActivityKey(a.Repo, a.Number)] = a
	}
	cutoff := now.Add(-issueActivityRetention)
	for k, a := range r.Issues {
		if !a.ClosedAt.IsZero() && a.ClosedAt.Before(cutoff) {
			delete(r.Issues, k)
		}
	}
	r.FetchedAt = now.UTC()
	r.Repos = append([]string(nil), repos...)
	sort.Strings(r.Repos)
	return r
}

func saveIssueActivityRollup(path string, r IssueActivityRollup) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// issueActivityDue reports whether the rollup at path is old enough to
// refresh.
func issueActivityDue(path string, now time.Time) bool {
	r := loadIssueActivityRollup(path)
	return r.FetchedAt.IsZero() || now.Sub(r.FetchedAt) >= issueActivityRefresh
}

// collectIssueActivity refreshes the durable issue rollup from GitHub for
// every counted repo, at most once per issueActivityRefresh. A failed or
// shed fetch keeps the previous rollup untouched: the panel would rather
// show last hour's truth than a hole.
func (mc *MetricsCollector) collectIssueActivity(ctx context.Context) {
	gh := mc.client()
	repos := mc.countRepos()
	if gh == nil || len(repos) == 0 {
		return
	}
	path := mc.issueActivityPath()
	now := time.Now()
	if !issueActivityDue(path, now) {
		return
	}
	since := now.Add(-issueActivityLookback)
	var fetched []ghpkg.IssueActivity
	var covered []string
	for _, repo := range repos {
		got, err := gh.IssueActivitySince(ctx, repo, since)
		if err != nil {
			mc.logger.Warn("issue activity fetch skipped", "repo", repo, "error", err)
			continue
		}
		fetched = append(fetched, got...)
		covered = append(covered, repo)
	}
	if len(covered) == 0 {
		return
	}
	r := mergeIssueActivity(loadIssueActivityRollup(path), fetched, covered, now)
	if err := saveIssueActivityRollup(path, r); err != nil {
		mc.logger.Warn("issue activity rollup not saved", "path", path, "error", err)
		return
	}
	mc.logger.Info("issue activity rollup refreshed", "repos", len(covered), "fetched", len(fetched), "known", len(r.Issues))
}

func (mc *MetricsCollector) issueActivityPath() string {
	if mc != nil && mc.issueActivityCachePath != "" {
		return mc.issueActivityCachePath
	}
	return issueActivityPath
}
