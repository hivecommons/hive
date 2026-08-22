package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"
)

// activityCollectInterval is how often the spoke recomputes its per-repo output
// activity from the audit log. Unlike the fleet-stats collector this makes ZERO
// GitHub calls (it reads /data/audit.jsonl), so it can run frequently and needs
// no jitter or search-quota backoff.
var activityCollectInterval = 5 * time.Minute

// activityWindow is the rolling window over which activity is counted. The hub's
// health verdict cares about the last 12h; we retain a wider window so the hub
// can compute freshness itself from NewestAt and so a slightly-late collect
// never drops a just-inside-12h event.
const activityWindow = 14 * 24 * time.Hour

// activityHealthWindowHours is advertised to the hub as the intended freshness
// window (the hub still computes recency from NewestAt).
const activityHealthWindowHours = 12

// Audit action names the collector counts as OUTPUT to a work source. Kept in
// sync with pkg/github/attribution.go. Advisory is counted separately because it
// is the L2 output signal.
var activityOutputActions = map[string]bool{
	"agent_issue_created":   true,
	"agent_pr_created":      true,
	"agent_comment_created": true,
	"pr_merged":             true,
	"agent_issue_claimed":   true,
	"agent_pr_reviewed":     true,
	"advisory_commented":    true,
}

// ActivityActionStat is one action's count within the window plus the newest
// timestamp seen for it (RFC3339). NewestAt lets the hub compute "within 12h"
// itself.
type ActivityActionStat struct {
	Count    int    `json:"count"`
	NewestAt string `json:"newest_at,omitempty"`
}

// RepoActivity is per-repo output activity over the window.
type RepoActivity struct {
	Repo     string             `json:"repo"`
	Issues   ActivityActionStat `json:"issues"`
	PRs      ActivityActionStat `json:"prs"`
	Comments ActivityActionStat `json:"comments"`
	Merges   ActivityActionStat `json:"merges"`
	Claims   ActivityActionStat `json:"claims"`
	Reviews  ActivityActionStat `json:"reviews"`
	Advisory ActivityActionStat `json:"advisory"`
}

// ActivitySnapshot is the full per-hive activity summary the collector produces
// and the heartbeat ships.
type ActivitySnapshot struct {
	Repos       []RepoActivity `json:"repos"`
	WindowHours int            `json:"window_hours"`
	CollectedAt time.Time      `json:"collected_at"`
}

// auditReader is the subset of *AuditLog the collector needs, so it can be
// faked in tests without a full dashboard Server.
type auditReader interface {
	OutputActionsSince(since time.Time, actions map[string]bool, filePath string) []AuditEntry
}

// ActivityCollector summarizes the spoke's audit-log output activity per repo.
// Mirrors FleetStatsCollector's lifecycle (EnablePersistence/Start/Snapshot/
// CollectedAt) but reads the local audit file instead of the GitHub API.
type ActivityCollector struct {
	mu          sync.Mutex
	audit       auditReader
	auditPath   string // "" → the default auditLogPath (used by OutputActionsSince)
	logger      *slog.Logger
	nowFn       func() time.Time
	persistPath string

	snap        ActivitySnapshot
	ready       bool
	collectedAt time.Time
}

// persistedActivity is the on-disk sidecar shape.
type persistedActivity struct {
	Snapshot    ActivitySnapshot `json:"snapshot"`
	CollectedAt time.Time        `json:"collected_at"`
}

// NewActivityCollector builds a collector over the given audit reader. audit nil
// makes it inert (Snapshot ready=false). auditPath is where OutputActionsSince
// reads ("" → the production audit log).
func NewActivityCollector(audit auditReader, auditPath string, logger *slog.Logger) *ActivityCollector {
	return &ActivityCollector{
		audit:     audit,
		auditPath: auditPath,
		logger:    logger,
		nowFn:     time.Now,
	}
}

// EnablePersistence mirrors each collect to path and restores whatever is there
// now, so the first heartbeat after a restart reports the last summary rather
// than nil. Missing file = clean first boot; corrupt = logged + ignored. Call
// once before Start.
func (ac *ActivityCollector) EnablePersistence(path string) {
	if ac == nil {
		return
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.persistPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && ac.logger != nil {
			ac.logger.Warn("activity store unreadable — starting fresh", "path", path, "error", err)
		}
		return
	}
	var stored persistedActivity
	if err := json.Unmarshal(data, &stored); err != nil {
		if ac.logger != nil {
			ac.logger.Warn("activity store corrupt — starting fresh", "path", path, "error", err)
		}
		return
	}
	if stored.CollectedAt.IsZero() {
		return
	}
	ac.snap = stored.Snapshot
	ac.collectedAt = stored.CollectedAt
	ac.ready = true
}

func (ac *ActivityCollector) persistLocked() {
	if ac.persistPath == "" {
		return
	}
	data, err := json.Marshal(persistedActivity{Snapshot: ac.snap, CollectedAt: ac.collectedAt})
	if err != nil {
		return
	}
	tmp := ac.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if ac.logger != nil {
			ac.logger.Warn("failed to write activity store", "path", ac.persistPath, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, ac.persistPath); err != nil && ac.logger != nil {
		ac.logger.Warn("failed to replace activity store", "path", ac.persistPath, "error", err)
	}
}

// Start runs the collect loop until ctx is cancelled — once up front, then every
// activityCollectInterval. Inert (returns) when there is no audit reader.
func (ac *ActivityCollector) Start(ctx context.Context) {
	if ac == nil || ac.audit == nil {
		return
	}
	ac.collect()
	ticker := time.NewTicker(activityCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ac.collect()
		}
	}
}

// repoRe extracts repo=<org/name> from an audit detail string ("k=v, k=v").
var repoRe = regexp.MustCompile(`(?:^|[,\s])repo=([^,\s]+)`)

// collect reads the window of output actions from the audit log and rebuilds the
// per-repo summary.
func (ac *ActivityCollector) collect() {
	now := ac.nowFn()
	since := now.Add(-activityWindow)
	entries := ac.audit.OutputActionsSince(since, activityOutputActions, ac.auditPath)

	byRepo := map[string]*RepoActivity{}
	bump := func(stat *ActivityActionStat, ts string) {
		stat.Count++
		if ts > stat.NewestAt { // RFC3339 is lexically sortable
			stat.NewestAt = ts
		}
	}
	for _, e := range entries {
		m := repoRe.FindStringSubmatch(e.Detail)
		if m == nil {
			continue // no repo → not attributable output (e.g. a lifecycle event)
		}
		repo := m[1]
		ra := byRepo[repo]
		if ra == nil {
			ra = &RepoActivity{Repo: repo}
			byRepo[repo] = ra
		}
		switch e.Action {
		case "agent_issue_created":
			bump(&ra.Issues, e.Timestamp)
		case "agent_pr_created":
			bump(&ra.PRs, e.Timestamp)
		case "agent_comment_created":
			bump(&ra.Comments, e.Timestamp)
		case "pr_merged":
			bump(&ra.Merges, e.Timestamp)
		case "agent_issue_claimed":
			bump(&ra.Claims, e.Timestamp)
		case "agent_pr_reviewed":
			bump(&ra.Reviews, e.Timestamp)
		case "advisory_commented":
			bump(&ra.Advisory, e.Timestamp)
		}
	}
	repos := make([]RepoActivity, 0, len(byRepo))
	for _, ra := range byRepo {
		repos = append(repos, *ra)
	}

	ac.mu.Lock()
	ac.snap = ActivitySnapshot{Repos: repos, WindowHours: activityHealthWindowHours, CollectedAt: now}
	ac.collectedAt = now
	ac.ready = true
	ac.persistLocked()
	ac.mu.Unlock()
}

// Snapshot returns the last computed summary and whether a collect has succeeded.
func (ac *ActivityCollector) Snapshot() (ActivitySnapshot, bool) {
	if ac == nil {
		return ActivitySnapshot{}, false
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.snap, ac.ready
}

// CollectedAt returns when the summary was last computed.
func (ac *ActivityCollector) CollectedAt() time.Time {
	if ac == nil {
		return time.Time{}
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.collectedAt
}
