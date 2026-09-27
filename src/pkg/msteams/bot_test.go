package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/chat"
)

type fakeGraph struct {
	t                *testing.T
	server           *httptest.Server
	mu               sync.Mutex
	tokenCalls       int
	graphCalls       []string
	bodies           []string
	methods          []string
	preferHeaders    []string
	statusByPath     map[string]int
	bodyByPath       map[string]string
	retryAfterByPath map[string]string
	locationByPath   map[string]string
	blockToken       chan struct{}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	fg := &fakeGraph{
		t:                t,
		statusByPath:     make(map[string]int),
		bodyByPath:       make(map[string]string),
		retryAfterByPath: make(map[string]string),
		locationByPath:   make(map[string]string),
	}
	fg.server = httptest.NewServer(http.HandlerFunc(fg.serve))
	t.Cleanup(fg.server.Close)
	return fg
}

func (fg *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	if r.Context().Err() != nil {
		fg.t.Fatalf("request context unexpectedly canceled")
	}
	body, _ := io.ReadAll(r.Body)
	fg.mu.Lock()
	fg.methods = append(fg.methods, r.Method)
	fg.graphCalls = append(fg.graphCalls, r.URL.RequestURI())
	fg.bodies = append(fg.bodies, string(body))
	fg.preferHeaders = append(fg.preferHeaders, r.Header.Get("Prefer"))
	fg.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
		if fg.blockToken != nil {
			<-fg.blockToken
		}
		fg.mu.Lock()
		fg.tokenCalls++
		fg.mu.Unlock()
		if r.Method != http.MethodPost {
			fg.t.Fatalf("token method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/x-www-form-urlencoded") {
			fg.t.Fatalf("token content type = %q", got)
		}
		values, _ := url.ParseQuery(string(body))
		if values.Get("grant_type") != "client_credentials" || values.Get("scope") != "https://graph.microsoft.com/.default" {
			fg.t.Fatalf("token form = %v", values)
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		return
	}
	if r.URL.Path == "/webhook" {
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			fg.t.Fatalf("webhook content type = %q", got)
		}
		if status := fg.status(r.URL.RequestURI()); status != 0 {
			if retry := fg.retry(r.URL.RequestURI()); retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
			return
		}
		_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
		return
	}
	if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
		fg.t.Fatalf("Authorization = %q", auth)
	}
	if status := fg.status(r.URL.RequestURI()); status != 0 {
		if retry := fg.retry(r.URL.RequestURI()); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		fg.mu.Lock()
		location := fg.locationByPath[r.URL.RequestURI()]
		fg.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
		return
	}
	_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
}

func (fg *fakeGraph) calls(path string) int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	n := 0
	for _, call := range fg.graphCalls {
		if call == path {
			n++
		}
	}
	return n
}

func (fg *fakeGraph) status(path string) int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return fg.statusByPath[path]
}

func (fg *fakeGraph) response(path string) string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if body, ok := fg.bodyByPath[path]; ok {
		return body
	}
	return `{}`
}

func (fg *fakeGraph) retry(path string) string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return fg.retryAfterByPath[path]
}

func (fg *fakeGraph) set(path string, status int, body string) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.statusByPath[path] = status
	fg.bodyByPath[path] = body
}

func (fg *fakeGraph) sentBodies() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.bodies...)
}

func (fg *fakeGraph) prefers() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.preferHeaders...)
}

func testBackend(t *testing.T, fg *fakeGraph) *Backend {
	t.Helper()
	b := NewBot(Config{TenantID: "tenant", ClientID: "app", ClientSecret: "secret", TeamID: "team", ChannelID: "chan", WebhookURL: fg.server.URL + "/webhook"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
	b.graphBase = fg.server.URL
	b.loginBase = fg.server.URL
	b.client = fg.server.Client()
	b.now = func() time.Time { return time.Unix(1000, 0) }
	b.sleep = func(time.Duration) {}
	b.ctxSleep = func(context.Context, time.Duration) bool { return true }
	return b
}

func TestNewBotStartValidationAndName(t *testing.T) {
	bot := NewBot(Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if bot.Name() != "msteams" {
		t.Fatalf("Name() = %q", bot.Name())
	}
	if err := bot.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "tenant_id") {
		t.Fatalf("Start error = %v", err)
	}
}

func TestBotWrappersAndStartSuccess(t *testing.T) {
	fg := newFakeGraph(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot := NewBot(Config{
		TenantID:       "tenant",
		ClientID:       "app",
		ClientSecret:   "secret",
		TeamID:         "team",
		ChannelID:      "chan",
		WebhookURL:     fg.server.URL + "/webhook",
		AllowedUsers:   []string{"user-a"},
		DashboardURL:   "http://dashboard",
		DashboardToken: "dash",
	}, logger)
	bot.graphBase = fg.server.URL
	bot.loginBase = fg.server.URL
	bot.client = fg.server.Client()
	bot.SetAgentNames([]string{"worker"})
	bot.RegisterCommand("noop", func(context.Context, string) (string, error) { return "ok", nil })
	SetAgentIdentities(map[string]AgentIdentity{"worker": {Emoji: "🐝", Color: 1}})
	SetAgentAliases(map[string]string{"w": "worker"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bot.Start(ctx); err != nil {
		t.Fatalf("Start error = %v", err)
	}
	if err := bot.SendMessage("hello"); err != nil {
		t.Fatalf("SendMessage error = %v", err)
	}
	bot.Listen(ctx, func(chat.Message) { t.Fatalf("unexpected delivery") })
	if err := bot.SetTopic("topic"); err != nil {
		t.Fatalf("SetTopic wrapper error = %v", err)
	}
}

func decodeWorkflowCard(t *testing.T, raw string) (workflowMessage, string) {
	t.Helper()
	var msg workflowMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("webhook body unmarshal: %v", err)
	}
	if msg.Type != "message" || len(msg.Attachments) != 1 {
		t.Fatalf("webhook envelope = %s", raw)
	}
	att := msg.Attachments[0]
	if att.ContentType != adaptiveCardType || att.Content.Type != "AdaptiveCard" || att.Content.Version == "" || att.Content.Schema != adaptiveCardSchema {
		t.Fatalf("webhook attachment = %s", raw)
	}
	var texts []string
	for _, block := range att.Content.Body {
		if block.Type != "TextBlock" || !block.Wrap {
			t.Fatalf("card block = %#v", block)
		}
		texts = append(texts, block.Text)
	}
	return msg, strings.Join(texts, "\n")
}

func TestSendPostsWorkflowAdaptiveCardToWebhook(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	if err := b.Send("**hi** [link](https://example.com/?a=1&b=2) <tag>\n```\nx < y\n```\nafter"); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if err := b.Send("again"); err != nil {
		t.Fatalf("second Send error = %v", err)
	}
	if fg.tokenCalls != 0 {
		t.Fatalf("tokenCalls = %d", fg.tokenCalls)
	}
	bodies := fg.sentBodies()
	if strings.Contains(bodies[0], `"text":"<`) {
		t.Fatalf("legacy connector HTML payload posted: %s", bodies[0])
	}
	msg, _ := decodeWorkflowCard(t, bodies[0])
	blocks := msg.Attachments[0].Content.Body
	want := []adaptiveTextBlock{
		{Type: "TextBlock", Text: "**hi** [link](https://example.com/?a=1&b=2) <tag>", Wrap: true},
		{Type: "TextBlock", Text: "x < y", Wrap: true, FontType: "Monospace"},
		{Type: "TextBlock", Text: "after", Wrap: true},
	}
	if fmt.Sprint(blocks) != fmt.Sprint(want) {
		t.Fatalf("card body = %#v, want %#v", blocks, want)
	}
	if _, text := decodeWorkflowCard(t, bodies[1]); text != "again" {
		t.Fatalf("second card text = %q", text)
	}
}

func TestSendSplitsFenceAware(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	long := "```\n" + strings.Repeat("x", teamsMessageLimit+100) + "\n```"
	if err := b.Send(long); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	posts := fg.sentBodies()
	if len(posts) < 2 {
		t.Fatalf("posts = %d, want split", len(posts))
	}
	for i, raw := range posts {
		msg, _ := decodeWorkflowCard(t, raw)
		for _, block := range msg.Attachments[0].Content.Body {
			if block.FontType != "Monospace" || strings.Contains(block.Text, "```") {
				t.Fatalf("post %d fenced block not rendered monospace: %#v", i, block)
			}
		}
	}
}

func TestSetTopicDegradesOnForbidden(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan", http.StatusForbidden, `{"error":{"message":"no permission"}}`)
	if err := b.SetTopic("topic"); err != nil {
		t.Fatalf("SetTopic forbidden error = %v", err)
	}
	bodies := fg.sentBodies()
	if !strings.Contains(bodies[len(bodies)-1], `"description":"topic"`) {
		t.Fatalf("topic payload = %q", bodies[len(bodies)-1])
	}
}

func TestSetTopicReturnsNonForbiddenErrors(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan", http.StatusBadRequest, `{"error":{"message":"bad topic"}}`)
	if err := b.SetTopic("topic"); err == nil || !strings.Contains(err.Error(), "bad topic") {
		t.Fatalf("SetTopic error = %v", err)
	}
}

func TestSendReturnsGraphError(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/webhook", http.StatusBadRequest, `plain bad`)
	if err := b.Send("hello"); err == nil || !strings.Contains(err.Error(), "plain bad") {
		t.Fatalf("Send error = %v", err)
	}
}

func TestWebhookRateLimitAndBadURL(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	var slept time.Duration
	b.ctxSleep = func(_ context.Context, d time.Duration) bool {
		slept = d
		return true
	}
	fg.set("/webhook", http.StatusTooManyRequests, "")
	fg.mu.Lock()
	fg.retryAfterByPath["/webhook"] = "90"
	fg.mu.Unlock()
	if err := b.Send("hello"); err == nil || !strings.Contains(err.Error(), "1m0s") || slept != 60*time.Second {
		t.Fatalf("webhook rate err=%#v slept=%s", err, slept)
	}
	b.webhookURL = "http://[::1"
	if err := b.Send("hello"); err == nil {
		t.Fatalf("bad webhook URL error was nil")
	}
}

func TestWebhookRateLimitRetriesOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	b := NewBot(Config{WebhookURL: srv.URL}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
	b.client = srv.Client()
	var slept time.Duration
	b.ctxSleep = func(_ context.Context, d time.Duration) bool {
		slept = d
		return true
	}
	if err := b.postWebhook(context.Background(), map[string]string{"text": "hello"}); err != nil {
		t.Fatalf("postWebhook retry error = %v", err)
	}
	if calls != 2 || slept != time.Second {
		t.Fatalf("calls=%d slept=%s, want one ctx-aware retry", calls, slept)
	}
}

func TestWebhookErrorsRedactSecretURL(t *testing.T) {
	secretURL := "https://example.invalid/webhook/secret-token?sig=abc"
	b := NewBot(Config{WebhookURL: secretURL}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
	b.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: secretURL, Err: errors.New("dial failed for " + secretURL)}
	})}
	err := b.Send("hello")
	if err == nil {
		t.Fatalf("Send error was nil")
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "sig=abc") {
		t.Fatalf("webhook secret leaked in error: %q", err.Error())
	}
	b.webhookURL = "http://[::1/secret-token"
	err = b.Send("hello")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("bad URL error leaked secret or was nil: %v", err)
	}
}

func TestWebhookMarshalAndCanceledRetry(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	if err := b.postWebhook(context.Background(), func() {}); err == nil {
		t.Fatalf("marshal error was nil")
	}
	fg.set("/webhook", http.StatusTooManyRequests, "")
	fg.mu.Lock()
	fg.retryAfterByPath["/webhook"] = "5"
	fg.mu.Unlock()
	b.ctxSleep = func(context.Context, time.Duration) bool { return false }
	var rl rateLimitError
	if err := b.postWebhook(context.Background(), map[string]string{"text": "hello"}); !errorsAs(err, &rl) || rl.after != 5*time.Second {
		t.Fatalf("canceled retry err=%#v", err)
	}
}

func TestPollOnceDeliversAfterProcessingAndDedupes(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	path := "/teams/team/channels/chan/messages/delta"
	b.deltaURL = path
	b.deltaReady = true
	fg.set(path, 0, `{"value":[{"id":"1","body":{"contentType":"html","content":"<div>!status &amp; more</div>"},"from":{"user":{"id":"user-a","displayName":"ignored"}}},{"id":"2","body":{"content":"bot"},"from":{"application":{"id":"app"}}},{"id":"3","body":{"content":"own"},"from":{"user":{"id":"app"}}}],"@odata.deltaLink":"/delta-token"}`)
	var got []chat.Message
	immediate, err := b.pollOnce(context.Background(), func(m chat.Message) {
		got = append(got, m)
		if b.deltaURL != path {
			t.Fatalf("delta advanced before processing: %q", b.deltaURL)
		}
	})
	if err != nil || immediate {
		t.Fatalf("pollOnce immediate=%v err=%v", immediate, err)
	}
	if b.deltaURL != "/delta-token" {
		t.Fatalf("deltaURL = %q", b.deltaURL)
	}
	if prefers := fg.prefers(); !strings.Contains(strings.Join(prefers, ","), "odata.maxpagesize=20") {
		t.Fatalf("Prefer headers = %#v", prefers)
	}
	if len(got) != 3 || got[0].Text != "!status & more" || got[0].AuthorID != "user-a" || got[0].FromBot || !got[1].FromBot || !got[2].FromBot {
		t.Fatalf("delivered = %#v", got)
	}
	fg.set("/delta-token", 0, `{"value":[{"id":"1","body":{"content":"dupe"},"from":{"user":{"id":"user-a"}}},{"id":"4","body":{"content":"new"},"from":{"user":{"id":"user-b"}}}],"@odata.deltaLink":"/delta-2"}`)
	got = nil
	_, err = b.pollOnce(context.Background(), func(m chat.Message) { got = append(got, m) })
	if err != nil {
		t.Fatalf("second poll error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "4" {
		t.Fatalf("deduped delivery = %#v", got)
	}
}

func TestPollOnceFollowsNextLinkImmediately(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan/messages/delta", 0, `{"@odata.nextLink":"`+fg.server.URL+`/page2"}`)
	immediate, err := b.pollOnce(context.Background(), func(chat.Message) {})
	if err != nil || !immediate {
		t.Fatalf("first poll immediate=%v err=%v", immediate, err)
	}
	if b.deltaURL != fg.server.URL+"/page2" {
		t.Fatalf("next link = %q", b.deltaURL)
	}
	fg.set("/page2", 0, `{"value":[{"id":"old-page-2","body":{"content":"old"},"from":{"user":{"id":"user-a"}}}],"@odata.deltaLink":"/delta-ready"}`)
	delivered := false
	immediate, err = b.pollOnce(context.Background(), func(chat.Message) { delivered = true })
	if err != nil || immediate {
		t.Fatalf("second poll immediate=%v err=%v", immediate, err)
	}
	if delivered || !b.deltaReady || b.deltaURL != "/delta-ready" {
		t.Fatalf("paged baseline delivered=%v ready=%v delta=%q", delivered, b.deltaReady, b.deltaURL)
	}
}

func TestPollOnceBaselinesInitialDeltaWithoutDelivery(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan/messages/delta", 0, `{"value":[{"id":"old","body":{"content":"old"},"from":{"user":{"id":"user-a"}}}],"@odata.deltaLink":"/delta-token"}`)
	delivered := false
	immediate, err := b.pollOnce(context.Background(), func(chat.Message) { delivered = true })
	if err != nil || immediate {
		t.Fatalf("pollOnce immediate=%v err=%v", immediate, err)
	}
	if delivered {
		t.Fatalf("initial baseline delivered historical message")
	}
	if !b.wasSeen("old") || b.deltaURL != "/delta-token" {
		t.Fatalf("baseline not recorded: seen=%v delta=%q", b.wasSeen("old"), b.deltaURL)
	}
}

func TestPollOnceOversizedDeltaResetsStream(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	b.deltaURL = "/stale-delta"
	b.deltaReady = true
	fg.set("/stale-delta", 0, strings.Repeat("x", deltaReadLimit+1))
	immediate, err := b.pollOnce(context.Background(), func(chat.Message) { t.Fatalf("unexpected delivery") })
	if err != nil || immediate {
		t.Fatalf("pollOnce immediate=%v err=%v", immediate, err)
	}
	if b.deltaURL != "" || b.deltaReady {
		t.Fatalf("delta not reset: url=%q ready=%v", b.deltaURL, b.deltaReady)
	}
}

func TestPollOnceExpiredDeltaResyncs(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		location string
		wantURL  string
	}{
		{name: "410 resyncRequired", status: http.StatusGone, body: `{"error":{"code":"resyncRequired","message":"expired"}}`},
		{name: "400 syncStateNotFound", status: http.StatusBadRequest, body: `{"error":{"code":"syncStateNotFound","message":"gone"}}`},
		{name: "410 with Location", status: http.StatusGone, body: "", location: "/delta-fresh", wantURL: "/delta-fresh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fg := newFakeGraph(t)
			b := testBackend(t, fg)
			b.deltaURL = "/delta-expired"
			b.deltaReady = true
			b.lastDeltaOK = time.Unix(900, 0)
			b.markSeen("old")
			b.trackThread("root", true)
			fg.set("/delta-expired", tc.status, tc.body)
			fg.locationByPath["/delta-expired"] = tc.location
			immediate, err := b.pollOnce(context.Background(), func(chat.Message) { t.Fatalf("unexpected delivery") })
			if err != nil || immediate {
				t.Fatalf("pollOnce immediate=%v err=%v", immediate, err)
			}
			if b.deltaURL != tc.wantURL || b.deltaReady || b.wasSeen("old") || len(b.threads) != 0 {
				t.Fatalf("delta not reset: url=%q ready=%v threads=%v", b.deltaURL, b.deltaReady, b.threads)
			}
			fresh := b.channelMessagesPath() + "/delta"
			if tc.wantURL != "" {
				fresh = tc.wantURL
			}
			fg.set(fresh, 0, `{"value":[{"id":"gap","body":{"content":"x"},"from":{"user":{"id":"user-a"}}}],"@odata.deltaLink":"/delta-new"}`)
			if _, err := b.pollOnce(context.Background(), func(chat.Message) { t.Fatalf("baseline delivered") }); err != nil {
				t.Fatalf("fresh baseline error = %v", err)
			}
			if !b.deltaReady || b.deltaURL != "/delta-new" || fg.calls("/delta-expired") != 1 || fg.calls(fresh) != 1 {
				t.Fatalf("fresh baseline not taken: ready=%v url=%q expired=%d fresh=%d", b.deltaReady, b.deltaURL, fg.calls("/delta-expired"), fg.calls(fresh))
			}
		})
	}
}

func TestPollOnceNonResyncDeltaErrorKeepsStream(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	b.deltaURL = "/delta-ok"
	b.deltaReady = true
	fg.set("/delta-ok", http.StatusBadRequest, `{"error":{"code":"badRequest","message":"nope"}}`)
	if _, err := b.pollOnce(context.Background(), func(chat.Message) {}); err == nil {
		t.Fatalf("pollOnce error = nil")
	}
	if b.deltaURL != "/delta-ok" || !b.deltaReady {
		t.Fatalf("delta reset on non-resync error: url=%q ready=%v", b.deltaURL, b.deltaReady)
	}
}

func repliesPath(root string) string {
	return "/teams/team/channels/chan/messages/" + root + "/replies?$top=50"
}

func TestPollOnceDeliversThreadRepliesToBotPosts(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	b.deltaURL = "/delta"
	b.deltaReady = true
	fg.set("/delta", 0, `{"value":[{"id":"prompt","body":{"content":"reply approve/reject"},"from":{"application":{"id":"flow"}}},{"id":"human","body":{"content":"hi"},"from":{"user":{"id":"user-a"}}}],"@odata.deltaLink":"/delta-2"}`)
	fg.set(repliesPath("prompt"), 0, `{"value":[{"id":"reply-1","body":{"contentType":"html","content":"<p>approve</p>"},"from":{"user":{"id":"user-a"}}}]}`)
	var got []chat.Message
	if _, err := b.pollOnce(context.Background(), func(m chat.Message) { got = append(got, m) }); err != nil {
		t.Fatalf("pollOnce error = %v", err)
	}
	if len(got) != 3 || got[2].ID != "reply-1" || got[2].Text != "approve" || got[2].AuthorID != "user-a" || got[2].FromBot {
		t.Fatalf("delivered = %#v", got)
	}
	if fg.calls(repliesPath("human")) != 0 {
		t.Fatalf("replies polled for non-bot root")
	}
	fg.set("/delta-2", 0, `{"@odata.deltaLink":"/delta-3"}`)
	fg.set(repliesPath("prompt"), 0, `{"value":[{"id":"reply-1","body":{"content":"approve"},"from":{"user":{"id":"user-a"}}}],"@odata.nextLink":"`+fg.server.URL+`/replies-page-2"}`)
	fg.set("/replies-page-2", 0, `{"value":[{"id":"reply-2","body":{"content":"!status"},"from":{"user":{"id":"user-b"}}}]}`)
	got = nil
	if _, err := b.pollOnce(context.Background(), func(m chat.Message) { got = append(got, m) }); err != nil {
		t.Fatalf("second poll error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "reply-2" || got[0].Text != "!status" {
		t.Fatalf("second delivery = %#v", got)
	}
}

func TestPollOnceBaselinesRepliesOfHistoricalBotPosts(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan/messages/delta", 0, `{"value":[{"id":"old-prompt","body":{"content":"gate"},"from":{"application":{"id":"flow"}}}],"@odata.deltaLink":"/delta"}`)
	fg.set(repliesPath("old-prompt"), 0, `{"value":[{"id":"old-reply","body":{"content":"approve"},"from":{"user":{"id":"user-a"}}}]}`)
	if _, err := b.pollOnce(context.Background(), func(m chat.Message) { t.Fatalf("baseline delivered %#v", m) }); err != nil {
		t.Fatalf("baseline error = %v", err)
	}
	if !b.wasSeen("old-reply") || !b.threadReady["old-prompt"] {
		t.Fatalf("historical replies not baselined")
	}
	fg.set("/delta", 0, `{"@odata.deltaLink":"/delta"}`)
	fg.set(repliesPath("old-prompt"), 0, `{"value":[{"id":"old-reply","body":{"content":"approve"},"from":{"user":{"id":"user-a"}}},{"id":"new-reply","body":{"content":"reject"},"from":{"user":{"id":"user-a"}}}]}`)
	var got []chat.Message
	if _, err := b.pollOnce(context.Background(), func(m chat.Message) { got = append(got, m) }); err != nil {
		t.Fatalf("poll error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "new-reply" {
		t.Fatalf("delivered = %#v", got)
	}
}

func TestReplyThreadsBoundedAndDroppedOnNotFound(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	b.deltaURL = "/delta"
	b.deltaReady = true
	var roots []string
	for i := 0; i < replyThreadLimit+3; i++ {
		roots = append(roots, fmt.Sprintf(`{"id":"p%d","body":{"content":"x"},"from":{"application":{"id":"flow"}}}`, i))
	}
	fg.set("/delta", 0, `{"value":[`+strings.Join(roots, ",")+`],"@odata.deltaLink":"/delta"}`)
	fg.set(repliesPath("p7"), http.StatusNotFound, `{"error":{"code":"NotFound","message":"deleted"}}`)
	if _, err := b.pollOnce(context.Background(), func(chat.Message) {}); err != nil {
		t.Fatalf("pollOnce error = %v", err)
	}
	if fg.calls(repliesPath("p0")) != 0 || fg.calls(repliesPath("p3")) != 1 {
		t.Fatalf("thread window wrong: threads=%v", b.threads)
	}
	if len(b.threads) != replyThreadLimit-1 || b.threadReady["p7"] {
		t.Fatalf("deleted thread not dropped: %v", b.threads)
	}
	fg.set(repliesPath("p6"), http.StatusTooManyRequests, "")
	fg.retryAfterByPath[repliesPath("p6")] = "3"
	var rl rateLimitError
	if _, err := b.pollOnce(context.Background(), func(chat.Message) {}); !errorsAs(err, &rl) || rl.after != 3*time.Second {
		t.Fatalf("replies rate limit err = %v", err)
	}
}

func TestSeenSetIsBounded(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	for i := 0; i < 5000; i++ {
		b.markSeen(fmt.Sprintf("m%d", i))
	}
	b.markSeen("m4999")
	if len(b.seen) != seenIDCacheSize || len(b.seenOrder) != seenIDCacheSize {
		t.Fatalf("seen=%d order=%d, want %d", len(b.seen), len(b.seenOrder), seenIDCacheSize)
	}
	if b.wasSeen("m0") || !b.wasSeen("m4999") {
		t.Fatalf("seen eviction order wrong")
	}
}

func TestListenDeliversAndStopsOnContext(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	b.deltaURL = "/teams/team/channels/chan/messages/delta"
	b.deltaReady = true
	fg.set("/teams/team/channels/chan/messages/delta", 0, `{"value":[{"id":"1","body":{"content":"hello"},"from":{"user":{"id":"user-a"}}}],"@odata.deltaLink":"/done"}`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan chat.Message, 1)
	b.Listen(ctx, func(m chat.Message) {
		done <- m
		cancel()
	})
	select {
	case got := <-done:
		if got.ID != "1" {
			t.Fatalf("delivered = %#v", got)
		}
	default:
		t.Fatalf("Listen returned without delivery")
	}
}

func TestListenBackoffStopsOnContext(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan/messages/delta", http.StatusInternalServerError, "boom")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	b.Listen(ctx, func(chat.Message) { t.Fatalf("unexpected delivery") })
}

func TestCallGraphReturnsRateLimitRetryAfter(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	fg.set("/teams/team/channels/chan/messages", http.StatusTooManyRequests, "")
	fg.mu.Lock()
	fg.retryAfterByPath["/teams/team/channels/chan/messages"] = "90"
	fg.mu.Unlock()
	err := b.callGraphJSON(context.Background(), http.MethodPost, b.channelMessagesPath(), map[string]string{"x": "y"}, nil)
	var rl rateLimitError
	if !errorsAs(err, &rl) || rl.after != 60*time.Second {
		t.Fatalf("rate error = %#v", err)
	}
}

func TestCallGraphMarshalAndDecodeErrors(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	if err := b.callGraphJSON(context.Background(), http.MethodPost, b.channelMessagesPath(), func() {}, nil); err == nil {
		t.Fatalf("marshal error was nil")
	}
	fg.set("/teams/team/channels/chan/messages/delta", 0, `{bad json`)
	if err := b.callGraphJSON(context.Background(), http.MethodGet, b.channelMessagesPath()+"/delta", nil, &deltaResponse{}); err == nil {
		t.Fatalf("decode error was nil")
	}
}

func TestDoGraphRejectsBadURL(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	if _, err := b.doGraph(context.Background(), http.MethodGet, "http://[::1", nil, false); err == nil {
		t.Fatalf("bad URL error was nil")
	}
}

func TestTokenErrorPaths(t *testing.T) {
	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("denied"))
		}))
		defer srv.Close()
		b := NewBot(Config{TenantID: "tenant", ClientID: "app", ClientSecret: "secret", TeamID: "team", ChannelID: "chan"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
		b.loginBase = srv.URL
		b.client = srv.Client()
		if _, err := b.getToken(context.Background()); err == nil || !strings.Contains(err.Error(), "denied") {
			t.Fatalf("token http error = %v", err)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{bad"))
		}))
		defer srv.Close()
		b := NewBot(Config{TenantID: "tenant", ClientID: "app", ClientSecret: "secret", TeamID: "team", ChannelID: "chan"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
		b.loginBase = srv.URL
		b.client = srv.Client()
		if _, err := b.getToken(context.Background()); err == nil {
			t.Fatalf("token invalid JSON error was nil")
		}
	})
	t.Run("missing token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"expires_in":3600}`))
		}))
		defer srv.Close()
		b := NewBot(Config{TenantID: "tenant", ClientID: "app", ClientSecret: "secret", TeamID: "team", ChannelID: "chan"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
		b.loginBase = srv.URL
		b.client = srv.Client()
		if _, err := b.getToken(context.Background()); err == nil || !strings.Contains(err.Error(), "access_token") {
			t.Fatalf("token missing error = %v", err)
		}
	})
}

func TestGetTokenRefreshesBeforeExpiryAndSingleFlights(t *testing.T) {
	fg := newFakeGraph(t)
	fg.blockToken = make(chan struct{})
	b := testBackend(t, fg)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := b.getToken(context.Background())
			if err == nil && tok == "tok" {
				successes.Add(1)
			}
		}()
	}
	close(fg.blockToken)
	wg.Wait()
	if successes.Load() != 8 {
		t.Fatalf("successes = %d", successes.Load())
	}
	fg.mu.Lock()
	calls := fg.tokenCalls
	fg.mu.Unlock()
	if calls != 1 {
		t.Fatalf("token calls = %d", calls)
	}
	b.now = func() time.Time { return b.tokenUntil.Add(-tokenRefreshSkew / 2) }
	if _, err := b.getToken(context.Background()); err != nil {
		t.Fatalf("refresh getToken error = %v", err)
	}
	fg.mu.Lock()
	calls = fg.tokenCalls
	fg.mu.Unlock()
	if calls != 2 {
		t.Fatalf("token calls after refresh window = %d", calls)
	}
}

func TestRequestContextCancellation(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.callGraphJSON(ctx, http.MethodGet, b.channelMessagesPath()+"/delta", nil, &deltaResponse{}); err == nil {
		t.Fatalf("callGraphJSON with canceled ctx succeeded")
	}
}

func TestHelpers(t *testing.T) {
	if retryAfter(" 2 ") != 2*time.Second || retryAfter("nope") != 0 {
		t.Fatalf("retryAfter unexpected")
	}
	if !sleepContext(context.Background(), 0) {
		t.Fatalf("zero sleep failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepContext(ctx, time.Hour) {
		t.Fatalf("canceled sleep succeeded")
	}
	if takeRunes("åßc", 2) != "åß" {
		t.Fatalf("takeRunes unicode failed")
	}
	if takeRunes("abc", 0) != "" {
		t.Fatalf("takeRunes zero failed")
	}
	if _, _, err := readLimited(errReader{}, 10); err == nil {
		t.Fatalf("readLimited error was nil")
	}
	parts := splitTeamsMessage("")
	if len(parts) != 1 || parts[0] != "" {
		t.Fatalf("empty split = %#v", parts)
	}
	if blocks := markdownToCardBody(""); len(blocks) != 1 || blocks[0].Text != " " {
		t.Fatalf("empty card body = %#v", blocks)
	}
	if blocks := markdownToCardBody("**unterminated"); len(blocks) != 1 || blocks[0].Text != "**unterminated" || blocks[0].FontType != "" {
		t.Fatalf("plain card body = %#v", blocks)
	}
	if isNotFound(nil) || !isNotFound(graphStatusError(http.StatusNotFound, nil)) {
		t.Fatalf("isNotFound unexpected")
	}
	if inboundText(graphMessageBody{ContentType: "text", Content: "<b>raw</b>"}) != "<b>raw</b>" {
		t.Fatalf("plain inbound text changed")
	}
	if htmlToText("one<br>two</p><span>three</span>") != "one\ntwo\nthree" {
		t.Fatalf("htmlToText unexpected")
	}
	if !updateFenceState(false, "```") || !updateFenceState(true, "``````") {
		t.Fatalf("fence state unexpected")
	}
	if isForbidden(nil) || isForbidden(fmt.Errorf("other")) {
		t.Fatalf("isForbidden false cases failed")
	}
	if graphStatusError(http.StatusBadGateway, nil).Error() != "msteams API 502: " {
		t.Fatalf("empty graphStatusError unexpected")
	}
}

func errorsAs(err error, target any) bool {
	switch t := target.(type) {
	case *rateLimitError:
		if v, ok := err.(rateLimitError); ok {
			*t = v
			return true
		}
	}
	return false
}

func BenchmarkMarkdownToCardBody(b *testing.B) {
	input := fmt.Sprintf("**hello** [link](https://example.com)\n```\n%s\n```", strings.Repeat("x", 10))
	for i := 0; i < b.N; i++ {
		_ = markdownToCardBody(input)
	}
}
