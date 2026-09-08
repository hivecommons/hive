package hub

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// NewAgentSummary
// ---------------------------------------------------------------------------

func TestNewAgentSummary(t *testing.T) {
	t.Run("all fields populated", func(t *testing.T) {
		started := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
		lastAct := time.Date(2025, 6, 1, 13, 30, 0, 0, time.UTC)
		act := AgentActivity{
			Paused:         true,
			NeedsLogin:     true,
			SessionMissing: true,
			StartedAt:      started,
			LastActivityAt: lastAct,
		}
		as := NewAgentSummary("scanner", "running", "auto", act)
		if as.Name != "scanner" {
			t.Errorf("Name = %q, want scanner", as.Name)
		}
		if as.State != "running" {
			t.Errorf("State = %q, want running", as.State)
		}
		if as.Mode != "auto" {
			t.Errorf("Mode = %q, want auto", as.Mode)
		}
		if !as.Paused {
			t.Error("Paused should be true")
		}
		if !as.NeedsLogin {
			t.Error("NeedsLogin should be true")
		}
		if !as.SessionMissing {
			t.Error("SessionMissing should be true")
		}
		if as.StartedAt != "2025-06-01T12:00:00Z" {
			t.Errorf("StartedAt = %q, want 2025-06-01T12:00:00Z", as.StartedAt)
		}
		if as.LastActivityAt != "2025-06-01T13:30:00Z" {
			t.Errorf("LastActivityAt = %q, want 2025-06-01T13:30:00Z", as.LastActivityAt)
		}
	})

	t.Run("zero timestamps produce empty strings", func(t *testing.T) {
		act := AgentActivity{}
		as := NewAgentSummary("quality", "stopped", "", act)
		if as.StartedAt != "" {
			t.Errorf("StartedAt = %q, want empty", as.StartedAt)
		}
		if as.LastActivityAt != "" {
			t.Errorf("LastActivityAt = %q, want empty", as.LastActivityAt)
		}
	})

	t.Run("non-UTC time is converted to UTC", func(t *testing.T) {
		loc := time.FixedZone("EST", -5*3600)
		act := AgentActivity{
			StartedAt: time.Date(2025, 3, 15, 10, 0, 0, 0, loc),
		}
		as := NewAgentSummary("sec", "running", "", act)
		if as.StartedAt != "2025-03-15T15:00:00Z" {
			t.Errorf("StartedAt = %q, want 2025-03-15T15:00:00Z (UTC conversion)", as.StartedAt)
		}
	})
}
