package agent

import (
	"strings"
	"testing"
)

func TestPaneShowsApprovalDenied(t *testing.T) {
	denied := `✗ Remove junk directory from scratch workspace (shell)
  │ rm -rf ~/crux-scratch/capture/junk && echo done
  └ Permission denied because the assisted approval check did not approve this request. Retry in an
    interactive session so the user can approve it, or try an alternative that does not require this
    permission. Feedback: Recursive deletion may destroy durable data that cannot be restored.
  Approval check
    └ ✗ Assisted Permissions declined
      Returned to agent.`
	tests := []struct {
		name         string
		pane         string
		wantOK       bool
		wantFeedback string
	}{
		{"denied transcript", denied, true, "Recursive deletion may destroy durable data that cannot be restored."},
		{"declined marker only", "└ ✗ Assisted Permissions declined\n  Returned to agent.", true, ""},
		{"approved", "Approval check\n  └ ✓ Assisted Permissions approved", false, ""},
		{"ordinary output", "ran go test ./...", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fb, ok := paneShowsApprovalDenied(strings.Split(tt.pane, "\n"))
			if ok != tt.wantOK || fb != tt.wantFeedback {
				t.Fatalf("got (%q, %v), want (%q, %v)", fb, ok, tt.wantFeedback, tt.wantOK)
			}
		})
	}
}
