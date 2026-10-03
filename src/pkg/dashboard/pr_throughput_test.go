package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// ── #9430: durable all-time PR throughput counters ──────────────────────────

func loadedPRCounterLog(t *testing.T, countersPath, auditPath string) *AuditLog {
	t.Helper()
	a := &AuditLog{}
	a.loadPRThroughputCounters(countersPath, auditPath)
	return a
}

func TestPRThroughputCountersActorBackCompat(t *testing.T) {
	dir := t.TempDir()
	countersPath := filepath.Join(dir, "pr-throughput-counters.json")
	auditPath := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(countersPath, []byte(`{"opened":2,"merged":1,"merged_by_path":{"sweep":1},"closed":0,"since":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := loadedPRCounterLog(t, countersPath, auditPath)
	before := a.PRThroughputCounters()
	if before.Opened != 2 || before.Merged != 1 {
		t.Fatalf("loaded legacy counters = %+v", before)
	}
	a.LogRecordAt("2026-01-02T00:30:00Z", "system", ghpkg.AuditActionPRMerged, "repo=o/r, number=9, path=human, actor=alice", "governor", "o/r", 9)
	got := loadedPRCounterLog(t, countersPath, auditPath).PRThroughputCounters()
	if got.Version != prThroughputCounterVersion || got.ByActor[prThroughputKindPR][prThroughputRoleMerged][prThroughputActorHuman] != 1 {
		t.Fatalf("upgraded counters = %+v, want version and human merge actor count", got)
	}
	if len(got.ActorBuckets) == 0 {
		t.Fatal("actor buckets must persist for all-time series")
	}
}

func TestPRThroughputCountersBumpOnlyOnPRActions(t *testing.T) {
	a := &AuditLog{} // no persistence path: in-memory only
	a.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=1", "scanner")
	a.Log("system", ghpkg.AuditActionPRMerged, "repo=o/r, number=1, path=sweep", "governor")
	a.Log("system", ghpkg.AuditActionPRMerged, "repo=o/r, number=2, path=queue", "governor")
	a.Log("system", ghpkg.AuditActionPRMerged, "repo=o/r, number=3", "governor")
	a.Log("system", ghpkg.AuditActionPRClosed, "repo=o/r, number=4, path=relay", "scanner")
	a.Log("system", ghpkg.AuditActionIssueClosed, "repo=o/r, number=5", "scanner")
	a.Log("alice", "config.save", "file=hive.yaml", "")

	got := a.PRThroughputCounters()
	if got.Opened != 1 || got.Merged != 3 || got.Closed != 1 {
		t.Fatalf("counters = %+v, want opened=1 merged=3 closed=1", got)
	}
	want := map[string]int{"sweep": 2, prThroughputMergePathUnknown: 1}
	for k, v := range want {
		if got.MergedByPath[k] != v {
			t.Errorf("merged_by_path[%s] = %d, want %d (all=%v)", k, got.MergedByPath[k], v, got.MergedByPath)
		}
	}
	if got.Since == "" {
		t.Error("Since should be stamped with the first counted event")
	}
	// The returned copy must not alias the live map.
	got.MergedByPath["sweep"] = 99
	if a.PRThroughputCounters().MergedByPath["sweep"] != 2 {
		t.Error("PRThroughputCounters returned an aliased map")
	}
}

func TestPRThroughputCountersSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	countersPath := filepath.Join(dir, "pr-throughput-counters.json")
	auditPath := filepath.Join(dir, "audit.jsonl") // never written: nothing to seed

	a := loadedPRCounterLog(t, countersPath, auditPath)
	a.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=1", "scanner")
	a.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=2", "scanner")
	a.Log("system", ghpkg.AuditActionPRMerged, "repo=o/r, number=1, path=relay", "governor")
	a.Log("system", ghpkg.AuditActionPRClosed, "repo=o/r, number=2, path=relay", "scanner")
	before := a.PRThroughputCounters()

	// "Restart": a fresh AuditLog reading the same counters file.
	b := loadedPRCounterLog(t, countersPath, auditPath)
	after := b.PRThroughputCounters()
	if after.Opened != 2 || after.Merged != 1 || after.Closed != 1 || after.MergedByPath["relay"] != 1 {
		t.Fatalf("after restart counters = %+v, want opened=2 merged=1(relay) closed=1", after)
	}
	if after.Since != before.Since {
		t.Errorf("Since changed across restart: %q → %q", before.Since, after.Since)
	}

	b.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=3", "scanner")
	if got := loadedPRCounterLog(t, countersPath, auditPath).PRThroughputCounters().Opened; got != 3 {
		t.Fatalf("opened after second restart = %d, want 3", got)
	}
}

func TestPRThroughputCountersSeedOnceAndNoDoubleCountOnRotation(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	countersPath := filepath.Join(dir, "pr-throughput-counters.json")
	auditPath := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-48 * time.Hour)), User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(now.Add(-24 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), User: "system", Action: "agent_issue_created", Detail: "repo=o/r, number=9"},
	})

	// First boot with the feature: seeded from the existing audit history.
	a := loadedPRCounterLog(t, countersPath, auditPath)
	got := a.PRThroughputCounters()
	if got.Opened != 1 || got.Merged != 1 || got.Closed != 0 || got.MergedByPath[prThroughputMergePathUnknown] != 1 {
		t.Fatalf("seeded counters = %+v, want opened=1 merged=1(unknown)", got)
	}
	if got.Since != rfc3339(now.Add(-48*time.Hour)) {
		t.Errorf("seeded Since = %q, want oldest PR event %q", got.Since, rfc3339(now.Add(-48*time.Hour)))
	}
	if _, err := os.Stat(countersPath); err != nil {
		t.Fatalf("seed should persist the counters file: %v", err)
	}

	// Rotate the audit log the way lumberjack does (current file becomes a
	// timestamped backup) and restart: the counters must not re-add history.
	rotated := filepath.Join(dir, "audit-2026-01-01T00-00-00.000.jsonl")
	if err := os.Rename(auditPath, rotated); err != nil {
		t.Fatal(err)
	}
	writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=2"},
	})
	b := loadedPRCounterLog(t, countersPath, auditPath)
	if got := b.PRThroughputCounters(); got.Opened != 1 || got.Merged != 1 {
		t.Fatalf("after rotation+restart counters = %+v, want unchanged opened=1 merged=1", got)
	}
}

func TestPRThroughputCountersCorruptFileReseeds(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	countersPath := filepath.Join(dir, "pr-throughput-counters.json")
	if err := os.WriteFile(countersPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	auditPath := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-time.Hour)), User: "system", Action: ghpkg.AuditActionPRClosed, Detail: "repo=o/r, number=1, path=relay"},
	})
	got := loadedPRCounterLog(t, countersPath, auditPath).PRThroughputCounters()
	if got.Closed != 1 {
		t.Fatalf("reseeded counters = %+v, want closed=1", got)
	}
	data, err := os.ReadFile(countersPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk PRThroughputCounters
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.Closed != 1 {
		t.Fatalf("counters file not rewritten: %q (%v)", data, err)
	}
}

func TestPRThroughputCountersPersistFailureKeepsMemory(t *testing.T) {
	// A counters path in a missing directory: writes fail, counts stay live.
	a := loadedPRCounterLog(t, filepath.Join(t.TempDir(), "missing", "c.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	a.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=1", "scanner")
	if got := a.PRThroughputCounters().Opened; got != 1 {
		t.Fatalf("opened = %d, want 1 despite persistence failure", got)
	}
}

func TestPRThroughputMergePath(t *testing.T) {
	tests := []struct {
		detail string
		want   string
	}{
		{"repo=o/r, number=1, method=squash, sha=abc, path=sweep", "sweep"},
		{"repo=o/r, number=1, path=queue", "sweep"},
		{"path=relay", "relay"},
		{"repo=o/r, number=1, method=squash", prThroughputMergePathUnknown},
		{"path=", prThroughputMergePathUnknown},
		{"", prThroughputMergePathUnknown},
	}
	for _, tt := range tests {
		if got := prThroughputMergePath(tt.detail); got != tt.want {
			t.Errorf("prThroughputMergePath(%q) = %q, want %q", tt.detail, got, tt.want)
		}
	}
}

func TestPRThroughputActorAttribution(t *testing.T) {
	tests := []struct {
		name               string
		entry              AuditEntry
		wantKind, wantRole string
		wantActor          string
	}{
		{"hive created PR", AuditEntry{Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=1, agent=scanner"}, prThroughputKindPR, prThroughputRoleCreated, prThroughputActorHive},
		{"hive created issue", AuditEntry{Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=o/r, number=2"}, prThroughputKindIssue, prThroughputRoleCreated, prThroughputActorHive},
		{"human merged PR", AuditEntry{Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=3, path=human, actor=alice"}, prThroughputKindPR, prThroughputRoleMerged, prThroughputActorHuman},
		{"other automation merged PR", AuditEntry{Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=4, path=other_automation, actor=renovate[bot]"}, prThroughputKindPR, prThroughputRoleMerged, prThroughputActorOtherAutomation},
		{"hive reviewed PR", AuditEntry{Action: ghpkg.AuditActionPRReviewed, Detail: "repo=o/r, number=5, state=approved, agent=reviewer"}, prThroughputKindPR, prThroughputRoleReviewed, prThroughputActorHive},
		{"bot co-authored still hive", AuditEntry{Action: ghpkg.AuditActionPRReviewed, Detail: "repo=o/r, number=6, state=commented, agent=reviewer, actor=hive"}, prThroughputKindPR, prThroughputRoleReviewed, prThroughputActorHive},
		{"issue commented by agent is review", AuditEntry{Action: ghpkg.AuditActionAgentCommentCreated, Detail: "repo=o/r, number=7, agent=scanner"}, prThroughputKindIssue, prThroughputRoleReviewed, prThroughputActorHive},
		{"issue closed by hive", AuditEntry{Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=8, reason=done"}, prThroughputKindIssue, prThroughputRoleClosed, prThroughputActorHive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, role, actor, ok := prThroughputActorAttribution(tt.entry)
			if !ok || kind != tt.wantKind || role != tt.wantRole || actor != tt.wantActor {
				t.Fatalf("attribution = %q/%q/%q ok=%v, want %q/%q/%q", kind, role, actor, ok, tt.wantKind, tt.wantRole, tt.wantActor)
			}
		})
	}
}

func TestPRThroughputObservedHumanMergeCountsOnce(t *testing.T) {
	a := &AuditLog{}
	a.LogRecordAt("2026-10-02T12:00:00Z", "system", ghpkg.AuditActionPRMerged, "repo=o/r, number=11, path=human, actor=alice", "governor", "o/r", 11)

	got := a.PRThroughputCounters()
	if got.Merged != 1 || got.MergedByPath["human"] != 1 {
		t.Fatalf("observed human merge counters = %+v, want merged=1 human=1", got)
	}
}

func TestPRThroughputSweepThenObservedMergeDedupes(t *testing.T) {
	a := &AuditLog{}
	a.LogRecordAt("2026-10-02T12:00:00Z", "system", ghpkg.AuditActionPRMerged, "repo=o/r, number=12, path=sweep", "governor", "o/r", 12)
	a.LogRecordAt("2026-10-02T12:01:00Z", "system", ghpkg.AuditActionPRMerged, "repo=o/r, number=12, path=other_automation, actor=hive[bot]", "governor", "o/r", 12)

	got := a.PRThroughputCounters()
	if got.Merged != 1 || got.MergedByPath["sweep"] != 1 || got.MergedByPath["other_automation"] != 0 {
		t.Fatalf("deduped merge counters = %+v, want one sweep merge", got)
	}
	window := buildPRThroughputWindow(a.RecentWithPrefixSince(time.Time{}, ""), time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC), 24, "", prThroughputRoleMerged)
	if window.Merged != 1 || window.MergedByPath["sweep"] != 1 {
		t.Fatalf("deduped window = %+v, want one sweep merge", window)
	}
}

// ── #9431: GET /api/pr-throughput ───────────────────────────────────────────

func prThroughputFixtureEntries(now time.Time) []AuditEntry {
	return []AuditEntry{
		{Timestamp: rfc3339(now.Add(-30 * time.Minute)), User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(now.Add(-40 * time.Minute)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=2, path=sweep"},
		{Timestamp: rfc3339(now.Add(-5 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=3, path=relay"},
		{Timestamp: rfc3339(now.Add(-6 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRClosed, Detail: "repo=o/r, number=4, path=relay"},
		{Timestamp: rfc3339(now.Add(-20 * time.Hour)), User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=5"},
		{Timestamp: rfc3339(now.Add(-72 * time.Hour)), User: "system", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=6"},
		// Noise and malformed entries never count.
		{Timestamp: rfc3339(now.Add(-10 * time.Minute)), User: "system", Action: "agent_issue_created", Detail: "repo=o/r, number=7"},
		{Timestamp: "yesterday", User: "system", Action: ghpkg.AuditActionAgentPRCreated, Detail: "bad ts"},
	}
}

func TestBuildPRThroughputWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	entries := prThroughputFixtureEntries(now)
	tests := []struct {
		hours                  int
		opened, merged, closed int
		byPath                 map[string]int
	}{
		{hours: 1, opened: 1, merged: 1, closed: 0, byPath: map[string]int{"sweep": 1}},
		{hours: 12, opened: 1, merged: 2, closed: 1, byPath: map[string]int{"sweep": 1, "relay": 1}},
		{hours: 24, opened: 2, merged: 2, closed: 1, byPath: map[string]int{"sweep": 1, "relay": 1}},
		{hours: 168, opened: 2, merged: 3, closed: 1, byPath: map[string]int{"sweep": 1, "relay": 1, prThroughputMergePathUnknown: 1}},
	}
	for _, tt := range tests {
		got := buildPRThroughputWindow(entries, now, tt.hours, "", prThroughputRoleMerged)
		if got.Hours != tt.hours || got.AllTime || got.Source != prThroughputSourceAudit {
			t.Errorf("hours=%d: envelope = %+v", tt.hours, got)
		}
		if got.Opened != tt.opened || got.Merged != tt.merged || got.Closed != tt.closed {
			t.Errorf("hours=%d: opened/merged/closed = %d/%d/%d, want %d/%d/%d",
				tt.hours, got.Opened, got.Merged, got.Closed, tt.opened, tt.merged, tt.closed)
		}
		if len(got.MergedByPath) != len(tt.byPath) {
			t.Errorf("hours=%d: merged_by_path = %v, want %v", tt.hours, got.MergedByPath, tt.byPath)
		}
		for k, v := range tt.byPath {
			if got.MergedByPath[k] != v {
				t.Errorf("hours=%d: merged_by_path[%s] = %d, want %d", tt.hours, k, got.MergedByPath[k], v)
			}
		}
		if got.RecordedSince != rfc3339(now.Add(-72*time.Hour)) {
			t.Errorf("hours=%d: recorded_since = %q, want oldest PR event", tt.hours, got.RecordedSince)
		}
		if got.Since != rfc3339(now.Add(-time.Duration(tt.hours)*time.Hour)) {
			t.Errorf("hours=%d: since = %q", tt.hours, got.Since)
		}
	}

	empty := buildPRThroughputWindow(nil, now, 1, "", prThroughputRoleMerged)
	if empty.RecordedSince != "" || empty.MergedByPath == nil {
		t.Errorf("empty window = %+v, want no recorded_since and a non-nil map", empty)
	}
}

func TestBuildPRThroughputAllTime(t *testing.T) {
	got := buildPRThroughputAllTime(PRThroughputCounters{
		Opened: 10, Merged: 7, Closed: 2, Since: "2026-01-01T00:00:00Z",
		MergedByPath: map[string]int{"sweep": 4, "relay": 3},
	}, nil, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "", prThroughputRoleMerged)
	if !got.AllTime || got.Hours != 0 || got.Source != prThroughputSourceCounters {
		t.Fatalf("envelope = %+v", got)
	}
	if got.Opened != 10 || got.Merged != 7 || got.Closed != 2 || got.MergedByPath["sweep"] != 4 || got.MergedByPath["relay"] != 3 {
		t.Fatalf("counts = %+v", got)
	}
	if got.Since != "2026-01-01T00:00:00Z" || got.RecordedSince != "2026-01-01T00:00:00Z" {
		t.Fatalf("since/recorded_since = %q/%q", got.Since, got.RecordedSince)
	}
	if empty := buildPRThroughputAllTime(PRThroughputCounters{}, nil, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "", prThroughputRoleMerged); empty.MergedByPath == nil {
		t.Error("merged_by_path must be a non-nil map")
	}
}

func TestBuildPRThroughputAnalytics(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	entries := []AuditEntry{
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=O/R, number=1", Agent: "alpha"},
		{Timestamp: rfc3339(now.Add(-90 * time.Minute)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=1, path=sweep", Agent: "alpha"},
		{Timestamp: rfc3339(now.Add(-80 * time.Minute)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=2", Agent: "beta"},
		{Timestamp: rfc3339(now.Add(-70 * time.Minute)), Action: ghpkg.AuditActionPRClosed, Detail: "repo=o/r, number=2, reason=stale", Agent: "beta"},
		{Timestamp: rfc3339(now.Add(-7 * time.Hour)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=9, path=relay", Agent: "alpha"},
		{Timestamp: rfc3339(now.Add(-45 * time.Minute)), Action: ghpkg.AuditActionPRReviewed, Detail: "repo=o/r, number=1, state=approved", Agent: "reviewer"},
		{Timestamp: rfc3339(now.Add(-40 * time.Minute)), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=10, reason=done", Agent: "scanner"},
		{Timestamp: rfc3339(now.Add(-50 * time.Minute)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/other, number=3", Agent: "gamma"},
	}
	got := buildPRThroughputWindow(entries, now, 6, "o/r", prThroughputRoleMerged)
	if got.Opened != 2 || got.Merged != 1 || got.Closed != 1 {
		t.Fatalf("filtered counts = %d/%d/%d", got.Opened, got.Merged, got.Closed)
	}
	if got.BucketSeconds != int((15*time.Minute).Seconds()) || len(got.Buckets) == 0 {
		t.Fatalf("buckets = seconds %d len %d", got.BucketSeconds, len(got.Buckets))
	}
	if got.Metrics.MedianTimeToMerge == nil || *got.Metrics.MedianTimeToMerge != 1.5 {
		t.Fatalf("median TTM = %v, want 1.5", got.Metrics.MedianTimeToMerge)
	}
	if got.Metrics.P90TimeToMerge == nil || *got.Metrics.P90TimeToMerge != 1.5 {
		t.Fatalf("p90 TTM = %v, want 1.5", got.Metrics.P90TimeToMerge)
	}
	if got.Metrics.MergeRatio == nil || *got.Metrics.MergeRatio != 0.5 {
		t.Fatalf("merge ratio = %v, want .5", got.Metrics.MergeRatio)
	}
	if got.Metrics.NetBacklogChange != 0 {
		t.Fatalf("net backlog = %d, want 0", got.Metrics.NetBacklogChange)
	}
	if got.Metrics.Trend.MergedPct == nil || *got.Metrics.Trend.MergedPct != 0 {
		t.Fatalf("merged trend = %v, want 0", got.Metrics.Trend.MergedPct)
	}
	if len(got.MergedByAgent) != 1 || got.MergedByAgent[0].Name != "alpha" || got.MergedByAgent[0].Count != 1 {
		t.Fatalf("merged_by_agent = %+v", got.MergedByAgent)
	}
	if len(got.ClosedReasons) != 1 || got.ClosedReasons[0].Name != "stale" {
		t.Fatalf("closed_reasons = %+v", got.ClosedReasons)
	}
	if got.ByActor[prThroughputKindPR][prThroughputRoleReviewed][prThroughputActorHive] != 1 ||
		got.ByActor[prThroughputKindIssue][prThroughputRoleClosed][prThroughputActorHive] != 1 {
		t.Fatalf("by_actor = %+v, want PR review and issue close attribution", got.ByActor)
	}
	if got.SelectedRole != prThroughputRoleMerged || len(got.Series) == 0 {
		t.Fatalf("series shape = role %q len %d", got.SelectedRole, len(got.Series))
	}
}

func TestPRThroughputBucketsAlignAndEmpty(t *testing.T) {
	since := time.Date(2026, 10, 2, 12, 7, 0, 0, time.UTC)
	until := since.Add(20 * time.Minute)
	entries := []AuditEntry{{Timestamp: rfc3339(since.Add(time.Minute)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=1"}}
	buckets := prThroughputBuckets(entries, since, until, 5*time.Minute, "")
	if len(buckets) == 0 || buckets[0].T != "2026-10-02T12:05:00Z" {
		t.Fatalf("first bucket = %+v, want aligned 12:05", buckets)
	}
	if buckets[0].Opened != 1 {
		t.Fatalf("first bucket opened = %d, want 1", buckets[0].Opened)
	}
	if got := prThroughputBuckets(nil, until, since, 5*time.Minute, ""); got != nil {
		t.Fatalf("empty range buckets = %+v, want nil", got)
	}
}

func TestPRThroughputBucketsTimeToMergePercentiles(t *testing.T) {
	since := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	entries := []AuditEntry{
		{Timestamp: rfc3339(since.Add(-30 * time.Minute)), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(since.Add(5 * time.Minute)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(since), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=2"},
		{Timestamp: rfc3339(since.Add(20 * time.Minute)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=2"},
		{Timestamp: rfc3339(since), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=3"},
		{Timestamp: rfc3339(since.Add(25 * time.Minute)), Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/r, number=3"},
	}

	buckets := prThroughputBuckets(entries, since, until, 15*time.Minute, "")
	if len(buckets) < 2 {
		t.Fatalf("bucket count = %d, want at least 2", len(buckets))
	}
	if buckets[0].TTMP50 == nil || *buckets[0].TTMP50 < 0.58 || *buckets[0].TTMP50 > 0.59 {
		t.Fatalf("bucket 0 p50 = %v, want about 0.58h", buckets[0].TTMP50)
	}
	if buckets[1].TTMP50 == nil || *buckets[1].TTMP50 < 0.37 || *buckets[1].TTMP50 > 0.38 {
		t.Fatalf("bucket 1 p50 = %v, want about 0.375h", buckets[1].TTMP50)
	}
	if buckets[1].TTMP90 == nil || *buckets[1].TTMP90 < 0.40 || *buckets[1].TTMP90 > 0.42 {
		t.Fatalf("bucket 1 p90 = %v, want about 0.408h", buckets[1].TTMP90)
	}
}

func TestPRThroughputPercentilesAndEmptyTrend(t *testing.T) {
	if percentileHours(nil, 0.5) != nil {
		t.Fatal("empty percentile must be nil")
	}
	one := percentileHours([]float64{2}, 0.9)
	if one == nil || *one != 2 {
		t.Fatalf("one-sample percentile = %v", one)
	}
	twoMedian := percentileHours([]float64{1, 3}, 0.5)
	if twoMedian == nil || *twoMedian != 2 {
		t.Fatalf("two-sample median = %v", twoMedian)
	}
	twoP90 := percentileHours([]float64{1, 3}, 0.9)
	if twoP90 == nil || *twoP90 < 2.79 || *twoP90 > 2.81 {
		t.Fatalf("two-sample p90 = %v", twoP90)
	}
	if pctChange(5, 0) != nil {
		t.Fatal("trend from empty previous window must be nil")
	}
}

func TestPRThroughputAllTimeRepoCounters(t *testing.T) {
	var counters PRThroughputCounters
	counters.add(AuditEntry{Timestamp: "2026-01-01T00:00:00Z", Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=O/R, number=1"})
	counters.add(AuditEntry{Timestamp: "2026-01-02T00:00:00Z", Action: ghpkg.AuditActionPRMerged, Detail: "repo=o/other, number=2, path=relay"})
	got := buildPRThroughputAllTime(counters, nil, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), "o/r", prThroughputRoleMerged)
	if got.Opened != 1 || got.Merged != 0 || got.Repo != "o/r" {
		t.Fatalf("repo all-time = %+v", got)
	}
}

func prThroughputServer(t *testing.T, auditPath string, audit *AuditLog) *Server {
	t.Helper()
	prev := prThroughputAuditPath
	prThroughputAuditPath = auditPath
	t.Cleanup(func() { prThroughputAuditPath = prev })
	return &Server{audit: audit}
}

func getPRThroughput(t *testing.T, s *Server, query string) (int, PRThroughput) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/pr-throughput"+query, nil)
	rec := httptest.NewRecorder()
	s.handlePRThroughput(rec, req)
	var body PRThroughput
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not JSON: %v; body=%q", err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestHandlePRThroughput(t *testing.T) {
	now := time.Now().UTC()
	auditPath := writeAuditFixture(t, t.TempDir(), prThroughputFixtureEntries(now))
	s := prThroughputServer(t, auditPath, &AuditLog{})

	tests := []struct {
		query     string
		wantCode  int
		wantHours int
		wantOpen  int
	}{
		{query: "", wantCode: http.StatusOK, wantHours: prThroughputDefaultHours, wantOpen: 2},
		{query: "?hours=1", wantCode: http.StatusOK, wantHours: 1, wantOpen: 1},
		{query: "?hours=99999", wantCode: http.StatusOK, wantHours: prThroughputMaxHours, wantOpen: 2},
		{query: "?hours=-1", wantCode: http.StatusBadRequest},
		{query: "?hours=abc", wantCode: http.StatusBadRequest},
		{query: "?repo=not-a-full-name", wantCode: http.StatusBadRequest},
	}
	for _, tt := range tests {
		code, body := getPRThroughput(t, s, tt.query)
		if code != tt.wantCode {
			t.Errorf("%q: status = %d, want %d", tt.query, code, tt.wantCode)
			continue
		}
		if code != http.StatusOK {
			continue
		}
		if body.Hours != tt.wantHours || body.Opened != tt.wantOpen || body.Source != prThroughputSourceAudit {
			t.Errorf("%q: body = %+v, want hours=%d opened=%d source=audit", tt.query, body, tt.wantHours, tt.wantOpen)
		}
	}
}

func TestHandlePRThroughputAllTimeUsesCounters(t *testing.T) {
	audit := &AuditLog{}
	audit.Log("system", ghpkg.AuditActionAgentPRCreated, "repo=o/r, number=1", "scanner")
	audit.Log("system", ghpkg.AuditActionPRMerged, "repo=o/r, number=1, path=queue", "governor")
	s := prThroughputServer(t, filepath.Join(t.TempDir(), "audit.jsonl"), audit)

	code, body := getPRThroughput(t, s, "?hours=0")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !body.AllTime || body.Source != prThroughputSourceCounters || body.Opened != 1 || body.Merged != 1 || body.MergedByPath["sweep"] != 1 {
		t.Fatalf("all-time body = %+v", body)
	}
	if body.ByActor[prThroughputKindPR][prThroughputRoleMerged][prThroughputActorHive] != 1 || body.SelectedRole != prThroughputRoleMerged {
		t.Fatalf("all-time actor shape = role %q by_actor %+v", body.SelectedRole, body.ByActor)
	}
	if body.RecordedSince == "" {
		t.Error("all-time response must say how far back the counters go")
	}
}

// With no audit file (no /data volume) the windowed counts fall back to the
// in-memory ring.
func TestHandlePRThroughputFallsBackToRing(t *testing.T) {
	audit := &AuditLog{}
	audit.Log("system", ghpkg.AuditActionPRClosed, "repo=o/r, number=1, path=relay", "scanner")
	audit.Log("alice", "config.save", "file=hive.yaml", "")
	s := prThroughputServer(t, filepath.Join(t.TempDir(), "absent.jsonl"), audit)

	code, body := getPRThroughput(t, s, "?hours=1")
	if code != http.StatusOK || body.Closed != 1 || body.Opened != 0 || body.Merged != 0 {
		t.Fatalf("ring fallback: status=%d body=%+v, want closed=1", code, body)
	}
}

func TestHandlePRThroughputNilAudit(t *testing.T) {
	s := prThroughputServer(t, filepath.Join(t.TempDir(), "absent.jsonl"), nil)
	code, body := getPRThroughput(t, s, "?hours=0")
	if code != http.StatusOK || !body.AllTime || body.Opened != 0 {
		t.Fatalf("nil audit: status=%d body=%+v", code, body)
	}
}
