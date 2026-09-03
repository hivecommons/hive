package dashboard

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
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
	got := a.OutputActionsSince(now.Add(-24*time.Hour), collect.ActivityOutputActions, p)
	if len(got) != 2 {
		t.Fatalf("want 2 (pr_created + merged within 24h, output actions only), got %d: %+v", len(got), got)
	}
}

func TestOutputActionsSinceReadsRotatedCompressedBackups(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	current := writeAuditFixture(t, dir, []AuditEntry{
		{Timestamp: rfc3339(now.Add(-1 * time.Hour)), Action: "agent_pr_created", Detail: "repo=o/current, number=1"},
	})
	rotated := filepath.Join(dir, "audit-2026-08-26T12-00-00.000.jsonl.gz")
	f, err := os.Create(rotated)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if err := json.NewEncoder(gz).Encode(AuditEntry{
		Timestamp: rfc3339(now.Add(-2 * time.Hour)),
		Action:    "agent_issue_created",
		Detail:    "repo=o/rotated, number=2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got := (&AuditLog{}).OutputActionsSince(now.Add(-24*time.Hour), collect.ActivityOutputActions, current)
	if len(got) != 2 {
		t.Fatalf("want current + rotated entries, got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Detail] = true
	}
	if !seen["repo=o/current, number=1"] || !seen["repo=o/rotated, number=2"] {
		t.Fatalf("rotated/current details missing: %+v", seen)
	}
}

func TestHandleRepoActivityReportsPhaseOneHonesty(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	p := writeAuditFixture(t, dir, []AuditEntry{{
		Timestamp: rfc3339(now.Add(-1 * time.Hour)),
		Action:    "agent_pr_created",
		Detail:    "repo=o/r, number=1",
		Agent:     "quality",
	}})
	ac := collect.NewActivityCollector(&AuditLog{}, p, nil)
	// One up-front collect, no ticker: Start with an already-cancelled context
	// performs exactly one collect before observing ctx.Done. The collector's
	// clock is time.Now; the fixture entry sits 1h back, well inside the window.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ac.Start(ctx)

	// RegisterAPI wires ContributeWSHub, which logs unconditionally; a nil
	// logger panics in NewContributeWSHub → loadCompletedTasks.
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(&Dependencies{Activity: ac})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/repo-activity", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp repoActivityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Ready || resp.Phase != "phase_1_activity_only" {
		t.Fatalf("ready/phase = %v/%q", resp.Ready, resp.Phase)
	}
	if len(resp.Snapshot.Repos) != 1 || len(resp.Snapshot.Repos[0].Agents) != 1 {
		t.Fatalf("repo/agent activity missing: %+v", resp.Snapshot)
	}
	if len(resp.Limitations) == 0 {
		t.Fatal("limitations must be explicit so activity is not mistaken for cost")
	}
}
