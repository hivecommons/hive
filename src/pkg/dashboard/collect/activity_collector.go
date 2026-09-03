// Package collect holds pkg/dashboard's producer-side collectors: the
// background cache/aggregation types (activity, repo-cost, fleet-stats), the
// generic timeSeries ring buffer behind every persisted sparkline history, and
// the budget-window tracker. Slice 2 of the kubestellar/hive#5565
// decomposition: pure moves out of the dashboard god package, wired back in
// through Dependencies fields exactly as before. This package deliberately
// imports only leaf vocabulary (pkg/github, pkg/tokens) — never pkg/dashboard.
package collect

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// AuditEntry is one line of the dashboard's audit log. The type lives here —
// with its consumers, the collectors — and pkg/dashboard aliases it
// (type AuditEntry = collect.AuditEntry), so the write side (AuditLog) and the
// wire/JSON contract are byte-identical to before the split.
type AuditEntry struct {
	Timestamp string `json:"ts"`
	User      string `json:"user"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
	// UserName is the hub-delivered display name when User is an opaque OIDC
	// identity key. Stamped at SERVE time only (handleAuditLog) — the ring and
	// the on-disk log keep the raw key, so history survives name changes.
	UserName string `json:"user_name,omitempty"`
}

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
//
// This is NOT the window Count is accumulated over — that is activityWindow
// (14d). The snapshot reports both: window_hours (this constant, freshness)
// and count_window_hours (activityWindow). They are 28x apart, so a consumer
// that divides Count by window_hours to get a rate overstates activity by 28x.
const activityHealthWindowHours = 12

// ActivityOutputActions is the set of audit action names the collector counts
// as OUTPUT to a work source. Kept in sync with pkg/github/attribution.go.
// Advisory is counted separately because it is the L2 output signal. Exported
// because pkg/dashboard's handleRepoCost fallback path reads the same set.
var ActivityOutputActions = map[string]bool{
	ghpkg.AuditActionAgentIssueCreated:       true,
	ghpkg.AuditActionAgentPRCreated:          true,
	ghpkg.AuditActionAgentCommentCreated:     true,
	ghpkg.AuditActionPRMerged:                true,
	ghpkg.AuditActionIssueClaimed:            true,
	ghpkg.AuditActionPRReviewed:              true,
	ghpkg.AuditActionAdvisoryCommented:       true,
	ghpkg.AuditActionHiveIssueCreated:        true,
	ghpkg.AuditActionPRAttributionReconciled: true,
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
	Repo       string              `json:"repo"`
	Issues     ActivityActionStat  `json:"issues"`
	PRs        ActivityActionStat  `json:"prs"`
	Comments   ActivityActionStat  `json:"comments"`
	Merges     ActivityActionStat  `json:"merges"`
	Claims     ActivityActionStat  `json:"claims"`
	Reviews    ActivityActionStat  `json:"reviews"`
	Advisory   ActivityActionStat  `json:"advisory"`
	Reconciled ActivityActionStat  `json:"reconciled"`
	Agents     []AgentRepoActivity `json:"agents,omitempty"`
}

// AgentRepoActivity is the same recorded-fact activity breakdown scoped to one
// hive agent inside one repo. Agent is "unknown" only when an older/broken audit
// line omitted both the typed Agent field and the detail's agent= pair.
type AgentRepoActivity struct {
	Agent      string             `json:"agent"`
	Issues     ActivityActionStat `json:"issues"`
	PRs        ActivityActionStat `json:"prs"`
	Comments   ActivityActionStat `json:"comments"`
	Merges     ActivityActionStat `json:"merges"`
	Claims     ActivityActionStat `json:"claims"`
	Reviews    ActivityActionStat `json:"reviews"`
	Advisory   ActivityActionStat `json:"advisory"`
	Reconciled ActivityActionStat `json:"reconciled"`
}

// ActivitySnapshot is the full per-hive activity summary the collector produces
// and the heartbeat ships.
type ActivitySnapshot struct {
	Repos        []RepoActivity     `json:"repos"`
	Unattributed ActivityActionStat `json:"unattributed"`
	// WindowHours is the FRESHNESS window the hub's health verdict uses. It is
	// deliberately NOT the window Count was accumulated over — see
	// CountWindowHours. Kept under its original name because the hub registry
	// and heartbeat already carry this field with this meaning.
	WindowHours int `json:"window_hours"`
	// CountWindowHours is the lookback the per-repo Count values were actually
	// accumulated over (activityWindow). A consumer deriving a RATE must divide
	// by this, not by WindowHours: they differ by 28x, so using the freshness
	// window would overstate activity by that factor.
	CountWindowHours int       `json:"count_window_hours"`
	CollectedAt      time.Time `json:"collected_at"`
}

// AuditReader is the subset of pkg/dashboard's *AuditLog the collectors need,
// so they can be faked in tests without a full dashboard Server.
type AuditReader interface {
	OutputActionsSince(since time.Time, actions map[string]bool, filePath string) []AuditEntry
}

// ActivityCollector summarizes the spoke's audit-log output activity per repo.
// Mirrors FleetStatsCollector's lifecycle (EnablePersistence/Start/Snapshot/
// CollectedAt) but reads the local audit file instead of the GitHub API.
type ActivityCollector struct {
	mu          sync.Mutex
	audit       AuditReader
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
func NewActivityCollector(audit AuditReader, auditPath string, logger *slog.Logger) *ActivityCollector {
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
var agentRe = regexp.MustCompile(`(?:^|[,\s])agent=([^,\s]+)`)

// collect reads the window of output actions from the audit log and rebuilds the
// per-repo summary.
func (ac *ActivityCollector) collect() {
	now := ac.nowFn()
	since := now.Add(-activityWindow)
	entries := ac.audit.OutputActionsSince(since, ActivityOutputActions, ac.auditPath)

	byRepo := map[string]*RepoActivity{}
	byRepoAgent := map[string]map[string]*AgentRepoActivity{}
	bump := func(stat *ActivityActionStat, ts string) {
		stat.Count++
		if ts > stat.NewestAt { // RFC3339 is lexically sortable
			stat.NewestAt = ts
		}
	}
	var unattributed ActivityActionStat
	for _, e := range entries {
		m := repoRe.FindStringSubmatch(e.Detail)
		if m == nil {
			bump(&unattributed, e.Timestamp)
			continue
		}
		repo := m[1]
		ra := byRepo[repo]
		if ra == nil {
			ra = &RepoActivity{Repo: repo}
			byRepo[repo] = ra
		}
		agent := activityAgent(e)
		agentMap := byRepoAgent[repo]
		if agentMap == nil {
			agentMap = map[string]*AgentRepoActivity{}
			byRepoAgent[repo] = agentMap
		}
		aa := agentMap[agent]
		if aa == nil {
			aa = &AgentRepoActivity{Agent: agent}
			agentMap[agent] = aa
		}
		switch e.Action {
		case ghpkg.AuditActionAgentIssueCreated, ghpkg.AuditActionHiveIssueCreated:
			bump(&ra.Issues, e.Timestamp)
			bump(&aa.Issues, e.Timestamp)
		case ghpkg.AuditActionAgentPRCreated:
			bump(&ra.PRs, e.Timestamp)
			bump(&aa.PRs, e.Timestamp)
		case ghpkg.AuditActionAgentCommentCreated:
			bump(&ra.Comments, e.Timestamp)
			bump(&aa.Comments, e.Timestamp)
		case ghpkg.AuditActionPRMerged:
			bump(&ra.Merges, e.Timestamp)
			bump(&aa.Merges, e.Timestamp)
		case ghpkg.AuditActionIssueClaimed:
			bump(&ra.Claims, e.Timestamp)
			bump(&aa.Claims, e.Timestamp)
		case ghpkg.AuditActionPRReviewed:
			bump(&ra.Reviews, e.Timestamp)
			bump(&aa.Reviews, e.Timestamp)
		case ghpkg.AuditActionAdvisoryCommented:
			bump(&ra.Advisory, e.Timestamp)
			bump(&aa.Advisory, e.Timestamp)
		case ghpkg.AuditActionPRAttributionReconciled:
			bump(&ra.Reconciled, e.Timestamp)
			bump(&aa.Reconciled, e.Timestamp)
		}
	}
	repos := make([]RepoActivity, 0, len(byRepo))
	for _, ra := range byRepo {
		agents := make([]AgentRepoActivity, 0, len(byRepoAgent[ra.Repo]))
		for _, aa := range byRepoAgent[ra.Repo] {
			agents = append(agents, *aa)
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].Agent < agents[j].Agent })
		ra.Agents = agents
		repos = append(repos, *ra)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })

	ac.mu.Lock()
	ac.snap = ActivitySnapshot{Repos: repos, Unattributed: unattributed, WindowHours: activityHealthWindowHours, CountWindowHours: int(activityWindow / time.Hour), CollectedAt: now}
	ac.collectedAt = now
	ac.ready = true
	ac.persistLocked()
	ac.mu.Unlock()
}

func activityAgent(e AuditEntry) string {
	if e.Agent != "" {
		return e.Agent
	}
	if m := agentRe.FindStringSubmatch(e.Detail); m != nil {
		return m[1]
	}
	return "unknown"
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

// AuditPath returns the audit file path this collector was configured with
// ("" = the production default). pkg/dashboard's handleRepoCost fallback uses
// it so the cost join reads exactly the same log file set.
func (ac *ActivityCollector) AuditPath() string {
	if ac == nil {
		return ""
	}
	return ac.auditPath
}
