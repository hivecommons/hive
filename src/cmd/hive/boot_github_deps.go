package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// bootGitHubDeps is bootGitHub's one effect (#7571, step 2): resolving the
// GitHub credentials, which reads the App key from disk and may reach
// GitHub. Everything after it — label/filter setters, review bots, the
// failure/state hand-off — runs against the returned client.
type bootGitHubDeps struct {
	initGitHubAuth func(ctx context.Context, cfg *config.Config, logger *slog.Logger) githubAuth
}

func defaultBootGitHubDeps() bootGitHubDeps {
	return bootGitHubDeps{initGitHubAuth: initGitHubAuth}
}

// bootAdvisoryDeps are the effects bootAdvisory performs against GitHub
// (#7571, step 2): ensuring the pinned advisory issue and, when that fails,
// classifying the App failure to decide whether the banner is raised. The
// notifier, ACMM inference, attribution hook, and on-disk brainstorm policy
// run for real (cfg.Policies.LocalDir points a test at a temp dir).
type bootAdvisoryDeps struct {
	ensureAdvisoryIssue func(ctx context.Context, c *github.Client, repo string) (int, error)
	classifyAppFailure  func(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (bool, string, github.AppAuthState)
}

func defaultBootAdvisoryDeps() bootAdvisoryDeps {
	return bootAdvisoryDeps{
		ensureAdvisoryIssue: func(ctx context.Context, c *github.Client, repo string) (int, error) {
			return c.EnsureAdvisoryIssue(ctx, repo)
		},
		classifyAppFailure: classifyGitHubAppFailure,
	}
}
