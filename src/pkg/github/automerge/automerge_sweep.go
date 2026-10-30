package automerge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
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
	// HotspotRecorder receives the merge train's PR file lists and the
	// conflicts it asks the resolver to repair, so the merge-hotspot
	// advisor (pkg/hotspot) derives its evidence without polling GitHub
	// again; nil records nothing.
	HotspotRecorder HotspotRecorder
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

	mergeTrainPolicy func() MergeTrainPolicy
	rebaseResolver   RebaseResolver
	rebaseEpisode    func(repo string, number int) (RebaseEpisode, bool)
	hotspotRecorder  HotspotRecorder
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
		rebaseResolver:                    opts.RebaseResolver,
		rebaseEpisode:                     opts.RebaseEpisode,
		hotspotRecorder:                   opts.HotspotRecorder,
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

// SweepQueuedAutoMerges consumes the configured Hive merger-queue label. It
// only squashes open, labelled, non-draft PRs in managed repos after GitHub
// reports them mergeable, commit statuses/check-runs are green, the latest
// Hive App-authored queue approval proves the queuer is not the PR author, and
// the queuer is a TRUSTED merger (audit F3 — see MergerAuthorizer). Without an
// authorizer installed the sweep fails closed and merges nothing.
func (c *Engine) SweepQueuedAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	c.reconcileInactiveNativeQueues(ctx)
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	label := c.transport.AutoMergeLabel()
	result := &AutoMergeSweepResult{}
	noAppBotLoginWarned := false
	noMergerAuthzWarned := false
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)

	// activeRepos: an operator-paused repo receives no automerges (#6203). This
	// sweep is hive-driven, not kick-driven, so leaving it on Repositories()
	// would have kept merging into a repo during its release freeze.
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
		}
		owner, repoName := c.transport.SplitRepo(repo)
		issues, err := c.listQueuedPullRequestIssues(ctx, owner, repoName, label)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Merged) >= maxMerges {
				break
			}
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			result.Seen++
			if reason := c.prefilterQueuedIssue(issue, label); reason != "" {
				// Only an opted-in repo may hold an App queue entry worth
				// withdrawing; the extra PR read is skipped everywhere else.
				if c.nativeQueue(owner+"/"+repoName) != nil {
					if pr, _, err := c.gh.PullRequests.Get(ctx, owner, repoName, issue.GetNumber()); err == nil {
						c.reconcileNativeQueueGate(ctx, nativeQueueLaneQueued, owner, repoName, pr, reason)
					}
				}
				if reason == "held" || reason == "exempt-label" {
					c.debug("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				} else {
					c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				}
				result.Skipped++
				continue
			}
			c.sweepClock.evaluating(repo, issue.GetNumber())
			event, reason, err := c.trySweepQueuedPR(ctx, repo, owner, repoName, issue.GetNumber(), label)
			c.sweepClock.evaluated()
			if err != nil {
				c.warn("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoAppBotLogin {
				if !noAppBotLoginWarned {
					c.warn(autoMergeWarnNoAppBotLogin, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoAppBotLoginOperatorRemediation)
					noAppBotLoginWarned = true
				}
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoMergerAuthz {
				if !noMergerAuthzWarned {
					c.warn(autoMergeWarnNoMergerAuthz, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoMergerAuthzRemediation)
					noMergerAuthzWarned = true
				}
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				result.Skipped++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

// SweepSelfAuthoredAutoMerges merges the App's OWN open PRs directly on green
// CI, without a human "Approved ... for Hive auto-merge" queue review and
// without waiting on tide.
//
// Why this must exist as a SEPARATE path from SweepQueuedAutoMerges: Prow
// structurally forbids self-approval — tide requires lgtm+approved labels
// from a reviewer distinct from the PR author, and the author here is always
// the App itself. A human queuer can supply that for someone else's PR (the
// existing sweep above), but nobody can supply it for the App's own PR: the
// App cannot review its own work, and asking a human to rubber-stamp every
// App PR defeats the point of automation. So the App must self-merge
// directly over the GitHub REST API (squash), the same bypass-tide mechanism
// SweepQueuedAutoMerges already uses for tide-pending/unstable states (see
// mergeableFromState) — this path just skips the human-queue-approval lookup
// entirely rather than needing one.
//
// Every OTHER safety property is identical to the human queue: mergeability
// (mergeableFromState), green required checks (commitGreen), the intent tier
// gate (selfMergeIntentGate, #6258 — the same refusal predicate
// writeMergeEligible applies before a PR can be queued), and a head-SHA
// re-check immediately before the merge call so a push landing between
// enumeration and merge can never be squashed unreviewed — mirrored below via
// re-fetching the PR right before calling Merge and comparing SHAs, the same
// pattern trySweepQueuedPR uses via the queue-approval's recorded HeadSHA.
// There is no queuedBy in this path, so the author==queuedBy self-merge-ban
// in trySweepQueuedPR simply does not apply — there is no queuer to compare
// against.
func (c *Engine) SweepSelfAuthoredAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = lowPriority(ctx)
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	result := &AutoMergeSweepResult{}
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		// No usable App identity: there is no "self" to authenticate PRs as
		// App-authored, so this sweep has nothing safe to do. Warn once per
		// call (matching the human-queue sweep's per-call warn cadence) rather
		// than per-repo, since the cause is hive-wide, not per-repo.
		c.warn(autoMergeWarnNoAppBotLogin, "reason", autoMergeReasonNoAppBotLogin, "cause", autoMergeNoAppBotLoginOperatorRemediation)
		return result, nil
	}

	// See the queued sweep above: paused repos are out of scope for automerge.
	selfAuthReleaseBudget := c.selfAuthorizationReleaseBudget()
	branchUpdateAttempts := 0
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(repo)
		if !opts.Targets.selectsRepo(owner + "/" + repoName) {
			continue
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("self-authored automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
		}
		prs, skipReason, err := c.openPullRequests(ctx, owner, repoName, opts.Targets)
		if err != nil {
			return result, err
		}
		if skipReason != "" {
			c.info("self-authored automerge sweep skipped repo", "repo", repo, "reason", skipReason)
			continue
		}
		repoSeen := 0
		repoSkipped := 0
		repoCandidates := 0
		repoMergedBefore := len(result.Merged)
		repoSkipReasons := make(map[string]int)
		train := c.planMergeTrains(ctx, stageLaneSelf, repo, owner, repoName, prs, c.prefilterSelfAuthoredPR)
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			repoSeen++
			stage := func(reason string, merged bool, err error) {
				c.reconcileNativeQueueGate(ctx, stageLaneSelf, owner, repoName, pr, reason)
				c.recordStage(stageLaneSelf, repo, owner, repoName, pr, reason, merged, err)
			}
			if _, held := train.duplicateOf(number); held {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainDuplicate]++
				stage(c.mergeTrainDuplicateSkip(train, number), false, nil)
				continue
			}
			if _, held := train.claimConflictOf(number); held {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainClaimConflict]++
				stage(c.mergeTrainClaimConflictSkip(train, number), false, nil)
				continue
			}
			if train.isSibling(number) {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainQueued]++
				stage(c.mergeTrainSkip(train, number), false, nil)
				continue
			}
			if reason, handled := c.mergeTrainFront(ctx, train, pr); handled {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainRebasing]++
				stage(reason, false, nil)
				continue
			}
			// #5117 hold release must run before the prefilter: the
			// prefilter skips held PRs outright, and an eligible
			// self-authorization hold has to be released, not skipped.
			if selfAuthReleaseBudget > 0 && pr != nil && !c.selfAuthorizationHoldActive(repo) && c.isHeld(labelNames(pr.Labels)) {
				released, err := c.releaseSelfAuthorizationHoldIfEligible(ctx, repo, owner, repoName, number)
				if err != nil {
					c.warn("self-authored automerge sweep could not evaluate #5117 hold release", "repo", repo, "pr", number, "error", err)
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-release-error"]++
					stage("self-authorization-release-error", false, err)
					continue
				}
				if released {
					selfAuthReleaseBudget--
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-hold-released"]++
					stage("self-authorization-hold-released", false, nil)
					continue
				}
			}
			// Level-applied holds (#7060) must also be evaluated before the
			// generic prefilter skips held PRs outright. If a hold is still
			// required (or is not a level-applied hold), count it as the returned
			// skip reason; if it was released, skip this tick and let the next
			// sweep evaluate the now-unheld PR.
			if pr != nil && c.isHeld(labelNames(pr.Labels)) {
				if _, reason, err := c.releaseLevelHoldIfEligible(ctx, owner, repoName, pr); err != nil {
					c.warn("self-authored automerge sweep could not evaluate level hold release", "repo", repo, "pr", number, "reason", reason, "error", err)
					result.Skipped++
					repoSkipped++
					if reason == "" {
						reason = "level-hold-release-error"
					}
					repoSkipReasons[reason]++
					stage(reason, false, err)
					continue
				} else if reason != "" {
					if reason == "hold" {
						reason = "held"
					}
					result.Skipped++
					repoSkipped++
					repoSkipReasons[reason]++
					stage(reason, false, nil)
					continue
				}
			}
			if reason := c.detectTrustedRegistryClankerWorkflowApprovalRequired(ctx, repo, owner, repoName, pr); reason != "" {
				if isTerminalRequiredCheckHold(reason) {
					c.info("automerge hold", "repo", repo, "pr", number, "sha", pr.GetHead().GetSHA(), "reason", reason)
				} else {
					c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				}
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			if reason := c.prefilterSelfAuthoredPR(pr); reason != "" {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			repoCandidates++
			c.sweepClock.evaluating(repo, number)
			event, reason, err := c.trySweepSelfAuthoredPR(withMergeTrainCandidateHold(ctx, c.mergeTrainCandidateReason(train, number)), repo, owner, repoName, number, branchUpdateAttempts < selfAuthoredSweepMaxBranchUpdates)
			c.sweepClock.evaluated()
			if event.BranchUpdated || reason == "updated-branch" || reason == "update-branch" {
				branchUpdateAttempts++
			}
			if err != nil {
				c.warnSelfAuthoredSkipOnce(repo, number, reason, err)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, err)
				continue
			}
			if event.BranchUpdated || reason == "updated-branch" {
				result.UpdatedBranches++
			}
			c.noteMergeTrainOutcome(stageLaneSelf, owner, repoName, number, pr.GetHead().GetSHA(), reason)
			if reason != "" {
				if isTerminalRequiredCheckHold(reason) || isPermanentMergeRefusal(reason) {
					if c.markTerminalHoldLogged(owner, repoName, number, pr.GetHead().GetSHA(), reason) {
						c.info("automerge hold", "repo", repo, "pr", number, "sha", pr.GetHead().GetSHA(), "reason", reason)
					}
				} else {
					c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				}
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			stage("", true, nil)
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if train.isFront(number) {
				c.mergeTrainAfterFrontMerge(ctx, train, number)
			}
		}
		if repoSeen > 0 || repoCandidates > 0 || len(result.Merged) > repoMergedBefore {
			args := []any{
				"repo", repo,
				"seen", repoSeen,
				"candidates", repoCandidates,
				"merged", len(result.Merged) - repoMergedBefore,
				"updated_branches", result.UpdatedBranches,
				"skipped", repoSkipped,
			}
			skipReasonOrder := []string{"label:hold", "label:needs-human", "label:needs-rebase", "held", "exempt-label", "draft", "closed", "not-app-authored", "missing-head-sha", "updated-branch", autoMergeReasonBehindBudgetExhausted, "conflicting", autoMergeReasonForkNotEditable, "not-mergeable", autoMergeReasonClosedSuperseded, autoMergeReasonClosedEmptyDiff, autoMergeReasonMergeQueueRequired, autoMergeReasonMergeRefusedReview, autoMergeReasonMergeRefusedMethod, autoMergeReasonMergeTrainQueued, autoMergeReasonMergeTrainRebasing}
			if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" {
				skipReasonOrder = append([]string{sentinelLabel}, skipReasonOrder...)
			}
			for _, reason := range skipReasonOrder {
				if count := repoSkipReasons[reason]; count > 0 {
					args = append(args, reason, count)
				}
			}
			c.info("self-authored automerge sweep tick", args...)
		}
	}
	return result, nil
}

// prMergeableStateConflicting reads GitHub's conflict flag off a single-PR
// GET payload.
// staleNeedsRebaseLabel: prow's needs-rebase label lags GitHub's
// mergeable_state. When GitHub says the branch is merely "behind" (clean
// update), the label must not block the lane — the lane's UpdateBranch is
// exactly what clears it, and before hivecommons/hive#15103 no lane ever
// reached that call under the label.
func staleNeedsRebaseLabel(blocked string, pr *gh.PullRequest) bool {
	return blocked == "label:needs-rebase" && strings.EqualFold(strings.TrimSpace(pr.GetMergeableState()), "behind")
}

func prMergeableStateConflicting(pr *gh.PullRequest) bool {
	state := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	return state == "dirty" || state == "conflicting"
}

// SweepTrustedAuthorAutoMerges merges green PRs whose human author already
// holds the configured authorized-users role and, by default, GitHub
// repository write/maintain/admin permission. This is an operator opt-in tier:
// unlike App self-merge, a human PR has no structural self-approval problem, so
// the sweep must prove Hive is only executing a merge the author could perform
// themselves.
func (c *Engine) SweepTrustedAuthorAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = lowPriority(ctx)
	policy := c.currentTrustedAuthorPolicy()
	result := &AutoMergeSweepResult{}
	if !policy.Enabled {
		return result, nil
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, displayRepo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(displayRepo)
		if !policy.repoAllowed(displayRepo) || !opts.Targets.selectsRepo(owner+"/"+repoName) {
			continue
		}
		if !c.repoAutoMergeAllowed(displayRepo) {
			c.info("trusted-author automerge sweep skipped repo", "repo", displayRepo, "reason", "repo-auto-merge-disabled")
			continue
		}
		prs, skipReason, err := c.openPullRequests(ctx, owner, repoName, opts.Targets)
		if err != nil {
			return result, err
		}
		if skipReason != "" {
			c.info("trusted-author automerge sweep skipped repo", "repo", displayRepo, "reason", skipReason)
			continue
		}
		train := c.planMergeTrains(ctx, stageLaneTrusted, displayRepo, owner, repoName, prs, func(pr *gh.PullRequest) string {
			login := hgithub.SafeGetLogin(pr.GetUser())
			if opts.SelfAuthoredLaneActive && c.isTrustedBotLaneAuthor(login) {
				return trustedBotLaneSkipReason
			}
			if lane, _ := c.sweepLaneForAuthor(login); lane == stageLaneSelf {
				return trustedAuthorReasonAppAuthored
			}
			return c.prefilterTrustedAuthorPR(owner, repoName, pr, policy)
		})
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			stage := func(reason string, merged bool, err error) {
				c.reconcileNativeQueueGate(ctx, stageLaneTrusted, owner, repoName, pr, reason)
				c.recordStage(stageLaneTrusted, displayRepo, owner, repoName, pr, reason, merged, err)
			}
			if _, held := train.duplicateOf(number); held {
				result.Skipped++
				stage(c.mergeTrainDuplicateSkip(train, number), false, nil)
				continue
			}
			if _, held := train.claimConflictOf(number); held {
				result.Skipped++
				stage(c.mergeTrainClaimConflictSkip(train, number), false, nil)
				continue
			}
			if train.isSibling(number) {
				result.Skipped++
				stage(c.mergeTrainSkip(train, number), false, nil)
				continue
			}
			if reason, handled := c.mergeTrainFront(ctx, train, pr); handled {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			if opts.SelfAuthoredLaneActive && c.isTrustedBotLaneAuthor(hgithub.SafeGetLogin(pr.GetUser())) {
				// The self-authored sweep owns trusted-bot PRs and records
				// their eligibility verdict; the tier hand-off is logged here
				// so every Seen PR still has a reason and Skipped stays
				// consistent, without a misleading untrusted-author-role.
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", trustedBotLaneSkipReason, "tier", "trusted-author")
				result.Skipped++
				stage(trustedBotLaneSkipReason, false, nil)
				continue
			}
			if lane, _ := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); lane == stageLaneSelf {
				// The self-authored lane owns the App's own PRs, so hand
				// off under this tier's own reason rather than a
				// contradictory untrusted-author-role or the contributor
				// lane's reason, which read as if that lane had routed the
				// PR (hivecommons/hive#14482).
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", trustedAuthorReasonAppAuthored, "tier", "trusted-author")
				result.Skipped++
				stage(trustedAuthorReasonAppAuthored, false, nil)
				continue
			}
			if reason := c.prefilterTrustedAuthorPR(owner, repoName, pr, policy); reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			c.sweepClock.evaluating(displayRepo, number)
			event, reason, err := c.trySweepTrustedAuthorPR(withMergeTrainCandidateHold(ctx, c.mergeTrainCandidateReason(train, number)), displayRepo, owner, repoName, number, policy)
			c.sweepClock.evaluated()
			if err == nil {
				c.noteMergeTrainOutcome(stageLaneTrusted, owner, repoName, number, pr.GetHead().GetSHA(), reason)
			}
			if err != nil {
				c.warn("trusted-author automerge sweep skipped PR", "repo", displayRepo, "pr", number, "reason", reason, "error", err)
				result.Skipped++
				stage(reason, false, err)
				continue
			}
			if reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			stage("", true, nil)
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if train.isFront(number) {
				c.mergeTrainAfterFrontMerge(ctx, train, number)
			}
		}
	}
	return result, nil
}

// StartSelfAuthoredAutoMergeSweep runs a loop that periodically calls
// SweepSelfAuthoredAutoMerges. It returns immediately; the loop runs until ctx
// is cancelled. A nil client is a no-op. maxMerges is passed straight through
// to AutoMergeSweepOptions.MaxMerges (<=0 falls back to
// DefaultAutoMergeSweepMaxMerges there). Mirrors
// StartMergeRequestWatcher/StartPRRequestWatcher's own-ticker-goroutine
// pattern so all three App-identity-dependent watchers share one shape.
//
// acmmAllowed is the caller-computed
// config.AutoMergeConfig.SelfAuthoredAutoMergeAllowed(acmmLevel) result (both
// the auto_merge.self_authored flag AND the hive's ACMM level gate self-merge
// authority — see config.SelfMergeMinACMMLevel). When false the loop is never
// started at all: an ACMM L4/L5 hive (l4.md/l5.md both forbid the App
// merging its own PRs) must not self-merge, matching console's L6 hive which
// is unaffected and keeps self-merging as before.
// refreshRateLimitCache re-reads GitHub's real rate limits and writes them back
// into the go-github client's cache.
//
// WHY THIS IS NEEDED. go-github refuses requests PRE-EMPTIVELY: once it has seen
// Remaining==0 it returns a synthetic 403 ("not making remote request") for
// every call until the cached Reset time passes, without contacting GitHub.
// That cache lives on the Client and is only updated by responses the Client
// itself receives.
//
// Hive's App client is created ONCE (NewClientFromApp) while appTransport
// injects a freshly minted INSTALLATION TOKEN per request, and installation
// tokens rotate roughly hourly. A new token gets a new allowance — but the
// Client's cache still says Remaining==0 with the old token's reset, so
// go-github keeps refusing requests the new token could happily serve.
//
// Observed live: the dashboard reported core remaining=6613 of 6900 while every
// sweep tick failed with "API rate limit of 6900 still exceeded", and the fleet
// consumed ZERO requests over six minutes — not rate-limited, just refusing.
// Merges stalled for the remainder of the window each time.
//
// GET /rate_limit does not count against any limit, and RateLimitService.Get
// writes the result back into the client's cache, so this is a cheap, exact
// correction rather than a guess.
func (c *Engine) refreshRateLimitCache(ctx context.Context) {
	if c == nil || c.gh == nil {
		return
	}
	if _, _, err := c.gh.RateLimit.Get(ctx); err != nil {
		c.warn("could not refresh rate-limit cache", "error", err)
		return
	}
	c.info("refreshed rate-limit cache after a pre-emptive rate-limit refusal")
}

// isRateLimited reports whether err is a primary/secondary rate-limit error,
// including go-github's pre-emptive synthetic one.
// sweepTickDecision is what one self-authored sweep tick does about the
// GitHub pacer before any request is made.
type sweepTickDecision struct {
	// tick is the ticker period to run at from now; reset says it changed.
	tick  time.Duration
	reset bool
	// cautionTick is non-zero while running at the caution floor.
	cautionTick time.Duration
	// skip sheds this tick: the pacing queue is deeper than it could wait.
	skip bool
}

// sweepTickGate backs the tick off to the caution floor while GitHub is
// pacing the hive after a rate-limit refusal, sheds the tick outright when
// the pacing queue is already deeper than selfAuthoredSweepTickBudget, and
// resumes the adaptive interval once the window ends. shed is
// hgithub.ShedLowPriorityTick (injected by tests).
func (c *Engine) sweepTickGate(interval, cautionTick time.Duration, shed func(string, time.Duration) bool) sweepTickDecision {
	d := sweepTickDecision{tick: interval, cautionTick: cautionTick}
	if !c.cautionEngaged() {
		if cautionTick != 0 {
			d.cautionTick = 0
			d.reset = true
		}
		return d
	}
	if want := cautionSweepInterval(interval); cautionTick != want {
		d.cautionTick = want
		d.tick = want
		d.reset = true
		c.info("self-authored automerge sweep backing off while rate-limit caution window is engaged", "interval", want)
	} else {
		d.tick = want
	}
	if shed != nil && shed("automerge:self-authored-sweep", selfAuthoredSweepTickBudget) {
		d.skip = true
	}
	return d
}

// cautionEngaged reports whether the slow-start caution window is engaged;
// tests inject the predicate through Options.
func (c *Engine) cautionEngaged() bool {
	if c.rateLimitCautious != nil {
		return c.rateLimitCautious()
	}
	return hgithub.RateLimitCautionEngaged()
}

func isRateLimited(err error) bool {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var ab *gh.AbuseRateLimitError
	return errors.As(err, &ab)
}

// StartSelfAuthoredAutoMergeSweep starts the sweep loop in its own goroutine.
// The returned channel closes once that loop has exited after ctx is
// cancelled, including any sweep tick that was in flight, or immediately when
// the sweep does not start at all. A caller that restarts the sweep on a
// rebuilt client (#9621) waits on it so two sweeps never overlap.
func (c *Engine) StartSelfAuthoredAutoMergeSweep(ctx context.Context, maxMerges int, acmmAllowed bool, acmmLevel *int) <-chan struct{} {
	done := make(chan struct{})
	if !c.ready() {
		close(done)
		return done
	}
	if !acmmAllowed && !c.currentTrustedAuthorPolicy().Enabled && !c.currentContributorLanePolicy().Enabled {
		level := "unset"
		if acmmLevel != nil {
			level = fmt.Sprintf("%d", *acmmLevel)
		}
		c.info("self-authored auto-merge sweep disabled: acmm_level below minimum (or auto_merge.self_authored is off)",
			"acmm_level", level, "min_acmm_level", selfMergeMinACMMLevel)
		close(done)
		return done
	}
	if c.openPRs != nil {
		go func() {
			defer close(done)
			c.runEventDrivenSweep(ctx, maxMerges, acmmAllowed)
		}()
		c.info("automerge lanes started event-driven on the forgestate cache",
			"floor_interval", automergeSweepFloorInterval,
			"merge_freshness_bound", automergeMergeFreshnessBound,
			"repos", len(c.transport.Repositories()))
		return done
	}
	repos := len(c.transport.Repositories())
	interval := selfAuthoredSweepInterval(repos)
	ctx = lowPriority(ctx)
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		c.sweepClock.armFloorEvery(c.now(), interval)
		// cautionTick is non-zero while the ticker is running at the caution
		// floor instead of the adaptive interval.
		var cautionTick time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.sweepTrigger.C():
				// A webhook nudge (CI failure on a PR, operator row action)
				// names the PRs; evaluate only those now. A full request
				// (nil targets) is a regular pass brought forward.
				c.sweepPass(ctx, maxMerges, acmmAllowed, c.sweepTrigger.take(), "event")
			case <-t.C:
				gate := c.sweepTickGate(interval, cautionTick, hgithub.ShedLowPriorityTick)
				if gate.reset {
					t.Reset(gate.tick)
				}
				cautionTick = gate.cautionTick
				tick := interval
				if cautionTick != 0 {
					tick = cautionTick
				}
				c.sweepClock.armFloorEvery(c.now(), tick)
				if gate.skip {
					continue
				}
				result := c.sweepPass(ctx, maxMerges, acmmAllowed, nil, "tick")
				if result == nil {
					continue
				}
				if next, changed := nextSelfAuthoredSweepInterval(repos, interval, result); changed {
					t.Reset(next)
					c.sweepClock.armFloorEvery(c.now(), next)
					c.info("self-authored automerge sweep interval adjusted",
						"previous_interval", interval,
						"next_interval", next,
						"repos", repos,
						"candidates", result.Candidates,
						"default_candidate_allowance", selfAuthoredSweepCandidateAllowance)
					interval = next
					if cautionTick != 0 {
						cautionTick = cautionSweepInterval(interval)
						t.Reset(cautionTick)
					}
				}
			}
		}
	}()
	c.info("self-authored automerge sweep started", "interval", interval, "repos", len(c.transport.Repositories()))
	return done
}

// sweepPass runs the three lanes once, in order, sharing one merge budget,
// then reconciles stage labels. targets narrows an event-driven pass to the
// PRs a webhook named (nil: everything); cause names what started the pass
// for the log. It returns nil when the self-authored lane failed outright
// (its callers then keep the current cadence).
func (c *Engine) sweepPass(ctx context.Context, maxMerges int, acmmAllowed bool, targets SweepTargets, cause string) *AutoMergeSweepResult {
	c.sweepClock.begin(cause, targets, c.now())
	defer func() { c.sweepClock.end(c.now()) }()
	result := &AutoMergeSweepResult{}
	if acmmAllowed {
		var err error
		result, err = c.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: maxMerges, Targets: targets})
		if err != nil {
			c.warn("self-authored automerge sweep failed", "error", err, "cause", cause)
			// A rate-limit refusal may be go-github's cached verdict
			// from a token that has since rotated. Re-read the real
			// limits (free, and it updates that cache) so the next tick
			// is decided on current truth instead of repeating a stale
			// refusal for the rest of the window.
			if isRateLimited(err) {
				c.refreshRateLimitCache(ctx)
			}
			return nil
		}
	}
	remaining := maxMerges
	if remaining <= 0 {
		remaining = DefaultAutoMergeSweepMaxMerges
	}
	remaining -= len(result.Merged)
	if remaining > 0 && c.currentTrustedAuthorPolicy().Enabled {
		trustedResult, trustedErr := c.SweepTrustedAuthorAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: remaining, SelfAuthoredLaneActive: acmmAllowed, Targets: targets})
		if trustedErr != nil {
			c.warn("trusted-author automerge sweep failed", "error", trustedErr, "cause", cause)
			if isRateLimited(trustedErr) {
				c.refreshRateLimitCache(ctx)
			}
		} else {
			result.Seen += trustedResult.Seen
			result.Skipped += trustedResult.Skipped
			result.Candidates += trustedResult.Candidates
			result.Merged = append(result.Merged, trustedResult.Merged...)
		}
	}
	remaining = maxMerges
	if remaining <= 0 {
		remaining = DefaultAutoMergeSweepMaxMerges
	}
	remaining -= len(result.Merged)
	if remaining > 0 && c.currentContributorLanePolicy().Enabled {
		laneResult, laneErr := c.SweepContributorLaneAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: remaining, Targets: targets})
		if laneErr != nil {
			c.warn("contributor-lane automerge sweep failed", "error", laneErr, "cause", cause)
			if isRateLimited(laneErr) {
				c.refreshRateLimitCache(ctx)
			}
		} else {
			result.Seen += laneResult.Seen
			result.Skipped += laneResult.Skipped
			result.Candidates += laneResult.Candidates
			result.Merged = append(result.Merged, laneResult.Merged...)
		}
	}
	if stageResult, stageErr := c.ApplyStageLabels(ctx); stageErr != nil {
		c.warn("stage labels reconcile failed", "error", stageErr, "items", stageResult.Items, "writes", stageResult.Writes)
	} else if stageResult.Writes+stageResult.Comments > 0 {
		c.info("stage labels reconciled", "items", stageResult.Items, "writes", stageResult.Writes, "comments", stageResult.Comments)
	}
	return result
}

// listOpenAppAuthoredPullRequests returns every open PR in owner/repo. Uses the
// PR list endpoint (not issue search) because the caller needs PullRequest
// objects: PullRequests.List populates Labels and Head.SHA, so cheap gates can
// run before paying for PullRequests.Get. It does not populate MergeableState;
// PRs that survive the cheap gates are fetched once for that safety-critical
// evaluation data and fetched again immediately before merge to re-verify SHA.
func (c *Engine) listOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []*gh.PullRequest
	for {
		prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
		}
		out = append(out, prs...)
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// trySweepSelfAuthoredPR evaluates and, if eligible, merges one App-authored
// PR. Re-fetches the PR immediately before merging to re-verify the head SHA
// against the one that was evaluated as green — the same
// evaluated-then-re-verified-at-merge-time safety property trySweepQueuedPR
// gets from the queue approval's recorded HeadSHA, just without a stored
// approval record to compare against (there is no queue step in this path).

func (c *Engine) releaseHoldActive(ctx context.Context, owner, repo, branch string) (bool, string, error) {
	branch = strings.TrimSpace(branch)
	if c == nil || c.gh == nil || branch == "" {
		return false, "", nil
	}
	br, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 3)
	if err != nil {
		return false, "release-hold-check", err
	}
	sha := br.GetCommit().GetSHA()
	if sha == "" {
		return false, "", nil
	}
	statuses, _, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return false, "release-hold-check", err
	}
	for _, st := range statuses.Statuses {
		if st == nil || !strings.EqualFold(st.GetContext(), releaseInProgressStatusContext) {
			continue
		}
		if !strings.EqualFold(st.GetState(), "pending") {
			return false, "", nil
		}
		updated := st.GetUpdatedAt().Time
		if updated.IsZero() || c.now().Sub(updated) <= releaseInProgressMaxAge {
			return true, releaseInProgressStatusContext, nil
		}
		return false, "", nil
	}
	return false, "", nil
}

func (c *Engine) trySweepSelfAuthoredPR(ctx context.Context, displayRepo, owner, repo string, number int, branchUpdateAllowed bool) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	lane, laneOK := c.sweepLaneForAuthor(author)
	if !laneOK {
		// Neither the App's own PR nor a trusted bot's: this path never touches
		// other PRs, matching the human-queue sweep's untouched behavior for
		// PRs it does not own. Defense in depth — prefilterSelfAuthoredPR
		// already filtered on author, but a PR can change hands (rare, but
		// GitHub permits transferring PR authorship attribution in some flows)
		// between listing and evaluating it here.
		return AutoMergeSweepEvent{}, "not-app-authored", nil
	}
	selfLabels := labelNames(pr.Labels)
	// The refetched PR is the one that merges: a title or label edited
	// since listing re-applies the human-merge rule (hivecommons/hive#15442).
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), selfLabels); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	blocked := c.labelBlockReason(selfLabels, true)
	// A conflicting head — GitHub's mergeable_state or the needs-rebase
	// label a lane already put on it — is asked the superseded question
	// before the label block returns: a PR whose issue another merged PR
	// closed is closed, not left to wear needs-rebase (hivecommons/hive#15208).
	if lane == "self-authored" && (prMergeableStateConflicting(pr) || blocked == autoMergeReasonLabelNeedsRebase) {
		if closer, ok := c.transport.(supersededCloserTransport); ok {
			res, err := closer.CloseSupersededHivePRIfEligible(ctx, owner+"/"+repo, pr)
			if err != nil {
				c.warn("self-authored sweep: superseded check failed", "repo", displayRepo, "pr", number, "error", err)
			} else if res.Closed {
				c.info("self-authored sweep: conflicting PR closed as superseded", "repo", displayRepo, "pr", number, "closer", res.CloserPR, "issue", res.Issue, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason)
				return AutoMergeSweepEvent{}, autoMergeReasonClosedSuperseded, nil
			}
		}
	}
	// A head GitHub reports as changing no files has nothing to merge: the
	// change is already on the base. The closer proves it with the compare
	// and closes the hive's own PR as superseded (hivecommons/hive#15499)
	// before a review cycle is spent asking a person to.
	if lane == "self-authored" && hgithub.PRChangedFilesClaimsEmpty(pr) {
		if closer, ok := c.transport.(emptyDiffCloserTransport); ok {
			verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, pr.GetHead().GetSHA())
			res, err := closer.CloseEmptyDiffHivePRIfEligible(ctx, owner+"/"+repo, pr, verdict.RedundantOf)
			if err != nil {
				c.warn("self-authored sweep: empty-diff check failed", "repo", displayRepo, "pr", number, "error", err)
			} else if res.Closed {
				c.info("self-authored sweep: empty-diff PR closed as superseded", "repo", displayRepo, "pr", number, "redundant_of", res.RedundantOf, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason)
				return AutoMergeSweepEvent{}, autoMergeReasonClosedEmptyDiff, nil
			}
		}
	}
	// A needs-rebase label left on a head GitHub now reports merely behind
	// is stale: the gate still sees a mergeable PR (hivecommons/hive#15103).
	if blocked != "" && !staleNeedsRebaseLabel(blocked, pr) {
		return AutoMergeSweepEvent{}, blocked, nil
	}
	if c.transport.IsExemptLabels(selfLabels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	evaluatedHeadSHA := ""
	if pr.GetHead() != nil {
		evaluatedHeadSHA = pr.GetHead().GetSHA()
	}
	if evaluatedHeadSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
	verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, evaluatedHeadSHA)
	c.noteStageVerdict(owner, repo, number, verdict)
	priorApprover, _ := c.trustedHumanApprovalForRequiresHuman(ctx, owner, repo, number, evaluatedHeadSHA, verdict)
	priorChecked := verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true})
	// A trusted maintainer's changes_requested on this head outranks every
	// swarm verdict and the merge gates below (hivecommons/hive#13041): the
	// PR goes to 070 and the planner kicks the author agent with the review
	// text. Mirrors the trusted-author and contributor lanes, which also
	// refuse to merge over an outstanding changes_requested.
	if c.humanChangesRequestedForHead(ctx, owner, repo, number, evaluatedHeadSHA) {
		return AutoMergeSweepEvent{}, autoMergeReasonHumanChangesRequested, nil
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, evaluatedHeadSHA, lane),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)

	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	// A native merge queue tests the merged result itself, and updating the
	// branch would push a new head that drops any queue entry — but the
	// current-base gate stays in force unless the operator explicitly set
	// auto_merge.native_merge_queue.skip_current_base_gate.
	queueSkipsBase := sync.OnceValue(func() bool { return c.nativeQueueSkipsCurrentBase(ctx, owner+"/"+repo, pr) })
	if strings.EqualFold(pr.GetMergeableState(), "behind") && !queueSkipsBase() && !c.requireCurrentBaseFor(owner+"/"+repo) {
		if !branchUpdateAllowed {
			// Keep the cause: still behind, retried next tick, not CI
			// state and not author work (hivecommons/hive#12607).
			return AutoMergeSweepEvent{}, autoMergeReasonBehindBudgetExhausted, nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "behind", c.isForkPR(pr))
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, evaluatedHeadSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, evaluatedHeadSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		if approveReason := c.detectActionRequiredWorkflowRuns(ctx, displayRepo, owner, repo, lane, pr, number, evaluatedHeadSHA, reason); approveReason != "" {
			return AutoMergeSweepEvent{}, approveReason, nil
		}
	}
	if !green {
		// A trusted bot's dependency-bump head never gets the contexts its
		// workflow skips by design (#12004); re-evaluate treating those as
		// satisfied. Every other required check must still be green.
		green, reason, err = c.recheckSkippedByDesign(ctx, displayRepo, owner, repo, baseBranch, pr, number, evaluatedHeadSHA, headPushedAt, reason)
		if err != nil {
			return AutoMergeSweepEvent{}, reason, err
		}
	}
	if !green {
		// A missing required check on a trusted-lane head may be a workflow
		// run GitHub is holding for approval; post /ok-to-test and surface it
		// distinctly rather than polling forever (automerge_held_runs.go).
		return AutoMergeSweepEvent{}, c.resolveMissingRequiredCheck(ctx, displayRepo, owner, repo, pr, number, evaluatedHeadSHA, reason, headPushedAt), nil
	}

	// Intent tier gate (#6258). The human merge lane never lets a PR reach
	// trySweepQueuedPR at a tier intent enforcement refuses (writeMergeEligible
	// drops it first); this path lists the App's PRs independently, so it
	// must ask the same question itself — via intent.EvaluateForAppSelfMerge,
	// the tier gate written for this path — or every green App PR merges
	// regardless of tier. Consulted after commitGreen so the extra files
	// fetch is only spent on PRs that are otherwise mergeable, and before the
	// approval desk so the desk still only sees requests policy permits.
	//
	// Human-merge paths (#11038) come first and do not depend on intent: the
	// App self-merge contract authorizes Tier 2/3 without a person, so a path
	// the operator reserved for a person must be refused before
	// EvaluateForAppSelfMerge is ever asked. A diff fence carried from an
	// adopted contributor PR (automerge_adoption_fence.go) is refused the
	// same way: the App PR carries the fenced diff, so a person merges it.
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	if intentReason, err := c.selfMergeIntentGate(ctx, displayRepo, owner, repo, pr, author, selfLabels); err != nil {
		return AutoMergeSweepEvent{}, intentReason, err
	} else if intentReason != "" {
		return AutoMergeSweepEvent{}, intentReason, nil
	}

	// Current-base gate (hivecommons/hive#13634). Consulted only once the
	// head is green and every policy gate above has passed: a red or pending
	// head is the author's to fix and must not be re-based under them, and a
	// head policy refuses would be updated for nothing. Sits before the desk
	// so the desk is only asked about a head that would merge this tick.
	if c.requireCurrentBaseFor(owner+"/"+repo) && !queueSkipsBase() {
		if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, lane, number, pr, evaluatedHeadSHA, priorApprover, priorChecked); reviewReason != "" {
			return AutoMergeSweepEvent{}, reviewReason, nil
		}
		if ev, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, evaluatedHeadSHA, branchUpdateAllowed, autoMergeReasonBehindBudgetExhausted, c.isForkPR(pr), stageLaneSelf); stop {
			return ev, reason, err
		}
	}

	// Approval desk (RFC #4000). Consulted AFTER the sweep's own eligibility
	// checks so the desk only ever sees requests the legacy gate already
	// permits — it can withhold a merge, never widen authority beyond what
	// SelfAuthoredAutoMergeAllowed already granted upstream. No-op when no hook
	// is installed, which is the default; see automerge_desk.go.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindSelfMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labelNames(pr.Labels),
		ChecksGreen: true, // commitGreen returned green immediately above
		HeadSHA:     evaluatedHeadSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	// Freshness-bounded read-through of the cached PR (hivecommons/hive#13377):
	// refuses on a stale or not-ready snapshot, a closed PR, or a moved head.
	if gateReason, err := c.forgestateMergeGate(ctx, owner, repo, number, evaluatedHeadSHA); gateReason != "" || err != nil {
		return AutoMergeSweepEvent{}, gateReason, err
	}
	// Re-verify the head SHA immediately before merging: a push landing
	// between the green-check above and the merge call below must never be
	// squashed without having gone through commitGreen itself.
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != evaluatedHeadSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}
	// Review gate (#12592), on the re-verified head immediately before the
	// merge call: the swarm verdict for exactly the SHA about to merge.
	// Shadow (the default) records and merges; enforce holds.
	if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, lane, number, current, evaluatedHeadSHA, priorApprover, priorChecked); reviewReason != "" {
		return AutoMergeSweepEvent{}, reviewReason, nil
	}

	if hold := mergeTrainCandidateHold(ctx); hold != "" {
		return AutoMergeSweepEvent{}, hold, nil
	}

	// Queue-enabled bases use GitHub's native queue after the same gates.
	// Other bases retain the direct merge and remembered refusal handling.
	if reason, err := c.tryNativeQueue(ctx, current, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: evaluatedHeadSHA, state: current.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, current, evaluatedHeadSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, evaluatedHeadSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			if !branchUpdateAllowed {
				return AutoMergeSweepEvent{}, autoMergeReasonChecksExpected, nil
			}
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "required-checks-expected", c.isForkPR(pr))
		}
		if refused := c.refusedMerge(attempt, err); refused != "" {
			return AutoMergeSweepEvent{}, refused, nil
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Audit the merge like MergePR does (pullrequest.go): the activity
	// collector counts pr_merged entries from this trail, and the /fleet L6
	// health verdict is judged on them. When the fleet's merges moved to this
	// sweep, the unaudited path made merging hives read as "no merge in Nd"
	// red on /fleet (observed live on kubestellar/console, 2026-08-26).
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: "", // no queuer in the self-authored path — the App merges its own PR
		HeadSHA:  evaluatedHeadSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     lane,
	}
	c.info("self-authored automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "lane", lane, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) releaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error) {
	if c == nil || c.transport == nil {
		return false, "", nil
	}
	transport, ok := c.transport.(levelHoldTransport)
	if !ok {
		return false, "", nil
	}
	return transport.ReleaseLevelHoldIfEligible(ctx, owner, repo, pr)
}

// sweepLaneForAuthor reports which self-authored-sweep lane a PR author falls
// in: "self-authored" for the App's own login, "trusted-bot" for a login in
// the operator's trusted_bot_authors set, or ok=false for anyone else. The
// lane is recorded on the mutation claim and the merge log so audits can tell
// the two apart; both lanes pass through exactly the same eligibility gates.
func (c *Engine) sweepLaneForAuthor(author string) (string, bool) {
	author = strings.TrimSpace(author)
	if author == "" {
		return "", false
	}
	if c != nil && c.transport != nil && strings.EqualFold(author, c.transport.AppBotLogin()) {
		return "self-authored", true
	}
	if c == nil || c.trustedBotAuthors == nil {
		return "", false
	}
	if c.trustedBotAuthors()[strings.ToLower(author)] {
		return "trusted-bot", true
	}
	return "", false
}

// trustedBotLaneSkipReason is the trusted-author tier's skip reason for a PR
// it hands off to the self-authored sweep's "trusted-bot" lane.
const trustedBotLaneSkipReason = prstage.TrustedBotLaneSkipReason

// trustedAuthorReasonAppAuthored is the trusted-author tier's skip reason
// for the App's own PRs, which the self-authored lane owns. It is
// namespaced to this tier so the skip line and the stage observation never
// read as the contributor lane's.
const trustedAuthorReasonAppAuthored = prstage.TrustedAuthorReasonAppAuthored

// isTrustedBotLaneAuthor reports whether author falls in the "trusted-bot"
// lane of sweepLaneForAuthor.
func (c *Engine) isTrustedBotLaneAuthor(author string) bool {
	lane, ok := c.sweepLaneForAuthor(author)
	return ok && lane == "trusted-bot"
}

func trustedRegistryClanker(labels []string) bool {
	return hasLabelFold(labels, "role/trusted") && hasLabelFold(labels, "author/clanker")
}

func (c *Engine) sentinelLabelPresent(labels []string) bool {
	sentinel := c.sentinelLabelName()
	if sentinel == "" {
		sentinel = defaultSentinelLabel
	}
	return hasLabelFold(labels, sentinel)
}

// isHeld is the sweep's single hold predicate. It defers to the transport so
// the configured hold set (generic substrings plus the exact hive-scoped
// `hive-pause/<hive-id>` dashboard hold) gates merges exactly as it gates
// enumeration. A nil engine or transport fails closed to the generic set.
func (c *Engine) isHeld(labels []string) bool {
	if c == nil || c.transport == nil {
		return hgithub.HasHoldLabel(labels)
	}
	return c.transport.IsHeldLabels(labels)
}

func (c *Engine) sentinelLabelName() string {
	if c == nil || c.sentinelLabel == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.sentinelLabel()))
}

func (c *Engine) labelBlockReason(labels []string, trusted bool) string {
	sentinelLabel := c.sentinelLabelName()
	holdLabels := labels
	if trusted && sentinelLabel != "" {
		holdLabels = make([]string, 0, len(labels))
		for _, label := range labels {
			if !strings.EqualFold(strings.TrimSpace(label), sentinelLabel) {
				holdLabels = append(holdLabels, label)
			}
		}
	}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		lower := strings.ToLower(trimmed)
		switch {
		case sentinelLabel != "" && strings.EqualFold(trimmed, sentinelLabel):
			if !trusted {
				return "sentinel"
			}
		case lower == "needs-rebase":
			return autoMergeReasonLabelNeedsRebase
		case lower == "needs-human":
			return "label:needs-human"
		case lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/"):
			return "label:" + lower
		}
	}
	if c.isHeld(holdLabels) {
		return "label:hold"
	}
	return ""
}

func (c *Engine) prefilterSelfAuthoredPR(pr *gh.PullRequest) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	if _, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); !ok {
		return "not-app-authored"
	}
	labels := labelNames(pr.Labels)
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, true); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	return ""
}

func (c *Engine) prefilterQueuedIssue(issue *gh.Issue, label string) string {
	if issue == nil || !issue.IsPullRequest() {
		return "not-pull-request"
	}
	labels := labelNames(issue.Labels)
	if !hasLabel(labels, label) {
		return "label-removed"
	}
	if reason := c.humanMergeRequiredReason(issue.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, false); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	return ""
}

func (c *Engine) prefilterTrustedAuthorPR(owner, repo string, pr *gh.PullRequest, policy TrustedAuthorPolicy) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	labels := labelNames(pr.Labels)
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, true); blocked != "" && !staleNeedsRebaseLabel(blocked, pr) {
		return blocked
	}
	if excluded := policy.excludedLabel(labels); excluded != "" {
		if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" && strings.EqualFold(excluded, sentinelLabel) {
			excluded = ""
		}
		if excluded != "" {
			return "excluded-label:" + excluded
		}
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	// The prefilter has no caller context; the admission read only reaches
	// GitHub to carry a decision over a base refresh, and the lane repeats
	// the read with its own context before merging.
	if _, ok := c.trustedAuthorOrOperatorDecision(context.Background(), owner, repo, pr, policy); !ok {
		return "untrusted-author-role"
	}
	return ""
}

// trustedAuthorRoleOperatorApprovedPrefix is the Role recorded on a
// TrustedAuthorDecision when an operator's "Approve and let hive merge"
// admitted the head of a PR whose author holds no trusted-author role. The
// merge audit comment and log carry it, so the merge is attributed to the
// person who approved it, never to the author's trust.
const trustedAuthorRoleOperatorApprovedPrefix = "operator-approved:"

// trustedAuthorOrOperatorDecision is trustedAuthorDecision with the one
// exception the maintainer's last word allows: an untrusted author's PR
// whose current head an operator admitted (the dashboard's "Approve and
// let hive merge") is treated as trusted for this head, attributed to the
// operator. Any other untrusted author stays refused (ok=false).
func (c *Engine) trustedAuthorOrOperatorDecision(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy TrustedAuthorPolicy) (TrustedAuthorDecision, bool) {
	author := hgithub.SafeGetLogin(pr.GetUser())
	decision := c.trustedAuthorDecision(author, policy.RequireRole)
	if decision.Allowed {
		return decision, true
	}
	login := c.operatorAdmission(ctx, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA())
	if login == "" {
		return decision, false
	}
	return TrustedAuthorDecision{Allowed: true, Role: trustedAuthorRoleOperatorApprovedPrefix + login}, true
}

// operatorApprovedDecision reports whether a decision came from
// trustedAuthorOrOperatorDecision's operator-admission exception.
func operatorApprovedDecision(d TrustedAuthorDecision) bool {
	return strings.HasPrefix(d.Role, trustedAuthorRoleOperatorApprovedPrefix)
}

func (c *Engine) listQueuedPullRequestIssues(ctx context.Context, owner, repo, label string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing queued PRs for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (c *Engine) trySweepTrustedAuthorPR(ctx context.Context, displayRepo, owner, repo string, number int, policy TrustedAuthorPolicy) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	author := hgithub.SafeGetLogin(pr.GetUser())
	if lane, _ := c.sweepLaneForAuthor(author); lane == stageLaneSelf {
		return AutoMergeSweepEvent{}, trustedAuthorReasonAppAuthored, nil
	}
	if reason := c.prefilterTrustedAuthorPR(owner, repo, pr, policy); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	decision, ok := c.trustedAuthorOrOperatorDecision(ctx, owner, repo, pr, policy)
	if !ok {
		return AutoMergeSweepEvent{}, "untrusted-author-role", nil
	}
	if operatorApprovedDecision(decision) {
		c.info("trusted-author lane admitted by operator approval", "repo", displayRepo, "pr", number, "author", author, "admitted_by", strings.TrimPrefix(decision.Role, trustedAuthorRoleOperatorApprovedPrefix), "head", pr.GetHead().GetSHA())
	} else {
		c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
	}
	verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, pr.GetHead().GetSHA())
	c.noteStageVerdict(owner, repo, number, verdict)
	priorApprover, _ := c.trustedHumanApprovalForRequiresHuman(ctx, owner, repo, number, pr.GetHead().GetSHA(), verdict)
	priorChecked := verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true})
	permission := trustedAuthorPermission{Allowed: true, Level: "not-required"}
	if operatorApprovedDecision(decision) {
		permission.Level = "operator-approved"
	}
	if policy.RequireGitHubPermission && !operatorApprovedDecision(decision) {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "author-permission-missing", nil
		}
	}
	if c.isForkPR(pr) && !policy.RequireGitHubPermission && !operatorApprovedDecision(decision) {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "fork-non-member", nil
		}
	}
	if blocked, err := c.hasOutstandingChangesRequested(ctx, owner, repo, number); err != nil {
		return AutoMergeSweepEvent{}, "review-state-check", err
	} else if blocked {
		return AutoMergeSweepEvent{}, "changes-requested", nil
	}
	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	// See trySweepSelfAuthoredPR: the queue bypass is an explicit opt-in.
	queueSkipsBase := sync.OnceValue(func() bool { return c.nativeQueueSkipsCurrentBase(ctx, owner+"/"+repo, pr) })
	if mergeableState == "behind" && !queueSkipsBase() && !c.requireCurrentBaseFor(owner+"/"+repo) {
		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}
		if headSHA == "" {
			return AutoMergeSweepEvent{}, "missing-head-sha", nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "behind", c.isForkPR(pr))
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return c.mergeForwardOrConflict(ctx, displayRepo, owner, repo, pr, number)
	}
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, headSHA, "trusted-author"),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	// Current-base gate (hivecommons/hive#13634): only a green head that
	// every gate above lets through is worth an update-branch commit.
	if c.requireCurrentBaseFor(owner+"/"+repo) && !queueSkipsBase() {
		if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, stageLaneTrusted, number, pr, headSHA, priorApprover, priorChecked); reviewReason != "" {
			return AutoMergeSweepEvent{}, reviewReason, nil
		}
		if ev, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, headSHA, true, "behind", c.isForkPR(pr), stageLaneTrusted); stop {
			return ev, reason, err
		}
	}
	if gateReason, err := c.forgestateMergeGate(ctx, owner, repo, number, headSHA); gateReason != "" || err != nil {
		return AutoMergeSweepEvent{}, gateReason, err
	}
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != headSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}
	// The swarm reviews human PRs too (hivecommons/hive#12974), so the same
	// review gate the self-authored lane runs sits in front of this merge:
	// a trusted author's role vouches for who they are, not for an
	// unreviewed head.
	if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, stageLaneTrusted, number, current, headSHA, priorApprover, priorChecked); reviewReason != "" {
		return AutoMergeSweepEvent{}, reviewReason, nil
	}
	if hold := mergeTrainCandidateHold(ctx); hold != "" {
		return AutoMergeSweepEvent{}, hold, nil
	}
	if reason, err := c.tryNativeQueue(ctx, current, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: headSHA, state: current.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, current, headSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, headSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "required-checks-expected", c.isForkPR(pr))
		}
		if refused := c.refusedMerge(attempt, err); refused != "" {
			return AutoMergeSweepEvent{}, refused, nil
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	if err := c.commentTrustedAuthorMerge(ctx, owner, repo, number, author, decision.Role, permission.Level, headSHA); err != nil {
		c.warn("trusted-author automerge audit comment failed", "repo", displayRepo, "pr", number, "error", err)
	}
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     "trusted-author",
	}
	c.info("trusted-author automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "role", decision.Role, "permission", permission.Level, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) authorRepoPermission(ctx context.Context, owner, repo, author string) (trustedAuthorPermission, error) {
	level, _, err := c.gh.Repositories.GetPermissionLevel(ctx, owner, repo, author)
	if err != nil {
		return trustedAuthorPermission{}, err
	}
	permission := strings.ToLower(strings.TrimSpace(level.GetPermission()))
	roleName := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
	display := roleName
	if display == "" {
		display = permission
	}
	allowed := permission == "admin" || permission == "write" || roleName == "admin" || roleName == "maintain" || roleName == "write"
	return trustedAuthorPermission{Allowed: allowed, Level: display}, nil
}

func (c *Engine) isForkPR(pr *gh.PullRequest) bool {
	if pr == nil || pr.GetHead() == nil || pr.GetBase() == nil {
		return false
	}
	headRepo := ""
	if pr.GetHead().GetRepo() != nil {
		headRepo = pr.GetHead().GetRepo().GetFullName()
	}
	baseRepo := ""
	if pr.GetBase().GetRepo() != nil {
		baseRepo = pr.GetBase().GetRepo().GetFullName()
	}
	return headRepo != "" && baseRepo != "" && !strings.EqualFold(headRepo, baseRepo)
}

func (c *Engine) updateBranchAndSkip(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, reason string, fork bool) (AutoMergeSweepEvent, string, error) {
	if err := c.executeUpdateBranchEffect(ctx, owner, repo, number, headSHA, reason); err != nil {
		if wait := preparationWaitReason(err); wait != "" {
			return AutoMergeSweepEvent{}, wait, nil
		}
		if c.requireCurrentBaseFor(owner+"/"+repo) && hgithub.IsUpdateBranchStaleHead(err) {
			return AutoMergeSweepEvent{}, basePreparationPrefix + "snapshot-changed", nil
		}
		switch classifyUpdateBranchError(err, fork) {
		case updateBranchUpToDate:
			return AutoMergeSweepEvent{}, "check-pending", nil
		case updateBranchForkNotEditable:
			c.info("automerge update-branch skipped: fork head not editable by the app", "repo", displayRepo, "pr", number, "reason", reason)
			return AutoMergeSweepEvent{}, autoMergeReasonForkNotEditable, nil
		case updateBranchConflict:
			return AutoMergeSweepEvent{}, "conflicting", nil
		}
		return AutoMergeSweepEvent{}, "update-branch", err
	}
	c.info("automerge update-branch", "repo", displayRepo, "pr", number, "reason", reason)
	if c.requireCurrentBaseFor(owner + "/" + repo) {
		return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, basePreparationReady, nil
	}
	return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, "check-pending", nil
}

const autoMergeReasonForkNotEditable = prstage.AutoMergeReasonForkNotEditable

type updateBranchFailure int

const (
	updateBranchOther updateBranchFailure = iota
	updateBranchConflict
	updateBranchUpToDate
	updateBranchForkNotEditable
)

// classifyUpdateBranchError separates the update-branch failures GitHub
// reports with overlapping statuses: 422 covers conflicts and an already
// current branch, while a 422 naming maintainer edits (or a 403 on a fork PR)
// means the app cannot write to the fork head. A 403 on a same-repo branch is
// a plain permission or rate-limit failure. An unrecognised 422 stays a conflict.
func classifyUpdateBranchError(err error, fork bool) updateBranchFailure {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return updateBranchOther
	}
	msg := strings.ToLower(ghErr.Message)
	switch ghErr.Response.StatusCode {
	case http.StatusForbidden:
		if fork {
			return updateBranchForkNotEditable
		}
		// TODO(hivecommons/hive#14589): a same-repo 403 "Resource not
		// accessible by integration" means the App lacks contents: write
		// and should mark the lane unauthorized (step: update-branch);
		// a rate-limit 403 must not. Wire through markUnauthorized once
		// the two are told apart here (isResourceNotAccessible).
	case http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(msg, "maintainer") || strings.Contains(msg, "permission") || strings.Contains(msg, "not authorized"):
			return updateBranchForkNotEditable
		case strings.Contains(msg, "merge conflict"):
			return updateBranchConflict
		case strings.Contains(msg, "up to date") || strings.Contains(msg, "up-to-date") || strings.Contains(msg, "no new commits"):
			return updateBranchUpToDate
		}
		return updateBranchConflict
	}
	return updateBranchOther
}

func (c *Engine) executeUpdateBranchEffect(ctx context.Context, owner, repo string, number int, headSHA, reason string) error {
	if c.requireCurrentBaseFor(owner + "/" + repo) {
		if err := c.claimBasePreparationUpdate(ctx, owner, repo, number, headSHA); err != nil {
			return err
		}
	}
	inputs := map[string]string{"expected_head_sha": headSHA, "reason": reason}
	observe := func(ctx context.Context) (effects.ExternalState, error) {
		pr, _, getErr := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_reconcile_branch_update"), owner, repo, number)
		if getErr != nil {
			return effects.ExternalState{}, getErr
		}
		return hgithub.BranchUpdateState(pr, headSHA, owner+"/"+repo+"#"+strconv.Itoa(number)), nil
	}
	_, err := effects.ExecuteReconciling(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: inputs,
	}, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
		var opts *gh.PullRequestBranchUpdateOptions
		if strings.TrimSpace(headSHA) != "" {
			opts = &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(headSHA)}
		}
		_, _, apiErr := c.gh.PullRequests.UpdateBranch(ctx, owner, repo, number, opts)
		var accepted *gh.AcceptedError
		if errors.As(apiErr, &accepted) {
			apiErr = nil
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: owner + "/" + repo + "#" + strconv.Itoa(number)}, nil
	})
	var rejected *gh.ErrorResponse
	if c.requireCurrentBaseFor(owner+"/"+repo) && errors.As(err, &rejected) && rejected.Response != nil &&
		(rejected.Response.StatusCode == http.StatusForbidden || rejected.Response.StatusCode == http.StatusUnprocessableEntity) {
		c.forgetRejectedPreparationUpdate(owner, repo, number, headSHA)
	}
	return err
}

func (c *Engine) hasOutstandingChangesRequested(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := map[string]string{}
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		seen = append(seen, reviews...)
		for _, review := range reviews {
			login := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(review.GetUser())))
			if login == "" {
				continue
			}
			state := strings.ToUpper(strings.TrimSpace(review.GetState()))
			if state == "APPROVED" || state == "CHANGES_REQUESTED" || state == "DISMISSED" {
				latest[login] = state
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return true, nil
		}
	}
	return false, nil
}

func (c *Engine) commentTrustedAuthorMerge(ctx context.Context, owner, repo string, number int, author, role, permission, sha string) error {
	body := fmt.Sprintf("merged by Hive trusted-author auto-merge: author @%s holds role %s and GitHub permission %s on repo; CI green at %s.", author, role, permission, sha)
	if login := strings.TrimPrefix(role, trustedAuthorRoleOperatorApprovedPrefix); login != role {
		body = fmt.Sprintf("merged by Hive on behalf of @%s, who approved this head (%s) with \"Approve and let hive merge\": author @%s holds no trusted-author role; CI green at %s.", login, stagelabels.OperatorApprovalAdmission(login), author, sha)
	}
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}

func (c *Engine) trySweepQueuedPR(ctx context.Context, displayRepo, owner, repo string, number int, label string) (eventOut AutoMergeSweepEvent, reasonOut string, errOut error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	defer func() { c.reconcileNativeQueueGate(ctx, nativeQueueLaneQueued, owner, repo, pr, reasonOut) }()
	c.noteStagePR(owner, repo, number, pr)
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	labels := hgithub.ExtractPRLabels(pr.Labels)
	if !hasLabel(labels, label) {
		return AutoMergeSweepEvent{}, "label-removed", nil
	}
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	// Hold labels outrank the merger queue (#5589): a hold applied AFTER a
	// merger queued the PR — including the hold guard re-applying one because
	// the branch moved while hold-gated — must stop the sweep, not race it.
	if c.isHeld(labels) {
		return AutoMergeSweepEvent{}, "held", nil
	}
	if c.transport.IsExemptLabels(labels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	author := hgithub.SafeGetLogin(pr.GetUser())
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, headSHA, "queued"),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return AutoMergeSweepEvent{}, autoMergeReasonNoAppBotLogin, nil
	}
	approval, ok, reason, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return AutoMergeSweepEvent{}, "queue-approval-check", err
	}
	if !ok {
		if reason != "" {
			return AutoMergeSweepEvent{}, reason, nil
		}
		return AutoMergeSweepEvent{}, autoMergeReasonNoHiveQueueApproval, nil
	}
	if approval.HeadSHA == "" {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval is missing a reviewed head SHA — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-missing-head", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-missing-head", nil
	}
	if approval.HeadSHA != headSHA {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval head changed since approval — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-head-changed", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-head-changed", nil
	}
	queuedBy := approval.QueuedBy
	if strings.EqualFold(author, queuedBy) {
		return AutoMergeSweepEvent{}, "self-merge-ban", nil
	}
	// SECURITY (audit F3): the self-merge ban above only proves queuer !=
	// author. It is defeated by a sockpuppet — a second account queues and
	// approves the first account's work — and on its own it lets ANY actor who
	// can get the merger-queue label applied merge anything. Re-verify the
	// merger tier here, at the point the merge actually happens, rather than
	// trusting the queue-time check in the dashboard handler.
	trusted, configured := c.isTrustedMerger(queuedBy)
	if !configured {
		return AutoMergeSweepEvent{}, autoMergeReasonNoMergerAuthz, nil
	}
	if !trusted {
		c.warn(autoMergeWarnUntrustedMerger, "owner", owner, "repo", repo, "pr", number,
			"queued_by", queuedBy, "author", author)
		return AutoMergeSweepEvent{}, autoMergeReasonUntrustedMerger, nil
	}

	mergeable := hgithub.MergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	// On a queue-required base GitHub reports blocked until the PR merges
	// through the queue; the queue, not this gate, settles that state.
	if mergeable != hgithub.MergeableYes && !(nativeQueueGatedState(pr) && c.nativeQueueEnabled(ctx, owner+"/"+repo, pr)) {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Human-merge paths (#11038): a merger queuing the PR is not the same as
	// a person merging it, so the queued lane honors the list too.
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}

	// Approval desk (RFC #4000) for the trusted human merge-queue lane.
	// Consulted only after the legacy queue approval, trusted-merger,
	// mergeability, and green-check gates pass, so enabling the desk can record
	// or withhold this operation but cannot widen merge authority.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindQueuedMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labels,
		ChecksGreen: true,
		HeadSHA:     headSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	if reason, err := c.tryNativeQueue(ctx, pr, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: headSHA, state: pr.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, pr, headSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, headSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			return AutoMergeSweepEvent{}, autoMergeReasonChecksExpected, nil
		}
		if refused := c.refusedMerge(attempt, err); refused != "" {
			return AutoMergeSweepEvent{}, refused, nil
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Same audit obligation as the self-authored path above: pr_merged on
	// the trail is what makes this merge count as hive output. path=queue:
	// a person queued this PR; the sweep only carried the merge out.
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathQueue)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: queuedBy,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Label:    label,
		Tier:     "queued",
	}
	c.info("automerge sweep merged PR", "repo", displayRepo, "pr", number, "queued_by", queuedBy, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) latestHiveQueueApproval(ctx context.Context, owner, repo string, number int) (hiveQueueApproval, bool, string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := hiveQueueApproval{}
	untrusted := false
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return hiveQueueApproval{}, false, "", err
		}
		seen = append(seen, reviews...)
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") {
				continue
			}
			queuedBy := parseHiveQueueReview(review.GetBody())
			if queuedBy == "" {
				continue
			}
			if !c.isHiveAppReviewAuthor(review) {
				untrusted = true
				c.warn(autoMergeWarnUntrustedQueueApproval, "owner", owner, "repo", repo, "pr", number, "review_author", hgithub.SafeGetLogin(review.GetUser()), "claimed_queued_by", queuedBy, "expected_app_bot", c.transport.AppBotLogin())
				continue
			}
			latest = hiveQueueApproval{QueuedBy: queuedBy, HeadSHA: review.GetCommitID()}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	if latest.QueuedBy != "" {
		return latest, true, "", nil
	}
	if untrusted {
		return latest, false, autoMergeReasonUntrustedQueueApproval, nil
	}
	return latest, false, "", nil
}

func (c *Engine) isHiveAppReviewAuthor(review *gh.PullRequestReview) bool {
	if c == nil || review == nil || strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return false
	}
	return strings.EqualFold(hgithub.SafeGetLogin(review.GetUser()), c.transport.AppBotLogin())
}

func (c *Engine) invalidateQueuedAutoMerge(ctx context.Context, owner, repo string, number int, label, body string) error {
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return fmt.Errorf("removing %s label: %w", label, err)
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("commenting on stale auto-merge approval: %w", err)
	}
	c.noteOwnStageComment(owner, repo, number)
	return nil
}

func parseHiveQueueReview(body string) string {
	matches := hiveQueueReviewRE.FindStringSubmatch(strings.TrimSpace(body))
	if len(matches) != 2 {
		return ""
	}

	return matches[1]
}

// commitGreen reports whether the head SHA is mergeable from a CI
// standpoint.
//
// Gating is REQUIRED-CHECKS-ONLY: commitGreen first asks which status
// contexts/check-run names are actually required for the target branch
// (requiredStatusCheckContexts). Any status or check-run whose context/name
// is NOT in that required set is skipped entirely, regardless of its state or
// conclusion — pending, failing, or cancelled non-required checks can never
// block self-merge. A required check still fully gates: pending blocks
// (return not-green, "pending" — the sweep must never squash a PR before its
// required CI has finished), and a failure/error/cancelled conclusion on a
// required check blocks too.
//
// Why required-only, not a hardcoded ignore-list: the previous approach
// (isMetaCheck/isIgnorableCICheck as an ALLOWLIST of names to ignore) is
// whack-a-mole against an open-ended set of non-required checks — #3611 added
// Playwright/Mobile Browser Tests/coverage-report/the chromium shard matrix,
// but "Detect untested files" (cancelled) still blocked #22471 and "Analyze
// (python)" (CodeQL, failure) still blocks #22450 because neither name was on
// the list. Every managed repo's ACTUAL required-checks set (e.g. console's
// main branch requires only "build-gate") is the one true source of what
// must be green; anything else is by definition non-required and must never
// wedge the queue.
//
// Where that required set comes from (see requiredStatusCheckContexts for
// the full precedence): branch protection first, then non-admin branch/rules
// fallbacks, then the operator-declared auto_merge.required_checks fallback.
// GitHub's protection APIs require administration:read on App tokens, but the
// branch payload exposes the same required status-check contexts to pushable
// installations.
//
// Unknown-source fallback: if the required-checks set cannot be determined by
// config, branch protection, the branch payload, or repository rulesets, the
// sweep no longer treats arbitrary red check-runs as required. It waits only
// while a check is still pending, then attempts the merge and lets GitHub's
// merge endpoint enforce the actual required checks server-side. A known-empty
// required set is not unknown: nothing is enforced server-side, so completed
// failing check-runs and statuses block locally.
func (c *Engine) commitGreen(ctx context.Context, owner, repo, branch, sha string) (bool, string, error) {
	return c.commitGreenForPR(ctx, owner, repo, branch, sha, 0, time.Time{}, nil)
}

func (c *Engine) commitGreenForPR(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache) (bool, string, error) {
	return c.commitGreenForPRSkipping(ctx, owner, repo, branch, sha, prNumber, headPushedAt, expectedChecks, nil)
}

func (c *Engine) commitGreenForPRSkipping(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache, skippedByDesign map[string]bool) (bool, string, error) {
	// The walk itself lives in hgithub.EvaluateCommitCI so the merge-request
	// watcher's positive-confirmation gate (#6173) evaluates a SHA with the
	// identical rules; only the required-set precedence is engine-specific.
	configRequired, configKnown := c.configRequiredChecksForRepo(owner + "/" + repo)
	required, requiredKnown, fromConfig, fallback, source := hgithub.RequiredStatusCheckContextsDetailedWithSource(ctx, c.gh, owner, repo, branch, configRequired, configKnown)
	actualRequired, actualKnown := required, requiredKnown && !fromConfig
	// A known-empty required set means the branch enforces nothing, so the
	// merge endpoint cannot be the source of truth: keep the positive-evidence
	// requirement but evaluate completed failures locally.
	positiveEvidenceGate := requiredKnown && len(required) == 0
	if positiveEvidenceGate {
		required, requiredKnown = nil, false
	}
	if prNumber > 0 {
		c.info("automerge required checks source", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source, "known", requiredKnown, "count", len(required))
		if !requiredKnown {
			c.info("automerge required checks unknown; relying on server-side enforcement", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source)
		}
	}
	if fallback && fromConfig {
		c.warnRequiredChecksFallback(owner, repo, branch)
	}
	st, err := hgithub.EvaluateCommitCI(ctx, c.gh, owner, repo, sha, hgithub.CommitCIOptions{
		Required:                            required,
		RequiredKnown:                       requiredKnown,
		RequiredKnownFromConfig:             fromConfig,
		UnknownRequiredChecksServerEnforced: !requiredKnown && !positiveEvidenceGate,
		RequiredKnownEmpty:                  positiveEvidenceGate,
		RequireEvidence:                     !requiredKnown,
		SkippedByDesign:                     skippedByDesign,
		MinHeadAge:                          c.minHeadAge,
		HeadPushedAt:                        headPushedAt,
		Now:                                 c.now,
	})
	if err != nil {
		return false, st.Reason, err
	}
	if prNumber > 0 && len(st.IgnoredMetaContexts) > 0 {
		c.info("automerge ignoring meta contexts on unprotected base", "pr", prNumber, "contexts", st.IgnoredMetaContexts)
	}
	if prNumber > 0 {
		c.noteStageRedChecks(owner, repo, prNumber, sha, st.FailingNonRequired)
		c.noteStageDCOGate(owner, repo, prNumber, sha, required, st.Observed)
	}
	if configKnown && actualKnown {
		c.warnRequiredChecksMismatch(owner, repo, configRequired, actualRequired, st.Observed)
	}
	if !st.Green && isFreshHeadOrMissingExpectedReason(st.Reason) && prNumber > 0 {
		c.info("automerge CI gate blocked PR", "repo", owner+"/"+repo, "pr", prNumber, "sha", sha, "reason", st.Reason)
	}
	if prNumber > 0 && !isFreshHeadOrMissingExpectedReason(st.Reason) {
		c.rememberEvaluatedHead(owner, repo, prNumber, sha)
	}
	reason := st.Reason
	if !st.Green && prNumber > 0 && classifyCIReason(reason) == ciReasonRed {
		// Red, but every failing check is also red on the base branch: the
		// merge is still blocked (we never merge on red), yet the recorded
		// skip reason names the base, not the head, so the stage engine and
		// the dashboard say whose fault it is (base_branch_red.go).
		if checks := stageBaseBranchRed(c.stagePolicy(), owner, repo, prNumber, sha); len(checks) > 0 {
			reason = baseBranchRedReason(checks)
		}
	}
	return st.Green, reason, nil
}

func isFreshHeadOrMissingExpectedReason(reason string) bool {
	return strings.HasPrefix(reason, "pending: head pushed ") || (strings.HasPrefix(reason, "pending: ") && strings.HasSuffix(reason, " has not started"))
}

func (c *Engine) warnRequiredChecksFallback(owner, repo, branch string) {
	if c == nil {
		return
	}
	key := strings.ToLower(owner + "/" + repo + "#" + branch)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("branch protection required checks unavailable; falling back to configured automerge required checks", "repo", owner+"/"+repo, "branch", branch)
}

func (c *Engine) warnRequiredChecksMismatch(owner, repo string, declared, actual, observed map[string]bool) {
	if c == nil {
		return
	}
	missing := mismatchedDeclaredChecks(declared, actual, observed)
	if len(missing) == 0 {
		return
	}
	key := strings.ToLower("mismatch:" + owner + "/" + repo)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("automerge required_checks override does not match repo protection", "repo", owner+"/"+repo, "declared", sortedSetKeys(declared), "actual", sortedSetKeys(actual), "using", "actual", "mismatch", missing)
}

func mismatchedDeclaredChecks(declared, actual, observed map[string]bool) []string {
	out := make([]string, 0)
	for name := range declared {
		if !actual[name] && !observed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requiredStatusCheckContexts returns the set of status-check contexts /
// check-run names that are actually required for branch, and whether that
// set could be determined at all. It is the single source of truth
// commitGreen gates on: membership in this set is what makes a check
// "required" (must be green) versus ignorable (any state/conclusion, never
// blocks).
//
// Precedence (first available source wins):
//  1. GitHub's branch-protection API (Repositories.GetRequiredStatusChecks).
//     Kept as a fallback in case the App ever does have admin-read scope, or
//     the branch is legitimately unprotected (gh.ErrBranchNotProtected — a
//     repo with zero required checks is a valid, common state, NOT an
//     error, so that case returns requiredKnown=true with an empty set).
//  2. Config: c.configRequiredChecks(), i.e. the operator-declared
//     auto_merge.required_checks list (config.AutoMergeConfig.RequiredCheckSet).
//  3. Neither available (no config list AND branch empty / API call failed
//     for a reason other than "not protected") → requiredKnown=false. The
//     caller must fall back to the OLD isMetaCheck/isIgnorableCICheck
//     allowlist rather than treating "we don't know the required set" as
//     "nothing is required" — see commitGreen's fail-closed comment.
func (c *Engine) requiredStatusCheckContexts(ctx context.Context, owner, repo, branch string) (map[string]bool, bool) {
	set, ok := c.configRequiredChecksForRepo(owner + "/" + repo)
	return hgithub.RequiredStatusCheckContexts(ctx, c.gh, owner, repo, branch, set, ok)
}

func labelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		if name := l.GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

func isGitHubStatus(err error, status int) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == status
}

func requiredChecksExpectedMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	msg := ghErr.Message
	if msg == "" {
		msg = ghErr.Error()
	}
	return requiredChecksExpectedRE.MatchString(msg)
}

// mergeQueueRequiredMergeError reports the 405 a base branch whose ruleset
// requires a merge queue returns for a direct merge ("Repository rule
// violations found ... Changes must be made through the merge queue"). The
// REST merge can never succeed there, so the sweeps record a skip reason
// instead of a merge-failed error every pass (hivecommons/hive#14018).
func mergeQueueRequiredMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return mergeQueueRequiredRE.MatchString(ghErr.Message) || mergeQueueRequiredRE.MatchString(ghErr.Error())
}

func mergeQueueBaseKey(owner, repo, base string) string {
	return strings.ToLower(owner+"/"+repo) + "#" + base
}

// noteMergeQueueBase records that base refused a direct merge as
// merge-queue-only.
func (c *Engine) noteMergeQueueBase(owner, repo, base string) {
	if c == nil || strings.TrimSpace(base) == "" {
		return
	}
	c.mergeQueueBasesMu.Lock()
	defer c.mergeQueueBasesMu.Unlock()
	if c.mergeQueueBases == nil {
		c.mergeQueueBases = make(map[string]time.Time)
	}
	c.mergeQueueBases[mergeQueueBaseKey(owner, repo, base)] = c.clock()
}

// mergeQueueBaseKnown reports whether base refused a direct merge as
// merge-queue-only within mergeQueueBaseRecheck.
func (c *Engine) mergeQueueBaseKnown(owner, repo, base string) bool {
	if c == nil || strings.TrimSpace(base) == "" {
		return false
	}
	key := mergeQueueBaseKey(owner, repo, base)
	c.mergeQueueBasesMu.Lock()
	defer c.mergeQueueBasesMu.Unlock()
	seen, ok := c.mergeQueueBases[key]
	if !ok {
		return false
	}
	if c.clock().Sub(seen) >= mergeQueueBaseRecheck {
		delete(c.mergeQueueBases, key)
		return false
	}
	return true
}

func mergeAPIErrorNotApplied(err error) error {
	if err == nil {
		return nil
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	switch ghErr.Response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusForbidden:
		return effects.NotApplied(err)
	}
	return err
}

// executeMergeEffect runs the merge call through the mutation boundary and
// brackets it with a merge attempt (hivecommons/hive#15245) so the tracker
// shows "Merging" for the call's duration and a refusal's reason after.
func (c *Engine) executeMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int, head string, merge func(context.Context) (effects.Result, error)) (effects.Result, error) {
	slug := owner + "/" + repo
	hgithub.BeginMergeAttemptOnHead(slug, number, hgithub.PRAuditPathSweep, head)
	out, err := c.executeMergeEffectOnce(ctx, claim, owner, repo, number, merge)
	switch {
	case err != nil:
		hgithub.EndMergeAttempt(slug, number, false, err)
	case out.Provenance == "":
		hgithub.EndMergeAttempt(slug, number, false, errors.New("merge-not-applied"))
	default:
		hgithub.EndMergeAttempt(slug, number, true, nil)
	}
	return out, err
}

func (c *Engine) executeMergeEffectOnce(ctx context.Context, claim effects.Claim, owner, repo string, number int, merge func(context.Context) (effects.Result, error)) (effects.Result, error) {
	out, err := effects.Execute(ctx, c.mutation, claim, merge)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		return out, err
	}
	reconciled, recErr := c.reconcileMergeEffect(ctx, claim, owner, repo, number)
	if recErr != nil {
		return out, fmt.Errorf("%w (reconciliation lookup failed: %v)", err, recErr)
	}
	c.info("reconciled unresolved automerge effect", "repo", owner+"/"+repo, "pr", number, "applied", reconciled.Applied)
	return effects.Execute(ctx, c.mutation, claim, merge)
}

func (c *Engine) reconcileOpenPRMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) {
	if _, ok := c.mutation.(effects.Reconciler); !ok {
		return
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, effects.ExternalState{Applied: false}); err == nil {
		c.info("reconciled open PR automerge effect as not applied", "repo", owner+"/"+repo, "pr", number)
	}
}

func (c *Engine) reconcileMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) (effects.ExternalState, error) {
	if c == nil || c.gh == nil {
		return effects.ExternalState{}, hgithub.ErrNoGitHubClient
	}
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_reconcile_merge"), owner, repo, number)
	if err != nil {
		return effects.ExternalState{}, err
	}
	state := effects.ExternalState{Applied: pr.GetMerged(), Provenance: pr.GetMergeCommitSHA()}
	if !pr.GetMerged() {
		state.Provenance = ""
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, state); err != nil {
		return state, err
	}
	return state, nil
}

func (c *Engine) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

// warnSelfAuthoredSkipOnce logs the skip at WARN once per repo#pr+reason+error
// and at Debug on later ticks, so a PR stuck on the same condition does not
// repeat the line every sweep.
func (c *Engine) warnSelfAuthoredSkipOnce(repo string, number int, reason string, err error) {
	if c == nil {
		return
	}
	key := fmt.Sprintf("%s#%d|%s|%v", repo, number, reason, err)
	c.selfSkipWarnedMu.Lock()
	if c.selfSkipWarned == nil || len(c.selfSkipWarned) > 4096 {
		c.selfSkipWarned = map[string]struct{}{}
	}
	_, seen := c.selfSkipWarned[key]
	c.selfSkipWarned[key] = struct{}{}
	c.selfSkipWarnedMu.Unlock()
	if seen {
		c.debug("self-authored automerge sweep skipped PR", "repo", repo, "pr", number, "reason", reason, "error", err)
		return
	}
	c.warn("self-authored automerge sweep skipped PR", "repo", repo, "pr", number, "reason", reason, "error", err)
}

func (c *Engine) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

func (c *Engine) debug(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Debug(msg, args...)
	}
}

// mergeTrainPolicyOption folds the legacy MergeTrainEnabled switch into
// the policy func when no full policy is wired.
func mergeTrainPolicyOption(opts Options) func() MergeTrainPolicy {
	if opts.MergeTrain != nil || opts.MergeTrainEnabled == nil {
		return opts.MergeTrain
	}
	return func() MergeTrainPolicy {
		p := DefaultMergeTrainPolicy
		p.Enabled = opts.MergeTrainEnabled()
		return p
	}
}
