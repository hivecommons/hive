package dashboard

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// acmm:merge-queue is credited from the capability, not a file name (#10891).
// The criterion ID is shared with the upstream ACMM scanner and never changes.
const (
	acmmMergeQueueID = "acmm:merge-queue"
	// SatisfiedBy values; the dashboard renders them as "satisfied by <value>".
	acmmMergeQueueProviderNative = "GitHub merge queue"
	acmmMergeQueueProviderLane   = "Hive serialized merge lane"
	// acmmOwnerTypeUser is GitHub's owner.type for a personal account.
	acmmOwnerTypeUser = "User"
)

// acmmMergeQueueCapability decides capability credit for acmm:merge-queue on
// the default branch: GitHub's native merge queue, or the hive-serialized
// strategy together with a known, non-empty required-check set. When the lane
// is on but earns no credit, reason carries rules.Reason so the row can say
// why.
func acmmMergeQueueCapability(strategy, branch string, rules github.BranchRulesResult) (provider, reason string, ok bool) {
	if rules.MergeQueueKnown && rules.MergeQueue {
		return acmmMergeQueueProviderNative, fmt.Sprintf("GitHub merge queue is on for %s", branch), true
	}
	if strategy == config.MergeStrategyHiveSerialized {
		if rules.Known && len(rules.Required) > 0 {
			return acmmMergeQueueProviderLane, fmt.Sprintf("merge_strategy %s with required checks on %s: %s",
				config.MergeStrategyHiveSerialized, branch, strings.Join(rules.SortedRequired(), ", ")), true
		}
		return "", rules.Reason, false
	}
	return "", "", false
}

// acmmMergeQueueCredit reads the repo's default branch and its rules and
// returns capability credit for acmm:merge-queue. Any read failure means no
// credit; the file check (and waivers) still apply as before.
func (s *Server) acmmMergeQueueCredit(ctx context.Context, client *gh.Client, owner, repo string) (provider, reason string, ok bool) {
	if client == nil || s.deps == nil || s.deps.Config == nil {
		return "", "", false
	}
	info, _, err := client.Repositories.Get(github.WithRESTCaller(ctx, "hive:acmm_merge_queue"), owner, repo)
	if err != nil || info == nil {
		return "", "", false
	}
	branch := info.GetDefaultBranch()
	if branch == "" {
		return "", "", false
	}
	set, known := s.deps.Config.AutoMerge.RequiredCheckSet()
	rules := github.ReadBranchRules(ctx, client, owner, repo, branch, set, known)
	return acmmMergeQueueCapability(s.deps.Config.RepoMergeStrategy(repo), branch, rules)
}

// applyMergeQueueCredit sets capability credit on res, or marks a pass that
// rests only on a marker file. A file-only pass keeps passing (no readiness
// regression); it is only labelled.
func (s *Server) applyMergeQueueCredit(ctx context.Context, client *gh.Client, owner, repo string, res *CriterionResult) {
	provider, reason, ok := s.acmmMergeQueueCredit(ctx, client, owner, repo)
	if ok {
		res.Passed = true
		res.SatisfiedBy = provider
		res.SatisfiedReason = reason
		return
	}
	if res.Passed {
		res.FileOnly = true
		return
	}
	res.UnsatisfiedReason = reason
}

// acmmRepoOwnedByUser reports whether owner/repo belongs to a personal
// account. Any failure to tell answers false, so the ticket stays the one
// organization repositories get today.
func (s *Server) acmmRepoOwnedByUser(ctx context.Context, owner, repo string) bool {
	if s.deps == nil || s.deps.GHClient == nil {
		return false
	}
	client := s.deps.GHClient.GoGitHub()
	if client == nil {
		return false
	}
	info, _, err := client.Repositories.Get(github.WithRESTCaller(ctx, "hive:acmm_owner_type"), owner, repo)
	if err != nil || info == nil {
		return false
	}
	return info.GetOwner().GetType() == acmmOwnerTypeUser
}

// acmmIssueLabels returns the labels a GitHub gap issue carries. The
// personal-account merge-queue ticket drops ai-fix-requested: its fix is an
// owner setting, not code an agent can write.
func acmmIssueLabels(personalMergeQueue bool) []string {
	if personalMergeQueue {
		return []string{acmmIssueLabelName}
	}
	return []string{acmmIssueLabelName, "ai-fix-requested"}
}

// acmmMergeQueuePersonalIssueContent is the gap ticket for acmm:merge-queue on
// a personal-account repository. GitHub's merge queue is not available there,
// so the ticket points at the serialized merge lane and required checks
// instead of asking for a file.
func acmmMergeQueuePersonalIssueContent(criterion *ACMMCriterion, repo string) (title, body string) {
	levelName := acmmLevelNames[criterion.Level]
	if levelName == "" {
		levelName = "Unknown"
	}
	title = fmt.Sprintf("[ACMM L%d] Turn on serialized merging (%s)", criterion.Level, criterion.Name)
	body = fmt.Sprintf("## ACMM Gap: %s\n\n"+
		"**Level:** L%d %s\n"+
		"**Category:** %s\n"+
		"**Criterion ID:** `%s`\n\n"+
		"### What's needed\n\n"+
		"This repository is owned by a personal account. GitHub's merge queue is only "+
		"available to repositories owned by an organization, so it cannot be turned on here.\n\n"+
		"Hive's serialized merge lane, together with required checks, satisfies this criterion "+
		"instead: when this repository's merge strategy is `%s` and its default branch has at "+
		"least one required status check, the ACMM evaluation credits the criterion as "+
		"\"satisfied by %s\" and it counts toward ACMM Level %d (%s).\n\n"+
		"### Why it matters\n\n"+
		"Serialized merging lands one pull request at a time and validates it against the "+
		"current base first, so two agent pull requests that pass on their own cannot break "+
		"the default branch together.\n\n"+
		"### How to fix\n\n"+
		"This is a setting a repository owner changes, not code:\n\n"+
		"1. In the Hive dashboard's ACMM evaluation, open the %s row for `%s` and choose "+
		"**Use serialized merge lane** (or `POST /api/repos/merge-strategy` with "+
		"`{\"repo\": \"%s\", \"merge_strategy\": \"%s\"}`).\n"+
		"2. Make sure the default branch requires at least one status check, in a branch "+
		"ruleset or in branch protection.\n\n"+
		"---\n"+
		"*Opened by Hive ACMM Evaluation*",
		criterion.Name,
		criterion.Level, levelName,
		criterion.Category,
		criterion.ID,
		config.MergeStrategyHiveSerialized,
		acmmMergeQueueProviderLane,
		criterion.Level, levelName,
		criterion.Name, repo,
		repo, config.MergeStrategyHiveSerialized,
	)
	return title, body
}
