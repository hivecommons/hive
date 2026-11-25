package dashboard

import (
	"context"
	"errors"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/worksource"
)

// The live assignment gate. A maintainer's `hold` (or the loss of the
// reporter-trust triage label) must reach the relay before a contributor is
// handed the issue, not after: selectTask used to pick from the cached
// status snapshot, lease and mint, and only then read the issue — so a hold
// put on 30 s before the assignment was logged as "claim refused" in the
// same millisecond the task_assign went out, and the contributor shipped a
// PR on a held issue.
const (
	// assignGatePassBudget bounds every live issue read one selection makes,
	// together. The reads run under selectMu (the claim must not be decided
	// on a read taken before another selection reserved the item), so the
	// budget is what keeps one slow forge from stalling the fleet's `ready`
	// handling for longer than this.
	assignGatePassBudget = 10 * time.Second
	// maxAssignGateCandidates is how many ordered candidates one selection
	// may read live before it gives up for this round. A refused candidate is
	// remembered (assignGateBlockedTTL), so the next selection does not read
	// it again and moves on to the rest of the queue.
	maxAssignGateCandidates = 5
	// assignGateBlockedTTL is how long a live refusal keeps the item out of
	// the candidate scan without another read. The webhook patch and the
	// next enumeration normally remove a held issue from the queue well
	// inside it; this only bounds the reads when they have not yet.
	assignGateBlockedTTL = 2 * time.Minute
	// assignGateErrorTTL is the same exclusion after a failed read: the item
	// is not assigned on an unknown verdict (fail closed), and is retried
	// sooner than a definite refusal.
	assignGateErrorTTL = 30 * time.Second
	// heldTaskReconcileInterval is how often in-flight contributor tasks are
	// re-read for a hold or a lost triage label, for the case the `issues`
	// webhook that would have revoked them was missed.
	heldTaskReconcileInterval = 2 * time.Minute
	// heldTaskReconcileTimeout bounds each of the reconcile's issue reads.
	heldTaskReconcileTimeout = 10 * time.Second
)

// filterLiveGate counts candidates the live issue read refused (#19052
// filtered map; additive).
const filterLiveGate = "live_gate"

// Revocation reasons the relay shows the contributor on task_revoke.
const (
	revokeReasonIssueHeld      = "the maintainer put this issue on hold"
	revokeReasonIssueUntriaged = "the issue is waiting for maintainer triage again (its triage label was removed)"
	revokeDetailIssueHeld      = "revoked: issue put on hold"
	revokeDetailIssueUntriaged = "revoked: issue lost its triage label"
	revokeActivityIssueBlocked = "reassigned after the issue was held"
)

// errAssignGateBudget is the fail-closed verdict for a candidate the pass
// had no budget left to read.
var errAssignGateBudget = errors.New("live issue check budget spent for this selection")

// assignGateFunc returns the live issue read: the injected seam (tests), the
// wired GitHub client, or nil when neither exists (a hive with no forge
// client mints no token either, so there is nothing to protect).
func (h *ContributeWSHub) assignGateFunc() func(ctx context.Context, repo string, number int) (ghpkg.IssueAssignGate, error) {
	if h == nil {
		return nil
	}
	if h.assignGate != nil {
		return h.assignGate
	}
	if h.server != nil && h.server.deps != nil && h.server.deps.GHClient != nil {
		return h.server.deps.GHClient.IssueAssignBlockReason
	}
	return nil
}

// assignGateApplies reports whether a candidate is a GitHub issue the live
// read can judge. External (Linear, Jira) items have no forge issue to read.
func assignGateApplies(ref worksource.Ref, number int) bool {
	if number <= 0 {
		return false
	}
	if ref != (worksource.Ref{}) {
		return ref.IsGitHubIssue()
	}
	return true
}

// assignGateRefusal runs the live gate on one candidate. It returns "" to
// admit, or the refusal to log. A failed read refuses (fail closed). The
// configured contribute skip labels are applied to the labels the forge
// reports now, so a skip label added since the snapshot counts as well.
func (h *ContributeWSHub) assignGateRefusal(ctx context.Context, repoFull string, number int, ref worksource.Ref) (string, error) {
	gate := h.assignGateFunc()
	if gate == nil || !assignGateApplies(ref, number) {
		return "", nil
	}
	if ctx.Err() != nil {
		return "", errAssignGateBudget
	}
	res, err := gate(ctx, repoFull, number)
	if err != nil {
		return "", err
	}
	if res.Reason != "" {
		return res.Reason, nil
	}
	if label, ok := h.contributeSkipLabel(res.Labels); ok {
		return "contribute skip label: " + label, nil
	}
	return "", nil
}

// markAssignBlocked keeps key out of the candidate scan until ttl elapses.
func (h *ContributeWSHub) markAssignBlocked(key string, now time.Time, ttl time.Duration) {
	if h == nil || key == "" {
		return
	}
	h.assignBlockedMu.Lock()
	defer h.assignBlockedMu.Unlock()
	if h.assignBlocked == nil {
		h.assignBlocked = make(map[string]time.Time)
	}
	h.assignBlocked[key] = now.Add(ttl)
}

// assignBlockedActive reports whether a live refusal still excludes key.
func (h *ContributeWSHub) assignBlockedActive(key string, now time.Time) bool {
	if h == nil || key == "" {
		return false
	}
	h.assignBlockedMu.Lock()
	defer h.assignBlockedMu.Unlock()
	until, ok := h.assignBlocked[key]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(h.assignBlocked, key)
		return false
	}
	return true
}

// PreemptIssueHeld revokes every in-flight contributor task on repo#number
// because a maintainer put the issue on hold. Modelled on
// PreemptIssueCoveredByPR: the task is taken off the connection, its lease,
// claim and pending credential are released, task_revoke tells the relay
// why, and the session is offered different work. The contributor's branch
// and any PR it opened are left exactly as they are. Returns the number of
// sessions released.
func (h *ContributeWSHub) PreemptIssueHeld(repo string, number int) int {
	return h.preemptIssueBlocked(repo, number, revokeReasonIssueHeld, revokeDetailIssueHeld)
}

// PreemptIssueUntriaged is PreemptIssueHeld for an issue that lost the
// reporter-trust triage gate (its triage label came off an untrusted
// reporter's issue).
func (h *ContributeWSHub) PreemptIssueUntriaged(repo string, number int) int {
	return h.preemptIssueBlocked(repo, number, revokeReasonIssueUntriaged, revokeDetailIssueUntriaged)
}

func (h *ContributeWSHub) preemptIssueBlocked(repo string, number int, reason, detail string) int {
	if h == nil || number <= 0 || strings.TrimSpace(repo) == "" {
		return 0
	}
	now := time.Now()
	var targets []releaseTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		match := c.currentTask != nil && c.currentTask.Number == number &&
			sameRepoSpelling(c.currentTask.Repo, repo)
		if match {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			c.pendingToken = ""
			c.credentialDelivered = false
			c.pendingExternalTask = nil
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, releaseTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		return 0
	}
	// The cached snapshot may still list the issue until the next
	// enumeration; keep it out of the reassignment that follows without
	// another read.
	for _, tgt := range targets {
		h.markAssignBlocked(worksource.Ref{Repo: tgt.task.Repo, Number: tgt.task.Number}.Key(), now, assignGateBlockedTTL)
	}
	released, _ := h.yankAndReassign(targets, reason, detail, revokeActivityIssueBlocked)
	h.logger.Info("[contribute-ws] in-flight task revoked: issue no longer assignable",
		"repo", repo, "number", number, "reason", reason, "released", released)
	return released
}

// preemptBlockedSnapshots is the webhook half of the revocation: an
// `issues` labeled/unlabeled re-read that classifies the issue as held, or
// as an untrusted reporter's issue awaiting triage, revokes any contributor
// task still dispatched on it. The classification is the GitHub client's
// own (classifyWebhookIssue), so it uses the configured hold labels and
// reporter-trust settings.
func (s *Server) preemptBlockedSnapshots(snaps []ghpkg.WebhookItemSnapshot) {
	if s == nil || s.contributeHub == nil {
		return
	}
	for _, snap := range snaps {
		if snap.IsPR || !snap.Open() || snap.Number <= 0 {
			continue
		}
		switch {
		case snap.Patch.Bucket == ghpkg.WebhookBucketHeld:
			s.contributeHub.PreemptIssueHeld(snap.Repo, snap.Number)
		case snap.Patch.WorkIssue != nil && snap.Patch.WorkIssue.Bucket == string(stageengine.BucketReporterTriage):
			s.contributeHub.PreemptIssueUntriaged(snap.Repo, snap.Number)
		}
	}
}

// heldTaskReconcileLoop re-reads, on a fixed cadence, every issue a
// contributor is working on and revokes the task when the issue is now held
// or awaiting triage. It is the backstop for a missed `issues` webhook.
func (h *ContributeWSHub) heldTaskReconcileLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.reconcileHeldTasks(context.Background())
		}
	}
}

// reconcileHeldTasks is one pass of heldTaskReconcileLoop. A failed read
// leaves the task alone: revocation needs a definite verdict. Returns the
// number of sessions released.
func (h *ContributeWSHub) reconcileHeldTasks(ctx context.Context) int {
	gate := h.assignGateFunc()
	if gate == nil {
		return 0
	}
	type item struct {
		repo   string
		number int
	}
	seen := map[string]bool{}
	var items []item
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		if t := c.currentTask; t != nil && t.Repo != "" && assignGateApplies(worksource.Ref{}, t.Number) {
			key := worksource.Ref{Repo: t.Repo, Number: t.Number}.Key()
			if !seen[key] {
				seen[key] = true
				items = append(items, item{repo: t.Repo, number: t.Number})
			}
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()
	released := 0
	for _, it := range items {
		readCtx, cancel := context.WithTimeout(ctx, heldTaskReconcileTimeout)
		res, err := gate(readCtx, it.repo, it.number)
		cancel()
		if err != nil {
			h.logger.Warn("[contribute-ws] held-task reconcile: issue read failed; task left running",
				"repo", it.repo, "number", it.number, "error", err)
			continue
		}
		switch {
		case res.Held:
			released += h.PreemptIssueHeld(it.repo, it.number)
		case res.AwaitingTriage:
			released += h.PreemptIssueUntriaged(it.repo, it.number)
		}
	}
	return released
}
