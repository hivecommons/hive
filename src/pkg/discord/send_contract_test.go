package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/chat"
)

// discordCreateMessageLimit is Discord's Create Message `content` ceiling.
const discordCreateMessageLimit = 2000

// TestReplyLongerThanDiscordLimitIsSplitAndDelivered is the hive#9127
// repro: a `!runs` style reply of 2,235 runes used to be POSTed whole, get
// 400 from Discord, and vanish. The fake enforces Discord's 2,000-rune
// ceiling; every chunk must land with 200 and reassemble the reply.
func TestReplyLongerThanDiscordLimitIsSplitAndDelivered(t *testing.T) {
	var (
		mu       sync.Mutex
		statuses []int
		contents []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		status := http.StatusOK
		if utf8.RuneCountInString(payload["content"]) > discordCreateMessageLimit {
			status = http.StatusBadRequest
		}
		mu.Lock()
		statuses = append(statuses, status)
		contents = append(contents, payload["content"])
		mu.Unlock()
		w.WriteHeader(status)
	}))
	defer ts.Close()

	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "• run-"+strings.Repeat("x", 60)+" — waiting on checkpoint")
	}
	reply := strings.Join(lines, "\n")
	if n := utf8.RuneCountInString(reply); n <= discordCreateMessageLimit {
		t.Fatalf("fixture reply is %d runes, must exceed %d", n, discordCreateMessageLimit)
	}

	b := newTestBot(ts, "ch")
	b.RegisterCommand("runs", func(context.Context, string) (string, error) { return reply, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.service.DrainLoop(ctx)
	b.service.Deliver(ctx, chat.Message{ID: "1", Text: "!runs", AuthorID: "u1"})

	testutil.Eventually(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(contents, "\n") == reply
	}, "reply never reassembled from the posted chunks: %q", contents)
	mu.Lock()
	defer mu.Unlock()
	for i, st := range statuses {
		if st != http.StatusOK {
			t.Fatalf("post %d got %d (content %d runes)", i, st, utf8.RuneCountInString(contents[i]))
		}
	}
	if len(statuses) < 2 {
		t.Fatalf("expected the reply to be split into several posts, got %d", len(statuses))
	}
}

func TestSendMessage_429IsRetryableWithRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		body   string
		want   time.Duration
	}{
		{name: "body retry_after wins", header: "9", body: `{"message":"You are being rate limited.","retry_after":1.5,"global":false}`, want: 1500 * time.Millisecond},
		{name: "header fallback", header: "3", body: `rate limited`, want: 3 * time.Second},
		{name: "no hint", header: "", body: ``, want: 0},
		{name: "capped", header: "", body: `{"retry_after":600}`, want: chat.MaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			err := newTestBot(ts, "ch").SendMessage("hi")
			var retryable *chat.RetryableError
			if !errors.As(err, &retryable) {
				t.Fatalf("SendMessage error = %v, want chat.RetryableError", err)
			}
			if retryable.RetryAfter != tt.want {
				t.Fatalf("RetryAfter = %v, want %v", retryable.RetryAfter, tt.want)
			}
			if !strings.Contains(err.Error(), "429") {
				t.Fatalf("error = %v, want status preserved", err)
			}
		})
	}
}

func TestSendMessage_RetryableOnlyForRateLimitAndServerErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := newTestBot(ts, "ch").SendMessage("hi")
		ts.Close()
		var retryable *chat.RetryableError
		if got := errors.As(err, &retryable); got != tc.retryable {
			t.Fatalf("status %d: retryable = %v, want %v (err %v)", tc.status, got, tc.retryable, err)
		}
	}
}

func TestFetchMessages_429IsRetryableWithRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"retry_after":0.25}`))
	}))
	defer ts.Close()

	_, err := newTestBot(ts, "ch").fetchMessages(context.Background(), "")
	var retryable *chat.RetryableError
	if !errors.As(err, &retryable) || retryable.RetryAfter != 250*time.Millisecond {
		t.Fatalf("fetchMessages error = %v, want retryable with 250ms", err)
	}
}

// TestListen_RateLimitedPollWaitsRetryAfterAndStopsOnCancel proves the
// poller parks for Discord's retry_after instead of re-polling on the next
// tick, and that the park is context-aware.
func TestListen_RateLimitedPollWaitsRetryAfterAndStopsOnCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow ticker test in -short mode")
	}
	polled := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case polled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"retry_after":600}`))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Listen(ctx, func(chat.Message) { t.Error("no message should be delivered") })
	}()
	select {
	case <-polled:
	case <-time.After(2 * pollIntervalS * time.Second):
		t.Fatal("poller never reached the fake Discord API")
	}
	// The poller now owes Discord a (capped) 60 s wait; cancel must cut it.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen stayed parked in the retry_after wait after cancel")
	}
}

func TestDiscordRetryAfterParsesBodyThenHeader(t *testing.T) {
	if got := discordRetryAfter([]byte(`{"retry_after":2.5}`), "7"); got != 2500*time.Millisecond {
		t.Fatalf("body precedence: got %v", got)
	}
	if got := discordRetryAfter([]byte(`not json`), " 4 "); got != 4*time.Second {
		t.Fatalf("header fallback: got %v", got)
	}
	if got := discordRetryAfter(nil, ""); got != 0 {
		t.Fatalf("no hint: got %v", got)
	}
	if got := discordRetryAfter([]byte(`{"retry_after":-1}`), "-2"); got != 0 {
		t.Fatalf("negative hints must be ignored: got %v", got)
	}
}

// Keep the request body readable in the retryable error so operators still
// see Discord's message text in the warning log.
func TestSendMessage_RetryableErrorKeepsBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "maintenance")
	}))
	defer ts.Close()
	err := newTestBot(ts, "ch").SendMessage("hi")
	if err == nil || !strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("error = %v, want body preserved", err)
	}
}
