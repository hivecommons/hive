package automerge

import (
	"context"
	"errors"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewgate"
)

// changesRequestedRecorder is the transport hook that audits a gated
// attempt (hgithub.Client.RecordChangesRequestedGate), so the engine's
// refusals land on the same audit action as every other path's.
type changesRequestedRecorder interface {
	RecordChangesRequestedGate(repo string, number int, action, path string, d reviewgate.Decision, cause error)
}

// gateChangesRequested is the open change-request gate (hivecommons/hive#20067)
// on the engine's own merge and merge-queue calls: every sweep lane (trusted,
// queued / merge train, contributor, ACMM L6 auto-merge on green) merges
// through executeMergeEffect or enqueues through the native queue, and both
// ask this first. It reads the reviews at merge time, not from the sweep's
// earlier snapshot, so a change request posted while CI ran still stops the
// merge. A failed read refuses (fail closed); the next sweep retries.
func (c *Engine) gateChangesRequested(ctx context.Context, owner, repo string, number int, action string) error {
	if c == nil || c.gh == nil || number <= 0 {
		return nil
	}
	full := owner + "/" + repo
	blocks, err := changesRequestedCheck(hgithub.WithRESTCaller(ctx, "hive:automerge_changes_requested_gate"), c.gh, owner, repo, number)
	var d reviewgate.Decision
	if err == nil {
		d = reviewgate.Decide(full, number, blocks)
		if len(d.Blocks) == 0 {
			return nil
		}
	}
	if rec, ok := c.transport.(changesRequestedRecorder); ok {
		rec.RecordChangesRequestedGate(full, number, action, hgithub.PRAuditPathSweep, d, err)
	} else {
		c.warn("open change-request gate", "repo", full, "pr", number, "action", action,
			"reviewers", reviewgate.Reviewers(d.Blocks), "review_ids", reviewgate.ReviewIDs(d.Blocks), "overridden", d.Overridden, "error", err)
	}
	if err != nil {
		return &reviewgate.BlockedError{Repo: full, Number: number, Cause: err}
	}
	if d.Allowed() {
		return nil
	}
	return &reviewgate.BlockedError{Repo: full, Number: number, Blocks: d.Blocks}
}

// changesRequestedCheck reads the open change requests. It is a variable
// only so this package's older tests, whose GitHub fakes predate the gate,
// can run without serving the review listing; the gate's own tests restore
// reviewgate.CheckGitHub.
var changesRequestedCheck = reviewgate.CheckGitHub

// changesRequestedSkipReason maps a gate refusal onto the sweep's skip
// reason: an open change request is the human-changes-requested lane reason
// (the author acts next), a failed read is a read failure. ok is false for
// any other error.
func changesRequestedSkipReason(err error) (reason string, ok bool) {
	if !errors.Is(err, reviewgate.ErrChangesRequested) {
		return "", false
	}
	if errors.Is(err, reviewgate.ErrUnreadable) {
		return autoMergeReasonChangesRequestedUnreadable, true
	}
	return autoMergeReasonHumanChangesRequested, true
}

// autoMergeReasonChangesRequestedUnreadable is the skip reason when the
// gate could not read the PR's reviews.
const autoMergeReasonChangesRequestedUnreadable = "changes-requested-gate-unreadable"
