package config

import (
	"strings"
	"testing"
)

const etagCacheTestYAML = `
project:
  org: my-org
  repos: [repo-a]
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`

func TestGitHubETagCacheLimitsDefaults(t *testing.T) {
	cfg, err := Load(writeTempConfig(t, etagCacheTestYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.ETagCacheMaxMB != 0 || cfg.GitHub.ETagCacheMaxEntries != 0 {
		t.Fatalf("unset etag cache bounds must stay 0 in the file, got mb=%d entries=%d", cfg.GitHub.ETagCacheMaxMB, cfg.GitHub.ETagCacheMaxEntries)
	}
	if got := cfg.GitHub.EffectiveETagCacheMaxBytes(); got != DefaultGitHubETagCacheMaxMB<<20 {
		t.Fatalf("EffectiveETagCacheMaxBytes() = %d, want %d MiB", got, DefaultGitHubETagCacheMaxMB)
	}
	if got := cfg.GitHub.EffectiveETagCacheMaxEntries(); got != DefaultGitHubETagCacheMaxEntries {
		t.Fatalf("EffectiveETagCacheMaxEntries() = %d, want %d", got, DefaultGitHubETagCacheMaxEntries)
	}
	if got := (GitHubConfig{ETagCacheMaxMB: 256, ETagCacheMaxEntries: 20000}).EffectiveETagCacheMaxBytes(); got != 256<<20 {
		t.Fatalf("EffectiveETagCacheMaxBytes(256) = %d", got)
	}
	if got := (GitHubConfig{ETagCacheMaxEntries: 20000}).EffectiveETagCacheMaxEntries(); got != 20000 {
		t.Fatalf("EffectiveETagCacheMaxEntries(20000) = %d", got)
	}
}

func TestGitHubETagCacheLimitsYAML(t *testing.T) {
	cfg, err := Load(writeTempConfig(t, strings.Replace(etagCacheTestYAML, "  token: ghp_tok\n", "  token: ghp_tok\n  etag_cache_max_mb: 256\n  etag_cache_max_entries: 16384\n", 1)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.ETagCacheMaxMB != 256 || cfg.GitHub.ETagCacheMaxEntries != 16384 {
		t.Fatalf("yaml etag cache bounds not read: mb=%d entries=%d", cfg.GitHub.ETagCacheMaxMB, cfg.GitHub.ETagCacheMaxEntries)
	}
}

// HIVE_GITHUB_ETAG_CACHE_MAX_MB is how an operator sizes the cache per
// deployment without editing the file; it wins over the YAML value.
func TestGitHubETagCacheLimitsEnvOverride(t *testing.T) {
	t.Setenv(GitHubETagCacheMaxMBEnv, "512")
	t.Setenv(GitHubETagCacheMaxEntriesEnv, "32768")
	cfg, err := Load(writeTempConfig(t, strings.Replace(etagCacheTestYAML, "  token: ghp_tok\n", "  token: ghp_tok\n  etag_cache_max_mb: 256\n", 1)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.ETagCacheMaxMB != 512 {
		t.Fatalf("%s=512 did not override etag_cache_max_mb: %d", GitHubETagCacheMaxMBEnv, cfg.GitHub.ETagCacheMaxMB)
	}
	if cfg.GitHub.ETagCacheMaxEntries != 32768 {
		t.Fatalf("%s=32768 not applied: %d", GitHubETagCacheMaxEntriesEnv, cfg.GitHub.ETagCacheMaxEntries)
	}

	// A malformed value is ignored rather than failing the boot.
	t.Setenv(GitHubETagCacheMaxMBEnv, "lots")
	t.Setenv(GitHubETagCacheMaxEntriesEnv, "")
	cfg, err = Load(writeTempConfig(t, etagCacheTestYAML))
	if err != nil {
		t.Fatalf("Load() with malformed env: %v", err)
	}
	if cfg.GitHub.ETagCacheMaxMB != 0 {
		t.Fatalf("malformed %s should be ignored, got %d", GitHubETagCacheMaxMBEnv, cfg.GitHub.ETagCacheMaxMB)
	}

	// An out-of-range env value is rejected exactly like a file value.
	t.Setenv(GitHubETagCacheMaxMBEnv, "4096")
	if _, err := Load(writeTempConfig(t, etagCacheTestYAML)); err == nil || !strings.Contains(err.Error(), "etag_cache_max_mb") {
		t.Fatalf("out-of-range env value should fail validation, got %v", err)
	}
}

func TestGitHubETagCacheLimitsValidation(t *testing.T) {
	for _, tc := range []struct {
		mb, entries int
		wantErr     string
	}{
		{0, 0, ""},
		{1, 1, ""},
		{MaxGitHubETagCacheMaxMB, MaxGitHubETagCacheMaxEntries, ""},
		{-1, 0, "etag_cache_max_mb"},
		{MaxGitHubETagCacheMaxMB + 1, 0, "etag_cache_max_mb"},
		{0, -1, "etag_cache_max_entries"},
		{0, MaxGitHubETagCacheMaxEntries + 1, "etag_cache_max_entries"},
	} {
		err := ValidateETagCacheLimits(tc.mb, tc.entries)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ValidateETagCacheLimits(%d, %d) = %v, want nil", tc.mb, tc.entries, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ValidateETagCacheLimits(%d, %d) = %v, want error mentioning %s", tc.mb, tc.entries, err, tc.wantErr)
		}
	}

	cfg, err := Load(writeTempConfig(t, etagCacheTestYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	cfg.GitHub.ETagCacheMaxMB = -5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "github.etag_cache_max_mb") {
		t.Fatalf("Validate() with negative etag_cache_max_mb = %v", err)
	}
	cfg.GitHub.ETagCacheMaxMB = 0
	cfg.GitHub.ETagCacheMaxEntries = MaxGitHubETagCacheMaxEntries + 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "github.etag_cache_max_entries") {
		t.Fatalf("Validate() with oversized etag_cache_max_entries = %v", err)
	}
}
