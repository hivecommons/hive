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

const limitlessTestKeyEnv = "HIVE_TEST_LIMITLESS_KEY"

// limitlessFixture is an in-memory Limitless Developer API.
type limitlessFixture struct {
	mu            sync.Mutex
	rateLimitOnce bool
	rateLimited   int
	queries       []string
	// total is how many lifelogs the account has (default 3).
	total int
}

func limitlessTestLifelog(i int, withText bool) map[string]any {
	start := time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)
	l := map[string]any{
		"id": "ll" + strconv.Itoa(i), "title": "Standup " + strconv.Itoa(i),
		"startTime": start.Format(time.RFC3339), "endTime": start.Add(15 * time.Minute).Format(time.RFC3339), "isStarred": i == 0,
	}
	if i == 1 {
		l["title"] = ""
	}
	if withText {
		l["markdown"] = "# Standup\n\n> Andy: Hi"
		switch i {
		case 0:
			l["contents"] = []any{
				map[string]any{"type": "heading1", "content": "Standup 0", "children": []any{
					map[string]any{"type": "heading2", "content": "Release plan", "children": []any{
						map[string]any{"type": "blockquote", "content": "Hello team", "speakerName": "Andy", "speakerIdentifier": "user", "startOffsetMs": float64(5000)},
						map[string]any{"type": "blockquote", "content": "Ship it", "speakerName": "Pat", "startOffsetMs": float64(65000)},
						map[string]any{"type": "blockquote", "content": "And again", "speakerName": "andy", "startOffsetMs": float64(70000)},
					}},
				}},
			}
		case 1:
			l["contents"] = []any{}
		default:
			l["contents"] = []any{map[string]any{"type": "heading1", "content": ""}}
			l["markdown"] = ""
		}
	}
	return l
}

func (f *limitlessFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-API-Key") != "ll-key" {
			http.Error(w, `{"error":"Invalid API key"}`, http.StatusUnauthorized)
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Path+"?"+r.URL.RawQuery)
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"error":"API key is rate limited","retryAfter":"60"}`, http.StatusTooManyRequests)
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/lifelogs", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		total := f.total
		f.mu.Unlock()
		if total == 0 {
			total = 3
		}
		withText := q.Get("includeContents") == "true"
		start := 0
		if c := q.Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		var list []any
		for i := start; i < total && i < start+limit; i++ {
			list = append(list, limitlessTestLifelog(i, withText))
		}
		meta := map[string]any{"count": len(list)}
		if start+limit < total {
			meta["nextCursor"] = strconv.Itoa(start + limit)
		}
		reply(w, map[string]any{"data": map[string]any{"lifelogs": list}, "meta": map[string]any{"lifelogs": meta}})
	})
	mux.HandleFunc("/v1/lifelogs/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/lifelogs/")
		for i := 0; i < 3; i++ {
			if l := limitlessTestLifelog(i, true); l["id"] == id {
				reply(w, map[string]any{"data": map[string]any{"lifelog": l}})
				return
			}
		}
		if id == "empty" {
			reply(w, map[string]any{"data": map[string]any{}})
			return
		}
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	return mux
}

func newLimitlessTest(t *testing.T, f *limitlessFixture, scope map[string]string) *limitlessConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	old := limitlessAPIBase
	limitlessAPIBase = srv.URL
	t.Cleanup(func() { limitlessAPIBase = old })
	t.Setenv(limitlessTestKeyEnv, "ll-key")
	if scope == nil {
		scope = map[string]string{}
	}
	cfg := ConnectorConfig{Name: "pendant", Type: TypeLimitless, Enabled: true, Layer: "personal", Scope: scope, Auth: Auth{Env: limitlessTestKeyEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	lc := c.(*limitlessConnector)
	lc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return lc
}

func TestLimitlessValidate(t *testing.T) {
	reg := DefaultRegistry()
	ok := Auth{Env: "LIMITLESS_API_KEY"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"starred_only": "true"}, ""},
		{"no key", Auth{}, nil, "auth.env"},
		{"client env", Auth{Env: "K", TenantIDEnv: "T"}, nil, "not used"},
		{"bad starred", ok, map[string]string{"starred_only": "yes please"}, "scope.starred_only"},
		{"bad participants", ok, map[string]string{"include_participants": "x"}, "include_participants"},
	}
	for _, tc := range cases {
		_, err := reg.New(ConnectorConfig{Name: "l", Type: TypeLimitless, Layer: "personal", Scope: tc.scope, Auth: tc.auth}, Deps{})
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestLimitlessSyncLifelogs(t *testing.T) {
	f := &limitlessFixture{}
	c := newLimitlessTest(t, f, map[string]string{"tags": "pendant", "starred_only": "true"})
	ctx := context.Background()
	if c.Type() != TypeLimitless {
		t.Fatalf("Type() = %q", c.Type())
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 3 || pages[0].ID != "ll0" || pages[2].ID != "ll2" {
		t.Fatalf("one fact per lifelog, newest first: %+v", pages)
	}
	if want := time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q", next, want)
	}
	f.mu.Lock()
	q := f.queries[1]
	f.mu.Unlock()
	for _, want := range []string{"start=2025-10-15+06%3A00%3A00", "timezone=UTC", "direction=desc", "limit=10", "includeContents=true", "isStarred=true"} {
		if !strings.Contains(q, want) {
			t.Fatalf("sync query %q lacks %q", q, want)
		}
	}
	a := pages[0]
	for _, want := range []string{"# Standup 0", "## Summary (Limitless)", "### Topics", "- Release plan", "## Transcript", "[00:05] Andy: Hello team", "[01:05] Pat: Ship it", "[01:10] andy: And again", "- **Participants:** Andy, Pat", "- **Duration:** 15m0s"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("lifelog lacks %q:\n%s", want, a.Markdown)
		}
	}
	if strings.Contains(a.Markdown, "- Standup 0") {
		t.Fatal("the heading that repeats the title is not a topic")
	}
	if a.Attrs["limitless_recording_id"] != "ll0" || a.Attrs["tags"] != "pendant" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if b := pages[1]; b.Title != "Limitless meeting 2025-11-13 14:00" || !strings.Contains(b.Markdown, "> Andy: Hi") {
		t.Fatalf("untitled lifelog without contents falls back to the markdown: %+v", b)
	}
	if p := pages[2]; p.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(p.Markdown, MeetingPendingText) {
		t.Fatalf("a lifelog without text is pending: %+v", p)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || recs[0].HasMedia || recs[0].Tags[0] != "pendant" {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	f.mu.Lock()
	lq := f.queries[len(f.queries)-1]
	f.mu.Unlock()
	if !strings.Contains(lq, "includeContents=false") {
		t.Fatalf("the Inbox listing must not fetch contents: %q", lq)
	}
	content, err := c.ReadMeeting(ctx, "ll0")
	if err != nil || !strings.Contains(content.Transcript, "Andy: Hello team") || !strings.Contains(content.Summary, "Release plan") || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "gone"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown lifelog: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "empty"); err == nil || !strings.Contains(err.Error(), "lifelog empty not found") {
		t.Fatalf("reply without lifelog: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "../x"); err == nil || !strings.Contains(err.Error(), "invalid lifelog id") {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := c.MediaLink(ctx, "ll0"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "behind the API key") {
		t.Fatalf("media link: %v", err)
	}
	if _, err := c.MediaLink(ctx, "../x"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("media link for a bad id: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestLimitlessPaginationErrorsAndRateLimit(t *testing.T) {
	f := &limitlessFixture{total: 25}
	c := newLimitlessTest(t, f, map[string]string{"include_participants": "false"})
	var pages []Page
	if _, err := c.Sync(context.Background(), "2025-11-10T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	queries := append([]string(nil), f.queries...)
	f.mu.Unlock()
	if len(pages) != 25 || !strings.Contains(queries[2], "cursor=20") || c.Truncated() {
		t.Fatalf("pages=%d queries=%v truncated=%v", len(pages), queries, c.Truncated())
	}
	if !strings.Contains(queries[0], "start=2025-11-08+00%3A00%3A00") || strings.Contains(queries[0], "isStarred") || strings.Contains(pages[0].Markdown, "Participants") {
		t.Fatalf("incremental sync without starred/participants: %q", queries[0])
	}

	capped := &limitlessFixture{total: 25}
	c2 := newLimitlessTest(t, capped, map[string]string{"max_recordings": "5"})
	if recs, err := c2.ListMeetings(context.Background()); err != nil || len(recs) != 5 || !c2.Truncated() {
		t.Fatalf("capped: %d, %v, %v", len(recs), err, c2.Truncated())
	}

	bad := newLimitlessTest(t, &limitlessFixture{}, nil)
	t.Setenv(limitlessTestKeyEnv, "wrong")
	if _, err := bad.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Limitless card") {
		t.Fatalf("401: %v", err)
	}
	if st := bad.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(limitlessTestKeyEnv, "")
	if _, err := bad.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing key: %v", err)
	}

	rl := &limitlessFixture{rateLimitOnce: true}
	c3 := newLimitlessTest(t, rl, nil)
	c3.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c3.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if recs, err := c3.ListMeetings(context.Background()); err != nil || len(recs) != 3 || waited != 7*time.Second {
		t.Fatalf("429 with Retry-After: %d, %v, waited %v", len(recs), err, waited)
	}
	rl2 := &limitlessFixture{rateLimitOnce: true}
	c4 := newLimitlessTest(t, rl2, nil)
	if _, err := c4.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "retry after 60s") {
		t.Fatalf("429 without retries left: %v", err)
	}
}

func TestLimitlessHelpers(t *testing.T) {
	segs := limitlessSegments(limitlessTestLifelog(0, true))
	if len(segs) != 5 {
		t.Fatalf("flattened contents = %d", len(segs))
	}
	if got := limitlessSpeakers(segs); strings.Join(got, ",") != "Andy,Pat" {
		t.Errorf("speakers = %v", got)
	}
	if got := limitlessContent(map[string]any{"markdown": "only markdown"}, nil); got.Transcript != "only markdown" || got.Pending {
		t.Errorf("markdown fallback = %+v", got)
	}
	if got := limitlessContent(map[string]any{}, nil); !got.Pending {
		t.Errorf("empty = %+v", got)
	}
	c := &limitlessConnector{cfg: ConnectorConfig{}}
	if rec := c.parseLimitlessLifelog(map[string]any{"id": "x", "startTime": "2025-11-01T00:00:00Z", "endTime": "bad"}); rec.ID != "x" || rec.DurationMS != 0 || rec.StartAt.IsZero() {
		t.Errorf("minimal lifelog = %+v", rec)
	}

	// Non-JSON replies, replies without data, and other HTTP failures.
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			_, _ = w.Write([]byte("<html>"))
		case 2:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(limitlessTestKeyEnv, "ll-key")
	old := limitlessAPIBase
	limitlessAPIBase = srv.URL
	t.Cleanup(func() { limitlessAPIBase = old })
	hc := &limitlessConnector{cfg: ConnectorConfig{Auth: Auth{Env: limitlessTestKeyEnv}}, http: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1}), now: time.Now}
	if _, _, err := hc.get(context.Background(), "/v1/lifelogs", nil); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("non-JSON: %v", err)
	}
	if _, _, err := hc.get(context.Background(), "/v1/lifelogs", nil); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("no data: %v", err)
	}
	if _, _, err := hc.get(context.Background(), "/v1/lifelogs", nil); err == nil || errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("500: %v", err)
	}
}
