package prstage

import (
	"reflect"
	"strings"
	"testing"
)

// The held-runs reason round-trips its run ids, and the 085 cause names
// them, in the singular for one run.
func TestWorkflowRunsHeldReason(t *testing.T) {
	reason := WorkflowRunsHeldReason([]int64{44261912124, 44257752419})
	if reason != "workflow-approval-required:held-runs:44261912124,44257752419" {
		t.Fatalf("reason = %q", reason)
	}
	ids, ok := ParseWorkflowRunsHeld(reason)
	if !ok || !reflect.DeepEqual(ids, []string{"44261912124", "44257752419"}) {
		t.Fatalf("parse = %v, %v", ids, ok)
	}
	if ids, ok := ParseWorkflowRunsHeld(WorkflowRunsHeldPrefix + " 7, ,"); !ok || !reflect.DeepEqual(ids, []string{"7"}) {
		t.Fatalf("blank ids kept: %v, %v", ids, ok)
	}
	for _, other := range []string{"workflow-approval-required:ci.yml", "fork-guard:Canary", ""} {
		if _, ok := ParseWorkflowRunsHeld(other); ok {
			t.Fatalf("%q parsed as held runs", other)
		}
	}

	for _, tc := range []struct {
		ids  []string
		want string
	}{
		{ids: nil, want: "holding the workflow runs on this head"},
		{ids: []string{"7"}, want: "holding workflow run 7 on this head"},
		{ids: []string{"7", "8"}, want: "holding workflow runs 7, 8 on this head"},
	} {
		if got := CauseWorkflowRunsHeld(tc.ids); !strings.Contains(got, tc.want) || !strings.Contains(got, "Approve and run") {
			t.Fatalf("cause(%v) = %q, want it to contain %q", tc.ids, got, tc.want)
		}
	}
}
