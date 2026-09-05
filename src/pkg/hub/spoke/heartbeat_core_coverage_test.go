package spoke

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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

// --- recordHeartbeatSuccess / recordHeartbeatAttempt / Last* / HeartbeatEnabled ---

func TestRecordHeartbeatSuccess(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	// Before any call, LastHeartbeatSuccess returns false.
	_, ok := LastHeartbeatSuccess()
	if ok {
		t.Fatal("expected no success before recordHeartbeatSuccess")
	}

	// Unix-second precision: truncate before, round up after.
	before := time.Now().Truncate(time.Second)
	recordHeartbeatSuccess()
	after := time.Now().Truncate(time.Second).Add(time.Second)

	ts, ok := LastHeartbeatSuccess()
	if !ok {
		t.Fatal("expected success after recordHeartbeatSuccess")
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("timestamp %v not in [%v, %v]", ts, before, after)
	}
}

func TestRecordHeartbeatAttempt(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	_, ok := LastHeartbeatAttempt()
	if ok {
		t.Fatal("expected no attempt before recordHeartbeatAttempt")
	}

	before := time.Now().Truncate(time.Second)
	recordHeartbeatAttempt()
	after := time.Now().Truncate(time.Second).Add(time.Second)

	ts, ok := LastHeartbeatAttempt()
	if !ok {
		t.Fatal("expected attempt after recordHeartbeatAttempt")
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("timestamp %v not in [%v, %v]", ts, before, after)
	}
}

func TestHeartbeatEnabled(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	if HeartbeatEnabled() {
		t.Fatal("expected HeartbeatEnabled=false at start")
	}

	heartbeatLoopStarted.Store(true)
	if !HeartbeatEnabled() {
		t.Fatal("expected HeartbeatEnabled=true after store")
	}
}

func TestStoreUnixOrZero_CoreCov(t *testing.T) {
	var dst atomic.Int64

	// Store a real time.
	now := time.Now()
	storeUnixOrZero(&dst, now)
	if got := dst.Load(); got != now.Unix() {
		t.Errorf("stored %d, want %d", got, now.Unix())
	}

	// Store zero time clears to 0.
	storeUnixOrZero(&dst, time.Time{})
	if got := dst.Load(); got != 0 {
		t.Errorf("stored %d after zero time, want 0", got)
	}
}

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

// --- SendUpgradingHeartbeat ---

func TestSendUpgradingHeartbeat_NilPayload(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	// nil collector → returns early, no panic
	SendUpgradingHeartbeat("http://x", func() *HeartbeatPayload { return nil }, "sha1", slog.Default())
}

func TestSendUpgradingHeartbeat_Success(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var received HeartbeatPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/heartbeat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("wrong content-type: %s", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	collect := func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "test-hive", GitHash: "current-sha"}
	}
	SendUpgradingHeartbeat(srv.URL, collect, "target-sha", slog.Default())

	if !received.Upgrading {
		t.Error("expected Upgrading=true in payload")
	}
	if received.UpgradeTargetSHA != "target-sha" {
		t.Errorf("UpgradeTargetSHA = %q, want 'target-sha'", received.UpgradeTargetSHA)
	}
	if received.Timestamp == "" {
		t.Error("expected non-empty Timestamp")
	}

	// Should record success.
	_, ok := LastHeartbeatSuccess()
	if !ok {
		t.Error("expected heartbeat success to be recorded after 2xx")
	}
}

func TestSendUpgradingHeartbeat_NonSuccess(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	collect := func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "hive1"}
	}
	SendUpgradingHeartbeat(srv.URL, collect, "sha2", slog.Default())

	// Should NOT record success on non-2xx.
	_, ok := LastHeartbeatSuccess()
	if ok {
		t.Error("should not record success on 403")
	}
}

func TestSendUpgradingHeartbeat_BearerToken(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	os.Setenv("HIVE_HUB_SECRET", "test-secret")
	defer os.Unsetenv("HIVE_HUB_SECRET")

	collect := func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "hive1"}
	}
	SendUpgradingHeartbeat(srv.URL, collect, "sha3", slog.Default())

	// v4 separates key domains: the heartbeat is authenticated with the
	// derived heartbeat key, NEVER the raw master. Assert both halves — that
	// the header carries the derived key, and that the master does not leak
	// onto the wire — so a regression back to the master is caught.
	want := "Bearer " + SpokeHeartbeatKey()
	if gotAuth != want {
		t.Errorf("Authorization header did not carry the derived heartbeat key")
	}
	if gotAuth == "Bearer "+os.Getenv("HIVE_HUB_SECRET") {
		t.Error("raw hub master secret leaked into the heartbeat Authorization header")
	}
}

// --- ReportUpgradeFailure ---

func TestReportUpgradeFailure_EmptyArgs(t *testing.T) {
	// Should return early without panic.
	ReportUpgradeFailure("", "hive1", "target", "current", "err", slog.Default())
	ReportUpgradeFailure("http://x", "", "target", "current", "err", slog.Default())
}

func TestReportUpgradeFailure_Success(t *testing.T) {
	var received HeartbeatPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ReportUpgradeFailure(srv.URL, "hive-x", "target-sha", "current-sha", "disk full", slog.Default())

	if !received.UpgradeFailed {
		t.Error("expected UpgradeFailed=true")
	}
	if received.UpgradeError != "disk full" {
		t.Errorf("UpgradeError = %q, want 'disk full'", received.UpgradeError)
	}
	if received.HiveID != "hive-x" {
		t.Errorf("HiveID = %q, want 'hive-x'", received.HiveID)
	}
	if received.GitHash != "current-sha" {
		t.Errorf("GitHash = %q, want 'current-sha'", received.GitHash)
	}
	if received.UpgradeTargetSHA != "target-sha" {
		t.Errorf("UpgradeTargetSHA = %q, want 'target-sha'", received.UpgradeTargetSHA)
	}
	if received.Timestamp == "" {
		t.Error("expected non-empty Timestamp")
	}
}

func TestReportUpgradeFailure_BearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	os.Setenv("HIVE_HUB_SECRET", "my-secret")
	defer os.Unsetenv("HIVE_HUB_SECRET")

	ReportUpgradeFailure(srv.URL, "h1", "t", "c", "err", slog.Default())

	want := "Bearer " + SpokeHeartbeatKey()
	if gotAuth != want {
		t.Errorf("Authorization header did not carry the derived heartbeat key")
	}
	if gotAuth == "Bearer "+os.Getenv("HIVE_HUB_SECRET") {
		t.Error("raw hub master secret leaked into the upgrade-failure Authorization header")
	}
}

func TestReportUpgradeFailure_Unreachable(t *testing.T) {
	// Should not panic on unreachable hub.
	ReportUpgradeFailure("http://127.0.0.1:1", "h1", "t", "c", "err", slog.Default())
}

// --- gatedPublicURLSelfCheck edge cases ---

func TestGatedPublicURLSelfCheck_TransitionFromOKToFail(t *testing.T) {
	consecutiveFailures := 0
	var stable *PublicURLSelfCheck

	// First: OK result establishes stable.
	okResult := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: "2026-08-11T12:00:00Z"}
	got := gatedPublicURLSelfCheck(okResult, &consecutiveFailures, &stable)
	if got.Status != PublicURLSelfCheckOK {
		t.Errorf("expected ok, got %q", got.Status)
	}
	if consecutiveFailures != 0 {
		t.Errorf("expected 0 failures after ok, got %d", consecutiveFailures)
	}

	// Second: single failure should NOT flip to fail (gated).
	failResult := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, CheckedAt: "2026-08-11T12:01:00Z"}
	got = gatedPublicURLSelfCheck(failResult, &consecutiveFailures, &stable)
	if got.Status != PublicURLSelfCheckOK {
		t.Errorf("expected gated ok after 1 fail, got %q", got.Status)
	}
}

func TestGatedPublicURLSelfCheck_UnknownResetsCounter(t *testing.T) {
	consecutiveFailures := 5
	var stable *PublicURLSelfCheck

	unknownResult := PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown, CheckedAt: "2026-08-11T12:00:00Z"}
	got := gatedPublicURLSelfCheck(unknownResult, &consecutiveFailures, &stable)
	if got.Status != PublicURLSelfCheckUnknown {
		t.Errorf("expected unknown, got %q", got.Status)
	}
	if consecutiveFailures != 0 {
		t.Errorf("expected counter reset to 0, got %d", consecutiveFailures)
	}
}
