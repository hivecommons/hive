package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/chat"
)

// Regression tests for hivecommons/hive#9158.

func TestMarkdownToHTMLAlwaysBalancesCodeTags(t *testing.T) {
	tests := map[string]string{
		"don't use `foo":            "don&#39;t use <code>foo</code>",
		"```\ncode never closed":    "<pre>\ncode never closed</pre>",
		"`a ```b``` c`":             "<code>a ```b``` c</code>",
		"```go\nfmt.Println()\n```": "<pre>\nfmt.Println()\n</pre>",
		"```\nno lang\n```":         "<pre>\nno lang\n</pre>",
		"```not a lang tag\nx\n```": "<pre>not a lang tag\nx\n</pre>",
		"one ` and ```\nfence\n```": "one <code> and ```\nfence\n```</code>",
		"`ok` then ```\nblock\n```": "<code>ok</code> then <pre>\nblock\n</pre>",
	}
	for in, want := range tests {
		got := markdownToHTML(in)
		if got != want {
			t.Errorf("markdownToHTML(%q) = %q, want %q", in, got, want)
		}
		for _, part := range splitTelegramMessage(got) {
			assertTelegramHTMLPart(t, part)
		}
	}
}

func TestSendFallsBackToPlainTextWhenEntitiesAreRejected(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		posts = append(posts, body)
		mu.Unlock()
		if body["parse_mode"] != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
			return
		}
		writeTelegramOK(w, map[string]any{"message_id": 1})
	}))
	defer ts.Close()

	if err := newTestBot(ts.URL).Send("**done** — see `x < y` & [docs](https://example.com)"); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 {
		t.Fatalf("post count = %d, want 2 (HTML attempt + plain fallback)", len(posts))
	}
	plain := posts[1]
	if _, ok := plain["parse_mode"]; ok {
		t.Fatalf("fallback still sets parse_mode: %+v", plain)
	}
	// The outbound dash filter (hivecommons/hive#19103) rewrites the em-dash.
	want := "done - see x < y & docs (https://example.com)"
	if plain["text"] != want {
		t.Fatalf("fallback text = %q, want %q", plain["text"], want)
	}
}

func TestCallTelegramRetriesOnceAfter429(t *testing.T) {
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":2}}`))
			return
		}
		writeTelegramOK(w, map[string]any{"message_id": 1})
	}))
	defer ts.Close()
	b := newTestBot(ts.URL)
	var slept []time.Duration
	b.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if err := b.Send("pong"); err != nil {
		t.Fatalf("Send error after retry_after: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (original + one retry)", calls.Load())
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want [2s]", slept)
	}
}

func TestCallTelegramDoesNotSleepBeforeGivingUpOn429(t *testing.T) {
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":2}}`))
	}))
	defer ts.Close()
	b := newTestBot(ts.URL)
	sleeps := 0
	b.sleep = func(context.Context, time.Duration) error {
		sleeps++
		return nil
	}
	if err := b.callTelegram(context.Background(), "sendMessage", map[string]string{"x": "y"}, nil); err == nil {
		t.Fatal("expected error after persistent 429")
	}
	if calls.Load() != 2 || sleeps != 1 {
		t.Fatalf("calls=%d sleeps=%d, want 2 requests and 1 wait", calls.Load(), sleeps)
	}
}

// fakeUpdatesServer models the Bot API update queue: updates stay queued until
// a getUpdates call confirms them with a higher offset, and a negative offset
// forgets everything but the last -offset updates.
type fakeUpdatesServer struct {
	mu      sync.Mutex
	queue   []map[string]any
	offsets []any
}

func (f *fakeUpdatesServer) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offsets = append(f.offsets, body["offset"])
	off, hasOffset := body["offset"].(float64)
	switch {
	case hasOffset && off < 0:
		if keep := int(-off); len(f.queue) > keep {
			f.queue = f.queue[len(f.queue)-keep:]
		}
	case hasOffset:
		var kept []map[string]any
		for _, u := range f.queue {
			if float64(u["update_id"].(int)) >= off {
				kept = append(kept, u)
			}
		}
		f.queue = kept
	}
	writeTelegramOK(w, append([]map[string]any{}, f.queue...))
}

func (f *fakeUpdatesServer) sentOffsets() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any(nil), f.offsets...)
}

func kickUpdate(id int) map[string]any {
	return map[string]any{
		"update_id": id,
		"message":   map[string]any{"message_id": id, "chat": map[string]any{"id": 42}, "from": map[string]any{"id": 7}, "text": "!kick agent1 deploy prod"},
	}
}

func TestListenDoesNotReplayBacklogAfterRestart(t *testing.T) {
	// Updates left unconfirmed by a previous process (a !kick being handled
	// at crash time, or commands typed during downtime).
	fake := &fakeUpdatesServer{queue: []map[string]any{kickUpdate(99), kickUpdate(100)}}
	ts := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer ts.Close()

	b := newTestBot(ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	var delivered atomic.Int64
	done := make(chan struct{})
	go func() {
		b.Listen(ctx, func(chat.Message) { delivered.Add(1) })
		close(done)
	}()
	testutil.Eventually(t, 2*time.Second, func() bool { return len(fake.sentOffsets()) >= 3 },
		"Listen never reached its third getUpdates call")
	cancel()
	<-done
	if n := delivered.Load(); n != 0 {
		t.Fatalf("%d backlog command(s) replayed after restart", n)
	}
	offsets := fake.sentOffsets()
	if offsets[0] != float64(-1) {
		t.Fatalf("first getUpdates offset = %v, want -1 (drop backlog)", offsets[0])
	}
	if offsets[1] != float64(101) {
		t.Fatalf("second getUpdates offset = %v, want 101 (confirm last backlog update)", offsets[1])
	}
}

func TestListenDeliversNewUpdatesAfterSkippingBacklog(t *testing.T) {
	fake := &fakeUpdatesServer{}
	ts := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer ts.Close()
	b := newTestBot(ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	delivered := make(chan chat.Message, 4)
	done := make(chan struct{})
	go func() {
		b.Listen(ctx, func(m chat.Message) { delivered <- m })
		close(done)
	}()
	defer func() { cancel(); <-done }()

	testutil.Eventually(t, 2*time.Second, func() bool { return len(fake.sentOffsets()) >= 2 },
		"Listen never finished skipping the backlog")
	fake.mu.Lock()
	fake.queue = append(fake.queue, kickUpdate(5))
	fake.mu.Unlock()
	select {
	case m := <-delivered:
		if m.ID != "5" {
			t.Fatalf("delivered = %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("new update never delivered; offsets=%v", fake.sentOffsets())
	}
	select {
	case m := <-delivered:
		t.Fatalf("update delivered twice: %+v", m)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPollOnceSkipsAlreadyConfirmedUpdates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTelegramOK(w, []map[string]any{kickUpdate(10), kickUpdate(11)})
	}))
	defer ts.Close()
	var delivered []string
	var offset int64
	_, err := newTestBot(ts.URL).pollOnce(context.Background(), 11, func(m chat.Message) {
		delivered = append(delivered, m.ID)
	}, func(next int64) { offset = next })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(delivered, ",") != "11" || offset != 12 {
		t.Fatalf("delivered=%v offset=%d, want [11] and 12", delivered, offset)
	}
}
