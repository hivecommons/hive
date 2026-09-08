package hub

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- NewAgentSummary ---

func TestNewAgentSummary_AllFields(t *testing.T) {
	started := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	lastAct := time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC)
	act := AgentActivity{
		Paused:         true,
		NeedsLogin:     true,
		SessionMissing: true,
		StartedAt:      started,
		LastActivityAt: lastAct,
		KickInterval:   4 * time.Hour,
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
	if as.StartedAt != "2026-01-15T10:30:00Z" {
		t.Errorf("StartedAt = %q, want 2026-01-15T10:30:00Z", as.StartedAt)
	}
	if as.LastActivityAt != "2026-01-15T11:00:00Z" {
		t.Errorf("LastActivityAt = %q, want 2026-01-15T11:00:00Z", as.LastActivityAt)
	}
	if as.KickIntervalSec != int64((4 * time.Hour / time.Second)) {
		t.Errorf("KickIntervalSec = %d, want 14400", as.KickIntervalSec)
	}
}

func TestNewAgentSummary_ZeroTimestamps(t *testing.T) {
	act := AgentActivity{}
	as := NewAgentSummary("quality", "stopped", "", act)
	if as.StartedAt != "" {
		t.Errorf("StartedAt should be empty for zero time, got %q", as.StartedAt)
	}
	if as.LastActivityAt != "" {
		t.Errorf("LastActivityAt should be empty for zero time, got %q", as.LastActivityAt)
	}
}

func TestNewAgentSummary_NonUTCTimestampConvertedToUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, loc)
	act := AgentActivity{StartedAt: started}
	as := NewAgentSummary("sec-check", "running", "", act)
	if !strings.HasSuffix(as.StartedAt, "Z") {
		t.Errorf("StartedAt should end with Z (UTC), got %q", as.StartedAt)
	}
	if as.StartedAt != "2026-03-01T17:00:00Z" {
		t.Errorf("StartedAt = %q, want 2026-03-01T17:00:00Z", as.StartedAt)
	}
}

// --- dashboardHost ---

func TestDashboardHost_ValidHTTPS(t *testing.T) {
	got := dashboardHost("https://my-hive.example.com/dashboard")
	if got != "my-hive.example.com" {
		t.Errorf("got %q, want my-hive.example.com", got)
	}
}

func TestDashboardHost_UpperCase(t *testing.T) {
	got := dashboardHost("https://MY-HIVE.Example.COM")
	if got != "my-hive.example.com" {
		t.Errorf("got %q, want lowercase", got)
	}
}

func TestDashboardHost_WithPort(t *testing.T) {
	got := dashboardHost("https://hive.example.com:8443/path")
	if got != "hive.example.com" {
		t.Errorf("got %q, want hive.example.com (no port)", got)
	}
}

func TestDashboardHost_Empty(t *testing.T) {
	if got := dashboardHost(""); got != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}

func TestDashboardHost_InvalidURL(t *testing.T) {
	if got := dashboardHost("://bad"); got != "" {
		t.Errorf("invalid URL: got %q, want empty", got)
	}
}

func TestDashboardHost_Whitespace(t *testing.T) {
	got := dashboardHost("  https://trimmed.example.com  ")
	if got != "trimmed.example.com" {
		t.Errorf("got %q, want trimmed.example.com", got)
	}
}
