package automerge

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Transport is the small surface the sweep needs from the GitHub transport
// client. The policy state lives in this package; the client only supplies API
// access and transport-scoped metadata.
type Transport interface {
	GoGitHub() *gh.Client
	Repositories() []string
	SplitRepo(repo string) (owner, repoName string)
	AutoMergeLabel() string
	AppBotLogin() string
	IsExemptLabels(labels []string) bool
	// IsHeldLabels reports whether labels carry a hold the transport's owner
	// configured: the generic hold substrings plus the exact hive-scoped
	// dashboard pause label (`hive-pause/<hive-id>`). The sweep must use it
	// instead of the generic package predicate so a dashboard ⏸ Hold blocks
	// merges the same way it blocks enumeration (#8927).
	IsHeldLabels(labels []string) bool
	UpdateBranch(ctx context.Context, repo string, number int) error
	RecordPRMergedAudit(repo string, number int, method, sha, path string)
}

type levelHoldTransport interface {
	ReleaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error)
}

// supersededCloserTransport is the optional transport hook that closes a
// hive-authored PR as superseded when every issue it closes was already
// closed by a different merged PR and its head conflicts with the base
// (hgithub.Client.CloseSupersededHivePRIfEligible; hivecommons/hive#14965).
// The lane calls it on a conflicting self-authored PR before any verdict
// or gate is read, so no review cycle is spent on a PR with nothing left
// to deliver.
type supersededCloserTransport interface {
	CloseSupersededHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest) (hgithub.HiveSupersededClose, error)
}

// emptyDiffCloserTransport is the optional transport hook that closes a
// hive-authored PR whose compare against its base lists no files
// (hgithub.Client.CloseEmptyDiffHivePRIfEligible; hivecommons/hive#15499).
// The lane calls it when GitHub's GET payload reports changed_files 0 and
// the stage reconciler calls it before parking the hive's own PR on a
// person, so no review cycle or decision is spent on nothing to merge.
type emptyDiffCloserTransport interface {
	CloseEmptyDiffHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int) (hgithub.HiveEmptyDiffClose, error)
}

// autoMergeReasonClosedSuperseded is the skip reason of a conflicting
// self-authored PR the lane closed as superseded.
const autoMergeReasonClosedSuperseded = "closed-superseded"

// autoMergeReasonClosedEmptyDiff is the skip reason of a self-authored PR
// the lane closed because its diff against the base is empty.
const autoMergeReasonClosedEmptyDiff = "closed-empty-diff"

// autoMergeReasonLabelNeedsRebase is labelBlockReason's answer for the
// needs-rebase label.
const autoMergeReasonLabelNeedsRebase = "label:needs-rebase"

// Options carries policy dependencies owned by the caller.
type Options struct {
	// IntegrationAdmission may withhold a NEW native enqueue for a scoped CI
	// budget. It never replaces any gate, cancels work, or reorders the queue.
	IntegrationAdmission func(context.Context, string, *gh.PullRequest) (string, error)
	// IntegrationExecution routes a fully gated candidate to the explicitly
	// selected execution authority before either direct merge or native enqueue.
	IntegrationExecution   IntegrationExecutionHook
	IntegrationSelfAllowed func() bool
	// BasePreparation persists front-only final preparation across all lanes.
	// nil uses the shared durable store.
	BasePreparation  *BasePreparationStore
	Logger           *slog.Logger
	MergerAuthorizer MergerAuthorizer
	RequiredChecks   map[string]bool
	// RequiredChecksForRepo returns the config-declared fallback required-check
	// set for owner/repo, including per-repo overrides.
	RequiredChecksForRepo func(repo string) (map[string]bool, bool)
	ApprovalDesk          hgithub.ApprovalDeskHook
	MutationBoundary      effects.Boundary
	// IntentGate is the intent-tier policy trySweepSelfAuthoredPR enforces
	// (#6258). nil installs no policy; see IntentGate for the semantics.
	IntentGate *IntentGate
	// ReviewGate is the swarm-review policy trySweepSelfAuthoredPR evaluates
	// on the App's own PRs (#12592). nil installs no policy; see ReviewGate.
	ReviewGate *ReviewGate
	// HumanChangeRequests receives a trusted maintainer's CHANGES_REQUESTED
	// review on a hive-authored PR head so the review planner can kick the
	// author agent with it (hivecommons/hive#13041). nil still parks the PR
	// at fixing; it just records nothing for the planner.
	HumanChangeRequests review.HumanChangeRequestStore
	// HumanMergePaths returns the live auto_merge.human_merge_paths patterns
	// for an owner/repo. Every sweep lane refuses a PR touching one (#11038).
	// nil means no repo has any.
	HumanMergePaths func(repo string) []string
	// HumanMergeRule returns the live auto_merge.human_merge_title_prefixes /
	// human_merge_labels rule (hivecommons/hive#15442). Every lane and the
	// merge train skip a PR RequiresHumanMerge. nil means the defaults.
	HumanMergeRule func() prstage.HumanMergeRule
	// RequireCurrentBase returns the live current-base gate switch for an
	// owner/repo (hivecommons/hive#13634; since #15335 resolved from the
	// hive-wide default plus per-repo overrides via
	// config.AutoMergeConfig.RequireCurrentBaseFor). nil means off.
	RequireCurrentBase func(repo string) bool
	// RequireCurrentBaseStrict reports whether owner/repo is explicitly
	// listed in auto_merge.require_current_base. A strict repo parks a head
	// the App cannot update; a repo gated only by the default merges the
	// green head as tested in that case, with one WARN per PR. nil means no
	// repo is strict.
	RequireCurrentBaseStrict func(repo string) bool
	// MergeForwardTrustedHuman returns the live
	// auto_merge.merge_forward_trusted_human membership for owner/repo
	// (hivecommons/hive#13636); nil means off.
	MergeForwardTrustedHuman func(repo string) bool
	// SelfAuthorizationHoldEnabled returns the live per-repo #5117 hold switch.
	// nil preserves the default-on behavior.
	SelfAuthorizationHoldEnabled      func(repo string) bool
	SelfAuthorizationHoldReleaseLimit int
	// RepoAutoMergeEnabled returns the live per-repo auto-merge switch. nil
	// preserves default-on behavior.
	RepoAutoMergeEnabled func(repo string) bool
	// RepoOnGitHub reports whether a repo lives on GitHub (its forge.Kind, via
	// config.RepoOnGitHub). Every lane merges through the GitHub API, so a repo
	// on GitLab or Gitea is left out of the sweep rather than looked up on
	// GitHub under its slug (hivecommons/hive#14150). nil means every repo is
	// on GitHub.
	RepoOnGitHub func(repo string) bool
	// TrustedBotAuthors returns the live lower-cased set of bot logins whose
	// PRs the self-authored sweep merges alongside the App's own (see
	// config.AutoMergeConfig.TrustedBotAuthors). nil means App-only.
	TrustedBotAuthors func() map[string]bool
	// BotTakeoverEnabled returns the live auto_merge.bot_takeover.enabled
	// switch: re-author a trusted github-actions[bot] PR whose
	// `pull_request` checks can never run (#12003). nil means off.
	BotTakeoverEnabled func() bool
	// TrustedAuthorPolicy returns the live, operator opt-in policy for the
	// human-authored tier. nil or disabled means the tier is off.
	TrustedAuthorPolicy func() TrustedAuthorPolicy
	// TrustedAuthorizer reports whether a PR author holds the policy's required
	// authorized-users role. nil fails closed.
	TrustedAuthorizer TrustedAuthorizer
	// ContributorLanePolicy returns the live contributor-lane tier policy
	// (auto_merge.contributor_lane.*). nil means the tier is off.
	ContributorLanePolicy func() ContributorLanePolicy
	// ContributorLaneVerdict reports the reviewer verdict for a PR head. nil
	// means no reviewer is configured, which satisfies the verdict gate.
	ContributorLaneVerdict ContributorLaneVerdictFunc
	// ContributorRegistryTier looks up a PR author's operator-set trust tier
	// in this hive's contributor registry for the contributor-lane registry
	// admission path. nil means only the claim-marker path admits.
	ContributorRegistryTier ContributorRegistryTierFunc
	// ContributorSoak overrides the process-wide soak tracker. nil shares
	// the package default so one-shot engines still soak across ticks.
	ContributorSoak *ContributorSoakTracker
	// HeadFirstSeen overrides the process-wide head-age tracker. nil shares
	// the package default so one-shot engines keep a head's pinned age.
	HeadFirstSeen *HeadFirstSeenTracker
	// SentinelLabel returns the configured sentinel alert label. The label also
	// travels through the transport's hold-label set, but the sweep names this
	// skip distinctly so operators can tell a sentinel block from an ordinary
	// hold in tick stats.
	SentinelLabel func() string
	// HiveSupersedingAutoMerge is the live auto_merge.hive_superseding.auto_merge
	// switch (owner rule: the hive's superseding copy of another PR
	// auto-merges with no operator decision). nil means on.
	HiveSupersedingAutoMerge func() bool
	// SentinelAcknowledgedBy returns the person whose approval acknowledged
	// the sentinel's findings on repo#number@head ("" when none stands),
	// for the superseder hard-stop; nil reads the review evidence bundle.
	SentinelAcknowledgedBy func(repo string, number int, head string) string
	// MinHeadAge is the youngest PR head commit age the sweep may merge when
	// the required-check set is not config-declared and fully green.
	MinHeadAge time.Duration
	Now        func() time.Time
	// StageLabelPolicy returns the live governor.labels.stage policy
	// (hivecommons/hive#12429). nil disables stage labelling entirely.
	StageLabelPolicy func() StageLabelPolicy
	// StageTracker collects per-PR lane observations for ApplyStageLabels;
	// nil shares the package default so one-shot lane engines feed one pass.
	StageTracker *StageTracker
	// StageStore persists stage timing (stalled/regressed); nil shares the
	// package default at stagelabels.StatePath.
	StageStore *stagelabels.Store
	// StageEngine configures the ADR-0025 reconciler ApplyStageLabels
	// routes every stage write through (hivecommons/hive#13008). Nil is the
	// default: stageengine.DefaultMode (enforce) over the transport's one
	// StageWriter; cmd/hive passes the process's shared Options so the mode
	// follows governor.labels.stage.engine.mode.
	StageEngine *stageengine.Options
	// ContributorNudge receives stage-072 nudges (hivecommons/hive#12436);
	// nil posts the GitHub comment only.
	ContributorNudge ContributorNudgeSink
	// CheckSummary reads a head's check digest for human-stage routing
	// (hivecommons/hive#12528); nil uses the GitHub client.
	CheckSummary CheckSummarizer
	// RateLimitCautious reports whether GitHub is pacing the hive after a
	// rate-limit refusal; nil reads the shared slow-start state.
	RateLimitCautious func() bool
	// OpenPRs is the forgestate cache the self-authored, trusted-author and
	// contributor lanes read instead of listing /pulls per tick
	// (hivecommons/hive#13377). With it set the lanes run event-driven: a
	// SweepTrigger notification evaluates the named PRs and a periodic floor
	// pass covers lost deliveries; the merge step reads the PR through with
	// a freshness bound and never acts on a stale snapshot. nil keeps the
	// listing ticker.
	OpenPRs OpenPRSource
	// SweepTrigger is the trigger webhook deliveries notify; nil with OpenPRs
	// set builds one, reachable through Engine.Trigger.
	SweepTrigger *SweepTrigger
	// MergeTrain returns the live auto_merge.merge_train.* policy
	// (hivecommons/hive#15117); nil means DefaultMergeTrainPolicy.
	MergeTrain func() MergeTrainPolicy
	// MergeTrainEnabled is the on/off-only form of MergeTrain kept for
	// callers wired before the policy existed (#15149); ignored when
	// MergeTrain is set.
	MergeTrainEnabled func() bool
	// NativeMergeQueue returns the live auto_merge.native_merge_queue.*
	// policy (hivecommons/hive#15198); nil means off, so no lane probes,
	// enqueues or withdraws a native merge queue entry.
	NativeMergeQueue func() NativeMergeQueuePolicy
	// RebaseResolver receives the train's rebase-resolve requests for a
	// conflicting PR; nil leaves a dirty front on its conflicting path.
	RebaseResolver RebaseResolver
	// RebaseEpisode reads the fixer's rebase-resolve episode for a PR (the
	// pkg/rebasemerge attempts ledger through cmd/hive): the adoption lane
	// parks a copy whose episode is exhausted at 085 instead of leaving it
	// on its fixing stage (hivecommons/hive#15307). nil reads as no episode.
	RebaseEpisode func(repo string, number int) (RebaseEpisode, bool)
	// FixDispatchStatus reads the review fixer's standing on a PR head from
	// the dispatch state (review.FixDispatchStatusFor through cmd/hive): a
	// hive PR whose fixer is exhausted parks at 085 instead of reading
	// "Fixing" with nobody on it (hivecommons/hive#15744). nil reads as
	// unknown and leaves the fixing stage alone.
	FixDispatchStatus func(repo string, number int, head string) (review.FixDispatchStatus, bool)
	// HotspotRecorder receives the merge train's PR file lists and the
	// conflicts it asks the resolver to repair, so the merge-hotspot
	// advisor (pkg/hotspot) derives its evidence without polling GitHub
	// again; nil records nothing.
	HotspotRecorder          HotspotRecorder
	IntegrationFilesObserved func(repo, head string, files []string)
}

// Engine owns the automerge sweep policy state.
type Engine struct {
	basePreparation *BasePreparationStore
	transport       Transport
	gh              *gh.Client
	logger          *slog.Logger

	inactiveQueueMu   sync.Mutex
	inactiveQueueLast map[string]time.Time

	// claimantsMu / claimants: the full open-PR list the legacy (no
	// forge-state source) /pulls listing returned per owner/name this
	// pass, kept before any event narrowing so the same-issue claim gate
	// sees every claimant (hivecommons/hive#15810).
	claimantsMu sync.Mutex
	claimants   map[string][]*gh.PullRequest

	nativeMergeQueuePolicy func() NativeMergeQueuePolicy
	integrationAdmission   func(context.Context, string, *gh.PullRequest) (string, error)
	integrationExecution   IntegrationExecutionHook
	integrationOptions     Options
	nativeQueueProbeMu     sync.Mutex
	nativeQueueProbes      map[string]nativeQueueProbe

	selfSkipWarnedMu sync.Mutex
	selfSkipWarned   map[string]struct{}

	mergerAuthzMu sync.RWMutex
	// humanReturnChecked remembers, per PR key, the head whose worn
	// 071/072 label was already attributed (see foldHumanReturnLabel), so
	// the label events are read once per head, not every pass.
	humanReturnCheckedMu sync.Mutex
	humanReturnChecked   map[string]string
	mergerAuthz          MergerAuthorizer

	// maintainerPerm caches whether a PR author holds admin or maintain on
	// the PR's repository (authorMaintainsRepo): adoption never takes over
	// a maintainer's own PR.
	maintainerPermMu sync.Mutex
	maintainerPerm   map[string]maintainerPermEntry

	requiredChecksMu      sync.RWMutex
	requiredChecks        map[string]bool
	requiredChecksForRepo func(repo string) (map[string]bool, bool)

	approvalDesk hgithub.ApprovalDeskHook
	mutation     effects.Boundary

	intentGateMu sync.RWMutex
	intentGate   *IntentGate
	reviewGateMu sync.RWMutex
	reviewGate   *ReviewGate

	humanChangeRequests review.HumanChangeRequestStore
	// baseRefresh memoises whether a PR head is the hive's own base refresh
	// of the head a decision was recorded on (automerge_decision_carry.go).
	baseRefresh hgithub.BaseRefreshChecker

	humanMergePathsMu sync.RWMutex
	humanMergePaths   func(repo string) []string

	humanMergeRuleMu sync.RWMutex
	humanMergeRule   func() prstage.HumanMergeRule

	requireCurrentBase       func(repo string) bool
	requireCurrentBaseStrict func(repo string) bool
	mergeForwardTrustedHuman func(repo string) bool

	currentBaseFallbackWarnedMu sync.Mutex
	currentBaseFallbackWarned   map[string]struct{}

	selfAuthorizationHoldEnabled      func(repo string) bool
	selfAuthorizationHoldReleaseLimit int
	repoAutoMergeEnabled              func(repo string) bool
	repoOnGitHub                      func(repo string) bool
	trustedBotAuthors                 func() map[string]bool
	botTakeoverEnabledFn              func() bool
	trustedAuthorPolicy               func() TrustedAuthorPolicy
	trustedAuthorizer                 TrustedAuthorizer
	contributorLanePolicy             func() ContributorLanePolicy
	contributorRegistryTier           ContributorRegistryTierFunc
	contributorLaneVerdict            ContributorLaneVerdictFunc
	contributorSoak                   *ContributorSoakTracker
	headFirstSeen                     *HeadFirstSeenTracker
	sentinelLabel                     func() string
	hiveSupersedingAutoMerge          func() bool
	sentinelAcknowledgedBy            func(repo string, number int, head string) string
	minHeadAge                        time.Duration
	now                               func() time.Time
	evaluatedHeadsMu                  sync.Mutex
	evaluatedHeads                    map[string]string
	requiredChecksFallbackWarnedMu    sync.Mutex
	requiredChecksFallbackWarned      map[string]bool
	// heldRunApprovals is the shared per-PR-head one-shot map for rebase
	// nudges and bot takeovers (historic name; workflow approvals no longer use it).
	heldRunApprovalsMu sync.Mutex
	heldRunApprovals   map[string]bool
	// workflowApprovalCommands records the first time this process saw and
	// commented /ok-to-test for a PR head. The mutation boundary dedupes the
	// actual comment across restarts; this clock drives the missing-caller grace.
	workflowApprovalCommandsMu sync.Mutex
	workflowApprovalCommands   map[string]time.Time
	// workflowApprovalScans is the last time this process read a PR's
	// comments for the hive's own /ok-to-test posts and the merge bot's
	// rejection reply; workflowApprovalUntrusted remembers PRs whose merge
	// bot refused the App so no later head posts again (per PR, not head).
	// workflowApprovalWorkflowChecks caches whether a repository carries
	// the hive-ok-to-test workflow (automerge_held_runs.go).
	workflowApprovalScans          map[string]time.Time
	workflowApprovalUntrusted      map[string]bool
	workflowApprovalWorkflowChecks map[string]okToTestWorkflowCheck
	// terminalHoldsLogged remembers PR head+reason pairs already reported as a
	// terminal held/never-started hold so later ticks only count them.
	terminalHoldsLogged map[string]bool
	// unauthorized remembers, per PR, the head and step this process marked
	// `hive:unauthorized` for, so the same head logs once and the mark
	// clears when the step is done; unauthorizedLabelReady remembers the
	// repositories whose label is provisioned (automerge_unauthorized.go).
	unauthorizedMu         sync.Mutex
	unauthorized           map[string]unauthorizedState
	unauthorizedLabelReady map[string]bool
	// mergeQueueBases remembers base branches whose ruleset refused a direct
	// merge as merge-queue-only, so the self-authored sweep stops re-issuing
	// a REST merge that cannot succeed (hivecommons/hive#14068).
	mergeQueueBasesMu sync.Mutex
	mergeQueueBases   map[string]time.Time
	// mergeRefusals remembers merges GitHub refused because an approving
	// review is required (per PR head) or the merge method is disabled (per
	// repository and method), so no sweep re-issues them every pass
	// (automerge_merge_refusal.go).
	mergeRefusalsMu sync.Mutex
	mergeRefusals   map[string]mergeRefusal

	stageLabelPolicy  func() StageLabelPolicy
	stageTracker      *StageTracker
	stageStore        *stagelabels.Store
	stageEngine       *stageengine.Options
	contributorNudge  ContributorNudgeSink
	checkSummary      CheckSummarizer
	rateLimitCautious func() bool

	openPRs      OpenPRSource
	sweepTrigger *SweepTrigger
	// sweepFloor overrides automergeSweepFloorInterval (tests only).
	sweepFloor time.Duration
	// sweepClock records the lanes' passes for the dashboard (sweep_clock.go).
	sweepClock *SweepClock

	mergeTrainPolicy         func() MergeTrainPolicy
	rebaseResolver           RebaseResolver
	rebaseEpisode            func(repo string, number int) (RebaseEpisode, bool)
	fixDispatch              func(repo string, number int, head string) (review.FixDispatchStatus, bool)
	hotspotRecorder          HotspotRecorder
	integrationFilesObserved func(repo, head string, files []string)
	mergeTrainState
}

// New returns an automerge sweep engine over a GitHub transport client.
func New(transport Transport, opts Options) *Engine {
	var ghClient *gh.Client
	if transport != nil {
		ghClient = transport.GoGitHub()
	}
	e := &Engine{
		basePreparation:                   opts.BasePreparation,
		transport:                         transport,
		gh:                                ghClient,
		logger:                            opts.Logger,
		sweepClock:                        defaultSweepClock,
		mergerAuthz:                       opts.MergerAuthorizer,
		requiredChecks:                    opts.RequiredChecks,
		requiredChecksForRepo:             opts.RequiredChecksForRepo,
		approvalDesk:                      opts.ApprovalDesk,
		mutation:                          opts.MutationBoundary,
		intentGate:                        opts.IntentGate,
		reviewGate:                        opts.ReviewGate,
		humanChangeRequests:               opts.HumanChangeRequests,
		humanMergePaths:                   opts.HumanMergePaths,
		humanMergeRule:                    opts.HumanMergeRule,
		requireCurrentBase:                opts.RequireCurrentBase,
		requireCurrentBaseStrict:          opts.RequireCurrentBaseStrict,
		mergeForwardTrustedHuman:          opts.MergeForwardTrustedHuman,
		selfAuthorizationHoldEnabled:      opts.SelfAuthorizationHoldEnabled,
		selfAuthorizationHoldReleaseLimit: opts.SelfAuthorizationHoldReleaseLimit,
		repoAutoMergeEnabled:              opts.RepoAutoMergeEnabled,
		repoOnGitHub:                      opts.RepoOnGitHub,
		trustedBotAuthors:                 opts.TrustedBotAuthors,
		rateLimitCautious:                 opts.RateLimitCautious,
		botTakeoverEnabledFn:              opts.BotTakeoverEnabled,
		trustedAuthorPolicy:               opts.TrustedAuthorPolicy,
		trustedAuthorizer:                 opts.TrustedAuthorizer,
		contributorLanePolicy:             opts.ContributorLanePolicy,
		contributorLaneVerdict:            opts.ContributorLaneVerdict,
		contributorRegistryTier:           opts.ContributorRegistryTier,
		contributorSoak:                   opts.ContributorSoak,
		headFirstSeen:                     opts.HeadFirstSeen,
		sentinelLabel:                     opts.SentinelLabel,
		hiveSupersedingAutoMerge:          opts.HiveSupersedingAutoMerge,
		sentinelAcknowledgedBy:            opts.SentinelAcknowledgedBy,
		minHeadAge:                        opts.MinHeadAge,
		now:                               opts.Now,
		stageLabelPolicy:                  opts.StageLabelPolicy,
		stageTracker:                      opts.StageTracker,
		stageStore:                        opts.StageStore,
		stageEngine:                       opts.StageEngine,
		contributorNudge:                  opts.ContributorNudge,
		checkSummary:                      opts.CheckSummary,
		openPRs:                           opts.OpenPRs,
		sweepTrigger:                      opts.SweepTrigger,
		mergeTrainPolicy:                  mergeTrainPolicyOption(opts),
		nativeMergeQueuePolicy:            opts.NativeMergeQueue,
		integrationAdmission:              opts.IntegrationAdmission,
		integrationExecution:              opts.IntegrationExecution,
		integrationOptions:                opts,
		rebaseResolver:                    opts.RebaseResolver,
		rebaseEpisode:                     opts.RebaseEpisode,
		fixDispatch:                       opts.FixDispatchStatus,
		hotspotRecorder:                   opts.HotspotRecorder,
		integrationFilesObserved:          opts.IntegrationFilesObserved,
		evaluatedHeads:                    make(map[string]string),
		requiredChecksFallbackWarned:      make(map[string]bool),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.contributorSoak == nil {
		e.contributorSoak = sharedContributorSoak
	}
	if e.headFirstSeen == nil {
		e.headFirstSeen = sharedHeadFirstSeen
	}
	if e.sweepTrigger == nil {
		// Always present: with the forgestate cache the event-driven loop
		// drains it; without, the legacy ticker loop still selects on it so
		// a webhook CI-failure nudge (SweepNudge) runs a targeted pass now
		// instead of on the next tick. A nil source only disables
		// NotifyHead's SHA lookup.
		e.sweepTrigger = NewSweepTrigger(e.openPRs)
	}

	if e.mutation == nil {
		if provider, ok := transport.(interface{ MutationBoundary() effects.Boundary }); ok {
			e.mutation = provider.MutationBoundary()
		}
	}
	return e
}

func (c *Engine) ready() bool {
	return c != nil && c.transport != nil && c.gh != nil
}

func (c *Engine) repoAutoMergeAllowed(repo string) bool {
	if c == nil || c.repoAutoMergeEnabled == nil {
		return true
	}
	return c.repoAutoMergeEnabled(repo)
}

// activeRepos is Repositories() minus the repos under an operator pause
// (#6203) and minus the repos that live on another forge (#14150). The pause
// narrowing lives behind an optional transport capability, the same shape New
// uses for MutationBoundary: a transport that does not know about pauses (the
// sweep's own fakes) keeps its full repository list, so pause support is
// additive rather than a Transport-interface break.
func (c *Engine) activeRepos() []string {
	if c == nil || c.transport == nil {
		return nil
	}
	var repos []string
	if provider, ok := c.transport.(interface{ ActiveRepositories() []string }); ok {
		repos = provider.ActiveRepositories()
	} else {
		repos = c.transport.Repositories()
	}
	if c.repoOnGitHub == nil {
		return repos
	}
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		if !c.repoOnGitHub(repo) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// SweepQueuedAutoMerges consumes queued automerge requests using a one-shot engine.
func SweepQueuedAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepQueuedAutoMerges(ctx, sweepOpts)
}

// StartSelfAuthoredAutoMergeSweep starts the self-authored sweep using caller-owned options.
// The returned channel closes once the sweep loop has exited (see the Engine
// method).
func StartSelfAuthoredAutoMergeSweep(ctx context.Context, transport Transport, maxMerges int, acmmAllowed bool, acmmLevel *int, opts Options) <-chan struct{} {
	return New(transport, opts).StartSelfAuthoredAutoMergeSweep(ctx, maxMerges, acmmAllowed, acmmLevel)
}

func SweepTrustedAuthorAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepTrustedAuthorAutoMerges(ctx, sweepOpts)
}

// SweepContributorLaneAutoMerges runs the contributor-lane tier on a one-shot
// engine. Soak state is shared process-wide (see ContributorSoakTracker), so
// a one-shot engine per tick still observes the soak.
func SweepContributorLaneAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepContributorLaneAutoMerges(ctx, sweepOpts)
}

// selfMergeMinACMMLevel mirrors config.SelfMergeMinACMMLevel. It is duplicated
// rather than imported so this package keeps no dependency on pkg/config
// (hivecommons/hive#5953 phase 1); config_parity_test.go pins the two equal so
// they cannot drift silently.
const selfMergeMinACMMLevel = 6

// SelfMergeMinACMMLevel is the minimum ACMM level for self-authored automerge.
const SelfMergeMinACMMLevel = selfMergeMinACMMLevel

const DefaultAutoMergeSweepMaxMerges = 3
const selfAuthoredSweepMaxBranchUpdates = 5

const releaseInProgressStatusContext = "release-in-progress"
const releaseInProgressMaxAge = 10 * time.Minute

// selfAuthoredAutoMergeSweepInterval is how often
// StartSelfAuthoredAutoMergeSweep re-scans the App's own open PRs. Matches
// the human-queue merge-request watcher's cadence (mergeRequestPollInterval):
// merges are latency-sensitive (an eligible PR should land quickly) but a
// tight loop risks GitHub secondary rate limits across every managed repo.
const selfAuthoredAutoMergeSweepInterval = 10 * time.Second

// selfAuthoredSweepCautionInterval is the floor the self-authored sweep tick
// backs off to while the GitHub slow-start caution window is engaged. The
// sweep is housekeeping: after a rate-limit refusal its 10s cadence was the
// largest single feeder of the pacing queue the governor's enumeration was
// starved behind (2026-10-21). The adaptive interval resumes when caution
// ends.
const selfAuthoredSweepCautionInterval = time.Minute

// selfAuthoredSweepTickBudget is how long one sweep tick may reasonably wait
// for its first pacing slot; a deeper queue sheds the tick.
const selfAuthoredSweepTickBudget = 5 * time.Second

// cautionSweepInterval returns the tick to use while cautious: the adaptive
// interval, floored at selfAuthoredSweepCautionInterval.
func cautionSweepInterval(interval time.Duration) time.Duration {
	if interval < selfAuthoredSweepCautionInterval {
		return selfAuthoredSweepCautionInterval
	}
	return interval
}

// lowPriority tags ctx as housekeeping for the slow-start pacer unless the
// caller already chose a class.
func lowPriority(ctx context.Context) context.Context {
	if hgithub.PacingClassOf(ctx) != hgithub.PacingNormal {
		return ctx
	}
	return hgithub.WithPacingClass(ctx, hgithub.PacingLow)
}

// selfAuthoredSweepBudgetShare caps the fraction of the App's hourly REST
// budget this sweep alone may consume. The sweep is a background nicety; the
// agents' own gh calls, the governor's eval cycle, the merge-eligible writer
// and the fix loops all draw on the same allowance and must not be starved.
const selfAuthoredSweepBudgetShare = 0.25

// githubAppHourlyRateLimit is the GitHub App installation REST allowance the
// interval is sized against.
const githubAppHourlyRateLimit = 6900

// selfAuthoredSweepCandidateAllowance is the default per-tick request allowance
// for per-CANDIDATE calls, on top of the one list call per repo. Listing is not
// the whole cost of a tick: every open App-authored non-draft PR that survives
// cheap list-response gates costs a PullRequests.Get in trySweepSelfAuthoredPR,
// plus a second re-verify Get on the ones that reach the merge step. Sizing the
// interval on repo count alone therefore understates a tick.
//
// This is only the starting allowance. StartSelfAuthoredAutoMergeSweep adapts
// the next tick to the observed post-filter candidate count when a backlog is
// larger than this default, so a held/exempt backlog does not burn the budget
// and a genuinely large mergeable backlog slows proportionally.
const selfAuthoredSweepCandidateAllowance = 32

// selfAuthoredSweepMaxInterval bounds adaptive backoff so the self-authored
// sweep still makes progress even during unusually large candidate backlogs.
const selfAuthoredSweepMaxInterval = 15 * time.Minute

// selfAuthoredSweepInterval sizes the sweep tick so a hive with many repos
// cannot exhaust its GitHub rate limit just by looking for merge candidates.
//
// Cost per tick is roughly repos + candidates: one list call per configured
// repo, plus one Get per open App-authored PR (and a re-verify Get per merge).
// The fixed 10s tick scaled none of this. A 45-repo hive issued 45 x 360 =
// 16,200 list requests/hour against a 6,900/hour limit — 2.3x over on the list
// calls alone, before candidate Gets and before a single agent made a call.
// Observed live: the sweep 403'd continuously, go-github then short-circuited
// every request until the recorded reset ("not making remote request"), and
// the App's own PRs stopped merging entirely.
//
// Small hives are unaffected: the fixed interval remains the floor, so a hive
// with a handful of repos still sweeps every 10s exactly as before.
// selfAuthoredSweepSmallHiveRepos: at or below this many repos the fixed 10s
// tick is kept as-is. The budget-share math above would slow even a 1-repo hive
// (1 list + the candidate allowance per tick lands it over a 25% share at 10s),
// but a small hive's ABSOLUTE spend is comfortably inside the 6,900/hour limit
// — the share exists to stop many-repo hives starving everything else, a
// failure mode small hives cannot produce. Keeping their tick unchanged also
// keeps this change a no-op for the common quick-start deployment.
const selfAuthoredSweepSmallHiveRepos = 4

func selfAuthoredSweepInterval(repos int) time.Duration {
	return selfAuthoredSweepIntervalForCandidates(repos, selfAuthoredSweepCandidateAllowance)
}

func selfAuthoredSweepIntervalForCandidates(repos, candidates int) time.Duration {
	if repos <= selfAuthoredSweepSmallHiveRepos {
		return selfAuthoredAutoMergeSweepInterval
	}
	if candidates < selfAuthoredSweepCandidateAllowance {
		candidates = selfAuthoredSweepCandidateAllowance
	}
	budget := float64(githubAppHourlyRateLimit) * selfAuthoredSweepBudgetShare
	perTick := float64(repos + candidates)
	seconds := perTick * 3600.0 / budget
	interval := time.Duration(seconds * float64(time.Second))
	if interval < selfAuthoredAutoMergeSweepInterval {
		return selfAuthoredAutoMergeSweepInterval
	}
	if interval > selfAuthoredSweepMaxInterval {
		return selfAuthoredSweepMaxInterval
	}
	return interval.Round(time.Second)
}

func nextSelfAuthoredSweepInterval(repos int, current time.Duration, result *AutoMergeSweepResult) (time.Duration, bool) {
	if result == nil {
		return current, false
	}
	next := selfAuthoredSweepIntervalForCandidates(repos, result.Candidates)
	return next, next != current
}

const (
	autoMergeReasonNoHiveQueueApproval        = "no-hive-queue-approval"
	autoMergeReasonHumanChangesRequested      = prstage.AutoMergeReasonHumanChangesRequested
	autoMergeReasonNoAppBotLogin              = prstage.AutoMergeReasonNoAppBotLogin
	autoMergeReasonMergeQueueRequired         = prstage.AutoMergeReasonMergeQueueRequired
	autoMergeReasonUntrustedQueueApproval     = "untrusted-hive-queue-approval"
	autoMergeReasonUntrustedMerger            = "untrusted-merger"
	autoMergeReasonNoMergerAuthz              = "no-merger-authorizer"
	autoMergeWarnNoAppBotLogin                = "automerge sweep disabled: no GitHub App bot login configured"
	autoMergeWarnUntrustedQueueApproval       = "rejected untrusted Hive auto-merge queue approval"
	autoMergeWarnUntrustedMerger              = "rejected Hive auto-merge queued by an untrusted actor"
	autoMergeWarnNoMergerAuthz                = "automerge sweep disabled: no trusted-merger authorizer configured"
	autoMergeNoAppBotLoginOperatorRemediation = "Hive has no usable GitHub App, so App-authorship cannot be verified and auto-merge is disabled"
	autoMergeNoMergerAuthzRemediation         = "Hive cannot classify who queued the merge, so auto-merge is disabled (fail-closed)"
)

var hiveQueueReviewRE = regexp.MustCompile(`(?i)^Approved by @([A-Za-z0-9-]+) for Hive auto-merge on green CI\.`)
var requiredChecksExpectedRE = regexp.MustCompile(`(?i)(required status check ["'][^"']+["'] is expected|\d+\s+of\s+\d+\s+required status checks are expected)`)
var mergeQueueRequiredRE = regexp.MustCompile(`(?i)must be made through the merge queue`)

// mergeQueueBaseRecheck is how long a base branch observed as
// merge-queue-only skips the direct merge before the sweep probes it again,
// so a ruleset change is picked up without a restart.
const mergeQueueBaseRecheck = 30 * time.Minute

// mergeMethodFor returns the GitHub merge method the sweep should use for pr.
// Forward-merge / top-up PRs between release lines must land as true merge
// commits so the target line keeps the source line's ancestry; everything else
// keeps the repository's squash convention. The classification (branch, title,
// and head history) is shared with the hive-merge relay via
// hgithub.ResolveForwardMergeMethod.
// A PR wearing `merge-method/merge` (mixed authorship, each commit
// self-signed; automerge_merge_commit.go) is merged with a merge commit too.
func (c *Engine) mergeMethodFor(ctx context.Context, owner, repo string, pr *gh.PullRequest) string {
	method, _ := c.mergeMethodWithReason(ctx, owner, repo, pr)
	return method
}

type AutoMergeSweepOptions struct {
	MaxMerges int
	Audit     func(AutoMergeSweepEvent)
	// SelfAuthoredLaneActive tells SweepTrustedAuthorAutoMerges that the
	// self-authored sweep is running alongside it, so trusted-bot PRs are
	// left to that lane (which reports their real eligibility) instead of
	// being judged by the human-author role gate. When false — ACMM below
	// SelfMergeMinACMMLevel or auto_merge.self_authored off — the
	// trusted-author tier evaluates them as before, so a bot an operator
	// granted the required role still has a sweep that can merge it.
	SelfAuthoredLaneActive bool
	// Targets narrows an event-driven pass to the PRs a webhook named
	// (hivecommons/hive#13377); nil evaluates every PR of every repo.
	Targets SweepTargets
}

type TrustedAuthorPolicy struct {
	Enabled                 bool
	Repos                   map[string]bool
	RequireRole             string
	RequireGitHubPermission bool
	ExcludeLabels           map[string]bool
}

type TrustedAuthorDecision struct {
	Allowed bool
	Role    string
}

// TrustedAuthorizer reports whether a PR author holds the configured
// authorized-users role for the trusted-author tier. The concrete role is
// returned for audit comments. Unknown actors fail closed.
type TrustedAuthorizer func(login, requireRole string) TrustedAuthorDecision

type trustedAuthorPermission struct {
	Allowed bool
	Level   string
}

type expectedCheckCache struct {
	entries map[string]map[string]bool
}

type expectedCheckCacheContextKey struct{}

func newExpectedCheckCache() *expectedCheckCache {
	return &expectedCheckCache{entries: make(map[string]map[string]bool)}
}

func contextWithExpectedCheckCache(ctx context.Context, cache *expectedCheckCache) context.Context {
	if ctx == nil || cache == nil {
		return ctx
	}
	return context.WithValue(ctx, expectedCheckCacheContextKey{}, cache)
}

func expectedCheckCacheFromContext(ctx context.Context) *expectedCheckCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(expectedCheckCacheContextKey{}).(*expectedCheckCache)
	return cache
}

func (c *Engine) evaluatedHeadKey(owner, repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func (c *Engine) rememberEvaluatedHead(owner, repo string, number int, sha string) {
	if c == nil || strings.TrimSpace(sha) == "" {
		return
	}
	c.evaluatedHeadsMu.Lock()
	defer c.evaluatedHeadsMu.Unlock()
	if c.evaluatedHeads == nil {
		c.evaluatedHeads = make(map[string]string)
	}
	c.evaluatedHeads[c.evaluatedHeadKey(owner, repo, number)] = sha
}

// MergerAuthorizer reports whether login is trusted to QUEUE a merge — i.e.
// holds at least config.RoleMerger in the hive's authorized-users allowlist.
//
// SECURITY (audit F3). The queue-time role check lives in the dashboard handler
// (requireMergerOrOwnerRole), but the sweep is a SEPARATE authority that runs a
// minute later off nothing but the label and the App-authored approval body. It
// re-derives the queuer's login from that body and used to merge on it
// unconditionally, so anything that could get the merger-queue label applied
// got its PR merged regardless of who asked. Re-verifying the role HERE is what
// makes the merger tier real at the point the merge actually happens.
//
// Returns false for an unknown/unclassifiable login: a nil authorizer or an
// unresolvable actor must never merge (fail CLOSED).
type MergerAuthorizer func(login string) bool

// SetMergerAuthorizer installs the trusted-merger gate consulted by
// SweepQueuedAutoMerges. nil fails closed — the sweep merges nothing.
func (c *Engine) SetMergerAuthorizer(fn MergerAuthorizer) {
	if c == nil {
		return
	}
	c.mergerAuthzMu.Lock()
	defer c.mergerAuthzMu.Unlock()
	c.mergerAuthz = fn
}

// SetAttributionHooks forwards test audit hooks to transports that support them.
func (c *Engine) SetAttributionHooks(hooks hgithub.AttributionHooks) {
	if c == nil {
		return
	}
	if setter, ok := c.transport.(interface {
		SetAttributionHooks(hgithub.AttributionHooks)
	}); ok {
		setter.SetAttributionHooks(hooks)
	}
}

// SetRequiredChecks installs the config-declared fallback required-status-check
// set (config.AutoMergeConfig.RequiredCheckSet). Branch protection is the
// authoritative source; this set is used only when protection cannot be read.
func (c *Engine) SetRequiredChecks(set map[string]bool) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecks = set
	c.requiredChecksForRepo = nil
}

func (c *Engine) SetRequiredChecksForRepo(fn func(repo string) (map[string]bool, bool)) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecksForRepo = fn
}

// SetAutoMergeLabel updates the underlying transport label when it supports the setter.
func (c *Engine) SetAutoMergeLabel(label string) {
	if c == nil {
		return
	}
	if setter, ok := c.transport.(interface{ SetAutoMergeLabel(string) }); ok {
		setter.SetAutoMergeLabel(label)
	}
}

// configRequiredChecks returns the currently installed config-declared
// required-check set and whether one is installed. Mirrors isTrustedMerger's
// nil-safe read pattern for c.mergerAuthz.
func (c *Engine) configRequiredChecks() (map[string]bool, bool) {
	return c.configRequiredChecksForRepo("")
}

func (c *Engine) configRequiredChecksForRepo(repo string) (map[string]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.requiredChecksMu.RLock()
	fn := c.requiredChecksForRepo
	set := c.requiredChecks
	c.requiredChecksMu.RUnlock()
	if fn != nil {
		return fn(repo)
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}

// isTrustedMerger reports whether login may queue a merge. Fails CLOSED.
func (c *Engine) isTrustedMerger(login string) (allowed, configured bool) {
	if c == nil {
		return false, false
	}
	c.mergerAuthzMu.RLock()
	fn := c.mergerAuthz
	c.mergerAuthzMu.RUnlock()
	if fn == nil {
		return false, false
	}
	if strings.TrimSpace(login) == "" {
		return false, true
	}
	return fn(login), true
}

func (c *Engine) currentTrustedAuthorPolicy() TrustedAuthorPolicy {
	if c == nil || c.trustedAuthorPolicy == nil {
		return TrustedAuthorPolicy{}
	}
	p := c.trustedAuthorPolicy()
	if p.RequireRole == "" {
		p.RequireRole = "merger"
	}
	if p.ExcludeLabels == nil {
		p.ExcludeLabels = map[string]bool{"hold": true, "do-not-merge": true, "needs-human": true}
	}
	return p
}

func (c *Engine) trustedAuthorDecision(login, requireRole string) TrustedAuthorDecision {
	if c == nil || c.trustedAuthorizer == nil || strings.TrimSpace(login) == "" {
		return TrustedAuthorDecision{}
	}
	return c.trustedAuthorizer(login, requireRole)
}

func (p TrustedAuthorPolicy) repoAllowed(repo string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Repos) == 0 {
		return true
	}
	return p.Repos[strings.ToLower(strings.TrimSpace(repo))]
}

func (p TrustedAuthorPolicy) excludedLabel(labels []string) string {
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if p.ExcludeLabels[key] {
			return key
		}
	}
	return ""
}

func (c *Engine) consultApprovalDesk(ctx context.Context, req hgithub.ApprovalDeskRequest) (bool, string) {
	if c == nil || c.approvalDesk == nil {
		return true, ""
	}
	allow, reason := c.approvalDesk(ctx, req)
	if !allow && reason == "" {
		reason = "approval-desk-withheld"
	}
	return allow, reason
}

type AutoMergeSweepEvent struct {
	Repo     string
	Number   int
	Author   string
	QueuedBy string
	HeadSHA  string
	MergeSHA string
	Label    string
	Tier     string
	// BranchUpdated is set on skipped events when the sweep successfully
	// queued a PR branch sync instead of merging this tick.
	BranchUpdated bool
}

type AutoMergeSweepResult struct {
	Merged          []AutoMergeSweepEvent
	Seen            int
	Skipped         int
	Candidates      int
	UpdatedBranches int
}

type hiveQueueApproval struct {
	QueuedBy string
	HeadSHA  string
}
