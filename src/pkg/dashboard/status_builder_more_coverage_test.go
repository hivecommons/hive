package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func covH2Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestCovH2_StatsConfigLoaders covers loadStatsConfig / LoadStatsConfigWithCfg
// / resolveStatsSources with the file-absent (default) path plus config fallback.
func TestCovH2_StatsConfigLoaders(t *testing.T) {
	// No /data/agents/<name>/stats.json in the sandbox → default config path.
	if got := loadStatsConfig("scanner"); len(got) == 0 {
		t.Fatalf("loadStatsConfig(scanner) should return defaults")
	}

	deps := testDeps(t)
	cfg := deps.Config

	// LoadStatsConfigWithCfg with no disk file and no StatsDisplay → default config.
	if got := LoadStatsConfigWithCfg("scanner", cfg); len(got) == 0 {
		t.Fatalf("LoadStatsConfigWithCfg(scanner) should return defaults")
	}

	// resolveStatsSources: a resolver-backed stat gets a value only if it resolves;
	// a dashboard-native stat passes through untouched.
	stats := []any{
		map[string]any{"key": "x", "source": "status", "field": "actionableCount"},
		map[string]any{"key": "y", "source": "static", "field": "MYVAR"},
		map[string]any{"key": "z", "source": "resolver"}, // no field → skipped
		"not-a-map", // non-map entry → skipped
	}
	out := resolveStatsSources(stats, cfg)
	if len(out) != len(stats) {
		t.Fatalf("resolveStatsSources changed slice length: %d", len(out))
	}

	// nil cfg / empty stats early return.
	if got := resolveStatsSources(nil, cfg); got != nil {
		t.Fatalf("resolveStatsSources(nil) should return nil")
	}
	if got := resolveStatsSources(stats, nil); len(got) != len(stats) {
		t.Fatalf("resolveStatsSources(_, nil) should pass through")
	}
}

// TestCovH2_BuildACMMPackAgents covers the pack-agents helper.
func TestCovH2_BuildACMMPackAgents(t *testing.T) {
	deps := testDeps(t)
	// Without an ACMM level, detectACMMLevel picks a default; the call must not panic
	// and returns a (possibly empty) name slice.
	_ = buildACMMPackAgents(deps.Config)

	// With an explicit level set, exercise the non-default branch.
	lvl := 5
	deps.Config.ACMMLevel = &lvl
	_ = buildACMMPackAgents(deps.Config)
}

// TestCovH2_BuildHealthAndRateLimits covers the nil-client fallbacks and the
// live path via a mock GitHub API.
func TestCovH2_BuildHealthAndRateLimits(t *testing.T) {
	deps := testDeps(t)
	logger := covH2Logger()

	// This test drives buildHealth with a live client below, and that
	// production path writes the package-level cachedHealth cache and can
	// refresh cachedGreenStreak (status_builder.go). Nothing in the non-test
	// code clears either, so without a restore the caches leak into every
	// later test that exercises the nil-client branch — those tests assert
	// the *default* {"ci": 100} and instead observe this test's fixture. The
	// shared hook snapshots both caches and restores them in t.Cleanup, so
	// the restore runs even when an assertion below calls t.Fatalf (#5570).
	restoreHealthCaches(t)

	// nil client → cached/default fallback branch.
	h := buildHealth(nil, nil)
	if h == nil {
		t.Fatalf("buildHealth(nil) returned nil")
	}
	rl := buildGHRateLimits(nil, nil, deps.Config)
	if rl == nil {
		t.Fatalf("buildGHRateLimits(nil) returned nil")
	}
	if _, ok := rl["identity"]; !ok {
		t.Fatalf("buildGHRateLimits missing identity")
	}

	// Live path with a mock GitHub server: rate_limits + workflow health calls
	// hit the server; whatever it returns, the non-nil branch executes.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rate_limit":
			json.NewEncoder(w).Encode(map[string]any{
				"resources": map[string]any{
					"core":    map[string]any{"limit": 5000, "remaining": 4999, "reset": 1700000000},
					"search":  map[string]any{"limit": 30, "remaining": 30, "reset": 1700000000},
					"graphql": map[string]any{"limit": 5000, "remaining": 5000, "reset": 1700000000},
				},
			})
		default:
			// Actions/workflow endpoints → empty-ish valid JSON.
			json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}, "workflows": []any{}})
		}
	}))
	defer ts.Close()

	ghc := ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
	ctx := context.Background()

	rl2 := buildGHRateLimits(ghc, ctx, deps.Config)
	if rl2 == nil {
		t.Fatalf("buildGHRateLimits(live) returned nil")
	}

	// The live branch of buildHealth CACHES what it fetched into the package
	// global cachedHealth — the mock above answers the workflow endpoints with
	// an empty run list, so the cached map carries ci=0 (#5553:
	// TestBuildHealth_NilClient_Boost failed with "ci = 0" only on the
	// shuffle orders that put this test first). Both dirtied globals are
	// restored by the restoreHealthCaches hook registered at the top.
	_ = buildHealth(ghc, ctx)

	// App-auth identity branch: set an AppID so buildGHRateLimits takes the "app" path.
	deps.Config.GitHub.AppID = 12345
	rl3 := buildGHRateLimits(ghc, ctx, deps.Config)
	if id, ok := rl3["identity"].(map[string]any); !ok || id["type"] != "app" {
		t.Fatalf("expected app identity, got %v", rl3["identity"])
	}
}

// TestCovH2_SystemResourcesAndRedact covers the resource collectors and redactor.
func TestCovH2_SystemResourcesAndRedact(t *testing.T) {
	// collectSystemResources reads real syscalls/proc — on any platform it must
	// return a non-nil struct without panicking (proc paths may be absent → -1).
	if res := collectSystemResources(); res == nil {
		t.Fatalf("collectSystemResources returned nil")
	}

	// readProcMemTotalBytes: on non-Linux /proc/meminfo is absent → -1;
	// on Linux it parses a value. Either way it must not panic.
	_ = readProcMemTotalBytes()

	// redactTokens masks GitHub tokens and device-flow codes.
	in := "token ghp_abcdefghijklmnop1234567890 and code ABCD-1234"
	out := redactTokens(in)
	if out == in {
		t.Fatalf("redactTokens did not alter a token-bearing string: %q", out)
	}
	// Clean string is unchanged.
	if got := redactTokens("nothing secret here"); got != "nothing secret here" {
		t.Fatalf("redactTokens altered a clean string: %q", got)
	}
}
