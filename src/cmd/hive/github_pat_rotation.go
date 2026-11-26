package main

import (
	"context"
	"errors"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
)

// PAT rotation from the dashboard (hivecommons/hive#19633): the two halves of
// PUT /api/github-token that need the boot's state. The handler verifies with
// verifyRotatedGitHubToken, persists through the config save path, then calls
// applyRotatedGitHubToken. The config reload path (#19687) rebuilds the PAT
// client the same way when github.token changes in the file.

// patRotationVerifyTimeout bounds the verification round trips (GET /user,
// one GET per project repo, the scope probe) so a slow GitHub cannot hold the
// owner's request open indefinitely.
const patRotationVerifyTimeout = 30 * time.Second

// verifyRotatedGitHubToken checks a candidate PAT against the configured
// api_url, every project repo and the scope check for the current ACMM level,
// and returns its login.
func (b *boot) verifyRotatedGitHubToken(ctx context.Context, token string) (string, error) {
	if b == nil || b.cfg == nil {
		return "", errors.New("config unavailable")
	}
	if b.cfg.GitHub.HasApp() {
		return "", errors.New("this hive authenticates as a GitHub App; there is no personal access token to rotate")
	}
	ctx, cancel := context.WithTimeout(ctx, patRotationVerifyTimeout)
	defer cancel()
	// Same level resolution as the boot scope check in initGitHubAuth.
	scopeLevel := github.ACMMLevelUnset
	if b.cfg.ACMMLevel != nil {
		scopeLevel = *b.cfg.ACMMLevel
	}
	res, err := github.VerifyPAT(ctx, token, b.cfg.GitHub.ResolvedAPIURL(), b.cfg.Project.Org, b.cfg.Project.Repos, scopeLevel, b.logger)
	if err != nil {
		return "", err
	}
	return res.Login, nil
}

// applyRotatedGitHubToken puts a verified, persisted PAT into service.
func (b *boot) applyRotatedGitHubToken(token string) {
	b.applyRotatedGitHubTokenWith(token, defaultBootWatchersDeps().refreshAgentTokens)
}

// applyRotatedGitHubTokenWith builds a fully configured PAT client and adopts
// it, as the config reload path does: every long-lived consumer (sandbox,
// relays, collectors, dashboard) follows through adoptGitHubClient, and the
// running agents get fresh credentials at once rather than at the next
// refresh tick.
func (b *boot) applyRotatedGitHubTokenWith(token string, refresh func(ctx context.Context, mgr crux.Runtime)) {
	if b == nil || b.cfg == nil || token == "" {
		return
	}
	newClient := b.newConfiguredGitHubPATClient(token)
	b.adoptGitHubClient(newClient, nil)
	if refresh != nil && b.agentMgr != nil {
		refresh(b.ctx, b.agentMgr)
	}
	if b.logger != nil {
		b.logger.Info("github token client rebuilt after dashboard rotation")
	}
}
