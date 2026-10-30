package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestNativeMergeQueueAutoMergeConfigIsOptInPerRepo(t *testing.T) {
	tests := []struct {
		name    string
		cfg     NativeMergeQueueAutoMergeConfig
		repo    string
		want    bool
		wantTTL time.Duration
	}{
		{"unset is off", NativeMergeQueueAutoMergeConfig{}, "acme/widget", false, DefaultNativeMergeQueueProbeTTL},
		{"repos without enabled stay off", NativeMergeQueueAutoMergeConfig{Repos: []string{"acme/widget"}}, "acme/widget", false, DefaultNativeMergeQueueProbeTTL},
		{"enabled with no allow-list covers every repo", NativeMergeQueueAutoMergeConfig{Enabled: true}, "acme/widget", true, DefaultNativeMergeQueueProbeTTL},
		{"listed repo matches case-insensitively", NativeMergeQueueAutoMergeConfig{Enabled: true, Repos: []string{" Acme/Widget "}}, "acme/widget", true, DefaultNativeMergeQueueProbeTTL},
		{"unlisted repo is off", NativeMergeQueueAutoMergeConfig{Enabled: true, Repos: []string{"acme/other"}}, "acme/widget", false, DefaultNativeMergeQueueProbeTTL},
		{"probe_ttl overrides the default", NativeMergeQueueAutoMergeConfig{Enabled: true, ProbeTTL: 5 * time.Minute}, "acme/widget", true, 5 * time.Minute},
		{"negative probe_ttl falls back", NativeMergeQueueAutoMergeConfig{Enabled: true, ProbeTTL: -time.Second}, "acme/widget", true, DefaultNativeMergeQueueProbeTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EnabledFor(tc.repo); got != tc.want {
				t.Fatalf("EnabledFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
			if got := tc.cfg.EffectiveProbeTTL(); got != tc.wantTTL {
				t.Fatalf("EffectiveProbeTTL = %v, want %v", got, tc.wantTTL)
			}
		})
	}
}

func TestNativeMergeQueueAutoMergeConfigParsesYAML(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
auto_merge:
  native_merge_queue:
    enabled: true
    repos: [acme/widget]
    skip_current_base_gate: true
    probe_ttl: 15m
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.AutoMerge.NativeMergeQueue
	if !q.EnabledFor("acme/widget") || q.EnabledFor("acme/other") || !q.SkipCurrentBaseGate || q.EffectiveProbeTTL() != 15*time.Minute {
		t.Fatalf("parsed %+v", q)
	}
}
