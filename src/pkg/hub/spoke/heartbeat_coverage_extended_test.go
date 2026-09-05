package spoke

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

// ---------------------------------------------------------------------------
// gatedPublicURLSelfCheck
// ---------------------------------------------------------------------------

func TestGatedPublicURLSelfCheck(t *testing.T) {
	t.Run("OK resets consecutive failures and updates stable", func(t *testing.T) {
		failures := 5
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: "2025-01-01T00:00:00Z"}
		result := gatedPublicURLSelfCheck(raw, &failures, &stable)
		if failures != 0 {
			t.Errorf("failures = %d, want 0", failures)
		}
		if stable == nil {
			t.Fatal("stable should be set")
		}
		if stable.Status != PublicURLSelfCheckOK {
			t.Errorf("stable.Status = %q, want ok", stable.Status)
		}
		if result.Status != PublicURLSelfCheckOK {
			t.Errorf("result.Status = %q, want ok", result.Status)
		}
	})

	t.Run("fail below threshold returns stable if available", func(t *testing.T) {
		failures := 0
		stableVal := PublicURLSelfCheck{Status: PublicURLSelfCheckOK, CheckedAt: "2025-01-01T00:00:00Z"}
		stable := &stableVal
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 502"}
		result := gatedPublicURLSelfCheck(raw, &failures, &stable)
		if failures != 1 {
			t.Errorf("failures = %d, want 1", failures)
		}
		if result.Status != PublicURLSelfCheckOK {
			t.Errorf("result.Status = %q, want ok (debounced)", result.Status)
		}
	})

	t.Run("fail below threshold with no stable returns unknown", func(t *testing.T) {
		failures := 0
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 502"}
		result := gatedPublicURLSelfCheck(raw, &failures, &stable)
		if failures != 1 {
			t.Errorf("failures = %d, want 1", failures)
		}
		if result.Status != PublicURLSelfCheckUnknown {
			t.Errorf("result.Status = %q, want unknown (no stable)", result.Status)
		}
	})

	t.Run("fail at threshold reports fail", func(t *testing.T) {
		failures := publicURLSelfCheckMinConsecutiveFailures - 1
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "HTTP 502"}
		result := gatedPublicURLSelfCheck(raw, &failures, &stable)
		if failures != publicURLSelfCheckMinConsecutiveFailures {
			t.Errorf("failures = %d, want %d", failures, publicURLSelfCheckMinConsecutiveFailures)
		}
		if result.Status != PublicURLSelfCheckFail {
			t.Errorf("result.Status = %q, want fail (threshold reached)", result.Status)
		}
		if stable == nil || stable.Status != PublicURLSelfCheckFail {
			t.Error("stable should be set to the fail result")
		}
	})

	t.Run("unknown status resets failures", func(t *testing.T) {
		failures := 2
		var stable *PublicURLSelfCheck
		raw := PublicURLSelfCheck{Status: PublicURLSelfCheckUnknown}
		result := gatedPublicURLSelfCheck(raw, &failures, &stable)
		if failures != 0 {
			t.Errorf("failures = %d, want 0", failures)
		}
		if result.Status != PublicURLSelfCheckUnknown {
			t.Errorf("result.Status = %q, want unknown", result.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// publicURLSelfProbe
// ---------------------------------------------------------------------------

func TestPublicURLSelfProbe(t *testing.T) {
	t.Run("invalid URL returns fail", func(t *testing.T) {
		result := publicURLSelfProbe(context.Background(), "://bad", nil)
		if result.Status != PublicURLSelfCheckFail {
			t.Errorf("Status = %q, want fail", result.Status)
		}
		if result.Error != "invalid dashboard URL" {
			t.Errorf("Error = %q, want 'invalid dashboard URL'", result.Error)
		}
	})

	t.Run("empty URL returns fail", func(t *testing.T) {
		result := publicURLSelfProbe(context.Background(), "", nil)
		if result.Status != PublicURLSelfCheckFail {
			t.Errorf("Status = %q, want fail", result.Status)
		}
	})

	t.Run("non-http scheme returns fail", func(t *testing.T) {
		result := publicURLSelfProbe(context.Background(), "ftp://example.com", nil)
		if result.Status != PublicURLSelfCheckFail {
			t.Errorf("Status = %q, want fail", result.Status)
		}
	})
}

// ---------------------------------------------------------------------------
// ReportUpgradeFailure
// ---------------------------------------------------------------------------

func TestReportUpgradeFailure(t *testing.T) {
	t.Run("empty hubURL is no-op", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ReportUpgradeFailure("", "hive-1", "abc123", "def456", "timeout", logger)
	})

	t.Run("empty hiveID is no-op", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ReportUpgradeFailure("http://hub.example.com", "", "abc123", "def456", "timeout", logger)
	})

	t.Run("sends correct payload to hub", func(t *testing.T) {
		var received HeartbeatPayload
		var authHeader string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &received)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		os.Setenv("HIVE_HUB_SECRET", "test-secret-123")
		defer os.Unsetenv("HIVE_HUB_SECRET")

		ReportUpgradeFailure(srv.URL, "my-hive", "target-sha", "current-sha", "disk full", logger)

		if received.HiveID != "my-hive" {
			t.Errorf("HiveID = %q, want my-hive", received.HiveID)
		}
		if received.GitHash != "current-sha" {
			t.Errorf("GitHash = %q, want current-sha", received.GitHash)
		}
		if received.UpgradeTargetSHA != "target-sha" {
			t.Errorf("UpgradeTargetSHA = %q, want target-sha", received.UpgradeTargetSHA)
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
		// v4 N1 (CWE-200/639/862): the spoke authenticates with a per-hive
		// DERIVED heartbeat sub-key, not the raw master secret — so the header
		// is a Bearer token that is present and non-empty but is NOT the raw
		// secret. (v2 sent the raw secret; v4 hardened this away.)
		if !strings.HasPrefix(authHeader, "Bearer ") || authHeader == "Bearer test-secret-123" || authHeader == "Bearer " {
			t.Errorf("Authorization = %q, want a non-empty derived Bearer token (not the raw secret)", authHeader)
		}
	})

	t.Run("handles server error gracefully", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ReportUpgradeFailure(srv.URL, "hive-1", "sha1", "sha2", "reason", logger)
	})
}

// ---------------------------------------------------------------------------
// StartTaskStatusPush
// ---------------------------------------------------------------------------

func TestStartTaskStatusPushExtended(t *testing.T) {
	t.Run("empty hubURL is immediate return", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx := context.Background()
		done := make(chan struct{})
		go func() {
			StartTaskStatusPush(ctx, "", func() *TaskStatusPayload { return nil }, logger)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("StartTaskStatusPush with empty URL should return immediately")
		}
	})

	t.Run("context cancellation stops the loop", func(t *testing.T) {
		oldInterval := taskPushInterval
		taskPushInterval = 10 * time.Millisecond
		defer func() { taskPushInterval = oldInterval }()

		var callCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			StartTaskStatusPush(ctx, srv.URL, func() *TaskStatusPayload {
				return &TaskStatusPayload{
					HiveID:      "test-hive",
					Leaderboard: []LeaderboardEntry{},
				}
			}, logger)
			close(done)
		}()

		// Wait until at least one push lands before cancelling: a fixed
		// sleep races with the ticker under load and made this test flaky.
		deadline := time.After(5 * time.Second)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for callCount.Load() == 0 {
			select {
			case <-deadline:
				t.Fatal("timed out waiting for task-status push")
			case <-tick.C:
			}
		}
		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("StartTaskStatusPush did not exit after context cancellation")
		}

		if callCount.Load() == 0 {
			t.Error("expected at least one push to the hub")
		}
	})

	t.Run("nil payload skips push", func(t *testing.T) {
		oldInterval := taskPushInterval
		taskPushInterval = 10 * time.Millisecond
		defer func() { taskPushInterval = oldInterval }()

		var callCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			StartTaskStatusPush(ctx, srv.URL, func() *TaskStatusPayload {
				return nil
			}, logger)
			close(done)
		}()

		<-time.After(50 * time.Millisecond)
		cancel()
		<-done

		if callCount.Load() != 0 {
			t.Errorf("expected 0 pushes for nil payload, got %d", callCount.Load())
		}
	})

	t.Run("sets authorization header when secret present", func(t *testing.T) {
		oldInterval := taskPushInterval
		taskPushInterval = 10 * time.Millisecond
		defer func() { taskPushInterval = oldInterval }()

		os.Setenv("HIVE_HUB_SECRET", "task-secret")
		defer os.Unsetenv("HIVE_HUB_SECRET")

		var gotAuth atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			StartTaskStatusPush(ctx, srv.URL, func() *TaskStatusPayload {
				return &TaskStatusPayload{HiveID: "h1"}
			}, logger)
			close(done)
		}()

		select {
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for authorized task-status push")
		default:
		}
		for {
			if _, ok := gotAuth.Load().(string); ok {
				break
			}
			select {
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for authorization header")
			case <-time.After(5 * time.Millisecond):
			}
		}
		cancel()
		<-done

		// v4 N1: task-status uses the same per-hive DERIVED sub-key as the
		// heartbeat, so the header is a non-empty Bearer token but not the raw
		// secret.
		if auth, ok := gotAuth.Load().(string); !ok || !strings.HasPrefix(auth, "Bearer ") || auth == "Bearer task-secret" || auth == "Bearer " {
			t.Errorf("Authorization = %q, want a non-empty derived Bearer token (not the raw secret)", auth)
		}
	})
}

// ---------------------------------------------------------------------------
// inClusterAPIConfig (via var overrides)
// ---------------------------------------------------------------------------

func TestInClusterAPIConfig(t *testing.T) {
	t.Run("missing env vars returns error", func(t *testing.T) {
		oldHost := kubernetesAPIHost
		oldPort := kubernetesAPIPort
		kubernetesAPIHost = func() string { return "" }
		kubernetesAPIPort = func() string { return "" }
		defer func() {
			kubernetesAPIHost = oldHost
			kubernetesAPIPort = oldPort
		}()

		_, err := inClusterAPIConfig()
		if err == nil {
			t.Fatal("expected error for missing env vars")
		}
		if err.Error() != "not running in a Kubernetes cluster" {
			t.Errorf("error = %q, want 'not running in a Kubernetes cluster'", err.Error())
		}
	})

	t.Run("missing token file returns error", func(t *testing.T) {
		oldHost := kubernetesAPIHost
		oldPort := kubernetesAPIPort
		oldDir := serviceAccountDir
		kubernetesAPIHost = func() string { return "10.0.0.1" }
		kubernetesAPIPort = func() string { return "443" }
		serviceAccountDir = t.TempDir()
		defer func() {
			kubernetesAPIHost = oldHost
			kubernetesAPIPort = oldPort
			serviceAccountDir = oldDir
		}()

		_, err := inClusterAPIConfig()
		if err == nil {
			t.Fatal("expected error for missing token")
		}
		if err.Error() != "serviceaccount token unavailable" {
			t.Errorf("error = %q, want 'serviceaccount token unavailable'", err.Error())
		}
	})

	t.Run("missing namespace file returns error", func(t *testing.T) {
		oldHost := kubernetesAPIHost
		oldPort := kubernetesAPIPort
		oldDir := serviceAccountDir
		dir := t.TempDir()
		os.WriteFile(dir+"/token", []byte("my-token"), 0o644)
		kubernetesAPIHost = func() string { return "10.0.0.1" }
		kubernetesAPIPort = func() string { return "443" }
		serviceAccountDir = dir
		defer func() {
			kubernetesAPIHost = oldHost
			kubernetesAPIPort = oldPort
			serviceAccountDir = oldDir
		}()

		_, err := inClusterAPIConfig()
		if err == nil {
			t.Fatal("expected error for missing namespace")
		}
		if err.Error() != "serviceaccount namespace unavailable" {
			t.Errorf("error = %q, want 'serviceaccount namespace unavailable'", err.Error())
		}
	})

	t.Run("success returns config", func(t *testing.T) {
		oldHost := kubernetesAPIHost
		oldPort := kubernetesAPIPort
		oldDir := serviceAccountDir
		dir := t.TempDir()
		os.WriteFile(dir+"/token", []byte("  my-token  "), 0o644)
		os.WriteFile(dir+"/namespace", []byte("hive-ns\n"), 0o644)
		kubernetesAPIHost = func() string { return "10.0.0.1" }
		kubernetesAPIPort = func() string { return "6443" }
		serviceAccountDir = dir
		defer func() {
			kubernetesAPIHost = oldHost
			kubernetesAPIPort = oldPort
			serviceAccountDir = oldDir
		}()

		cfg, err := inClusterAPIConfig()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.BaseURL != "https://10.0.0.1:6443" {
			t.Errorf("BaseURL = %q, want https://10.0.0.1:6443", cfg.BaseURL)
		}
		if cfg.Token != "my-token" {
			t.Errorf("Token = %q, want 'my-token'", cfg.Token)
		}
		if cfg.Namespace != "hive-ns" {
			t.Errorf("Namespace = %q, want 'hive-ns'", cfg.Namespace)
		}
		if cfg.CAPath != dir+"/ca.crt" {
			t.Errorf("CAPath = %q, want %q", cfg.CAPath, dir+"/ca.crt")
		}
	})
}

// ---------------------------------------------------------------------------
// inClusterHTTPClient
// ---------------------------------------------------------------------------

func TestInClusterHTTPClient(t *testing.T) {
	t.Run("missing CA file returns error", func(t *testing.T) {
		cfg := &inClusterConfig{
			BaseURL: "https://10.0.0.1:6443",
			Token:   "tok",
			CAPath:  "/nonexistent/ca.crt",
		}
		_, err := inClusterHTTPClient(cfg)
		if err == nil {
			t.Fatal("expected error for missing CA")
		}
	})

	t.Run("invalid PEM returns error", func(t *testing.T) {
		dir := t.TempDir()
		caPath := dir + "/ca.crt"
		os.WriteFile(caPath, []byte("not a PEM certificate"), 0o644)

		cfg := &inClusterConfig{
			BaseURL: "https://10.0.0.1:6443",
			Token:   "tok",
			CAPath:  caPath,
		}
		_, err := inClusterHTTPClient(cfg)
		if err == nil {
			t.Fatal("expected error for invalid PEM")
		}
	})
}

// ---------------------------------------------------------------------------
// routeExistenceProbe
// ---------------------------------------------------------------------------

func TestRouteExistenceProbe(t *testing.T) {
	t.Run("empty host returns unknown", func(t *testing.T) {
		result := routeExistenceProbe(context.Background(), "")
		if result.Status != RouteExistenceUnknown {
			t.Errorf("Status = %q, want unknown", result.Status)
		}
		if result.Error != "empty dashboard host" {
			t.Errorf("Error = %q, want 'empty dashboard host'", result.Error)
		}
	})

	t.Run("not in cluster returns unknown", func(t *testing.T) {
		oldHost := kubernetesAPIHost
		oldPort := kubernetesAPIPort
		kubernetesAPIHost = func() string { return "" }
		kubernetesAPIPort = func() string { return "" }
		defer func() {
			kubernetesAPIHost = oldHost
			kubernetesAPIPort = oldPort
		}()

		result := routeExistenceProbe(context.Background(), "example.com")
		if result.Status != RouteExistenceUnknown {
			t.Errorf("Status = %q, want unknown", result.Status)
		}
		if result.Host != "example.com" {
			t.Errorf("Host = %q, want example.com", result.Host)
		}
	})
}
