package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence/benchmarks"
)

func useTempBenchmarksStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldDir, oldState := benchmarksStoreDir, benchmarksStatePath
	benchmarksStoreDir = filepath.Join(dir, "benchmarks")
	benchmarksStatePath = filepath.Join(dir, "state.json")
	hubBenchmarks.mu.Lock()
	hubBenchmarks.pending = map[string]bool{}
	hubBenchmarks.state = BenchmarksPublishState{}
	hubBenchmarks.loaded = true
	hubBenchmarks.mu.Unlock()
	t.Cleanup(func() {
		benchmarksStoreDir, benchmarksStatePath = oldDir, oldState
		hubBenchmarks.mu.Lock()
		hubBenchmarks.pending = map[string]bool{}
		hubBenchmarks.state = BenchmarksPublishState{}
		hubBenchmarks.loaded = false
		hubBenchmarks.mu.Unlock()
	})
	return dir
}

func benchRecord(hash, id, mergedAt string) benchmarks.Record {
	return benchmarks.Record{
		SchemaVersion: benchmarks.SchemaVersion, ID: id, PublishedAt: "2026-11-11T00:00:00Z",
		Source:         benchmarks.Source{HiveID: hash, HiveVersion: "v5"},
		Agent:          benchmarks.Agent{Name: "a", Role: "author", Harness: "copilot"},
		Model:          benchmarks.Model{Provider: "anthropic", Name: "claude-fable-5.1"},
		SandboxRuntime: "none", Task: benchmarks.Task{Kind: "docs"},
		Outcome: benchmarks.Outcome{PROpened: true, PRMerged: mergedAt != "", MergedAt: mergedAt, CIStatusAtMerge: "unknown", ReviewVerdict: "none"},
	}
}

func benchIngest(s *HubServer, body string, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, benchmarksIngestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.handleBenchmarksIngest(rec, req)
	return rec
}

func TestBenchmarksIngestStoresByMonthAndRejectsForeignHash(t *testing.T) {
	useTempBenchmarksStore(t)
	s := npsTestHub("h1")
	hash := benchmarks.HiveHash("h1")
	payload := benchmarksIngestBody{HiveID: "h1", Records: []benchmarks.Record{
		benchRecord(hash, "aaa", "2026-10-02T00:00:00Z"),
		benchRecord(hash, "bbb", "2026-11-05T00:00:00Z"),
		benchRecord(benchmarks.HiveHash("h2"), "ccc", "2026-11-05T00:00:00Z"),
	}}
	body, _ := json.Marshal(payload)
	rec := benchIngest(s, string(body), s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted":2`) || !strings.Contains(rec.Body.String(), `"rejected":1`) {
		t.Errorf("response: %s", rec.Body.String())
	}
	for _, f := range []string{"2026-10.json", "2026-11.json"} {
		if _, err := os.Stat(filepath.Join(benchmarksStoreDir, hash, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(benchmarksStoreDir, benchmarks.HiveHash("h2"))); err == nil {
		t.Error("a record claiming another hive's hash was stored")
	}
	// Re-ingesting the same id merges rather than duplicates.
	benchIngest(s, string(body), s.heartbeatKeyFor("h1"))
	data, _ := os.ReadFile(filepath.Join(benchmarksStoreDir, hash, "2026-11.json"))
	var recs []benchmarks.Record
	_ = json.Unmarshal(data, &recs)
	if len(recs) != 1 {
		t.Errorf("want 1 record after re-ingest, got %d", len(recs))
	}
	hubBenchmarks.mu.Lock()
	pending := len(hubBenchmarks.pending)
	hubBenchmarks.mu.Unlock()
	if pending != 2 {
		t.Errorf("pending files = %d, want 2", pending)
	}

	// Status reflects the store.
	st := httptest.NewRecorder()
	s.handleBenchmarksStatus(st, httptest.NewRequest(http.MethodGet, benchmarksStatusPath, nil))
	var status BenchmarksPublishState
	_ = json.Unmarshal(st.Body.Bytes(), &status)
	if status.HiveCount != 1 || status.RecordCount != 2 || status.PendingFiles != 2 || status.Hives[hash] != 2 {
		t.Errorf("status = %+v", status)
	}
}

func TestBenchmarksIngestAuth(t *testing.T) {
	useTempBenchmarksStore(t)
	s := npsTestHub("h1", "h2")
	body := `{"hive_id":"h1","records":[]}`
	for name, bearer := range map[string]string{"none": "", "other hive": s.heartbeatKeyFor("h2"), "fleet key": s.heartbeatKey()} {
		if rec := benchIngest(s, body, bearer); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d want 401", name, rec.Code)
		}
	}
	if rec := benchIngest(s, `{"hive_id":"h3","records":[]}`, s.heartbeatKeyFor("h3")); rec.Code != http.StatusForbidden {
		t.Errorf("unregistered hive: code=%d want 403", rec.Code)
	}
}

func TestPublishBenchmarksCommitsPendingFiles(t *testing.T) {
	useTempBenchmarksStore(t)
	hash := benchmarks.HiveHash("h1")
	changed, _, err := storeBenchmarkRecords(hash, []benchmarks.Record{benchRecord(hash, "aaa", "2026-11-05T00:00:00Z")})
	if err != nil || len(changed) != 1 {
		t.Fatalf("store: %v %v", changed, err)
	}
	hubBenchmarks.mu.Lock()
	hubBenchmarks.pending[changed[0]] = true
	hubBenchmarks.mu.Unlock()

	var puts atomic.Int32
	var gotPath, gotSHA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": "oldsha", "content": base64.StdEncoding.EncodeToString([]byte("[]"))})
		case http.MethodPut:
			puts.Add(1)
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			gotPath = r.URL.Path
			gotSHA, _ = p["sha"].(string)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	oldAPI, oldTok := benchmarksGitHubAPI, hubGitHubAuthTokenForRequest
	benchmarksGitHubAPI = srv.URL
	hubGitHubAuthTokenForRequest = func(ctx context.Context) string { return "tok" }
	t.Cleanup(func() { benchmarksGitHubAPI, hubGitHubAuthTokenForRequest = oldAPI, oldTok })
	t.Setenv(benchmarksRepoEnv, "example/data")

	s := newTestHubServer("secret")
	s.publishBenchmarks(t.Context())
	if puts.Load() != 1 {
		t.Fatalf("puts = %d, want 1", puts.Load())
	}
	if want := "/repos/example/data/contents/data/fleet/" + hash + "/2026-11.json"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if gotSHA != "oldsha" {
		t.Errorf("update must carry the existing blob sha, got %q", gotSHA)
	}
	hubBenchmarks.mu.Lock()
	defer hubBenchmarks.mu.Unlock()
	if len(hubBenchmarks.pending) != 0 || hubBenchmarks.state.LastPublishFiles != 1 || hubBenchmarks.state.LastPublishAt == "" {
		t.Errorf("state after publish: pending=%v state=%+v", hubBenchmarks.pending, hubBenchmarks.state)
	}
}

func TestValidBenchmarkRel(t *testing.T) {
	for rel, ok := range map[string]bool{"0123456789abcdef/2026-11.json": true, "../x/2026-11.json": false, "0123456789abcdef/evil.json": false, "0123456789abcdef/2026-11.json/..": false} {
		if validBenchmarkRel(rel) != ok {
			t.Errorf("%q: want %v", rel, ok)
		}
	}
}

func TestBenchmarksIngestRejectsMalformedBodies(t *testing.T) {
	useTempBenchmarksStore(t)
	s := npsTestHub("h1")
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":       {"{", http.StatusBadRequest},
		"bad hive id":    {`{"hive_id":"../x","records":[]}`, http.StatusBadRequest},
		"too large":      {`{"hive_id":"h1","records":[` + strings.Repeat(`{},`, benchmarksIngestMaxBodyBytes/3) + `{}]}`, http.StatusRequestEntityTooLarge},
		"publish is off": {`{"hive_id":"h1","records":[]}`, http.StatusAccepted},
	} {
		if rec := benchIngest(s, tc.body, s.heartbeatKeyFor("h1")); rec.Code != tc.want {
			t.Errorf("%s: code=%d want %d (%s)", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	nosecret := newTestHubServer("")
	if rec := benchIngest(nosecret, `{"hive_id":"h1","records":[]}`, "x"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no hub secret: code=%d", rec.Code)
	}
}

func TestBenchmarksStatePersistsAcrossLoad(t *testing.T) {
	useTempBenchmarksStore(t)
	hash := benchmarks.HiveHash("h1")
	changed, _, err := storeBenchmarkRecords(hash, []benchmarks.Record{benchRecord(hash, "aaa", "2026-11-05T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	hubBenchmarks.mu.Lock()
	hubBenchmarks.pending[changed[0]] = true
	hubBenchmarks.state.LastIngestAt = "2026-11-11T00:00:00Z"
	hubBenchmarks.save()
	hubBenchmarks.pending = map[string]bool{}
	hubBenchmarks.state = BenchmarksPublishState{}
	hubBenchmarks.loaded = false
	hubBenchmarks.load()
	defer hubBenchmarks.mu.Unlock()
	if !hubBenchmarks.pending[changed[0]] || hubBenchmarks.state.LastIngestAt != "2026-11-11T00:00:00Z" {
		t.Fatalf("state did not round-trip: pending=%v state=%+v", hubBenchmarks.pending, hubBenchmarks.state)
	}
}

func TestPublishBenchmarksRecordsMissingIdentity(t *testing.T) {
	useTempBenchmarksStore(t)
	hash := benchmarks.HiveHash("h1")
	changed, _, _ := storeBenchmarkRecords(hash, []benchmarks.Record{benchRecord(hash, "aaa", "")})
	hubBenchmarks.mu.Lock()
	hubBenchmarks.pending[changed[0]] = true
	hubBenchmarks.mu.Unlock()
	oldTok := hubGitHubAuthTokenForRequest
	hubGitHubAuthTokenForRequest = func(ctx context.Context) string { return "" }
	t.Cleanup(func() { hubGitHubAuthTokenForRequest = oldTok })
	s := newTestHubServer("secret")
	s.publishBenchmarks(t.Context())
	hubBenchmarks.mu.Lock()
	defer hubBenchmarks.mu.Unlock()
	if hubBenchmarks.state.LastError == "" || len(hubBenchmarks.pending) != 1 {
		t.Fatalf("missing identity must be surfaced and keep files pending: %+v pending=%v", hubBenchmarks.state, hubBenchmarks.pending)
	}
	if benchmarkMonth(benchmarks.Record{}) != time.Now().UTC().Format("2006-01") {
		t.Error("record without timestamps must land in the current month")
	}
}
