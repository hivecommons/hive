package main

import (
	"context"
	"log/slog"

	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/github"
)

// GitHub App credential verdicts now live in pkg/apphealth (#7238 stage 1).
// These wrappers keep every existing call site and test unchanged; the only
// thing they add is the private-key paths, which the extracted code takes as
// a field instead of reading the package-level appKeys global.

// appHealthChecker builds a Checker from this process's App key locations.
func appHealthChecker() apphealth.Checker {
	return apphealth.Checker{KeyPaths: []string{appKeys.DataKeyPath, appKeys.ProvisionedKeyPath}}
}

func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return appHealthChecker().ClassifyFailure(ctx, appAuth, expectedOwner, logger)
}

func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (string, github.AppAuthState) {
	return appHealthChecker().ClassifyWriteForbidden(ctx, appAuth, expectedOwner, repo)
}

func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyRepoCoverage(ctx, appAuth, org, repos, logger)
}
