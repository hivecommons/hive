package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAuditFixture writes a JSONL audit file with the given entries.
func writeAuditFixture(t *testing.T, dir string, entries []AuditEntry) string {
	t.Helper()
	p := filepath.Join(dir, "audit.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// OutputActionsSince filters by action set and since-time, reading the file.
func TestOutputActionsSince(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	p := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=o/r, number=1"},
		{Timestamp: rfc3339(now.Add(-30 * time.Hour)), Action: "agent_pr_created", Detail: "repo=o/r, number=2"}, // too old
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "agent_start", Detail: "trigger=startup"},          // not an output action
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), Action: "pr_merged", Detail: "repo=o/r, number=3"},
	})
	a := &AuditLog{}
	got := a.OutputActionsSince(now.Add(-24*time.Hour), activityOutputActions, p)
	if len(got) != 2 {
		t.Fatalf("want 2 (pr_created + merged within 24h, output actions only), got %d: %+v", len(got), got)
	}
}

// collect groups by repo + action with counts and newest timestamps, across
// multiple repos — the multi-repo case that broke the hand-scraped counts.
func TestActivityCollector_CountsPerRepo(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	newest := now.Add(-10 * time.Minute)
	p := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-2 * time.Hour)), Action: "agent_issue_created", Detail: "repo=z/ui, number=1"},
		{Timestamp: rfc3339(newest), Action: "agent_issue_created", Detail: "repo=z/ui, number=2"},
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=z/api-server, number=9"},
		{Timestamp: rfc3339(now.Add(-3 * time.Hour)), Action: "pr_merged", Detail: "repo=z/api-server, number=8"},
		{Timestamp: rfc3339(now.Add(-4 * time.Hour)), Action: "agent_pr_reviewed", Detail: "repo=z/ui, number=5, state=approved"},
		{Timestamp: rfc3339(now.Add(-5 * time.Hour)), Action: "agent_issue_claimed", Detail: "repo=z/ui-framework, number=3"},
	})
	ac := NewActivityCollector(&AuditLog{}, p, nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()
	snap, ok := ac.Snapshot()
	if !ok {
		t.Fatal("snapshot not ready after collect")
	}
	if len(snap.Repos) != 3 {
		t.Fatalf("want 3 repos, got %d", len(snap.Repos))
	}
	byRepo := map[string]RepoActivity{}
	for _, r := range snap.Repos {
		byRepo[r.Repo] = r
	}
	if byRepo["z/ui"].Issues.Count != 2 {
		t.Errorf("z/ui issues = %d, want 2", byRepo["z/ui"].Issues.Count)
	}
	if byRepo["z/ui"].Issues.NewestAt != rfc3339(newest) {
		t.Errorf("z/ui newest issue = %q, want %q", byRepo["z/ui"].Issues.NewestAt, rfc3339(newest))
	}
	if byRepo["z/ui"].Reviews.Count != 1 {
		t.Errorf("z/ui reviews = %d, want 1", byRepo["z/ui"].Reviews.Count)
	}
	if byRepo["z/api-server"].PRs.Count != 1 || byRepo["z/api-server"].Merges.Count != 1 {
		t.Errorf("z/api-server prs/merges = %d/%d, want 1/1", byRepo["z/api-server"].PRs.Count, byRepo["z/api-server"].Merges.Count)
	}
	if byRepo["z/ui-framework"].Claims.Count != 1 {
		t.Errorf("z/ui-framework claims = %d, want 1", byRepo["z/ui-framework"].Claims.Count)
	}
	if snap.WindowHours != activityHealthWindowHours {
		t.Errorf("window hours = %d, want %d", snap.WindowHours, activityHealthWindowHours)
	}
}

// Entries with no repo= are skipped (not attributable output).
func TestActivityCollector_SkipsNoRepo(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	p := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "advisory_commented", Detail: "flow=advisory-digest"}, // no repo=
	})
	ac := NewActivityCollector(&AuditLog{}, p, nil)
	ac.nowFn = func() time.Time { return now }
	ac.collect()
	snap, _ := ac.Snapshot()
	if len(snap.Repos) != 0 {
		t.Errorf("entry without repo= must be skipped, got %+v", snap.Repos)
	}
}

// EnablePersistence round-trips a snapshot across a restart.
func TestActivityCollector_PersistRestore(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	p := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=o/r, number=1"},
	})
	sidecar := filepath.Join(dir, "activity.json")

	ac := NewActivityCollector(&AuditLog{}, p, nil)
	ac.nowFn = func() time.Time { return now }
	ac.EnablePersistence(sidecar)
	ac.collect()
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}

	// Fresh collector with NO audit file readable but the sidecar present →
	// restores the prior snapshot.
	ac2 := NewActivityCollector(&AuditLog{}, filepath.Join(dir, "gone.jsonl"), nil)
	ac2.EnablePersistence(sidecar)
	snap, ok := ac2.Snapshot()
	if !ok || len(snap.Repos) != 1 || snap.Repos[0].PRs.Count != 1 {
		t.Errorf("restore failed: ok=%v snap=%+v", ok, snap)
	}
}

// Start is inert with a nil audit reader (no panic, returns).
func TestActivityCollector_NilAuditInert(t *testing.T) {
	ac := NewActivityCollector(nil, "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ac.Start(ctx) // must return immediately
	if _, ok := ac.Snapshot(); ok {
		t.Error("nil-audit collector must not be ready")
	}
}
