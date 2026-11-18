package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/scheduler"
)

func TestSkipReasonLogLabelsAReplayedDecline(t *testing.T) {
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	now := at.Add(5 * time.Hour)
	dlog, _ := scheduler.OpenWorkflowDispatchLog("")
	decided := at.Add(time.Minute)
	old := scheduler.WorkflowDispatchEntry{Key: "a", Ref: "o/r#7", Agent: "quality", At: at, Status: scheduler.WorkflowHandoffNotStarted,
		Reason: "quality declined the hand-off: old text", ReasonAt: &decided, ReasonSession: "quality@2026-11-17T01:00:00Z"}
	_ = dlog.Record(old)
	skip := scheduler.WorkflowDispatchSkip{Ref: "o/r#7", Reason: scheduler.WorkflowSkipNotStarted + ": " + old.Reason, Replayed: &old}

	reason, attrs := workflowSkipReasonLog(skip, dlog, now)
	if strings.HasPrefix(reason, "stale") || !hasAttr(attrs, "reason_stale", false) || !hasAttr(attrs, "reason_session", "quality@2026-11-17T01:00:00Z") || !hasAttr(attrs, "reason_age", "4h 59m") {
		t.Fatalf("current decline mislabelled: %q %v", reason, attrs)
	}

	_ = dlog.Record(scheduler.WorkflowDispatchEntry{Key: "b", Ref: "o/r#7", Agent: "writer", At: at.Add(2 * time.Hour), Status: scheduler.WorkflowHandoffPending})
	reason, attrs = workflowSkipReasonLog(skip, dlog, now)
	if !strings.HasPrefix(reason, "stale (not current): ") || !hasAttr(attrs, "reason_stale", true) {
		t.Fatalf("a stale decline shown as current: %q %v", reason, attrs)
	}
}

func TestSkipReasonLogLeavesOtherSkipsAlone(t *testing.T) {
	reason, attrs := workflowSkipReasonLog(scheduler.WorkflowDispatchSkip{Ref: "o/r#1", Reason: scheduler.WorkflowSkipNoAgent}, nil, time.Now())
	if reason != scheduler.WorkflowSkipNoAgent || attrs != nil {
		t.Fatalf("%q %v", reason, attrs)
	}
}

func hasAttr(attrs []any, key string, want any) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == key && attrs[i+1] == want {
			return true
		}
	}
	return false
}
