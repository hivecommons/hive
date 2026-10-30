package automerge

import (
	"context"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stale-red recovery for adopted PRs (hivecommons/hive#14500).
//
// An in-place adoption can sit red for a reason nobody owns: the check that
// failed was a shared-CI flake or a red base branch, fixed on the base since,
// while the adopted head never ran CI again. Before this step the 074 escape
// read that as "nothing the hive can push" and handed the PR to a person at
// 085 — observed on hivecommons/hive#14430, #14440 and #14456 after the
// #14406 flake was fixed by #14415. The fix was a branch update or a re-run
// away.
//
// When the dwell would escalate a red adopted head whose failing check is
// green on the base branch tip, the hive first tries one recovery: if the
// head is behind the base, update-branch (a merge commit, no human commit
// rewritten; GitHub allows it on a fork when maintainers may edit); if it is
// current, re-run the failed workflow jobs for the head. One attempt per
// (head, base tip) pair and a small lifetime budget per adoption are recorded
// on the stage record, so the step never loops. Escalation to 085 waits
// until an attempt was made and the head is still red.

const (
	adoptionRecoveryUpdateBranch = "update-branch"
	adoptionRecoveryRerun        = "rerun-failed-jobs"
	adoptionRecoverySkipPrefix   = "skipped:"
	// adoptionRecoveryClaimReason is the update-branch claim reason recorded
	// when the hive refreshes a stale-red adopted head.
	adoptionRecoveryClaimReason = "adopted-recovery"
	// adoptionRecoveryMaxAttempts bounds recoveries over the life of one
	// adoption: a base that keeps moving under a genuinely red head gets a
	// few refreshes, then the 074 escape runs as before.
	adoptionRecoveryMaxAttempts = 3
)

// adoptionRecovery is what recoverStaleRedAdoption did for one sweep.
type adoptionRecovery struct {
	// Attempted means an update-branch or re-run was issued: the escalation
	// waits for CI on the result.
	Attempted bool
	// Deferred means preparation ownership withheld the request, so no
	// recovery attempt or lifetime budget was consumed.
	Deferred bool
	// Reason is the structured outcome: update-branch, rerun-failed-jobs or
	// skipped:<why>.
	Reason string
}

func adoptionRecoverySkipped(why string) adoptionRecovery {
	return adoptionRecovery{Reason: adoptionRecoverySkipPrefix + why}
}

// adoptionFailingCheck names the check the lane reason blames, or "".
func adoptionFailingCheck(reason string) string {
	for _, prefix := range []string{"required-check-failing:", "check-failing:"} {
		if name, ok := strings.CutPrefix(reason, prefix); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// adoptionRecoveryCandidate reports a hive-owned in-place adoption whose
// only blocker is red CI on the adopted head, with no open shared-CI
// incident, red base, fork guard, hold label or review blocker to defer to,
// and no workflow-path refusal pinning it to a person.
func adoptionRecoveryCandidate(obs stageObservation, rec stagelabels.Record) bool {
	if !rec.AdoptedInPlace() || rec.AdoptedBy > 0 || obs.HeadSHA == "" || obs.HeadSHA != rec.AdoptedHead {
		return false
	}
	if rec.AdoptionEscalated(obs.HeadSHA) && rec.AdoptEscalatedReason == adoptionWorkflowRefusedReason {
		return false
	}
	if adoptionBlocked(obs.Labels) != "" || verdictBlocks(obs.Detail) || verdictNeedsHuman(obs.Detail) {
		return false
	}
	if obs.SharedCIIncident > 0 || len(obs.BaseRedChecks) > 0 || len(obs.ForkGuardChecks) > 0 {
		return false
	}
	reason := nonEmpty(obs.Reason, obs.LaneReason)
	if strings.HasPrefix(reason, stageReasonBaseBranchRed) {
		return false
	}
	return classifyCIReason(reason) == ciReasonRed
}

// baseCheckConclusionIsRed mirrors the CI gate's rollup reading of a
// completed check-run conclusion.
func baseCheckConclusionIsRed(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return true
	}
	return false
}

// baseCheckState is the latest completed check run per name on a ref.
type baseCheckState struct {
	completed map[string]bool // name → present
	red       map[string]bool // name → red
}

// baseChecks lists the latest completed check run per name on ref.
func (c *Engine) baseChecks(ctx context.Context, owner, repo, ref string) (baseCheckState, error) {
	ctx = hgithub.WithRESTCaller(ctx, "hive:adopted_recovery_base_checks")
	latest := map[string]*gh.CheckRun{}
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return baseCheckState{}, err
		}
		if runs != nil {
			for _, cr := range runs.CheckRuns {
				if cr == nil || cr.GetName() == "" {
					continue
				}
				if prev := latest[cr.GetName()]; prev == nil || cr.GetID() > prev.GetID() {
					latest[cr.GetName()] = cr
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	st := baseCheckState{completed: map[string]bool{}, red: map[string]bool{}}
	for name, cr := range latest {
		if cr.GetStatus() != "completed" {
			continue
		}
		st.completed[name] = true
		if baseCheckConclusionIsRed(cr.GetConclusion()) {
			st.red[name] = true
		}
	}
	return st, nil
}

// baseClearsFailure reports whether the base tip's checks say the head's
// failure is not the PR's: the blamed check completed green on the base, or
// — when the base does not run that check (PR-only workflows) or the
// reason names none — the base tip has completed checks and none is red.
// skip names why not.
func baseClearsFailure(st baseCheckState, failing string) (ok bool, skip string) {
	if failing != "" && st.completed[failing] {
		if st.red[failing] {
			return false, "base-red:" + failing
		}
		return true, ""
	}
	if len(st.completed) == 0 {
		return false, "base-checks-unknown"
	}
	if len(st.red) > 0 {
		return false, "base-red"
	}
	return true, ""
}

// recoverStaleRedAdoption runs where the 074 escape would otherwise
// escalate a red adopted head. It issues at most one update-branch or
// re-run per (head, base tip) and reports whether one was issued this
// sweep; a skipped recovery leaves the escalation to proceed as before.
// API failures are logged and treated as a skip for this sweep without
// consuming the attempt, so a transient error is retried next sweep.
func (c *Engine) recoverStaleRedAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record) adoptionRecovery {
	if !adoptionRecoveryCandidate(obs, rec) {
		return adoptionRecoverySkipped("not-a-candidate")
	}
	logArgs := func(extra ...any) []any {
		return append([]any{"repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "base", obs.BaseRef, "lane_reason", nonEmpty(obs.Reason, obs.LaneReason)}, extra...)
	}
	if rec.AdoptRecoveryCount >= adoptionRecoveryMaxAttempts {
		out := adoptionRecoverySkipped("attempts-exhausted")
		c.info("adopted PR recovery", logArgs("reason", out.Reason, "attempts", rec.AdoptRecoveryCount)...)
		return out
	}
	baseRef := strings.TrimSpace(obs.BaseRef)
	if baseRef == "" {
		return adoptionRecoverySkipped("no-base-ref")
	}
	restCtx := hgithub.WithRESTCaller(ctx, "hive:adopted_recovery_compare")
	cmp, _, err := c.gh.Repositories.CompareCommits(restCtx, obs.Owner, obs.Repo, baseRef, obs.HeadSHA, &gh.ListOptions{PerPage: 1})
	if err != nil {
		c.warn("adopted PR recovery", logArgs("reason", adoptionRecoverySkipPrefix+"compare-failed", "error", err)...)
		return adoptionRecoverySkipped("compare-failed")
	}
	baseSHA := cmp.GetBaseCommit().GetSHA()
	if baseSHA == "" {
		return adoptionRecoverySkipped("base-sha-unknown")
	}
	if rec.AdoptionRecoveryAttempted(obs.HeadSHA, baseSHA) {
		// Steady state after an attempt that left the head red: the escape
		// proceeds; no log, this repeats every sweep until the head moves.
		return adoptionRecoverySkipped("already-attempted")
	}
	st, err := c.baseChecks(ctx, obs.Owner, obs.Repo, baseSHA)
	if err != nil {
		c.warn("adopted PR recovery", logArgs("reason", adoptionRecoverySkipPrefix+"base-checks-failed", "error", err)...)
		return adoptionRecoverySkipped("base-checks-failed")
	}
	failing := adoptionFailingCheck(nonEmpty(obs.Reason, obs.LaneReason))
	if ok, skip := baseClearsFailure(st, failing); !ok {
		out := adoptionRecoverySkipped(skip)
		c.info("adopted PR recovery", logArgs("reason", out.Reason, "base_sha", baseSHA, "failing_check", failing)...)
		return out
	}
	behind := cmp.GetBehindBy()
	if behind > 0 {
		if c.requireCurrentBaseFor(obs.Owner + "/" + obs.Repo) {
			// Existing recovery evidence already proved that the new base
			// clears this failure. This admits repair, never a merge: the
			// normal current-head CI and review gates still run afterward.
			if reason, err := c.admitBasePreparation(ctx, obs.Owner, obs.Repo, baseRef, obs.Number, obs.HeadSHA, obs.Lane, false); reason != "" || err != nil {
				return adoptionRecoverySkipped(nonEmpty(reason, "base-refresh-state-unavailable"))
			}
		}
		out, done := c.recoverByUpdateBranch(ctx, store, key, obs, logArgs("base_sha", baseSHA, "behind_by", behind, "failing_check", failing))
		if done {
			if !out.Deferred {
				store.MarkAdoptionRecovery(key, obs.HeadSHA, baseSHA)
			}
			return out
		}
	}
	store.MarkAdoptionRecovery(key, obs.HeadSHA, baseSHA)
	return c.recoverByRerun(ctx, obs, logArgs("base_sha", baseSHA, "behind_by", behind, "failing_check", failing))
}

// recoverByUpdateBranch merges the base into the adopted head. done is
// false only when GitHub says the head is already current, in which case
// the caller falls through to a re-run.
func (c *Engine) recoverByUpdateBranch(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, logArgs []any) (adoptionRecovery, bool) {
	fork := obs.HeadRepo != "" && !strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo)
	if err := c.executeUpdateBranchEffect(ctx, obs.Owner, obs.Repo, obs.Number, obs.HeadSHA, adoptionRecoveryClaimReason); err != nil {
		if wait := preparationWaitReason(err); wait != "" {
			return adoptionRecovery{Deferred: true, Reason: adoptionRecoverySkipPrefix + wait}, true
		}
		var out adoptionRecovery
		switch classifyUpdateBranchError(err, fork) {
		case updateBranchUpToDate:
			return adoptionRecovery{}, false
		case updateBranchForkNotEditable:
			out = adoptionRecoverySkipped("fork-not-editable")
		case updateBranchConflict:
			out = adoptionRecoverySkipped("conflict")
		default:
			out = adoptionRecoverySkipped("update-branch-failed")
		}
		c.warn("adopted PR recovery", append(logArgs, "reason", out.Reason, "error", err)...)
		return out, true
	}
	c.info("adopted PR recovery", append(logArgs, "reason", adoptionRecoveryUpdateBranch)...)
	// The merge commit is the hive's; when GitHub already shows it, record
	// the push so the attribution check does not read it as the author's.
	if pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:adopted_recovery_head"), obs.Owner, obs.Repo, obs.Number); err == nil {
		if head := pr.GetHead().GetSHA(); head != "" && head != obs.HeadSHA {
			store.MarkHivePushedHead(key, head)
		}
	}
	return adoptionRecovery{Attempted: true, Reason: adoptionRecoveryUpdateBranch}, true
}

// recoverByRerun re-runs the failed jobs of the latest failed run of every
// workflow on the adopted head.
func (c *Engine) recoverByRerun(ctx context.Context, obs stageObservation, logArgs []any) adoptionRecovery {
	ctx = hgithub.WithRESTCaller(ctx, "hive:adopted_recovery_rerun")
	runs, err := c.latestFailedWorkflowRuns(ctx, obs.Owner, obs.Repo, obs.HeadSHA)
	if err != nil {
		c.warn("adopted PR recovery", append(logArgs, "reason", adoptionRecoverySkipPrefix+"list-runs-failed", "error", err)...)
		return adoptionRecoverySkipped("list-runs-failed")
	}
	if len(runs) == 0 {
		out := adoptionRecoverySkipped("no-failed-runs")
		c.info("adopted PR recovery", append(logArgs, "reason", out.Reason)...)
		return out
	}
	var rerun []int64
	for _, run := range runs {
		if _, err := c.gh.Actions.RerunFailedJobsByID(ctx, obs.Owner, obs.Repo, run.GetID()); err != nil {
			c.warn("adopted PR recovery: re-run refused", append(logArgs, "run", run.GetID(), "error", err)...)
			continue
		}
		rerun = append(rerun, run.GetID())
	}
	if len(rerun) == 0 {
		out := adoptionRecoverySkipped("rerun-refused")
		c.warn("adopted PR recovery", append(logArgs, "reason", out.Reason, "runs", len(runs))...)
		return out
	}
	c.info("adopted PR recovery", append(logArgs, "reason", adoptionRecoveryRerun, "runs", rerun)...)
	return adoptionRecovery{Attempted: true, Reason: adoptionRecoveryRerun}
}

// latestFailedWorkflowRuns returns, per workflow, the newest run for sha
// that completed as failure or timed_out.
func (c *Engine) latestFailedWorkflowRuns(ctx context.Context, owner, repo, sha string) ([]*gh.WorkflowRun, error) {
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: 100}}
	latest := map[int64]*gh.WorkflowRun{}
	for {
		runs, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		if runs != nil {
			for _, run := range runs.WorkflowRuns {
				if run == nil || run.GetHeadSHA() != sha {
					continue
				}
				cur, ok := latest[run.GetWorkflowID()]
				if !ok || run.GetRunNumber() > cur.GetRunNumber() ||
					(run.GetRunNumber() == cur.GetRunNumber() && run.GetRunAttempt() > cur.GetRunAttempt()) {
					latest[run.GetWorkflowID()] = run
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	var out []*gh.WorkflowRun
	for _, run := range latest {
		if run.GetStatus() != "completed" {
			continue
		}
		switch run.GetConclusion() {
		case "failure", "timed_out":
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetID() < out[j].GetID() })
	return out, nil
}
