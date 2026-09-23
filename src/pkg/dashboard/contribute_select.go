package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// rateLimitHourWindow and rateLimitDayWindow are the trailing (rolling) windows
// over which tier_limits.max_per_hour and max_per_day are counted (#2566). They
// are sliding windows anchored on "now", not calendar buckets: a contributor's
// assignment stops counting against the hourly cap exactly rateLimitHourWindow
// after it was made, and against the daily cap after rateLimitDayWindow. This
// matches the field names (per HOUR / per DAY) while avoiding a hard reset at the
// top of the clock hour/day that would let a burst straddle the boundary.
const (
	rateLimitHourWindow = time.Hour
	rateLimitDayWindow  = 24 * time.Hour
)

// taskUnavailableReason* are the machine-readable reasons carried on a
// task_unavailable negative-ack. They let the relay (and operators reading the
// log) tell "there is simply no admissible work right now" apart from "the hub
// refused to assign work to this contributor" for a specific enforced reason.
// See kubestellar/hive#2436.
const (
	// taskUnavailableTokenMintFailed: a scoped GitHub token could not be minted
	// for the contributor's tier (e.g. the installation lacks the permission the
	// tier requests), so no task_assign can be honestly issued. Previously this
	// path returned nil and the contributor waited forever with no explanation.
	taskUnavailableTokenMintFailed = "token_mint_failed"
	// taskUnavailableRepoUnmintable: every admissible candidate this pass lives in
	// a repository the GitHub App installation cannot mint a token for — renamed,
	// deleted, or removed from the installation (#7869). Distinct from
	// token_mint_failed so an operator reading the reason knows to fix the repo
	// list rather than the App credential.
	taskUnavailableRepoUnmintable = "repo_unmintable"
	// taskUnavailableLeasePersistFailed: the assignment's server-issued lease
	// could not be written to the lease registry on disk (#8287). The grant is
	// refused rather than handed out, because a lease that exists only in memory
	// is gone on the next restart — the contributor would resume against a hub
	// with no record of it. The item goes back to the queue untouched.
	taskUnavailableLeasePersistFailed = "lease_persist_failed"
	// taskUnavailableTierDisabled: the contributor's TrustTier is listed in
	// hub.disabled_tiers, so the operator has switched that tier off.
	taskUnavailableTierDisabled = "tier_disabled"
	// taskUnavailableConcurrencyLimit: assigning would exceed the tier's
	// tier_limits.max_concurrent for this identity (counting every live
	// connection this identity holds).
	taskUnavailableConcurrencyLimit = "concurrency_limit"
	// taskUnavailableHourlyLimit / taskUnavailableDailyLimit (#2566): assigning
	// would exceed the tier's tier_limits.max_per_hour / max_per_day for this
	// identity, counting the assignments handed out inside the trailing
	// rateLimitHourWindow / rateLimitDayWindow. These mirror the enforced-refusal
	// shape of taskUnavailableConcurrencyLimit (#2436) and the tier_disabled gate:
	// the fields were admin-writable and displayed by the #2562 control-plane but
	// previously left as TODO(#2436) and never enforced, so the displayed caps were
	// inert. A contributor at or over the cap now learns exactly which window it hit.
	taskUnavailableHourlyLimit = "hourly_limit"
	taskUnavailableDailyLimit  = "daily_limit"

	// The reasons below (kubestellar/hive#2546) extend the same task_unavailable
	// negative-ack to the three formerly-SILENT selectTask paths — each returned a
	// bare nil, so an idle contributor could not tell "operator suspended us" from
	// "hub is not ready yet" from "nothing matches right now". They are additive
	// and wire-compatible: same message type/shape as #2436, only new reason
	// strings. Unlike the #2436 reasons these are not enforced refusals — they mean
	// "no work to hand you right now, and here is why".
	//
	// taskUnavailableContributionSuspended: the operator has turned the whole
	// contribute queue off (hub.contribute_suspended). No contributor gets work
	// until it is re-enabled.
	taskUnavailableContributionSuspended = "contribution_suspended"
	// taskUnavailableHubNotReady: the hub has no status snapshot yet (it has not
	// finished its first enumeration, or has no server reference), so there is no
	// candidate set to select from. Transient at startup.
	taskUnavailableHubNotReady = "hub_not_ready"
	// taskUnavailableNoMatchingWork: the hub is running and unsuspended but, after
	// all filters (cooldown, disabled repos, allow/deny, skip-assigned, own-work),
	// the candidate set is empty. There is simply nothing admissible to do now.
	taskUnavailableNoMatchingWork = "no_matching_work"
	// taskUnavailableCapabilityMismatch: work exists, but every otherwise
	// admissible candidate had explicit task requirements contradicted by this
	// client's self-declared capabilities. Undeclared/unknown clients do not hit
	// this path; absence is not incapability.
	taskUnavailableCapabilityMismatch = "capability_mismatch"
	// taskUnavailableRoleNotPermitted: the relay requested a spoke agent role, but
	// the hive config/tier/grant policy does not allow this contributor to claim it.
	taskUnavailableRoleNotPermitted = "agent_role_not_permitted"
	// taskUnavailableFailureStreak (kubestellar/hive#6450): this identity's last
	// contributorFailureStreakThreshold assignments each failed within
	// contributorFastFailureMax of assignment — the signature of an agent runtime
	// dying at startup — so claims are paused for contributorFailureStreakPause.
	// Unlike no_matching_work this is about THIS contributor, not the queue, and
	// the message names the streak and the pause expiry. Not an enforced-policy
	// refusal like tier_disabled: it lifts on its own and is reset by any
	// completion or genuinely-attempted (slow) failure.
	taskUnavailableFailureStreak          = "contributor_failure_streak"
	taskUnavailableExternalDispatchFailed = "external_dispatch_failed"
)

// rateWindowCounts returns how many task assignments the given identity has been
// handed inside the trailing hour and day windows ending at `now`. It prunes any
// timestamps older than the day window (the widest of the two) as a side effect so
// assignmentTimes stays bounded to at most a day of history per identity. Caller
// must NOT hold rateMu; this method takes it. See assignmentTimes / #2566 for the
// rolling-window semantics.
func (h *ContributeWSHub) rateWindowCounts(identity string, now time.Time) (hour, day int) {
	if identity == "" {
		return 0, 0
	}
	dayCutoff := now.Add(-rateLimitDayWindow)
	hourCutoff := now.Add(-rateLimitHourWindow)

	h.rateMu.Lock()
	defer h.rateMu.Unlock()

	times := h.assignmentTimes[identity]
	kept := times[:0]
	for _, t := range times {
		if t.Before(dayCutoff) {
			// Older than the widest window — it can never count again; drop it.
			continue
		}
		kept = append(kept, t)
		day++
		if !t.Before(hourCutoff) {
			hour++
		}
	}
	if len(kept) == 0 {
		delete(h.assignmentTimes, identity)
	} else {
		h.assignmentTimes[identity] = kept
	}
	return hour, day
}

// recordAssignment appends an assignment timestamp for the identity. Called once
// per task_assign actually shipped, so the rate windows count tasks HANDED OUT
// (matching max_concurrent's semantics and the max_tasks_per_hour/day naming), not
// completions. Caller must NOT hold rateMu. See #2566.
func (h *ContributeWSHub) recordAssignment(identity string, at time.Time) {
	if identity == "" {
		return
	}
	h.rateMu.Lock()
	if h.assignmentTimes == nil {
		// The constructor initializes this map; a hub built as a bare struct literal
		// (some tests, defensive) would otherwise panic on append to a nil map.
		h.assignmentTimes = make(map[string][]time.Time)
	}
	h.assignmentTimes[identity] = append(h.assignmentTimes[identity], at)
	h.rateMu.Unlock()
}

// unrecordAssignment removes ONE assignment stamp `at` for the identity — the
// undo of recordAssignment, for a claim that was committed under selectMu and then
// could not be delivered (the token mint failed, or the task_assign send failed
// because the socket had already closed) (hivecommons/hive#7775). Without it a
// task the contributor never received would still consume one of its
// max_per_hour / max_per_day slots. Caller must NOT hold rateMu.
func (h *ContributeWSHub) unrecordAssignment(identity string, at time.Time) {
	if identity == "" || at.IsZero() {
		return
	}
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	times := h.assignmentTimes[identity]
	for i := len(times) - 1; i >= 0; i-- {
		if times[i].Equal(at) {
			times = append(times[:i], times[i+1:]...)
			break
		}
	}
	if len(times) == 0 {
		delete(h.assignmentTimes, identity)
	} else {
		h.assignmentTimes[identity] = times
	}
}

// rollbackAssignment undoes a claim selectTask committed to a connection whose
// task_assign never reached the relay (hivecommons/hive#7775): the scoped-token
// mint failed after the claim, or the send failed because the heartbeat loop had
// already closed the socket. It clears the connection's task, revokes the lease
// and frees the rate-window slot, so that nothing downstream — releaseOnDisconnect
// in particular — sees a task to release: no release cooldown is booked on an issue
// nobody worked, and no lease is left to expire. Guarded on taskID so a claim that
// was already released (or replaced) by another path is left alone. The generation
// is bumped as on every other release path, so the never-shipped generation can
// never be echoed back and accepted.
func (h *ContributeWSHub) rollbackAssignment(c *ContributorConnection, taskID string) {
	if c == nil || taskID == "" {
		return
	}
	c.mu.Lock()
	if c.currentTask == nil || c.currentTask.TaskID != taskID {
		c.mu.Unlock()
		return
	}
	assignedAt := c.taskAssignedAt
	c.currentTask = nil
	c.currentPrompt = ""
	c.currentLabels = nil
	c.pendingToken = ""
	c.credentialDelivered = false
	c.pendingExternalTask = nil
	c.tokenMintedAt = time.Time{}
	c.lastLeaseRenew = time.Time{}
	c.taskAssignedAt = time.Time{}
	c.currentTaskGen = h.nextTaskGen()
	c.mu.Unlock()
	h.revokeLease(identityOf(c), taskID)
	h.unrecordAssignment(identityOf(c), assignedAt)
}

// taskUnavailable builds the explicit negative-ack the ready handler sends in
// place of silence. It carries a machine-readable reason so the failure is
// diagnosable rather than an indefinite hang (kubestellar/hive#2436, finding 1).
func (h *ContributeWSHub) taskUnavailable(reason string) *WSMessage {
	return &WSMessage{
		Type:   "task_unavailable",
		Seq:    h.nextSeq(),
		Reason: reason,
	}
}

// canonicalRepoKey maps an arbitrary, possibly client-supplied repo string to the
// SAME canonical form the server keys everything on: FrontendRepo.Full, i.e. the
// exact string selectTask's activeIssues guard, the failure/quarantine cooldowns,
// and the completion cooldown all build their "repo#number" keys from.
//
// Why this is load-bearing (#2644): currentTask.Repo is normally set by selectTask
// to chosen.repoFull (== repo.Full). But the task_progress RESUME path re-populates
// currentTask from the CLIENT-supplied msg.Repo after a reconnect (the relay keeps
// its task locally and re-asserts it via task_progress). If the relay reports the
// repo in ANY other spelling than repo.Full — a bare name where the hub uses
// "owner/repo", or a differently-cased/prefixed cross-org name — then every
// server-side "%s#%d" key built from currentTask.Repo silently MISSES:
//   - the activeIssues double-assign guard no longer excludes the in-flight issue,
//     so a concurrent selectTask hands the SAME issue to a second contributor; and
//   - the disconnect/abandon reconnect-window cooldown (recordTaskFailure) is booked
//     under the wrong key, so it does not protect that issue either.
//
// This is exactly the #2356/#2492 duplicate-assignment race re-opened through a key
// mismatch, and it is intermittent + repo-specific: it only fires for a repo whose
// relay-reported name differs from the hub's repo.Full, and only across a reconnect
// that resumes via task_progress — which is why #2644 was seen "only in this repo".
//
// Resolution order:
//  1. Exact match on a known repo's Full (already canonical) → return it unchanged.
//  2. Match on a known repo's Name (case-insensitive) or its Full (case-insensitive)
//     → return that repo's Full, adopting the canonical casing/prefix.
//  3. No status/no match: fall back to the SAME rule buildRepos uses — prefix the
//     configured Org when the string carries no "owner/" segment — so a bare name
//     still lands on "org/name". If even the org is unknown, return the raw string
//     (unchanged behaviour of last resort; never worse than before).
func (h *ContributeWSHub) canonicalRepoKey(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return repo
	}

	var status *StatusPayload
	if h != nil && h.server != nil {
		h.server.statusMu.RLock()
		status = h.server.status
		h.server.statusMu.RUnlock()
	}
	if status != nil {
		// First pass: an exact Full match is already canonical — cheap and common.
		for _, r := range status.Repos {
			if r.Full == repo {
				return r.Full
			}
		}
		// Second pass: reconcile a differently-spelled client value against the
		// known set by Name or case-insensitive Full, adopting the canonical Full.
		for _, r := range status.Repos {
			if strings.EqualFold(r.Name, repo) || strings.EqualFold(r.Full, repo) {
				return r.Full
			}
		}
	}

	// Fallback mirrors buildRepos: a bare name is qualified with the configured org
	// so it matches the "org/name" Full the rest of the server builds.
	if !strings.Contains(repo, "/") &&
		h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		if org := h.server.deps.Config.Project.Org; org != "" {
			return org + "/" + repo
		}
	}
	return repo
}

// resumeGateReason re-runs, for a RESUME (hivecommons/hive C4), the same admission
// gates a fresh selectTask assignment must clear that are NOT specific to a
// particular candidate issue: the profile must not be revoked, the whole contribute
// queue must not be suspended, and the contributor's trust tier must not be
// operator-disabled. It returns a non-empty machine-readable reason when resume must
// be REFUSED (so the caller can revoke the lease and tell the relay why), or "" when
// resume may proceed. It deliberately does NOT re-run the per-candidate cooldown /
// repo-filter / concurrency-window gates: the lease is for a task the hub already
// committed to this identity, so re-applying candidate selection would falsely refuse
// a legitimately in-flight task. The gates checked here are the ones that represent an
// operator turning access OFF after assignment.
func (h *ContributeWSHub) resumeGateReason(c *ContributorConnection) string {
	tier := ""
	if c != nil && c.profile != nil {
		if c.profile.TrustTier == "revoked" {
			return "contribution access revoked"
		}
		tier = c.profile.TrustTier
	}
	if h.server == nil {
		return taskUnavailableHubNotReady
	}
	if h.server.deps != nil && h.server.deps.Config != nil {
		if h.server.deps.Config.Hub.ContributeSuspended {
			return taskUnavailableContributionSuspended
		}
		for _, dt := range h.server.deps.Config.Hub.DisabledTiers {
			if dt == tier {
				return taskUnavailableTierDisabled
			}
		}
	}
	return ""
}

// mintFailureRepoCooldown is how long a repository stays excluded from selection
// after the App refused to mint a token scoped to it (#7869). A renamed repo
// stays broken until the operator fixes the repo list, so the block is long
// enough that the fleet is not re-discovering it on every 30 s retry; it is short
// enough that the fix takes effect without a restart.
const mintFailureRepoCooldown = 10 * time.Minute

// maxUnmintableRepoRetries bounds how many repo-scoped mint failures one
// selectTask call will absorb by moving to the next candidate before it gives up
// and reports the pass as unavailable. Each retry costs one GitHub round-trip.
const maxUnmintableRepoRetries = 3

// markRepoUnmintable records that a token scoped to repo could not be minted, so
// selection skips the repo's items until the cooldown lapses (#7869).
func (h *ContributeWSHub) markRepoUnmintable(repo string, now time.Time) {
	h.unmintableMu.Lock()
	defer h.unmintableMu.Unlock()
	if h.unmintableRepos == nil {
		h.unmintableRepos = make(map[string]time.Time)
	}
	h.unmintableRepos[repo] = now.Add(mintFailureRepoCooldown)
}

// repoUnmintable reports whether repo is inside its post-mint-failure cooldown,
// dropping the record once it has lapsed so a fixed repo is re-offered.
func (h *ContributeWSHub) repoUnmintable(repo string, now time.Time) bool {
	h.unmintableMu.Lock()
	defer h.unmintableMu.Unlock()
	until, ok := h.unmintableRepos[repo]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(h.unmintableRepos, repo)
		return false
	}
	return true
}

func (h *ContributeWSHub) selectTask(c *ContributorConnection) *WSMessage {
	// #7869: one selection may drop a candidate whose repository the App cannot
	// mint a token for and scan again; skippedUnmintable counts those drops so the
	// re-scan is bounded and an empty re-scan can name the real cause.
	skippedUnmintable := 0
	for {
		msg := h.selectTaskPass(c, &skippedUnmintable)
		if msg == nil && skippedUnmintable > 0 && skippedUnmintable <= maxUnmintableRepoRetries {
			continue
		}
		if msg != nil && skippedUnmintable > 0 && msg.Type == "task_unavailable" &&
			(msg.Reason == taskUnavailableNoMatchingWork || msg.Reason == taskUnavailableTokenMintFailed) {
			// Nothing else admissible after excluding the repo(s): say so, so the
			// operator fixes the repo list rather than the App credential.
			msg.Reason = taskUnavailableRepoUnmintable
		}
		return msg
	}
}

// selectTaskPass is one selection scan. It returns nil ONLY when it dropped a
// candidate for a repo-scoped mint failure and the caller should scan again
// (#7869); every other outcome is a message.
func (h *ContributeWSHub) selectTaskPass(c *ContributorConnection, skippedUnmintable *int) *WSMessage {
	// selectMu makes offer→claim atomic: scan the candidates, pick one, commit
	// it to the connection, so two contributors are never handed the same item.
	// It guards IN-MEMORY state only. The GitHub round-trips that decorate a
	// claim — the scoped-token mint and the push-permission lookup — run AFTER
	// the unlock below (hivecommons/hive#7775). They used to run inside this
	// section, which serialized every contributor's `ready` behind one
	// contributor's GitHub latency; and because handleReady runs on the
	// connection's read goroutine, a contributor queued behind a few slow
	// selections stopped reading the hub's pongs, was hung up on by the
	// heartbeat loop, and then had an assignment committed to its dead socket.
	// The claim itself (currentTask, the lease, the rate-window slot) IS the
	// reservation other selections see, so it can safely be decorated unlocked
	// and rolled back if the decoration fails.
	h.selectMu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			h.selectMu.Unlock()
		}
	}
	defer unlock()

	if h.server == nil {
		// No server reference — the hub cannot read status or config, so there is
		// nothing to select from. Jorge flagged this path as arguably not
		// contributor-visible; it is folded into hub_not_ready since it is the same
		// "the hub cannot serve work yet" condition and giving it a reason is
		// trivial and harmless (#2546).
		return h.taskUnavailable(taskUnavailableHubNotReady)
	}

	requestedRole := ""
	if c != nil {
		requestedRole = normalizeAgentRole(c.role)
	}
	if requestedRole != "" {
		if ok, reason := h.roleClaimAllowed(c, requestedRole); !ok {
			h.logger.Warn("[contribute-ws] refusing task: agent role not permitted",
				"username", identityOf(c), "role", requestedRole, "reason", reason)
			h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
				"agent role "+requestedRole+" not permitted: "+reason)
			return h.taskUnavailable(taskUnavailableRoleNotPermitted)
		}
	}
	if h.server.deps != nil && h.server.deps.Config != nil && h.server.deps.Config.Hub.ContributeSuspended {
		// #2546: the operator suspended the whole contribute queue. Previously this
		// returned a bare nil and the contributor waited in silence, unable to tell
		// "suspended" from "misconfigured" from "wedged". Send an explicit
		// contribution_suspended negative-ack so the idle state is legible.
		return h.taskUnavailable(taskUnavailableContributionSuspended)
	}

	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()

	if status == nil {
		// #2546: no status snapshot yet (hub still warming up). Same wire-shape as
		// above, distinct reason, so the contributor learns it is a transient
		// not-ready state rather than a permanent refusal.
		return h.taskUnavailable(taskUnavailableHubNotReady)
	}

	// #2436 finding 2: refuse to assign work to a contributor whose TrustTier is
	// switched off via hub.disabled_tiers. This control was declared and
	// admin-writable but never read, so an operator "disabling" a tier changed
	// nothing. Send an explicit tier_disabled negative-ack rather than silently
	// handing out work anyway.
	tier := ""
	if c.profile != nil {
		tier = c.profile.TrustTier
	}
	if h.server.deps != nil && h.server.deps.Config != nil {
		for _, dt := range h.server.deps.Config.Hub.DisabledTiers {
			if dt == tier {
				h.logger.Warn("[contribute-ws] refusing task: tier disabled",
					"username", identityOf(c), "tier", tier)
				h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
					"tier "+tier+" is disabled on this hive")
				return h.taskUnavailable(taskUnavailableTierDisabled)
			}
		}
	}

	// activeIssues tracks issues already held by SOME live connection so we do
	// not double-assign the same work item. identityHolds counts, per identity,
	// how many tasks that identity currently holds across ALL of its live
	// connections — the source of truth for the #2436 finding 3 concurrency gate
	// (one identity opening several connections must not exceed MaxConcurrent).
	activeIssues := make(map[string]bool)
	identityHolds := make(map[string]int)
	h.mu.RLock()
	for _, conn := range h.connections {
		conn.mu.Lock()
		if conn.currentTask != nil {
			if key := conn.currentTask.identityKey(); key != "" {
				activeIssues[key] = true
			}
			identityHolds[identityOf(conn)]++
		}
		conn.mu.Unlock()
	}
	h.mu.RUnlock()

	// Also exclude every item some OTHER identity still holds an unexpired lease
	// on (#7773). The live-connection scan above sees only relays that are
	// connected right now; a relay whose socket dropped keeps its lease so it can
	// resume (#4260), and between #2356's ten-minute release hedge lapsing and the
	// thirty-minute lease expiring the item was offerable here while still
	// resumable there — a second contributor was handed it, the first came back
	// and resumed it, and both opened PRs. A lease that can still be resumed is a
	// hold for exactly as long as it can be resumed. (#5681 applied this to
	// RESTORED leases during a post-restart grace; it is now the rule for all.)
	for key := range h.leasedIssueKeys(identityOf(c), time.Now()) {
		activeIssues[key] = true
	}
	// #8380: an item some OTHER holder has a live worker claim on — a human
	// session, a hub agent, another contributor, or an external author — is
	// not offerable either. The claim is what lets a person say "mine" before
	// any PR exists; the relay must honour it exactly like a lease.
	for key := range h.claimedIssueKeys(identityOf(c)) {
		activeIssues[key] = true
	}

	// #2436 finding 3 / #2566: enforce tier_limits per identity. The config ships
	// populated MaxConcurrent/MaxPerHour/MaxPerDay defaults, so an operator
	// reasonably believes concurrency AND rate are capped — and since #2562 the
	// Management & Operations control-plane DISPLAYS all three as if authoritative.
	// Before this, only MaxConcurrent was enforced; MaxPerHour / MaxPerDay were left
	// as TODO(#2436) and never read, so the hourly/daily numbers an operator set (or
	// saw in the control-plane) were inert. All three are now enforced here.
	//
	// A limit <= 0 is treated as "unlimited" for every field (the "advisor" default
	// is 0 across the board, and existing configs that never set a field must keep
	// working). MaxConcurrent counts tasks currently HELD (identityHolds, from the
	// live-connection scan above); MaxPerHour / MaxPerDay count tasks ASSIGNED inside
	// the trailing rateLimitHourWindow / rateLimitDayWindow (rolling windows, see
	// assignmentTimes). Concurrency is checked first as the tightest, most immediate
	// gate; the rate windows are checked next so a contributor learns exactly which
	// cap they hit. The daily assignment is recorded only once a task is actually
	// shipped, at the task_assign site below.
	if h.server.deps != nil && h.server.deps.Config != nil {
		if limits, ok := h.server.deps.Config.Hub.TierLimits[tier]; ok {
			if limits.MaxConcurrent > 0 && identityHolds[identityOf(c)] >= limits.MaxConcurrent {
				h.logger.Warn("[contribute-ws] refusing task: concurrency limit reached",
					"username", identityOf(c), "tier", tier,
					"held", identityHolds[identityOf(c)], "max_concurrent", limits.MaxConcurrent)
				h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
					"concurrency limit for tier "+tier+": holding "+
						strconv.Itoa(identityHolds[identityOf(c)])+" of "+
						strconv.Itoa(limits.MaxConcurrent))
				return h.taskUnavailable(taskUnavailableConcurrencyLimit)
			}
			if limits.MaxPerHour > 0 || limits.MaxPerDay > 0 {
				hourCount, dayCount := h.rateWindowCounts(identityOf(c), time.Now())
				if limits.MaxPerHour > 0 && hourCount >= limits.MaxPerHour {
					h.logger.Warn("[contribute-ws] refusing task: hourly rate limit reached",
						"username", identityOf(c), "tier", tier,
						"assigned_last_hour", hourCount, "max_per_hour", limits.MaxPerHour)
					h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
						"hourly rate limit for tier "+tier+": "+strconv.Itoa(hourCount)+
							" assigned in the last hour, max "+strconv.Itoa(limits.MaxPerHour))
					return h.taskUnavailable(taskUnavailableHourlyLimit)
				}
				if limits.MaxPerDay > 0 && dayCount >= limits.MaxPerDay {
					h.logger.Warn("[contribute-ws] refusing task: daily rate limit reached",
						"username", identityOf(c), "tier", tier,
						"assigned_last_day", dayCount, "max_per_day", limits.MaxPerDay)
					h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
						"daily rate limit for tier "+tier+": "+strconv.Itoa(dayCount)+
							" assigned in the last day, max "+strconv.Itoa(limits.MaxPerDay))
					return h.taskUnavailable(taskUnavailableDailyLimit)
				}
			}
		}
	}

	// #6450: pause claims for an identity whose recent assignments all died
	// within seconds — a dying agent runtime. Checked after the operator gates
	// (which are policy and must win the log/refusal narrative) and before the
	// candidate scan (so a broken runtime cannot book another issue's failure
	// cooldown). Hub-measured only; see contribute_failure_streak.go.
	if paused, streak, until := h.contributorFailureStreakActive(identityOf(c), time.Now()); paused {
		h.logger.Warn("[contribute-ws] refusing task: contributor failure streak",
			"username", identityOf(c),
			"consecutive_fast_failures", streak.Count,
			"consecutive_stall_failures", streak.StallCount,
			"paused_until", until.UTC().Format(time.RFC3339))
		h.recordDecision(decisionUsername(c), decisionRefused, "", "", 0,
			"contributor failure streak: "+strconv.Itoa(streak.Count)+
				" consecutive fast failures, "+strconv.Itoa(streak.StallCount)+
				" consecutive stall failures, paused until "+until.UTC().Format(time.RFC3339))
		msg := h.taskUnavailable(taskUnavailableFailureStreak)
		msg.Message = contributorFailureStreakMessage(streak, until)
		return msg
	}

	totalAvailable := 0
	for _, repo := range status.Repos {
		totalAvailable += len(repo.ActionableIssues)
	}
	h.logger.Info("[contribute-ws] selectTask scanning", "repos", len(status.Repos), "totalIssues", totalAvailable, "cooldown", len(h.completedTasks), "active", len(activeIssues))

	var disabledRepos []string
	var heldIssues map[string]struct{}
	if h.server.deps != nil && h.server.deps.Config != nil {
		disabledRepos = h.server.deps.Config.Hub.DisabledRepos
		// Operator HOLD (#queue-hold): a manually-parked issue must never be offered,
		// indefinitely, until the operator Resumes it. Built once per selectTask from
		// the same canonical "%s#%d" keys the cooldown/active/failure checks use, so
		// the exclusion cannot miss on a repo-name spelling mismatch (#2648).
		heldIssues = queueHoldSet(h.server.deps.Config.Hub.ContributeQueueHold)
	}

	// --- Collect the eligible candidates, then order them (#2390) ---------------
	//
	// #2390 (castrojo): "a great default would be starting with MY PRs that are
	// blocking someone else or have been reviewed and are waiting for something."
	// i.e. before we hand a connected contributor a brand-new issue, prefer a work
	// item that is already THEIRS and needs their attention.
	//
	// Priority order applied below:
	//   1. The contributor's OWN work (candidate author == c.profile.GitHubUsername)
	//   2. Everything else — today's plain first-eligible ordering
	//
	// Honest scope note on the available signal:
	//   selectTask only iterates ActionableIssues (issues), and the per-candidate
	//   map carries `author` but NO review state. The richer signal #2390 really
	//   wants — "my PR has been reviewed / is approved / requested changes / is
	//   blocking someone else" — is not collected anywhere yet:
	//     * bin/enumerate-actionable.sh enumerates PRs with only
	//       title/labels/author/draft/url (no reviewDecision, no requested-changes,
	//       no "blocking"), and those PRs land in FrontendRepo.OpenPrs, which this
	//       selector does not read at all.
	//     * github.PullRequest has no review-decision field.
	//   So this change implements ONLY the ordering half of #2390, keyed on the one
	//   own-work signal that actually exists today (issue authorship). When the
	//   contributor has no own-authored candidate, we fall back to today's exact
	//   ordering — behaviour is unchanged in that (common) case.
	//
	//   TODO(#2390, depends on read-only-gh #2393-#2396): thread review state into
	//   the candidate data — extend enumerate-actionable.sh to collect the
	//   contributor's own open PRs together with their reviewDecision
	//   (APPROVED / CHANGES_REQUESTED / REVIEW_REQUIRED) and any "blocking" signal,
	//   surface those as candidates here, and refine ownWorkPriority to rank a
	//   reviewed/blocking own-PR ahead of a merely-own issue. Guard gracefully when
	//   that data is absent, exactly as the ownWork fallback does now.
	ownUsername := ""
	var ownInterests []string
	if c.profile != nil {
		ownUsername = c.profile.GitHubUsername
		ownInterests = c.profile.LabelInterests
	}
	// interestMatchesLabels reports whether any of the issue's labels matches one of
	// the contributor's opt-in label interests (#2637), case-insensitively. Empty
	// interests → never a match (the affinity tier is then a no-op). This is the
	// SOFT routing signal: a contributor with interests set is offered matching work
	// first (see the sort below), but still receives non-matching work when none
	// matches, so a willing contributor never sits idle.
	interestMatchesLabels := func(labels []string) bool {
		if len(ownInterests) == 0 || len(labels) == 0 {
			return false
		}
		for _, want := range ownInterests {
			for _, have := range labels {
				if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(have)) {
					return true
				}
			}
		}
		return false
	}
	declaredCaps, declaredBackend := capabilityRoutingInputs(c)

	type candidate struct {
		repoFull string
		number   int
		// ref is the candidate's canonical, source-aware identity
		// (kubestellar/hive#4245). Every exclusion below keys on ref.Key(), so
		// two zero-numbered external items are distinct work rather than one
		// shared "repo#0".
		ref    worksource.Ref
		title  string
		url    string
		stage  string
		labels []string
		lane   string
		isOwn  bool
		// interestMatch is true when the issue carries a label the contributor has
		// opted into (#2637). It is a SOFT priority tier below own-work: matching
		// work is offered first, but a contributor with no match still gets other
		// work. Off entirely when the contributor set no interests.
		interestMatch bool
		requirements  ContributorTaskRequirements
		// recentFailures is the issue's current consecutive-failure count (#2435).
		// It is a stable tie-break in the ordering below: among equally-admissible
		// candidates, fewer-recent-failures first. Issues in an active failure
		// cooldown are already excluded above, so this only deprioritises an issue
		// whose short cooldown just elapsed but which still has failure history —
		// a backstop that keeps the queue moving even if the ledger is imperfect.
		recentFailures     int
		extEngine          string
		extMode            string
		extCapability      string
		extWorkflowVersion string
	}
	var candidates []candidate
	capabilityMismatchSeen := false

	// One ledger snapshot for this whole selection pass (#3845), shared with the
	// contributor-neutral admission gate below so ReadyQueue and selectTask judge
	// dependencies against the same contract. Discarded when the pass ends, so
	// every selection re-observes current state rather than trusting a cache.
	admissionSweep := h.newAdmissionSweep()

	for _, repo := range status.Repos {
		if len(repo.ActionableIssues) == 0 {
			continue
		}
		if config.MatchesAny(repo.Full, disabledRepos) || config.MatchesAny(repo.Name, disabledRepos) {
			continue
		}
		if h.repoUnmintable(repo.Full, time.Now()) {
			// #7869: the App could not mint a token scoped to this repo moments ago
			// (renamed / deleted / dropped from the installation). Offering its items
			// would fail the same way; skip the whole repo until the cooldown lapses.
			h.logger.Info("[contribute-ws] skip: repo excluded after a repo-scoped token mint failure",
				"repo", repo.Full, "items", len(repo.ActionableIssues))
			continue
		}
		forEachActionableIssue(h.logger, "[contribute-ws]", repo.Full, repo.ActionableIssues, func(issue map[string]any, ref worksource.Ref) {
			itemKey := ref.Key()
			number := ref.Number
			// Operator HOLD (#queue-hold): skip a manually-parked issue outright. This
			// is a persistent operator decision, DISTINCT from the time-based cooldown
			// below — a held issue never becomes a candidate until the operator Resumes
			// it. Keyed on the same canonical "%s#%d" as every other exclusion.
			if _, isHeld := heldIssues[itemKey]; isHeld {
				return
			}
			if h.isTaskInCooldownKey(itemKey) {
				return
			}
			// #3987: skip an issue with a live no_work_needed completion verdict
			// (the #2547 shape — shippable parts merged, remainder maintainer-
			// gated, no open PR left for the claim ledger to see). The verdict
			// is voided the moment the issue shows activity newer than it, so a
			// genuinely reopened issue comes straight back into the pool.
			if h.isSuppressedByNoWorkVerdictKey(itemKey, issueUpdatedAtFromMap(issue)) {
				return
			}
			// #2435: skip an issue still inside its short post-failure cooldown or
			// its longer quarantine. This is the primary livelock fix — a failing
			// issue at the head of the scan is no longer instantly re-admissible.
			if h.isTaskInFailureCooldownKey(itemKey) {
				return
			}
			if activeIssues[itemKey] {
				return
			}
			labels := stringSliceFromAny(issue["labels"])
			stage := runStageFromIssueMap(issue)
			if stage != "" && !relaySupportsRunStage(c) {
				capabilityMismatchSeen = true
				h.logger.Info("[contribute-ws] skip: run stage requires a relay capability",
					"repo", repo.Full, "number", number, "stage", stage, "required_capability", capRunStage)
				return
			}
			// #8361: an item bound to an external engine is refused, never
			// downgraded to local work, unless the binding is enabled and the
			// relay declared the engine capability.
			extEngine := extExecEngineFromIssueMap(issue)
			extMode := ""
			extCapability := ""
			extWorkflowVersion := ""
			if extEngine != "" {
				if ok, reason := h.extExecAdmissible(extEngine, c); !ok {
					capabilityMismatchSeen = true
					h.logger.Info("[contribute-ws] skip: external execution item refused",
						"repo", repo.Full, "number", number, "engine", extEngine, "reason", reason)
					return
				}
				var cfg *config.Config
				if h.server != nil && h.server.deps != nil {
					cfg = h.server.deps.Config
				}
				extMode = extExecMode(extEngine, cfg)
				extCapability, _, _ = extExecGate(extEngine, cfg)
				extWorkflowVersion = extExecWorkflowVersion(extEngine, cfg)
			}
			requirements := TaskRequirementsFromLabels(labels)
			if !ContributorCanRunTask(declaredCaps, declaredBackend, requirements) {
				capabilityMismatchSeen = true
				h.logger.Info("[contribute-ws] skip: issue requirements do not fit declared client capabilities",
					"repo", repo.Full, "number", number,
					"required_container_runtime", requirements.ContainerRuntime,
					"required_os", requirements.OS,
					"required_arch", requirements.Arch,
					"required_backend", requirements.CLIBackend,
					"required_credential", requirements.CredentialType)
				return
			}
			// #3768: skip an issue that an open PR — from ANYONE, hive agent or
			// human contributor — already claims to fix. The activeIssues guard
			// above only covers tasks held by LIVE connections, and the
			// completion cooldown only starts when the relay reports a verified
			// task_complete; a contributor who opens a PR but whose completion
			// report is missed (relay scrape failure, disconnect, verification
			// downgrade) left the issue re-offerable after the short 4h no-PR
			// cooldown, so projectbluefin/dakota#353 accumulated one duplicate
			// PR per window. The claim ledger sees the open PR itself on the
			// next eval cycle, which closes that hole regardless of what the
			// relay managed to report.
			issueClaim, claimed := h.claimFromIssueMap(issue, time.Now())
			decision := h.evaluateContributorNeutralAdmission(admissionSweep, contributorAdmissionCandidate{
				repoFull:   repo.Full,
				repoName:   repo.Name,
				number:     number,
				ref:        ref,
				labels:     labels,
				dependsOn:  dependenciesFromIssueMap(issue),
				issueClaim: issueClaim,
				claimed:    claimed,
			})
			if !decision.admitted {
				switch decision.reason {
				case contributorAdmissionReasonOpenPRClaim:
					h.logger.Info("[contribute-ws] skip: issue already claimed by a PR",
						"repo", repo.Full, "number", number,
						"pr_url", decision.claim.PRURL, "pr_author", decision.claim.PRAuthor,
						"merged", decision.claim.MergedPR,
						"source", decision.claim.Source, "source_reporter", decision.claim.SourceReporter)
				case contributorAdmissionReasonIssueClaim:
					// #8380: someone claimed the issue on the issue itself; the
					// claim expires on its own, so the log names when.
					h.logger.Info("[contribute-ws] skip: issue claimed",
						"repo", repo.Full, "number", number,
						"claimed_by", decision.issueClaim.Identity,
						"claim_expires_at", decision.issueClaim.ExpiresAt.UTC().Format(time.RFC3339),
						"claim_source", decision.issueClaim.Source)
				case contributorAdmissionReasonMergedClaimStale:
					// #8003: the fix landed days ago and the issue is still
					// open. Logged as the question it is, so the Operations
					// feed reads "close it" rather than "claimed by a PR".
					h.logger.Info("[contribute-ws] skip: merged claim is stale — issue needs a maintainer to close it",
						"repo", repo.Full, "number", number,
						"pr_url", decision.claim.PRURL, "pr_author", decision.claim.PRAuthor,
						"settled_at", decision.claim.SettledAt(),
						"source", decision.claim.Source, "source_reporter", decision.claim.SourceReporter)
				case contributorAdmissionReasonIssueChurn:
					// #7995: not "somebody is on it" but "this issue has
					// already eaten several PRs and nobody can say what is
					// left". Logged with the counts so the reason is legible
					// without opening the Withheld panel.
					h.logger.Info("[contribute-ws] skip: issue churn needs maintainer triage",
						"repo", repo.Full, "number", number,
						"merged_prs", len(decision.churn.Merged),
						"closed_prs", len(decision.churn.ClosedUnmerged),
						"prs", strings.Join(decision.churn.PRNumbers(), ","))
				case contributorAdmissionReasonWorkflowBlocked:
					h.logger.Info("[contribute-ws] skip: issue is blocked by workflow state",
						"repo", repo.Full, "number", number, "label", decision.skippedLabel)
				case contributorAdmissionReasonLabelSkipped:
					h.logger.Info("[contribute-ws] skip: issue has contribute skip label",
						"repo", repo.Full, "number", number, "label", decision.skippedLabel)
				// #3845: a declared dependency is not satisfied (or cannot be
				// resolved), so this issue is not admissible work yet. Only THIS
				// candidate is withheld — the scan continues and unrelated ready
				// work is still offered.
				case contributorAdmissionReasonDependencyBlocked, contributorAdmissionReasonDependencyUnknown:
					h.logger.Info("[contribute-ws] skip: dependency not satisfied",
						"repo", repo.Full, "number", number,
						"reason", decision.convergence.Reason,
						"bead", decision.convergence.ObservedRecord,
						"generation", decision.convergence.ObservedGeneration,
						"blockers", strings.Join(decision.convergence.Blockers, ","))
				}
				return
			}
			// Yank self-exclusion: an issue this SAME clanker was just yanked off is
			// briefly skipped for it (yankSelfExcludeSeconds), so the immediate post-yank
			// reassignment moves the clanker to genuinely different work instead of re-
			// grabbing the same item. Scoped to (this clanker, this issue): every OTHER
			// contributor may still be offered the issue immediately. Guarded by h.mu.
			if c.profile != nil {
				h.mu.Lock()
				selfExcluded := h.isYankSelfExcludedKeyLocked(c.profile.ContributorID, itemKey)
				h.mu.Unlock()
				if selfExcluded {
					return
				}
			}

			title, _ := issue["title"].(string)
			url, _ := issue["url"].(string)
			author, _ := issue["author"].(string)
			lane, _ := issue["lane"].(string)
			assignees := stringSliceFromAny(issue["assignees"])

			// A tracker/umbrella issue is coordination-only: its children carry
			// the work and are queued independently, so handing the parent to one
			// contributor is never right. The enumerator has flagged these all
			// along (github.Issue.IsTracker) and this queue simply never read it —
			// scheduler.go consumed the flag for a "[TRACKER]" prompt annotation
			// and nothing else.
			//
			// Live cost of the omission: kubestellar/hive#4188 ("coordination-only
			// umbrella. Do not assign #4188 as one Hive task", per its own body,
			// above a 13-item child task list) was offered to a contributor, which
			// spent the full 30-minute MAX_TASK_DURATION budget and booked a
			// failure on an issue that only final integration can close. The agent
			// behaved correctly throughout — it triaged the children and shipped
			// one — but no agent can "complete" a parent like this.
			//
			// Logged rather than skipped silently: an issue that is never offered
			// should not be a mystery to the operator watching the queue.
			if isTracker, _ := issue["is_tracker"].(bool); isTracker {
				h.logger.Info("[contribute-ws] skipping tracker/umbrella issue",
					"repo", repo.Full, "number", number, "title", title)
				return
			}
			if requestedRole != "" && !h.issueMatchesAgentRole(requestedRole, title, labels, lane) {
				return
			}

			// Apply the title / author / label contribute filters: hive-wide
			// first, then any full-repo override.
			if h.server.deps != nil && h.server.deps.Config != nil {
				hub := h.server.deps.Config.Hub
				if !hub.EvaluateContributeFilters(repo.Full, title, author, labels).Admitted() {
					return
				}
				// #2357: optionally skip issues already assigned to someone else.
				// An issue assigned to the contributor themselves (or unassigned)
				// stays eligible; only issues assigned solely to OTHER users are
				// skipped when the toggle is on.
				if hub.ContributeSkipAssignedToOthers &&
					assignedToOthers(assignees, c.profile.GitHubUsername) {
					return
				}
			}

			// #2390: instead of assigning the first eligible issue inline, collect
			// it as a candidate. The own-work partition below reorders the whole
			// eligible set before we pick and assign one. The sibling filters
			// (#2357 skip-assigned, contribute allow/deny) have already run above,
			// so every appended candidate is genuinely assignable.
			candidates = append(candidates, candidate{
				repoFull: repo.Full,
				number:   number,
				ref:      ref,
				title:    title,
				url:      url,
				stage:    stage,
				// The issue's own labels travel with the candidate so the chosen
				// task_assign can populate the Labels envelope field (kubestellar/
				// hive#2393 item 8). They're already computed for filtering above.
				labels: labels,
				lane:   lane,
				// "Own work" is the only #2390 signal available today: the
				// candidate was authored by the connected contributor. When the
				// username is unknown (empty), nothing is own → we keep today's
				// ordering untouched.
				isOwn:         ownUsername != "" && strings.EqualFold(author, ownUsername),
				interestMatch: interestMatchesLabels(labels),
				requirements:  requirements,
				// #2435: carry any lingering failure history so the ordering below
				// can deprioritise a recently-failed issue within its bucket.
				recentFailures:     h.recentFailureCountKey(itemKey),
				extEngine:          extEngine,
				extMode:            extMode,
				extCapability:      extCapability,
				extWorkflowVersion: extWorkflowVersion,
			})
		})
	}

	if len(candidates) == 0 {
		// #2546: the hub is running and unsuspended but nothing is admissible right
		// now (everything is in cooldown, filtered out, disabled, or already held).
		// Previously a bare nil — indistinguishable on the wire from "suspended" or
		// "hub not ready". Send an explicit no_matching_work negative-ack.
		if capabilityMismatchSeen {
			return h.taskUnavailable(taskUnavailableCapabilityMismatch)
		}
		return h.taskUnavailable(taskUnavailableNoMatchingWork)
	}

	// Operator priority override (#queue-reorder): the ordered list of issue keys
	// the operator dragged to the front of the ready-work queue on the Operations
	// tab. It takes precedence over the default ordering below so a prioritised
	// issue is OFFERED FIRST. It never bypasses admission: every entry in
	// `candidates` already passed the SAME cooldown / failure / disabled-repo /
	// filter / in-flight exclusions above, so a pinned-but-no-longer-actionable key
	// simply never became a candidate (stale keys are skipped). Rank sentinel: a
	// candidate NOT in the override ranks at len(override), so all pinned candidates
	// sort ahead of all unpinned ones while their own relative order is the operator's.
	var queueOrderIdx map[string]int
	if h.server.deps != nil && h.server.deps.Config != nil {
		queueOrderIdx = queueOrderIndex(h.server.deps.Config.Hub.ContributeQueueOrder)
	}
	orderRank := func(c candidate) int {
		if len(queueOrderIdx) == 0 {
			return 0 // no override → every candidate ties, key is a no-op
		}
		if r, ok := queueOrderIdx[fmt.Sprintf("%s#%d", c.repoFull, c.number)]; ok {
			return r
		}
		return len(queueOrderIdx)
	}

	// Order the admissible set with a STABLE sort so the pick is deterministic
	// (easy to reason about and to test — no randomness):
	//   0. operator priority override first (#queue-reorder) — pinned issues in the
	//      operator's dragged order; a no-op when no override is set;
	//   1. own-work first (#2390 — preserved unchanged);
	//   2. then fewer recent failures first (#2435 remedy 3 backstop) — an issue
	//      whose short failure cooldown has just elapsed but which still carries
	//      failure history is deprioritised behind never-failed peers, so a
	//      flaky issue can no longer monopolise the head of the queue even if the
	//      ledger is imperfect;
	//   3. otherwise the established per-repo / creation scan order is kept.
	// When the contributor has no own work AND nothing has failed AND no override is
	// set, this is a no-op and behaviour is identical to the previous first-eligible pick.
	ownFirst := make([]candidate, len(candidates))
	copy(ownFirst, candidates)
	sort.SliceStable(ownFirst, func(i, j int) bool {
		if ri, rj := orderRank(ownFirst[i]), orderRank(ownFirst[j]); ri != rj {
			return ri < rj // operator-pinned (lower rank) sorts ahead
		}
		if ownFirst[i].isOwn != ownFirst[j].isOwn {
			return ownFirst[i].isOwn // own work sorts ahead of non-own
		}
		if ownFirst[i].interestMatch != ownFirst[j].interestMatch {
			return ownFirst[i].interestMatch // label-affinity matches ahead of non-matches (#2637)
		}
		if ownFirst[i].recentFailures != ownFirst[j].recentFailures {
			return ownFirst[i].recentFailures < ownFirst[j].recentFailures // fewer failures first
		}
		return false // equal keys → SliceStable preserves original scan order
	})

	chosen := ownFirst[0]
	if chosen.isOwn {
		h.logger.Info("[contribute-ws] prioritizing contributor's own work (#2390)",
			"username", ownUsername, "repo", chosen.repoFull, "number", chosen.number)
	}

	// The task id carries the item's own identity segment so two zero-numbered
	// external items cannot mint the same id within one second
	// (kubestellar/hive#4245). GitHub-backed work keeps its historical
	// "ct-<repo>-<number>-<unix>" shape byte for byte.
	taskID := fmt.Sprintf("ct-%s-%s-%d", chosen.repoFull, taskIDSegment(chosen.ref), time.Now().Unix())

	// #2568: mint a fresh assignment generation for this task. It is stamped on the
	// connection, shipped in task_assign below, and echoed back by the relay so a
	// later stale-worker completion carrying an older generation is fenced out.
	gen := h.nextTaskGen()
	assignedAt := time.Now()

	// Commit the CLAIM under selectMu (#7775): currentTask is what the
	// activeIssues scan above reads, so from here on no other selection can pick
	// this item. The prompt and the scoped token are filled in below, after the
	// unlock, once the GitHub round-trips that produce them have returned; if
	// either the mint or the send then fails, rollbackAssignment undoes exactly
	// this claim.
	assignment := &WSTaskAssign{
		TaskID:     taskID,
		Kind:       "issue",
		Stage:      chosen.stage,
		Role:       requestedRole,
		Repo:       chosen.repoFull,
		Number:     chosen.number,
		Title:      chosen.title,
		Key:        chosen.ref.Key(),
		SourceType: chosen.ref.SourceType,
		ExternalID: chosen.ref.ExternalID,
		URL:        chosen.url,
		Complexity: taskComplexityFromLabels(chosen.labels),
		Requirements: func() *ContributorTaskRequirements {
			if chosen.requirements.IsZero() {
				return nil
			}
			req := chosen.requirements
			return &req
		}(),
	}
	c.mu.Lock()
	c.currentTask = assignment
	c.currentTaskGen = gen
	// #2568: start the hub-owned lease clock. task_progress renews it; cleanupLoop
	// auto-releases the task if it is not renewed within wsTaskTimeout.
	c.lastLeaseRenew = assignedAt
	// Duration anchor for the run log — lastLeaseRenew moves on every
	// progress report, so it cannot serve as the start time. Also the stamp
	// rollbackAssignment hands back to unrecordAssignment.
	c.taskAssignedAt = assignedAt
	c.currentLabels = chosen.labels
	c.lastIdleReason = ""
	// The prompt and the credential are not known yet — see below. A stale
	// pendingToken from a previous task must not survive into this claim.
	c.currentPrompt = ""
	c.pendingToken = ""
	c.credentialDelivered = false
	c.tokenMintedAt = time.Time{}
	c.mu.Unlock()

	// C4: record the SERVER-AUTHORITATIVE lease for this assignment so a later
	// reconnect can be validated against what the hub actually issued — the exact
	// {task, repo, generation, tier} bound here — instead of reconstructing ownership
	// from client-supplied task_progress fields. Revoked on every release path.
	// #5681: record the item's canonical key too, so the double-assignment guard can
	// recognise the lease after a restart — including for external work, whose
	// identity is Key rather than repo#number (#4245).
	// #8287: a grant whose lease did not reach disk is refused, not handed out.
	// The claim is rolled back in full — the item is offerable again at once, no
	// slot or lease records a task that never shipped — and the contributor gets
	// an explicit lease_persist_failed instead of a task_assign the hub would
	// have no record of after its next restart. No task-MCP lease token is
	// minted for it either: that mint sits downstream of the task_assign.
	if err := h.recordLeaseForKeyStage(identityOf(c), taskID, chosen.repoFull, chosen.number,
		chosen.ref.Key(), c.profile.TrustTier, chosen.stage, gen, assignedAt); err != nil {
		h.rollbackAssignment(c, taskID)
		h.logger.Warn("[contribute-ws] refusing task: lease could not be persisted — "+
			"the grant would not survive a hub restart; check the lease registry directory",
			"username", identityOf(c), "task", taskID, "repo", chosen.repoFull,
			"number", chosen.number, "reason", taskUnavailableLeasePersistFailed, "error", err)
		h.recordDecision(decisionUsername(c), decisionRefused, taskID, chosen.repoFull, chosen.number,
			"reason="+taskUnavailableLeasePersistFailed+": "+err.Error())
		return h.taskUnavailable(taskUnavailableLeasePersistFailed)
	}

	// #2566: record this assignment against the identity's rolling hourly/daily
	// windows so the next selectTask enforces tier_limits.max_per_hour /
	// max_per_day. Recorded here, under the lock, so the next selection's count
	// is exact; a refused pass (which returns early above) never consumes a slot,
	// and a claim that fails to ship hands its slot back through
	// rollbackAssignment (#7775). Uses the same identity key as the concurrency
	// gate.
	h.recordAssignment(identityOf(c), assignedAt)

	// #8380: mirror the lease into the worker-claim ledger so humans, hub
	// agents and other hives can see this contributor holds the item. Issues
	// only — synthetic pr-review and external items key a claim on nothing.
	if chosen.number > 0 {
		h.claimIssueForContributor(c, chosen.repoFull, chosen.number)
	}

	// The claim is committed and visible to every other selection; nothing below
	// touches the shared selection state, so the fleet-wide lock is released
	// BEFORE the GitHub round-trips (#7775).
	unlock()

	if chosen.extEngine != "" {
		prompt := buildTaskPromptForContributor(chosen.ref, chosen.title, false, h.writingGuideSection())
		if chosen.stage != "" {
			prompt += runStageWorktreePrompt(chosen.repoFull, chosen.ref.Key(), chosen.stage, gen)
		}
		task := ExternalExecutionTask{
			Engine:           chosen.extEngine,
			Mode:             chosen.extMode,
			Capability:       chosen.extCapability,
			WorkflowVersion:  chosen.extWorkflowVersion,
			ContractRevision: externalWorkflowContractRevision,
			Identity:         identityOf(c),
			Tier:             c.profile.TrustTier,
			TaskID:           taskID,
			TaskGen:          gen,
			WorkKey:          chosen.ref.Key(),
			Repo:             chosen.repoFull,
			Number:           chosen.number,
			Stage:            chosen.stage,
			Title:            chosen.title,
			Summary:          prompt,
			SourceType:       chosen.ref.SourceType,
			ExternalID:       chosen.ref.ExternalID,
			URL:              chosen.url,
		}
		task.InputRevision = externalInputRevision(task)
		c.mu.Lock()
		if c.currentTask == nil || c.currentTask.TaskID != taskID {
			c.mu.Unlock()
			h.logger.Info("[contribute-ws] external claim released before dispatch; not sending assignment ack",
				"username", ownUsername, "task", taskID)
			return nil
		}
		c.currentPrompt = prompt
		c.pendingToken = ""
		c.credentialDelivered = false
		c.tokenMintedAt = time.Time{}
		pending := task
		c.pendingExternalTask = &pending
		c.mu.Unlock()
		h.recordAgentClaim(context.Background(), c, taskID, chosen.repoFull, chosen.number, time.Now())
		return &WSMessage{
			Type:           "task_assign_external",
			Seq:            h.nextSeq(),
			TaskID:         taskID,
			TaskGen:        gen,
			Stage:          chosen.stage,
			Kind:           "issue",
			Repo:           chosen.repoFull,
			Number:         chosen.number,
			Title:          chosen.title,
			URL:            chosen.url,
			TaskKey:        chosen.ref.Key(),
			SourceType:     chosen.ref.SourceType,
			ExternalID:     chosen.ref.ExternalID,
			ExternalEngine: chosen.extEngine,
			Prompt:         prompt,
			Labels:         chosen.labels,
			ContribLabels:  []string{"contributor/" + c.profile.GitHubUsername},
		}
	}

	// Mint through the shared path so task_assign and the heartbeat token-refresh
	// advertise tokens minted the same way (#2393 item 2). tokenMintedAt below
	// arms the refresh ticker for the token we hand out here. C4: the token is
	// scoped to the chosen issue's REPOSITORY, not the whole installation.
	ghToken, err := h.mintScopedToken(c.profile.TrustTier, chosen.repoFull)
	if err != nil {
		// #7775: the claim was committed before the mint; hand it back in full so
		// the item is offerable again at once and no slot, lease or cooldown
		// records a task that never shipped.
		h.rollbackAssignment(c, taskID)

		// #7869: since C4 the token is scoped to the CANDIDATE'S repository, so a
		// 422/404 from GitHub means that repo — renamed, deleted, or removed from
		// the installation — cannot be minted for, and nothing else in the pass
		// need be affected. Exclude the repo for a cooldown and select again
		// (bounded), rather than letting one stale repo-list entry wedge the hive
		// behind an opaque token_mint_failed for every contributor.
		if ghpkg.IsRepoScopeMintError(err) {
			h.markRepoUnmintable(chosen.repoFull, time.Now())
			h.logger.Warn("[contribute-ws] GitHub App cannot mint a token scoped to this repo — "+
				"excluding it from selection; check the project repo list for a renamed or removed repository",
				"repo", chosen.repoFull, "cooldown", mintFailureRepoCooldown.String(), "error", err)
			*skippedUnmintable++
			if *skippedUnmintable <= maxUnmintableRepoRetries {
				return nil // selectTask scans again without this repo
			}
			return h.taskUnavailable(taskUnavailableRepoUnmintable)
		}

		// #2436 finding 1: a mint failure previously returned nil, stranding the
		// contributor with no message (the log even said "skipping task" while
		// abandoning the whole selection). Send an explicit token_mint_failed
		// negative-ack so the failure is diagnosable instead of an indefinite
		// hang. Any other mint error is keyed on the tier or the App, not the
		// candidate, so every candidate in this pass would fail identically; do not
		// fall through. Preserve the existing Warn log.
		h.logger.Warn("[contribute-ws] failed to mint scoped token — task unavailable",
			"tier", c.profile.TrustTier, "repo", chosen.repoFull, "error", err)
		return h.taskUnavailable(taskUnavailableTokenMintFailed)
	}

	// #2539: build the prompt through the shared, credential-free buildTaskPrompt
	// so the exact text shipped in task_assign below can also be PREVIEWED
	// read-only in the ops tab. The prompt is a pure function of task metadata —
	// the minted github_token is attached to the WSMessage separately (never inside
	// the prompt), so previewing the prompt can never leak the token. buildTaskPrompt
	// itself carries the #2545 workspace-clone instruction (real checkout into
	// $HIVE_WORKSPACE_DIR rather than a fork-only --clone=false).
	canPush := h.contributorCanPush(chosen.repoFull, ownUsername)
	// #8124: the guide is THIS hive's project.writing_guide, resolved here
	// rather than inside the builder — the builder stays a pure function of task
	// metadata, and the assigning hub is the only party that should decide how
	// PRs in its repos read. A relay subscribed to two hives therefore gets each
	// hive's guide on that hive's tasks. Unset renders nothing.
	guide := h.writingGuideSection()
	prompt := buildTaskPromptForContributor(chosen.ref, chosen.title, canPush, guide)
	if requestedRole != "" {
		prompt = buildRoleTaskPromptForContributor(chosen.ref, chosen.title, requestedRole, h.roleKickPrompt(requestedRole), canPush, guide)
	}
	if chosen.stage != "" {
		prompt += runStageWorktreePrompt(chosen.repoFull, chosen.ref.Key(), chosen.stage, gen)
	}
	// #4105: tell the agent up front — from the hub's own handshake-recorded
	// invocation values — the exact attribution trailer its PR body must end
	// with, so the footer is intentionally produced rather than appended only
	// by the post-merge reconciliation safety net (#4088, unchanged).
	prompt += attributionPromptInstruction(promptInvocationMeta(c))

	// Decorate the claim. If it is no longer on the connection — the socket
	// dropped during the GitHub round-trips and releaseOnDisconnect released it —
	// there is nobody to send to: return nil, which handleReady treats as
	// "nothing to send", rather than a task_assign for a released item.
	c.mu.Lock()
	if c.currentTask == nil || c.currentTask.TaskID != taskID {
		c.mu.Unlock()
		h.logger.Info("[contribute-ws] claim released while its credential was being minted; not sending task_assign",
			"username", ownUsername, "task", taskID)
		return nil
	}

	// Store the prompt (never the token) so FleetSnapshot can preview it (#2539).
	c.currentPrompt = prompt
	// #2537: hold the minted scoped token as PENDING rather than shipping it in the
	// task_assign below. It is delivered only AFTER the acceptance decision — see
	// the ready-handler (auto-accept default) and the task_accepted handler
	// (explicit-accept mode) — via deliverTaskCredential. A fresh assignment resets
	// the delivered flag so the new task's credential is (re)delivered post-accept.
	// tokenMintedAt is set here so the #2393 refresh cycle is armed for the token we
	// hand out; deliverTaskCredential re-stamps it on actual delivery to anchor the
	// 50-minute refresh on when the relay truly received the credential.
	c.pendingToken = ghToken
	c.credentialDelivered = false
	c.tokenMintedAt = time.Now()
	c.mu.Unlock()

	turnEnvelopeID := h.persistTurnEnvelopeForAssignment(c, assignment, gen, prompt, chosen.labels)

	// #8380: assert the issue claim for this lease — on the issue itself when
	// the tier may write comments, on the lease alone otherwise. Runs after the
	// assignment is confirmed still live and off the selection lock, like the
	// mint; a failed post never refuses the task.
	h.recordAgentClaim(context.Background(), c, taskID, chosen.repoFull, chosen.number, time.Now())

	return &WSMessage{
		Type:    "task_assign",
		Seq:     h.nextSeq(),
		TaskID:  taskID,
		TaskGen: gen,
		Stage:   chosen.stage,
		Kind:    "issue",
		Role:    requestedRole,
		Repo:    chosen.repoFull,
		Number:  chosen.number,
		Title:   chosen.title,
		URL:     chosen.url,
		// Source-aware identity (kubestellar/hive#4245). Additive: a GitHub
		// assignment carries exactly the fields it always did, and these tell a
		// relay working a Linear or Jira item what the item actually IS instead
		// of leaving it to infer one from `number: 0`.
		TaskKey:    chosen.ref.Key(),
		SourceType: chosen.ref.SourceType,
		ExternalID: chosen.ref.ExternalID,
		Requirements: func() *ContributorTaskRequirements {
			if chosen.requirements.IsZero() {
				return nil
			}
			req := chosen.requirements
			return &req
		}(),
		Complexity: taskComplexityFromLabels(chosen.labels),
		// #2537: NO github_token / token_expires_at here. The scoped credential is
		// split out of task_assign and delivered only after acceptance (see
		// pendingToken / deliverTaskCredential). task_assign now carries exactly the
		// metadata needed to DECIDE — repo/number/title/url/labels/prompt — plus the
		// #2568 TaskGen lease token, and no credential, so nothing an agent could act
		// on is authenticated until the task's source has been accepted under the
		// operator/contributor policy.
		Prompt: prompt,
		// The chosen issue's own labels — the Labels envelope field was declared
		// but never populated, so a client reading it got nothing (kubestellar/
		// hive#2393 item 8). Carried on the candidate from the scan above.
		Labels:         chosen.labels,
		ContribLabels:  []string{"contributor/" + c.profile.GitHubUsername},
		TurnEnvelopeID: turnEnvelopeID,
	}
}

// runStageWorktreePrompt tells a run-stage-capable relay to put the task's
// writes in a per-stage git worktree instead of the shared checkout.
func runStageWorktreePrompt(repoFull, runKey, stage string, gen uint64) string {
	if repoFull == "" || runKey == "" || stage == "" || gen == 0 {
		return ""
	}
	return fmt.Sprintf(" This is run stage %q generation %d for %s. After the shared checkout exists, create and use a per-stage worktree at '$HIVE_WORKSPACE_DIR/runs/%s/%s-%d' from the task's target base branch with 'mkdir -p \"$HIVE_WORKSPACE_DIR/runs/%s\"' and 'git -C \"$HIVE_WORKSPACE_DIR/%s\" worktree add --detach \"$HIVE_WORKSPACE_DIR/runs/%s/%s-%d\" upstream/<base-branch>'; do all edits and git status checks in that worktree, not in the shared checkout. ",
		stage, gen, runKey, sanitizeRunPromptPath(runKey), sanitizeRunPromptPath(stage), gen, sanitizeRunPromptPath(runKey), repoFull, sanitizeRunPromptPath(runKey), sanitizeRunPromptPath(stage), gen)
}

func runStageWorktreePath(identity, runKey, stage string, gen uint64) string {
	if identity == "" || runKey == "" || stage == "" || gen == 0 {
		return ""
	}
	return filepath.Join(agentWorkspaceRoot, identity, "runs", sanitizeRunPromptPath(runKey), fmt.Sprintf("%s-%d", sanitizeRunPromptPath(stage), gen))
}

func removeRunStageWorktree(identity, runKey, stage string, gen uint64) error {
	path := runStageWorktreePath(identity, runKey, stage, gen)
	if path == "" {
		return nil
	}
	return os.RemoveAll(path)
}

func sanitizeRunPromptPath(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "run"
	}
	return out
}

// assignedToOthers reports whether an issue is assigned to at least one user
// AND none of its assignees is the given contributor. An unassigned issue
// (empty list) returns false, and an issue assigned to the contributor
// themselves returns false, so both remain eligible for pickup (#2357). The
// username comparison is case-insensitive to match GitHub login semantics.
func assignedToOthers(assignees []string, username string) bool {
	if len(assignees) == 0 {
		return false
	}
	for _, a := range assignees {
		if strings.EqualFold(a, username) {
			return false
		}
	}
	return true
}

// stringSliceFromAny coerces a JSON-decoded value (from an issue map marshaled
// via encoding/json) into a []string. Labels arrive as []any of strings; any
// non-string elements are skipped. Returns nil for a missing/other-typed value.
func stringSliceFromAny(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func runStageFromIssueMap(issue map[string]any) string {
	for _, key := range []string{"stage", "run_stage"} {
		if raw, ok := issue[key].(string); ok && validStage(strings.TrimSpace(raw)) {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}
