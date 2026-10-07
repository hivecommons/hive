package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContributorPRsBaseSyncDefaultsOff(t *testing.T) {
	var rc ReviewConfig
	if rc.ContributorPRs.BaseSync {
		t.Fatal("review.contributor_prs.base_sync must default off")
	}
}

func TestLoadContributorPRsBaseSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte("project:\n  org: hivecommons\n  repos: [hive]\ngithub:\n  token: ghp_testtoken\nagents:\n  scanner:\n    backend: claude\nreview:\n  contributor_prs:\n    base_sync: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Review.ContributorPRs.BaseSync {
		t.Fatalf("contributor_prs.base_sync not loaded: %+v", cfg.Review.ContributorPRs)
	}
}
