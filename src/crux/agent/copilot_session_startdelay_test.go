package agent

import (
	"testing"
	"time"
)

func TestCopilotSessionRefreshStartDelay(t *testing.T) {
	t.Setenv(CopilotSessionRefreshStartDelayEnv, "")
	if got := copilotSessionRefreshStartDelay(); got != defaultCopilotSessionRefreshStartDelay {
		t.Errorf("default = %v, want %v", got, defaultCopilotSessionRefreshStartDelay)
	}
	t.Setenv(CopilotSessionRefreshStartDelayEnv, "0s")
	if got := copilotSessionRefreshStartDelay(); got != 0 {
		t.Errorf("0s override = %v, want 0 (tests drive near-zero)", got)
	}
	t.Setenv(CopilotSessionRefreshStartDelayEnv, "5s")
	if got := copilotSessionRefreshStartDelay(); got != 5*time.Second {
		t.Errorf("5s override = %v, want 5s", got)
	}
	t.Setenv(CopilotSessionRefreshStartDelayEnv, "garbage")
	if got := copilotSessionRefreshStartDelay(); got != defaultCopilotSessionRefreshStartDelay {
		t.Errorf("invalid override should fall back, got %v", got)
	}
}
