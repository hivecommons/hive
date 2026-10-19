package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// Plain login must never request a GitHub OAuth scope (#1927); only the
// explicit feedback opt-in maps to the narrow public_repo grant.
func TestDeviceFlowScopeFor(t *testing.T) {
	for in, want := range map[string]string{
		"":          "",
		"login":     "",
		"repo":      "",
		"feedback":  github.DeviceScopeFeedback,
		" Feedback": github.DeviceScopeFeedback,
	} {
		if got := deviceFlowScopeFor(in); got != want {
			t.Errorf("deviceFlowScopeFor(%q) = %q, want %q", in, got, want)
		}
	}
}
