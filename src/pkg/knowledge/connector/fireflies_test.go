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

const firefliesTestKeyEnv = "HIVE_TEST_FIREFLIES_KEY"

// firefliesFixture is an in-memory Fireflies GraphQL API.
type firefliesFixture struct {
	mu            sync.Mutex
	authError     bool
	paidMedia     bool
	rateLimitOnce bool
	rateLimited   int
	rateLimitBody bool
	listVars      []map[string]any
	queries       []string
	// total is how many transcripts the account has (default 3).
	total int
}

func firefliesTestTranscript(i int, withText bool) map[string]any {
	id := []string{"ffA", "ffB", "ffC"}[i%3]
	if i >= 3 {
		id = "ffX" + strconv.Itoa(i)
	}
	t := map[string]any{
		"id": id, "title": "Weekly sync " + id, "date": float64(1763046000000 - int64(i)*3600000), "duration": 30.5,
		"organizer_email": "andy@example.com", "transcript_url": "https://app.fireflies.ai/view/" + id,
		"participants": []any{"andy@example.com,pat@example.com"}, "meeting_attendees": []any{},
	}
	if i == 0 {
		t["meeting_attendees"] = []any{map[string]any{"displayName": "Andy", "email": "andy@example.com"}, map[string]any{"name": "Pat Lee", "email": "pat@example.com"}}
	}
	if withText {
		switch id {
		case "ffA":
			t["sentences"] = []any{
				map[string]any{"speaker_name": "Andy", "text": "Hello team", "start_time": 5.2},
				map[string]any{"speaker_name": "Pat", "text": "", "raw_text": "ship it", "start_time": "65.2"},
			}
			t["summary"] = map[string]any{"overview": "The team agreed to ship.", "action_items": "Andy opens the PR", "keywords": []any{"ship", "release"}, "topics_discussed": []any{"Release"}}
		case "ffB":
			t["sentences"] = []any{}
			t["summary"] = map[string]any{"short_summary": "Short only."}
		default:
			t["sentences"] = nil
			t["summary"] = nil
		}
	}
	return t
}

func (f *firefliesFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer ff-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Invalid API key","code":"auth_failed"}]}`))
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.queries = append(f.queries, req.Query)
		authErr, paid, total := f.authError, f.paidMedia, f.total
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			body := f.rateLimitBody
			f.mu.Unlock()
			if body {
				_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Too many requests","extensions":{"code":"too_many_requests"}}]}`))
				return
			}
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Too many requests","code":"too_many_requests"}]}`))
			return
		}
		f.mu.Unlock()
		if total == 0 {
			total = 3
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		if authErr {
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Invalid API key","extensions":{"code":"auth_failed","status":401}}]}`))
			return
		}
		withText := strings.Contains(req.Query, "sentences")
		switch {
		case strings.Contains(req.Query, "HiveUser"):
			reply(map[string]any{"user": map[string]any{"user_id": "u1", "email": "andy@example.com", "name": "Andy"}})
		case strings.Contains(req.Query, "HiveTranscripts"):
			f.mu.Lock()
			f.listVars = append(f.listVars, req.Variables)
			f.mu.Unlock()
			skip := int(req.Variables["skip"].(float64))
			limit := int(req.Variables["limit"].(float64))
			var list []any
			for i := skip; i < total && i < skip+limit; i++ {
				list = append(list, firefliesTestTranscript(i, withText))
			}
			reply(map[string]any{"transcripts": list})
		case strings.Contains(req.Query, "audio_url"):
			id, _ := req.Variables["id"].(string)
			if paid {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":[{"message":"Requires a paid plan","code":"paid_required","extensions":{"metadata":{"tier":"pro_or_higher"}}}]}`))
				return
			}
			switch id {
			case "ffA":
				reply(map[string]any{"transcript": map[string]any{"audio_url": "https://cdn.fireflies.example/a.mp3?sig=1", "video_url": nil}})
			case "ffB":
				reply(map[string]any{"transcript": map[string]any{"audio_url": "", "video_url": "https://cdn.fireflies.example/b.mp4"}})
			case "ffC":
				reply(map[string]any{"transcript": map[string]any{"audio_url": "", "video_url": ""}})
			default:
				reply(map[string]any{"transcript": map[string]any{"audio_url": "http://insecure.example/x.mp3"}})
			}
		case strings.Contains(req.Query, "HiveTranscript"):
			id, _ := req.Variables["id"].(string)
			for i := 0; i < 3; i++ {
				if tr := firefliesTestTranscript(i, withText); tr["id"] == id {
					reply(map[string]any{"transcript": tr})
					return
				}
			}
			_, _ = w.Write([]byte(`{"data":{"transcript":null},"errors":[{"message":"Transcript not found","code":"object_not_found"}]}`))
		default:
			reply(map[string]any{})
		}
	})
}

func newFirefliesTest(t *testing.T, f *firefliesFixture, scope map[string]string) *firefliesConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	old := firefliesAPIURL
	firefliesAPIURL = srv.URL + "/graphql"
	t.Cleanup(func() { firefliesAPIURL = old })
	t.Setenv(firefliesTestKeyEnv, "ff-key")
	if scope == nil {
		scope = map[string]string{}
	}
	cfg := ConnectorConfig{Name: "ff", Type: TypeFireflies, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: firefliesTestKeyEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	fc := c.(*firefliesConnector)
	fc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return fc
}

func TestFirefliesValidate(t *testing.T) {
	reg := DefaultRegistry()
	ok := Auth{Env: "FIREFLIES_API_KEY"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"mine": "true", "since_days": "7"}, ""},
		{"no key", Auth{}, nil, "auth.env"},
		{"client env", Auth{Env: "K", ClientIDEnv: "A"}, nil, "not used"},
		{"bad mine", ok, map[string]string{"mine": "maybe"}, "scope.mine"},
		{"bad max", ok, map[string]string{"max_recordings": "-1"}, "max_recordings"},
		{"bad summary", ok, map[string]string{"include_summary": "x"}, "include_summary"},
	}
	for _, tc := range cases {
		_, err := reg.New(ConnectorConfig{Name: "f", Type: TypeFireflies, Layer: "project", Scope: tc.scope, Auth: tc.auth}, Deps{})
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestFirefliesSyncTranscriptsAndSummary(t *testing.T) {
	f := &firefliesFixture{}
	c := newFirefliesTest(t, f, map[string]string{"tags": "meetings", "mine": "true"})
	ctx := context.Background()
	if c.Type() != TypeFireflies {
		t.Fatalf("Type() = %q", c.Type())
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 3 || pages[0].ID != "ffA" || pages[2].ID != "ffC" {
		t.Fatalf("one fact per transcript, newest first: %+v", pages)
	}
	if want := time.UnixMilli(1763046000000).UTC().Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q", next, want)
	}
	f.mu.Lock()
	vars := f.listVars[0]
	f.mu.Unlock()
	if vars["fromDate"] != "2025-10-15T06:00:00.000Z" || vars["mine"] != true || vars["limit"] != float64(firefliesPageSize) {
		t.Fatalf("first sync reaches since_days back with mine: %v", vars)
	}
	a := pages[0]
	for _, want := range []string{"# Weekly sync ffA", "## Summary (Fireflies)", "The team agreed to ship.", "### Action items", "Andy opens the PR", "**Topics:** Release", "**Keywords:** ship, release", "## Transcript", "[00:05] Andy: Hello team", "[01:05] Pat: ship it", "- **Host:** andy@example.com", "- **Participants:** Andy, Pat Lee", "- **Duration:** 30m30s", "https://app.fireflies.ai/view/ffA"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("meeting lacks %q:\n%s", want, a.Markdown)
		}
	}
	if a.Attrs["fireflies_recording_id"] != "ffA" || a.Attrs["tags"] != "meetings" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if b := pages[1]; !strings.Contains(b.Markdown, "Short only.") || strings.Contains(b.Markdown, "## Transcript") || !strings.Contains(b.Markdown, "pat@example.com") {
		t.Fatalf("summary-only meeting with comma-joined participants: %s", b.Markdown)
	}
	if p := pages[2]; p.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(p.Markdown, MeetingPendingText) {
		t.Fatalf("a meeting without text is pending: %+v", p)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || !recs[0].HasMedia || recs[0].Tags[0] != "meetings" {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	f.mu.Lock()
	listQuery := f.queries[len(f.queries)-1]
	f.mu.Unlock()
	if strings.Contains(listQuery, "sentences") {
		t.Fatal("the Inbox listing must not fetch sentences")
	}
	content, err := c.ReadMeeting(ctx, "ffA")
	if err != nil || !strings.Contains(content.Transcript, "Andy: Hello team") || !strings.Contains(content.Summary, "agreed to ship") || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if content, err := c.ReadMeeting(ctx, "ffC"); err != nil || !content.Pending {
		t.Fatalf("pending ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "missing1"); err == nil || !strings.Contains(err.Error(), "object_not_found") {
		t.Fatalf("unknown transcript: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "../etc"); err == nil || !strings.Contains(err.Error(), "invalid transcript id") {
		t.Fatalf("bad id: %v", err)
	}

	link, err := c.MediaLink(ctx, "ffA")
	if err != nil || link.URL != "https://cdn.fireflies.example/a.mp3?sig=1" || !link.ExpiresAt.Equal(c.now().Add(firefliesMediaTTL)) {
		t.Fatalf("audio link = %+v, %v", link, err)
	}
	if link, err := c.MediaLink(ctx, "ffB"); err != nil || !strings.HasSuffix(link.URL, "b.mp4") {
		t.Fatalf("video fallback = %+v, %v", link, err)
	}
	if _, err := c.MediaLink(ctx, "ffC"); !errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("no media: %v", err)
	}
	if _, err := c.MediaLink(ctx, "ffZ"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "not an https link") {
		t.Fatalf("insecure link: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestFirefliesPagination(t *testing.T) {
	f := &firefliesFixture{total: firefliesPageSize + 7}
	c := newFirefliesTest(t, f, map[string]string{"include_summary": "false", "include_participants": "false"})
	var pages []Page
	if _, err := c.Sync(context.Background(), "2025-11-10T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	calls := len(f.listVars)
	second := f.listVars[1]
	first := f.listVars[0]
	q := f.queries[0]
	f.mu.Unlock()
	if len(pages) != firefliesPageSize+7 || calls != 2 || second["skip"] != float64(firefliesPageSize) || c.Truncated() {
		t.Fatalf("pages=%d calls=%d second=%v truncated=%v", len(pages), calls, second, c.Truncated())
	}
	if first["fromDate"] != "2025-11-08T00:00:00.000Z" || first["mine"] != nil {
		t.Fatalf("incremental sync starts at the cursor minus the overlap, without mine: %v", first)
	}
	if strings.Contains(q, "summary") || strings.Contains(pages[0].Markdown, "Participants") {
		t.Fatalf("include_summary/include_participants false: %s", q)
	}

	capped := &firefliesFixture{total: firefliesPageSize + 7}
	c2 := newFirefliesTest(t, capped, map[string]string{"max_recordings": "10"})
	recs, err := c2.ListMeetings(context.Background())
	if err != nil || len(recs) != 10 || !c2.Truncated() {
		t.Fatalf("capped listing: %d, %v, truncated=%v", len(recs), err, c2.Truncated())
	}
}

func TestFirefliesErrorsAndRateLimit(t *testing.T) {
	f := &firefliesFixture{authError: true}
	c := newFirefliesTest(t, f, nil)
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Fireflies card") {
		t.Fatalf("dead key in a 200 reply: %v", err)
	}
	if st := c.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(firefliesTestKeyEnv, "wrong")
	if _, err := c.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("HTTP 401 auth_failed: %v", err)
	}
	t.Setenv(firefliesTestKeyEnv, "")
	if _, err := c.ReadMeeting(context.Background(), "ffA"); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing key: %v", err)
	}

	paid := &firefliesFixture{paidMedia: true}
	c2 := newFirefliesTest(t, paid, nil)
	if _, err := c2.MediaLink(context.Background(), "ffA"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "Pro plan") {
		t.Fatalf("paid_required media: %v", err)
	}

	rl := &firefliesFixture{rateLimitOnce: true}
	c3 := newFirefliesTest(t, rl, nil)
	c3.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c3.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	recs, err := c3.ListMeetings(context.Background())
	if err != nil || len(recs) != 3 || waited != 4*time.Second {
		t.Fatalf("429 with Retry-After: %+v, %v, waited %v", recs, err, waited)
	}

	rl2 := &firefliesFixture{rateLimitOnce: true}
	c4 := newFirefliesTest(t, rl2, nil)
	if _, err := c4.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "too_many_requests") {
		t.Fatalf("429 without retries left: %v", err)
	}
	rl3 := &firefliesFixture{rateLimitOnce: true, rateLimitBody: true}
	c5 := newFirefliesTest(t, rl3, nil)
	if _, err := c5.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("too_many_requests in the body: %v", err)
	}
}

func TestFirefliesHelpers(t *testing.T) {
	cases := []struct {
		body   string
		status int
		code   string
		msg    string
		isNil  bool
	}{
		{`{"errors":[{"message":"m","code":"PAID_REQUIRED"}]}`, 403, "paid_required", "m", false},
		{`{"errors":[{"message":"m2","extensions":{"code":"forbidden"}}]}`, 200, "forbidden", "m2", false},
		{`{"data":{}}`, 200, "", "", true},
		{`{"errors":[{"message":"cut off","code":"auth_fai`, 401, "", "cut off", false},
		{`{"errors":[{"code":"auth_failed","message":"Invalid`, 401, "auth_failed", "", false},
		{`<html>`, 502, "", "", true},
	}
	for _, tc := range cases {
		e := parseFirefliesError(tc.body, tc.status)
		if tc.isNil {
			if e != nil {
				t.Errorf("%s: want nil, got %+v", tc.body, e)
			}
			continue
		}
		if e == nil || e.Code != tc.code || e.Message != tc.msg {
			t.Errorf("%s: got %+v", tc.body, e)
		}
	}
	if err := firefliesClassify(&firefliesError{Status: 401, Message: "x"}); !errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("401 = %v", err)
	}
	if err := firefliesClassify(&firefliesError{Code: "forbidden", Status: 403}); errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "request failed (forbidden)") {
		t.Errorf("forbidden = %v", err)
	}
	if got := firefliesStrings([]any{" a ", 3, "", "b"}); strings.Join(got, "|") != "a|b" {
		t.Errorf("strings = %v", got)
	}
	if got := firefliesStrings(nil); got != nil {
		t.Errorf("nil strings = %v", got)
	}
	for _, tc := range []struct {
		in   any
		want time.Duration
	}{{1.5, 1500 * time.Millisecond}, {"2", 2 * time.Second}, {"x", 0}, {-1.0, 0}, {nil, 0}} {
		if got := firefliesSeconds(tc.in); got != tc.want {
			t.Errorf("seconds(%v) = %v", tc.in, got)
		}
	}
	if got := firefliesSummary(map[string]any{}); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	list := []map[string]any{{"date": 1.0}, {"dateString": "2026-01-01T00:00:00Z"}, {"date": 2.0}}
	recsSortRaw(list)
	if list[0]["dateString"] == nil {
		t.Errorf("sort = %v", list)
	}

	// Non-JSON 200 replies and replies without data.
	mux := http.NewServeMux()
	n := 0
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			_, _ = w.Write([]byte("<html>"))
			return
		}
		if n == 2 {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(firefliesTestKeyEnv, "ff-key")
	c := &firefliesConnector{cfg: ConnectorConfig{Auth: Auth{Env: firefliesTestKeyEnv}}, http: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1}), now: time.Now}
	old := firefliesAPIURL
	firefliesAPIURL = srv.URL
	t.Cleanup(func() { firefliesAPIURL = old })
	if _, err := c.query(context.Background(), "{}", nil); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("non-JSON: %v", err)
	}
	if _, err := c.query(context.Background(), "{}", nil); err == nil || errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("502: %v", err)
	}
	if _, err := c.query(context.Background(), "{}", nil); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("no data: %v", err)
	}
}
