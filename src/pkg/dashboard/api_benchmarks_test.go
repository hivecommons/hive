package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func writeBenchmarkFixtureBundle(t *testing.T, root string, b *evidence.Bundle) {
	t.Helper()
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, filepath.FromSlash(b.Repo), strconv.Itoa(b.Number))
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

func TestPushBenchmarksAdvancesWatermarkAndRecordsFailures(t *testing.T) {
	s := benchmarksTestServer(t, true)
	prevState := benchmarksPublishStatePath
	benchmarksPublishStatePath = filepath.Join(t.TempDir(), "benchmarks-publish.json")
	t.Cleanup(func() { benchmarksPublishStatePath = prevState })
	t.Setenv(spoke.EnvHeartbeatKey, "spoke-key")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var got struct {
		HiveID  string              `json:"hive_id"`
		Records []benchmarks.Record `json:"records"`
	}
	var bearer string
	status := http.StatusAccepted
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
	}))
	defer hub.Close()

	s.pushBenchmarks(t.Context(), hub.URL, logger)
	if bearer != "Bearer spoke-key" || got.HiveID != "h1" || len(got.Records) != 1 {
		t.Fatalf("push: bearer=%q hive=%q n=%d", bearer, got.HiveID, len(got.Records))
	}
	st := s.BenchmarksStatusBlock()
	if st.TotalPushed != 1 || st.LastPushAt == "" || st.WatermarkAt == "" || st.LastError != "" {
		t.Fatalf("status after push = %+v", st)
	}
	if _, err := os.Stat(benchmarksPublishStatePath); err != nil {
		t.Fatalf("watermark not persisted: %v", err)
	}

	// Nothing new since the watermark: no second request.
	got.Records = nil
	s.pushBenchmarks(t.Context(), hub.URL, logger)
	if got.Records != nil {
		t.Fatal("pushed again with nothing newer than the watermark")
	}

	// A hub error is surfaced and keeps the watermark where it was.
	s.benchmarks().state.Watermark = time.Time{}
	status = http.StatusInternalServerError
	s.pushBenchmarks(t.Context(), hub.URL, logger)
	if st := s.BenchmarksStatusBlock(); st.LastError == "" || st.TotalPushed != 1 {
		t.Fatalf("failure not recorded: %+v", st)
	}

	// Opt-out short-circuits before any request.
	off := false
	s.deps.Config.Benchmarks.Publish = &off
	bearer = ""
	s.pushBenchmarks(t.Context(), hub.URL, logger)
	if bearer != "" {
		t.Fatal("publish:false must not push")
	}
}

// benchmarksSeedMany seeds n merged agent PRs in all (one record each), one minute
// apart, and returns the server with the state file redirected to a temp dir.
func benchmarksSeedMany(t *testing.T, n int) *Server {
	t.Helper()
	s := benchmarksTestServer(t, true)
	prevState := benchmarksPublishStatePath
	benchmarksPublishStatePath = filepath.Join(t.TempDir(), "benchmarks-publish.json")
	t.Cleanup(func() { benchmarksPublishStatePath = prevState })
	t.Setenv(spoke.EnvHeartbeatKey, "spoke-key")
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := 2; i < n+1; i++ { // the base fixture is record 1
		ts := base.Add(time.Duration(i) * time.Minute)
		writeBenchmarkFixtureBundle(t, ghpkg.ReviewEvidenceRoot, &evidence.Bundle{
			SchemaVersion: evidence.SchemaVersion, Repo: "o/r", Number: i, HeadSHA: "sha" + strconv.Itoa(i),
			Author:    evidence.Author{Login: "bot", Kind: evidence.AuthorAgent, Model: "claude-fable-5.1", Backend: "copilot", Runtime: "podman", TaskKind: "docs"},
			CreatedAt: ts, UpdatedAt: ts,
			Merge: &evidence.MergeEvent{Actor: "hive", Method: "squash", At: ts.Add(time.Minute), OpenedAt: ts},
		})
	}
	return s
}

func benchmarksDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBenchmarksBatchEndSplitsByCountAndBytes(t *testing.T) {
	sizes := []int{1000, 1000, 1000, 1000, 1000}
	// Budget fits the envelope plus two records (each +1 comma).
	budget := benchmarksEnvelopeOverheadBytes + 2*1001
	if got := benchmarksBatchEnd(sizes, 0, 100, budget); got != 2 {
		t.Errorf("byte split: end=%d want 2", got)
	}
	if got := benchmarksBatchEnd(sizes, 2, 100, budget); got != 4 {
		t.Errorf("second batch: end=%d want 4", got)
	}
	if got := benchmarksBatchEnd(sizes, 0, 3, 1<<30); got != 3 {
		t.Errorf("count split: end=%d want 3", got)
	}
	// A record bigger than the whole budget still travels alone.
	if got := benchmarksBatchEnd([]int{budget * 2, 10}, 0, 100, budget); got != 1 {
		t.Errorf("oversized single: end=%d want 1", got)
	}
}

func TestPushBenchmarksHalvesBatchOn413AndKeepsProgress(t *testing.T) {
	const total, accepts = 8, 2
	s := benchmarksSeedMany(t, total)
	var sizes []int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got benchmarksIngestPayload
		_ = json.NewDecoder(r.Body).Decode(&got)
		sizes = append(sizes, len(got.Records))
		if len(got.Records) > accepts {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte("payload too large"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()

	s.pushBenchmarks(t.Context(), hub.URL, benchmarksDiscardLogger())
	st := s.BenchmarksStatusBlock()
	// 8 is refused, 4 is refused, then 2-record batches carry the rest.
	if st.TotalPushed != total || st.LastError != "" || st.WatermarkAt == "" {
		t.Fatalf("status = %+v (request sizes %v)", st, sizes)
	}
	if sizes[0] != total || sizes[1] != total/2 || sizes[2] != accepts {
		t.Errorf("request sizes %v: want halving 8,4,2 then 2-record batches", sizes)
	}
	// Nothing left: a second tick sends no request.
	n := len(sizes)
	s.pushBenchmarks(t.Context(), hub.URL, benchmarksDiscardLogger())
	if len(sizes) != n {
		t.Errorf("second tick pushed again: %v", sizes[n:])
	}
}

func TestPushBenchmarksSkipsSingleRecordThatAlwaysGets413(t *testing.T) {
	const total = 5
	s := benchmarksSeedMany(t, total)
	recs, _, err := s.benchmarksRecords(t.Context(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	sortRecs := append([]benchmarks.Record(nil), recs...)
	// Poison a record in the middle of the time order.
	for i := range sortRecs {
		for j := i + 1; j < len(sortRecs); j++ {
			if sortRecs[j].UpdatedAt.Before(sortRecs[i].UpdatedAt) {
				sortRecs[i], sortRecs[j] = sortRecs[j], sortRecs[i]
			}
		}
	}
	poison := sortRecs[2].ID
	accepted := map[string]bool{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got benchmarksIngestPayload
		_ = json.NewDecoder(r.Body).Decode(&got)
		for _, rec := range got.Records {
			if rec.ID == poison {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
		}
		for _, rec := range got.Records {
			accepted[rec.ID] = true
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer hub.Close()
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	s.pushBenchmarks(t.Context(), hub.URL, logger)
	if len(accepted) != total-1 || accepted[poison] {
		t.Fatalf("accepted %d records, poison accepted=%v", len(accepted), accepted[poison])
	}
	if n := strings.Count(logs.String(), "skipping record the hub rejects even alone"); n != 1 {
		t.Errorf("skip logged %d times, want once; logs=%s", n, logs.String())
	}
	st := s.BenchmarksStatusBlock()
	if st.LastError != "" || st.TotalPushed != total-1 {
		t.Fatalf("publisher wedged: %+v", st)
	}
	if got := s.benchmarks().watermark(); !got.Equal(sortRecs[total-1].UpdatedAt) {
		t.Errorf("watermark %v, want newest %v", got, sortRecs[total-1].UpdatedAt)
	}
}

func TestPushBenchmarksDoesNotDropRecordsWhenEveryPushGets413(t *testing.T) {
	s := benchmarksSeedMany(t, 6)
	requests := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte("<html>\n<body>413 Request Entity Too Large</body>\n</html>"))
	}))
	defer hub.Close()

	s.pushBenchmarks(t.Context(), hub.URL, benchmarksDiscardLogger())
	st := s.BenchmarksStatusBlock()
	if st.TotalPushed != 0 || st.WatermarkAt != "" {
		t.Fatalf("records dropped though nothing was accepted: %+v", st)
	}
	if !strings.Contains(st.LastError, "413") || !strings.Contains(st.LastError, "<body>413 Request Entity Too Large</body>") {
		t.Errorf("last_error = %q", st.LastError)
	}
	if limit := benchmarksPushMaxShrinkAttempts + benchmarksPushMaxUnprovenSkips*benchmarksPushMaxShrinkAttempts; requests > limit {
		t.Errorf("%d requests, want bounded by %d", requests, limit)
	}
}

func TestBenchmarksFailureIncludesTruncatedBody(t *testing.T) {
	s := benchmarksSeedMany(t, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream\n  said " + strings.Repeat("x", 5000) + " Bearer spoke-key"))
	}))
	defer hub.Close()

	s.pushBenchmarks(t.Context(), hub.URL, benchmarksDiscardLogger())
	got := s.BenchmarksStatusBlock().LastError
	if !strings.HasPrefix(got, "hub answered 502 Bad Gateway: upstream said xxx") {
		t.Errorf("last_error = %q", got)
	}
	if max := len("hub answered 502 Bad Gateway: ") + benchmarksErrBodyMaxBytes + len("..."); len(got) > max {
		t.Errorf("last_error is %d bytes, want <= %d", len(got), max)
	}
	if strings.Contains(got, "spoke-key") {
		t.Error("last_error leaks the bearer")
	}
}
