package pauseprov

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		trigger, by, kind, contains string
	}{
		{"dashboard-api", "alice", KindDashboardUser, "dashboard user alice"},
		{"dashboard-api", "", KindDashboardUser, "a dashboard user"},
		{"provider-rotation", "", KindAutoRateLimit, "rate limit or backoff"},
		{"state-restore", "", KindStartup, "startup persisted pause"},
		{"", "", KindUnknown, UnknownLabel},
		{"hook:quiet", "", KindHook, "hook"},
		{"fleet-breaker", "", KindAuto, "auto-pause (fleet-breaker)"},
		{"startup", "", KindStartup, "startup"},
	} {
		a := Classify(c.trigger, c.by)
		if a.Kind != c.kind || !strings.Contains(a.Label, c.contains) {
			t.Errorf("Classify(%q,%q) = %+v", c.trigger, c.by, a)
		}
	}
}

func TestDescribePauseLineAndMigration(t *testing.T) {
	p := Describe(true, PauseFacts{Trigger: "dashboard-api", By: "alice", Reason: "manual pause", At: t0}, nil)
	if p.Pause.Line != "paused by dashboard user alice at 2026-11-17T12:00:00Z: manual pause" || p.LastResume != nil {
		t.Fatalf("%+v", p)
	}
	old := Describe(true, PauseFacts{}, nil)
	if old.Pause.Kind != KindUnknown || !strings.Contains(old.Pause.Line, UnknownLabel) {
		t.Fatalf("old state must read as unknown: %+v", old.Pause)
	}
	if Describe(false, PauseFacts{Trigger: "dashboard-api"}, nil) != nil {
		t.Fatal("a running agent has no pause provenance")
	}
}

func TestDescribeSanitizesReason(t *testing.T) {
	p := Describe(true, PauseFacts{Trigger: "login-detector", Reason: "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 " + strings.Repeat("x", 400), At: t0}, nil)
	if strings.Contains(p.Pause.Reason, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") || len([]rune(p.Pause.Reason)) > 200 {
		t.Fatalf("reason not sanitized: %q", p.Pause.Reason)
	}
}

func TestRefusalMessage(t *testing.T) {
	p := Describe(true, PauseFacts{Trigger: "dashboard-api", By: "alice", Reason: "manual pause", At: t0}, nil)
	want := "agent a cannot be kicked: it is paused by dashboard user alice at 2026-11-17T12:00:00Z: manual pause; resume here"
	if got := RefusalMessage("a", p); got != want {
		t.Fatalf("%q", got)
	}
	if RefusalMessage("a", nil) != "" || RefusalMessage("a", Describe(false, PauseFacts{}, &Event{Trigger: "x"})) != "" {
		t.Fatal("a running agent got a paused refusal")
	}
}

func TestResumeAttributionAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	s, _ := Open(path, nil)
	now := t0
	s.SetClock(func() time.Time { return now })
	s.IntendResume("a", "alice")
	s.ObserveResume("a", Event{Trigger: "dashboard-api", Reason: "manual resume", At: t0})
	// A later automatic resume must not borrow the earlier intent.
	s.IntendResume("b", "alice")
	now = t0.Add(time.Hour)
	s.ObserveResume("b", Event{Trigger: "acmm-pack", At: now})
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := s2.LastResume("a"), s2.LastResume("b")
	if a == nil || a.By != "alice" || b == nil || b.By != "" || s2.LastResume("c") != nil {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	d := Describe(false, PauseFacts{}, a)
	if d.LastResume.Line != "last resumed by dashboard user alice at 2026-11-17T12:00:00Z" {
		t.Fatalf("%+v", d.LastResume)
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.IntendResume("a", "x")
	s.ObserveResume("a", Event{})
	if s.LastResume("a") != nil {
		t.Fatal("nil store remembered")
	}
}
