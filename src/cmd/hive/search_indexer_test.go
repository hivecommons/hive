package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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
	cfg.Project.PrimaryRepo = "x/b"
	if got := searchHomeRepo(cfg); got != "x/b" {
		t.Errorf("searchHomeRepo = %q, want primary repo", got)
	}
}
