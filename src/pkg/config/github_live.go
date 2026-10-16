package config

import (
	"sync"
	"time"
)

// githubLiveMu guards the github.* settings the dashboard rewrites at runtime
// while GitHub client hooks read them on their own goroutines through live
// closures (hivecommons/hive#12279): graphql_pr_batch,
// graphql_pr_batch_page_size, pr_detail_ttl_s, app_authored_prs,
// app_signed_commits and self_authorization_hold. Writers go through
// UpdateGitHubLive; live readers use the Config.GitHub* accessors below,
// which copy only the guarded fields so they never read the rest of the
// block unlocked.
//
// Lock order is saveMu → githubLiveMu, never the reverse.
var githubLiveMu sync.RWMutex

// UpdateGitHubLive applies fn to the github block under githubLiveMu, so a
// runtime edit of the live-read settings cannot race their readers.
func (c *Config) UpdateGitHubLive(fn func(g *GitHubConfig)) {
	if c == nil || fn == nil {
		return
	}
	githubLiveMu.Lock()
	defer githubLiveMu.Unlock()
	fn(&c.GitHub)
}

// githubLive returns a copy of only the runtime-mutable github settings, taken
// under githubLiveMu.
func (c *Config) githubLive() GitHubConfig {
	if c == nil {
		return GitHubConfig{}
	}
	githubLiveMu.RLock()
	defer githubLiveMu.RUnlock()
	return GitHubConfig{
		PRDetailTTLS:                     c.GitHub.PRDetailTTLS,
		GraphQLPRBatch:                   c.GitHub.GraphQLPRBatch,
		GraphQLPRBatchPageSize:           c.GitHub.GraphQLPRBatchPageSize,
		AppAuthoredPRs:                   c.GitHub.AppAuthoredPRs,
		AppSignedCommits:                 c.GitHub.AppSignedCommits,
		SelfAuthorizationHold:            c.GitHub.SelfAuthorizationHold,
		selfAuthorizationHoldEnvOverride: c.GitHub.selfAuthorizationHoldEnvOverride,
	}
}

// GitHubPRDetailTTL is the lock-safe live form of GitHubConfig.PRDetailTTL.
func (c *Config) GitHubPRDetailTTL() time.Duration {
	return c.githubLive().PRDetailTTL()
}

// GitHubGraphQLPRBatchEnabled is the lock-safe live form of
// GitHubConfig.GraphQLPRBatchEnabled.
func (c *Config) GitHubGraphQLPRBatchEnabled() bool {
	return c.githubLive().GraphQLPRBatchEnabled()
}

// GitHubGraphQLPRBatchPageSize is the lock-safe live form of
// GitHubConfig.EffectiveGraphQLPRBatchPageSize.
func (c *Config) GitHubGraphQLPRBatchPageSize() int {
	return c.githubLive().EffectiveGraphQLPRBatchPageSize()
}

// GitHubAppAuthoredPRsEnabled is the lock-safe live form of
// GitHubConfig.AppAuthoredPRsEnabled.
func (c *Config) GitHubAppAuthoredPRsEnabled() bool {
	return c.githubLive().AppAuthoredPRsEnabled()
}

// GitHubAppSignedCommitsEnabled is the lock-safe live form of
// GitHubConfig.AppSignedCommitsEnabled.
func (c *Config) GitHubAppSignedCommitsEnabled() bool {
	return c.githubLive().AppSignedCommitsEnabled()
}

// GitHubSelfAuthorizationHoldEnabledAtLevel is the lock-safe live form of
// GitHubConfig.SelfAuthorizationHoldEnabledAtLevel.
func (c *Config) GitHubSelfAuthorizationHoldEnabledAtLevel(acmmLevel int) bool {
	return c.githubLive().SelfAuthorizationHoldEnabledAtLevel(acmmLevel)
}
