package github

import (
	"strings"
	"testing"
)

type recordingMergeAlertSink struct {
	adds   []SystemAlertLike
	clears []string
}

type SystemAlertLike struct {
	id, severity, message string
}

func (s *recordingMergeAlertSink) AddSystemAlert(id, severity, message string) {
	s.adds = append(s.adds, SystemAlertLike{id: id, severity: severity, message: message})
}

func (s *recordingMergeAlertSink) ClearSystemAlert(id string) {
	s.clears = append(s.clears, id)
}

func TestClassifyMergeFailureForOperator(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want []string
	}{
		{
			name: "app permission",
			err:  "merging PR o/r#1 (squash): PUT https://api.github.test: 403 Resource not accessible by integration []",
			want: []string{"Grant the hivecommons App Contents & Pull requests write on o/r", "install the App on the repo"},
		},
		{
			name: "review required",
			err:  "GH006: Protected branch update failed: At least 1 approving review is required by reviewers with write access.",
			want: []string{"requires reviews/approvals", "Add the App to the bypass list", "approve the PR"},
		},
		{
			name: "missing check",
			err:  "ci gate: required check(s) not yet reported on abc123: build-gate, lint (still pending after 360 ticks)",
			want: []string{"required status check(s) never reported: build-gate, lint", "auto_merge.required_checks"},
		},
		{
			name: "fork approval permission",
			err:  "ci gate: fork PR workflow runs are awaiting maintainer approval (action_required): \"CI\"(41). Approve the runs or relax the repo setting at https://github.com/o/r/settings/actions (Approval for running fork pull request workflows)",
			want: []string{"fork PR workflow runs are awaiting maintainer approval", "https://github.com/o/r/settings/actions", "Approve the runs manually"},
		},
		{
			name: "merge method",
			err:  "Validation Failed: squash commits are not allowed for this repository",
			want: []string{"requested merge method is not allowed", "Enable squash/merge"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alert, ok := classifyMergeFailureForOperator("o/r", tc.err)
			if !ok {
				t.Fatalf("expected operator alert for %q", tc.err)
			}
			for _, want := range tc.want {
				if !strings.Contains(alert.message, want) {
					t.Fatalf("alert %q missing %q", alert.message, want)
				}
			}
		})
	}
}

func TestClassifyMergeFailureSuppressesNonActionable(t *testing.T) {
	for _, msg := range []string{
		"secondary rate limit exceeded",
		"merge conflict: not mergeable",
	} {
		if alert, ok := classifyMergeFailureForOperator("o/r", msg); ok {
			t.Fatalf("expected no operator alert for %q, got %+v", msg, alert)
		}
	}
}

func TestMergeFailureAlertsDedupeAndClearOnSuccess(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, nil)
	sink := &recordingMergeAlertSink{}
	c.SetMergeFailureAlertSink(sink)
	errMsg := "403 Resource not accessible by integration"

	c.raiseMergeFailureAlert("o/r", errMsg)
	c.raiseMergeFailureAlert("o/r", errMsg)
	if len(sink.adds) != 2 {
		t.Fatalf("sink should receive idempotent updates for active alert, got %d", len(sink.adds))
	}
	if sink.adds[0].id != sink.adds[1].id {
		t.Fatalf("duplicate reason should reuse alert id: %+v", sink.adds)
	}
	if sink.adds[0].severity != "error" {
		t.Fatalf("severity = %q, want error", sink.adds[0].severity)
	}

	c.clearMergeFailureAlertsForRepo("o/r")
	if len(sink.clears) != 1 || sink.clears[0] != sink.adds[0].id {
		t.Fatalf("clear on success = %+v, want %q", sink.clears, sink.adds[0].id)
	}
	c.clearMergeFailureAlertsForRepo("o/r")
	if len(sink.clears) != 1 {
		t.Fatalf("second clear should be no-op, got %+v", sink.clears)
	}
}
