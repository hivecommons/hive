package claude_test

import (
	"testing"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/claude/claudetest"
)

func TestAuthMode(t *testing.T) {
	claudetest.SetOperatorToken(t, "")
	if got := claude.AuthMode(); got != claude.AuthModeDashboardSignIn {
		t.Fatalf("unset: AuthMode() = %q, want %q", got, claude.AuthModeDashboardSignIn)
	}
	claudetest.SetOperatorToken(t, "sk-ant-oat01-mode")
	if got := claude.AuthMode(); got != claude.AuthModeLongLivedToken {
		t.Fatalf("set: AuthMode() = %q, want %q", got, claude.AuthModeLongLivedToken)
	}
}
