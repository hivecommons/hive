package hub

import (
	"testing"
	"time"
)

// ============================================================
// heartbeat.go — core functions that previously lacked tests
// ============================================================
//
// Heartbeat state lives in package-level atomics, so every test here resets it
// BEFORE it runs as well as after — the same idiom as the other heartbeat test
// files. Several of these tests assert the ABSENCE of recorded state ("should
// not record success on 403"), which an earlier test that recorded a success
// would satisfy vacuously or fail outright depending on run order. Resetting
// only on the way out leaves a test at the mercy of whoever ran before it:
// that is what made TestSendUpgradingHeartbeat_NonSuccess fail under -shuffle
// and on CI (#5553).

// --- NewAgentSummary ---

func TestNewAgentSummary_Basic(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	act := AgentActivity{
		Paused:         true,
		NeedsLogin:     true,
		SessionMissing: false,
		StartedAt:      now,
		LastActivityAt: now.Add(5 * time.Minute),
	}
	as := NewAgentSummary("scanner", "running", "auto", act)
	if as.Name != "scanner" || as.State != "running" || as.Mode != "auto" {
		t.Errorf("basic fields wrong: %+v", as)
	}
	if !as.Paused {
		t.Error("expected Paused=true")
	}
	if !as.NeedsLogin {
		t.Error("expected NeedsLogin=true")
	}
	if as.SessionMissing {
		t.Error("expected SessionMissing=false")
	}
	if as.StartedAt != "2026-08-11T12:00:00Z" {
		t.Errorf("StartedAt = %q, want RFC3339 UTC", as.StartedAt)
	}
	if as.LastActivityAt != "2026-08-11T12:05:00Z" {
		t.Errorf("LastActivityAt = %q, want RFC3339 UTC", as.LastActivityAt)
	}
}

func TestNewAgentSummary_ZeroTimes(t *testing.T) {
	as := NewAgentSummary("quality", "idle", "", AgentActivity{})
	if as.StartedAt != "" {
		t.Errorf("expected empty StartedAt for zero time, got %q", as.StartedAt)
	}
	if as.LastActivityAt != "" {
		t.Errorf("expected empty LastActivityAt for zero time, got %q", as.LastActivityAt)
	}
}
