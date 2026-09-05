package spoke

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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

// --- terseProbeError ---

func TestTerseProbeError_Nil(t *testing.T) {
	if got := terseProbeError(nil); got != "" {
		t.Errorf("terseProbeError(nil) = %q, want empty", got)
	}
}

func TestTerseProbeError_Short(t *testing.T) {
	err := errors.New("connection refused")
	if got := terseProbeError(err); got != "connection refused" {
		t.Errorf("terseProbeError = %q, want %q", got, "connection refused")
	}
}

func TestTerseProbeError_ExactlyAtLimit(t *testing.T) {
	msg := strings.Repeat("x", 180)
	err := errors.New(msg)
	got := terseProbeError(err)
	if got != msg {
		t.Errorf("expected untruncated 180-char message")
	}
}

func TestTerseProbeError_TruncatesLongMessages(t *testing.T) {
	msg := strings.Repeat("a", 200)
	err := errors.New(msg)
	got := terseProbeError(err)
	runes := []rune(got)
	if len(runes) != 180 {
		t.Errorf("expected 180 runes, got %d", len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated message should end with …")
	}
}

// --- publicURLSelfCheckStatusForError ---

func TestPublicURLSelfCheckStatusForError_NilReturnsUnknown(t *testing.T) {
	if got := publicURLSelfCheckStatusForError(nil); got != PublicURLSelfCheckUnknown {
		t.Errorf("got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_DNSError(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "example.com"}
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckUnknown {
		t.Errorf("DNSError: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_NoSuchHost(t *testing.T) {
	err := errors.New("dial tcp: lookup bad.host: no such host")
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckUnknown {
		t.Errorf("no such host: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_TemporaryNameResolution(t *testing.T) {
	err := errors.New("dial tcp: temporary failure in name resolution")
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckUnknown {
		t.Errorf("temp DNS failure: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_DeadlineExceeded(t *testing.T) {
	if got := publicURLSelfCheckStatusForError(context.DeadlineExceeded); got != PublicURLSelfCheckUnknown {
		t.Errorf("deadline exceeded: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_ConnectionRefused(t *testing.T) {
	err := errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckFail {
		t.Errorf("connection refused: got %q, want %q", got, PublicURLSelfCheckFail)
	}
}

func TestPublicURLSelfCheckStatusForError_ServerMisbehaving(t *testing.T) {
	err := errors.New("dial tcp: server misbehaving")
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckUnknown {
		t.Errorf("server misbehaving: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

func TestPublicURLSelfCheckStatusForError_Port53Timeout(t *testing.T) {
	err := errors.New("read udp 10.0.0.1:53: i/o timeout")
	if got := publicURLSelfCheckStatusForError(err); got != PublicURLSelfCheckUnknown {
		t.Errorf(":53 i/o timeout: got %q, want %q", got, PublicURLSelfCheckUnknown)
	}
}

// --- clonePublicURLSelfCheck ---

func TestClonePublicURLSelfCheck_Nil(t *testing.T) {
	if got := clonePublicURLSelfCheck(nil); got != nil {
		t.Error("clonePublicURLSelfCheck(nil) should be nil")
	}
}

func TestClonePublicURLSelfCheck_Independence(t *testing.T) {
	orig := &PublicURLSelfCheck{Status: "ok", CheckedAt: "2026-01-01T00:00:00Z"}
	clone := clonePublicURLSelfCheck(orig)
	clone.Status = "fail"
	if orig.Status != "ok" {
		t.Error("mutating clone should not affect original")
	}
}

// --- cloneRouteExistenceCheck ---

func TestCloneRouteExistenceCheck_Nil(t *testing.T) {
	if got := cloneRouteExistenceCheck(nil); got != nil {
		t.Error("cloneRouteExistenceCheck(nil) should be nil")
	}
}

func TestCloneRouteExistenceCheck_Independence(t *testing.T) {
	orig := &RouteExistenceCheck{Status: "found", Host: "h.example.com"}
	clone := cloneRouteExistenceCheck(orig)
	clone.Status = "missing"
	if orig.Status != "found" {
		t.Error("mutating clone should not affect original")
	}
}

// --- gatedPublicURLSelfCheck ---

func TestGatedPublicURLSelfCheck_OKResetsFailures(t *testing.T) {
	failures := 5
	var stable *PublicURLSelfCheck
	raw := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: "now"}
	result := gatedPublicURLSelfCheck(raw, &failures, &stable)
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after OK", failures)
	}
	if result.Status != PublicURLSelfCheckOK {
		t.Errorf("status = %q, want ok", result.Status)
	}
	if stable == nil || stable.Status != PublicURLSelfCheckOK {
		t.Error("stable should be set to the OK result")
	}
}

func TestGatedPublicURLSelfCheck_FailBelowThresholdReturnsStable(t *testing.T) {
	failures := 0
	stableVal := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: "prev"}
	stable := &stableVal
	raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "refused"}
	result := gatedPublicURLSelfCheck(raw, &failures, &stable)
	if failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
	// Should return the stable (previous OK) value
	if result.Status != PublicURLSelfCheckOK {
		t.Errorf("status = %q, want ok (stable fallback)", result.Status)
	}
}

func TestGatedPublicURLSelfCheck_FailBelowThresholdNoStableReturnsUnknown(t *testing.T) {
	failures := 0
	var stable *PublicURLSelfCheck
	raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "refused"}
	result := gatedPublicURLSelfCheck(raw, &failures, &stable)
	if result.Status != PublicURLSelfCheckUnknown {
		t.Errorf("status = %q, want unknown (no stable, below threshold)", result.Status)
	}
}

func TestGatedPublicURLSelfCheck_FailAtThresholdPropagates(t *testing.T) {
	failures := 2 // will become 3 == publicURLSelfCheckMinConsecutiveFailures
	var stable *PublicURLSelfCheck
	raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "timeout"}
	result := gatedPublicURLSelfCheck(raw, &failures, &stable)
	if failures != 3 {
		t.Errorf("failures = %d, want 3", failures)
	}
	if result.Status != PublicURLSelfCheckFail {
		t.Errorf("status = %q, want fail (at threshold)", result.Status)
	}
}

func TestGatedPublicURLSelfCheck_UnknownResetsFailures(t *testing.T) {
	failures := 2
	var stable *PublicURLSelfCheck
	raw := PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown}
	result := gatedPublicURLSelfCheck(raw, &failures, &stable)
	if failures != 0 {
		t.Errorf("failures = %d, want 0 after unknown", failures)
	}
	if result.Status != PublicURLSelfCheckUnknown {
		t.Errorf("status = %q, want unknown", result.Status)
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

// --- ReportUpgradeFailure (with httptest) ---

func TestReportUpgradeFailure_EmptyHubURL(t *testing.T) {
	// Should be a no-op; no panic.
	ReportUpgradeFailure("", "hive-1", "abc123", "def456", "OOM", discardLogger())
}

func TestReportUpgradeFailure_EmptyHiveID(t *testing.T) {
	ReportUpgradeFailure("http://hub.example.com", "", "abc", "def", "reason", discardLogger())
}

func TestReportUpgradeFailure_SendsPayload(t *testing.T) {
	var received HeartbeatPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/heartbeat" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type")
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ReportUpgradeFailure(srv.URL, "hive-42", "sha-new", "sha-old", "disk full", discardLogger())

	if received.HiveID != "hive-42" {
		t.Errorf("HiveID = %q, want hive-42", received.HiveID)
	}
	if received.UpgradeTargetSHA != "sha-new" {
		t.Errorf("UpgradeTargetSHA = %q, want sha-new", received.UpgradeTargetSHA)
	}
	if received.GitHash != "sha-old" {
		t.Errorf("GitHash = %q, want sha-old", received.GitHash)
	}
	if !received.UpgradeFailed {
		t.Error("UpgradeFailed should be true")
	}
	if received.UpgradeError != "disk full" {
		t.Errorf("UpgradeError = %q, want 'disk full'", received.UpgradeError)
	}
	if received.Timestamp == "" {
		t.Error("Timestamp should be set")
	}
}

// --- StartTaskStatusPush ---

func TestStartTaskStatusPush_EmptyHubURLReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		StartTaskStatusPush(ctx, "", func() *TaskStatusPayload { return nil }, discardLogger())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartTaskStatusPush with empty hubURL should return immediately")
	}
}

func TestStartTaskStatusPush_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartTaskStatusPush(ctx, "http://example.com", func() *TaskStatusPayload { return nil }, discardLogger())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartTaskStatusPush did not exit on context cancel")
	}
}

// --- publicURLSelfProbe (with httptest serving locally) ---

func TestPublicURLSelfProbe_InvalidURL(t *testing.T) {
	result := publicURLSelfProbe(context.Background(), "not a url", nil)
	if result.Status != PublicURLSelfCheckFail {
		t.Errorf("status = %q, want fail for invalid URL", result.Status)
	}
	if result.Error != "invalid dashboard URL" {
		t.Errorf("error = %q, want 'invalid dashboard URL'", result.Error)
	}
}

func TestPublicURLSelfProbe_EmptyHost(t *testing.T) {
	result := publicURLSelfProbe(context.Background(), "http://", nil)
	if result.Status != PublicURLSelfCheckFail {
		t.Errorf("status = %q, want fail for empty host", result.Status)
	}
}

func TestPublicURLSelfProbe_NonHTTPScheme(t *testing.T) {
	result := publicURLSelfProbe(context.Background(), "ftp://host.com/path", nil)
	if result.Status != PublicURLSelfCheckFail {
		t.Errorf("status = %q, want fail for ftp scheme", result.Status)
	}
}
