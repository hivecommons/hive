package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// prThroughputDefaultHours is the window when ?hours is absent.
	prThroughputDefaultHours = 24
	// prThroughputMaxHours caps a windowed request at the audit log's own
	// retention (auditMaxAgeDays): asking for more would report a partial
	// window as if it were complete. Longer spans use hours=0 (all time).
	prThroughputMaxHours = auditMaxAgeDays * 24

	prThroughputSourceAudit    = "audit"
	prThroughputSourceCounters = "counters"
)

// prThroughputAuditPath is where the windowed counts scan the on-disk audit
// log; "" means the production auditLogPath. A test seam in the shape of
// watchdogActivityAuditPath.
var prThroughputAuditPath = ""

// PRThroughput is the GET /api/pr-throughput response.
type PRThroughput struct {
	// Hours is the window actually applied (after clamping); 0 means all time.
	Hours   int  `json:"hours"`
	AllTime bool `json:"all_time"`
	// Since is the RFC3339 start of the window (for all time: the oldest event
	// the durable counters cover). Empty when there is nothing recorded yet.
	Since string `json:"since,omitempty"`
	// RecordedSince is the RFC3339 timestamp of the oldest PR event the
	// underlying data holds, so a reader can tell when the window is longer
	// than the recorded history ("over the last X of recorded history").
	RecordedSince string `json:"recorded_since,omitempty"`
	// Source names the data behind the counts: "audit" (the audit log, for a
	// window) or "counters" (the durable all-time counters).
	Source string `json:"source"`
	// Opened counts PRs the hive's agents created (agent_pr_created).
	Opened int `json:"opened"`
	// Merged counts merges the hive performed (pr_merged), split by the path
	// that performed them in MergedByPath: sweep, queue, relay, or unknown for
	// entries written before the path field existed.
	Merged       int            `json:"merged"`
	MergedByPath map[string]int `json:"merged_by_path"`
	// Closed counts PRs the hive closed without merging (pr_closed).
	Closed int `json:"closed"`
}

// handlePRThroughput serves GET /api/pr-throughput?hours=N: how many PRs the
// hive opened, merged, and closed without merging over the last N hours, or
// all time when hours=0. Read-only and additive like /api/lifecycle-timeline,
// which already exposes merged counts at the same tier.
func (s *Server) handlePRThroughput(w http.ResponseWriter, r *http.Request) {
	hours := prThroughputDefaultHours
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(w, "hours must be a non-negative integer (0 = all time)", http.StatusBadRequest)
			return
		}
		hours = n
	}
	if hours > prThroughputMaxHours {
		hours = prThroughputMaxHours
	}

	audit := s.audit
	if audit == nil {
		audit = &AuditLog{}
	}
	if hours == 0 {
		jsonResponse(w, buildPRThroughputAllTime(audit.PRThroughputCounters()))
		return
	}

	// The file is the record; the ring is consulted only when there is no
	// file at all (no /data volume), as in handleWatchdogActivity.
	var entries []AuditEntry
	if audit.HasOnDiskLog(prThroughputAuditPath) {
		entries = audit.OutputActionsSince(time.Time{}, prThroughputActions, prThroughputAuditPath)
	} else {
		for _, e := range audit.RecentWithPrefixSince(time.Time{}, "") {
			if prThroughputActions[e.Action] {
				entries = append(entries, e)
			}
		}
	}
	jsonResponse(w, buildPRThroughputWindow(entries, time.Now().UTC(), hours))
}

// buildPRThroughputWindow counts the PR throughput entries at or after
// now-hours. entries may reach further back than the window: the oldest one
// becomes RecordedSince. Pure, so the handler and unit tests share it.
func buildPRThroughputWindow(entries []AuditEntry, now time.Time, hours int) PRThroughput {
	since := now.Add(-time.Duration(hours) * time.Hour)
	out := PRThroughput{
		Hours:        hours,
		Since:        since.Format(time.RFC3339),
		Source:       prThroughputSourceAudit,
		MergedByPath: map[string]int{},
	}
	var counts PRThroughputCounters
	var oldest time.Time
	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || !prThroughputActions[e.Action] {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		if t.Before(since) {
			continue
		}
		counts.add(e)
	}
	if !oldest.IsZero() {
		out.RecordedSince = oldest.UTC().Format(time.RFC3339)
	}
	fillPRThroughputCounts(&out, counts)
	return out
}

// buildPRThroughputAllTime renders the durable counters as the hours=0 row.
func buildPRThroughputAllTime(c PRThroughputCounters) PRThroughput {
	out := PRThroughput{
		AllTime:       true,
		Since:         c.Since,
		RecordedSince: c.Since,
		Source:        prThroughputSourceCounters,
		MergedByPath:  map[string]int{},
	}
	fillPRThroughputCounts(&out, c)
	return out
}

func fillPRThroughputCounts(out *PRThroughput, c PRThroughputCounters) {
	out.Opened = c.Opened
	out.Merged = c.Merged
	out.Closed = c.Closed
	for k, v := range c.MergedByPath {
		out.MergedByPath[k] = v
	}
}
