package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const otterTestKeyEnv = "HIVE_TEST_OTTER_KEY"

// otterFixture is an in-memory Otter.ai Public API.
type otterFixture struct {
	mu            sync.Mutex
	rateLimitOnce bool
	rateLimited   int
	queries       []string
	// total is how many conversations the account has (default 4; the
	// fourth is older than since_days).
	total int
}

func otterTestStart(i int) time.Time {
	return time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)
}

func otterTestConversation(i int, rel bool, total int) map[string]any {
	start := otterTestStart(i)
	if total == 4 && i == 3 {
		start = time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	}
	m := map[string]any{
		"id": "conv" + strconv.Itoa(i), "title": "Customer call " + strconv.Itoa(i), "url": "https://otter.ai/u/conv" + strconv.Itoa(i),
		"owner":           map[string]any{"id": "u1", "name": "Jane Doe", "email": "jane@example.com"},
		"created_at":      start.Format(time.RFC3339),
		"calendar_guests": []any{map[string]any{"name": "Alex Taylor", "email": "alex@example.com"}, map[string]any{"name": "", "email": "sam@example.com"}},
	}
	if i == 0 {
		m["abstract_summary"] = "The team met with ACME."
	}
	if i == 1 {
		m["title"] = ""
		m["calendar_guests"] = nil
	}
	if rel {
		r := map[string]any{"insights": nil, "custom_prompt": nil, "transcript": nil, "outline": nil, "action_items": nil}
		if i == 0 {
			r["outline"] = []any{map[string]any{"section": "Pricing", "text": []any{"Per-seat and flat-rate options."}}, map[string]any{"section": "", "text": "Loose note"}}
			r["action_items"] = []any{
				map[string]any{"text": "Send pricing", "assignee": map[string]any{"name": "Jane Doe"}, "status": map[string]any{"completed": true}},
				map[string]any{"text": "Confirm users", "assignee": nil, "status": nil},
				map[string]any{"text": ""},
			}
			r["transcript"] = map[string]any{"format": "txt", "content": "Jane Doe  00:00\nThanks everyone for joining.\n\nAlex Taylor  00:07\nRight now we take notes\nby hand.\n\nJane Doe  1:02:03\nWrapping up."}
		}
		if i == 1 {
			r["transcript"] = map[string]any{"format": "txt", "content": "plain words without heads"}
		}
		m["relationships"] = r
	}
	return m
}

func (f *otterFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer otter-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Path+"?"+r.URL.RawQuery)
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"Too Many Requests"}`, http.StatusTooManyRequests)
			return false
		}
		return true
	}
	total := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.total == 0 {
			return 4
		}
		return f.total
	}
	mux.HandleFunc("/v1/workspace", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		reply(w, map[string]any{"meta": map[string]any{}, "data": map[string]any{"id": 42, "name": "Acme", "owner": map[string]any{"name": "Jane Doe"}}})
	})
	mux.HandleFunc("/v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		q := r.URL.Query()
		n := total()
		start := 0
		if c := q.Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		var data []any
		for i := start; i < n && i < start+limit; i++ {
			data = append(data, otterTestConversation(i, false, n))
		}
		meta := map[string]any{"retrieved_at": "2025-11-14T00:00:00Z", "has_more": start+limit < n}
		if start+limit < n {
			meta["next_cursor"] = strconv.Itoa(start + limit)
		}
		reply(w, map[string]any{"meta": meta, "data": data})
	})
	mux.HandleFunc("/v1/conversations/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		id, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/conversations/"), "/")
		i, err := strconv.Atoi(strings.TrimPrefix(id, "conv"))
		if err != nil || !strings.HasPrefix(id, "conv") || i == 2 && sub == "" && total() == 5 {
			http.Error(w, `{"error":"Conversation not found"}`, http.StatusNotFound)
			return
		}
		switch sub {
		case "audio":
			switch i {
			case 0:
				reply(w, map[string]any{"data": map[string]any{"url": "https://download.otter.example/conv0.mp3?sig=1"}})
			case 1:
				reply(w, map[string]any{"data": map[string]any{"url": "http://insecure.example/a.mp3"}})
			default:
				http.Error(w, `{"error":"Conversation not found"}`, http.StatusNotFound)
			}
		case "":
			if r.URL.Query().Get("include") != otterInclude {
				http.Error(w, "bad include", http.StatusBadRequest)
				return
			}
			if i == 9 {
				reply(w, map[string]any{"meta": map[string]any{}})
				return
			}
			reply(w, map[string]any{"meta": map[string]any{}, "data": otterTestConversation(i, true, total())})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func newOtterTest(t *testing.T, f *otterFixture, scope map[string]string) *otterConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	old := otterAPIBase
	otterAPIBase = srv.URL + "/v1"
	t.Cleanup(func() { otterAPIBase = old })
	t.Setenv(otterTestKeyEnv, "otter-key")
	if scope == nil {
		scope = map[string]string{}
	}
	cfg := ConnectorConfig{Name: "otter", Type: TypeOtter, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: otterTestKeyEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	oc := c.(*otterConnector)
	oc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return oc
}

func TestOtterValidate(t *testing.T) {
	reg := DefaultRegistry()
	ok := Auth{Env: "OTTER_API_KEY"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"include_shared": "true", "channel": "channel_1"}, ""},
		{"no key", Auth{}, nil, "auth.env"},
		{"client env", Auth{Env: "K", ClientIDEnv: "C"}, nil, "not used"},
		{"bad shared", ok, map[string]string{"include_shared": "sometimes"}, "include_shared"},
		{"bad channel", ok, map[string]string{"channel": "a/b"}, "not an Otter channel id"},
		{"bad since", ok, map[string]string{"since_days": "-3"}, "since_days"},
	}
	for _, tc := range cases {
		_, err := reg.New(ConnectorConfig{Name: "o", Type: TypeOtter, Layer: "project", Scope: tc.scope, Auth: tc.auth}, Deps{})
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestOtterSyncConversations(t *testing.T) {
	f := &otterFixture{}
	c := newOtterTest(t, f, map[string]string{"tags": "calls", "include_shared": "true"})
	ctx := context.Background()
	if c.Type() != TypeOtter {
		t.Fatalf("Type() = %q", c.Type())
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected || st.Account != "Acme" {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 3 || pages[0].ID != "conv0" || pages[2].ID != "conv2" {
		t.Fatalf("conversations within since_days, newest first: %+v", pages)
	}
	if want := otterTestStart(0).Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q", next, want)
	}
	f.mu.Lock()
	q := f.queries[1]
	f.mu.Unlock()
	if !strings.Contains(q, "include_shared=true") || !strings.Contains(q, "limit=20") {
		t.Fatalf("listing query %q", q)
	}
	a := pages[0]
	for _, want := range []string{"# Customer call 0", "## Summary (Otter)", "The team met with ACME.", "### Outline", "- **Pricing**", "  - Per-seat and flat-rate options.", "  - Loose note", "### Action items", "- Send pricing (Jane Doe) ✓", "- Confirm users", "## Transcript", "[00:00] Jane Doe: Thanks everyone for joining.", "[00:07] Alex Taylor: Right now we take notes by hand.", "[1:02:03] Jane Doe: Wrapping up.", "- **Host:** Jane Doe", "- **Participants:** Alex Taylor, sam@example.com", "- **Duration:** 1h2m3s", "https://otter.ai/u/conv0"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("conversation lacks %q:\n%s", want, a.Markdown)
		}
	}
	if a.Attrs["otter_recording_id"] != "conv0" || a.Attrs["tags"] != "calls" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if b := pages[1]; !strings.HasPrefix(b.Title, "Otter meeting ") || !strings.Contains(b.Markdown, "plain words without heads") || strings.Contains(b.Markdown, "Participants") {
		t.Fatalf("untitled conversation with a headless transcript: %+v", b)
	}
	if p := pages[2]; p.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(p.Markdown, MeetingPendingText) {
		t.Fatalf("a conversation without text is pending: %+v", p)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || !recs[0].HasMedia || recs[0].Tags[0] != "calls" {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "conv0")
	if err != nil || !strings.Contains(content.Transcript, "Alex Taylor: Right now") || !strings.Contains(content.Summary, "ACME") || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown conversation: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "conv9"); err == nil || !strings.Contains(err.Error(), "conversation conv9 not found") {
		t.Fatalf("reply without data: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "../x"); err == nil || !strings.Contains(err.Error(), "invalid conversation id") {
		t.Fatalf("bad id: %v", err)
	}

	link, err := c.MediaLink(ctx, "conv0")
	if err != nil || link.URL != "https://download.otter.example/conv0.mp3?sig=1" || !link.ExpiresAt.Equal(c.now().Add(meetingMediaLinkTTLFallback)) {
		t.Fatalf("audio link = %+v, %v", link, err)
	}
	if _, err := c.MediaLink(ctx, "conv1"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "no https audio link") {
		t.Fatalf("insecure link: %v", err)
	}
	if _, err := c.MediaLink(ctx, "conv2"); !errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("no audio: %v", err)
	}
	if _, err := c.MediaLink(ctx, "../x"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("bad id: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestOtterPaginationErrorsAndRateLimit(t *testing.T) {
	f := &otterFixture{total: 45}
	c := newOtterTest(t, f, map[string]string{"channel": "channel_1", "include_summary": "false", "include_participants": "false"})
	recs, err := c.ListMeetings(context.Background())
	f.mu.Lock()
	queries := append([]string(nil), f.queries...)
	f.mu.Unlock()
	if err != nil || len(recs) != 45 || len(queries) != 3 || !strings.Contains(queries[2], "cursor=40") || !strings.Contains(queries[0], "channel_id=channel_1") || strings.Contains(queries[0], "include_shared") || c.Truncated() {
		t.Fatalf("pagination: %d, %v, %v", len(recs), err, queries)
	}
	if content, err := c.ReadMeeting(context.Background(), "conv0"); err != nil || content.Summary != "" {
		t.Fatalf("include_summary=false: %+v, %v", content, err)
	}
	capped := &otterFixture{total: 45}
	c2 := newOtterTest(t, capped, map[string]string{"max_recordings": "5"})
	if recs, err := c2.ListMeetings(context.Background()); err != nil || len(recs) != 5 || !c2.Truncated() {
		t.Fatalf("capped: %d, %v, %v", len(recs), err, c2.Truncated())
	}

	// A conversation that cannot be read is skipped and reported.
	partial := &otterFixture{total: 5}
	c3 := newOtterTest(t, partial, nil)
	var pages []Page
	_, err = c3.Sync(context.Background(), "2025-11-13T10:00:00Z", func(p Page) error { pages = append(pages, p); return nil })
	var pe *PartialError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "conv2") || len(pages) != 4 {
		t.Fatalf("partial sync: %d pages, %v", len(pages), err)
	}

	bad := newOtterTest(t, &otterFixture{}, nil)
	t.Setenv(otterTestKeyEnv, "wrong")
	if _, err := bad.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Otter card") || !strings.Contains(err.Error(), "without the Public API") {
		t.Fatalf("401: %v", err)
	}
	if st := bad.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(otterTestKeyEnv, "")
	if _, err := bad.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing key: %v", err)
	}

	rl := &otterFixture{rateLimitOnce: true}
	c4 := newOtterTest(t, rl, nil)
	c4.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c4.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if recs, err := c4.ListMeetings(context.Background()); err != nil || len(recs) != 3 || waited != time.Second {
		t.Fatalf("429 with Retry-After: %d, %v, waited %v", len(recs), err, waited)
	}
	rl2 := &otterFixture{rateLimitOnce: true}
	c5 := newOtterTest(t, rl2, nil)
	if _, err := c5.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("429 without retries left: %v", err)
	}
}

func TestOtterHelpers(t *testing.T) {
	for in, want := range map[string]time.Duration{"00:07": 7 * time.Second, "1:02:03": time.Hour + 2*time.Minute + 3*time.Second, "x:01": 0, "-1:00": 0} {
		if got := otterOffset(in); got != want {
			t.Errorf("otterOffset(%q) = %v, want %v", in, got, want)
		}
	}
	if got, last := otterTranscript(""); got != "" || last != 0 {
		t.Errorf("empty transcript = %q %v", got, last)
	}
	if got, _ := otterTranscript("A  00:01\nhi\n\nloose text\n\nB  00:05\n"); got != "[00:01] A: hi\nloose text" {
		t.Errorf("mixed transcript = %q", got)
	}
	if got := otterUser(nil); got != "" {
		t.Errorf("user(nil) = %q", got)
	}
	if got := otterSummary(map[string]any{}, nil); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	c := &otterConnector{cfg: ConnectorConfig{}}
	if got, _ := c.otterContent(map[string]any{}, nil); !got.Pending {
		t.Errorf("empty conversation = %+v", got)
	}

	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			_, _ = w.Write([]byte("<html>"))
		case 2:
			http.Error(w, "no", http.StatusForbidden)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(otterTestKeyEnv, "otter-key")
	old := otterAPIBase
	otterAPIBase = srv.URL
	t.Cleanup(func() { otterAPIBase = old })
	hc := &otterConnector{cfg: ConnectorConfig{Auth: Auth{Env: otterTestKeyEnv}}, http: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1}), now: time.Now}
	if _, err := hc.get(context.Background(), "/workspace", nil); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("non-JSON: %v", err)
	}
	if _, err := hc.get(context.Background(), "/workspace", nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("403: %v", err)
	}
	if _, err := hc.MediaLink(context.Background(), "conv0"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Errorf("500 on audio: %v", err)
	}
}
