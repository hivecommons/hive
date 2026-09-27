package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/chat"
)

// redirectTransport rewrites every outgoing request so that requests targeting
// discordAPIBase (https://discord.com/api/v10) are transparently redirected to
// the given test server URL.  This lets us exercise the real Bot code paths
// without modifying the production source.
type redirectTransport struct {
	target string // base URL of the httptest.Server, e.g. "http://127.0.0.1:PORT"
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request so we can mutate the URL safely.
	cloned := req.Clone(req.Context())

	parsed, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}

	cloned.URL.Scheme = parsed.Scheme
	cloned.URL.Host = parsed.Host
	// Host header must match the rewritten host to avoid mismatches.
	cloned.Host = parsed.Host

	return http.DefaultTransport.RoundTrip(cloned)
}

// newTestBot builds a Bot wired to the given httptest server.
func newTestBot(ts *httptest.Server, channelID string) *Bot {
	b := NewBot(Config{
		Token:     "test-token",
		ChannelID: channelID,
		// Test messages are authored as "uid" (makeMsg) or "u1" (poll-loop
		// fixtures); allowlist both so command-dispatch tests exercise the handler
		// path. The allowlist gate itself is covered by
		// TestRouteMessage_NonAllowlistedUserBlocked / _EmptyAllowlistBlocksAll.
		AllowedUsers: []string{"uid", "u1"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	b.client = &http.Client{
		Transport: &redirectTransport{target: ts.URL},
		Timeout:   httpTimeoutS * time.Second,
	}
	// Poll in milliseconds so Listen-driven tests finish quickly.
	b.pollInterval = 5 * time.Millisecond

	return b
}

// discardLogger returns a logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ──────────────────────────────────────────────────────────────────────────────
// NewBot
// ──────────────────────────────────────────────────────────────────────────────

func TestNewBot_FieldsSet(t *testing.T) {
	cfg := Config{Token: "tok", ChannelID: "chan"}
	logger := discardLogger()
	b := NewBot(cfg, logger)

	if b.token != "tok" {
		t.Errorf("token: got %q, want %q", b.token, "tok")
	}
	if b.channelID != "chan" {
		t.Errorf("channelID: got %q, want %q", b.channelID, "chan")
	}
	if b.service == nil {
		t.Error("chat service is nil")
	}
	if b.logger != logger {
		t.Error("logger not stored correctly")
	}
	if b.client == nil {
		t.Error("http client is nil")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// RegisterCommand
// ──────────────────────────────────────────────────────────────────────────────

func TestStart_EmptyToken_ReturnsError(t *testing.T) {
	b := NewBot(Config{Token: "", ChannelID: "c"}, discardLogger())
	err := b.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error message should mention token, got: %v", err)
	}
}

func TestStart_WithToken_ReturnsNilAndStartsLoop(t *testing.T) {
	// We need a test server so Listen's HTTP calls don't fail fatally.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := b.Start(ctx)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// SendMessage
// ──────────────────────────────────────────────────────────────────────────────

func TestSendMessage_CorrectURLAndHeaders(t *testing.T) {
	const channelID = "123456789"

	var (
		gotMethod      string
		gotPath        string
		gotAuthHeader  string
		gotContentType string
		gotBody        string
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuthHeader = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")

		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, channelID)
	err := b.SendMessage("hello world")
	if err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}

	wantPath := fmt.Sprintf("/api/v10/channels/%s/messages", channelID)
	if gotPath != wantPath {
		t.Errorf("URL path: got %q, want %q", gotPath, wantPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("HTTP method: got %q, want %q", gotMethod, http.MethodPost)
	}
	if gotAuthHeader != "Bot test-token" {
		t.Errorf("Authorization header: got %q, want %q", gotAuthHeader, "Bot test-token")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", gotContentType, "application/json")
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body is not valid JSON: %v – raw: %s", err, gotBody)
	}
	if payload["content"] != "hello world" {
		t.Errorf("body content: got %q, want %q", payload["content"], "hello world")
	}
}

func TestSendMessage_4xxReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401: Unauthorized"}`))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	err := b.SendMessage("hi")
	if err == nil {
		t.Fatal("expected error for 4xx response, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status code 401, got: %v", err)
	}
}

func TestSendMessage_5xxReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	err := b.SendMessage("hi")
	if err == nil {
		t.Fatal("expected error for 5xx response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500, got: %v", err)
	}
}

func TestSendMessage_NetworkError(t *testing.T) {
	// Point to a server that is immediately closed so the connection is refused.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close() // close before the request is made

	b := newTestBot(ts, "ch")
	err := b.SendMessage("hi")
	if err == nil {
		t.Fatal("expected error for network failure, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// fetchMessages
// ──────────────────────────────────────────────────────────────────────────────

func TestFetchMessages_URLWithoutAfter(t *testing.T) {
	const channelID = "ch1"

	var gotPath string
	var gotRawQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, channelID)
	_, err := b.fetchMessages(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchMessages returned error: %v", err)
	}

	wantPath := fmt.Sprintf("/api/v10/channels/%s/messages", channelID)
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}
	if !strings.Contains(gotRawQuery, "limit=10") {
		t.Errorf("query should contain limit=10, got %q", gotRawQuery)
	}
	if strings.Contains(gotRawQuery, "after=") {
		t.Errorf("query should NOT contain after= when empty, got %q", gotRawQuery)
	}
}

func TestFetchMessages_URLWithAfter(t *testing.T) {
	const (
		channelID = "ch2"
		afterID   = "999"
	)

	var gotRawQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, channelID)
	_, err := b.fetchMessages(context.Background(), afterID)
	if err != nil {
		t.Fatalf("fetchMessages returned error: %v", err)
	}

	if !strings.Contains(gotRawQuery, "after="+afterID) {
		t.Errorf("query should contain after=%s, got %q", afterID, gotRawQuery)
	}
}

func TestFetchMessages_ParsesJSONResponse(t *testing.T) {
	messages := []discordMessage{
		{ID: "1", Content: "!hive help", Author: struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		}{ID: "u1", Bot: false}},
		{ID: "2", Content: "!hive status", Author: struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		}{ID: "bot1", Bot: true}},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messages)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	got, err := b.fetchMessages(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchMessages returned error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].ID != "1" || got[0].Content != "!hive help" {
		t.Errorf("first message mismatch: %+v", got[0])
	}
	if got[1].ID != "2" || !got[1].Author.Bot {
		t.Errorf("second message mismatch: %+v", got[1])
	}
}

func TestFetchMessages_AuthorizationHeader(t *testing.T) {
	var gotAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	_, _ = b.fetchMessages(context.Background(), "")

	if gotAuth != "Bot test-token" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bot test-token")
	}
}

func TestFetchMessages_APIErrorReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Missing Permissions"}`))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	msgs, err := b.fetchMessages(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for 403 response, got nil")
	}
	if msgs != nil {
		t.Error("expected nil messages on error")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status 403, got: %v", err)
	}
}

func TestFetchMessages_InvalidJSONReturnsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	_, err := b.fetchMessages(context.Background(), "")
	if err == nil {
		t.Fatal("expected JSON decode error, got nil")
	}
}

func TestFetchMessages_NetworkError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Close()

	b := newTestBot(ts, "ch")
	_, err := b.fetchMessages(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for network failure, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// routeMessage
// ──────────────────────────────────────────────────────────────────────────────

// routeMessage enqueues replies onto b.msgQueue. drainQueue reads from the
// channel without the production rate-limit sleep.

func TestSendMessage_NewRequestError(t *testing.T) {
	// A null byte in the channel ID makes the URL unparseable by http.NewRequest.
	b := NewBot(Config{Token: "tok", ChannelID: "\x00"}, discardLogger())

	err := b.SendMessage("hi")
	if err == nil {
		t.Fatal("expected error from http.NewRequest with invalid URL, got nil")
	}
}

// TestFetchMessages_NewRequestError covers the http.NewRequest error branch in
// fetchMessages by using a channelID containing a null byte.
func TestFetchMessages_NewRequestError(t *testing.T) {
	b := NewBot(Config{Token: "tok", ChannelID: "\x00"}, discardLogger())

	_, err := b.fetchMessages(context.Background(), "")
	if err == nil {
		t.Fatal("expected error from http.NewRequest with invalid URL, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Listen (integration-level: context cancellation)
// ──────────────────────────────────────────────────────────────────────────────

func TestPollLoop_StopsOnContextCancel(t *testing.T) {
	var pollCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			pollCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")

	ctx, cancel := context.WithCancel(context.Background())

	// Run Listen in a goroutine — it blocks until ctx is cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(msg chat.Message) { b.service.Deliver(ctx, msg) })
	}()

	// Give the loop a moment to start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good — loop exited
	case <-time.After(3 * time.Second):
		t.Fatal("Listen did not stop after context cancellation within timeout")
	}
}

func TestPollLoop_ContinuesOnFetchError(t *testing.T) {
	var requestCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requestCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]discordMessage{})
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(msg chat.Message) { b.service.Deliver(ctx, msg) })
	}()

	// The first poll fails with 500; the loop must keep polling afterwards.
	testutil.Eventually(t, 2*time.Second, func() bool {
		return requestCount.Load() >= 2
	}, "expected the poll loop to continue past a fetch error")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not stop after context cancellation")
	}
}

func TestPollLoop_ProcessesMessagesInReverseOrder(t *testing.T) {
	// Discord returns newest-first; Listen reverses to deliver oldest-first and
	// advances the cursor to the newest ID. The first poll only seeds the
	// cursor (its messages are skipped); the second poll must carry after=2 and
	// its messages must be delivered as 3 then 4.
	var callCount atomic.Int64
	var secondAfter atomic.Value

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode([]discordMessage{
				{ID: "2", Content: "hello"},
				{ID: "1", Content: "world"},
			})
		case 2:
			secondAfter.Store(r.URL.Query().Get("after"))
			_ = json.NewEncoder(w).Encode([]discordMessage{
				{ID: "4", Content: "second"},
				{ID: "3", Content: "first"},
			})
		default:
			_ = json.NewEncoder(w).Encode([]discordMessage{})
		}
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delivered := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(msg chat.Message) { delivered <- msg.ID })
	}()

	var got []string
	for len(got) < 2 {
		select {
		case id := <-delivered:
			got = append(got, id)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for deliveries, got %v", got)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not stop after context cancellation")
	}

	if got[0] != "3" || got[1] != "4" {
		t.Errorf("expected deliveries in oldest-first order [3 4], got %v", got)
	}
	if after, _ := secondAfter.Load().(string); after != "2" {
		t.Errorf("expected second poll to carry after=2 (newest ID from first poll), got %q", after)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Listen – ticker.C branch coverage
//
// newTestBot shortens the poll interval to milliseconds so the ticker.C case
// in Listen (fetchMessages + deliver) is exercised without waiting for the
// production 5 s cadence.
// ──────────────────────────────────────────────────────────────────────────────

// TestPollLoop_TickerSuccessPath exercises the happy-path ticker branch:
// fetchMessages returns a non-bot !ping message and the spine dispatches it.
// Listen skips messages on the first poll (firstPoll=true), so the message
// is served on the second fetch.
func TestPollLoop_TickerSuccessPath(t *testing.T) {
	t.Parallel()

	var fetchCount atomic.Int64
	var sendCount atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			n := fetchCount.Add(1)
			if n == 2 {
				msgs := []discordMessage{
					{ID: "99", Content: "!ping", Author: struct {
						ID  string `json:"id"`
						Bot bool   `json:"bot"`
					}{ID: "u1", Bot: false}},
				}
				_ = json.NewEncoder(w).Encode(msgs)
			} else {
				_ = json.NewEncoder(w).Encode([]discordMessage{})
			}
		case http.MethodPost:
			sendCount.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) {
		return "pong", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(msg chat.Message) { b.service.Deliver(ctx, msg) })
	}()
	// Start drainLoop so enqueued replies are sent via HTTP.
	go b.service.DrainLoop(ctx)

	testutil.Eventually(t, 2*time.Second, func() bool {
		return fetchCount.Load() >= 2 && sendCount.Load() > 0
	}, "expected two ticker fetches and a routed !ping reply")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not stop after context cancellation")
	}

	if fc := fetchCount.Load(); fc < 2 {
		t.Errorf("expected at least two fetchMessages calls via ticker, got %d", fc)
	}
	if sc := sendCount.Load(); sc == 0 {
		t.Error("expected at least one SendMessage call (reply to !ping), got 0")
	}
}

// TestPollLoop_TickerFetchErrorPath exercises the error-continue branch inside
// the ticker.C case: fetchMessages fails and the loop logs the warning and
// continues rather than exiting.
func TestPollLoop_TickerFetchErrorPath(t *testing.T) {
	t.Parallel()

	var fetchAttempts atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchAttempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(msg chat.Message) { b.service.Deliver(ctx, msg) })
	}()

	testutil.Eventually(t, 2*time.Second, func() bool {
		return fetchAttempts.Load() > 1
	}, "expected the poll loop to keep fetching after an error")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not stop after context cancellation")
	}
}
