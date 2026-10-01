package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPRPrecheckConfigDefaults(t *testing.T) {
	var cfg Config
	if !cfg.GitHub.PRPrecheck.DocsEnabled() {
		t.Fatal("docs precheck default = false, want true")
	}
	// Tier C is opt-in: CI owns test verdicts and an in-pod `go test` can
	// read and mutate the live /data volume.
	if cfg.GitHub.PRPrecheck.GoTestsEnabled() {
		t.Fatal("go_tests precheck default = true, want false")
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveTimeout(); got != DefaultPRPrecheckTimeout {
		t.Fatalf("timeout default = %s, want %s", got, DefaultPRPrecheckTimeout)
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveMaxConcurrent(); got != DefaultPRPrecheckMaxConcurrent {
		t.Fatalf("max_concurrent default = %d, want %d", got, DefaultPRPrecheckMaxConcurrent)
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveCacheDir("/data"); got != "/data/pr-precheck/gocache" {
		t.Fatalf("cache_dir default = %q, want /data/pr-precheck/gocache", got)
	}
}

func TestPRPrecheckConfigParsesYAML(t *testing.T) {
	var cfg Config
	data := []byte(`
github:
  pr_precheck:
    docs: false
    go_tests: true
    timeout: 3m
    cache_dir: /cache/pr-precheck
    max_concurrent: 2
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.PRPrecheck.DocsEnabled() {
		t.Fatal("docs precheck = true, want explicit false")
	}
	if !cfg.GitHub.PRPrecheck.GoTestsEnabled() {
		t.Fatal("go_tests precheck = false, want explicit true")
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveTimeout(); got != 3*time.Minute {
		t.Fatalf("timeout = %s, want 3m", got)
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveCacheDir("/data"); got != "/cache/pr-precheck" {
		t.Fatalf("cache_dir = %q, want explicit cache dir", got)
	}
	if got := cfg.GitHub.PRPrecheck.EffectiveMaxConcurrent(); got != 2 {
		t.Fatalf("max_concurrent = %d, want 2", got)
	}
}
