package main

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// promotionRulesFunc resolves auto_merge.promotions for one owner/repo, read
// live so a Settings save or an ACMM change applies without a restart. The
// promotion lane is part of the self-authored sweep, so where that sweep
// does not run (auto_merge.self_authored off, or below its ACMM level) it
// returns nothing: every merge lane routes the pull request as before, and
// the sentinel exempts nothing, so a person who merges by hand sees every
// finding. The same holds for a repository whose auto-merge switch is off
// (project.repo_policies[].auto_merge), where that sweep skips the repo. A
// rule for a repository that left project.repos never applies.
func promotionRulesFunc(cfg *config.Config) func(repo string) []config.PromotionRule {
	return func(repo string) []config.PromotionRule {
		if cfg == nil || !cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel) || !cfg.RepoAutoMergeEnabled(repo) {
			return nil
		}
		return cfg.PromotionRulesFor(repo)
	}
}

// promotionBranchPR reports whether an auto_merge.promotions rule names pr
// (its base, author and head ref; no GitHub call), matched or not. The hive
// never writes to such a branch: the workflow that opened it owns it, and a
// hive merge of the target into it would take the head off the source
// branch for good. The hold guard's keep-mergeable update and the stale-red
// base refresh skip it, as the automerge lanes do.
func promotionBranchPR(cfg *config.Config, pr github.PullRequest) bool {
	if cfg == nil {
		return false
	}
	repo := fullRepoName(pr.Repo, cfg.Project.Org)
	for _, rule := range cfg.AutoMerge.PromotionRulesFor(repo) {
		if strings.TrimSpace(pr.BaseRef) == rule.Target && strings.HasPrefix(strings.TrimSpace(pr.HeadRef), rule.HeadRefPrefix) && rule.HasAuthor(pr.Author) {
			return true
		}
	}
	return false
}

// withoutPromotionBranches is prs less the ones promotionBranchPR names.
func withoutPromotionBranches(cfg *config.Config, prs []github.PullRequest) []github.PullRequest {
	var out []github.PullRequest
	for _, pr := range prs {
		if !promotionBranchPR(cfg, pr) {
			out = append(out, pr)
		}
	}
	return out
}
