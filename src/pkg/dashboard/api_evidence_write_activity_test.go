package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/proxy"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestBuildEvidenceWriteActivity pins the boundary panel's denominator
// policy (hivecommons/hive#12368): total writes = direct writes the proxy
// saw (weekly buckets overlapping the window) + hive-mediated writes the
// audit log recorded; blocked = proxy refusals + mediated refusals. A
// counter file that started inside the window is flagged partial, and an
// absent file is "not recorded", never zero.
func TestBuildEvidenceWriteActivity(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) // Thu, week of 2026-10-12
	path := filepath.Join(t.TempDir(), "write-counters.json")
	file := proxy.DurableWriteCounts{
		RecordedSince: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Weeks: map[string]proxy.WeekCounts{
			"2026-10-05": {Attempted: 10, Blocked: 1, Tiers: map[string]proxy.TierCounts{"ISSUES_PRS_MERGE": {Attempted: 10, Blocked: 1}}, BlockedByReason: map[string]int64{proxy.DenyReasonMode: 1}},
			"2026-10-12": {Attempted: 6, Blocked: 6, Tiers: map[string]proxy.TierCounts{"ISSUES_PRS_MERGE": {Attempted: 6, Blocked: 6}}, BlockedByReason: map[string]int64{proxy.DenyReasonMode: 4, proxy.DenyReasonRepoPaused: 2}},
		},
	}
	data, _ := json.Marshal(file)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	entries := []AuditEntry{
		{Timestamp: ts(now.Add(-time.Hour)), Action: ghpkg.AuditActionAgentCommentCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: ts(now.Add(-time.Hour)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=2"},
		{Timestamp: ts(now.Add(-2 * time.Hour)), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=x/y, number=3"},
		{Timestamp: ts(now.Add(-time.Hour)), Action: ghpkg.AuditActionAgentWriteRefused, Detail: "repo=o/r, number=4"},
		// not a write
		{Timestamp: ts(now.Add(-time.Hour)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=5"},
		// outside the 30d window
		{Timestamp: ts(now.Add(-40 * 24 * time.Hour)), Action: ghpkg.AuditActionAgentCommentCreated, Detail: "repo=o/r, number=6"},
	}
	win := evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}
	got := buildEvidenceWriteActivity(path, entries, win, "", now)
	if !got.Direct.Recorded || got.Direct.Attempted != 16 || got.Direct.Blocked != 7 {
		t.Errorf("direct = %+v", got.Direct)
	}
	if got.Direct.ByTier["ISSUES_PRS_MERGE"].Attempted != 16 || got.Direct.BlockedByReason[proxy.DenyReasonMode] != 5 || got.Direct.BlockedByReason[proxy.DenyReasonRepoPaused] != 2 {
		t.Errorf("direct tiers/reasons = %+v %+v", got.Direct.ByTier, got.Direct.BlockedByReason)
	}
	if !got.Direct.PartialCoverage || got.Direct.Since != "2026-10-05T00:00:00Z" {
		t.Errorf("a counter file younger than the window must be flagged partial with its start: %+v", got.Direct)
	}
	if got.Mediated.N != 3 || got.Mediated.Refused != 1 || got.Mediated.ByAction[ghpkg.AuditActionAgentCommentCreated] != 1 {
		t.Errorf("mediated = %+v", got.Mediated)
	}
	if got.TotalWrites != 19 || got.Blocked != 8 {
		t.Errorf("total_writes=%d blocked=%d, want 19/8 (direct 16+mediated 3; blocked 7+refused 1)", got.TotalWrites, got.Blocked)
	}
	// repo filter applies to mediated writes only; the counter file has no repo.
	if r := buildEvidenceWriteActivity(path, entries, win, "o/r", now); r.Mediated.N != 2 || r.Direct.Attempted != 16 {
		t.Errorf("repo-filtered = %+v", r)
	}
	// a window the file fully covers is not partial
	wk := evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}
	if d := buildEvidenceDirectWrites(path, wk, now); d.PartialCoverage || d.Attempted != 6 {
		t.Errorf("24h window (current week bucket only) = %+v", d)
	}
	// all time is never partial: the file's own start is the horizon
	if d := buildEvidenceDirectWrites(path, evidenceWindow{To: now}, now); d.Attempted != 16 || d.PartialCoverage {
		t.Errorf("all-time = %+v", d)
	}
	// a file that predates the window covers it fully
	old := file
	old.RecordedSince = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	data, _ = json.Marshal(old)
	oldPath := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(oldPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if d := buildEvidenceDirectWrites(oldPath, win, now); d.PartialCoverage || d.Attempted != 16 || d.Since != "2026-01-05T00:00:00Z" {
		t.Errorf("file older than window = %+v", d)
	}
	// absent file: not recorded, not zero
	if d := buildEvidenceDirectWrites(filepath.Join(t.TempDir(), "none.json"), win, now); d.Recorded || d.Attempted != 0 || d.Since != "" {
		t.Errorf("absent file = %+v", d)
	}
}
