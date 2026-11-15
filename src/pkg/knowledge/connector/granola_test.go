package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const granolaTestKeyEnv = "HIVE_TEST_GRANOLA_KEY"

// granolaFixture is an in-memory Granola public API.
type granolaFixture struct {
	mu            sync.Mutex
	rateLimitOnce bool
	rateLimited   int
	queries       []string
	// total is how many notes the account has (default 4).
	total int
}

func granolaTestID(i int) string { return fmt.Sprintf("not_%014d", i) }

func granolaTestStart(i int) time.Time {
	return time.Date(2025, 11, 13, 15, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour)
}

func granolaTestSummary(i int) map[string]any {
	return map[string]any{
		"id": granolaTestID(i), "object": "note", "title": "Planning " + strconv.Itoa(i),
		"owner":      map[string]any{"name": "Andy", "email": "andy@example.com"},
		"created_at": granolaTestStart(i).Format(time.RFC3339), "updated_at": granolaTestStart(i).Add(time.Hour).Format(time.RFC3339), "deleted_at": nil,
	}
}

func granolaTestNote(i int, withTranscript bool) map[string]any {
	n := granolaTestSummary(i)
	start := granolaTestStart(i)
	n["web_url"] = "https://notes.granola.ai/d/" + granolaTestID(i)
	n["calendar_event"] = map[string]any{"event_title": "Calendar planning", "invitees": []any{map[string]any{"email": "pat@example.com"}},
		"scheduled_start_time": start.Format(time.RFC3339), "scheduled_end_time": start.Add(45 * time.Minute).Format(time.RFC3339)}
	n["attendees"] = []any{map[string]any{"name": "Andy", "email": "andy@example.com"}, map[string]any{"name": nil, "email": "pat@example.com"}}
	n["summary_markdown"] = "## Decisions\n- Ship on Friday"
	n["private_notes_markdown"] = "remember the demo"
	if i == 1 {
		n["title"] = ""
		n["attendees"] = []any{}
		n["summary_markdown"] = nil
		n["summary_text"] = "Plain summary"
		n["private_notes_markdown"] = nil
	}
	if withTranscript {
		n["transcript"] = []any{
			map[string]any{"speaker": map[string]any{"source": "microphone", "attribution": "me"}, "text": "Hello team", "start_time": start.Add(2 * time.Second).Format(time.RFC3339), "end_time": start.Add(4 * time.Second).Format(time.RFC3339)},
			map[string]any{"speaker": map[string]any{"source": "speaker", "attribution": "them", "name": "Pat"}, "text": "Ship it", "start_time": start.Add(67 * time.Second).Format(time.RFC3339), "end_time": start.Add(30 * time.Minute).Format(time.RFC3339)},
			map[string]any{"speaker": map[string]any{"source": "speaker", "attribution": "them"}, "text": "Agreed", "start_time": start.Add(70 * time.Second).Format(time.RFC3339)},
		}
	}
	return n
}

func (f *granolaFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer grn_test" {
			http.Error(w, `{"error":"Unauthorized - Invalid API key"}`, http.StatusUnauthorized)
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Path+"?"+r.URL.RawQuery)
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":"Rate limited"}`, http.StatusTooManyRequests)
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/notes", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		q := r.URL.Query()
		f.mu.Lock()
		total := f.total
		f.mu.Unlock()
		if total == 0 {
			total = 4
		}
		start := 0
		if c := q.Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		size, _ := strconv.Atoi(q.Get("page_size"))
		var notes []any
		for i := start; i < total && i < start+size; i++ {
			s := granolaTestSummary(i)
			if i == 3 && total == 4 {
				s["deleted_at"] = "2025-11-13T00:00:00Z"
			}
			notes = append(notes, s)
		}
		more := start+size < total
		reply(w, map[string]any{"notes": notes, "hasMore": more, "cursor": map[bool]any{true: strconv.Itoa(start + size), false: nil}[more]})
	})
	mux.HandleFunc("/v1/notes/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v1/notes/")
		id, sub, _ := strings.Cut(rest, "/")
		var idx = -1
		for i := 0; i < 60; i++ {
			if granolaTestID(i) == id {
				idx = i
			}
		}
		switch {
		case idx < 0 || idx == 2:
			http.Error(w, `{"error":"NOTE_NOT_FOUND"}`, http.StatusNotFound)
		case sub == "transcript":
			tr := granolaTestNote(idx, true)["transcript"].([]any)
			if r.URL.Query().Get("cursor") == "" {
				reply(w, map[string]any{"transcript": tr[:2], "hasMore": true, "cursor": "p2"})
				return
			}
			reply(w, map[string]any{"transcript": tr[2:], "hasMore": false})
		case idx == 4 && r.URL.Query().Get("include") == "transcript":
			http.Error(w, `{"error":"too large"}`, http.StatusRequestEntityTooLarge)
		case idx == 5:
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			reply(w, granolaTestNote(idx, r.URL.Query().Get("include") == "transcript"))
		}
	})
	return mux
}

func newGranolaTest(t *testing.T, f *granolaFixture, scope map[string]string) *granolaConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	old := granolaAPIBase
	granolaAPIBase = srv.URL
	t.Cleanup(func() { granolaAPIBase = old })
	t.Setenv(granolaTestKeyEnv, "grn_test")
	if scope == nil {
		scope = map[string]string{}
	}
	cfg := ConnectorConfig{Name: "granola", Type: TypeGranola, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: granolaTestKeyEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	gc := c.(*granolaConnector)
	gc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return gc
}

func TestGranolaValidate(t *testing.T) {
	reg := DefaultRegistry()
	ok := Auth{Env: "GRANOLA_API_KEY"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"include_private_notes": "false", "folder": "fol_1"}, ""},
		{"no key", Auth{}, nil, "auth.env"},
		{"client env", Auth{Env: "K", AccountIDEnv: "A"}, nil, "not used"},
		{"bad private", ok, map[string]string{"include_private_notes": "maybe"}, "include_private_notes"},
		{"bad max", ok, map[string]string{"max_recordings": "0"}, "max_recordings"},
	}
	for _, tc := range cases {
		_, err := reg.New(ConnectorConfig{Name: "g", Type: TypeGranola, Layer: "project", Scope: tc.scope, Auth: tc.auth}, Deps{})
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestGranolaSyncNotes(t *testing.T) {
	f := &granolaFixture{}
	c := newGranolaTest(t, f, map[string]string{"tags": "meetings", "folder": "fol_1", "space": "spc_2"})
	ctx := context.Background()
	if c.Type() != TypeGranola {
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
	if len(pages) != 2 || pages[0].ID != granolaTestID(0) || pages[1].ID != granolaTestID(1) {
		t.Fatalf("deleted notes and notes without generated notes are skipped: %+v", pages)
	}
	if want := granolaTestStart(0).Add(time.Hour).Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q (updated_at)", next, want)
	}
	f.mu.Lock()
	q := f.queries[1]
	f.mu.Unlock()
	for _, want := range []string{"updated_after=2025-10-15T06%3A00%3A00Z", "page_size=30", "folder_id=fol_1", "space_id=spc_2"} {
		if !strings.Contains(q, want) {
			t.Fatalf("listing query %q lacks %q", q, want)
		}
	}
	a := pages[0]
	for _, want := range []string{"# Planning 0", "## Summary (Granola)", "## Decisions", "- Ship on Friday", "### My notes", "remember the demo", "## Transcript", "[00:00] Me: Hello team", "[01:05] Pat: Ship it", "[01:08] Them: Agreed", "- **Host:** Andy", "- **Participants:** Andy, pat@example.com", "- **Duration:** 1m8s", "https://notes.granola.ai/d/" + granolaTestID(0)} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("note lacks %q:\n%s", want, a.Markdown)
		}
	}
	if a.Attrs["granola_recording_id"] != granolaTestID(0) || a.Attrs["tags"] != "meetings" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if b := pages[1]; b.Title != "Calendar planning" || !strings.Contains(b.Markdown, "Plain summary") || strings.Contains(b.Markdown, "My notes") || !strings.Contains(b.Markdown, "- **Participants:** pat@example.com") {
		t.Fatalf("calendar title, summary_text, invitee fallback: %s", b.Markdown)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || recs[0].HasMedia || recs[0].Tags[0] != "meetings" {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, granolaTestID(0))
	if err != nil || !strings.Contains(content.Transcript, "Me: Hello team") || !strings.Contains(content.Summary, "Ship on Friday") || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, granolaTestID(2)); !errors.Is(err, errGranolaNotFound) {
		t.Fatalf("not found: %v", err)
	}
	if _, err := c.ReadMeeting(ctx, "not_bad"); err == nil || !strings.Contains(err.Error(), "invalid note id") {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := c.MediaLink(ctx, granolaTestID(0)); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "keeps no meeting audio") {
		t.Fatalf("media link: %v", err)
	}
	if _, err := c.MediaLink(ctx, "bad"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("media link for a bad id: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestGranolaLargeTranscriptPaginationAndErrors(t *testing.T) {
	f := &granolaFixture{total: 6}
	c := newGranolaTest(t, f, map[string]string{"include_private_notes": "false", "include_participants": "false"})
	content, err := c.ReadMeeting(context.Background(), granolaTestID(4))
	if err != nil || !strings.Contains(content.Transcript, "Them: Agreed") || strings.Contains(content.Summary, "My notes") {
		t.Fatalf("413 falls back to the paged transcript: %+v, %v", content, err)
	}
	var pages []Page
	_, err = c.Sync(context.Background(), "2025-11-10T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil })
	var pe *PartialError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), granolaTestID(5)) || len(pages) != 4 {
		t.Fatalf("a failing note is skipped and reported: %d pages, %v", len(pages), err)
	}
	if strings.Contains(pages[0].Markdown, "Participants") {
		t.Fatal("include_participants=false")
	}

	big := &granolaFixture{total: 65}
	c2 := newGranolaTest(t, big, map[string]string{"max_recordings": "100"})
	recs, err := c2.ListMeetings(context.Background())
	big.mu.Lock()
	queries := append([]string(nil), big.queries...)
	big.mu.Unlock()
	if err != nil || len(recs) != 65 || len(queries) != 3 || !strings.Contains(queries[2], "cursor=60") || c2.Truncated() {
		t.Fatalf("pagination: %d, %v, %v", len(recs), err, queries)
	}
	capped := &granolaFixture{total: 65}
	c3 := newGranolaTest(t, capped, map[string]string{"max_recordings": "5"})
	if recs, err := c3.ListMeetings(context.Background()); err != nil || len(recs) != 5 || !c3.Truncated() {
		t.Fatalf("capped: %d, %v, %v", len(recs), err, c3.Truncated())
	}

	bad := newGranolaTest(t, &granolaFixture{}, nil)
	t.Setenv(granolaTestKeyEnv, "grn_wrong")
	if _, err := bad.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Granola card") {
		t.Fatalf("401: %v", err)
	}
	if st := bad.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(granolaTestKeyEnv, "")
	if _, err := bad.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing key: %v", err)
	}

	rl := &granolaFixture{rateLimitOnce: true}
	c4 := newGranolaTest(t, rl, nil)
	c4.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c4.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if recs, err := c4.ListMeetings(context.Background()); err != nil || len(recs) != 3 || waited != 2*time.Second {
		t.Fatalf("429 with Retry-After: %d, %v, waited %v", len(recs), err, waited)
	}
	rl2 := &granolaFixture{rateLimitOnce: true}
	c5 := newGranolaTest(t, rl2, nil)
	if _, err := c5.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("429 without retries left: %v", err)
	}
}

func TestGranolaHelpers(t *testing.T) {
	if got := granolaUser("x"); got != "" {
		t.Errorf("user of a string = %q", got)
	}
	if got := granolaTranscript([]map[string]any{{"text": "no time"}, {"speaker": map[string]any{"diarization_label": "Speaker B"}, "text": "labelled"}}); got != "[00:00] no time\n[00:00] Speaker B: labelled" {
		t.Errorf("transcript = %q", got)
	}
	c := &granolaConnector{cfg: ConnectorConfig{}}
	if rec := c.parseGranolaNote(map[string]any{"id": granolaTestID(9), "created_at": "2025-11-01T00:00:00Z"}); rec.ID != granolaTestID(9) || rec.DurationMS != 0 || rec.StartAt.IsZero() {
		t.Errorf("minimal note = %+v", rec)
	}
	if got := c.granolaContent(map[string]any{}, nil, nil); !got.Pending {
		t.Errorf("empty note = %+v", got)
	}

	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			_, _ = w.Write([]byte("<html>"))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(granolaTestKeyEnv, "grn_test")
	old := granolaAPIBase
	granolaAPIBase = srv.URL
	t.Cleanup(func() { granolaAPIBase = old })
	hc := &granolaConnector{cfg: ConnectorConfig{Auth: Auth{Env: granolaTestKeyEnv}}, http: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1}), now: time.Now}
	if _, err := hc.get(context.Background(), "/v1/notes", nil); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Errorf("non-JSON: %v", err)
	}
	if _, _, err := hc.getNote(context.Background(), granolaTestID(1)); err == nil || errors.Is(err, errGranolaNotFound) {
		t.Errorf("500: %v", err)
	}
}
