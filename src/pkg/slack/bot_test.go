package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/chat"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestBot(apiBase string) *Bot {
	b := NewBot(Config{AppToken: "xapp-test", BotToken: "xoxb-test", ChannelID: "C1"}, discardLogger())
	b.apiBase = apiBase
	b.reconnectBase = time.Millisecond
	b.reconnectMax = time.Millisecond
	return b
}

func TestSendPostMessageSuccessAndMrkdwnSplit(t *testing.T) {
	var posts []map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Fatalf("auth = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		posts = append(posts, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer ts.Close()

	b := newTestBot(ts.URL)
	long := strings.Repeat("a", slackMessageLimit-5) + "\n\n**bold** and `code`"
	if err := b.Send(long); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("post count = %d, want 2", len(posts))
	}
	if posts[0]["channel"] != "C1" || len([]rune(posts[0]["text"])) > slackMessageLimit {
		t.Fatalf("bad first post: %+v", posts[0])
	}
	if posts[1]["text"] != "*bold* and `code`" {
		t.Fatalf("translated text = %q", posts[1]["text"])
	}
}

func TestSendHonorsRetryAfterOn429(t *testing.T) {
	var slept time.Duration
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	b := newTestBot(ts.URL)
	b.sleep = func(d time.Duration) { slept = d }
	if err := b.Send("hello"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("Send error = %v, want 429", err)
	}
	if slept != 2*time.Second {
		t.Fatalf("slept = %v, want 2s", slept)
	}
}

func TestSendAPIError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "http error", status: http.StatusInternalServerError, body: "boom"},
		{name: "slack not ok", status: http.StatusOK, body: `{"ok":false,"error":"channel_not_found"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()
			if err := newTestBot(ts.URL).Send("hello"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSetTopicMissingScopeDegradesToUnsupported(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversations.setTopic" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope"})
	}))
	defer ts.Close()

	err := newTestBot(ts.URL).SetTopic("topic")
	if !errors.Is(err, chat.ErrTopicUnsupported) {
		t.Fatalf("SetTopic error = %v, want ErrTopicUnsupported", err)
	}
}

func TestFacadeWiring(t *testing.T) {
	b := NewBot(Config{AppToken: "a", BotToken: "b", ChannelID: "c", AllowedUsers: []string{"U1"}}, discardLogger())
	if b.Name() != "slack" {
		t.Fatalf("Name = %q", b.Name())
	}
	SetAgentIdentities(map[string]AgentIdentity{"scanner": {Emoji: "🔎", Color: 1}})
	SetAgentAliases(map[string]string{"scan": "scanner"})
	b.SetAgentNames([]string{"scanner"})
	b.RegisterCommand("ping", func(context.Context, string) (string, error) { return "pong", nil })
}

func TestStartValidationAndSuccess(t *testing.T) {
	if err := NewBot(Config{}, discardLogger()).Start(context.Background()); err == nil {
		t.Fatal("expected missing config error")
	}
	b := NewBot(Config{AppToken: "a", BotToken: "b", ChannelID: "c"}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
}

func TestOpenSocketURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps.connections.open" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xapp-test" {
			t.Fatalf("auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "ws://example/socket"})
	}))
	defer ts.Close()
	url, err := newTestBot(ts.URL).openSocketURL(context.Background())
	if err != nil || url != "ws://example/socket" {
		t.Fatalf("openSocketURL = %q, %v", url, err)
	}
}

func TestListenAcksDeliversAndReconnects(t *testing.T) {
	var connections atomic.Int64
	var acks atomic.Int64
	acked := make(chan struct{}, 1)
	var apiBase string
	upgrader := websocket.Upgrader{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(apiBase+"/socket", "http", "ws", 1)})
		case "/socket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if connections.Add(1) == 1 {
				_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "e-disconnect", Type: "disconnect", Reason: "refresh_requested"})
				_, _, _ = conn.ReadMessage()
				return
			}
			payload := eventPayload{Event: slackEvent{Type: "message", Channel: "C1", Text: "!status", User: "U1", TS: "123.4"}}
			body, _ := json.Marshal(payload)
			_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "e-message", Type: "events_api", Payload: body})
			_, ack, _ := conn.ReadMessage()
			if strings.Contains(string(ack), "e-message") {
				acks.Add(1)
				acked <- struct{}{}
			}
			<-r.Context().Done()
		}
	}))
	defer ts.Close()
	apiBase = ts.URL

	b := newTestBot(ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := make(chan chat.Message, 1)
	go b.Listen(ctx, func(msg chat.Message) { delivered <- msg })

	select {
	case msg := <-delivered:
		if msg.Text != "!status" || msg.AuthorID != "U1" || msg.FromBot {
			t.Fatalf("delivered = %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for delivered message")
	}
	select {
	case <-acked:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ack")
	}
	cancel()
	if connections.Load() < 2 {
		t.Fatalf("connections = %d, want reconnect", connections.Load())
	}
	if acks.Load() != 1 {
		t.Fatalf("message acks = %d, want 1", acks.Load())
	}
}

func TestListenFiltersChannelAndMarksBots(t *testing.T) {
	b := newTestBot("http://unused")
	events := []slackEvent{
		{Type: "message", Channel: "other", Text: "skip", User: "U1"},
		{Type: "reaction_added", Channel: "C1", Text: "skip", User: "U1"},
		{Type: "message", Channel: "C1", Text: "bot", User: "B1", BotID: "BID", TS: "1"},
	}
	for _, ev := range events[:2] {
		if ev.Type == "message" && ev.Channel == b.channelID {
			t.Fatal("bad fixture")
		}
	}
	msg := chat.Message{ID: events[2].TS, Text: events[2].Text, AuthorID: events[2].User, FromBot: events[2].BotID != "" || events[2].Subtype == "bot_message"}
	if !msg.FromBot {
		t.Fatal("expected bot-authored message")
	}
}

func TestMarkdownToMrkdwn(t *testing.T) {
	tests := map[string]string{
		"**bold**":                        "*bold*",
		"keep `**code**` literal":         "keep `**code**` literal",
		"mix **bold** and `code` here":    "mix *bold* and `code` here",
		"see [docs](https://example.com)": "see <https://example.com|docs>",
		"keep `[x](y)` literal":           "keep `[x](y)` literal",
	}
	for in, want := range tests {
		if got := markdownToMrkdwn(in); got != want {
			t.Fatalf("markdownToMrkdwn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitSlackMessageHardSplitsLongParagraph(t *testing.T) {
	parts := splitSlackMessage(strings.Repeat("界", slackMessageLimit+5))
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	for _, part := range parts {
		if len([]rune(part)) > slackMessageLimit {
			t.Fatalf("part too long: %d", len([]rune(part)))
		}
	}
}

func TestWrappersSendMessageAndBackendSetTopic(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer ts.Close()
	b := newTestBot(ts.URL)
	if err := b.SendMessage("hello"); err != nil {
		t.Fatalf("SendMessage error: %v", err)
	}
	if err := b.slackBackend.SetTopic("topic"); err != nil {
		t.Fatalf("backend SetTopic error: %v", err)
	}
	if strings.Join(paths, ",") != "/chat.postMessage,/conversations.setTopic" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestListenStopsWhenContextAlreadyCanceled(t *testing.T) {
	b := newTestBot("http://unused")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Listen(ctx, func(chat.Message) {})
}

func TestOpenSocketURLErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not ok", body: `{"ok":false,"error":"invalid_auth"}`},
		{name: "no url", body: `{"ok":true}`},
		{name: "bad json", body: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()
			if _, err := newTestBot(ts.URL).openSocketURL(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPostJSONMarshalError(t *testing.T) {
	b := newTestBot("http://unused")
	if err := b.postJSON("/chat.postMessage", map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestRetryAfterInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "0", "-1"} {
		if got := retryAfter(in); got != 0 {
			t.Fatalf("retryAfter(%q) = %v, want 0", in, got)
		}
	}
}

func TestConsumeSocketIgnoresMalformedAndFiltersThenMarksBot(t *testing.T) {
	upgrader := websocket.Upgrader{}
	apiBase := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(apiBase+"/socket", "http", "ws", 1)})
		case "/socket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte("{"))
			_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "ignored", Type: "hello"})
			_, _, _ = conn.ReadMessage()
			wrongPayload, _ := json.Marshal(eventPayload{Event: slackEvent{Type: "message", Channel: "WRONG", Text: "skip", User: "U1", TS: "1"}})
			_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "wrong-channel", Type: "events_api", Payload: wrongPayload})
			_, _, _ = conn.ReadMessage()
			botPayload, _ := json.Marshal(eventPayload{Event: slackEvent{Type: "message", Channel: "C1", Text: "bot", User: "U2", Subtype: "bot_message", TS: "2"}})
			_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "bot-message", Type: "events_api", Payload: botPayload})
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"))
		}
	}))
	defer ts.Close()
	apiBase = ts.URL

	b := newTestBot(ts.URL)
	var delivered []chat.Message
	err := b.consumeSocket(context.Background(), func(msg chat.Message) { delivered = append(delivered, msg) })
	if err == nil {
		t.Fatal("expected close error")
	}
	if len(delivered) != 1 || !delivered[0].FromBot || delivered[0].ID != "2" {
		t.Fatalf("delivered = %+v", delivered)
	}
}

func TestSplitSlackMessageEmptyAndCombinesParagraphs(t *testing.T) {
	if got := splitSlackMessage(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty split = %#v", got)
	}
	got := splitSlackMessage("one\n\ntwo")
	if len(got) != 1 || got[0] != "one\n\ntwo" {
		t.Fatalf("paragraph split = %#v", got)
	}
}

func TestListenReconnectDelayStopsOnCancelAfterOpenError(t *testing.T) {
	calls := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
	}))
	defer ts.Close()
	b := newTestBot(ts.URL)
	b.reconnectBase = 50 * time.Millisecond
	b.reconnectMax = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.Listen(ctx, func(chat.Message) {})
		close(done)
	}()
	<-calls
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Listen did not stop after cancel")
	}
}

func TestCallSlackNewRequestError(t *testing.T) {
	b := newTestBot("http://example.com/\x00")
	if _, err := b.callSlack(context.Background(), "/chat.postMessage", nil, "token"); err == nil {
		t.Fatal("expected request construction error")
	}
}

func TestTakeRunesNonPositive(t *testing.T) {
	if got := takeRunes("abc", 0); got != "" {
		t.Fatalf("takeRunes n=0 = %q, want empty", got)
	}
}

func TestParseMarkdownLinkRejectsMalformed(t *testing.T) {
	for _, in := range []string{"[x]", "[](/u)", "[x]()"} {
		if _, _, _, ok := parseMarkdownLink(in); ok {
			t.Fatalf("parseMarkdownLink(%q) ok, want false", in)
		}
	}
}
