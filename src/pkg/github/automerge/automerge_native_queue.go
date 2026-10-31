package automerge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	nativeQueueReason        = prstage.AutoMergeReasonNativeQueued
	nativeQueueReasonRead    = prstage.AutoMergeReasonNativeQueuePrefix + "-read"
	nativeQueueReasonEnqueue = prstage.AutoMergeReasonNativeQueuePrefix + "-enqueue"
	nativeQueueReasonMethod  = prstage.AutoMergeReasonNativeQueueMethod
	nativeQueueReasonPaused  = "repo-paused-or-disabled"

	// Lanes passed to reconcileNativeQueueGate by callers outside the three
	// authoring lanes.
	nativeQueueLaneQueued   = "queued"
	nativeQueueLaneInactive = "inactive"
)

// inactiveQueueInterval bounds how often a paused repo's open PRs are re-read;
// each read costs GraphQL points per PR.
const inactiveQueueInterval = 10 * time.Minute

// DefaultNativeMergeQueueProbeTTL mirrors
// config.DefaultNativeMergeQueueProbeTTL: how long a base branch's
// "has a merge queue" answer is reused before it is re-read.
const DefaultNativeMergeQueueProbeTTL = 30 * time.Minute

// NativeMergeQueuePolicy is the live auto_merge.native_merge_queue.* policy
// (hivecommons/hive#15198). The zero value is OFF: no lane reads, enqueues
// or withdraws a native merge queue entry, so a hive without queues sees
// no behaviour change.
type NativeMergeQueuePolicy struct {
	// Enabled turns the hand-off on.
	Enabled bool
	// Repos is the lower-cased owner/repo allow-list; empty means every
	// watched repository once Enabled.
	Repos map[string]bool
	// Bases is an optional exact repo#base allow-list, preserving branch case.
	Bases map[string]bool
	// SkipCurrentBaseGate lets a queue-enabled base bypass the
	// require_current_base gate and the behind update-branch. Off keeps
	// both gates in force exactly as on any other base.
	SkipCurrentBaseGate bool
	// ProbeTTL bounds the per-base "has a merge queue" cache; <= 0 means
	// DefaultNativeMergeQueueProbeTTL.
	ProbeTTL time.Duration
}

func (p NativeMergeQueuePolicy) repoAllowed(repo string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Repos) == 0 {
		return true
	}
	return p.Repos[strings.ToLower(strings.TrimSpace(repo))]
}

func (p NativeMergeQueuePolicy) probeTTL() time.Duration {
	if p.ProbeTTL <= 0 {
		return DefaultNativeMergeQueueProbeTTL
	}
	return p.ProbeTTL
}

func (p NativeMergeQueuePolicy) baseAllowed(repo, base string) bool {
	return p.repoAllowed(repo) && (p.Bases == nil || p.Bases[strings.ToLower(strings.TrimSpace(repo))+"#"+base])
}

// nativeQueueProbe is one cached isMergeQueueEnabled answer for a base.
type nativeQueueProbe struct {
	enabled bool
	at      time.Time
}

func (c *Engine) nativeQueuePolicy() NativeMergeQueuePolicy {
	if c == nil || c.nativeMergeQueuePolicy == nil {
		return NativeMergeQueuePolicy{}
	}
	return c.nativeMergeQueuePolicy()
}

// nativeQueueOptedIn reports whether repo (owner/repo) may use the native
// queue at all: the policy is on, the repo is allow-listed and the
// transport can enqueue (App auth).
func (c *Engine) nativeQueueOptedIn(repo string) bool {
	return c.nativeQueuePolicy().repoAllowed(repo) && c.nativeQueueTransport() != nil
}

// nativeQueueProbeKey identifies one base branch of one repo.
func nativeQueueProbeKey(repo string, pr *gh.PullRequest) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + pr.GetBase().GetRef()
}

// cachedNativeQueueProbe returns the base's cached answer when it is
// younger than the policy's probe TTL.
func (c *Engine) cachedNativeQueueProbe(key string) (bool, bool) {
	c.nativeQueueProbeMu.Lock()
	defer c.nativeQueueProbeMu.Unlock()
	probe, ok := c.nativeQueueProbes[key]
	if !ok || c.clock().Sub(probe.at) >= c.nativeQueuePolicy().probeTTL() {
		return false, false
	}
	return probe.enabled, true
}

func (c *Engine) recordNativeQueueProbe(key string, enabled bool) {
	c.nativeQueueProbeMu.Lock()
	defer c.nativeQueueProbeMu.Unlock()
	if c.nativeQueueProbes == nil {
		c.nativeQueueProbes = map[string]nativeQueueProbe{}
	}
	c.nativeQueueProbes[key] = nativeQueueProbe{enabled: enabled, at: c.clock()}
}

type nativeQueueTransport interface {
	NativeMergeQueueAvailable() bool
	ReadNativeMergeQueue(context.Context, string) (hgithub.NativeMergeQueueState, error)
	EnqueueNativePullRequest(context.Context, string, string) (*hgithub.NativeMergeQueueEntry, error)
	DequeueNativePullRequest(context.Context, string) error
	NativeMergeQueueMethod(context.Context, string, string) (string, error)
}

// botSlug normalises a bot login: REST reports "<slug>[bot]" while GraphQL's
// Bot.login omits the suffix.
func botSlug(login string) string {
	return strings.TrimSuffix(strings.TrimSpace(login), "[bot]")
}

// ownNativeQueueEntry matches the enqueuer by type as well as slug: a user
// account may hold a login equal to the App's slug.
func ownNativeQueueEntry(e *hgithub.NativeMergeQueueEntry, slug string) bool {
	return e != nil && e.Enqueuer.Typename == "Bot" && strings.EqualFold(botSlug(e.Enqueuer.Login), slug)
}

// nativeQueueWithdrawReason reports whether a lane's rejection reason means
// the PR lost merge eligibility (#15198): a hold, a withdrawn or stale
// approval, changes requested, a paused or disabled repo, a draft or closed
// PR, a release in progress, or a human-merge path. Anything else — pending or red checks, behind, a moved head, an
// API error, a routing observation — keeps the App's queue entry.
func nativeQueueWithdrawReason(reason string) bool {
	switch reason {
	case "held", "label:hold", "label:needs-human", "sentinel", contributorLaneReasonSentinelBlocked, "exempt-label",
		"label-removed", autoMergeReasonNoHiveQueueApproval, autoMergeReasonUntrustedQueueApproval,
		"queue-approval-missing-head", "queue-approval-head-changed", "self-merge-ban", autoMergeReasonUntrustedMerger,
		"changes-requested", autoMergeReasonHumanChangesRequested, contributorLaneReasonChangesRequested,
		nativeQueueReasonPaused, "repo-auto-merge-disabled", relayLaneReasonDisabled,
		"draft", "closed", releaseInProgressStatusContext, autoMergeReasonHumanMergePath:
		return true
	}
	return strings.HasPrefix(reason, "label:do-not-merge") || strings.HasPrefix(reason, "excluded-label:") ||
		strings.HasPrefix(reason, autoMergeReasonReviewGatePrefix+ReviewGateOutcomeChangesRequested)
}

// nativeQueueEnabled reports whether the PR's base has a native merge queue
// and repo (owner/repo) opted in. The isMergeQueueEnabled answer is cached
// per repo#base for the policy's probe TTL (default 30m) so a sweep over
// many PRs costs one GraphQL read per base, not one per PR. A read failure
// reports false (and is not cached) so the lane keeps its pre-queue
// behaviour.
func (c *Engine) nativeQueueEnabled(ctx context.Context, repo string, pr *gh.PullRequest) bool {
	t := c.nativeQueue(repo)
	if t == nil || pr.GetNodeID() == "" || !c.nativeQueuePolicy().baseAllowed(repo, pr.GetBase().GetRef()) {
		return false
	}
	key := nativeQueueProbeKey(repo, pr)
	if enabled, ok := c.cachedNativeQueueProbe(key); ok {
		return enabled
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err != nil {
		return false
	}
	c.recordNativeQueueProbe(key, state.IsMergeQueueEnabled)
	return state.IsMergeQueueEnabled
}

// nativeQueueSkipsCurrentBase reports whether a lane may bypass the
// current-base gate and the behind update-branch for pr: only when the
// operator set auto_merge.native_merge_queue.skip_current_base_gate AND the
// base has a queue. By default the gate stays in force on queue bases.
func (c *Engine) nativeQueueSkipsCurrentBase(ctx context.Context, repo string, pr *gh.PullRequest) bool {
	return c.nativeQueuePolicy().SkipCurrentBaseGate && c.nativeQueueEnabled(ctx, repo, pr)
}

// nativeQueueGatedState: a PR not yet merged through a required queue reads
// blocked (or behind) even when every gate the queue re-tests has passed.
func nativeQueueGatedState(pr *gh.PullRequest) bool {
	switch strings.ToLower(strings.TrimSpace(pr.GetMergeableState())) {
	case "blocked", "behind":
		return true
	}
	return false
}

// nativeQueueTransport returns the transport when it can enqueue (App auth),
// regardless of policy.
func (c *Engine) nativeQueueTransport() nativeQueueTransport {
	if c == nil {
		return nil
	}
	t, ok := c.transport.(nativeQueueTransport)
	if !ok || !t.NativeMergeQueueAvailable() {
		return nil
	}
	return t
}

// nativeQueue returns the queue transport for repo (owner/repo), or nil when
// the repo has not opted in or the transport cannot enqueue.
func (c *Engine) nativeQueue(repo string) nativeQueueTransport {
	if !c.nativeQueuePolicy().repoAllowed(repo) {
		return nil
	}
	return c.nativeQueueTransport()
}

// tryNativeQueue runs only after the lane's existing merge gates passed. A
// queued PR is not a merged PR: no merged audit, issue closure or merge event.
func (c *Engine) tryNativeQueue(ctx context.Context, pr *gh.PullRequest, claim effects.Claim) (string, error) {
	if capture, ok := ctx.Value(integrationEligibilityContextKey{}).(*integrationEligibilityCapture); ok {
		if pr.GetHead().GetSHA() != capture.head || pr.GetBase().GetRef() != capture.base ||
			pr.GetBase().GetRepo().GetFullName() != capture.repo {
			return "integration exact member identity changed", nil
		}
		capture.pr = pr
		return integrationEligibilityCaptured, nil
	}
	requiredNative := false
	if c.integrationExecution != nil {
		decision, err := c.integrationExecution(ctx, claim.Repo, pr, c.IntegrationEligibility)
		if err != nil {
			return decision.Reason, err
		}
		switch decision.Authority {
		case "hive", "hold":
			if decision.Reason == "" {
				decision.Reason = "integration execution owns destination; no direct fallback"
			}
			return decision.Reason, nil
		case "native":
			requiredNative = true
		case "":
		default:
			return "integration execution authority unknown; no fallback", nil
		}
	}
	direct := func() (string, error) {
		if requiredNative {
			return "integration native authority unavailable; no direct fallback", nil
		}
		return "", nil
	}
	t := c.nativeQueue(claim.Repo)
	if t == nil || pr.GetNodeID() == "" || !c.nativeQueuePolicy().baseAllowed(claim.Repo, pr.GetBase().GetRef()) {
		return direct()
	}
	key := nativeQueueProbeKey(claim.Repo, pr)
	if enabled, ok := c.cachedNativeQueueProbe(key); ok && !enabled {
		return direct()
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err != nil {
		return nativeQueueReasonRead, err
	}
	c.recordNativeQueueProbe(key, state.IsMergeQueueEnabled)
	if !state.IsMergeQueueEnabled && !state.IsInMergeQueue {
		return direct()
	}
	if state.HeadRefOid != claim.Inputs["expect_sha"] {
		return "head-changed-since-eval", nil
	}
	// A queue chooses its own merge method. Preserve the attribution gate
	// that elected a merge commit to retain each author's signed commits.
	if claim.Inputs["method_reason"] == mergeMethodReasonMixedAuthorship {
		if c.knownNativeQueueMethodRefusal(claim, state.HeadRefOid) {
			return nativeQueueReasonMethod, nil
		}
		method, err := t.NativeMergeQueueMethod(ctx, state.ID, pr.GetBase().GetRef())
		if err != nil {
			// A transient read failure is not a merge-method refusal; it
			// must not park the PR at stage/085.
			return nativeQueueReasonRead, err
		}
		if !strings.EqualFold(method, "MERGE") {
			c.noteNativeQueueMethodRefusal(claim, state.HeadRefOid)
			return nativeQueueReasonMethod, fmt.Errorf("mixed authorship requires a merge commit; queue uses %s", method)
		}
	}
	if !state.IsInMergeQueue {
		if c.integrationAdmission != nil {
			reason, err := c.integrationAdmission(ctx, claim.Repo, pr)
			if reason != "" || err != nil {
				return "integration-admission: " + reason, err
			}
		}
		// Each removal starts a new queue attempt, even at the same head.
		// The previous applied journal entry must never suppress re-enqueue.
		cycle := ""
		if nodes := state.TimelineItems.Nodes; len(nodes) > 0 {
			cycle = nodes[len(nodes)-1].ID
		}
		claim.Inputs = map[string]string{"operation": "enqueue", "expected_head_sha": state.HeadRefOid, "after_removal": cycle}
		observe := func(ctx context.Context) (effects.ExternalState, error) {
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.ExternalState{}, err
			}
			result := effects.ExternalState{Applied: current.IsInMergeQueue}
			if current.MergeQueueEntry != nil {
				result.Provenance = current.MergeQueueEntry.ID
			}
			return result, nil
		}
		_, err = effects.ExecuteReconciling(ctx, c.mutation, claim, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			entry, err := t.EnqueueNativePullRequest(ctx, state.ID, state.HeadRefOid)
			if err != nil {
				return effects.Result{}, err
			}
			state.MergeQueueEntry = entry
			return effects.Result{Provenance: entry.ID}, nil
		})
		if err != nil {
			return nativeQueueReasonEnqueue, err
		}
	}
	if e := state.MergeQueueEntry; e != nil {
		c.info("automerge queued", "repo", claim.Repo, "pr", pr.GetNumber(), "position", e.Position, "state", e.State)
		return fmt.Sprintf("%s: queued (position %d, %s)", nativeQueueReason, e.Position, e.State), nil
	}
	return nativeQueueReason, nil
}

// reconcileNativeQueueGate runs for the owning lane's rejected candidates,
// including cheap prefilter holds. Never withdraw another actor's entry.
func (c *Engine) reconcileNativeQueueGate(ctx context.Context, lane, owner, repo string, pr *gh.PullRequest, reason string) {
	t := c.nativeQueue(owner + "/" + repo)
	if t == nil || pr == nil || pr.GetNodeID() == "" || !nativeQueueWithdrawReason(reason) {
		return
	}
	// A base without a queue holds no entry to withdraw; reuse the probe
	// so held PRs on plain bases cost nothing.
	if enabled, ok := c.cachedNativeQueueProbe(nativeQueueProbeKey(owner+"/"+repo, pr)); ok && !enabled {
		return
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if _, own := c.sweepLaneForAuthor(author); own && lane != stageLaneSelf && lane != nativeQueueLaneInactive {
		return
	}
	if lane == stageLaneContributor && c.trustedAuthorDecision(author, c.currentTrustedAuthorPolicy().RequireRole).Allowed {
		return
	}
	login := botSlug(c.transport.AppBotLogin())
	if login == "" {
		return
	}
	state, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
	if err == nil && state.IsInMergeQueue && ownNativeQueueEntry(state.MergeQueueEntry, login) {
		entryID := state.MergeQueueEntry.ID
		claim := effects.Claim{Repo: owner + "/" + repo, Kind: effects.KindPullRequestMerge, Target: strconv.Itoa(pr.GetNumber()), Actor: "automerge", Inputs: map[string]string{"operation": "dequeue", "entry": entryID}}
		observe := func(ctx context.Context) (effects.ExternalState, error) {
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.ExternalState{}, err
			}
			return effects.ExternalState{Applied: !current.IsInMergeQueue || current.MergeQueueEntry == nil || current.MergeQueueEntry.ID != entryID, Provenance: entryID}, nil
		}
		_, err = effects.ExecuteReconciling(ctx, c.mutation, claim, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			// Recheck provenance immediately before the write; a person may have
			// removed and re-enqueued the PR since the lane evaluated it.
			current, err := t.ReadNativeMergeQueue(ctx, pr.GetNodeID())
			if err != nil {
				return effects.Result{}, err
			}
			if !current.IsInMergeQueue || current.MergeQueueEntry == nil || current.MergeQueueEntry.ID != entryID || !ownNativeQueueEntry(current.MergeQueueEntry, login) {
				return effects.Result{}, nil
			}
			if err := t.DequeueNativePullRequest(ctx, state.ID); err != nil {
				return effects.Result{}, err
			}
			return effects.Result{Provenance: entryID}, nil
		})
	}
	if err != nil {
		c.warn("native merge queue gate reconciliation failed", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "reason", reason, "error", err)
	}
}

// Pausing a repo stops new merges, but GitHub may still hold entries from a
// previous pass. Withdraw only this App's entries through the usual boundary.
func (c *Engine) reconcileInactiveNativeQueues(ctx context.Context) {
	if !c.nativeQueuePolicy().Enabled || c.nativeQueueTransport() == nil {
		return
	}
	active := map[string]bool{}
	for _, repo := range c.activeRepos() {
		active[repo] = true
	}
	for _, display := range c.transport.Repositories() {
		if (active[display] && c.repoAutoMergeAllowed(display)) || (c.repoOnGitHub != nil && !c.repoOnGitHub(display)) {
			continue
		}
		owner, repo := c.transport.SplitRepo(display)
		// Only an opted-in repo ever held an App entry; listing the others'
		// open PRs every 10 minutes would be pure API cost.
		if !c.nativeQueueOptedIn(owner + "/" + repo) {
			continue
		}
		if !c.inactiveQueueDue(display) {
			continue
		}
		opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
			if err != nil {
				c.warn("native merge queue pause reconciliation failed", "repo", display, "error", err)
				break
			}
			for _, pr := range prs {
				c.reconcileNativeQueueGate(ctx, nativeQueueLaneInactive, owner, repo, pr, nativeQueueReasonPaused)
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
}

func (c *Engine) inactiveQueueDue(repo string) bool {
	c.inactiveQueueMu.Lock()
	defer c.inactiveQueueMu.Unlock()
	now := c.now()
	if last, ok := c.inactiveQueueLast[repo]; ok && now.Sub(last) < inactiveQueueInterval {
		return false
	}
	if c.inactiveQueueLast == nil {
		c.inactiveQueueLast = map[string]time.Time{}
	}
	c.inactiveQueueLast[repo] = now
	return true
}
