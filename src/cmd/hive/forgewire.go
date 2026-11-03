package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// governorForge returns the forge-neutral write seam the governor path should
// use for this config, or nil when no forge can be reached at all.
//
// Selection follows project.forge, or per repo when any
// project.repo_policies[] entry sets forge/forge_url (see repoForgeRouter):
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
	// Per-repo forge overrides (project.repo_policies[].forge / forge_url,
	// hivecommons/hive#14112) need a writer that picks the forge per call. A
	// hive with none keeps the single project-wide writer below, unchanged.
	if cfg.HasRepoForgeOverrides() {
		return &repoForgeRouter{cfg: cfg, ghClient: ghClient, logger: logger}
	}
	return forgeForRepo(cfg, "", ghClient, logger)
}

// forgeForRepo is governorForge for one repo: the writer for the forge repo
// lives on (its repo_policies override, else project.forge). An empty repo
// resolves the project-wide forge. Fallback rules are governorForge's.
func forgeForRepo(cfg *config.Config, repo string, ghClient *github.Client, logger *slog.Logger) forge.IssueWriter {
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
	switch k := cfg.ForgeKindForRepo(repo); k {
	case config.ForgeGitHub:
		return fallback()
	case config.ForgeGitLab:
		kind, baseURL, tokenID = forge.KindGitLab, cfg.ForgeURLForRepo(repo), cfg.GitLab.TokenEnvName()
	case config.ForgeGitea:
		kind, baseURL, tokenID = forge.KindGitea, cfg.ForgeURLForRepo(repo), cfg.Gitea.TokenEnvName()
	default:
		logger.Warn("unknown project.forge; falling back to the GitHub client",
			"forge", k, "repo", repo, "known", config.KnownForgeKinds)
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
			"forge", kind, "repo", repo, "token_env", tokenID, "error", err)
		return fallback()
	}
	return f
}

// repoForgeRouter is the governor's writer on a hive whose repos do not all
// live on one forge: each write goes to the forge its repo argument names.
type repoForgeRouter struct {
	cfg      *config.Config
	ghClient *github.Client
	logger   *slog.Logger
}

func (r *repoForgeRouter) writerFor(repo string) (forge.IssueWriter, error) {
	w := forgeForRepo(r.cfg, repo, r.ghClient, r.logger)
	if w == nil {
		return nil, fmt.Errorf("forge: no writer reachable for repo %q (%s)", repo, r.cfg.ForgeKindForRepo(repo))
	}
	return w, nil
}

func (r *repoForgeRouter) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	w, err := r.writerFor(repo)
	if err != nil {
		return err
	}
	return w.CreateIssueComment(ctx, repo, number, body)
}

func (r *repoForgeRouter) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	w, err := r.writerFor(repo)
	if err != nil {
		return err
	}
	return w.AddLabels(ctx, repo, number, labels)
}

// newForgeMerger builds the merge primitive for a non-GitHub forge. It is a
// variable so tests can substitute a fake forge.
var newForgeMerger = func(kind forge.Kind, token string, opts forge.Options) (forge.Merger, error) {
	f, err := forge.NewForge(kind, token, opts)
	if err != nil {
		return nil, err
	}
	m, ok := f.(forge.Merger)
	if !ok {
		return nil, fmt.Errorf("forge %q adapter does not implement merge", kind)
	}
	return m, nil
}

// forgeMergerForRepo returns the forge.Merger for a repo that lives on GitLab
// or Gitea, or (nil, nil) for a GitHub repo — and for an unknown kind, which
// falls back to GitHub exactly as forgeForRepo does — so the GitHub merge path
// (MergePR on the concrete client) stays byte-for-byte unchanged. Unlike the
// comment/label writer, a GitLab/Gitea repo whose adapter cannot be built
// returns an error rather than falling back: merging it through the GitHub
// client could never succeed and must not be attempted.
func forgeMergerForRepo(cfg *config.Config, repo string) (forge.Merger, error) {
	var kind forge.Kind
	var tokenID string
	switch cfg.ForgeKindForRepo(repo) {
	case config.ForgeGitLab:
		kind, tokenID = forge.KindGitLab, cfg.GitLab.TokenEnvName()
	case config.ForgeGitea:
		kind, tokenID = forge.KindGitea, cfg.Gitea.TokenEnvName()
	default:
		return nil, nil
	}
	return newForgeMerger(kind, os.Getenv(tokenID), forge.Options{
		BaseURL: cfg.ForgeURLForRepo(repo),
		Org:     cfg.Project.Org,
	})
}

// forgeMergeSelector is the merge relay's per-repo forge router: a repo whose
// forge.Kind is GitLab or Gitea merges through that forge's Merger at the
// pinned head; a GitHub repo gets nil and keeps the relay's GitHub path. cfg
// is read per request so a reload takes effect without a restart. The
// merge-eligible binding (bindMergeAuthz) has already run by the time the
// returned function is called — it is the same gate on every forge.
func forgeMergeSelector(cfgFn func() *config.Config, logger *slog.Logger) github.ForgeMergeSelector {
	return func(repo string) github.ForgeMergeFunc {
		cfg := cfgFn()
		if cfg == nil {
			return nil
		}
		kind := cfg.ForgeKindForRepo(repo)
		if kind != config.ForgeGitLab && kind != config.ForgeGitea {
			return nil
		}
		return func(ctx context.Context, repo string, number int, method, expectSHA string) error {
			m, err := forgeMergerForRepo(cfg, repo)
			if err != nil {
				return fmt.Errorf("forge merge unavailable for %s (%s): %w", repo, kind, err)
			}
			if m == nil {
				return fmt.Errorf("forge merge unavailable for %s (%s)", repo, kind)
			}
			err = m.Merge(ctx, repo, number, forge.MergeOptions{Strategy: method, HeadSHA: expectSHA})
			if errors.Is(err, forge.ErrAlreadyMerged) {
				// A retried request whose first attempt landed: the desired
				// end state is reached.
				logger.Info("forge merge: change request already merged", "repo", repo, "number", number, "forge", kind)
				return nil
			}
			return err
		}
	}
}
