package automerge

import (
	"strings"
	"time"

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
func (c *Engine) operatorAdmission(owner, repo string, number int, head string) string {
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
	login, ok := c.recordedHumanApprovalForHead(owner, repo, number, head)
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
// is the same person choosing re-authoring, so it is not refused.
func (c *Engine) adoptionRefusedByAdmission(store *stagelabels.Store, key string, obs stageObservation, requested bool, now time.Time) bool {
	if requested {
		return false
	}
	login := c.operatorAdmission(obs.Owner, obs.Repo, obs.Number, obs.HeadSHA)
	if login == "" {
		return false
	}
	store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedOperatorAdmission, now)
	c.info("stage labels: adoption due but a person admitted this head; not adopting", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "admitted_by", stagelabels.OperatorApprovalAdmission(login))
	return true
}
