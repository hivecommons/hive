package hub

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// ── dashboardHost ────────────────────────────────────────────────────────

func TestDashboardHost(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"https with path", "https://hive.example.com/dashboard", "hive.example.com"},
		{"https bare", "https://hive.example.com", "hive.example.com"},
		{"http", "http://local.dev:8080/api", "local.dev"},
		{"uppercase host", "https://HIVE.EXAMPLE.COM", "hive.example.com"},
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"no scheme", "hive.example.com", ""},
		{"invalid url", "://bad", ""},
		{"leading/trailing spaces", "  https://hive.example.com  ", "hive.example.com"},
		{"with port", "https://hive.example.com:9443", "hive.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashboardHost(tt.url)
			if got != tt.want {
				t.Errorf("dashboardHost(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// ── terseProbeError ──────────────────────────────────────────────────────

func TestTerseProbeError(t *testing.T) {
	t.Run("nil error returns empty", func(t *testing.T) {
		if got := terseProbeError(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("short message is preserved", func(t *testing.T) {
		err := errors.New("connection refused")
		got := terseProbeError(err)
		if got != "connection refused" {
			t.Errorf("got %q, want %q", got, "connection refused")
		}
	})

	t.Run("long message is truncated", func(t *testing.T) {
		// Build a string >180 runes.
		long := make([]byte, 300)
		for i := range long {
			long[i] = 'x'
		}
		err := errors.New(string(long))
		got := terseProbeError(err)
		runes := []rune(got)
		if len(runes) != 180 {
			t.Errorf("truncated length = %d runes, want 180", len(runes))
		}
		if runes[len(runes)-1] != '…' {
			t.Errorf("truncated message should end with '…', got %q", string(runes[len(runes)-1]))
		}
	})

	t.Run("exactly 180 runes is not truncated", func(t *testing.T) {
		msg := make([]byte, 180)
		for i := range msg {
			msg[i] = 'a'
		}
		err := errors.New(string(msg))
		got := terseProbeError(err)
		if got != string(msg) {
			t.Error("message of exactly 180 chars should not be truncated")
		}
	})
}

// ── publicURLSelfCheckStatusForError ─────────────────────────────────────

func TestPublicURLSelfCheckStatusForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error",
			err:  nil,
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "DNS error",
			err:  &net.DNSError{Err: "no such host", Name: "bad.example.com"},
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "no such host string",
			err:  errors.New("dial tcp: lookup bad.example.com: no such host"),
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "temporary failure in name resolution",
			err:  errors.New("temporary failure in name resolution"),
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "server misbehaving",
			err:  errors.New("server misbehaving"),
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: PublicURLSelfCheckUnknown,
		},
		{
			name: "connection refused is a real failure",
			err:  errors.New("connection refused"),
			want: PublicURLSelfCheckFail,
		},
		{
			name: "TLS handshake error is a real failure",
			err:  errors.New("tls: handshake failure"),
			want: PublicURLSelfCheckFail,
		},
		{
			name: "generic error is a failure",
			err:  errors.New("something unexpected"),
			want: PublicURLSelfCheckFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := publicURLSelfCheckStatusForError(tt.err)
			if got != tt.want {
				t.Errorf("publicURLSelfCheckStatusForError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// ── gatedPublicURLSelfCheck ──────────────────────────────────────────────

func TestGatedPublicURLSelfCheck(t *testing.T) {
	t.Run("ok resets failures and returns raw", func(t *testing.T) {
		failures := 5
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 200}

		got := gatedPublicURLSelfCheck(raw, &failures, &stable)

		if got.Status != PublicURLSelfCheckOK {
			t.Errorf("status = %q, want ok", got.Status)
		}
		if failures != 0 {
			t.Errorf("failures = %d, want 0", failures)
		}
		if stable == nil {
			t.Error("stable should be set after an OK result")
		}
	})

	t.Run("first failure returns unknown when no stable exists", func(t *testing.T) {
		failures := 0
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "refused"}

		got := gatedPublicURLSelfCheck(raw, &failures, &stable)

		if got.Status != PublicURLSelfCheckUnknown {
			t.Errorf("status = %q, want unknown (first failure, no stable)", got.Status)
		}
		if failures != 1 {
			t.Errorf("failures = %d, want 1", failures)
		}
	})

	t.Run("failure below threshold returns stable when available", func(t *testing.T) {
		failures := 1
		stable := &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 200}
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "refused"}

		got := gatedPublicURLSelfCheck(raw, &failures, &stable)

		if got.Status != PublicURLSelfCheckOK {
			t.Errorf("status = %q, want ok (returned stable)", got.Status)
		}
		if failures != 2 {
			t.Errorf("failures = %d, want 2", failures)
		}
	})

	t.Run("failure at threshold returns raw failure", func(t *testing.T) {
		// publicURLSelfCheckMinConsecutiveFailures is 3, so at failure count 2
		// (incrementing to 3) it should return the raw failure.
		failures := 2
		stable := &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 200}
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "refused"}

		got := gatedPublicURLSelfCheck(raw, &failures, &stable)

		if got.Status != PublicURLSelfCheckFail {
			t.Errorf("status = %q, want fail (threshold reached)", got.Status)
		}
		if failures != 3 {
			t.Errorf("failures = %d, want 3", failures)
		}
	})

	t.Run("unknown status resets failures", func(t *testing.T) {
		failures := 2
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown}

		got := gatedPublicURLSelfCheck(raw, &failures, &stable)

		if got.Status != PublicURLSelfCheckUnknown {
			t.Errorf("status = %q, want unknown", got.Status)
		}
		if failures != 0 {
			t.Errorf("failures = %d, want 0", failures)
		}
	})
}

// ── NewAgentSummary ──────────────────────────────────────────────────────

func TestNewAgentSummary(t *testing.T) {
	t.Run("zero times produce empty strings", func(t *testing.T) {
		as := NewAgentSummary("scanner", "running", "copilot", AgentActivity{})
		if as.Name != "scanner" || as.State != "running" || as.Mode != "copilot" {
			t.Error("basic fields not set correctly")
		}
		if as.StartedAt != "" {
			t.Errorf("StartedAt = %q, want empty for zero time", as.StartedAt)
		}
		if as.LastActivityAt != "" {
			t.Errorf("LastActivityAt = %q, want empty for zero time", as.LastActivityAt)
		}
	})

	t.Run("non-zero times serialize as RFC3339 UTC", func(t *testing.T) {
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.FixedZone("EST", -5*3600))
		as := NewAgentSummary("quality", "running", "claude", AgentActivity{
			StartedAt:      now,
			LastActivityAt: now.Add(5 * time.Minute),
		})
		if as.StartedAt != "2026-08-01T17:00:00Z" {
			t.Errorf("StartedAt = %q, want UTC conversion", as.StartedAt)
		}
		wantActivity := "2026-08-01T17:05:00Z"
		if as.LastActivityAt != wantActivity {
			t.Errorf("LastActivityAt = %q, want %q", as.LastActivityAt, wantActivity)
		}
	})

	t.Run("activity flags propagated", func(t *testing.T) {
		as := NewAgentSummary("scanner", "running", "", AgentActivity{
			Paused:         true,
			NeedsLogin:     true,
			SessionMissing: true,
		})
		if !as.Paused {
			t.Error("Paused not propagated")
		}
		if !as.NeedsLogin {
			t.Error("NeedsLogin not propagated")
		}
		if !as.SessionMissing {
			t.Error("SessionMissing not propagated")
		}
	})
}

// ── storeUnixOrZero / LastHeartbeatSuccess / LastHeartbeatAttempt ─────────

func TestStoreUnixOrZero(t *testing.T) {
	t.Run("zero time stores 0", func(t *testing.T) {
		var dst atomic.Int64
		storeUnixOrZero(&dst, time.Time{})
		if dst.Load() != 0 {
			t.Errorf("got %d, want 0", dst.Load())
		}
	})

	t.Run("non-zero time stores unix seconds", func(t *testing.T) {
		var dst atomic.Int64
		ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		storeUnixOrZero(&dst, ts)
		if dst.Load() != ts.Unix() {
			t.Errorf("got %d, want %d", dst.Load(), ts.Unix())
		}
	})
}

func TestLastHeartbeatAccessors(t *testing.T) {
	// Save and restore global state.
	origSuccess := lastHeartbeatSuccessUnix.Load()
	origAttempt := lastHeartbeatAttemptUnix.Load()
	defer func() {
		lastHeartbeatSuccessUnix.Store(origSuccess)
		lastHeartbeatAttemptUnix.Store(origAttempt)
	}()

	t.Run("zero returns ok=false", func(t *testing.T) {
		lastHeartbeatSuccessUnix.Store(0)
		lastHeartbeatAttemptUnix.Store(0)
		if _, ok := LastHeartbeatSuccess(); ok {
			t.Error("expected ok=false for zero success")
		}
		if _, ok := LastHeartbeatAttempt(); ok {
			t.Error("expected ok=false for zero attempt")
		}
	})

	t.Run("non-zero returns correct time and ok=true", func(t *testing.T) {
		now := time.Now()
		lastHeartbeatSuccessUnix.Store(now.Unix())
		lastHeartbeatAttemptUnix.Store(now.Unix())

		gotS, okS := LastHeartbeatSuccess()
		if !okS {
			t.Error("expected ok=true for non-zero success")
		}
		if gotS.Unix() != now.Unix() {
			t.Errorf("success time = %v, want %v", gotS.Unix(), now.Unix())
		}

		gotA, okA := LastHeartbeatAttempt()
		if !okA {
			t.Error("expected ok=true for non-zero attempt")
		}
		if gotA.Unix() != now.Unix() {
			t.Errorf("attempt time = %v, want %v", gotA.Unix(), now.Unix())
		}
	})
}

// ── clonePublicURLSelfCheck ──────────────────────────────────────────────

func TestClonePublicURLSelfCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := clonePublicURLSelfCheck(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("clone is a deep copy", func(t *testing.T) {
		orig := &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: 200}
		clone := clonePublicURLSelfCheck(orig)
		if clone == orig {
			t.Error("clone should be a different pointer")
		}
		clone.Status = PublicURLSelfCheckFail
		if orig.Status != PublicURLSelfCheckOK {
			t.Error("modifying clone should not affect original")
		}
	})
}

// ── cloneRouteExistenceCheck ─────────────────────────────────────────────

func TestCloneRouteExistenceCheck(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		if got := cloneRouteExistenceCheck(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("clone is a deep copy", func(t *testing.T) {
		orig := &RouteExistenceCheck{Status: RouteExistenceFound, Host: "hive.example.com"}
		clone := cloneRouteExistenceCheck(orig)
		if clone == orig {
			t.Error("clone should be a different pointer")
		}
		clone.Status = RouteExistenceMissing
		if orig.Status != RouteExistenceFound {
			t.Error("modifying clone should not affect original")
		}
	})
}
