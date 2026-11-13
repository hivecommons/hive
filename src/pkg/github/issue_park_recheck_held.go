package github

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The park re-check asks the write-back gate about its note on every pass
// (every issues.hard_suppress_reconcile_interval), and a refusal used to be
// silent: the pass logged "posted 0" and nothing said which issue was held
// or why. A person-opened issue a contributor relay parked sat for hours
// with its blocker merged, no note, and no line in the log to explain it.
//
// A refused note is now logged, once per issue and reason. The gate keeps
// the reason it last refused each issue for (writeback.Gate.NoteHeld, as the
// blocked sweep's repair uses it); these helpers keep that memory in step
// with the re-check. Repeats go to the debug log, and a hold is news again
// after the note went through, the issue stopped needing one, or it left
// the park.

// parkRecheckHeldRepo is owner/repo in the form the gate keys its hold
// memory by (lower-cased, trimmed).
func parkRecheckHeldRepo(owner, repo string) string {
	return strings.ToLower(strings.TrimSpace(owner + "/" + repo))
}

// parkRecheckHeldKey names one parked issue for retainParkRecheckHeld.
func parkRecheckHeldKey(owner, repo string, number int) string {
	return parkRecheckHeldRepo(owner, repo) + "#" + strconv.Itoa(number)
}

// noteParkRecheckHeld logs that the gate refused the re-check note on
// owner/repo#number: at Info the first time and whenever the reason changes,
// at Debug otherwise. parkedBy is the login that applied the park's label
// and hiveApplied whether the re-check counted it as the hive's, so a
// "needs-opt-in-label" hold shows whose park it was.
func (c *Client) noteParkRecheckHeld(ctx context.Context, owner, repo string, number int, parkedBy string, hiveApplied bool, d writeback.Decision) {
	level := slog.LevelDebug
	if c.WritebackGate().NoteHeld(owner+"/"+repo, number, writeback.KindNeedsDecisionComment, d.Reason) {
		level = slog.LevelInfo
	}
	if c.logger == nil {
		return
	}
	c.logger.Log(ctx, level, "park re-check note held by writeback gate",
		"repo", repo, "issue", number, "reason", d.Reason, "detail", d.Detail,
		"parked_by", parkedBy, "hive_applied_park", hiveApplied)
}

// forgetParkRecheckHeld drops the hold remembered for owner/repo#number: the
// gate allowed the note, the note on the issue already says what a new one
// would, or the issue has nothing to report. A later hold is logged again.
func (c *Client) forgetParkRecheckHeld(owner, repo string, number int) {
	c.WritebackGate().ForgetHeld(owner+"/"+repo, number, writeback.KindNeedsDecisionComment)
}

// retainParkRecheckHeld forgets the holds of issues that parked, the open
// parked issues read this pass, no longer contains: an issue that was closed
// or un-parked never reaches the re-check again. That bounds the memory by
// the open parked issues of the repos the re-check reads. A repo whose
// listing failed this pass (not in listed) keeps its holds, so one failed
// read does not make every hold in that repo news again. The re-check is the
// only writer that remembers holds under needs_decision_comment.
func (c *Client) retainParkRecheckHeld(listed, parked map[string]bool) {
	gate := c.WritebackGate()
	if gate == nil {
		return
	}
	gate.RetainHeld(writeback.KindNeedsDecisionComment, func(repo string, number int) bool {
		return !listed[repo] || parked[repo+"#"+strconv.Itoa(number)]
	})
}
