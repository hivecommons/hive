package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/chat"
)

func testBackend(serverURL string) *matrixBackend {
	return &matrixBackend{
		homeserverURL: serverURL,
		accessToken:   "token",
		roomID:        "!room:example",
		userID:        "@bot:example",
		logger:        slog.Default(),
		client:        &http.Client{Timeout: time.Second},
		sleep:         func(time.Duration) {},
		reconnectBase: time.Millisecond,
		reconnectMax:  2 * time.Millisecond,
	}
}

func TestWhoamiUsesBearerAuthAndContext(t *testing.T) {
	seenAuth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != matrixAPIPath+"/account/whoami" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"user_id":"@bot:example"}`))
	}))
	defer server.Close()

	got, err := testBackend(server.URL).Whoami(context.Background())
	if err != nil {
		t.Fatalf("Whoami error = %v", err)
	}
	if got != "@bot:example" || seenAuth != "Bearer token" {
		t.Fatalf("Whoami = %q auth %q", got, seenAuth)
	}
}

func TestSendPostsMatrixHTMLAndHonorsRateLimit(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var slept time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/send/m.room.message/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if requests == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":25}`))
			return
		}
		var payload matrixMessagePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.MsgType != "m.text" || payload.Body != "**hi** `x` [link](https://example.test)" {
			t.Fatalf("payload = %+v", payload)
		}
		wantHTML := `<strong>hi</strong> <code>x</code> <a href="https://example.test">link</a>`
		if payload.Format != "org.matrix.custom.html" || payload.FormattedBody != wantHTML {
			t.Fatalf("formatted payload = %+v want %q", payload, wantHTML)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	backend := testBackend(server.URL)
	backend.reconnectMax = time.Second
	backend.sleep = func(d time.Duration) { slept = d }
	if err := backend.Send("**hi** `x` [link](https://example.test)"); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if requests != 2 || slept != 25*time.Millisecond {
		t.Fatalf("requests = %d slept = %v", requests, slept)
	}
}

func TestSetTopicDegradesOnForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/state/m.room.topic") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"missing power"}`))
	}))
	defer server.Close()

	if err := testBackend(server.URL).SetTopic("topic"); err != nil {
		t.Fatalf("SetTopic error = %v", err)
	}
}

func TestListenDiscardsFirstSyncFiltersDedupesAndMarksBot(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != matrixAPIPath+"/sync" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("timeout") != fmt.Sprintf("%d", syncTimeoutMS) {
			t.Fatalf("timeout query = %q", r.URL.RawQuery)
		}
		assertSyncFilter(t, r.URL.Query().Get("filter"), "!room:example")
		requests++
		switch requests {
		case 1:
			if r.URL.Query().Get("since") != "" {
				t.Fatalf("first sync since = %q", r.URL.Query().Get("since"))
			}
			_, _ = w.Write([]byte(`{"next_batch":"s0","rooms":{"join":{"!room:example":{"timeline":{"events":[{"event_id":"old","type":"m.room.message","sender":"@old:example","content":{"msgtype":"m.text","body":"old"}}]}}}}}`))
		case 2:
			if r.URL.Query().Get("since") != "s0" {
				t.Fatalf("second sync since = %q", r.URL.Query().Get("since"))
			}
			_, _ = w.Write([]byte(`{"next_batch":"s1","rooms":{"join":{"!room:example":{"timeline":{"events":[{"event_id":"e1","type":"m.room.message","sender":"@alice:example","content":{"msgtype":"m.text","body":"hello"}},{"event_id":"e1","type":"m.room.message","sender":"@alice:example","content":{"msgtype":"m.text","body":"dupe"}},{"event_id":"topic","type":"m.room.topic","sender":"@alice:example","content":{"body":"ignore"}},{"event_id":"e2","type":"m.room.message","sender":"@bot:example","content":{"msgtype":"m.text","body":"bot"}}]},"state":{"events":[]}},"!other:example":{"timeline":{"events":[{"event_id":"other","type":"m.room.message","sender":"@mallory:example","content":{"body":"wrong room"}}]}}}}}`))
		default:
			_, _ = w.Write([]byte(`{"next_batch":"s2"}`))
		}
	}))
	defer server.Close()

	backend := testBackend(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []chat.Message
	backend.Listen(ctx, func(msg chat.Message) {
		got = append(got, msg)
		if len(got) == 2 {
			cancel()
		}
	})

	if len(got) != 2 {
		t.Fatalf("delivered %d messages: %+v", len(got), got)
	}
	if got[0].ID != "e1" || got[0].Text != "hello" || got[0].AuthorID != "@alice:example" || got[0].FromBot {
		t.Fatalf("first message = %+v", got[0])
	}
	if got[1].ID != "e2" || !got[1].FromBot {
		t.Fatalf("second message = %+v", got[1])
	}
}

func TestListenHonorsLimitExceededRetryAfter(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":1}`))
			return
		}

		_, _ = w.Write([]byte(`{"next_batch":"s0"}`))
	}))
	defer server.Close()

	backend := testBackend(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		backend.Listen(ctx, func(chat.Message) {})
	}()
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		seen := requests
		mu.Unlock()
		if seen >= 2 {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("requests = %d, want retry", seen)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestSyncSendsFilterAndParsesOversizedBodyUnderRaisedCap(t *testing.T) {
	large := strings.Repeat("x", maxResponseBodyBytes+1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != matrixAPIPath+"/sync" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("since") != "s0" {
			t.Fatalf("since = %q", r.URL.Query().Get("since"))
		}
		assertSyncFilter(t, r.URL.Query().Get("filter"), "!room:example")
		_, _ = fmt.Fprintf(w, `{"next_batch":"s1","account_data":{"events":[{"content":{"blob":%q}}]}}`, large)
	}))
	defer server.Close()

	resp, err := testBackend(server.URL).sync(context.Background(), "s0")
	if err != nil {
		t.Fatalf("sync error = %v", err)
	}
	if resp.NextBatch != "s1" {
		t.Fatalf("NextBatch = %q", resp.NextBatch)
	}
}

func assertSyncFilter(t *testing.T, rawFilter, roomID string) {
	t.Helper()
	if rawFilter == "" {
		t.Fatal("missing sync filter")
	}
	var filter struct {
		Room struct {
			Rooms []string `json:"rooms"`
			State struct {
				LazyLoadMembers bool `json:"lazy_load_members"`
			} `json:"state"`
			Timeline struct {
				Limit int `json:"limit"`
			} `json:"timeline"`
		} `json:"room"`
	}
	if err := json.Unmarshal([]byte(rawFilter), &filter); err != nil {
		t.Fatalf("invalid sync filter %q: %v", rawFilter, err)
	}
	if len(filter.Room.Rooms) != 1 || filter.Room.Rooms[0] != roomID {
		t.Fatalf("filter rooms = %#v", filter.Room.Rooms)
	}
	if !filter.Room.State.LazyLoadMembers {
		t.Fatal("filter lazy_load_members is false")
	}
	if filter.Room.Timeline.Limit != syncTimelineLimit {
		t.Fatalf("filter timeline limit = %d", filter.Room.Timeline.Limit)
	}
}
func TestMarkdownToMatrixHTML(t *testing.T) {
	in := "**bold** `<tag>`\n```\ncode & more\n```"
	got := markdownToMatrixHTML(in)
	want := "<strong>bold</strong> <code>&lt;tag&gt;</code><br />\n<pre><code><br />\ncode &amp; more<br />\n</code></pre>"
	if got != want {
		t.Fatalf("markdownToMatrixHTML() = %q want %q", got, want)
	}
}

func TestSplitMatrixMessagePreservesRunes(t *testing.T) {
	input := strings.Repeat("å", matrixMessageLimit+2)
	parts := splitMatrixMessage(input)
	if len(parts) != 2 || len([]rune(parts[0])) != matrixMessageLimit || strings.Join(parts, "") != input {
		t.Fatalf("split parts lengths = %d/%d", len(parts), len([]rune(parts[0])))
	}
}

func TestBotWrappersAndStart(t *testing.T) {
	var sent, topics int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == matrixAPIPath+"/account/whoami":
			_, _ = w.Write([]byte(`{"user_id":"@bot:example"}`))
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			sent++
			_, _ = w.Write([]byte(`{}`))
		case strings.Contains(r.URL.Path, "/state/m.room.topic"):
			topics++
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == matrixAPIPath+"/sync":
			_, _ = w.Write([]byte(`{"next_batch":"s0"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	bot := NewBot(Config{
		HomeserverURL: server.URL + "/",
		AccessToken:   " token ",
		RoomID:        " !room:example ",
		AllowedUsers:  []string{"@alice:example"},
	}, slog.Default())
	if bot.Name() != "matrix" {
		t.Fatalf("Name() = %q", bot.Name())
	}
	SetAgentIdentities(map[string]AgentIdentity{"worker": {Emoji: "w", Color: 1}})
	SetAgentAliases(map[string]string{"w": "worker"})
	bot.SetAgentNames([]string{"worker"})
	bot.RegisterCommand("noop", func(context.Context, string) (string, error) { return "ok", nil })
	ctx, cancel := context.WithCancel(context.Background())
	if err := bot.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start error = %v", err)
	}
	if bot.userID != "@bot:example" {
		cancel()
		t.Fatalf("userID = %q", bot.userID)
	}
	if err := bot.SendMessage("hello"); err != nil {
		cancel()
		t.Fatalf("SendMessage error = %v", err)
	}
	if err := bot.SetTopic("topic"); err != nil {
		cancel()
		t.Fatalf("SetTopic error = %v", err)
	}
	cancel()
	bot.Listen(ctx, func(chat.Message) {})
	if sent == 0 || topics != 1 {
		t.Fatalf("sent = %d topics = %d", sent, topics)
	}
}

func TestStartValidationAndWhoamiErrors(t *testing.T) {
	if err := NewBot(Config{}, slog.Default()).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "homeserver_url") {
		t.Fatalf("Start empty error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	if _, err := testBackend(server.URL).Whoami(context.Background()); err == nil || !strings.Contains(err.Error(), "user_id") {
		t.Fatalf("Whoami empty error = %v", err)
	}
}

func TestHelpersAndErrorPaths(t *testing.T) {
	if (matrixAPIError{StatusCode: 403, ErrCode: "M_FORBIDDEN", ErrorMessage: "no"}).Error() == "" {
		t.Fatal("matrixAPIError Error is empty")
	}
	if (matrixAPIError{StatusCode: 500}).Error() != "matrix API 500" {
		t.Fatal("status-only matrixAPIError mismatch")
	}
	if err := testBackend("http://127.0.0.1:1").doJSON(context.Background(), http.MethodPost, "/x", func() {}, nil); err == nil {
		t.Fatal("doJSON with unmarshalable payload returned nil")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()
	var out whoamiResponse
	if err := testBackend(server.URL).doJSON(context.Background(), http.MethodGet, "/account/whoami", nil, &out); err == nil {
		t.Fatal("doJSON invalid JSON returned nil")
	}
	if got := defaultDuration(0, 3*time.Second); got != 3*time.Second {
		t.Fatalf("defaultDuration fallback = %v", got)
	}
	if got := defaultDuration(time.Second, 3*time.Second); got != time.Second {
		t.Fatalf("defaultDuration value = %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitContext(ctx, time.Hour) {
		t.Fatal("waitContext returned true for canceled context")
	}
	if !waitContext(context.Background(), 0) {
		t.Fatal("waitContext zero returned false")
	}
	if sleepWithContext(ctx, nil, time.Hour) {
		t.Fatal("sleepWithContext returned true for canceled context")
	}
	seen := map[string]struct{}{}
	order := []string{}
	for i := 0; i < dedupeEventIDCacheSize+1; i++ {
		rememberEvent(seen, &order, fmt.Sprintf("event-%d", i))
	}
	if _, ok := seen["event-0"]; ok || len(order) != dedupeEventIDCacheSize {
		t.Fatalf("dedupe cache not trimmed: len=%d old=%v", len(order), ok)
	}
	if _, _, _, ok := parseMarkdownLink("[bad]"); ok {
		t.Fatal("invalid markdown link parsed")
	}
	if takeRunes("abc", 0) != "" {
		t.Fatal("takeRunes zero not empty")
	}
}

func TestSendAndTopicErrorPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
	}))
	defer server.Close()
	backend := testBackend(server.URL)
	if err := backend.Send("hello"); err == nil || !strings.Contains(err.Error(), "M_UNKNOWN") {
		t.Fatalf("Send error = %v", err)
	}
	if err := backend.SetTopic("topic"); err == nil || !strings.Contains(err.Error(), "M_UNKNOWN") {
		t.Fatalf("SetTopic error = %v", err)
	}
}

func TestListenBacksOffOnServerErrorThenResetsAfterSuccess(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		switch requests {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"boom"}`))
		default:
			_, _ = w.Write([]byte(`{"next_batch":"s0","rooms":{"join":{}}}`))
		}
	}))
	defer server.Close()

	backend := testBackend(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		backend.Listen(ctx, func(chat.Message) {})
	}()
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		seen := requests
		mu.Unlock()
		if seen >= 2 {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("requests = %d, want retry after server error", seen)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDoJSONRateLimitCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":1000}`))
	}))
	defer server.Close()
	backend := testBackend(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	backend.sleep = func(time.Duration) { cancel() }
	if err := backend.doJSON(ctx, http.MethodGet, "/sync", nil, nil); err == nil {
		t.Fatal("doJSON returned nil for canceled retry sleep")
	}

}

func TestRetryAfterDelayIsCapped(t *testing.T) {
	backend := testBackend("http://127.0.0.1:1")
	if got := backend.retryAfterDelay(int64(time.Hour / time.Millisecond)); got != backend.reconnectMax {
		t.Fatalf("retryAfterDelay large = %v want %v", got, backend.reconnectMax)
	}
	if got := backend.retryAfterDelay(1); got != time.Millisecond {
		t.Fatalf("retryAfterDelay small = %v", got)
	}
}
func TestMarkdownEdgeCases(t *testing.T) {
	if got := markdownToMatrixHTML("`open"); got != "<code>open</code>" {
		t.Fatalf("open inline code = %q", got)
	}
	if got := markdownToMatrixHTML("```open"); got != "<pre><code>open</code></pre>" {
		t.Fatalf("open fence = %q", got)
	}
	if _, _, _, ok := parseMarkdownLink("[](x)"); ok {
		t.Fatal("empty markdown link text parsed")
	}
	if _, _, _, ok := parseMarkdownLink("[x]()"); ok {
		t.Fatal("empty markdown link href parsed")
	}
}
