package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// knowledgeSeedProbe is the slice of *github.Client the git-source seed
// needs: the primary repo's default branch and whether a path exists there.
type knowledgeSeedProbe interface {
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	PathExistsAtRef(ctx context.Context, owner, repo, path, ref string) (bool, error)
}

// knowledgeSeedCandidatePaths are probed in order when
// knowledge.bead_synthesizer.publish.path is unset; the first that exists
// in the primary repo becomes the seeded git source's subpath.
var knowledgeSeedCandidatePaths = []string{"docs/knowledge", "src/docs/knowledge"}

// knowledgeSeedLayer is the layer a repo's own knowledge dir is indexed at.
const knowledgeSeedLayer = "project"

// seedKnowledgeGitSource gives a hive with no knowledge.git_sources one
// entry pointing at its primary repo's knowledge directory, so hosted spokes
// index the repo's curated entries without per-spoke hand config. It runs
// in memory at every boot and never writes hive.yaml: an operator list (even
// an explicit empty `git_sources: []`, which is what a dashboard disconnect
// saves) is left alone, so the decision is idempotent and never overwrites
// configuration. It reports whether an entry was appended.
func seedKnowledgeGitSource(ctx context.Context, cfg *config.Config, probe knowledgeSeedProbe, logger *slog.Logger) bool {
	if cfg == nil {
		return false
	}
	if cfg.Knowledge.GitSources != nil {
		logger.Debug("knowledge: git source seed skipped", "reason", "knowledge.git_sources is operator-set", "count", len(cfg.Knowledge.GitSources))
		return false
	}
	repo := strings.TrimSpace(cfg.Project.PrimaryRepo)
	if repo == "" && len(cfg.Project.Repos) > 0 {
		repo = strings.TrimSpace(cfg.Project.Repos[0])
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		logger.Debug("knowledge: git source seed skipped", "reason", "no primary repo")
		return false
	}
	if cfg.Project.ForgeKind() != config.ForgeGitHub {
		logger.Debug("knowledge: git source seed skipped", "reason", "forge is not github", "forge", cfg.Project.ForgeKind())
		return false
	}
	if probe == nil {
		logger.Debug("knowledge: git source seed skipped", "reason", "no github client", "repo", repo)
		return false
	}
	branch, err := probe.DefaultBranch(ctx, owner, name)
	if err != nil {
		logger.Debug("knowledge: git source seed skipped", "reason", "default branch lookup failed", "repo", repo, "error", err)
		return false
	}
	var subpath string
	for _, candidate := range knowledgeSeedPaths(cfg.Knowledge.BeadSynthesizer.Publish.Path) {
		exists, err := probe.PathExistsAtRef(ctx, owner, name, candidate, branch)
		if err != nil {
			logger.Debug("knowledge: git source seed skipped", "reason", "path probe failed", "repo", repo, "path", candidate, "error", err)
			return false
		}
		if exists {
			subpath = candidate
			break
		}
	}
	if subpath == "" {
		logger.Debug("knowledge: git source seed skipped", "reason", "no knowledge dir in primary repo", "repo", repo, "branch", branch)
		return false
	}
	base := strings.TrimRight(cfg.GitHub.BaseURL, "/")
	if base == "" {
		base = config.DefaultGitHubBaseURL
	}
	gs := config.GitSourceConfigYAML{
		Name:    name + "-knowledge",
		URL:     base + "/" + repo,
		Branch:  branch,
		Subpath: subpath,
		Layer:   knowledgeSeedLayer,
	}
	cfg.Knowledge.GitSources = append(cfg.Knowledge.GitSources, gs)
	logger.Info("knowledge: seeded git source "+gs.Name+" from "+repo+"/"+subpath, "branch", branch, "layer", gs.Layer)
	return true
}

// knowledgeSeedPaths orders the subpaths to probe: the configured bead-synth
// publish path first (that is where this repo keeps its knowledge dir), then
// the conventional locations, without repeats.
func knowledgeSeedPaths(publishPath string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(publishPath)
	for _, p := range knowledgeSeedCandidatePaths {
		add(p)
	}
	return out
}
