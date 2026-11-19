package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	zoomTestClientIDEnv     = "HIVE_TEST_ZOOM_CLIENT_ID"
	zoomTestClientSecretEnv = "HIVE_TEST_ZOOM_CLIENT_SECRET"
	zoomTestAccountIDEnv    = "HIVE_TEST_ZOOM_ACCOUNT_ID"
	zoomTestVTT             = "WEBVTT\n\n1\n00:00:01.500 --> 00:00:04.000\nAndy: Hello hive\n\n2\n00:01:05.000 --> 00:01:07.000\nAndy: Second line\n"
)

// zoomFixture is an in-memory Zoom API + OAuth server.
type zoomFixture struct {
	mu            sync.Mutex
	tokenGrants   []string
	refreshTokens []string
	unauthorized  bool
	recordingsFor []string
	revoked       int
}

func (f *zoomFixture) meetings() []map[string]any {
	m1 := map[string]any{
		"uuid": "uuid-1", "id": 111.0, "topic": "Weekly sync", "start_time": "2026-11-12T15:00:00Z", "duration": 30.0, "share_url": "https://zoom.us/rec/share/one", "host_email": "andy@example.com",
		"recording_files": []any{
			map[string]any{"id": "f-a", "file_type": "M4A", "recording_type": "audio_only", "status": "completed", "download_url": "https://zoom.us/rec/download/one-audio", "play_url": "https://zoom.us/rec/play/one", "recording_start": "2026-11-12T15:00:00Z", "recording_end": "2026-11-12T15:31:00Z"},
			map[string]any{"id": "f-v", "file_type": "MP4", "recording_type": "shared_screen_with_speaker_view", "status": "completed", "download_url": "https://zoom.us/rec/download/one-video", "play_url": "https://zoom.us/rec/play/one"},
			map[string]any{"id": "f-t", "file_type": "TRANSCRIPT", "recording_type": "audio_transcript", "status": "completed", "download_url": "https://zoom.us/rec/download/one-vtt"},
		},
	}
	m2 := map[string]any{
		"uuid": "/uuid//2", "id": 222.0, "topic": "", "start_time": "2026-11-11T09:00:00Z", "duration": 10.0,
		"recording_files": []any{
			map[string]any{"id": "f-b", "file_type": "MP4", "status": "completed", "download_url": "https://zoom.us/rec/download/two-video", "play_url": "https://zoom.us/rec/play/two"},
		},
	}
	return []map[string]any{m1, m2}
}

func (f *zoomFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "cid" || pass != "csecret" {
			http.Error(w, `{"reason":"Invalid client_id or client_secret"}`, http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		f.mu.Lock()
		f.tokenGrants = append(f.tokenGrants, grant)
		f.mu.Unlock()
		switch grant {
		case "authorization_code":
			if r.Form.Get("code") != "the-code" || r.Form.Get("redirect_uri") != zoomDefaultRedirectURI {
				http.Error(w, `{"reason":"Invalid authorization code"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600, "scope": "recording:read user:read"})
		case "refresh_token":
			f.mu.Lock()
			f.refreshTokens = append(f.refreshTokens, r.Form.Get("refresh_token"))
			f.mu.Unlock()
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"reason":"Invalid Token!"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600})
		case "account_credentials":
			if r.Form.Get("account_id") != "acc-1" {
				http.Error(w, `{"reason":"Invalid account"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "s2s-1", "expires_in": 3600})
		default:
			http.Error(w, `{"reason":"unsupported grant"}`, http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "success"})
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		unauthorized := f.unauthorized
		f.mu.Unlock()
		if unauthorized || !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"code":124,"message":"Invalid access token."}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/v2/users/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/users/")
		user, tail, _ := strings.Cut(rest, "/")
		switch tail {
		case "":
			writeJSON(w, map[string]any{"id": "u1", "email": "andy@example.com"})
		case "recordings":
			f.mu.Lock()
			f.recordingsFor = append(f.recordingsFor, user)
			f.mu.Unlock()
			if r.URL.Query().Get("page_size") != "300" || r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
				http.Error(w, `{"message":"bad paging"}`, http.StatusBadRequest)
				return
			}
			ms := f.meetings()
			if r.URL.Query().Get("next_page_token") == "" {
				writeJSON(w, map[string]any{"meetings": ms[:1], "next_page_token": "p2"})
				return
			}
			writeJSON(w, map[string]any{"meetings": ms[1:], "next_page_token": ""})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v2/meetings/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/meetings/")
		id, tail, _ := strings.Cut(rest, "/")
		// Go's mux decodes one level; a double-encoded UUID arrives still
		// encoded once, which is what Zoom requires for "/" and "//" UUIDs.
		if dec, err := url.PathUnescape(id); err == nil {
			id = dec
		}
		var meeting map[string]any
		for _, m := range f.meetings() {
			if m["uuid"] == id {
				meeting = m
			}
		}
		if meeting == nil {
			http.Error(w, `{"code":3001,"message":"Meeting does not exist"}`, http.StatusNotFound)
			return
		}
		switch tail {
		case "recordings":
			if r.URL.Query().Get("include_fields") == "download_access_token" {
				meeting["download_access_token"] = "dl-" + r.URL.Query().Get("ttl")
			}
			writeJSON(w, meeting)
		case "meeting_summary":
			if id != "uuid-1" {
				http.Error(w, `{"code":3001,"message":"No summary"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{
				"summary_title":    "Weekly sync",
				"summary_overview": "The team agreed to ship the connector.",
				"summary_details":  []any{map[string]any{"label": "Decisions", "summary": "Ship it on Friday."}},
				"next_steps":       []any{"Andy to open the PR"},
			})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/v2/past_meetings/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/participants") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"participants": []any{map[string]any{"name": "Andy"}, map[string]any{"name": "Pat"}, map[string]any{"name": "Andy"}}})
	})
	mux.HandleFunc("/rec/download/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "one-vtt") {
			w.Header().Set("Content-Type", "text/vtt")
			_, _ = w.Write([]byte(zoomTestVTT))
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// newZoomTest starts the fixture, points the connector at it and returns the
// connector plus the fixture. Download URLs in the fixture name zoom.us, so
// they are rewritten to the test server before the request leaves.
func newZoomTest(t *testing.T, f *zoomFixture, auth Auth, scope map[string]string) *zoomConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	oldAPI, oldOAuth := zoomAPIBase, zoomOAuthBase
	zoomAPIBase, zoomOAuthBase = srv.URL+"/v2", srv.URL+"/oauth"
	t.Cleanup(func() { zoomAPIBase, zoomOAuthBase = oldAPI, oldOAuth })
	t.Setenv(zoomTestClientIDEnv, "cid")
	t.Setenv(zoomTestClientSecretEnv, "csecret")
	t.Setenv(zoomTestAccountIDEnv, "acc-1")
	if auth.ClientIDEnv == "" {
		auth.ClientIDEnv, auth.ClientSecretEnv = zoomTestClientIDEnv, zoomTestClientSecretEnv
	}
	cfg := ConnectorConfig{Name: "calls", Type: TypeZoom, Enabled: true, Layer: "personal", Scope: scope, Auth: auth}
	// Rewrite zoom.us links to the fixture so downloads stay in-process.
	transport := rewriteHostTransport{host: strings.TrimPrefix(srv.URL, "http://")}
	deps := Deps{StateDir: t.TempDir(), HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1, Transport: transport})}
	c, err := DefaultRegistry().New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	zc := c.(*zoomConnector)
	zc.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	return zc
}

// rewriteHostTransport sends requests for zoom.us hosts to the fixture.
type rewriteHostTransport struct{ host string }

func (rt rewriteHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Host, "zoom.us") {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = "http", rt.host
		r = clone
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestZoomValidate(t *testing.T) {
	reg := DefaultRegistry()
	base := func(auth Auth, scope map[string]string) error {
		_, err := reg.New(ConnectorConfig{Name: "z", Type: TypeZoom, Layer: "personal", Scope: scope, Auth: auth}, Deps{})
		return err
	}
	ok := Auth{ClientIDEnv: "ZOOM_ID", ClientSecretEnv: "ZOOM_SECRET"}
	if err := base(ok, nil); err != nil {
		t.Fatalf("minimal user_oauth config must validate: %v", err)
	}
	if err := base(Auth{ClientIDEnv: "ZOOM_ID"}, nil); err == nil || !strings.Contains(err.Error(), "client_secret_env") {
		t.Fatalf("missing secret env: %v", err)
	}
	if err := base(Auth{Env: "TOKEN", ClientIDEnv: "A", ClientSecretEnv: "B"}, nil); err == nil || !strings.Contains(err.Error(), "auth.env/auth.file are not used") {
		t.Fatalf("auth.env must be rejected: %v", err)
	}
	if err := base(Auth{Mode: "device", ClientIDEnv: "A", ClientSecretEnv: "B"}, nil); err == nil || !strings.Contains(err.Error(), "auth.mode") {
		t.Fatalf("bad mode: %v", err)
	}
	s2s := Auth{Mode: AuthModeServerToServer, ClientIDEnv: "A", ClientSecretEnv: "B"}
	if err := base(s2s, map[string]string{"users": "alice@example.com"}); err == nil || !strings.Contains(err.Error(), "account_id_env") {
		t.Fatalf("s2s without account: %v", err)
	}
	s2s.AccountIDEnv = "C"
	if err := base(s2s, nil); err == nil || !strings.Contains(err.Error(), "scope.users") {
		t.Fatalf("s2s without users: %v", err)
	}
	if err := base(s2s, map[string]string{"users": "alice@example.com"}); err != nil {
		t.Fatalf("s2s config must validate: %v", err)
	}
	if err := base(ok, map[string]string{"redirect_uri": "not a url"}); err == nil || !strings.Contains(err.Error(), "redirect_uri") {
		t.Fatalf("bad redirect: %v", err)
	}
	if err := base(ok, map[string]string{"since_days": "0"}); err == nil || !strings.Contains(err.Error(), "since_days") {
		t.Fatalf("bad since_days: %v", err)
	}
}

func TestZoomConnectCompleteAndSync(t *testing.T) {
	f := &zoomFixture{}
	c := newZoomTest(t, f, Auth{}, nil)
	ctx := context.Background()

	if st := c.AuthStatus(ctx, false); st.Connected {
		t.Fatalf("fresh connector must not be connected: %+v", st)
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Pending || st.AuthURL == "" {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	u, err := url.Parse(st.AuthURL)
	if err != nil || !strings.HasPrefix(st.AuthURL, zoomOAuthBase+"/authorize?") || u.Query().Get("client_id") != "cid" || u.Query().Get("redirect_uri") != zoomDefaultRedirectURI || u.Query().Get("state") == "" {
		t.Fatalf("authorization URL = %q", st.AuthURL)
	}
	if _, err := c.CompleteAuth(ctx, AuthCompletion{RedirectURL: zoomDefaultRedirectURI + "?code=the-code&state=wrong"}); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("state mismatch must be rejected: %v", err)
	}
	st, err = c.CompleteAuth(ctx, AuthCompletion{RedirectURL: zoomDefaultRedirectURI + "?code=the-code&state=" + u.Query().Get("state")})
	if err != nil || !st.Connected || st.Account != "andy@example.com" || st.Pending {
		t.Fatalf("CompleteAuth = %+v, %v", st, err)
	}
	assertFileMode(t, c.store.path, meetingTokenFileMode)
	if tok, err := c.store.load(); err != nil || tok.RefreshToken != "rt-1" || tok.Account != "andy@example.com" {
		t.Fatalf("stored tokens = %+v, %v", tok, err)
	}

	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(fn func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 2 || pages[0].ID != "uuid-1" || pages[1].ID != "/uuid//2" {
		t.Fatalf("pages = %+v", pages)
	}
	if next != "2026-11-12T15:00:00Z" {
		t.Fatalf("cursor = %q", next)
	}
	p := pages[0]
	for _, want := range []string{"# Weekly sync", "[00:01] Andy: Hello hive", "[01:05] Andy: Second line", "## Summary (Zoom)", "The team agreed to ship the connector.", "### Decisions", "- Andy to open the PR", "- **Participants:** Andy, Pat", "- **Duration:** 31m0s"} {
		if !strings.Contains(p.Markdown, want) {
			t.Fatalf("page 1 lacks %q:\n%s", want, p.Markdown)
		}
	}
	if strings.Contains(p.Markdown, "rec/play") {
		t.Fatal("play link must not be stored unless include_video_link is set")
	}
	if p.Attrs["zoom_meeting_id"] != "111" || p.Attrs["zoom_recording_id"] != "uuid-1" || p.Attrs["duration_ms"] != "1860000" || p.Attrs[meetingContentAttr] != plaudContentReady || p.Attrs["participants"] != "Andy, Pat" {
		t.Fatalf("attrs = %v", p.Attrs)
	}
	p2 := pages[1]
	if p2.Title != "Zoom meeting 2026-11-11 09:00" || p2.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(p2.Markdown, MeetingPendingText) {
		t.Fatalf("summary-less, transcript-less recording must be pending: %+v", p2)
	}
	f.mu.Lock()
	users := append([]string(nil), f.recordingsFor...)
	f.mu.Unlock()
	if len(users) == 0 || users[0] != "me" {
		t.Fatalf("user-level OAuth must list /users/me/recordings: %v", users)
	}

	// MeetingReader: list + read one.
	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 2 || recs[0].ID != "uuid-1" || recs[0].Topic != "Weekly sync" || !recs[0].HasMedia {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "/uuid//2")
	if err != nil || !content.Pending {
		t.Fatalf("ReadMeeting(2) = %+v, %v", content, err)
	}

	// Media link: audio file preferred, download token appended, never stored.
	link, err := c.MediaLink(ctx, "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.URL, "https://zoom.us/rec/download/one-audio?access_token=dl-3600") || link.ExpiresAt.Sub(c.now()) != zoomMediaTokenTTL {
		t.Fatalf("media link = %+v", link)
	}
	if data, _ := os.ReadFile(c.store.path); strings.Contains(string(data), "dl-3600") {
		t.Fatal("download token must never be persisted")
	}

	st, err = c.Disconnect(ctx)
	if err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
	if _, err := c.store.load(); !os.IsNotExist(err) {
		t.Fatalf("token store must be cleared, got %v", err)
	}
	f.mu.Lock()
	revoked := f.revoked
	f.mu.Unlock()
	if revoked != 1 {
		t.Fatalf("revoke calls = %d", revoked)
	}
}

func TestZoomRefreshRotatesStoredToken(t *testing.T) {
	f := &zoomFixture{}
	c := newZoomTest(t, f, Auth{}, nil)
	expired := oauthTokens{AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: c.now().Add(-time.Minute), Account: "andy@example.com"}
	if err := c.store.save(expired); err != nil {
		t.Fatal(err)
	}
	if st := c.AuthStatus(context.Background(), false); !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("a stored sign-in must read as connected without a network call: %+v", st)
	}
	tok, err := c.accessToken(context.Background())
	if err != nil || tok.AccessToken != "at-2" {
		t.Fatalf("accessToken = %+v, %v", tok, err)
	}
	stored, err := c.store.load()
	if err != nil || stored.RefreshToken != "rt-2" || stored.Account != "andy@example.com" {
		t.Fatalf("rotated refresh token not saved: %+v, %v", stored, err)
	}
	if tok, err := c.accessToken(context.Background()); err != nil || tok.AccessToken != "at-2" {
		t.Fatalf("second call must reuse the fresh token: %+v, %v", tok, err)
	}
	f.mu.Lock()
	grants := append([]string(nil), f.tokenGrants...)
	f.mu.Unlock()
	if len(grants) != 1 || grants[0] != "refresh_token" {
		t.Fatalf("token grants = %v", grants)
	}

	// A dead refresh token reads as "not connected" with the card hint.
	_ = c.store.save(oauthTokens{AccessToken: "old", RefreshToken: "dead", ExpiresAt: c.now().Add(-time.Minute)})
	_, err = c.Sync(context.Background(), "", func(Page) error { return nil })
	if !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Connect in the Knowledge → Integrations → Zoom card") {
		t.Fatalf("dead refresh token: %v", err)
	}
}

func TestZoomUnauthorizedIsNotConnected(t *testing.T) {
	f := &zoomFixture{}
	c := newZoomTest(t, f, Auth{}, nil)
	_ = c.store.save(oauthTokens{AccessToken: "revoked-upstream", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	f.mu.Lock()
	f.unauthorized = true
	f.mu.Unlock()
	_, err := c.Sync(context.Background(), "", func(Page) error { return nil })
	if !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "not connected") || !strings.Contains(err.Error(), "Zoom card") {
		t.Fatalf("401 must read as not connected: %v", err)
	}
	if st := c.AuthStatus(context.Background(), true); st.Connected || st.Message == "" {
		t.Fatalf("refresh after 401 must report the failure: %+v", st)
	}
	if _, err := c.MediaLink(context.Background(), "uuid-1"); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("media link on 401: %v", err)
	}
}

func TestZoomServerToServer(t *testing.T) {
	f := &zoomFixture{}
	auth := Auth{Mode: AuthModeServerToServer, ClientIDEnv: zoomTestClientIDEnv, ClientSecretEnv: zoomTestClientSecretEnv, AccountIDEnv: zoomTestAccountIDEnv}
	c := newZoomTest(t, f, auth, map[string]string{"users": "alice@example.com, bob@example.com", "include_video_link": "true", "include_summary": "false", "include_participants": "false"})
	ctx := context.Background()
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected || st.Pending || st.AuthURL != "" {
		t.Fatalf("S2S Connect must sign in without a browser: %+v, %v", st, err)
	}
	var pages []Page
	if _, err := c.Sync(ctx, "", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d", len(pages))
	}
	if !strings.Contains(pages[0].Markdown, "- **Recording (Zoom):** https://zoom.us/rec/play/one") || strings.Contains(pages[0].Markdown, "## Summary") || strings.Contains(pages[0].Markdown, "Participants") {
		t.Fatalf("include_video_link/summary/participants flags not honoured:\n%s", pages[0].Markdown)
	}
	f.mu.Lock()
	grants, users := append([]string(nil), f.tokenGrants...), append([]string(nil), f.recordingsFor...)
	f.mu.Unlock()
	if len(grants) == 0 || grants[0] != "account_credentials" {
		t.Fatalf("grants = %v", grants)
	}
	if len(users) < 2 || users[0] != "alice@example.com" || users[len(users)-1] != "bob@example.com" {
		t.Fatalf("S2S must list each configured user: %v", users)
	}
	if _, err := os.Stat(c.store.path); !os.IsNotExist(err) {
		t.Fatal("S2S must never write a token store")
	}
	if _, err := c.CompleteAuth(ctx, AuthCompletion{Tokens: `{"access_token":"x"}`}); err == nil {
		t.Fatal("pasted token JSON must be rejected")
	}
}

func TestZoomSummaryOptionalAndPathEncoding(t *testing.T) {
	if got := zoomPathID("/uuid//2"); got != url.PathEscape(url.PathEscape("/uuid//2")) {
		t.Fatalf("double encoding = %q", got)
	}
	if got := zoomPathID("plain-uuid=="); got != url.PathEscape("plain-uuid==") {
		t.Fatalf("single encoding = %q", got)
	}
	if err := zoomTrustedURL("https://evil.example/rec?x=1"); err == nil {
		t.Fatal("non-Zoom download host must be rejected")
	}
	if err := zoomTrustedURL("https://us02web.zoom.us/rec/download/x"); err != nil {
		t.Fatal(err)
	}
	if renderZoomSummary([]byte(`{"summary_overview":"","summary_details":[],"next_steps":[]}`)) != "" {
		t.Fatal("empty summary must render empty")
	}
}
