package github

// Branch-promotion pull requests (auto_merge.promotions).
//
// Many repositories promote one protected branch to another with a pull
// request a workflow opens, main into stable for example. Every change in it
// already merged to the source branch through the hive's own review,
// sentinel and CI, so the check a person makes before merging it is
// mechanical: the head's tree is the source branch's tree, and the checks
// are green. This file is that check, and nothing else.
//
// A rule (config.PromotionRule) only names which pull requests to check:
// base branch, author and head-ref prefix (PromotionRuleFor, no GitHub
// call). The proof is VerifyPromotion, which reads GitHub live every time
// it is asked:
//
//   - GET the source branch: its tip SHA, that commit's tree SHA and whether
//     the branch is protected. The transport's ETag layer only ever
//     revalidates with GitHub (a 304 means GitHub says the branch has not
//     moved), so this is never a stale read.
//   - The source must take changes only through a pull request, because
//     trust carries over: the target inherits exactly the source's review
//     guarantee, with the approvals and bypass actors the owner configured
//     (promotionSourceProtection). Anything else, and any read error, is
//     PromotionReasonSourceUnprotected.
//   - The head commit's tree must equal the source tip's tree, and the head
//     must have one of two shapes: the source tip or an ancestor of it (a
//     workflow that pushes the source to the promotion branch), or a single
//     commit whose only parent is the live target tip (a workflow that runs
//     `git commit-tree <source tree> -p <target>`). When the head is the
//     source tip the branch read is the whole content cost; otherwise the
//     head commit is read, then the target branch or one compare.
//
// Anything else, including any read error, is no match: the pull request
// takes the path it takes today. A match is never cached for a merge: the
// merge lane asks again immediately before the merge call, and then also
// checks the merge result (VerifyPromotionMerge): GitHub's test merge of
// the head into the target must carry exactly the source tip's tree, with
// the live target tip and the checked head as its parents. On a target that
// has diverged from the source (every squash promote does) the merge is a
// three-way merge, so a head equal to the source does not by itself make
// the target equal to the source; a revert on the source, for example, does
// not carry over.

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// Promotion check outcomes. They are stable strings: they appear in the
// merge lane's skip reason, the sentinel's log line and the audit detail.
const (
	// PromotionReasonTreeMismatch: the head's tree is not the source tip's
	// tree (the source moved, or the head carries other content).
	PromotionReasonTreeMismatch = "promotion-tree-mismatch"
	// PromotionReasonNotOnSource: the head brings a commit the source
	// branch does not have.
	PromotionReasonNotOnSource = "promotion-head-not-on-source"
	// PromotionReasonReadFailed: GitHub could not be read, so nothing is
	// proven.
	PromotionReasonReadFailed = "promotion-read-failed"
	// PromotionReasonMissingHead: the pull request reports no head commit.
	PromotionReasonMissingHead = "promotion-missing-head"
	// PromotionReasonSourceUnprotected: the source branch does not take
	// changes only through a pull request (no classic protection requiring
	// pull request reviews without force pushes, and no active ruleset
	// pull_request rule), or that could not be read.
	PromotionReasonSourceUnprotected = "promotion-source-unprotected"
	// PromotionReasonMergeTreeMismatch: GitHub's test merge of the head into
	// the target does not carry the source tip's tree.
	PromotionReasonMergeTreeMismatch = "promotion-merge-tree-mismatch"
	// PromotionReasonHeadShape: the head carries the source tip's tree but is
	// neither the source tip (or an ancestor of it) nor a single commit on
	// the live target tip.
	PromotionReasonHeadShape = "promotion-head-shape"
	// PromotionReasonMergeUnknown: GitHub has not computed the test merge for
	// the current target tip (mergeable unknown, no merge commit, or a merge
	// commit built on an older target tip), or it could not be read.
	PromotionReasonMergeUnknown = "promotion-merge-unknown"
)

// PromotionCheck is the outcome of one live VerifyPromotion call.
type PromotionCheck struct {
	// Matched is true only when the head's tree equals the source tip's tree
	// and the head is the source tip or one of its ancestors.
	Matched bool
	// Reason is one of the PromotionReason* outcomes when Matched is false.
	Reason string
	Rule   config.PromotionRule
	// HeadSHA is the pull request head that was checked.
	HeadSHA string
	// SourceSHA and TreeSHA are the source tip and its tree as read.
	SourceSHA string
	TreeSHA   string
	// HeadTreeSHA is the head commit's tree, when it was read.
	HeadTreeSHA string
	// Protection names what made the source count as protected:
	// "branch-protection" or "ruleset".
	Protection string
	// Shape is the accepted head shape (PromotionShape*) when Matched.
	Shape string
	// MergeSHA and MergeTreeSHA are GitHub's test merge commit and its tree,
	// when VerifyPromotionMerge read them; TargetSHA is the live target tip.
	MergeSHA     string
	MergeTreeSHA string
	TargetSHA    string
}

// Summary is the one-line evidence a matching check records:
// "promotion of already-reviewed content, tree == main at <sha>".
func (p PromotionCheck) Summary() string {
	return "promotion of already-reviewed content, tree == " + p.Rule.Source + " at " + shortPromotionSHA(p.SourceSHA)
}

func shortPromotionSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// PromotionRuleFor returns the rule among rules that pr names: its base
// branch is the rule's target, its author is one of the rule's authors
// (exact login), its head ref starts with the rule's head_ref_prefix, and
// its head branch lives in owner/repo itself (never a fork). It reads only
// the pull request payload. A match here proves nothing about content;
// VerifyPromotion does.
func PromotionRuleFor(rules []config.PromotionRule, owner, repo string, pr *gh.PullRequest) (config.PromotionRule, bool) {
	if len(rules) == 0 || pr == nil || pr.GetHead() == nil || pr.GetBase() == nil {
		return config.PromotionRule{}, false
	}
	headRepo := pr.GetHead().GetRepo()
	if headRepo == nil || !strings.EqualFold(strings.TrimSpace(headRepo.GetFullName()), owner+"/"+repo) {
		return config.PromotionRule{}, false
	}
	base := strings.TrimSpace(pr.GetBase().GetRef())
	ref := strings.TrimSpace(pr.GetHead().GetRef())
	author := SafeGetLogin(pr.GetUser())
	for _, rule := range rules {
		rule = rule.Normalized()
		if rule.Source == "" || rule.Target == "" || rule.HeadRefPrefix == "" {
			continue
		}
		if base != rule.Target || !strings.HasPrefix(ref, rule.HeadRefPrefix) || !rule.HasAuthor(author) {
			continue
		}
		return rule, true
	}
	return config.PromotionRule{}, false
}

// Promotion head shapes VerifyPromotion accepts (PromotionCheck.Shape).
const (
	// PromotionShapeSourceTip: the head is the source tip or one of its
	// ancestors, so every commit it brings is on the source branch.
	PromotionShapeSourceTip = "source-tip-or-ancestor"
	// PromotionShapeTargetChild: the head is one commit, built on the live
	// target tip, carrying the source tip's tree (a workflow running
	// `git commit-tree <source tree> -p <target>`). It brings exactly one
	// commit, whose content is the source branch's.
	PromotionShapeTargetChild = "single-commit-on-target-tip"
)

// VerifyPromotion proves, against GitHub now, that pr's head carries
// exactly the current content of rule.Source, that rule.Source only takes
// changes through a pull request (promotionSourceProtection), and that the
// head has one of the two accepted shapes:
//
//   - the head is the source tip or one of its ancestors (a workflow that
//     pushes the source to the promotion branch); or
//   - the head is a single commit whose only parent is the live target tip
//     (a workflow that commits the source tree onto the target).
//
// In both the head commit's tree must equal the source tip's tree. Anything
// else is no match: a tree that differs is PromotionReasonTreeMismatch, a
// head with the right tree in neither shape PromotionReasonHeadShape (or
// PromotionReasonNotOnSource when the compare shows commits the source
// lacks and the head is not built on the target tip). The error is the read
// failure behind a PromotionReasonReadFailed or
// PromotionReasonSourceUnprotected outcome; the check is never Matched with
// an error. Cost: the source branch read (plus the protection reads,
// promotionSourceProtection); nothing more when the head is the source tip;
// otherwise the head commit, and then the target branch or one compare.
func VerifyPromotion(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest, rule config.PromotionRule) (PromotionCheck, error) {
	rule = rule.Normalized()
	out := PromotionCheck{Rule: rule}
	if pr != nil && pr.GetHead() != nil {
		out.HeadSHA = strings.TrimSpace(pr.GetHead().GetSHA())
	}
	if out.HeadSHA == "" {
		out.Reason = PromotionReasonMissingHead
		return out, nil
	}
	if client == nil || rule.Source == "" || rule.Target == "" {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("promotion check: no GitHub client, source or target branch")
	}
	ctx = WithRESTCaller(ctx, "hive:promotion_check")
	branch, _, err := client.Repositories.GetBranch(ctx, owner, repo, rule.Source, 0)
	if err != nil {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("reading branch %s: %w", rule.Source, err)
	}
	out.SourceSHA = strings.TrimSpace(branch.GetCommit().GetSHA())
	out.TreeSHA = strings.TrimSpace(branch.GetCommit().GetCommit().GetTree().GetSHA())
	if out.SourceSHA == "" || out.TreeSHA == "" {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("branch %s reported no tip commit or tree", rule.Source)
	}
	protection, err := promotionSourceProtection(ctx, client, owner, repo, rule.Source, branch)
	if protection == "" {
		out.Reason = PromotionReasonSourceUnprotected
		return out, err
	}
	out.Protection = protection
	if out.SourceSHA == out.HeadSHA {
		// The head is the source tip: its tree is the tip's tree and every
		// commit it brings is on the source branch.
		out.HeadTreeSHA = out.TreeSHA
		out.Shape = PromotionShapeSourceTip
		out.Matched = true
		return out, nil
	}
	head, _, err := client.Git.GetCommit(ctx, owner, repo, out.HeadSHA)
	if err != nil {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("reading the head commit %s: %w", shortPromotionSHA(out.HeadSHA), err)
	}
	out.HeadTreeSHA = strings.TrimSpace(head.GetTree().GetSHA())
	if out.HeadTreeSHA == "" {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("the head commit %s reported no tree", shortPromotionSHA(out.HeadSHA))
	}
	if out.HeadTreeSHA != out.TreeSHA {
		out.Reason = PromotionReasonTreeMismatch
		return out, nil
	}
	if len(head.Parents) == 1 {
		target, _, err := client.Repositories.GetBranch(ctx, owner, repo, rule.Target, 0)
		if err != nil {
			out.Reason = PromotionReasonReadFailed
			return out, fmt.Errorf("reading branch %s: %w", rule.Target, err)
		}
		out.TargetSHA = strings.TrimSpace(target.GetCommit().GetSHA())
		if out.TargetSHA != "" && strings.TrimSpace(head.Parents[0].GetSHA()) == out.TargetSHA {
			out.Shape = PromotionShapeTargetChild
			out.Matched = true
			return out, nil
		}
	}
	cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, out.SourceSHA, out.HeadSHA, &gh.ListOptions{PerPage: 1})
	if err != nil {
		out.Reason = PromotionReasonReadFailed
		return out, fmt.Errorf("comparing %s...%s: %w", shortPromotionSHA(out.SourceSHA), shortPromotionSHA(out.HeadSHA), err)
	}
	status := strings.ToLower(strings.TrimSpace(cmp.GetStatus()))
	if cmp.GetAheadBy() != 0 || (status != "behind" && status != "identical") {
		out.Reason = PromotionReasonNotOnSource
		if len(head.Parents) == 1 {
			// The right tree on a commit built on some other target state
			// (an older tip, or a chain of commits).
			out.Reason = PromotionReasonHeadShape
		}
		return out, nil
	}
	out.Shape = PromotionShapeSourceTip
	out.Matched = true
	return out, nil
}

// promotionSourceProtection names what makes the source branch take changes
// only through a pull request, or "" when nothing does or it cannot be
// told. Two sources count:
//
//   - classic branch protection whose settings (GET
//     branches/{source}/protection, which needs administration read) require
//     pull request reviews (any number of approvals, zero included) and do
//     not allow force pushes;
//   - an active ruleset pull_request rule (GET rules/branches/{source}; any
//     number of approvals). Ruleset force pushes are not checked: a
//     pull_request rule refuses every direct ref update by a non-bypass
//     actor, force pushes included.
//
// The approvals and the bypass actors are whatever the owner configured:
// the target inherits exactly that guarantee. A ruleset update rule alone
// is not enough (it leaves the branch to its bypass actors), nor are status
// checks, signatures or linear history (a direct push still lands content
// no pull request carried). A protection read that fails (403 without
// administration read, 404 for a branch protected only by rulesets) falls
// through to the ruleset read; a ruleset read that fails is "" with the
// error.
func promotionSourceProtection(ctx context.Context, client *gh.Client, owner, repo, source string, branch *gh.Branch) (string, error) {
	if branch.GetProtected() {
		if p, _, err := client.Repositories.GetBranchProtection(ctx, owner, repo, source); err == nil && p != nil &&
			p.RequiredPullRequestReviews != nil && (p.AllowForcePushes == nil || !p.AllowForcePushes.Enabled) {
			return "branch-protection", nil
		}
	}
	rules, _, err := client.Repositories.GetRulesForBranch(ctx, owner, repo, source, nil)
	if err != nil {
		return "", fmt.Errorf("reading the rules for branch %s: %w", source, err)
	}
	if rules != nil && len(rules.PullRequest) > 0 {
		return "ruleset", nil
	}
	return "", nil
}

// VerifyPromotionMerge checks the merge result of a matching promotion
// (check.Matched, from VerifyPromotion on the same pr): GitHub's test merge
// commit (pr.merge_commit_sha, computed once mergeable is known) must have
// exactly the source tip's tree, and its parents must be the live tip of
// the target branch and the checked head. On a target that diverged from
// the source the test merge is a three-way merge, so a head equal to the
// source does not by itself make the target equal to it (a revert on the
// source, for example, does not carry over). It costs two REST calls (the
// target branch and the merge commit). Unknown mergeability, a missing or
// stale test merge, or any read error is PromotionReasonMergeUnknown; a
// different tree or parent set is PromotionReasonMergeTreeMismatch. The
// returned check carries the merge evidence and Matched only when it holds.
func VerifyPromotionMerge(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest, check PromotionCheck) (PromotionCheck, error) {
	out := check
	out.Matched = false
	if !check.Matched {
		return out, nil
	}
	fail := func(reason string, err error) (PromotionCheck, error) {
		out.Reason = reason
		return out, err
	}
	if client == nil || pr == nil || pr.Mergeable == nil {
		return fail(PromotionReasonMergeUnknown, nil)
	}
	if !pr.GetMergeable() {
		return fail(PromotionReasonMergeTreeMismatch, nil)
	}
	if strings.TrimSpace(pr.GetHead().GetSHA()) != check.HeadSHA {
		return fail(PromotionReasonMergeUnknown, nil)
	}
	out.MergeSHA = strings.TrimSpace(pr.GetMergeCommitSHA())
	target := strings.TrimSpace(pr.GetBase().GetRef())
	if out.MergeSHA == "" || target == "" {
		return fail(PromotionReasonMergeUnknown, nil)
	}
	ctx = WithRESTCaller(ctx, "hive:promotion_check")
	tip, _, err := client.Repositories.GetBranch(ctx, owner, repo, target, 0)
	if err != nil {
		return fail(PromotionReasonMergeUnknown, fmt.Errorf("reading branch %s: %w", target, err))
	}
	out.TargetSHA = strings.TrimSpace(tip.GetCommit().GetSHA())
	merge, _, err := client.Git.GetCommit(ctx, owner, repo, out.MergeSHA)
	if err != nil {
		return fail(PromotionReasonMergeUnknown, fmt.Errorf("reading the test merge %s: %w", shortPromotionSHA(out.MergeSHA), err))
	}
	out.MergeTreeSHA = strings.TrimSpace(merge.GetTree().GetSHA())
	parents := merge.Parents
	if len(parents) != 2 || out.TargetSHA == "" || strings.TrimSpace(parents[0].GetSHA()) != out.TargetSHA {
		// Built on another target tip, or not a merge of the two: GitHub
		// recomputes it, and the next pass reads it again.
		return fail(PromotionReasonMergeUnknown, nil)
	}
	if check.Shape == PromotionShapeTargetChild && check.TargetSHA != "" && check.TargetSHA != out.TargetSHA {
		// The head was built on a target tip that has since moved.
		return fail(PromotionReasonHeadShape, nil)
	}
	if strings.TrimSpace(parents[1].GetSHA()) != check.HeadSHA || out.MergeTreeSHA == "" || out.MergeTreeSHA != check.TreeSHA {
		return fail(PromotionReasonMergeTreeMismatch, nil)
	}
	out.Matched = true
	out.Reason = ""
	return out, nil
}

// PromotionAuditDetail is the audit detail pairs for one promotion
// decision: decision, head, source, source_sha and tree.
func PromotionAuditDetail(decision string, check PromotionCheck) []string {
	out := []string{
		"decision", decision,
		"head", check.HeadSHA,
		"source", check.Rule.Source,
		"source_sha", check.SourceSHA,
		"tree", check.TreeSHA,
	}
	if check.MergeSHA != "" {
		out = append(out, "merge_commit", check.MergeSHA, "merge_tree", check.MergeTreeSHA, "target_sha", check.TargetSHA)
	}
	return out
}

// RecordPromotionDecision records one auto_merge.promotions decision on
// repo#number in the audit log (AuditActionPromotionDecision).
func (c *Client) RecordPromotionDecision(repo string, number int, decision string, check PromotionCheck) {
	if c == nil {
		return
	}
	c.recordWriteAudit(AuditActionPromotionDecision, InvocationMeta{Agent: AttributionAgentGovernor},
		WriteTarget{Repo: repo, Number: number}, PromotionAuditDetail(decision, check)...)
}
