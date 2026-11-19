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

const fathomTestKeyEnv = "HIVE_TEST_FATHOM_KEY"

// fathomFixture is an in-memory Fathom External API.
type fathomFixture struct {
	mu            sync.Mutex
	srvURL        string
	rateLimitOnce bool
	rateLimited   int
	listQueries   []string
	// total is how many meetings the account has (default 3).
	total int
	// polls counts download status reads per download id.
	polls     map[string]int
	downloads int
}

func fathomTestMeeting(i int, withText bool) map[string]any {
	start := time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)
	m := map[string]any{
		"title": "Weekly sync " + strconv.Itoa(i), "meeting_title": "Calendar title", "recording_id": float64(1000 + i),
		"url": "https://fathom.video/calls/" + strconv.Itoa(1000+i), "share_url": "https://fathom.video/share/abc" + strconv.Itoa(i),
		"created_at":           start.Add(31 * time.Minute).Format(time.RFC3339),
		"recording_start_time": start.Format(time.RFC3339), "recording_end_time": start.Add(30 * time.Minute).Format(time.RFC3339),
		"recorded_by":       map[string]any{"name": "Andy", "email": "andy@example.com"},
		"calendar_invitees": []any{map[string]any{"name": "Andy", "email": "andy@example.com"}, map[string]any{"name": nil, "email": "pat@example.com"}},
	}
	if i == 1 {
		m["title"] = ""
		m["share_url"] = nil
	}
	if withText {
		switch i {
		case 0:
			m["transcript"] = []any{
				map[string]any{"speaker": map[string]any{"display_name": "Andy"}, "text": "Hello team", "timestamp": "00:00:05"},
				map[string]any{"speaker": map[string]any{"display_name": "Pat"}, "text": "Ship it", "timestamp": "01:02:03"},
			}
			m["default_summary"] = map[string]any{"template_name": "General", "markdown_formatted": "## Summary\nThe team agreed to ship."}
			m["action_items"] = []any{
				map[string]any{"description": "Open the PR", "assignee": map[string]any{"name": "Andy"}, "completed": false},
				map[string]any{"description": "Tag the release", "completed": true},
				map[string]any{"description": ""},
			}
		case 1:
			m["transcript"] = []any{map[string]any{"speaker": map[string]any{"display_name": "Pat"}, "text": "Only words", "timestamp": "00:10"}}
			m["default_summary"] = nil
		default:
			m["transcript"] = nil
			m["default_summary"] = nil
		}
	}
	return m
}

func (f *fathomFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-Api-Key") != "fathom-key" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return false
		}
		return true
	}
	mux.HandleFunc("/external/v1/meetings", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		f.listQueries = append(f.listQueries, r.URL.RawQuery)
		total := f.total
		f.mu.Unlock()
		if total == 0 {
			total = 3
		}
		withText := q.Get("include_transcript") == "true"
		start := 0
		if c := q.Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		const page = 10
		var items []any
		for i := start; i < total && i < start+page; i++ {
			items = append(items, fathomTestMeeting(i, withText))
		}
		next := any(nil)
		if start+page < total {
			next = strconv.Itoa(start + page)
		}
		reply(w, map[string]any{"items": items, "next_cursor": next, "limit": page})
	})
	mux.HandleFunc("/external/v1/recordings/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/external/v1/recordings/"), "/")
		id := parts[0]
		switch {
		case len(parts) == 2 && parts[1] == "transcript":
			if id == "1000" {
				reply(w, map[string]any{"transcript": fathomTestMeeting(0, true)["transcript"]})
				return
			}
			if id == "1403" {
				http.Error(w, "limited", http.StatusForbidden)
				return
			}
			reply(w, map[string]any{"transcript": []any{}})
		case len(parts) == 2 && parts[1] == "summary":
			if id == "1000" {
				reply(w, map[string]any{"summary": map[string]any{"markdown_formatted": "The team agreed to ship."}})
				return
			}
			http.Error(w, "boom", http.StatusInternalServerError)
		case len(parts) == 2 && parts[1] == "download" && r.Method == http.MethodPost:
			f.mu.Lock()
			f.downloads++
			n := f.downloads
			f.mu.Unlock()
			switch id {
			case "1000": // audio-only: completes at once
				reply(w, map[string]any{"download_id": "d0", "recording_id": 1000, "status": "completed", "audio": map[string]any{"url": "https://media.fathom.example/a.m4a?sig=1", "content_type": "audio/mp4", "expires_at": "2025-11-15T06:00:00Z"}})
			case "1001": // video: processing first
				reply(w, map[string]any{"download_id": "d1-" + strconv.Itoa(n), "recording_id": 1001, "status": "processing"})
			case "1002":
				http.Error(w, `{"error":"no media"}`, http.StatusUnprocessableEntity)
			case "1003":
				reply(w, map[string]any{"download_id": "d3", "status": "failed", "failure_reason": "generation_failed"})
			case "1004":
				reply(w, map[string]any{"download_id": "d4", "status": "completed", "video": map[string]any{"url": "http://insecure.example/v.mp4"}})
			case "1005":
				reply(w, map[string]any{"download_id": "d5", "status": "completed"})
			default:
				reply(w, map[string]any{"download_id": "d6", "status": "completed", "video": map[string]any{"url": "https://media.fathom.example/v.mp4"}})
			}
		case len(parts) == 3 && parts[1] == "downloads":
			f.mu.Lock()
			if f.polls == nil {
				f.polls = map[string]int{}
			}
			f.polls[parts[2]]++
			n := f.polls[parts[2]]
			f.mu.Unlock()
			if n == 1 {
				reply(w, map[string]any{"download_id": parts[2], "status": "processing"})
				return
			}
			if n == 2 {
				reply(w, map[string]any{"download_id": parts[2], "status": "completed", "video": map[string]any{"url": "https://media.fathom.example/v1.mp4"}})
				return
			}
			reply(w, map[string]any{"download_id": parts[2], "status": "expired"})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func newFathomTest(t *testing.T, f *fathomFixture, scope map[string]string) *fathomConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	f.srvURL = srv.URL
	old := fathomAPIBase
	fathomAPIBase = srv.URL + "/external/v1"
	t.Cleanup(func() { fathomAPIBase = old })
	t.Setenv(fathomTestKeyEnv, "fathom-key")
	if scope == nil {
		scope = map[string]string{}
	}
	cfg := ConnectorConfig{Name: "fathom", Type: TypeFathom, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: fathomTestKeyEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	fc := c.(*fathomConnector)
	fc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return fc
}

func TestFathomValidate(t *testing.T) {
	reg := DefaultRegistry()
	ok := Auth{Env: "FATHOM_API_KEY"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"recorded_by": "andy@example.com, pat@example.com", "teams": "Sales"}, ""},
		{"no key", Auth{}, nil, "auth.env"},
		{"client env", Auth{Env: "K", ClientSecretEnv: "S"}, nil, "not used"},
		{"bad recorder", ok, map[string]string{"recorded_by": "andy"}, "not an email"},
		{"bad since", ok, map[string]string{"since_days": "x"}, "since_days"},
	}
	for _, tc := range cases {
		_, err := reg.New(ConnectorConfig{Name: "f", Type: TypeFathom, Layer: "project", Scope: tc.scope, Auth: tc.auth}, Deps{})
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestFathomSyncMeetings(t *testing.T) {
	f := &fathomFixture{}
	c := newFathomTest(t, f, map[string]string{"tags": "meetings", "recorded_by": "andy@example.com", "teams": "Eng"})
	ctx := context.Background()
	if c.Type() != TypeFathom {
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
	if len(pages) != 3 || pages[0].ID != "1000" || pages[2].ID != "1002" {
		t.Fatalf("one fact per recording, newest first: %+v", pages)
	}
	if want := time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q", next, want)
	}
	f.mu.Lock()
	q := f.listQueries[1]
	f.mu.Unlock()
	for _, want := range []string{"created_after=2025-10-15T06%3A00%3A00Z", "include_transcript=true", "include_summary=true", "include_action_items=true", "recorded_by%5B%5D=andy%40example.com", "teams%5B%5D=Eng"} {
		if !strings.Contains(q, want) {
			t.Fatalf("sync query %q lacks %q", q, want)
		}
	}
	a := pages[0]
	for _, want := range []string{"# Weekly sync 0", "## Summary (Fathom)", "The team agreed to ship.", "### Action items", "- Open the PR (Andy)", "- Tag the release ✓", "## Transcript", "[00:05] Andy: Hello team", "[1:02:03] Pat: Ship it", "- **Host:** Andy", "- **Participants:** Andy, pat@example.com", "- **Duration:** 30m0s", "https://fathom.video/share/abc0"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("meeting lacks %q:\n%s", want, a.Markdown)
		}
	}
	if a.Attrs["fathom_recording_id"] != "1000" || a.Attrs["tags"] != "meetings" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if b := pages[1]; b.Title != "Calendar title" || b.URL != "https://fathom.video/calls/1001" || !strings.Contains(b.Markdown, "[00:10] Pat: Only words") || strings.Contains(b.Markdown, "## Summary") {
		t.Fatalf("calendar title, page url, transcript-only: %+v", b)
	}
	if p := pages[2]; p.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(p.Markdown, MeetingPendingText) {
		t.Fatalf("a recording without text is pending: %+v", p)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || !recs[0].HasMedia || recs[0].Tags[0] != "meetings" {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	f.mu.Lock()
	lq := f.listQueries[len(f.listQueries)-1]
	f.mu.Unlock()
	if strings.Contains(lq, "include_transcript") {
		t.Fatal("the Inbox listing must not fetch transcripts")
	}
	content, err := c.ReadMeeting(ctx, "1000")
	if err != nil || !strings.Contains(content.Transcript, "Andy: Hello team") || content.Summary != "The team agreed to ship." || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if content, err := c.ReadMeeting(ctx, "1002"); err != nil || !content.Pending {
		t.Fatalf("summary failure keeps the item pending, not failed: %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "1403"); err == nil || !strings.Contains(err.Error(), "limited access") {
		t.Fatalf("403: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "../x"); err == nil || !strings.Contains(err.Error(), "invalid recording id") {
		t.Fatalf("bad id: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestFathomMediaLink(t *testing.T) {
	f := &fathomFixture{}
	c := newFathomTest(t, f, nil)
	ctx := context.Background()
	link, err := c.MediaLink(ctx, "1000")
	if err != nil || link.URL != "https://media.fathom.example/a.m4a?sig=1" || !link.ExpiresAt.Equal(time.Date(2025, 11, 15, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("audio-only download = %+v, %v", link, err)
	}
	// Video: processing, then the same download completes, then it expires
	// and a new download starts.
	if _, err := c.MediaLink(ctx, "1001"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "still preparing") {
		t.Fatalf("processing: %v", err)
	}
	if _, err := c.MediaLink(ctx, "1001"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "still preparing") {
		t.Fatalf("still processing on the first poll: %v", err)
	}
	link, err = c.MediaLink(ctx, "1001")
	if err != nil || link.URL != "https://media.fathom.example/v1.mp4" || !link.ExpiresAt.Equal(c.now().Add(meetingMediaLinkTTLFallback)) {
		t.Fatalf("completed poll = %+v, %v", link, err)
	}
	f.mu.Lock()
	created := f.downloads
	f.mu.Unlock()
	if created != 2 {
		t.Fatalf("a pending download is polled, not re-requested: %d downloads", created)
	}
	for id, want := range map[string]string{"1002": "no downloadable media", "1003": "generation_failed", "1004": "not an https link", "1005": "has no file"} {
		if _, err := c.MediaLink(ctx, id); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), want) {
			t.Errorf("MediaLink(%s) = %v, want %q", id, err, want)
		}
	}
	if link, err := c.MediaLink(ctx, "1006"); err != nil || !strings.HasSuffix(link.URL, "v.mp4") {
		t.Fatalf("video download = %+v, %v", link, err)
	}
	if _, err := c.MediaLink(ctx, "abc"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("bad id: %v", err)
	}

	// An expired cached download starts a new one.
	c.downloads["1001"] = "gone"
	f.mu.Lock()
	f.polls = map[string]int{"gone": 2}
	f.mu.Unlock()
	if _, err := c.MediaLink(ctx, "1001"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "still preparing") {
		t.Fatalf("expired download restarts: %v", err)
	}
}

func TestFathomPaginationErrorsAndRateLimit(t *testing.T) {
	f := &fathomFixture{total: 25}
	c := newFathomTest(t, f, map[string]string{"include_summary": "false", "include_participants": "false"})
	var pages []Page
	if _, err := c.Sync(context.Background(), "2025-11-10T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	queries := append([]string(nil), f.listQueries...)
	f.mu.Unlock()
	if len(pages) != 25 || len(queries) != 4 || !strings.Contains(queries[2], "cursor=20") || c.Truncated() {
		t.Fatalf("pages=%d queries=%v truncated=%v", len(pages), queries, c.Truncated())
	}
	if !strings.Contains(queries[0], "created_after=2025-11-08T00%3A00%3A00Z") || strings.Contains(queries[0], "include_summary") || strings.Contains(pages[0].Markdown, "Participants") {
		t.Fatalf("incremental sync without summary/participants: %q", queries[0])
	}
	if content, err := c.ReadMeeting(context.Background(), "1000"); err != nil || content.Summary != "" {
		t.Fatalf("include_summary=false skips /summary: %+v, %v", content, err)
	}

	capped := &fathomFixture{total: 25}
	c2 := newFathomTest(t, capped, map[string]string{"max_recordings": "5"})
	if recs, err := c2.ListMeetings(context.Background()); err != nil || len(recs) != 5 || !c2.Truncated() {
		t.Fatalf("capped: %d, %v, %v", len(recs), err, c2.Truncated())
	}

	bad := newFathomTest(t, &fathomFixture{}, nil)
	t.Setenv(fathomTestKeyEnv, "wrong")
	if _, err := bad.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Fathom card") {
		t.Fatalf("401: %v", err)
	}
	if st := bad.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(fathomTestKeyEnv, "")
	if _, err := bad.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing key: %v", err)
	}

	rl := &fathomFixture{rateLimitOnce: true}
	c3 := newFathomTest(t, rl, nil)
	c3.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c3.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if recs, err := c3.ListMeetings(context.Background()); err != nil || len(recs) != 3 || waited != 5*time.Second {
		t.Fatalf("429 with Retry-After: %d, %v, waited %v", len(recs), err, waited)
	}
	rl2 := &fathomFixture{rateLimitOnce: true}
	c4 := newFathomTest(t, rl2, nil)
	if _, err := c4.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("429 without retries left: %v", err)
	}
}

func TestFathomHelpers(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:00:05": 5 * time.Second, "01:02:03": time.Hour + 2*time.Minute + 3*time.Second, "02:30": 150 * time.Second,
		"": 0, "5": 0, "a:b": 0, "1:2:3:4": 0, "-1:00": 0,
	} {
		if got := fathomTimestamp(in); got != want {
			t.Errorf("fathomTimestamp(%q) = %v, want %v", in, got, want)
		}
	}
	if got := fathomSummary(nil, nil); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	if got := fathomSummary(map[string]any{"markdown_formatted": "S"}, []map[string]any{{"description": "x", "assignee": map[string]any{"email": "a@b.c"}}}); got != "S\n\n### Action items\n\n- x (a@b.c)" {
		t.Errorf("summary = %q", got)
	}
	if got := fathomTranscript([]map[string]any{{"text": "no speaker", "timestamp": "00:00:01"}, {"text": ""}}); got != "[00:01] no speaker" {
		t.Errorf("transcript = %q", got)
	}
	c := &fathomConnector{cfg: ConnectorConfig{}}
	if rec := c.parseFathomMeeting(map[string]any{"recording_id": "7", "created_at": "2025-11-01T00:00:00Z"}); rec.ID != "7" || rec.DurationMS != 0 || rec.StartAt.IsZero() {
		t.Errorf("minimal meeting = %+v", rec)
	}

	// A non-JSON reply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	t.Cleanup(srv.Close)
	t.Setenv(fathomTestKeyEnv, "fathom-key")
	old := fathomAPIBase
	fathomAPIBase = srv.URL
	t.Cleanup(func() { fathomAPIBase = old })
	hc := &fathomConnector{cfg: ConnectorConfig{Auth: Auth{Env: fathomTestKeyEnv}}, http: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1}), now: time.Now}
	if _, err := hc.call(context.Background(), http.MethodGet, "/meetings", nil); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("non-JSON: %v", err)
	}
}
