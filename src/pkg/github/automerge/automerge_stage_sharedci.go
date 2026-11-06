package automerge

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageReasonSharedCIIncident prefixes the recorded reason when a red PR is
// deferred to an open [shared-ci] incident (`<!-- hive-shared-ci-<n> -->`,
// hivecommons/hive#10528). The base or a shared required check is what is
// red, so the author has nothing to fix: the PR stays out of 070/072 and gets
// no contributor nudge until the incident closes (hivecommons/hive#12606).
const stageReasonSharedCIIncident = prstage.StageReasonSharedCIIncident

// stageReasonBaseBranchRed prefixes the recorded reason when every failing
// check on a red PR head is also red on the base branch's tip and the PR
// touches no CI-owning path (hgithub.PullRequest.AllFailuresBaseRed). The
// checks follow, comma-separated. The base branch is what is red, so the
// author has nothing to fix: the PR stays out of 070/071/072, gets no
// contributor nudge and no fix kick, and is handed back as usual once the
// base recovers and the head is still red.
const stageReasonBaseBranchRed = prstage.StageReasonBaseBranchRed

// stageReasonForkGuard prefixes the recorded reason when every failing check
// on a fork PR head refuses it only because it is from a fork
// (hgithub.PullRequest.ForkGuardChecks). The checks follow, comma-separated.
// Only a maintainer can clear such a guard, so the PR is parked at 085 and
// never nudged or adopted (hivecommons/hive#14273).
const stageReasonForkGuard = prstage.StageReasonForkGuard

// autoMergeReasonBehindBudgetExhausted is the self-authored sweep's reason
// for a `behind` PR it did not update only because this tick's branch-update
// budget (selfAuthoredSweepMaxBranchUpdates) was spent. The next tick retries
// update-branch; nothing is owed by the author (hivecommons/hive#12607).
const autoMergeReasonBehindBudgetExhausted = prstage.AutoMergeReasonBehindBudgetExhausted

// stageSharedCIIncident asks the policy which open shared-CI incident, if
// any, the PR's head is deferred to.
func stageSharedCIIncident(policy StageLabelPolicy, owner, repo string, number int, head string) int {
	if policy.SharedCIIncident == nil || number <= 0 {
		return 0
	}
	return policy.SharedCIIncident(owner+"/"+repo, number, head)
}

// stageBaseBranchRed asks the policy which failing checks on the PR's head
// are the base branch's; empty unless all of them are.
func stageBaseBranchRed(policy StageLabelPolicy, owner, repo string, number int, head string) []string {
	if policy.BaseBranchRed == nil || number <= 0 {
		return nil
	}
	return policy.BaseBranchRed(owner+"/"+repo, number, head)
}

// stageForkGuard asks the policy which failing checks on the PR's head
// refuse it only because it is from a fork; empty unless all of them do.
func stageForkGuard(policy StageLabelPolicy, owner, repo string, number int, head string) []string {
	if policy.ForkGuard == nil || number <= 0 {
		return nil
	}
	return policy.ForkGuard(owner+"/"+repo, number, head)
}

// foldForkGuard keeps a fork PR's fork-guard evidence across heads and
// sweeps. The policy answers only for the head the last CI-enriched
// enumeration saw, so every new head (and every snapshot that dropped the
// PR) read as plain red CI: 072, a nudge telling the author to fix the
// guard, and after the grace an adoption — before the next enumeration
// parked the PR at 085 again (projectbluefin/utah-packages#862, #869).
//
// Fresh evidence is remembered on the record. Without it, a fork head whose
// every red check — from the head's own check digest, not the lane's
// reason, which names one check — is one the PR's guard refused before is
// read as the same guard, so it goes straight to the 085 park. A head with
// any other red check keeps the author-facing route.
func (c *Engine) foldForkGuard(ctx context.Context, tracker *StageTracker, store *stagelabels.Store, list []stageObservation, now time.Time) {
	var o *stageObservation
	for i := range list {
		if len(list[i].ForkGuardChecks) > 0 {
			store.NoteForkGuard(stageKey(list[i].Owner, list[i].Repo, list[i].Number), list[i].ForkGuardChecks)
			return
		}
		if o == nil && strings.TrimSpace(list[i].HeadSHA) != "" {
			o = &list[i]
		}
	}
	if o == nil || o.Merged || o.HeadRepo == "" || headInBaseRepo(*o) {
		return
	}
	key := stageKey(o.Owner, o.Repo, o.Number)
	rec, ok := store.Get(key)
	if !ok || len(rec.ForkGuardChecks) == 0 {
		return
	}
	head := o.HeadSHA
	red := false
	for _, l := range list {
		if l.HeadSHA == head && (classifyCIReason(l.Reason) == ciReasonRed || l.Detail.RedCheck(head) != "") {
			red = true
			break
		}
	}
	if !red {
		return
	}
	summary, known := c.headChecks(ctx, tracker, key, *o, now)
	if !known || len(summary.Red) == 0 {
		return
	}
	for _, name := range summary.Red {
		if !slices.Contains(rec.ForkGuardChecks, name) {
			return
		}
	}
	for i := range list {
		if list[i].HeadSHA == head {
			list[i].ForkGuardChecks = append([]string(nil), summary.Red...)
		}
	}
	c.debug("stage labels: fork guard carried to a head the enumeration has not classified", "repo", o.Display, "pr", o.Number, "head", head, "checks", summary.Red)
}

func baseBranchRedReason(checks []string) string { return stageengine.BaseBranchRedReason(checks) }

// stageReasonWaitsOnHive reports reasons that leave the PR waiting on the
// hive rather than a person (no stall): a shared-CI incident being repaired,
// a red base branch, a branch update deferred to the next sweep tick, a
// merge-train slot behind the front (queued or being rebased by the hive)
// and a base refresh the hive is running. None of those is anything the
// author can act on, so none may read as stage/stalled.
func stageReasonWaitsOnHive(reason string) bool {
	if reason == autoMergeReasonBehindBudgetExhausted || strings.HasPrefix(reason, stageReasonSharedCIIncident) ||
		strings.HasPrefix(reason, stageReasonBaseBranchRed) || strings.HasPrefix(reason, basePreparationPrefix) {
		return true
	}
	if rest, ok := strings.CutPrefix(strings.TrimSpace(reason), prstage.MergeTrainReasonPrefix); ok {
		state, _, _ := strings.Cut(rest, "#")
		switch strings.TrimSpace(state) {
		case prstage.MergeTrainStateQueuedBehind, prstage.MergeTrainStateRebasing:
			return true
		}
	}
	return false
}
