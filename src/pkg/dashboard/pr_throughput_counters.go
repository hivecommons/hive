package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// prThroughputCountersPath is where the all-time PR throughput counters
// persist. The audit log cannot answer "all time": it rotates at
// auditMaxSizeMB, keeps auditMaxBackups files and drops anything older than
// auditMaxAgeDays, and its in-memory ring is emptied by a restart. A var (not
// const) only so tests can redirect it, mirroring auditLogPath.
var prThroughputCountersPath = "/data/pr-throughput-counters.json"

// prThroughputMergePathUnknown buckets pr_merged entries that carry no path=
// field (written before the field existed).
const prThroughputMergePathUnknown = "unknown"

// prThroughputActions are the audit actions the PR throughput counts are
// built from: a PR the hive opened, merged, or closed without merging.
var prThroughputActions = map[string]bool{
	ghpkg.AuditActionAgentPRCreated: true,
	ghpkg.AuditActionPRMerged:       true,
	ghpkg.AuditActionPRClosed:       true,
}

// PRThroughputCounters are the durable all-time totals behind the "all time"
// row of the PR throughput card. They are bumped once per audit entry at
// write time (AuditLog.Log), never recomputed from the audit files, so audit
// log rotation cannot double-count or lose them.
type PRThroughputCounters struct {
	Opened       int            `json:"opened"`
	Merged       int            `json:"merged"`
	MergedByPath map[string]int `json:"merged_by_path,omitempty"`
	Closed       int            `json:"closed"`
	// Since is the RFC3339 timestamp of the oldest event the counters cover —
	// how far back "all time" actually goes.
	Since string `json:"since,omitempty"`
}

// clone returns a deep copy safe to hand out of the AuditLog lock.
func (c PRThroughputCounters) clone() PRThroughputCounters {
	out := c
	out.MergedByPath = make(map[string]int, len(c.MergedByPath))
	for k, v := range c.MergedByPath {
		out.MergedByPath[k] = v
	}
	return out
}

// add counts one audit entry; it reports whether the entry was a PR
// throughput action at all.
func (c *PRThroughputCounters) add(e AuditEntry) bool {
	if !prThroughputActions[e.Action] {
		return false
	}
	switch e.Action {
	case ghpkg.AuditActionAgentPRCreated:
		c.Opened++
	case ghpkg.AuditActionPRMerged:
		c.Merged++
		if c.MergedByPath == nil {
			c.MergedByPath = map[string]int{}
		}
		c.MergedByPath[prThroughputMergePath(e.Detail)]++
	case ghpkg.AuditActionPRClosed:
		c.Closed++
	}
	if c.Since == "" || (e.Timestamp != "" && e.Timestamp < c.Since) {
		c.Since = e.Timestamp
	}
	return true
}

// prThroughputMergePath extracts the path= field of a pr_merged detail
// (sweep / queue / relay), or prThroughputMergePathUnknown when absent.
func prThroughputMergePath(detail string) string {
	if p := strings.TrimSpace(parseAuditDetailAttrs(detail)["path"]); p != "" {
		return p
	}
	return prThroughputMergePathUnknown
}

// loadPRThroughputCounters restores the counters from countersPath and makes
// it their persistence target. When no counters file exists yet (first boot
// with this feature, or an unreadable file) they are seeded ONCE from the
// audit files at auditPath and written out; from then on only Log bumps them,
// so the history the seed read is never counted a second time.
func (a *AuditLog) loadPRThroughputCounters(countersPath, auditPath string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prCountersPath = countersPath
	if data, err := os.ReadFile(countersPath); err == nil {
		var c PRThroughputCounters
		if json.Unmarshal(data, &c) == nil {
			a.prCounters = c
			return
		}
	}
	var seeded PRThroughputCounters
	for _, e := range a.actionsSince(time.Time{}, func(action string) bool { return prThroughputActions[action] }, nil, auditPath) {
		seeded.add(e)
	}
	a.prCounters = seeded
	a.persistPRThroughputCountersLocked()
}

// notePRThroughput bumps and persists the counters for a PR throughput audit
// entry. Callers must hold a.mu.
func (a *AuditLog) notePRThroughput(e AuditEntry) {
	if !a.prCounters.add(e) {
		return
	}
	a.persistPRThroughputCountersLocked()
}

// persistPRThroughputCountersLocked writes the counters atomically (temp file
// + rename) so a crash mid-write cannot leave a torn file that would reseed.
// Callers must hold a.mu.
func (a *AuditLog) persistPRThroughputCountersLocked() {
	if a.prCountersPath == "" {
		return
	}
	data, err := json.Marshal(a.prCounters)
	if err != nil {
		return
	}
	tmp := a.prCountersPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		slog.Error("pr throughput counters write failed", "error", err)
		return
	}
	if err := os.Rename(tmp, a.prCountersPath); err != nil {
		slog.Error("pr throughput counters rename failed", "error", err)
	}
}

// PRThroughputCounters returns a copy of the all-time PR throughput totals.
func (a *AuditLog) PRThroughputCounters() PRThroughputCounters {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prCounters.clone()
}
