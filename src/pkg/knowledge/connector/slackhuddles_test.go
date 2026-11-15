package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const slackTestTokenEnv = "HIVE_TEST_SLACK_TOKEN"

// slackFixture is an in-memory Slack Web API.
type slackFixture struct {
	mu            sync.Mutex
	srvURL        string
	tokenError    string
	historyError  string
	noCanvasRead  bool
	rateLimitOnce bool
	rateLimited   int
	historyArgs   []string
}

func slackHuddleMsg(ts, roomID string, start, end int64, ended bool, files ...string) map[string]any {
	attached := []any{}
	for _, f := range files {
		attached = append(attached, f)
	}
	return map[string]any{
		"type": "message", "subtype": "huddle_thread", "ts": ts, "text": "A huddle started",
		"room": map[string]any{
			"id": roomID, "created_by": "U1", "date_start": float64(start), "date_end": float64(end), "has_ended": ended,
			"participant_history": []any{"U1", "U2"}, "participants": []any{}, "thread_root_ts": ts, "attached_file_ids": attached,
		},
	}
}

func (f *slackFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code string, extra map[string]any) {
		m := map[string]any{"ok": false, "error": code}
		for k, v := range extra {
			m[k] = v
		}
		reply(w, m)
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		te := f.tokenError
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			fail(w, "not_authed", nil)
			return false
		}
		if te != "" {
			fail(w, te, nil)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		reply(w, map[string]any{"ok": true, "url": "https://acme.slack.com/", "team": "Acme", "user": "hive-bot", "bot_id": "B1"})
	})
	mux.HandleFunc("/api/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			f.mu.Unlock()
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			return
		}
		he := f.historyError
		q := r.URL.Query()
		f.historyArgs = append(f.historyArgs, q.Get("channel")+"@"+q.Get("oldest"))
		f.mu.Unlock()
		if he != "" {
			fail(w, he, map[string]any{"needed": "channels:history", "provided": "users:read"})
			return
		}
		all := []any{
			slackHuddleMsg("1763046000.000100", "R1", 1763046000, 1763047800, true),          // notes in thread
			map[string]any{"type": "message", "ts": "1763045000.000100", "text": "hello"},    // plain message
			slackHuddleMsg("1763040000.000200", "R2", 1763040000, 0, false),                  // still live
			slackHuddleMsg("1763030000.000300", "R3", 1763030000, 1763030600, true, "FATT1"), // notes attached
			slackHuddleMsg("1763020000.000400", "R4", 1763020000, 1763020300, true),          // no notes
		}
		if q.Get("limit") == "1" && q.Get("latest") != "" {
			for _, m := range all {
				if m.(map[string]any)["ts"] == q.Get("latest") {
					reply(w, map[string]any{"ok": true, "messages": []any{m}})
					return
				}
			}
			reply(w, map[string]any{"ok": true, "messages": []any{}})
			return
		}
		if q.Get("limit") == "1" {
			reply(w, map[string]any{"ok": true, "messages": all[:1]})
			return
		}
		if q.Get("cursor") == "" {
			reply(w, map[string]any{"ok": true, "messages": all[:2], "has_more": true, "response_metadata": map[string]any{"next_cursor": "c2"}})
			return
		}
		reply(w, map[string]any{"ok": true, "messages": all[2:], "has_more": false})
	})
	mux.HandleFunc("/api/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		switch r.URL.Query().Get("ts") {
		case "1763046000.000100":
			reply(w, map[string]any{"ok": true, "messages": []any{
				map[string]any{"ts": "1763046000.000100"},
				map[string]any{"ts": "1763047900.000100", "files": []any{
					map[string]any{"id": "FOTHER", "filetype": "canvas", "title": "Agenda"},
					map[string]any{"id": "FNOTES1", "filetype": "quip", "pretty_type": "Canvas", "title": "Huddle notes: Weekly sync"},
					map[string]any{"id": "FIMG", "filetype": "png", "title": "huddle screenshot"},
				}},
			}})
		case "1763020000.000400":
			fail(w, "thread_not_found", nil)
		default:
			reply(w, map[string]any{"ok": true, "messages": []any{map[string]any{"ts": r.URL.Query().Get("ts")}}})
		}
	})
	mux.HandleFunc("/api/files.info", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		switch r.URL.Query().Get("file") {
		case "FATT1":
			reply(w, map[string]any{"ok": true, "file": map[string]any{"id": "FATT1", "filetype": "canvas", "title": "", "url_private_download": f.srvURL + "/files-pri/FATT1/download"}})
		case "FNOTES1":
			reply(w, map[string]any{"ok": true, "file": map[string]any{"id": "FNOTES1", "filetype": "quip", "url_private_download": f.srvURL + "/files-pri/FNOTES1/download"}})
		default:
			fail(w, "file_not_found", nil)
		}
	})
	mux.HandleFunc("/api/canvases.getContent", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		noRead := f.noCanvasRead
		f.mu.Unlock()
		if r.Method != http.MethodPost {
			fail(w, "invalid_request", nil)
			return
		}
		_ = r.ParseForm()
		if noRead {
			fail(w, "missing_scope", map[string]any{"needed": "canvases:read", "provided": "files:read"})
			return
		}
		switch r.Form.Get("canvas_id") {
		case "FNOTES1":
			reply(w, map[string]any{"ok": true, "content": "# Huddle notes\n\n## Summary\nThe team agreed to ship.\n\n## Action items\n- Andy opens the PR\n\n## Transcript\n[00:05] Andy: Hello\n[00:09] Pat: Ship it\n"})
		case "FATT1":
			reply(w, map[string]any{"ok": true, "content": "Short notes, no transcript."})
		default:
			fail(w, "canvas_not_found", nil)
		}
	})
	mux.HandleFunc("/files-pri/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("<h1>Huddle notes</h1><p>Downloaded notes.</p><h2>Transcript</h2><p>Andy: from the file</p>"))
	})
	mux.HandleFunc("/api/users.info", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		switch r.URL.Query().Get("user") {
		case "U1":
			reply(w, map[string]any{"ok": true, "user": map[string]any{"id": "U1", "name": "andy", "profile": map[string]any{"display_name": "Andy"}}})
		case "U2":
			reply(w, map[string]any{"ok": true, "user": map[string]any{"id": "U2", "real_name": "Pat Lee", "profile": map[string]any{}}})
		default:
			fail(w, "user_not_found", nil)
		}
	})
	mux.HandleFunc("/api/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		reply(w, map[string]any{"ok": true, "channel": map[string]any{"id": r.URL.Query().Get("channel"), "name": "eng"}})
	})
	return mux
}

func newSlackTest(t *testing.T, f *slackFixture, scope map[string]string) *slackHuddlesConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	f.srvURL = srv.URL
	old := slackAPIBase
	slackAPIBase = srv.URL + "/api"
	t.Cleanup(func() { slackAPIBase = old })
	t.Setenv(slackTestTokenEnv, "xoxb-test")
	if scope == nil {
		scope = map[string]string{}
	}
	if scope["channels"] == "" {
		scope["channels"] = "C0123ABC"
	}
	cfg := ConnectorConfig{Name: "team", Type: TypeSlackHuddles, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{Env: slackTestTokenEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	sc := c.(*slackHuddlesConnector)
	sc.now = func() time.Time { return time.Unix(1763100000, 0).UTC() }
	return sc
}

func TestSlackHuddlesValidate(t *testing.T) {
	reg := DefaultRegistry()
	base := func(auth Auth, scope map[string]string) error {
		_, err := reg.New(ConnectorConfig{Name: "s", Type: TypeSlackHuddles, Layer: "project", Scope: scope, Auth: auth}, Deps{})
		return err
	}
	ok := Auth{Env: "SLACK_TOKEN"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"channels": "C0123ABC, G0456DEF"}, ""},
		{"no token", Auth{}, map[string]string{"channels": "C0123ABC"}, "auth.env"},
		{"client env", Auth{Env: "T", ClientIDEnv: "A"}, map[string]string{"channels": "C0123ABC"}, "not used"},
		{"no channels", ok, nil, "scope.channels"},
		{"bad channel", ok, map[string]string{"channels": "#general"}, "not a Slack conversation id"},
		{"bad since", ok, map[string]string{"channels": "C0123ABC", "since_days": "0"}, "since_days"},
	}
	for _, tc := range cases {
		err := base(tc.auth, tc.scope)
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestSlackHuddlesSyncNotesAndTranscript(t *testing.T) {
	f := &slackFixture{}
	c := newSlackTest(t, f, map[string]string{"tags": "huddles"})
	ctx := context.Background()
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected || st.Account != "hive-bot @ Acme" {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 2 || pages[0].ID != "C0123ABC:1763046000.000100" || pages[1].ID != "C0123ABC:1763030000.000300" {
		t.Fatalf("only ended huddles with a notes canvas become facts: %+v", pages)
	}
	if want := time.Unix(1763046000, 0).UTC().Format(time.RFC3339); string(next) != want {
		t.Fatalf("cursor = %q, want %q", next, want)
	}
	f.mu.Lock()
	first := f.historyArgs[1]
	f.mu.Unlock()
	if first != "C0123ABC@1760508000" {
		t.Fatalf("first sync reaches since_days back: %q", first)
	}
	a := pages[0]
	for _, want := range []string{"# Huddle notes: Weekly sync", "## Summary (Slack huddles)", "The team agreed to ship.", "- Andy opens the PR", "## Transcript", "[00:05] Andy: Hello", "- **Host:** Andy", "- **Participants:** Andy, Pat Lee", "- **Duration:** 30m0s", "https://acme.slack.com/archives/C0123ABC/p1763046000000100"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("huddle lacks %q:\n%s", want, a.Markdown)
		}
	}
	if strings.Contains(a.Markdown, "Agenda") {
		t.Fatal("the huddle-notes canvas wins over other canvases in the thread")
	}
	if a.Attrs["slack_huddle_recording_id"] != a.ID || a.Attrs["slack_huddle_meeting_id"] != "R1" || a.Attrs["tags"] != "huddles" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	b := pages[1]
	if b.Title != "Huddle in #eng" || !strings.Contains(b.Markdown, "Short notes, no transcript.") || strings.Contains(b.Markdown, "## Transcript") {
		t.Fatalf("attached canvas without transcript: %+v", b)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 2 || recs[0].HasMedia {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "C0123ABC:1763046000.000100")
	if err != nil || !strings.Contains(content.Transcript, "Pat: Ship it") || !strings.Contains(content.Summary, "agreed to ship") {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "C0123ABC:1763000000.000999"); err == nil {
		t.Fatal("unknown huddle must fail")
	}
	for _, bad := range []string{"nope", "C0123ABC", "general:1763046000.000100", "C0123ABC:abc"} {
		if _, err := c.ReadMeeting(ctx, bad); err == nil || !strings.Contains(err.Error(), "invalid huddle id") {
			t.Fatalf("ReadMeeting(%q) = %v", bad, err)
		}
	}
	if _, err := c.MediaLink(ctx, a.ID); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "does not record huddle audio") {
		t.Fatalf("media link: %v", err)
	}
	if _, err := c.MediaLink(ctx, "bad"); err == nil || errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("media link for a bad id: %v", err)
	}
	if st, err := c.Disconnect(ctx); err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}

func TestSlackHuddlesCanvasDownloadFallback(t *testing.T) {
	f := &slackFixture{noCanvasRead: true}
	c := newSlackTest(t, f, map[string]string{"include_participants": "false"})
	content, err := c.ReadMeeting(context.Background(), "C0123ABC:1763046000.000100")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Summary, "Downloaded notes.") || !strings.Contains(content.Transcript, "Andy: from the file") || len(content.Participants) != 0 {
		t.Fatalf("download fallback = %+v", content)
	}
}

func TestSlackHuddlesErrorsAndRateLimit(t *testing.T) {
	f := &slackFixture{tokenError: "token_revoked"}
	c := newSlackTest(t, f, nil)
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Slack huddles card") {
		t.Fatalf("revoked token: %v", err)
	}
	if st := c.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe: %+v", st)
	}
	t.Setenv(slackTestTokenEnv, "")
	if _, err := c.ListMeetings(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("missing token: %v", err)
	}

	scope := &slackFixture{historyError: "missing_scope"}
	c2 := newSlackTest(t, scope, nil)
	st, _ := c2.Connect(context.Background())
	if st.Connected || !strings.Contains(st.Message, "needs scope channels:history") {
		t.Fatalf("missing scope on the card: %+v", st)
	}
	nic := &slackFixture{historyError: "not_in_channel"}
	c3 := newSlackTest(t, nic, nil)
	if _, err := c3.ListMeetings(context.Background()); err == nil || !strings.Contains(err.Error(), "/invite") {
		t.Fatalf("not in channel: %v", err)
	}

	rl := &slackFixture{rateLimitOnce: true}
	c4 := newSlackTest(t, rl, nil)
	c4.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c4.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	recs, err := c4.ListMeetings(context.Background())
	if err != nil || len(recs) != 2 || waited != 3*time.Second {
		t.Fatalf("429 with Retry-After: %+v, %v, waited %v", recs, err, waited)
	}
}

func TestSlackHuddlesHelpers(t *testing.T) {
	s, tr := splitSlackNotes("Notes\n\n### Huddle transcript\nA: hi")
	if s != "Notes" || tr != "A: hi" {
		t.Errorf("split = %q / %q", s, tr)
	}
	if s, tr := splitSlackNotes("  only notes "); s != "only notes" || tr != "" {
		t.Errorf("no transcript = %q / %q", s, tr)
	}
	if got := slackTSTime("bad"); !got.IsZero() {
		t.Errorf("bad ts = %v", got)
	}
	if got := slackTSTime(nil); !got.IsZero() {
		t.Errorf("nil ts = %v", got)
	}
	if _, ok := parseSlackHuddle("C1", map[string]any{"subtype": "huddle_thread"}); ok {
		t.Error("a huddle message without room is skipped")
	}
	h, ok := parseSlackHuddle("C1", map[string]any{"subtype": "huddle_thread", "ts": "1763046000.000100", "room": map[string]any{"has_ended": true, "participants": []any{"U9"}}})
	if !ok || !h.StartAt.Equal(time.Unix(1763046000, 0).UTC()) || h.ThreadTS != h.TS || len(h.Members) != 1 {
		t.Errorf("room without dates = %+v %v", h, ok)
	}
	for _, link := range []string{"https://files.slack.com/files-pri/T1-F1/download/x", "https://acme.slack.com/x"} {
		if err := slackTrustedFileURL(link); err != nil {
			t.Errorf("%s: %v", link, err)
		}
	}
	for _, link := range []string{"https://evil.example/x", "", "::"} {
		if err := slackTrustedFileURL(link); err == nil {
			t.Errorf("%q must be rejected", link)
		}
	}
	e := &slackError{Method: "conversations.history", Code: "channel_not_found"}
	if !strings.Contains(e.Error(), "check scope.channels") {
		t.Errorf("channel_not_found = %q", e.Error())
	}
	if got := slackHTMLText("<h2>Action items</h2><ul><li>Ship &amp; tell</li><li>Rest</li></ul><p>a<br>b</p>"); got != "## Action items\n\n- Ship & tell\n\n- Rest\n\na\nb" {
		t.Errorf("slackHTMLText = %q", got)
	}
	if !isSlackCanvas(map[string]any{"pretty_type": "Canvas"}) || isSlackCanvas(map[string]any{"filetype": "png"}) {
		t.Error("canvas detection")
	}
}

func TestSlackHuddlesEdgeCases(t *testing.T) {
	f := &slackFixture{}
	c := newSlackTest(t, f, map[string]string{"max_recordings": "1"})
	ctx := context.Background()
	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 1 || !c.Truncated() {
		t.Fatalf("truncated listing: %+v, %v, truncated=%v", recs, err, c.Truncated())
	}
	// Unknown users keep their id; a failed canvas read is reported.
	if n := c.userName(ctx, "U404"); n != "U404" {
		t.Fatalf("unknown user = %q", n)
	}
	if _, err := c.canvasMarkdown(ctx, "FMISSING"); err == nil || !strings.Contains(err.Error(), "canvas FMISSING") {
		t.Fatalf("missing canvas: %v", err)
	}
	if _, err := c.fetchContent(ctx, slackHuddle{CanvasID: "FMISSING"}); err == nil {
		t.Fatal("a canvas that cannot be read fails the item")
	}
	if got, err := c.fetchContent(ctx, slackHuddle{}); err != nil || !got.Pending {
		t.Fatalf("no canvas = %+v, %v", got, err)
	}

	// Non-JSON, HTTP 401 and an ok:false without a code.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) })
	mux.HandleFunc("/api/users.info", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusUnauthorized) })
	mux.HandleFunc("/api/conversations.info", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":false}`)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	slackAPIBase = srv.URL + "/api"
	if _, err := c.call(ctx, "auth.test", nil, false); err == nil || !strings.Contains(err.Error(), "unexpected reply") {
		t.Fatalf("non-JSON reply: %v", err)
	}
	if _, err := c.call(ctx, "users.info", nil, false); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("HTTP 401: %v", err)
	}
	var se *slackError
	if _, err := c.call(ctx, "conversations.info", nil, false); !errors.As(err, &se) || se.Code != "unknown_error" {
		t.Fatalf("ok:false without a code: %v", err)
	}
	if n := c.channelName(ctx, "C0999"); n != "" {
		t.Fatalf("failed channel lookup = %q", n)
	}
	if _, err := c.getHuddle(ctx, "C0123ABC:1763046000.000100"); err == nil {
		t.Fatal("getHuddle must fail when auth.test fails")
	}
}
