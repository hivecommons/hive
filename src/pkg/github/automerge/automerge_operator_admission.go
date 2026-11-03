package automerge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/review"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// contributorLaneReasonOperatorApproval is the admit reason logged when a
// person's Approve admitted the head (hivecommons/hive#15136): an owner or
// trusted merger pressed Approve on the dashboard (or typed `/hive decision
// approve`) on a PR whose author no registry tier or claim marker vouches
// for. Their approval is a trust override for exactly that head: the lane
// merges it as-is after checks and soak, and the adoption engine leaves
// the PR alone while the admission stands.
const contributorLaneReasonOperatorApproval = prstage.ContributorLaneReasonOperatorApproval

// operatorAdmission reports the person whose approval admits head on
// owner/repo#number, or "" when nobody did. Two sources agree on one
// answer: the stage record's admitted_by (the dashboard writes it when the
// Approve lands) and a recorded human approval for the head by a trusted
// merger (`/hive decision approve`, or a dashboard Approve that landed
// before the record existed). The second source is written back to the
// record so the activity monitor shows the admission.
func (c *Engine) operatorAdmission(ctx context.Context, owner, repo string, number int, head string) string {
	head = strings.TrimSpace(head)
	if c == nil || head == "" {
		return ""
	}
	key := stageKey(owner, repo, number)
	store := c.stageStoreOrShared()
	if rec, ok := store.Get(key); ok {
		if by := rec.Admission(head); by != "" {
			if login := stagelabels.AdmissionLogin(by); login != "" {
				return login
			}
		}
	}
	login, ok := c.recordedHumanApprovalForHead(ctx, owner, repo, number, head)
	if !ok {
		return ""
	}
	if trusted, configured := c.isTrustedMerger(login); !configured || !trusted {
		return ""
	}
	store.NoteAdmission(key, head, stagelabels.OperatorApprovalAdmission(login), c.now())
	return login
}

// admittedByOperator reports whether a person's approval admitted the head.
func (m contributorClaimMatch) admittedByOperator() bool {
	return m.OperatorLogin != ""
}

// adoptionRefusedByAdmission is the adoption engine's answer when the head
// about to be adopted carries a person's admission (hivecommons/hive#15136):
// the PR is theirs to merge as-is, so the hive records why it stood down
// and does not take it over. An explicit Adopt request from the dashboard
// is the same person choosing re-authoring, so it is not refused. Nor is a
// head the lane cannot merge as-is (fork not editable and behind the base,
// conflicting, DCO missing): approval there means "approved to proceed",
// so the hive adopts and carries the approval to its copy rather than
// deadlocking between "merge as-is" and "cannot merge".
func (c *Engine) adoptionRefusedByAdmission(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, requested bool, now time.Time) bool {
	if requested {
		return false
	}
	login := c.operatorAdmission(ctx, obs.Owner, obs.Repo, obs.Number, obs.HeadSHA)
	if login == "" {
		return false
	}
	if reason := headNotMergeableAsIs(obs); reason != "" {
		c.info("stage labels: a person admitted this head but the hive cannot merge it as-is; adopting and carrying the approval", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "admitted_by", stagelabels.OperatorApprovalAdmission(login), "reason", reason)
		return false
	}
	store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedOperatorAdmission, now)
	c.info("stage labels: adoption due but a person admitted this head; not adopting", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "admitted_by", stagelabels.OperatorApprovalAdmission(login))
	return true
}

// headNotMergeableAsIsReasons are the lane reasons under which no approval
// makes the head land unchanged: the hive must re-author it on a branch it
// owns.
var headNotMergeableAsIsReasons = map[string]bool{
	autoMergeReasonForkNotEditable: true, "conflicting": true, "not-mergeable": true,
	"label:needs-rebase": true, stagelabels.LaneReasonDCOMissing: true,
}

// headNotMergeableAsIs names why the lane cannot merge obs's head unchanged,
// or "" when an admission alone would let it land.
func headNotMergeableAsIs(obs stageObservation) string {
	for _, r := range []string{obs.Reason, obs.LaneReason} {
		if headNotMergeableAsIsReasons[r] {
			return r
		}
	}
	if hasLabelFold(obs.Labels, "needs-rebase") {
		return "label:needs-rebase"
	}
	if obs.Detail.DCO == stagelabels.DCONo {
		return stagelabels.LaneReasonDCOMissing
	}
	return ""
}

// carryAdmissionToCopy moves a person's approval of the adopted head onto the
// hive's copy: the approval stands for the content, and the copy carries
// that content unchanged. Recorded as a decision approval on the copy's head
// so the lanes read the carried-over requires_human question as answered.
func (c *Engine) carryAdmissionToCopy(obs stageObservation, login string, superseding int, copyHead string, now time.Time) {
	if c == nil || c.humanChangeRequests == nil || login == "" || superseding <= 0 || strings.TrimSpace(copyHead) == "" {
		return
	}
	req := review.HumanChangeRequest{
		Repo: obs.Owner + "/" + obs.Repo, Number: superseding, HeadSHA: strings.TrimSpace(copyHead),
		Source: review.HumanChangeRequestSourceDecision, SourceID: int64(obs.Number), Actor: login,
		Body: fmt.Sprintf("approve (carried from #%d head %s)", obs.Number, shortSHA(obs.HeadSHA)), Kind: review.HumanChangeRequestKindApprove, RecordedAt: now,
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("stage labels: carrying the approval to the hive's copy failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding, "by", login, "error", err)
		return
	}
	c.info("stage labels: approval carried to the hive's copy", "repo", obs.Display, "pr", obs.Number, "superseded_by", superseding, "head", copyHead, "by", login)
}
