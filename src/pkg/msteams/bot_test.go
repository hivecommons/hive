package msteams

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
	statusByPath     map[string]int
	bodyByPath       map[string]string
	retryAfterByPath map[string]string
	blockToken       chan struct{}
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	fg := &fakeGraph{
		t:                t,
		statusByPath:     make(map[string]int),
		bodyByPath:       make(map[string]string),
		retryAfterByPath: make(map[string]string),
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
		w.WriteHeader(status)
		_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
		return
	}
	_, _ = w.Write([]byte(fg.response(r.URL.RequestURI())))
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

func (fg *fakeGraph) calls() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.graphCalls...)
}

func (fg *fakeGraph) sentBodies() []string {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string(nil), fg.bodies...)
}

func testBackend(t *testing.T, fg *fakeGraph) *Backend {
	t.Helper()
	b := NewBot(Config{TenantID: "tenant", ClientID: "app", ClientSecret: "secret", TeamID: "team", ChannelID: "chan", WebhookURL: fg.server.URL + "/webhook"}, slog.New(slog.NewTextHandler(io.Discard, nil))).Backend
	b.graphBase = fg.server.URL
	b.loginBase = fg.server.URL
	b.client = fg.server.Client()
	b.now = func() time.Time { return time.Unix(1000, 0) }
	b.sleep = func(time.Duration) {}
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

func TestSendPostsTeamsHTMLToWebhook(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	if err := b.Send("**hi** `code` [link](https://example.com/?a=1&b=2) <tag>\n```\nx < y\n```"); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	if err := b.Send("again"); err != nil {
		t.Fatalf("second Send error = %v", err)
	}
	if fg.tokenCalls != 0 {
		t.Fatalf("tokenCalls = %d", fg.tokenCalls)
	}
	var payload struct {
		Text string `json:"text"`
	}
	bodies := fg.sentBodies()
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("posted body unmarshal: %v", err)
	}
	wantPieces := []string{"<strong>hi</strong>", "<code>code</code>", `<a href="https://example.com/?a=1&amp;b=2">link</a>`, "&lt;tag&gt;", "<pre>x &lt; y<br></pre>"}
	for _, want := range wantPieces {
		if !strings.Contains(payload.Text, want) {
			t.Fatalf("Teams HTML %q missing %q", payload.Text, want)
		}
	}
}

func TestSendSplitsFenceAware(t *testing.T) {
	fg := newFakeGraph(t)
	b := testBackend(t, fg)
	long := "```\n" + strings.Repeat("x", teamsMessageLimit+100) + "\n```"
	if err := b.Send(long); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	bodies := fg.sentBodies()
	posts := bodies
	if len(posts) < 2 {
		t.Fatalf("posts = %d, want split", len(posts))
	}
	for i, raw := range posts {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("post %d unmarshal: %v", i, err)
		}
		if strings.Count(payload.Text, "<pre>") != strings.Count(payload.Text, "</pre>") {
			t.Fatalf("post %d has unbalanced pre tags: %q", i, payload.Text)
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
	b.sleep = func(d time.Duration) { slept = d }
	fg.set("/webhook", http.StatusTooManyRequests, "")
	fg.mu.Lock()
	fg.retryAfterByPath["/webhook"] = "3"
	fg.mu.Unlock()
	var rl rateLimitError
	if err := b.Send("hello"); !errorsAs(err, &rl) || rl.after != 3*time.Second || slept != 3*time.Second {
		t.Fatalf("webhook rate err=%#v slept=%s", err, slept)
	}
	b.webhookURL = "http://[::1"
	if err := b.Send("hello"); err == nil {
		t.Fatalf("bad webhook URL error was nil")
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
	fg.retryAfterByPath["/teams/team/channels/chan/messages"] = "7"
	fg.mu.Unlock()
	err := b.callGraphJSON(context.Background(), http.MethodPost, b.channelMessagesPath(), map[string]string{"x": "y"}, nil)
	var rl rateLimitError
	if !errorsAs(err, &rl) || rl.after != 7*time.Second {
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
	parts := splitTeamsMessage("")
	if len(parts) != 1 || parts[0] != "" {
		t.Fatalf("empty split = %#v", parts)
	}
	if markdownToTeamsHTML("[bad](") != "[bad](" {
		t.Fatalf("bad link conversion changed")
	}
	if markdownToTeamsHTML("**unterminated") != "**unterminated" {
		t.Fatalf("unterminated bold changed")
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

func BenchmarkMarkdownToTeamsHTML(b *testing.B) {
	input := fmt.Sprintf("**hello** [link](https://example.com) `%s`", strings.Repeat("x", 10))
	for i := 0; i < b.N; i++ {
		_ = markdownToTeamsHTML(input)
	}
}
