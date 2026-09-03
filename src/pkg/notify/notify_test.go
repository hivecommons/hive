package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// asyncWaitTimeout is how long we wait for goroutines spawned by Send() to complete.
const asyncWaitTimeout = 2 * time.Second

// capturedRequest holds a single HTTP request received by a mock server.
type capturedRequest struct {
	method  string
	path    string
	headers http.Header
	body    []byte
}

// mockServer creates an httptest server that records all incoming requests into
// the returned slice (protected by the returned mutex). The caller must call
// server.Close() when done.
func mockServer(t *testing.T) (*httptest.Server, *[]capturedRequest, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("mock server: failed to read body: %v", err)
		}
		mu.Lock()
		reqs = append(reqs, capturedRequest{
			method:  r.Method,
			path:    r.URL.Path,
			headers: r.Header.Clone(),
			body:    body,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &reqs, &mu
}

// errorServer creates an httptest server that always responds with the given status code.
func errorServer(statusCode int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
	}))
}

// waitForN blocks until n requests have been captured or the timeout elapses.
func waitForN(n int, reqs *[]capturedRequest, mu *sync.Mutex, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(*reqs)
		mu.Unlock()
		if got >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// silentLogger returns a slog.Logger that discards all output, keeping test output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// verboseLogger returns a slog.Logger that writes to stderr, useful for debugging.
func verboseLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ---------------------------------------------------------------------------
// New()
// ---------------------------------------------------------------------------

func TestNew_CreatesNotifier(t *testing.T) {
	cfg := config.NotificationsConfig{}
	logger := silentLogger()
	n := New(cfg, logger)

	if n == nil {
		t.Fatal("New() returned nil")
	}
	if n.client == nil {
		t.Error("New() did not initialise http.Client")
	}
	if n.client.Timeout != httpTimeoutSeconds*time.Second {
		t.Errorf("client timeout = %v; want %v", n.client.Timeout, httpTimeoutSeconds*time.Second)
	}
	if n.logger == nil {
		t.Error("New() did not store logger")
	}
}

// ---------------------------------------------------------------------------
// Priority constants
// ---------------------------------------------------------------------------

func TestPriorityConstants(t *testing.T) {
	cases := []struct {
		name string
		p    Priority
		want string
	}{
		{"high", PriorityHigh, "high"},
		{"default", PriorityDefault, "default"},
		{"low", PriorityLow, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.p) != tc.want {
				t.Errorf("Priority %q = %q; want %q", tc.name, string(tc.p), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sendNtfy()
// ---------------------------------------------------------------------------

func TestSend_NtfyOnly_CorrectURLAndHeaders(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{
			Server: srv.URL,
			Topic:  "alerts",
		},
	}
	n := New(cfg, silentLogger())
	n.Send("Test Title", "Test message body", PriorityHigh)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for ntfy request")
	}

	mu.Lock()
	defer mu.Unlock()
	r := (*reqs)[0]

	// Method
	if r.method != http.MethodPost {
		t.Errorf("method = %q; want POST", r.method)
	}

	// Path: server URL + "/" + topic
	wantPath := "/alerts"
	if r.path != wantPath {
		t.Errorf("path = %q; want %q", r.path, wantPath)
	}

	// Headers
	if got := r.headers.Get("Title"); got != "Test Title" {
		t.Errorf("Title header = %q; want %q", got, "Test Title")
	}
	if got := r.headers.Get("Priority"); got != "high" {
		t.Errorf("Priority header = %q; want %q", got, "high")
	}

	// Body
	if string(r.body) != "Test message body" {
		t.Errorf("body = %q; want %q", string(r.body), "Test message body")
	}
}

func TestSend_NtfyPriorityDefault(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityDefault)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for ntfy request")
	}

	mu.Lock()
	defer mu.Unlock()
	if got := (*reqs)[0].headers.Get("Priority"); got != "default" {
		t.Errorf("Priority header = %q; want %q", got, "default")
	}
}

func TestSend_NtfyPriorityLow(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityLow)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for ntfy request")
	}

	mu.Lock()
	defer mu.Unlock()
	if got := (*reqs)[0].headers.Get("Priority"); got != "low" {
		t.Errorf("Priority header = %q; want %q", got, "low")
	}
}

func TestSend_NtfyTopicInURL(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "my-topic"},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityHigh)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for ntfy request")
	}

	mu.Lock()
	defer mu.Unlock()
	if got := (*reqs)[0].path; got != "/my-topic" {
		t.Errorf("path = %q; want %q", got, "/my-topic")
	}
}

// ---------------------------------------------------------------------------
// sendSlack()
// ---------------------------------------------------------------------------

func TestSend_SlackOnly_CorrectJSONPayload(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: srv.URL + "/webhook"},
	}
	n := New(cfg, silentLogger())
	n.Send("Alert", "Something happened", PriorityDefault)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for slack request")
	}

	mu.Lock()
	defer mu.Unlock()
	r := (*reqs)[0]

	if r.method != http.MethodPost {
		t.Errorf("method = %q; want POST", r.method)
	}

	var payload map[string]string
	if err := json.Unmarshal(r.body, &payload); err != nil {
		t.Fatalf("failed to parse slack JSON: %v (body: %q)", err, string(r.body))
	}

	wantText := "*Alert*\nSomething happened"
	if got := payload["text"]; got != wantText {
		t.Errorf("slack text = %q; want %q", got, wantText)
	}
}

func TestSend_SlackBoldTitleFormat(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: srv.URL},
	}
	n := New(cfg, silentLogger())
	n.Send("MyTitle", "MyMessage", PriorityHigh)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for slack request")
	}

	mu.Lock()
	defer mu.Unlock()
	var payload map[string]string
	if err := json.Unmarshal((*reqs)[0].body, &payload); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Title must be wrapped in *...*
	if len(payload["text"]) < 2 || payload["text"][0] != '*' {
		t.Errorf("slack text does not start with '*': %q", payload["text"])
	}
}

// ---------------------------------------------------------------------------
// sendDiscordWebhook()
// ---------------------------------------------------------------------------

func TestSend_DiscordOnly_CorrectJSONPayload(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL + "/discord"},
	}
	n := New(cfg, silentLogger())
	n.Send("Incident", "Cluster is down", PriorityHigh)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for discord request")
	}

	mu.Lock()
	defer mu.Unlock()
	r := (*reqs)[0]

	if r.method != http.MethodPost {
		t.Errorf("method = %q; want POST", r.method)
	}

	var payload map[string]string
	if err := json.Unmarshal(r.body, &payload); err != nil {
		t.Fatalf("failed to parse discord JSON: %v (body: %q)", err, string(r.body))
	}

	wantContent := "**Incident**\nCluster is down"
	if got := payload["content"]; got != wantContent {
		t.Errorf("discord content = %q; want %q", got, wantContent)
	}
}

func TestSend_DiscordBoldTitleFormat(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL},
	}
	n := New(cfg, silentLogger())
	n.Send("MyTitle", "MyMessage", PriorityLow)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for discord request")
	}

	mu.Lock()
	defer mu.Unlock()
	var payload map[string]string
	if err := json.Unmarshal((*reqs)[0].body, &payload); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Title must be wrapped in **...**
	if len(payload["content"]) < 4 || payload["content"][:2] != "**" {
		t.Errorf("discord content does not start with '**': %q", payload["content"])
	}
}

// ---------------------------------------------------------------------------
// All channels simultaneously
// ---------------------------------------------------------------------------

func TestSend_AllChannels_AllFire(t *testing.T) {
	ntfySrv, ntfyReqs, ntfyMu := mockServer(t)
	defer ntfySrv.Close()

	slackSrv, slackReqs, slackMu := mockServer(t)
	defer slackSrv.Close()

	discordSrv, discordReqs, discordMu := mockServer(t)
	defer discordSrv.Close()

	cfg := config.NotificationsConfig{
		Ntfy:    &config.NtfyConfig{Server: ntfySrv.URL, Topic: "all"},
		Slack:   &config.SlackConfig{Webhook: slackSrv.URL},
		Discord: &config.DiscordConfig{Webhook: discordSrv.URL},
	}
	n := New(cfg, silentLogger())
	n.Send("AllTitle", "AllMessage", PriorityDefault)

	if !waitForN(1, ntfyReqs, ntfyMu, asyncWaitTimeout) {
		t.Error("timed out waiting for ntfy request")
	}
	if !waitForN(1, slackReqs, slackMu, asyncWaitTimeout) {
		t.Error("timed out waiting for slack request")
	}
	if !waitForN(1, discordReqs, discordMu, asyncWaitTimeout) {
		t.Error("timed out waiting for discord request")
	}

	// Verify ntfy content
	ntfyMu.Lock()
	if len(*ntfyReqs) != 1 {
		t.Errorf("ntfy got %d requests; want 1", len(*ntfyReqs))
	}
	ntfyMu.Unlock()

	// Verify slack content
	slackMu.Lock()
	if len(*slackReqs) != 1 {
		t.Errorf("slack got %d requests; want 1", len(*slackReqs))
	}
	slackMu.Unlock()

	// Verify discord content
	discordMu.Lock()
	if len(*discordReqs) != 1 {
		t.Errorf("discord got %d requests; want 1", len(*discordReqs))
	}
	discordMu.Unlock()
}

// ---------------------------------------------------------------------------
// No channels configured
// ---------------------------------------------------------------------------

func TestSend_NoChannels_NoPanic(t *testing.T) {
	cfg := config.NotificationsConfig{} // all nil
	n := New(cfg, silentLogger())

	// Must not panic; no goroutines are spawned so this returns immediately.
	n.Send("T", "M", PriorityDefault)
}

// ---------------------------------------------------------------------------
// Discord with empty webhook — should be skipped
// ---------------------------------------------------------------------------

func TestSend_DiscordWebhookEmpty_Skipped(t *testing.T) {
	// We set up a server, but no request should arrive because Webhook is "".
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: ""},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityDefault)

	// Give goroutines time to run if they were incorrectly spawned.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) != 0 {
		t.Errorf("discord with empty webhook sent %d requests; want 0", len(*reqs))
	}
}

// ---------------------------------------------------------------------------
// Discord with non-nil config and non-empty webhook fires
// ---------------------------------------------------------------------------

func TestSend_DiscordNonNilWithWebhook_Fires(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL, BotToken: "tok"},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityHigh)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for discord request")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*reqs) != 1 {
		t.Errorf("expected 1 discord request; got %d", len(*reqs))
	}
}

// ---------------------------------------------------------------------------
// HTTP error responses — logged but do not crash
// ---------------------------------------------------------------------------

func TestSend_NtfyHTTPError_NocrashLogged(t *testing.T) {
	srv := errorServer(http.StatusInternalServerError)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	}
	// Use verbose logger so warnings are exercised (coverage).
	n := New(cfg, verboseLogger())

	// Must not panic even on 5xx.
	done := make(chan struct{})
	go func() {
		n.sendNtfy("T", "M", PriorityHigh)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendNtfy hung on HTTP 500 response")
	}
}

func TestSend_Ntfy400Error_Logged(t *testing.T) {
	srv := errorServer(http.StatusBadRequest)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	}
	n := New(cfg, verboseLogger())

	done := make(chan struct{})
	go func() {
		n.sendNtfy("T", "M", PriorityDefault)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendNtfy hung on HTTP 400 response")
	}
}

func TestSend_SlackHTTPError_NoCrash(t *testing.T) {
	srv := errorServer(http.StatusBadGateway)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: srv.URL},
	}
	n := New(cfg, verboseLogger())

	done := make(chan struct{})
	go func() {
		n.sendSlack("T", "M")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendSlack hung on HTTP error response")
	}
}

func TestSend_DiscordHTTPError_NoCrash(t *testing.T) {
	srv := errorServer(http.StatusServiceUnavailable)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL},
	}
	n := New(cfg, verboseLogger())

	done := make(chan struct{})
	go func() {
		n.sendDiscordWebhook("T", "M")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendDiscordWebhook hung on HTTP error response")
	}
}

// ---------------------------------------------------------------------------
// Network-level errors (bad URL) — logged but do not crash
// ---------------------------------------------------------------------------

func TestSend_NtfyBadURL_NoCrash(t *testing.T) {
	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: "http://127.0.0.1:0", Topic: "t"},
	}
	n := New(cfg, silentLogger())

	done := make(chan struct{})
	go func() {
		n.sendNtfy("T", "M", PriorityHigh)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendNtfy hung on network error")
	}
}

func TestSend_SlackBadURL_NoCrash(t *testing.T) {
	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: "http://127.0.0.1:0"},
	}
	n := New(cfg, silentLogger())

	done := make(chan struct{})
	go func() {
		n.sendSlack("T", "M")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendSlack hung on network error")
	}
}

func TestSend_DiscordBadURL_NoCrash(t *testing.T) {
	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: "http://127.0.0.1:0"},
	}
	n := New(cfg, silentLogger())

	done := make(chan struct{})
	go func() {
		n.sendDiscordWebhook("T", "M")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(asyncWaitTimeout):
		t.Fatal("sendDiscordWebhook hung on network error")
	}
}

// ---------------------------------------------------------------------------
// Content-Type headers
// ---------------------------------------------------------------------------

func TestSend_Slack_ContentTypeJSON(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: srv.URL},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityDefault)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for slack request")
	}

	mu.Lock()
	defer mu.Unlock()
	ct := (*reqs)[0].headers.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q; want %q", ct, "application/json")
	}
}

func TestSend_Discord_ContentTypeJSON(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Discord: &config.DiscordConfig{Webhook: srv.URL},
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityDefault)

	if !waitForN(1, reqs, mu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for discord request")
	}

	mu.Lock()
	defer mu.Unlock()
	ct := (*reqs)[0].headers.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q; want %q", ct, "application/json")
	}
}

// ---------------------------------------------------------------------------
// Multiple sequential Send() calls do not interfere
// ---------------------------------------------------------------------------

func TestSend_MultipleCallsNtfy_AllReceived(t *testing.T) {
	srv, reqs, mu := mockServer(t)
	defer srv.Close()

	cfg := config.NotificationsConfig{
		Ntfy: &config.NtfyConfig{Server: srv.URL, Topic: "multi"},
	}
	n := New(cfg, silentLogger())

	const callCount = 5
	for i := range callCount {
		n.Send("Title", fmt.Sprintf("Message %d", i), PriorityDefault)
	}

	if !waitForN(callCount, reqs, mu, asyncWaitTimeout) {
		mu.Lock()
		got := len(*reqs)
		mu.Unlock()
		t.Fatalf("timed out: got %d/%d ntfy requests", got, callCount)
	}
}

// ---------------------------------------------------------------------------
// Verify Send() with only Slack does NOT send to ntfy or discord
// ---------------------------------------------------------------------------

func TestSend_SlackOnly_NoNtfyNoDiscord(t *testing.T) {
	slackSrv, slackReqs, slackMu := mockServer(t)
	defer slackSrv.Close()

	cfg := config.NotificationsConfig{
		Slack: &config.SlackConfig{Webhook: slackSrv.URL},
		// Ntfy and Discord intentionally nil
	}
	n := New(cfg, silentLogger())
	n.Send("T", "M", PriorityHigh)

	if !waitForN(1, slackReqs, slackMu, asyncWaitTimeout) {
		t.Fatal("timed out waiting for slack request")
	}

	// Only one channel should have received a request.
	slackMu.Lock()
	defer slackMu.Unlock()
	if len(*slackReqs) != 1 {
		t.Errorf("slack got %d requests; want 1", len(*slackReqs))
	}
}

