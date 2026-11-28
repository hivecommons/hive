package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/rebasemerge"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// applyStageDriverOptions installs every automerge.Options field the
// stage-label pass (automerge.ApplyStageLabels and what it calls) reads
// whose value does not depend on which driver runs the pass. Two drivers
// run that pass: the engine loop (bootAgentsWith, main.go) and the
// governor's automerge sweep (governorAutoMergeOptions, main_sweeps.go).
// Before this helper each built its own options, and the governor's lacked
// FixDispatchStatus and RebaseEpisode: its pass saw the hive's fixer as
// unknown and routed an exhausted-fixer PR back to 070 while the engine
// loop routed the same head to 085 fixer-exhausted:N, until the dampener
// froze it at 070 with nobody on it (hivecommons/hive#15744 regression).
//
// Driver-specific stage inputs (Logger, ReviewGate's audit sink,
// ContributorNudge, MutationBoundary, OpenPRs) stay at each driver.
// TestStageDriversShareStageOptions guards the split: a stage field set at
// one driver only fails it.
//
// Every callback reads through cfg on each call, so a reload (which copies
// into *cfg) applies without a restart; MinHeadAge and RequiredChecks are
// read when the options are built, as each driver did before.
func applyStageDriverOptions(opts *automerge.Options, cfg *config.Config, logger *slog.Logger) {
	if opts == nil {
		return
	}
	// SECURITY (audit F3): the merger tier is re-verified inside the sweep
	// against the same allowlist the dashboard's queue endpoint uses.
	opts.MergerAuthorizer = trustedMergerFunc(cfg)
	opts.HumanChangeRequests = review.DefaultHumanChangeRequests()
	opts.TrustedAuthorizer = trustedAuthorFunc(cfg)
	opts.TrustedAuthorPolicy = trustedAuthorPolicyFunc(cfg)
	opts.ContributorLanePolicy = contributorLanePolicyFunc(cfg)
	opts.ContributorLaneVerdict = contributorLaneVerdictFunc(cfg, logger)
	opts.ContributorRegistryTier = contributorRegistryTierFunc()
	opts.StageLabelPolicy = stageLabelPolicyFunc(cfg)
	opts.StageEngine = stageEngineOptions(cfg, logger)
	// The review fixer's standing on a head (hivecommons/hive#15744): an
	// exhausted fixer parks the hive's own PR at 085 fixer-exhausted:N.
	opts.FixDispatchStatus = fixDispatchStatusReader()
	// Merge train (#15117) and adoption (#15307): a member or copy that
	// would not replay onto its base is handed to the fixer's
	// rebase-resolve episode under the shared two-attempt guard (#15104);
	// RebaseEpisode reads that guard back. Both are process-shared, so the
	// two drivers see one episode per PR.
	opts.RebaseResolver = automerge.RebaseResolverFunc(func(_ context.Context, req automerge.RebaseRequest) error {
		requester := mergeTrainRebaseRequester
		if req.RequestedBy != "" {
			requester = req.RequestedBy
		}
		rebasemerge.SharedAttempts().Request(req.Repo, req.Number, req.HeadSHA, requester, time.Now())
		return nil
	})
	opts.RebaseEpisode = func(repo string, number int) (automerge.RebaseEpisode, bool) {
		a, ok := rebasemerge.SharedAttempts().Get(repo, number)
		if !ok {
			return automerge.RebaseEpisode{}, false
		}
		return automerge.RebaseEpisode{Attempt: a.Count(), MaxAttempts: rebasemerge.MaxRebaseAttempts, Exhausted: a.Exhausted(), RequestedBy: a.RequestedBy}, true
	}
	if cfg == nil {
		return
	}
	opts.SentinelLabel = func() string { return cfg.Sentinel.LabelOrDefault() }
	opts.HiveSupersedingAutoMerge = func() bool { return cfg.AutoMerge.HiveSuperseding.AutoMergeEnabled() }
	// commitGreen's required-checks gate: the operator-declared
	// auto_merge.required_checks list, if any, so naming required checks
	// does not depend on GitHub's required-status-checks branch-protection
	// API (older Hive App installations lack administration:read). Unset
	// leaves the API/allowlist fallback chain intact.
	if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
		opts.RequiredChecks = set
	}
	opts.RequiredChecksForRepo = cfg.AutoMerge.RequiredCheckSetForRepo
	opts.MinHeadAge = cfg.AutoMerge.EffectiveMinHeadAge()
	opts.RepoAutoMergeEnabled = func(repo string) bool { return cfg.RepoAutoMergeEnabled(repo) }
	opts.RepoAutoMergeSwitchedOff = func(repo string) bool { return cfg.RepoAutoMergeSwitchedOff(repo) }
	// auto_merge.trusted_bot_authors and review bot authors, read on every
	// call so a reload applies without a restart.
	opts.TrustedBotAuthors = func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() }
	opts.ReviewBotAuthors = func() []string { return cfg.Review.EffectiveBotAuthors() }
	// Branch-promotion rules (auto_merge.promotions): the self-authored
	// sweep's promotion lane and the other lanes' hand-off to it. The lane
	// is part of the self-authored sweep, so where that sweep does not run
	// (auto_merge.self_authored off, or below its ACMM level) no rule is
	// returned and every lane routes the pull request exactly as before.
	opts.Promotions = promotionRulesFunc(cfg)
	// The write guards read every configured rule, lane or no lane, so the
	// hive never updates a promotion branch (as promotionBranchPR does here).
	opts.PromotionBranches = func(repo string) []config.PromotionRule { return cfg.AutoMerge.PromotionRulesFor(repo) }
	// Human-merge paths (#11038).
	opts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(cfg, repo) }
	opts.HumanMergePinOnly = func(repo string) (config.PinOnlyException, bool) { return humanMergePinOnlyFor(cfg, repo) }
	opts.HumanMergeOwnerApproval = func(repo string) bool { return cfg.AutoMerge.HumanMergeOwnerApprovalFor(repo) }
	opts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) }
	opts.RequireCurrentBase = func(repo string) bool { return cfg.AutoMerge.RequireCurrentBaseFor(repo) }
}
