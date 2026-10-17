package automerge

import (
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageReasonSharedCIIncident prefixes the recorded reason when a red PR is
// deferred to an open [shared-ci] incident (`<!-- hive-shared-ci-<n> -->`,
// hivecommons/hive#10528). The base or a shared required check is what is
// red, so the author has nothing to fix: the PR stays out of 070/072 and gets
// no contributor nudge until the incident closes (hivecommons/hive#12606).
const stageReasonSharedCIIncident = "shared-ci-incident:"

// autoMergeReasonBehindBudgetExhausted is the self-authored sweep's reason
// for a `behind` PR it did not update only because this tick's branch-update
// budget (selfAuthoredSweepMaxBranchUpdates) was spent. The next tick retries
// update-branch; nothing is owed by the author (hivecommons/hive#12607).
const autoMergeReasonBehindBudgetExhausted = "behind-update-budget-exhausted"

// stageSharedCIIncident asks the policy which open shared-CI incident, if
// any, the PR's head is deferred to.
func stageSharedCIIncident(policy StageLabelPolicy, owner, repo string, number int, head string) int {
	if policy.SharedCIIncident == nil || number <= 0 {
		return 0
	}
	return policy.SharedCIIncident(owner+"/"+repo, number, head)
}

// sharedCIDeferredStage keeps a red PR whose failure is the shared incident's
// out of the repair stages: it reads as CI still pending (050, or 060 with an
// approve verdict), with the incident named in the recorded reason. It only
// applies when the lane's red is CI (a required check, or a red optional
// check on the head); changes requested and conflicts stay author-facing.
func sharedCIDeferredStage(o stageObservation) (stageVerdict, bool) {
	if o.SharedCIIncident <= 0 {
		return stageVerdict{}, false
	}
	if classifyCIReason(o.Reason) != ciReasonRed && o.Detail.redCheck(o.HeadSHA) == "" {
		return stageVerdict{}, false
	}
	stage := stagelabels.ReviewPending
	if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
		stage = stagelabels.Reviewed
	}
	return stageVerdict{Stage: stage, Rank: stageRankOwned, Reason: stageReasonSharedCIIncident + strconv.Itoa(o.SharedCIIncident)}, true
}

// stageReasonWaitsOnHive reports reasons that leave the PR waiting on the
// hive rather than a person (no stall): a shared-CI incident being repaired
// and a branch update deferred to the next sweep tick.
func stageReasonWaitsOnHive(reason string) bool {
	return reason == autoMergeReasonBehindBudgetExhausted || strings.HasPrefix(reason, stageReasonSharedCIIncident)
}
