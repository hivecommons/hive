package config

// AppSignedCommitsEnabled reports whether the PR-request watcher re-authors
// agent branches through createCommitOnBranch so their commits are GitHub-
// signed. Opt-in: nil and false both mean off. See AppSignedCommits.
func (g GitHubConfig) AppSignedCommitsEnabled() bool {
	return g.AppSignedCommits != nil && *g.AppSignedCommits
}

// SelfAuthorizationHoldEnabled reports whether the #5117 self-authorization
// hold is active for this hive without considering ACMM level. Default ON
// preserves the existing policy for callers that have not been wired to the
// live level-aware resolver.
func (g GitHubConfig) SelfAuthorizationHoldEnabled() bool {
	return g.SelfAuthorizationHoldEnabledAtLevel(0)
}

// SelfAuthorizationHoldEnabledAtLevel reports whether the #5117
// self-authorization hold is active at the provided live ACMM level. Explicit
// env/config values win; otherwise L6 fully autonomous defaults the policy off.
func (g GitHubConfig) SelfAuthorizationHoldEnabledAtLevel(acmmLevel int) bool {
	if g.selfAuthorizationHoldEnvOverride != nil {
		return *g.selfAuthorizationHoldEnvOverride
	}
	if g.SelfAuthorizationHold == nil {
		return acmmLevel < MaxACMMLevel
	}
	return *g.SelfAuthorizationHold
}

// SelfAuthorizationHoldEnvOverrideSet reports whether
// HIVE_SELF_AUTHORIZATION_HOLD is currently forcing the effective value.
func (g GitHubConfig) SelfAuthorizationHoldEnvOverrideSet() bool {
	return g.selfAuthorizationHoldEnvOverride != nil
}
