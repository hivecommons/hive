package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/forgestate"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

type matrixIngestFakeCache struct {
	snaps map[string]forgestate.Snapshot
}

func (f matrixIngestFakeCache) Snapshot(repo string) (forgestate.Snapshot, bool) {
	s, ok := f.snaps[repo]
	return s, ok
}

// matrixIngestFakeGitHub serves one open PR (o/r#7 at head) with a one-line
// Go patch and counts the reads the ingest pass makes.
type matrixIngestFakeGitHub struct {
	mu    sync.Mutex
	head  string
	gets  int
	files int
}

func (g *matrixIngestFakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/repos/o/r/pulls/7/files":
		g.files++
		_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "main.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1 @@\n+package main\n"}})
	case p == "/repos/o/r/pulls/7":
		g.gets++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "state": "open", "title": "t",
			"user": map[string]string{"login": "hive-bot"},
			"head": map[string]string{"sha": g.head},
			"base": map[string]any{"repo": map[string]any{"private": false}},
		})
	case strings.HasPrefix(p, "/repos/o/r/issues/7/comments"), p == "/repos/o/r/issues/7/timeline":
		_ = json.NewEncoder(w).Encode([]any{})
	case p == "/user":
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "hive-bot"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *matrixIngestFakeGitHub) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets, g.files
}

func (g *matrixIngestFakeGitHub) setHead(h string) {
	g.mu.Lock()
	g.head = h
	g.mu.Unlock()
}

func matrixIngestTestConfig(active bool) *config.Config {
	cfg := &config.Config{}
	cfg.Dashboard.Matrix = config.DashboardMatrixConfig{Enabled: active, Repos: []string{"o/r"}}
	return cfg
}

// The incremental pass (#16276) is a no-op when dashboard.matrix is off,
// reads a new head once, costs nothing while heads are unchanged, and is
// gated to matrixIngestInterval between runs.
func TestRunMatrixIngest(t *testing.T) {
	fake := &matrixIngestFakeGitHub{head: "aaa111"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ghClient := hgithub.NewClientForTest(srv.URL, "o", []string{"o/r"}, logger)
	cache := matrixIngestFakeCache{snaps: map[string]forgestate.Snapshot{
		"o/r": {Repo: "o/r", Ready: true, PRs: map[int]forgestate.Item{
			7: {Kind: forgestate.KindPR, Number: 7, State: "open", HeadSHA: "aaa111"},
		}},
	}}
	hgithub.SetSentinelPRCache(cache)
	t.Cleanup(func() { hgithub.SetSentinelPRCache(nil) })
	dashSrv := dashboard.NewServer(0, logger)
	cfg := matrixIngestTestConfig(true)
	dashSrv.RegisterAPI(&dashboard.Dependencies{Config: cfg})
	t.Cleanup(dashSrv.Close)
	ctx := context.Background()
	st := &matrixIngestState{}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

	if ran, _ := runMatrixIngest(ctx, nil, matrixIngestTestConfig(true), dashSrv, st, now, logger); ran {
		t.Fatal("nil client ran a pass")
	}
	if ran, _ := runMatrixIngest(ctx, ghClient, matrixIngestTestConfig(false), dashSrv, st, now, logger); ran {
		t.Fatal("inactive matrix ran a pass")
	}
	if gets, files := fake.counts(); gets != 0 || files != 0 {
		t.Fatalf("inactive matrix touched GitHub: gets=%d files=%d", gets, files)
	}

	if ran, cached := runMatrixIngest(ctx, ghClient, cfg, dashSrv, st, now, logger); !ran || cached != 1 {
		t.Fatalf("active matrix: ran=%v cached=%d, want a pass over 1 cached repo", ran, cached)
	}
	if gets, files := fake.counts(); gets != 1 || files != 1 {
		t.Fatalf("new head read once: gets=%d files=%d", gets, files)
	}
	lines, _ := matrixLinesForTest(t, dashSrv)
	if lines != 1 {
		t.Fatalf("matrix lines = %d, want 1", lines)
	}

	// Inside the interval the gate holds; past it an unchanged head is free.
	if ran, _ := runMatrixIngest(ctx, ghClient, cfg, dashSrv, st, now.Add(matrixIngestInterval/2), logger); ran {
		t.Fatal("pass ran inside matrixIngestInterval")
	}
	if ran, _ := runMatrixIngest(ctx, ghClient, cfg, dashSrv, st, now.Add(matrixIngestInterval), logger); !ran {
		t.Fatal("pass did not run after matrixIngestInterval")
	}
	if gets, files := fake.counts(); gets != 1 || files != 1 {
		t.Fatalf("unchanged head cost GitHub calls: gets=%d files=%d", gets, files)
	}

	// The head moves: exactly one more read, one more line.
	fake.setHead("bbb222")
	cache.snaps["o/r"].PRs[7] = forgestate.Item{Kind: forgestate.KindPR, Number: 7, State: "open", HeadSHA: "bbb222"}
	if ran, _ := runMatrixIngest(ctx, ghClient, cfg, dashSrv, st, now.Add(2*matrixIngestInterval), logger); !ran {
		t.Fatal("pass did not run for the moved head")
	}
	if gets, files := fake.counts(); gets != 2 || files != 2 {
		t.Fatalf("moved head read once more: gets=%d files=%d", gets, files)
	}
	if _, last := matrixLinesForTest(t, dashSrv); last == "" || last == "null" {
		t.Fatalf("last_ingest_at not recorded: %q", last)
	}
}

// matrixLinesForTest reads /api/matrix/lines and returns the line count and
// the raw last_ingest_at value.
func matrixLinesForTest(t *testing.T, dashSrv *dashboard.Server) (int, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req := httptest.NewRequest(http.MethodGet, "/api/matrix/lines", nil)
		req.Header.Set("X-Hive-Role", config.RoleRead)
		rr := httptest.NewRecorder()
		dashSrv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("/api/matrix/lines: %d %s", rr.Code, rr.Body.String())
		}
		var body struct {
			Lines        []json.RawMessage `json:"lines"`
			LastIngestAt json.RawMessage   `json:"last_ingest_at"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The source parses observations on its own goroutine; poll the
		// handler until the line lands or the deadline passes.
		if len(body.Lines) > 0 || time.Now().After(deadline) {
			return len(body.Lines), string(body.LastIngestAt)
		}
		runtime.Gosched()
	}
}
