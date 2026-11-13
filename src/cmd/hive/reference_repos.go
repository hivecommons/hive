package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// referenceRepoCredentialExitCode marks an exit caused by a
// project.reference_repos entry that opted in to writes without a credential
// GitHub would honour for that repo (hivecommons/hive#18015). Its own code so
// the termination state says "install the App on the target org or supply
// write_token_file", not "bug".
const referenceRepoCredentialExitCode = 22

// referenceRepoDeps are the seams buildReferenceRepoPolicies needs: App
// installation discovery on a foreign org and token-identity resolution.
// Either may be nil when the hive has no GitHub App auth, in which case a
// write-mode entry can only be served by write_token_file.
type referenceRepoDeps struct {
	botLogin string
	discover func(ctx context.Context, org string) (int64, error)
	whoAmI   func(ctx context.Context, token string) (string, error)
	auth     func() *github.AppAuth
}

// buildReferenceRepoPolicies turns the config list into proxy policies. A
// read-only entry needs nothing; a write-mode entry is verified now so the
// failure is a boot-time config error naming the repo.
func buildReferenceRepoPolicies(ctx context.Context, refs []config.ReferenceRepo, deps referenceRepoDeps, logger *slog.Logger) ([]proxy.ReferenceRepoPolicy, error) {
	out := make([]proxy.ReferenceRepoPolicy, 0, len(refs))
	for _, rr := range refs {
		pol := proxy.ReferenceRepoPolicy{Repo: rr.Repo, Issues: rr.WritesIssues(), PRs: rr.WritesPRs()}
		if !rr.ReadOnly() {
			login, err := github.VerifyReferenceRepoWriteCredential(ctx, rr, deps.botLogin, deps.discover, deps.whoAmI)
			if err != nil {
				return nil, err
			}
			pol.Login = login
			pol.Token = github.ReferenceRepoTokenSource(rr, deps.auth, logger)
		}
		out = append(out, pol)
	}
	return out, nil
}

// wireReferenceRepos installs project.reference_repos on the proxy, refusing
// to boot when a write-mode entry has no usable credential.
func (b *boot) wireReferenceRepos() {
	refs := b.cfg.Project.ReferenceRepos
	if len(refs) == 0 || b.githubProxy == nil {
		return
	}
	deps := referenceRepoDeps{auth: func() *github.AppAuth { return nil }}
	if b.ghClient != nil {
		if auth := b.ghClient.AppAuth(); auth != nil {
			apiURL := auth.APIURL()
			deps.botLogin = auth.BotLogin()
			deps.discover = auth.DiscoverInstallationID
			deps.whoAmI = func(ctx context.Context, token string) (string, error) {
				return github.TokenUserLogin(ctx, token, apiURL)
			}
			// Re-fetched per call: the AppAuth is replaced on key rotation.
			deps.auth = func() *github.AppAuth { return b.ghClient.AppAuth() }
		} else {
			deps.whoAmI = func(ctx context.Context, token string) (string, error) {
				return github.TokenUserLogin(ctx, token, "")
			}
		}
	}
	policies, err := buildReferenceRepoPolicies(b.ctx, refs, deps, b.logger)
	if err != nil {
		b.logger.Error("refusing to start: project.reference_repos write-mode entry has no usable credential (#18015)", "error", err.Error())
		if errors.Is(err, github.ErrReferenceRepoNoCredential) {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(referenceRepoCredentialExitCode)
	}
	b.githubProxy.SetReferenceRepos(policies)
	b.logger.Info("reference repos wired", "count", len(policies), "repos", b.cfg.ReferenceRepoSlugs())
}
