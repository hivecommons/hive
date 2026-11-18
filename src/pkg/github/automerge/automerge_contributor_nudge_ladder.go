package automerge

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// One notification per head (hivecommons/hive#18872). A contributor used to
// get "this PR is back in your court" and, 16 to 39 minutes later, the
// adoption note for the same head: the nudge asked for work the author no
// longer got to do. The contributor ladder (pkg/delegatedecision, rung 5)
// supersedes a red, conflicting or DCO-missing head of an author whose relay
// is not connected without waiting for any grace, and those are the very
// heads the nudge went out for.
//
// The hive now picks one outcome per head before anyone is notified:
//
//   - a head the ladder would supersede at once gets no nudge comment. The
//     relay still gets the fix-pr task (a queue entry, not a GitHub
//     notification) and the adoption clock starts as if nudged, so the one
//     comment the author sees is the adoption note;
//   - a head that was nudged keeps its grace: the ladder recommends waiting
//     (delegatedecision.RuleNudgeGrace) until NudgeGracePending says the
//     shortest adoption wait since the nudge has run out.

// ladderSupersedesNudgeHead reports whether the contributor ladder would
// adopt this head in supersede mode right now, so the nudge is withheld.
// It only says yes where the adoption engine would really follow through:
// adoption is on, the author's relay is registered here and not connected
// (an author with no relay here has only the GitHub comment to learn from,
// and without a presence source nobody knows), no hold label or recorded
// adoption block, not a person's PR and not a fork-guard-only head.
func ladderSupersedesNudgeHead(rec stagelabels.Record, obs stageObservation, policy StageLabelPolicy, checks []string, presence ContributorPresence, hasPresence bool) bool {
	if !policy.Adoption || !hasPresence || !presence.Registered || presence.Online {
		return false
	}
	if obs.HeadSHA == "" || adoptionBlocked(obs.Labels) != "" || rec.AdoptionBlocked(obs.HeadSHA) != "" || personAuthored(obs, policy) || adoptionForkGuardOnly(obs) {
		return false
	}
	return delegatedecision.SupersedesImmediately(nudgeLadderInput(rec, obs, checks))
}

// nudgeLadderInput is the ladder's view of a stage-072 observation: the
// same facts the dashboard gathers for "Let hive decide", read from the
// lane's reason, its routed evidence and the failing checks the nudge would
// list. The caller has already established the author's relay is offline.
func nudgeLadderInput(rec stagelabels.Record, obs stageObservation, checks []string) delegatedecision.Input {
	reason := strings.ToLower(strings.TrimSpace(obs.Reason))
	in := delegatedecision.Input{
		Kind: delegatedecision.KindPR, Repo: obs.Display, Number: obs.Number, HeadSHA: obs.HeadSHA,
		Labels: obs.Labels, Author: obs.Author, Reasons: []string{obs.Reason},
		AuthorTooling: delegatedecision.AuthorToolingClanker,
		CIGreen:       len(checks) == 0,
		Conflicting:   nudgeHeadConflicting(obs),
		DCOMissing:    obs.Detail.DCO == stagelabels.DCONo || reason == stagelabels.LaneReasonDCOMissing,
		// A new head since the last nudge (or one pushed over an in-place
		// adoption) is the author's own push: they picked the rework up.
		ReworkPickedUp:    (rec.NudgeHead != "" && rec.NudgeHead != obs.HeadSHA) || (rec.YieldedHead != "" && rec.YieldedHead == obs.HeadSHA),
		ReporterTrustHold: reason == contributorLaneReasonReporterTrustHold,
		WorkflowPath:      strings.Contains(reason, contributorLaneReasonWorkflowPath),
	}
	return in
}

// nudgeHeadConflicting reads a merge conflict off the lane reason or the
// routed evidence, the same signals nudgeDiagnosisRouted turns into the
// "needs a rebase" ask.
func nudgeHeadConflicting(obs stageObservation) bool {
	switch strings.TrimSpace(obs.Reason) {
	case "conflicting", "not-mergeable", "label:needs-rebase":
		return true
	}
	if obs.Route != nil {
		for _, r := range obs.Route.Reasons {
			if strings.HasPrefix(r, "merge conflict") {
				return true
			}
		}
	}
	return false
}

// NudgeGracePending reports whether the author was nudged on head (a
// posted comment, not a withheld one) and the shortest adoption wait the
// clock allows since that nudge has not run out. The dashboard hands it to
// the ladder (delegatedecision.Input.NudgeGracePending) so a nudged head is
// not superseded before the author had a real chance (hivecommons/hive#18872).
func NudgeGracePending(rec stagelabels.Record, head string, clock AdoptionClock, now time.Time) bool {
	start := rec.NudgeStart()
	if head == "" || rec.NudgeHead != head || rec.NudgeCount == 0 || start.IsZero() || rec.NudgeWithheld(head) {
		return false
	}
	if rec.Adopted(head) || rec.AdoptedInPlace() {
		return false
	}
	return now.Before(start.Add(clock.minimumWait()))
}

// minimumWait is the shortest wait any author gets after a nudge before
// automatic adoption: the "minimum age since the nudge" of
// hivecommons/hive#18872.
func (c AdoptionClock) minimumWait() time.Duration {
	return min(c.graceOffline(), c.afterOffline(), c.afterUnregistered())
}
