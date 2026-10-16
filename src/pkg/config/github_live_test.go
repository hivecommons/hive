package config

import (
	"sync"
	"testing"
)

// TestGitHubLiveSettingsConcurrentUpdateAndRead exercises the runtime writer
// (the dashboard's github settings save) against the live client-hook readers
// on separate goroutines. Under -race it fails if either side bypasses
// githubLiveMu (hivecommons/hive#12279).
func TestGitHubLiveSettingsConcurrentUpdateAndRead(t *testing.T) {
	t.Setenv(GitHubPRDetailTTLEnv, "")
	cfg := &Config{}

	const iterations = 500
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			on := i%2 == 0
			cfg.UpdateGitHubLive(func(g *GitHubConfig) {
				g.GraphQLPRBatch = &on
				g.GraphQLPRBatchPageSize = i%100 + 1
				g.PRDetailTTLS = i + 1
				g.AppAuthoredPRs = &on
				g.AppSignedCommits = &on
				g.SelfAuthorizationHold = &on
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = cfg.GitHubGraphQLPRBatchEnabled()
			if n := cfg.GitHubGraphQLPRBatchPageSize(); n < 1 || n > 100 {
				t.Errorf("page size %d out of range", n)
				return
			}
			if d := cfg.GitHubPRDetailTTL(); d <= 0 {
				t.Errorf("pr detail ttl %v, want positive", d)
				return
			}
			_ = cfg.GitHubAppAuthoredPRsEnabled()
			_ = cfg.GitHubAppSignedCommitsEnabled()
			_ = cfg.GitHubSelfAuthorizationHoldEnabledAtLevel(0)
			_ = cfg.SelfAuthorizationHoldEnabledForRepoAtLevel("org/repo", 0)
		}
	}()
	wg.Wait()
}

// TestGitHubLiveAccessorsMatchValueAccessors pins the locked accessors to the
// GitHubConfig value methods they wrap, so the lock changes no behavior.
func TestGitHubLiveAccessorsMatchValueAccessors(t *testing.T) {
	t.Setenv(GitHubPRDetailTTLEnv, "")
	on, off := true, false
	for _, g := range []GitHubConfig{
		{},
		{GraphQLPRBatch: &off, GraphQLPRBatchPageSize: 250, PRDetailTTLS: 7, AppAuthoredPRs: &off, AppSignedCommits: &on, SelfAuthorizationHold: &off},
		{GraphQLPRBatch: &on, GraphQLPRBatchPageSize: 10, AppAuthoredPRs: &on, AppSignedCommits: &off, SelfAuthorizationHold: &on},
		{selfAuthorizationHoldEnvOverride: &off, SelfAuthorizationHold: &on},
	} {
		cfg := &Config{GitHub: g}
		if got, want := cfg.GitHubGraphQLPRBatchEnabled(), g.GraphQLPRBatchEnabled(); got != want {
			t.Errorf("GraphQLPRBatchEnabled = %v, want %v", got, want)
		}
		if got, want := cfg.GitHubGraphQLPRBatchPageSize(), g.EffectiveGraphQLPRBatchPageSize(); got != want {
			t.Errorf("GraphQLPRBatchPageSize = %d, want %d", got, want)
		}
		if got, want := cfg.GitHubPRDetailTTL(), g.PRDetailTTL(); got != want {
			t.Errorf("PRDetailTTL = %v, want %v", got, want)
		}
		if got, want := cfg.GitHubAppAuthoredPRsEnabled(), g.AppAuthoredPRsEnabled(); got != want {
			t.Errorf("AppAuthoredPRsEnabled = %v, want %v", got, want)
		}
		if got, want := cfg.GitHubAppSignedCommitsEnabled(), g.AppSignedCommitsEnabled(); got != want {
			t.Errorf("AppSignedCommitsEnabled = %v, want %v", got, want)
		}
		for _, level := range []int{0, MaxACMMLevel} {
			if got, want := cfg.GitHubSelfAuthorizationHoldEnabledAtLevel(level), g.SelfAuthorizationHoldEnabledAtLevel(level); got != want {
				t.Errorf("SelfAuthorizationHoldEnabledAtLevel(%d) = %v, want %v", level, got, want)
			}
		}
	}
	var nilCfg *Config
	if got, want := nilCfg.GitHubPRDetailTTL(), DefaultGitHubPRDetailTTL; got != want {
		t.Errorf("nil config PRDetailTTL = %v, want %v", got, want)
	}
}
