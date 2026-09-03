package main

import (
	"log/slog"
	"os"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// This file wires pkg/forge onto the live governor path — the same structure
// hookwire.go and celwire.go use for their packages.
//
// pkg/forge ships GitHub, GitLab and Gitea adapters with the read path and the
// core write path implemented and tested, and pkg/config already carries the
// selector (project.forge) and the per-forge endpoint/token-env settings. What
// was missing was any production caller: every write on the governor path went
// straight to *github.Client, so a hive configured with project.forge: gitlab
// silently got GitHub behavior from an abstraction that was never reached
// (kubestellar/hive#5259).
//
// The seam is deliberately the SMALLEST one that removes that gap:
// forge.IssueWriter, the comment+label pair. *github.Client satisfies it
// already — its CreateIssueComment and AddLabels were given those exact
// signatures for this swap — so a GitHub hive keeps calling the very same
// client through the very same methods. Nothing about the default path changes;
// only the static type at the call site does, which is what lets a non-GitHub
// hive be handed an adapter instead.

// governorForge returns the forge-neutral write seam the governor path should
// use for this config, or nil when no forge can be reached at all.
//
// Selection follows project.forge:
//
//   - "github" (and unset, the default): ghClient itself. No adapter is
//     interposed, so the GitHub path is byte-for-byte what it was before.
//   - "gitlab" / "gitea": the corresponding pkg/forge adapter, built from the
//     already-present config (instance URL + the env var named by token_env).
//
// It falls back to ghClient on any construction failure — a Gitea forge with no
// URL set, an unparseable instance URL, a forge kind this build does not know.
// Falling back rather than failing closed is the conservative choice for a
// GitHub-hosted hive that merely typo'd its forge key: escalation evidence
// still reaches the PR. The failure is logged at warn either way, and when
// there is no GitHub client either the result is nil and callers no-op.
//
// The returned adapter is cheap to build (a struct plus an http.Client whose
// nil Transport shares http.DefaultTransport, so connection pooling survives),
// which is why this is called per governor cycle instead of being memoized: a
// hot config reload takes effect on the next tick with no invalidation logic.
func governorForge(cfg *config.Config, ghClient *github.Client, logger *slog.Logger) forge.IssueWriter {
	// A typed-nil *github.Client stored in an interface is NOT == nil, and the
	// callers' "no client, do nothing" guards test the interface. Normalize it
	// to an untyped nil here so those guards keep working.
	fallback := func() forge.IssueWriter {
		if ghClient == nil {
			return nil
		}
		return ghClient
	}
	if cfg == nil {
		return fallback()
	}

	var (
		kind    forge.Kind
		baseURL string
		tokenID string
	)
	switch k := cfg.Project.ForgeKind(); k {
	case config.ForgeGitHub:
		return fallback()
	case config.ForgeGitLab:
		kind, baseURL, tokenID = forge.KindGitLab, cfg.GitLab.InstanceURL(), cfg.GitLab.TokenEnvName()
	case config.ForgeGitea:
		kind, baseURL, tokenID = forge.KindGitea, cfg.Gitea.InstanceURL(), cfg.Gitea.TokenEnvName()
	default:
		logger.Warn("unknown project.forge; falling back to the GitHub client",
			"forge", k, "known", []string{config.ForgeGitHub, config.ForgeGitLab, config.ForgeGitea})
		return fallback()
	}

	// The token is read from the environment by name, never from config: the
	// no-hardcoded-secrets rule is why config carries token_env rather than the
	// secret itself. An empty value is passed through — the forge will reject it
	// with a 401 whose message names the real problem, which beats a local error
	// that cannot distinguish "unset" from "set but wrong".
	f, err := forge.NewForge(kind, os.Getenv(tokenID), forge.Options{
		BaseURL: baseURL,
		Org:     cfg.Project.Org,
	})
	if err != nil {
		logger.Warn("forge adapter unavailable; falling back to the GitHub client",
			"forge", kind, "token_env", tokenID, "error", err)
		return fallback()
	}
	return f
}
