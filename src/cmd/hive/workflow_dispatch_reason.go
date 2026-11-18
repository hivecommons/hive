package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// workflowSkipReasonLog is the reason and provenance attributes of a skipped
// item's log line. A reason replayed from the ledger carries its age, the
// session that decided it and whether a newer hand-off has made it stale,
// so a decline from a session that is over is never read as a new refusal
// (hivecommons/hive#18916). Every other skip is returned unchanged.
func workflowSkipReasonLog(s scheduler.WorkflowDispatchSkip, dlog *scheduler.WorkflowDispatchLog, now time.Time) (reason string, attrs []any) {
	reason = s.Reason
	if s.Replayed == nil {
		return reason, nil
	}
	o := scheduler.ReplayedReasonOrigin(*s.Replayed, now, dlog.Superseded(*s.Replayed))
	if o.Stale {
		reason = "stale (not current): " + reason
	}
	attrs = []any{"reason_replayed", true, "reason_age", o.Age, "reason_session", o.Session, "reason_stale", o.Stale}
	if !o.At.IsZero() {
		attrs = append(attrs, "reason_at", o.At.UTC().Format(time.RFC3339))
	}
	return reason, attrs
}

// workflowHandoffReasonOrigin fills the provenance of a not-started
// hand-off's reason on its dashboard record. A stale reason is no longer the
// item's state, so it never retries and its next step says so.
func workflowHandoffReasonOrigin(h *dashboard.WorkflowDispatchHandoff, e scheduler.WorkflowDispatchEntry, dlog *scheduler.WorkflowDispatchLog, now time.Time) {
	o := scheduler.ReplayedReasonOrigin(e, now, dlog.Superseded(e))
	h.ReasonAge, h.ReasonSession, h.Stale = o.Age, o.Session, o.Stale
	if !o.At.IsZero() {
		at := o.At
		h.ReasonAt = &at
	}
	if o.Stale {
		h.Retrying = false
		h.Next = "a newer hand-off of this item exists; this decline is history, not its state now"
	}
}
