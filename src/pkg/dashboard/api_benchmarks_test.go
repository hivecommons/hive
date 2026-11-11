package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func writeBenchmarkFixtureBundle(t *testing.T, root string, b *evidence.Bundle) {
	t.Helper()
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, filepath.FromSlash(b.Repo), "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(dir, b.HeadSHA+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func benchmarksTestServer(t *testing.T, publish bool) *Server {
	t.Helper()
	root := t.TempDir()
	prev := ghpkg.ReviewEvidenceRoot
	ghpkg.ReviewEvidenceRoot = root
	t.Cleanup(func() { ghpkg.ReviewEvidenceRoot = prev })
	now := time.Now().UTC().Add(-time.Hour)
	writeBenchmarkFixtureBundle(t, root, &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, Repo: "o/r", Number: 1, HeadSHA: "abc1234",
		Author:    evidence.Author{Login: "bot", Kind: evidence.AuthorAgent, Model: "claude-fable-5.1", Backend: "copilot", Runtime: "podman", TaskKind: "docs"},
		CreatedAt: now, UpdatedAt: now,
		Merge: &evidence.MergeEvent{Actor: "hive", Method: "squash", At: now.Add(30 * time.Minute), OpenedAt: now},
	})
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Benchmarks: config.BenchmarksConfig{Publish: &publish}}}
	return s
}

func TestBenchmarksExportEmitsSchemaRecordsAndHonoursSince(t *testing.T) {
	s := benchmarksTestServer(t, true)
	rec := httptest.NewRecorder()
	s.handleBenchmarksExport(rec, httptest.NewRequest(http.MethodGet, "/api/benchmarks/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		HiveHash string              `json:"hive_hash"`
		Count    int                 `json:"count"`
		Records  []benchmarks.Record `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.HiveHash != benchmarks.HiveHash("h1") || resp.Count != 1 {
		t.Fatalf("resp=%+v", resp)
	}
	r := resp.Records[0]
	if r.Source.HiveID != resp.HiveHash || r.Model.Name != "claude-fable-5.1" || r.SandboxRuntime != "podman" || r.Task.Kind != "docs" || !r.Outcome.PRMerged {
		t.Errorf("record=%+v", r)
	}
	if r.Task.Repo != "" {
		t.Errorf("repo of unknown visibility must be anonymized, got %q", r.Task.Repo)
	}

	rec = httptest.NewRecorder()
	s.handleBenchmarksExport(rec, httptest.NewRequest(http.MethodGet, "/api/benchmarks/export?since="+time.Now().UTC().Format(time.RFC3339), nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Count != 0 {
		t.Errorf("since in the future should yield 0 records: count=%d err=%v", resp.Count, err)
	}
	rec = httptest.NewRecorder()
	s.handleBenchmarksExport(rec, httptest.NewRequest(http.MethodGet, "/api/benchmarks/export?since=yesterday", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad since: code=%d", rec.Code)
	}
}

func TestBenchmarksExportRespectsPublishOptOut(t *testing.T) {
	s := benchmarksTestServer(t, false)
	rec := httptest.NewRecorder()
	s.handleBenchmarksExport(rec, httptest.NewRequest(http.MethodGet, "/api/benchmarks/export", nil))
	var resp struct {
		Publish bool `json:"publish"`
		Count   int  `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("code=%d err=%v", rec.Code, err)
	}
	if resp.Publish || resp.Count != 0 {
		t.Errorf("opt-out must export nothing: %+v", resp)
	}
	if st := s.BenchmarksStatusBlock(); st == nil || st.Publish || st.HiveHash != benchmarks.HiveHash("h1") {
		t.Errorf("status block = %+v", st)
	}
}
