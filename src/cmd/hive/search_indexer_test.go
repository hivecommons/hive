package main

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/search"
)

func withSearchIndexDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "search")
	prev := searchIndexDir
	searchIndexDir = dir
	t.Cleanup(func() { searchIndexDir = prev })
	return dir
}

// search.enabled defaults off: no indexer is built and no index directory
// is created (#14635).
func TestNewSearchIndexerDisabledByDefault(t *testing.T) {
	dir := withSearchIndexDir(t)
	for _, cfg := range []*config.Config{nil, {}} {
		ix, err := newSearchIndexer(cfg, nil, nil, slog.Default())
		if err != nil || ix != nil {
			t.Fatalf("newSearchIndexer(disabled) = %v, %v; want nil, nil", ix, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("disabled search created %s (stat err %v)", dir, err)
	}
	b := &boot{cfg: &config.Config{}, logger: slog.Default()}
	b.startSearchIndexer()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("startSearchIndexer with search disabled created %s", dir)
	}
}

func TestNewSearchIndexerEnabled(t *testing.T) {
	dir := withSearchIndexDir(t)
	on := true
	cfg := &config.Config{}
	cfg.Project.Org = "o"
	cfg.Project.Repos = []string{"a", "x/b"}
	cfg.Search = config.SearchConfig{Enabled: &on, Retention: 6}
	ix, err := newSearchIndexer(cfg, nil, nil, slog.Default())
	if err != nil || ix == nil {
		t.Fatalf("newSearchIndexer(enabled) = %v, %v", ix, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("index dir not created: %v", err)
	}
	if ix.HomeRepo != "o/a" || ix.RetentionMonths != 6 || ix.StartupBudget <= 0 {
		t.Errorf("indexer = home %q retention %d budget %v", ix.HomeRepo, ix.RetentionMonths, ix.StartupBudget)
	}
	if h := ix.HealthOptions; h.StaleAfter != search.DefaultHealthStaleAfter || h.SecurityDocMaxAge != search.DefaultHealthSecurityDocMaxAge || !maps.Equal(h.Weights, search.DefaultHealthWeights()) {
		t.Errorf("health options = %+v, want search.health defaults", h)
	}
	cfg.Project.PrimaryRepo = "x/b"
	if got := searchHomeRepo(cfg); got != "x/b" {
		t.Errorf("searchHomeRepo = %q, want primary repo", got)
	}
}

// With search on, the dashboard health endpoint is served from the indexer
// (#14638): 503 before wiring, 404 for a repo with no evidence after.
func TestStartSearchIndexerWiresRepoHealth(t *testing.T) {
	withSearchIndexDir(t)
	on := true
	cfg := &config.Config{}
	cfg.Search = config.SearchConfig{Enabled: &on}
	srv := dashboard.NewServer(0, slog.Default())
	srv.RegisterAPI(&dashboard.Dependencies{Config: &config.Config{}})
	get := func() int {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/repos/o/a/health", nil))
		return rec.Code
	}
	if code := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("before wiring: code %d, want 503", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &boot{cfg: cfg, logger: slog.Default(), ctx: ctx, dashSrv: srv}
	b.startSearchIndexer()
	if code := get(); code != http.StatusNotFound {
		t.Fatalf("after wiring: code %d, want 404", code)
	}
}

// The config package duplicates the search package's health defaults so
// that search.health documents them without importing pkg/search; keep the
// two copies from drifting (#14638).
func TestSearchHealthDefaultsMatchConfig(t *testing.T) {
	if config.DefaultSearchHealthStaleAfter != search.DefaultHealthStaleAfter {
		t.Errorf("stale_after: config %v, search %v", config.DefaultSearchHealthStaleAfter, search.DefaultHealthStaleAfter)
	}
	if config.DefaultSearchHealthSecurityDocMaxAge != search.DefaultHealthSecurityDocMaxAge {
		t.Errorf("security_doc_max_age: config %v, search %v", config.DefaultSearchHealthSecurityDocMaxAge, search.DefaultHealthSecurityDocMaxAge)
	}
	if !maps.Equal(config.DefaultSearchHealthWeights, search.DefaultHealthWeights()) {
		t.Errorf("weights: config %v, search %v", config.DefaultSearchHealthWeights, search.DefaultHealthWeights())
	}
}
