package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An adopted-in-place PR is the hive's own fixer's to repair only while the
// hive can push its branch. projectbluefin/utah#919 (a fork the hive recorded
// fork-rehome-refused on) and projectbluefin/testsuite#1037 (a fork whose
// diff changes .github/workflows/e2e.yml; the fixer reported three times that
// GitHub refused its push) were both decided "hive's fixer: … then it merges"
// and read "the hive is fixing it" for days while nothing could be pushed.

// OwnershipReasonAdoptedPushRefused: a person answered the review on an
// adopted PR's head, but the hive cannot push that branch, so the answer
// cannot be carried out by its fixer.
const OwnershipReasonAdoptedPushRefused = "adopted_push_refused"

func init() {
	ownershipReasonVerbs[OwnershipReasonAdoptedPushRefused] = OwnershipVerbDecide
}

// adoptedPushFacts is what says whether the hive can push an adopted PR's
// branch: the PR's shape and what the hive recorded trying.
type adoptedPushFacts struct {
	Fork                bool
	MaintainerCanModify bool
	// WorkflowPath: the PR's diff against the base changes
	// .github/workflows/ (known only where the files were listed).
	WorkflowPath bool
	// RehomeRefused: the adoption engine recorded fork-rehome-refused on
	// this head (it can neither push the fork in place nor supersede it).
	RehomeRefused bool
	// Escalation is the adoption-stalled reason recorded on this head, if
	// any; Capability is the short text of a capability blocker the ledger
	// holds for the item ("App lacks workflows permission").
	Escalation string
	Capability string
	// Adopted: the stage store says the hive took this PR on (a live
	// adoption, an escalated one at this head, or a refused re-home).
	Adopted bool
}

// refusal names why the hive cannot push the branch; "" when it can.
func (f adoptedPushFacts) refusal() string {
	esc := strings.ToLower(f.Escalation)
	switch {
	case f.RehomeRefused || strings.HasSuffix(esc, "fork-rehome-refused"):
		return "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"
	case f.Fork && !f.MaintainerCanModify:
		return "the fork does not allow edits by maintainers"
	case f.Fork && f.WorkflowPath:
		return "the PR changes .github/workflows/, which the hive cannot push to a fork"
	case strings.Contains(esc, stageengineprstage.ContributorLaneReasonWorkflowPath):
		return "the adoption stalled because the PR changes .github/workflows/ (" + f.Escalation + ")"
	case f.Capability != "":
		return "the hive's fixer reported it cannot push: " + f.Capability
	}
	return ""
}

// recordedAdoptedPush fills what the stage store and the blocker ledger hold
// about pushing repo#number at head.
func recordedAdoptedPush(store *stagelabels.Store, repo string, number int, head string, f *adoptedPushFacts) {
	if store != nil {
		if rec, ok := store.Get(stagelabels.Key(repo, number)); ok {
			f.RehomeRefused = rec.ForkRehomeRefused(head)
			if rec.AdoptionEscalated(head) {
				f.Escalation = strings.TrimSpace(rec.AdoptEscalatedReason)
			}
			f.Adopted = rec.AdoptionLive(head) || rec.AdoptionEscalated(head) || f.RehomeRefused
		}
	}
	if e, ok := blockerLedger().Get(repo, number); ok && e.Blocks() && e.Blocker.Kind.IsCapability() {
		f.Capability = e.Blocker.Kind.Short()
	}
}

// adoptedPushStore is the stage store the ownership pass reads; tests swap it.
var adoptedPushStore = stagelabels.Shared

// adoptedPushRefusalFor is the ownership pass's check on a listed PR. It
// applies only to a PR the hive took on at its head: on any other PR the
// author's own agent is the one fixing. The listing carries no file names,
// so a fork's workflow-file diff counts only once the adoption or the fixer
// recorded it.
// A short repo name is qualified with org, as the decision-answer lookup does.
func adoptedPushRefusalFor(org string) func(pr github.PullRequest) string {
	return func(pr github.PullRequest) string {
		repo := strings.TrimSpace(pr.Repo)
		if !strings.Contains(repo, "/") && strings.TrimSpace(org) != "" {
			repo = strings.TrimSpace(org) + "/" + repo
		}
		f := adoptedPushFacts{Fork: pr.FromFork, MaintainerCanModify: pr.MaintainerCanModify}
		recordedAdoptedPush(adoptedPushStore(), repo, pr.Number, strings.TrimSpace(pr.HeadSHA), &f)
		if !f.Adopted {
			return ""
		}
		return f.refusal()
	}
}
