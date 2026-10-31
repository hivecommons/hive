package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noRateLimitSleep stops the 429 tests from waiting out retry_after.
func noRateLimitSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	prev := rateLimitSleep
	rateLimitSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { rateLimitSleep = prev })
	return &slept
}

func TestPostMessageNonce_SendsEnforcedNonceAndReturnsID(t *testing.T) {
	var path string
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"id":"m-1"}`))
	}))
	defer ts.Close()

	id, err := newTestBot(ts, "status").PostMessageNonce("rfc", "RFC opened", "abc123")
	if err != nil {
		t.Fatalf("PostMessageNonce: %v", err)
	}
	if id != "m-1" {
		t.Fatalf("id = %q, want m-1", id)
	}
	if !strings.HasSuffix(path, "/channels/rfc/messages") {
		t.Fatalf("path = %q", path)
	}
	if got["nonce"] != "abc123" || got["enforce_nonce"] != true {
		t.Fatalf("nonce payload = %v, want nonce abc123 with enforce_nonce true", got)
	}
	if mentions, _ := got["allowed_mentions"].(map[string]any); mentions == nil {
		t.Fatalf("allowed_mentions missing: %v", got)
	}
}

func TestPostMessageNonce_Errors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	b := newTestBot(ts, "status")
	if _, err := b.PostMessageNonce(" ", "x", "n"); err == nil {
		t.Fatal("empty channel must error")
	}
	if _, err := b.PostMessageNonce("rfc", "x", "n"); err == nil {
		t.Fatal("a response without an id must error")
	}
	if _, err := b.PostMessageNonce("rfc", "x", strings.Repeat("n", 26)); err == nil {
		t.Fatal("a nonce over 25 characters must error")
	}
}

func TestStartThreadFromMessage_PostsNameAndReturnsThreadID(t *testing.T) {
	var path, method string
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"id":"t-9","guild_id":"g-1"}`))
	}))
	defer ts.Close()

	long := strings.Repeat("é", 150)
	id, err := newTestBot(ts, "status").StartThreadFromMessage("rfc", "m-1", long)
	if err != nil {
		t.Fatalf("StartThreadFromMessage: %v", err)
	}
	if id != "t-9" {
		t.Fatalf("id = %q, want t-9", id)
	}
	if method != http.MethodPost || !strings.HasSuffix(path, "/channels/rfc/messages/m-1/threads") {
		t.Fatalf("%s %s", method, path)
	}
	if name, _ := got["name"].(string); len([]rune(name)) != threadNameLimit {
		t.Fatalf("name has %d runes, want %d", len([]rune(name)), threadNameLimit)
	}
}

func TestStartThreadFromMessage_Errors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Missing Permissions"}`))
	}))
	defer ts.Close()
	b := newTestBot(ts, "status")
	if _, err := b.StartThreadFromMessage("", "m", "n"); err == nil {
		t.Fatal("empty channel must error")
	}
	if _, err := b.StartThreadFromMessage("c", "", "n"); err == nil {
		t.Fatal("empty message must error")
	}
	if _, err := b.StartThreadFromMessage("c", "m", "  "); err == nil {
		t.Fatal("empty name must error")
	}
	if _, err := b.StartThreadFromMessage("c", "m", "n"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want the 403", err)
	}
}

func TestRecentMessages_BoundsLimitAndMarksOwnMessages(t *testing.T) {
	var query string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/@me"):
			_, _ = w.Write([]byte(`{"id":"bot-1"}`))
		case strings.HasSuffix(r.URL.Path, "/channels/rfc/messages"):
			query = r.URL.RawQuery
			_, _ = w.Write([]byte(`[{"id":"2","content":"mine","nonce":"n-2","author":{"id":"bot-1","bot":true}},{"id":"1","content":"theirs","nonce":5,"author":{"id":"other","bot":true}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	msgs, err := newTestBot(ts, "status").RecentMessages("rfc", 500)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if query != "limit=50" {
		t.Fatalf("query = %q, want the limit capped at 50", query)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if !msgs[0].Self || msgs[0].Nonce != "n-2" || msgs[0].Content != "mine" {
		t.Fatalf("own message = %+v", msgs[0])
	}
	if msgs[1].Self {
		t.Fatalf("another bot's message must not count as ours: %+v", msgs[1])
	}
}

func TestRecentMessages_SelfLookupFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	if _, err := newTestBot(ts, "status").RecentMessages("rfc", 10); err == nil {
		t.Fatal("an unresolved bot identity must error rather than adopt foreign messages")
	}
	if _, err := newTestBot(ts, "status").RecentMessages(" ", 10); err == nil {
		t.Fatal("empty channel must error")
	}
}

func TestThreadURL_UsesGuildOfTheThreadAndCaches(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"t-9","guild_id":"g-1"}`))
	}))
	defer ts.Close()
	b := newTestBot(ts, "status")
	for i := 0; i < 2; i++ {
		u, err := b.ThreadURL("t-9")
		if err != nil {
			t.Fatalf("ThreadURL: %v", err)
		}
		if u != "https://discord.com/channels/g-1/t-9" {
			t.Fatalf("url = %q", u)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d channel lookups, want the guild cached after the first", calls.Load())
	}
}

func TestThreadURL_NoGuild(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"t-9"}`))
	}))
	defer ts.Close()
	if _, err := newTestBot(ts, "status").ThreadURL("t-9"); err == nil {
		t.Fatal("a thread without a guild id has no URL")
	}
	if _, err := newTestBot(ts, "status").ThreadURL(""); err == nil {
		t.Fatal("empty thread id must error")
	}
}

func TestRateLimit_RetriesAfterRetryAfterThenSucceeds(t *testing.T) {
	slept := noRateLimitSleep(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":1.5,"global":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"m-2"}`))
	}))
	defer ts.Close()

	id, err := newTestBot(ts, "status").PostMessageNonce("rfc", "x", "n")
	if err != nil || id != "m-2" {
		t.Fatalf("id=%q err=%v, want m-2 after one retry", id, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d requests, want 2", calls.Load())
	}
	if len(*slept) != 1 || (*slept)[0] != 1500*time.Millisecond {
		t.Fatalf("slept %v, want one 1.5s wait", *slept)
	}
}

func TestRateLimit_GivesUpAfterBoundedRetries(t *testing.T) {
	slept := noRateLimitSleep(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	if _, err := newTestBot(ts, "status").StartThreadFromMessage("c", "m", "n"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want the 429 surfaced", err)
	}
	if want := int32(maxRateLimitRetries + 1); calls.Load() != want {
		t.Fatalf("%d requests, want %d", calls.Load(), want)
	}
	for _, d := range *slept {
		if d > maxRetryAfter {
			t.Fatalf("waited %v, want each wait capped at %v", d, maxRetryAfter)
		}
	}
}

func TestSendMessageTo_RetriesOn429(t *testing.T) {
	noRateLimitSleep(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := newTestBot(ts, "status").SendMessageTo("rfc", "x"); err != nil {
		t.Fatalf("SendMessageTo: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d requests, want 2", calls.Load())
	}
}

func TestStartThreadFromMessage_ExistingThreadSharesTheMessageID(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":160004,"message":"A thread has already been created for this message"}`))
	}))
	defer ts.Close()
	id, err := newTestBot(ts, "status").StartThreadFromMessage("rfc", "m-1", "#1 x")
	if err != nil || id != "m-1" {
		t.Fatalf("id=%q err=%v, want the message id as the existing thread", id, err)
	}
}
