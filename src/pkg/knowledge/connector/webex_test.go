package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	webexTestClientIDEnv     = "HIVE_TEST_WEBEX_CLIENT_ID"
	webexTestClientSecretEnv = "HIVE_TEST_WEBEX_CLIENT_SECRET"
	// Webex VTT: the cue identifier carries the speaker, the body repeats it.
	webexTestVTT = "WEBVTT\n\n1 \"Andy Anderson\" (2405637888)\n00:00:02.000 --> 00:00:04.000\nAndy Anderson: Kick-off\n\n2 \"Pat Lee\" (2405637999)\n00:01:10.000 --> 00:01:12.000\nPat Lee: Ship Friday\n"
)

// webexFixture is an in-memory Webex REST API + OAuth server.
type webexFixture struct {
	mu            sync.Mutex
	srvURL        string
	grants        []string
	unauthorized  bool
	peopleDenied  bool
	rateLimitOnce bool
	rateLimited   int
	listQueries   []url.Values
	noDownload    bool
}

func (f *webexFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/v1/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" {
			http.Error(w, `{"message":"invalid client"}`, http.StatusUnauthorized)
			return
		}
		grant := r.Form.Get("grant_type")
		f.mu.Lock()
		f.grants = append(f.grants, grant)
		f.mu.Unlock()
		switch grant {
		case "authorization_code":
			if r.Form.Get("code") != "the-code" || r.Form.Get("redirect_uri") != webexDefaultRedirectURI {
				http.Error(w, `{"error":"invalid_grant","error_description":"bad code"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-1", "expires_in": 1209599, "refresh_token": "rt-1", "refresh_token_expires_in": 7775999})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"message":"The refresh token provided is expired, revoked, malformed, or invalid."}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-2", "expires_in": 1209599, "refresh_token": "rt-2"})
		default:
			http.Error(w, `{"message":"unsupported"}`, http.StatusBadRequest)
		}
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		unauthorized := f.unauthorized
		f.mu.Unlock()
		if unauthorized || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"message":"The request requires a valid access token set in the Authorization request header."}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/people/me", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		denied := f.peopleDenied
		f.mu.Unlock()
		if denied {
			http.Error(w, `{"message":"missing scope spark:people_read"}`, http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"emails": []any{"andy@example.com"}, "displayName": "Andy"})
	})
	recordings := []any{
		map[string]any{"id": "rec-1", "meetingId": "mtg-a", "topic": "Weekly sync", "timeRecorded": "2026-11-12T15:00:00Z", "durationSeconds": 1800.0, "playbackUrl": "https://acme.webex.com/recordingservice/play/rec-1", "hostEmail": "andy@example.com", "status": "available"},
		map[string]any{"id": "rec-2", "meetingId": "mtg-b", "topic": "Design review", "timeRecorded": "2026-11-11T09:00:00Z", "durationSeconds": 600.0, "status": "available"},
		map[string]any{"id": "rec-3", "meetingId": "mtg-z", "topic": "Deleted", "timeRecorded": "2026-11-10T09:00:00Z", "status": "deleted"},
	}
	transcripts := []any{
		map[string]any{"id": "tr-a", "meetingId": "mtg-a", "meetingTopic": "Weekly sync", "startTime": "2026-11-12T15:00:00Z", "status": "available"},
		map[string]any{"id": "tr-c", "meetingId": "mtg-c", "meetingTopic": "Transcript only", "startTime": "2026-11-13T10:00:00Z", "status": "available"},
	}
	// filter applies the meetingId query and pages after the first item.
	list := func(w http.ResponseWriter, r *http.Request, items []any) {
		q := r.URL.Query()
		if q.Get("from") == "" || q.Get("to") == "" || q.Get("max") != "100" {
			http.Error(w, `{"message":"bad range"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.listQueries = append(f.listQueries, q)
		f.mu.Unlock()
		from, _ := time.Parse(time.RFC3339, q.Get("from"))
		to, _ := time.Parse(time.RFC3339, q.Get("to"))
		var out []any
		for _, it := range items {
			m := it.(map[string]any)
			if id := q.Get("meetingId"); id != "" && m["meetingId"] != id {
				continue
			}
			at, _ := m["timeRecorded"].(string)
			if at == "" {
				at, _ = m["startTime"].(string)
			}
			// from is inclusive, to exclusive, as in the API.
			if when, _ := time.Parse(time.RFC3339, at); when.Before(from) || !when.Before(to) {
				continue
			}
			out = append(out, it)
		}
		if q.Get("cursor") == "" && len(out) > 1 {
			next := *r.URL
			nq := next.Query()
			nq.Set("cursor", "2")
			next.RawQuery = nq.Encode()
			w.Header().Set("Link", `<`+f.srvURL+next.Path+"?"+next.RawQuery+`>; rel="next"`)
			out = out[:1]
		} else if q.Get("cursor") == "2" {
			out = out[1:]
		}
		writeJSON(w, map[string]any{"items": out})
	}
	mux.HandleFunc("/v1/recordings", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			f.mu.Unlock()
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"message":"Too many requests"}`, http.StatusTooManyRequests)
			return
		}
		f.mu.Unlock()
		if r.URL.Query().Get("max") == "1" {
			writeJSON(w, map[string]any{"items": []any{}})
			return
		}
		list(w, r, recordings)
	})
	mux.HandleFunc("/v1/recordings/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/recordings/")
		f.mu.Lock()
		noDownload := f.noDownload
		f.mu.Unlock()
		m := map[string]any{"id": id, "playbackUrl": "https://acme.webex.com/recordingservice/play/" + id}
		if !noDownload {
			m["temporaryDirectDownloadLinks"] = map[string]any{
				"audioDownloadLink":     "https://acme.webex.com/nbr/MultiThreadDownloadServlet?audio=" + id,
				"recordingDownloadLink": "https://acme.webex.com/nbr/MultiThreadDownloadServlet?mp4=" + id,
				"expiration":            "2026-11-14T15:00:00Z",
			}
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("/v1/meetingTranscripts", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		list(w, r, transcripts)
	})
	mux.HandleFunc("/v1/meetingTranscripts/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/meetingTranscripts/tr-") || !strings.HasSuffix(r.URL.Path, "/download") || r.URL.Query().Get("format") != "vtt" {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/vtt")
		_, _ = w.Write([]byte(webexTestVTT))
	})
	mux.HandleFunc("/v1/meetingSummaries", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if r.URL.Query().Get("meetingId") != "mtg-a" {
			http.Error(w, `{"message":"AI Assistant is not enabled"}`, http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{
			"id": "sum-1", "meetingId": "mtg-a", "status": "available",
			"notes":       map[string]any{"content": "<p>The team agreed to <b>ship</b>.</p>"},
			"actionItems": []any{map[string]any{"content": "Andy opens the PR"}, map[string]any{"content": " "}},
		}}})
	})
	mux.HandleFunc("/v1/meetingParticipants", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, map[string]any{"items": []any{map[string]any{"displayName": "Andy Anderson"}, map[string]any{"email": "pat@example.com"}}})
	})
	return mux
}

func newWebexTest(t *testing.T, f *webexFixture, scope map[string]string) *webexConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	f.srvURL = srv.URL
	old := webexAPIBase
	webexAPIBase = srv.URL + "/v1"
	t.Cleanup(func() { webexAPIBase = old })
	t.Setenv(webexTestClientIDEnv, "cid")
	t.Setenv(webexTestClientSecretEnv, "csecret")
	cfg := ConnectorConfig{Name: "calls", Type: TypeWebex, Enabled: true, Layer: "personal", Scope: scope, Auth: Auth{ClientIDEnv: webexTestClientIDEnv, ClientSecretEnv: webexTestClientSecretEnv}}
	deps := Deps{StateDir: t.TempDir(), KnowledgeDir: t.TempDir(), HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})}
	c, err := DefaultRegistry().New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	wc := c.(*webexConnector)
	wc.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	return wc
}

func TestWebexValidate(t *testing.T) {
	reg := DefaultRegistry()
	base := func(auth Auth, scope map[string]string) error {
		_, err := reg.New(ConnectorConfig{Name: "w", Type: TypeWebex, Layer: "personal", Scope: scope, Auth: auth}, Deps{})
		return err
	}
	ok := Auth{ClientIDEnv: "WEBEX_ID", ClientSecretEnv: "WEBEX_SECRET"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"ok", ok, map[string]string{"site_url": "acme.webex.com"}, ""},
		{"auth.env", Auth{Env: "TOKEN", ClientIDEnv: "A", ClientSecretEnv: "B"}, nil, "auth.env/auth.file are not used"},
		{"s2s", Auth{Mode: AuthModeServerToServer, ClientIDEnv: "A", ClientSecretEnv: "B"}, nil, "auth.mode"},
		{"missing secret", Auth{ClientIDEnv: "A"}, nil, "client_secret_env"},
		{"teams setting", Auth{ClientIDEnv: "A", ClientSecretEnv: "B", TenantIDEnv: "T"}, nil, "teams"},
		{"bad redirect", ok, map[string]string{"redirect_uri": "nope"}, "redirect_uri"},
		{"bad site", ok, map[string]string{"site_url": "https://acme.webex.com/"}, "site_url"},
		{"bad max", ok, map[string]string{"max_recordings": "x"}, "max_recordings"},
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

func TestWebexConnectCompleteAndSync(t *testing.T) {
	f := &webexFixture{}
	c := newWebexTest(t, f, map[string]string{"site_url": "acme.webex.com"})
	ctx := context.Background()
	if st := c.AuthStatus(ctx, false); st.Connected {
		t.Fatalf("fresh connector must not be connected: %+v", st)
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Pending {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	u, _ := url.Parse(st.AuthURL)
	if !strings.HasPrefix(st.AuthURL, webexAPIBase+"/authorize?") || u.Query().Get("client_id") != "cid" || u.Query().Get("scope") != webexScopes || u.Query().Get("redirect_uri") != webexDefaultRedirectURI {
		t.Fatalf("authorization URL = %q", st.AuthURL)
	}
	if st := c.AuthStatus(ctx, true); !st.Pending {
		t.Fatalf("pending sign-in: %+v", st)
	}
	for _, bad := range []AuthCompletion{
		{Tokens: "{}"},
		{RedirectURL: "nope"},
		{RedirectURL: webexDefaultRedirectURI + "?error=access_denied"},
		{RedirectURL: webexDefaultRedirectURI + "?state=x"},
		{RedirectURL: webexDefaultRedirectURI + "?code=the-code&state=wrong"},
	} {
		if _, err := c.CompleteAuth(ctx, bad); err == nil {
			t.Fatalf("CompleteAuth(%+v) must fail", bad)
		}
	}
	st, err = c.CompleteAuth(ctx, AuthCompletion{RedirectURL: webexDefaultRedirectURI + "?code=the-code&state=" + u.Query().Get("state")})
	if err != nil || !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("CompleteAuth = %+v, %v", st, err)
	}
	assertFileMode(t, c.store.path, meetingTokenFileMode)

	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	ids := []string{}
	for _, p := range pages {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "mtg-c,mtg-a,mtg-b" {
		t.Fatalf("meetings newest first, deleted recordings dropped: %v", ids)
	}
	if next != "2026-11-13T10:00:00Z" {
		t.Fatalf("cursor = %q", next)
	}
	a := pages[1]
	for _, want := range []string{"# Weekly sync", "[00:02] Andy Anderson: Kick-off", "[01:10] Pat Lee: Ship Friday", "## Summary (Webex)", "The team agreed to ship.", "### Action items", "- Andy opens the PR", "- **Participants:** Andy Anderson, pat@example.com", "- **Duration:** 30m0s", "- **Host:** andy@example.com"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("meeting a lacks %q:\n%s", want, a.Markdown)
		}
	}
	if a.Attrs["webex_recording_id"] != "mtg-a" || a.Attrs["webex_meeting_id"] != "mtg-a" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	if strings.Contains(a.Markdown, "- **Recording (Webex):**") {
		t.Fatal("the media line is written only with include_video_link")
	}
	b := pages[2]
	if b.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(b.Markdown, MeetingPendingText) {
		t.Fatalf("a recording without transcript or summary is pending: %+v", b)
	}
	f.mu.Lock()
	q0 := f.listQueries[0]
	f.mu.Unlock()
	if q0.Get("siteUrl") != "acme.webex.com" || q0.Get("to") != "2026-11-14T12:01:00Z" {
		t.Fatalf("listing query = %v", q0)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 3 || recs[1].Topic != "Weekly sync" || !recs[1].HasMedia || recs[0].HasMedia {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "mtg-a")
	if err != nil || content.Pending || !strings.Contains(content.Transcript, "Kick-off") {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "a/b"); err == nil {
		t.Fatal("ids outside the id alphabet are rejected")
	}
	if _, err := c.ReadMeeting(ctx, "mtg-unknown"); err == nil || !strings.Contains(err.Error(), "mtg-unknown") {
		t.Fatalf("unknown meeting: %v", err)
	}

	link, err := c.MediaLink(ctx, "mtg-a")
	if err != nil || link.URL != "https://acme.webex.com/nbr/MultiThreadDownloadServlet?audio=rec-1" || !link.ExpiresAt.Equal(time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("media link = %+v, %v", link, err)
	}
	if _, err := c.MediaLink(ctx, "mtg-c"); !errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("transcript-only meeting: %v", err)
	}
	f.mu.Lock()
	f.noDownload = true
	f.mu.Unlock()
	if _, err := c.MediaLink(ctx, "mtg-a"); !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "recordingservice/play/rec-1") {
		t.Fatalf("prevent-download recording: %v", err)
	}
	if data, _ := os.ReadFile(c.store.path); strings.Contains(string(data), "MultiThreadDownloadServlet") {
		t.Fatal("download links are never persisted")
	}

	st, err = c.Disconnect(ctx)
	if err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
	if _, err := c.store.load(); !os.IsNotExist(err) {
		t.Fatalf("token store must be cleared: %v", err)
	}
	if _, err := c.ListMeetings(ctx); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("after sign-out: %v", err)
	}
}

func TestWebexRefreshRotatesAndDeadTokenIsNotConnected(t *testing.T) {
	f := &webexFixture{}
	c := newWebexTest(t, f, nil)
	_ = c.store.save(oauthTokens{AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: c.now().Add(-time.Minute), Account: "andy@example.com"})
	if st := c.AuthStatus(context.Background(), false); !st.Connected {
		t.Fatalf("stored sign-in reads as connected: %+v", st)
	}
	tok, err := c.accessToken(context.Background())
	if err != nil || tok.AccessToken != "at-2" {
		t.Fatalf("refresh = %+v, %v", tok, err)
	}
	if stored, _ := c.store.load(); stored.RefreshToken != "rt-2" || stored.Account != "andy@example.com" {
		t.Fatalf("rotated refresh token not saved: %+v", stored)
	}
	_ = c.store.save(oauthTokens{AccessToken: "old", RefreshToken: "dead", ExpiresAt: c.now().Add(-time.Minute)})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Webex card") {
		t.Fatalf("dead refresh token: %v", err)
	}
	_ = c.store.save(oauthTokens{AccessToken: "old"})
	if _, err := c.accessToken(context.Background()); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("no refresh token: %v", err)
	}
}

func TestWebexUnauthorizedRateLimitAndScopeFallback(t *testing.T) {
	f := &webexFixture{unauthorized: true}
	c := newWebexTest(t, f, nil)
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Webex card") {
		t.Fatalf("401: %v", err)
	}
	if st := c.AuthStatus(context.Background(), true); st.Connected || st.Message == "" {
		t.Fatalf("probe after 401: %+v", st)
	}
	if _, err := c.MediaLink(context.Background(), "mtg-a"); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("media link on 401: %v", err)
	}

	// Without spark:people_read the probe falls back to the recordings list.
	g := &webexFixture{peopleDenied: true}
	c2 := newWebexTest(t, g, nil)
	_ = c2.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c2.now().Add(time.Hour), Account: "kept@example.com"})
	if st := c2.AuthStatus(context.Background(), true); !st.Connected || st.Account != "kept@example.com" {
		t.Fatalf("scope fallback probe: %+v", st)
	}

	rl := &webexFixture{rateLimitOnce: true}
	c3 := newWebexTest(t, rl, map[string]string{"include_summary": "false", "include_participants": "false", "include_video_link": "true"})
	c3.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c3.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	_ = c3.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c3.now().Add(time.Hour)})
	var pages []Page
	if _, err := c3.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if waited != 5*time.Second {
		t.Fatalf("Retry-After must be honoured, waited %v", waited)
	}
	if len(pages) != 3 || strings.Contains(pages[1].Markdown, "## Summary") || strings.Contains(pages[1].Markdown, "Participants") || !strings.Contains(pages[1].Markdown, "- **Recording (Webex):** https://acme.webex.com/recordingservice/play/rec-1") {
		t.Fatalf("flags not honoured: %d pages\n%s", len(pages), pages[1].Markdown)
	}
}

func TestWebexPendingMeetingsAreReRead(t *testing.T) {
	f := &webexFixture{}
	c := newWebexTest(t, f, nil)
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	if err := os.MkdirAll(c.vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fact := "---\ntitle: Design review\nsource: webex\nconnector: calls\nsource_id: mtg-b\nattr_meeting_content: pending\nattr_recorded_at: 2026-11-11T09:00:00Z\n---\n\n# Design review\n"
	if err := os.WriteFile(filepath.Join(c.vaultDir, slugPrefix(TypeWebex, "calls")+"mtg-b.md"), []byte(fact), 0o600); err != nil {
		t.Fatal(err)
	}
	// The cursor is past mtg-b, so only the pending re-read brings it back.
	var ids []string
	if _, err := c.Sync(context.Background(), "2026-11-14T00:00:00Z", func(p Page) error { ids = append(ids, p.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "mtg-c,mtg-a,mtg-b" {
		t.Fatalf("pending fact must be re-read after the listing: %v", ids)
	}
}

func TestWebexHelpers(t *testing.T) {
	h := http.Header{}
	h.Add("Link", `<https://webexapis.com/v1/recordings?cursor=abc>; rel="next"`)
	if got := webexNextLink(h); got != "https://webexapis.com/v1/recordings?cursor=abc" {
		t.Errorf("next link = %q", got)
	}
	h = http.Header{}
	h.Add("Link", `<https://webexapis.com/v1/x>; rel="prev"`)
	if got := webexNextLink(h); got != "" {
		t.Errorf("prev only = %q", got)
	}
	for body, want := range map[string]string{
		`{"message":"Invalid scope\nmore"}`:       "Invalid scope",
		`{"error":"invalid_client"}`:              "invalid_client",
		`{"error_description":"Refresh expired"}`: "Refresh expired",
		`not json`: "not json",
	} {
		if got := webexErrorText(body); got != want {
			t.Errorf("webexErrorText(%q) = %q, want %q", body, got, want)
		}
	}
	if err := webexTrustedURL("https://evil.example/v1"); err == nil {
		t.Error("foreign host rejected")
	}
	if err := webexTrustedURL("::"); err == nil {
		t.Error("invalid link rejected")
	}
	err := webexAuthError(&StatusError{StatusCode: http.StatusForbidden, Body: `{"message":"missing scope"}`})
	if err == nil || !strings.Contains(err.Error(), "missing scope") || errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("403 = %v", err)
	}
	if webexAuthError(nil) != nil {
		t.Error("nil stays nil")
	}
	if err := webexAuthError(errors.New("boom")); err == nil || err.Error() != "boom" {
		t.Errorf("plain error = %v", err)
	}
	if got := renderWebexSummary(map[string]any{}); got != "" {
		t.Errorf("empty summary = %q", got)
	}
	c := &webexConnector{cfg: ConnectorConfig{Scope: map[string]string{"oauth_scopes": "meeting:recordings_read, meeting:transcripts_read"}}}
	if got := c.scopes(); got != "meeting:recordings_read meeting:transcripts_read" {
		t.Errorf("scope override = %q", got)
	}
	if got := (&webexConnector{}).redirectURI(); got != webexDefaultRedirectURI {
		t.Errorf("default redirect = %q", got)
	}
}

func TestWebexEdgeCases(t *testing.T) {
	f := &webexFixture{}
	c := newWebexTest(t, f, map[string]string{"max_recordings": "1"})
	ctx := context.Background()

	// No pending sign-in, then an expired one.
	if _, err := c.CompleteAuth(ctx, AuthCompletion{RedirectURL: webexDefaultRedirectURI + "?code=the-code"}); err == nil || !strings.Contains(err.Error(), "no sign-in is pending") {
		t.Fatalf("no pending sign-in: %v", err)
	}
	if _, err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.pendingAt = c.now().Add(-2 * webexLoginWindow)
	c.mu.Unlock()
	if _, err := c.CompleteAuth(ctx, AuthCompletion{RedirectURL: webexDefaultRedirectURI + "?code=the-code"}); err == nil || !strings.Contains(err.Error(), "no sign-in is pending") {
		t.Fatalf("expired sign-in: %v", err)
	}
	// A rejected code surfaces the token endpoint's reason.
	if _, err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompleteAuth(ctx, AuthCompletion{RedirectURL: webexDefaultRedirectURI + "?code=wrong"}); err == nil || !strings.Contains(err.Error(), "bad code") {
		t.Fatalf("rejected code: %v", err)
	}
	// Connect without the client id variable fails by name.
	t.Setenv(webexTestClientIDEnv, "")
	if _, err := c.Connect(ctx); err == nil || !strings.Contains(err.Error(), webexTestClientIDEnv) {
		t.Fatalf("missing client id: %v", err)
	}
	t.Setenv(webexTestClientIDEnv, "cid")

	// The probe without any stored sign-in reports not connected.
	c.mu.Lock()
	c.pendingState = ""
	c.mu.Unlock()
	if st := c.AuthStatus(ctx, true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe without sign-in: %+v", st)
	}

	// max_recordings caps the listing and reports truncation.
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 1 || !c.Truncated() {
		t.Fatalf("truncated listing: %+v, %v, truncated=%v", recs, err, c.Truncated())
	}
	if _, err := c.MediaLink(ctx, "bad/id"); err == nil {
		t.Fatal("an invalid id must be rejected")
	}

	// Grouping ignores items without a meeting id or with a dead status.
	g := &webexMeetings{}
	g.addRecording(map[string]any{"id": "r"})
	g.addRecording(map[string]any{"id": "r", "meetingId": "m", "status": "purged"})
	g.addTranscript(map[string]any{"id": "t", "meetingId": "m", "status": "deleted"})
	g.addTranscript(map[string]any{"id": "t2"})
	if len(g.order) != 0 {
		t.Fatalf("dead items must not group: %v", g.order)
	}
	g.addTranscript(map[string]any{"id": "t3", "meetingId": "m2", "startTime": "2026-11-12T10:00:00Z"})
	g.addRecording(map[string]any{"id": "r3", "meetingId": "m2", "timeRecorded": "2026-11-12T09:59:00Z", "topic": "Later topic"})
	if wm := g.byID["m2"]; wm.Topic != "Later topic" || !wm.StartAt.Equal(time.Date(2026, 11, 12, 9, 59, 0, 0, time.UTC)) || !wm.HasMedia {
		t.Fatalf("merged meeting = %+v", wm)
	}
	if _, err := c.fetchContent(ctx, "at-1", webexMeeting{TranscriptIDs: []string{"bad/id"}}); err == nil {
		t.Fatal("an invalid transcript id must be rejected")
	}
	if got := webexNextLink(http.Header{}); got != "" {
		t.Fatalf("no Link header = %q", got)
	}
}
