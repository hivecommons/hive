package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// copilotDeviceMock serves fake /login/device/code and /login/oauth/access_token
// endpoints so handleCopilotAuthStart and pollCopilotToken can be driven without
// real network access. tokenResponses is consumed in order for successive polls
// of the access_token endpoint; the last entry repeats once exhausted.
type copilotDeviceMock struct {
	srv            *httptest.Server
	tokenResponses []map[string]any
	callCount      int
	pollTimes      []time.Time
}

func newCopilotDeviceMock(t *testing.T, deviceCode string, expiresIn, interval int, tokenResponses ...map[string]any) *copilotDeviceMock {
	t.Helper()
	m := &copilotDeviceMock{tokenResponses: tokenResponses}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      deviceCode,
			"user_code":        "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       expiresIn,
			"interval":         interval,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		m.pollTimes = append(m.pollTimes, time.Now())
		idx := m.callCount
		if idx >= len(m.tokenResponses) {
			idx = len(m.tokenResponses) - 1
		}
		m.callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.tokenResponses[idx])
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// withCopilotMockURLs redirects the package-level device-flow endpoint vars at
// the mock server for the duration of the test, restoring the real GitHub URLs
// afterward.
func withCopilotMockURLs(t *testing.T, mockURL string) {
	t.Helper()
	origDevice := copilotDeviceCodeURL
	origToken := copilotAccessTokenURL
	copilotDeviceCodeURL = mockURL + "/login/device/code"
	copilotAccessTokenURL = mockURL + "/login/oauth/access_token"
	t.Cleanup(func() {
		copilotDeviceCodeURL = origDevice
		copilotAccessTokenURL = origToken
	})
}

// withCopilotTokenPath redirects the package-level token-file path var at a
// file inside t.TempDir(), restoring the real path afterward.
func withCopilotTokenPath(t *testing.T) string {
	t.Helper()
	orig := copilotUserTokenPath
	path := filepath.Join(t.TempDir(), "copilot-user-token")
	copilotUserTokenPath = path
	t.Cleanup(func() {
		copilotUserTokenPath = orig
	})
	return path
}

// ---- handleCopilotAuthStatus ----

func TestCopilotAuthStatus_Unauthenticated(t *testing.T) {
	withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doGet(s, "/api/copilot-auth/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["logged_in"] != false {
		t.Fatalf("logged_in = %v, want false", got["logged_in"])
	}
	if got["pending"] != false {
		t.Fatalf("pending = %v, want false", got["pending"])
	}
	if got["error"] != "" {
		t.Fatalf("error = %v, want empty", got["error"])
	}
}

func TestCopilotAuthStatus_AuthenticatedViaTokenFile(t *testing.T) {
	path := withCopilotTokenPath(t)
	if err := os.WriteFile(path, []byte("gho_faketoken"), 0o600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doGet(s, "/api/copilot-auth/status")
	got := decodeJSON(t, rec)
	if got["logged_in"] != true {
		t.Fatalf("logged_in = %v, want true (token file present)", got["logged_in"])
	}
}

func TestCopilotAuthStatus_AuthenticatedViaEnv(t *testing.T) {
	withCopilotTokenPath(t)
	t.Setenv("COPILOT_GITHUB_TOKEN", "gho_envtoken")
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doGet(s, "/api/copilot-auth/status")
	got := decodeJSON(t, rec)
	if got["logged_in"] != true {
		t.Fatalf("logged_in = %v, want true (env token)", got["logged_in"])
	}
}

func TestCopilotAuthStatus_ReflectsPollingAndLastError(t *testing.T) {
	withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = true
	s.copilotAuthFlow.lastError = "some prior error"
	s.copilotAuthFlow.mu.Unlock()

	rec := doGet(s, "/api/copilot-auth/status")
	got := decodeJSON(t, rec)
	if got["pending"] != true {
		t.Fatalf("pending = %v, want true", got["pending"])
	}
	if got["error"] != "some prior error" {
		t.Fatalf("error = %v, want %q", got["error"], "some prior error")
	}
}

// ---- handleCopilotAuthStart ----

func TestCopilotAuthStart_ReturnsDeviceCode(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-abc", 900, 5,
		map[string]any{"error": "authorization_pending"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))
	// The handler spawns a poll goroutine that reads the package-level
	// endpoint/path vars; join it before the withCopilot* cleanups restore
	// them, or it races the restores (and later tests' mocks).
	t.Cleanup(s.stopCopilotPoll)

	rec := doPost(s, "/api/copilot-auth/start", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if got["user_code"] != "ABCD-1234" {
		t.Fatalf("user_code = %v, want ABCD-1234", got["user_code"])
	}
	if got["verification_uri"] != "https://github.com/login/device" {
		t.Fatalf("verification_uri = %v, want https://github.com/login/device", got["verification_uri"])
	}
	if got["expires_in"] != float64(900) {
		t.Fatalf("expires_in = %v, want 900", got["expires_in"])
	}

	// handleCopilotAuthStart also flips polling=true and spawns the poll
	// goroutine; verify the flag flipped promptly (goroutine start is async).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.copilotAuthFlow.mu.Lock()
		polling := s.copilotAuthFlow.polling
		s.copilotAuthFlow.mu.Unlock()
		if polling {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected polling=true after start")
}

func TestCopilotAuthStart_DeviceCodeEndpointError(t *testing.T) {
	withCopilotTokenPath(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"error": "boom"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	withCopilotMockURLs(t, srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/copilot-auth/start", map[string]interface{}{})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

// ---- pollCopilotToken ----

func TestPollCopilotToken_SucceedsAndSavesToken(t *testing.T) {
	tokenPath := withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-1", 900, 0, // interval 0 -> fast, driven directly below
		map[string]any{"access_token": "gho_realtoken", "token_type": "bearer"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	s.copilotAuthFlow.mu.Lock()
	s.copilotAuthFlow.polling = true
	s.copilotAuthFlow.mu.Unlock()

	// Poll directly with a 0-second interval and a short expiry so the test
	// does not sleep for real GitHub-scale intervals.
	done := make(chan struct{})
	go func() {
		s.pollCopilotToken(context.Background(), "dev-code-1", 0, 2*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("pollCopilotToken did not return in time")
	}

	s.copilotAuthFlow.mu.Lock()
	polling := s.copilotAuthFlow.polling
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()
	if polling {
		t.Fatalf("polling should be false after success")
	}
	if lastErr != "" {
		t.Fatalf("lastError = %q, want empty on success", lastErr)
	}

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if string(data) != "gho_realtoken" {
		t.Fatalf("token file contents = %q, want gho_realtoken", string(data))
	}
}

func TestPollCopilotToken_RetriesOnPendingThenSucceeds(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-2", 900, 0,
		map[string]any{"error": "authorization_pending"},
		map[string]any{"error": "authorization_pending"},
		map[string]any{"access_token": "gho_afterretries", "token_type": "bearer"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	done := make(chan struct{})
	go func() {
		s.pollCopilotToken(context.Background(), "dev-code-2", 0, 5*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("pollCopilotToken did not return in time")
	}

	if mock.callCount < 3 {
		t.Fatalf("expected at least 3 polls (2 pending + 1 success), got %d", mock.callCount)
	}

	s.copilotAuthFlow.mu.Lock()
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()
	if lastErr != "" {
		t.Fatalf("lastError = %q, want empty on eventual success", lastErr)
	}
}

func TestPollCopilotToken_SlowDownBumpsInterval(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-3", 900, 0,
		map[string]any{"error": "slow_down"},
		map[string]any{"access_token": "gho_afterslowdown", "token_type": "bearer"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	start := time.Now()
	done := make(chan struct{})
	go func() {
		// Start at interval 0; after slow_down the poller adds
		// copilotSlowDownBumpSec (5s) to intervalSec, so the NEXT loop
		// iteration sleeps 5s before polling again. pollCopilotToken checks
		// its expiry deadline only at the top of each loop iteration (after
		// the sleep), so with a 6s expiry the bumped 5s sleep still fits
		// inside the deadline and the second (successful) poll fires.
		s.pollCopilotToken(context.Background(), "dev-code-3", 0, 6*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatalf("pollCopilotToken did not return in time")
	}
	elapsed := time.Since(start)

	if mock.callCount < 2 {
		t.Fatalf("expected slow_down poll + a retry poll (>=2 calls), got %d (elapsed=%s)", mock.callCount, elapsed)
	}
	// The retry must not have fired immediately — the slow_down bump should
	// have forced a real gap between the first and second poll.
	if len(mock.pollTimes) >= 2 {
		gap := mock.pollTimes[1].Sub(mock.pollTimes[0])
		if gap < 4*time.Second {
			t.Fatalf("gap between slow_down and retry = %s, want >= ~5s (bump honored)", gap)
		}
	}
	s.copilotAuthFlow.mu.Lock()
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()
	if lastErr != "" {
		t.Fatalf("expected eventual success after slow_down retry, got error %q", lastErr)
	}
}

func TestPollCopilotToken_ExpiresWithoutSuccess(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-4", 900, 0,
		map[string]any{"error": "authorization_pending"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	start := time.Now()
	done := make(chan struct{})
	go func() {
		s.pollCopilotToken(context.Background(), "dev-code-4", 0, 300*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("pollCopilotToken did not return in time (expiry not honored)")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("pollCopilotToken took %s, expiry should have stopped it quickly", elapsed)
	}

	s.copilotAuthFlow.mu.Lock()
	polling := s.copilotAuthFlow.polling
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()
	if polling {
		t.Fatalf("polling should be false after expiry")
	}
	if lastErr == "" {
		t.Fatalf("expected a non-empty expiry error message")
	}
}

func TestPollCopilotToken_AccessDeniedStopsImmediately(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-5", 900, 0,
		map[string]any{"error": "access_denied", "error_description": "user declined"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	done := make(chan struct{})
	go func() {
		s.pollCopilotToken(context.Background(), "dev-code-5", 0, 10*time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("pollCopilotToken should stop immediately on access_denied")
	}

	s.copilotAuthFlow.mu.Lock()
	lastErr := s.copilotAuthFlow.lastError
	s.copilotAuthFlow.mu.Unlock()
	if lastErr == "" {
		t.Fatalf("expected access_denied error message")
	}
	if lastErr != "access_denied — user declined" {
		t.Fatalf("lastError = %q, want %q", lastErr, "access_denied — user declined")
	}
}

// ---- saveCopilotToken ----

func TestSaveCopilotToken_WritesAtomicallyWithPermissions(t *testing.T) {
	tokenPath := withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	if err := s.saveCopilotToken("gho_savedtoken"); err != nil {
		t.Fatalf("saveCopilotToken: %v", err)
	}

	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 0600", perm)
	}

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != "gho_savedtoken" {
		t.Fatalf("token contents = %q, want gho_savedtoken", string(data))
	}

	// The temp file used for the atomic rename should not be left behind.
	if _, err := os.Stat(tokenPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected .tmp file to be gone after rename, stat err = %v", err)
	}
}

func TestSaveCopilotToken_ErrorOnBadPath(t *testing.T) {
	orig := copilotUserTokenPath
	// A path with a nonexistent parent directory forces os.WriteFile to fail.
	copilotUserTokenPath = filepath.Join(t.TempDir(), "nonexistent-subdir", "copilot-user-token")
	t.Cleanup(func() { copilotUserTokenPath = orig })

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	if err := s.saveCopilotToken("gho_x"); err == nil {
		t.Fatalf("expected error writing to a path with a missing parent dir")
	}
}

// ---- handleCopilotAuthLogout ----

func TestCopilotAuthLogout_RemovesTokenFile(t *testing.T) {
	tokenPath := withCopilotTokenPath(t)
	if err := os.WriteFile(tokenPath, []byte("gho_tobedeleted"), 0o600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	// Sanity: status is authenticated before logout.
	rec := doGet(s, "/api/copilot-auth/status")
	if got := decodeJSON(t, rec); got["logged_in"] != true {
		t.Fatalf("precondition: logged_in = %v, want true", got["logged_in"])
	}

	rec = doPost(s, "/api/copilot-auth/logout", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["status"] != "logged_out" {
		t.Fatalf("status = %v, want logged_out", got["status"])
	}

	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("expected token file removed, stat err = %v", err)
	}

	rec = doGet(s, "/api/copilot-auth/status")
	got = decodeJSON(t, rec)
	if got["logged_in"] != false {
		t.Fatalf("logged_in after logout = %v, want false", got["logged_in"])
	}
}

func TestCopilotAuthLogout_NoFileIsNoop(t *testing.T) {
	withCopilotTokenPath(t)
	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/copilot-auth/logout", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON(t, rec)
	if got["status"] != "logged_out" {
		t.Fatalf("status = %v, want logged_out", got["status"])
	}
}

func TestPollCopilotToken_CancelStopsPromptly(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-6", 900, 0,
		map[string]any{"error": "authorization_pending"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	// interval 5s: without cancellation the poller would sit in its sleep
	// for the full interval (and keep polling for the whole 10m expiry).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.pollCopilotToken(ctx, "dev-code-6", 5, 10*time.Minute)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("pollCopilotToken did not stop promptly after cancel")
	}
}

func TestStopCopilotPoll_JoinsHandlerSpawnedPoller(t *testing.T) {
	withCopilotTokenPath(t)
	mock := newCopilotDeviceMock(t, "dev-code-7", 900, 5,
		map[string]any{"error": "authorization_pending"},
	)
	withCopilotMockURLs(t, mock.srv.URL)

	s := NewServer(0, covBLogger())
	s.RegisterAPI(testDeps(t))

	rec := doPost(s, "/api/copilot-auth/start", map[string]interface{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// stopCopilotPoll must cancel the goroutine the handler spawned, wait
	// for it to exit, and leave the flow idle — promptly, not after the
	// poller's 5s interval or 900s expiry.
	joined := make(chan struct{})
	go func() {
		s.stopCopilotPoll()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatalf("stopCopilotPoll did not join the poll goroutine promptly")
	}

	s.copilotAuthFlow.mu.Lock()
	polling := s.copilotAuthFlow.polling
	cancelFn := s.copilotAuthFlow.cancel
	s.copilotAuthFlow.mu.Unlock()
	if polling {
		t.Fatalf("polling = true after stopCopilotPoll, want false")
	}
	if cancelFn != nil {
		t.Fatalf("cancel should be cleared after stopCopilotPoll")
	}

	// A second stop with no poller running must be a harmless no-op.
	s.stopCopilotPoll()
}
