package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/chat"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestBot(apiBase string) *Bot {
	b := NewBot(Config{BotToken: "123:abc", ChatID: "42", AllowedUsers: []string{"7"}}, discardLogger())
	b.apiBase = apiBase
	b.backoffBase = time.Millisecond
	b.backoffMax = time.Millisecond
	b.sleep = func(time.Duration) {}
	return b
}

func writeTelegramOK(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func TestSendMessageSuccessHTMLAndSplit(t *testing.T) {
	var mu sync.Mutex
	var posts []map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:abc/sendMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		posts = append(posts, body)
		mu.Unlock()
		writeTelegramOK(w, map[string]any{"message_id": 1})
	}))
	defer ts.Close()

	long := strings.Repeat("a", telegramMessageLimit-5) + "\n\n**bold** and `code` [docs](https://example.com?a=1&b=2) <unsafe>"
	if err := newTestBot(ts.URL).SendMessage(long); err != nil {
		t.Fatalf("SendMessage error: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("post count = %d, want 2", len(posts))
	}
	if posts[0]["chat_id"] != "42" || posts[0]["parse_mode"] != "HTML" || len([]rune(posts[0]["text"])) > telegramMessageLimit {
		t.Fatalf("bad first post: %+v", posts[0])
	}
	want := `<b>bold</b> and <code>code</code> <a href="https://example.com?a=1&amp;b=2">docs</a> &lt;unsafe&gt;`
	if posts[1]["text"] != want {
		t.Fatalf("translated = %q, want %q", posts[1]["text"], want)
	}
}

func TestSetTopicNoopAndFacadeWiring(t *testing.T) {
	b := NewBot(Config{BotToken: "t", ChatID: "42", AllowedUsers: []string{"7"}}, discardLogger())
	if b.Name() != "telegram" {
		t.Fatalf("Name = %q", b.Name())
	}
	SetAgentIdentities(map[string]AgentIdentity{"scanner": {Emoji: "🔎", Color: 1}})
	SetAgentAliases(map[string]string{"scan": "scanner"})
	b.SetAgentNames([]string{"scanner"})
	b.RegisterCommand("ping", func(context.Context, string) (string, error) { return "pong", nil })
	if err := b.SetTopic("topic"); err != nil {
		t.Fatalf("SetTopic error: %v", err)
	}
}

func TestStartValidationAndSuccess(t *testing.T) {
	if err := NewBot(Config{}, discardLogger()).Start(context.Background()); err == nil {
		t.Fatal("expected missing config error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewBot(Config{BotToken: "t", ChatID: "42"}, discardLogger()).Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
}

func TestCallTelegramErrorsAndRetryAfter(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantSleep time.Duration
	}{
		{name: "http error", status: http.StatusInternalServerError, body: `{"ok":false,"description":"boom"}`},
		{name: "api not ok", status: http.StatusOK, body: `{"ok":false,"description":"bad request"}`},
		{name: "bad json", status: http.StatusOK, body: `{`},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"ok":false,"description":"retry","parameters":{"retry_after":3}}`, wantSleep: 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()
			b := newTestBot(ts.URL)
			var slept time.Duration
			b.sleep = func(d time.Duration) { slept = d }
			if err := b.callTelegram(context.Background(), "sendMessage", map[string]string{"x": "y"}, nil); err == nil {
				t.Fatal("expected error")
			}
			if slept != tt.wantSleep {
				t.Fatalf("slept = %v, want %v", slept, tt.wantSleep)
			}
		})
	}
}

func TestCallTelegramRequestAndMarshalErrors(t *testing.T) {
	b := newTestBot("http://example.com/\x00")
	if err := b.callTelegram(context.Background(), "sendMessage", map[string]string{}, nil); err == nil {
		t.Fatal("expected request error")
	}
	b = newTestBot("http://unused")
	if err := b.callTelegram(context.Background(), "sendMessage", map[string]any{"bad": func() {}}, nil); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestPollOnceDeliversAndAdvancesOffsetAfterDelivery(t *testing.T) {
	var seenOffset int64 = -1
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if v, ok := body["offset"].(float64); ok {
			seenOffset = int64(v)
		}
		if timeout := int(body["timeout"].(float64)); timeout > longPollTimeoutSeconds {
			t.Fatalf("timeout = %d", timeout)
		}
		writeTelegramOK(w, []map[string]any{{
			"update_id": 100,
			"message":   map[string]any{"message_id": 9, "chat": map[string]any{"id": 42}, "from": map[string]any{"id": 7, "is_bot": false}, "text": "!status"},
		}})
	}))
	defer ts.Close()

	b := newTestBot(ts.URL)
	var delivered []chat.Message
	var offset int64
	polled, err := b.pollOnce(context.Background(), 55, func(msg chat.Message) {
		if offset != 0 {
			t.Fatalf("offset advanced before delivery: %d", offset)
		}
		delivered = append(delivered, msg)
	}, func(next int64) { offset = next })
	if err != nil || !polled {
		t.Fatalf("pollOnce = %v, %v", polled, err)
	}
	if seenOffset != 55 || offset != 101 {
		t.Fatalf("seenOffset=%d offset=%d", seenOffset, offset)
	}
	if len(delivered) != 1 || delivered[0].AuthorID != "7" || delivered[0].ID != "9" || delivered[0].FromBot {
		t.Fatalf("delivered = %+v", delivered)
	}
}

func TestPollOnceSkipsWrongChatEmptyTextAndMarksBot(t *testing.T) {
	updates := []map[string]any{
		{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 41}, "from": map[string]any{"id": 7}, "text": "skip"}},
		{"update_id": 2, "message": map[string]any{"message_id": 2, "chat": map[string]any{"id": 42}, "from": map[string]any{"id": 7}, "text": ""}},
		{"update_id": 3, "message": map[string]any{"message_id": 3, "chat": map[string]any{"id": 42}, "from": map[string]any{"id": 8, "is_bot": true}, "text": "bot"}},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeTelegramOK(w, updates) }))
	defer ts.Close()
	var delivered []chat.Message
	var offset int64
	polled, err := newTestBot(ts.URL).pollOnce(context.Background(), 0, func(msg chat.Message) { delivered = append(delivered, msg) }, func(next int64) { offset = next })
	if err != nil || !polled || offset != 4 {
		t.Fatalf("pollOnce = %v, %v offset=%d", polled, err, offset)
	}
	if len(delivered) != 1 || !delivered[0].FromBot || delivered[0].AuthorID != "8" {
		t.Fatalf("delivered = %+v", delivered)
	}
}

func TestListenBackoffResetsAfterSuccessfulPoll(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		n := calls.Add(1)
		if n == 3 {
			writeTelegramOK(w, []any{})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "temporary"})
	}))
	defer ts.Close()
	b := newTestBot(ts.URL)
	b.backoffBase = 20 * time.Millisecond
	b.backoffMax = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Listen(ctx, func(chat.Message) {})
	deadline := time.After(2 * time.Second)
	for calls.Load() < 4 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for polls; calls=%d", calls.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 4 {
		t.Fatalf("times = %d", len(times))
	}
	if delay := times[3].Sub(times[2]); delay > 60*time.Millisecond {
		t.Fatalf("post-success delay = %v, want reset near base", delay)
	}
}

func TestGetUpdatesContextCancelUnblocks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer func() { close(release); ts.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := newTestBot(ts.URL).pollOnce(ctx, 0, func(chat.Message) {}, func(int64) {})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not unblock after context cancellation")
	}
}

func TestMarkdownToHTML(t *testing.T) {
	tests := map[string]string{
		"**bold**":                        "<b>bold</b>",
		"keep `**code**` literal":         "keep <code>**code**</code> literal",
		"```\n<unsafe>\n```":              "<pre>\n&lt;unsafe&gt;\n</pre>",
		"see [docs](https://example.com)": `see <a href="https://example.com">docs</a>`,
		"plain < & >":                     "plain &lt; &amp; &gt;",
		"literal ** without close":        "literal ** without close",
	}
	for in, want := range tests {
		if got := markdownToHTML(in); got != want {
			t.Fatalf("markdownToHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitTelegramMessageBalancesFences(t *testing.T) {
	inputs := []string{
		"before\n\n<pre>\n" + strings.Repeat("a", telegramMessageLimit) + "\n\ninside\n</pre>\nafter",
		"<pre>\n" + strings.Repeat("b", 5000) + "\n</pre>",
	}
	for _, input := range inputs {
		parts := splitTelegramMessage(input)
		if len(parts) < 2 {
			t.Fatalf("expected split for length %d", len([]rune(input)))
		}
		for i, part := range parts {
			if len([]rune(part)) > telegramMessageLimit {
				t.Fatalf("part %d length = %d", i, len([]rune(part)))
			}
			if strings.Count(part, "<pre>") != strings.Count(part, "</pre>") {
				t.Fatalf("part %d has unbalanced fences: %q", i, part[:min(len(part), 80)])
			}
		}
	}
}

func TestSplitTelegramMessageEmptyAndTakeRunes(t *testing.T) {
	if got := splitTelegramMessage(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty split = %#v", got)
	}
	if got := takeRunes("abc", 0); got != "" {
		t.Fatalf("takeRunes(0) = %q", got)
	}
	if got := takeRunes("界abc", 2); got != "界a" || len([]rune(got)) != 2 {
		t.Fatalf("takeRunes unicode = %q", got)
	}
}

func TestPollOnceResultDecodeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer ts.Close()
	if _, err := newTestBot(ts.URL).pollOnce(context.Background(), 0, func(chat.Message) {}, func(int64) {}); err == nil {
		t.Fatal("expected result decode error")
	}
}

func TestParseMarkdownLinkRejectsMalformed(t *testing.T) {
	for _, in := range []string{"[x]", "[](/u)", "[x]()"} {
		if _, _, _, ok := parseMarkdownLink(in); ok {
			t.Fatalf("parseMarkdownLink(%q) ok, want false", in)
		}
	}
}

func TestHandleUpdateSupportsNegativeChatID(t *testing.T) {
	b := NewBot(Config{BotToken: "t", ChatID: "-100123"}, discardLogger())
	var upd update
	upd.UpdateID = 1
	upd.Message.MessageID = 2
	upd.Message.Chat.ID = -100123
	upd.Message.From.ID = 7
	upd.Message.Text = "!status"
	var got chat.Message
	b.handleUpdate(upd, func(msg chat.Message) { got = msg })
	if got.AuthorID != strconv.FormatInt(7, 10) || got.Text != "!status" {
		t.Fatalf("message = %+v", got)
	}
}

func TestListenStopsWhenContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	newTestBot("http://unused").Listen(ctx, func(chat.Message) { t.Fatal("unexpected delivery") })
}

func TestCallTelegramNetworkError(t *testing.T) {
	b := newTestBot("http://127.0.0.1:1")
	if err := b.callTelegram(context.Background(), "sendMessage", map[string]string{}, nil); err == nil {
		t.Fatal("expected network error")
	}
}
