package connector

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	meetTestClientIDEnv     = "HIVE_TEST_MEET_CLIENT_ID"
	meetTestClientSecretEnv = "HIVE_TEST_MEET_CLIENT_SECRET"
)

// meetFixture is an in-memory Google Meet REST API + Drive + OAuth server.
type meetFixture struct {
	mu           sync.Mutex
	grants       []string
	subjects     []string
	revoked      int
	unauthorized bool
	// rateLimitOnce answers the first conferenceRecords call with 429.
	rateLimitOnce bool
	rateLimited   int
	// entriesEmpty makes the structured transcript return nothing, so the
	// Docs export is used.
	entriesEmpty bool
	publicKey    *rsa.PublicKey
	listFilters  []string
}

func meetIDToken(email string) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "none"}) + "." + enc(map[string]string{"email": email}) + ".sig"
}

func meetRecordJSON(id, start, end string) map[string]any {
	m := map[string]any{"name": "conferenceRecords/" + id, "startTime": start, "space": "spaces/space-" + id}
	if end != "" {
		m["endTime"] = end
	}
	return m
}

func (f *meetFixture) records() map[string]map[string]any {
	return map[string]map[string]any{
		"rec-a": meetRecordJSON("rec-a", "2026-11-12T15:00:00Z", "2026-11-12T15:30:00Z"),
		"rec-b": meetRecordJSON("rec-b", "2026-11-11T09:00:00Z", "2026-11-11T09:10:00Z"),
		"rec-c": meetRecordJSON("rec-c", "2026-11-13T09:00:00Z", ""),
		"rec-d": meetRecordJSON("rec-d", "2026-11-10T09:00:00Z", "2026-11-10T09:05:00Z"),
	}
}

func (f *meetFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		f.mu.Lock()
		f.grants = append(f.grants, grant)
		f.mu.Unlock()
		switch grant {
		case "authorization_code":
			if r.Form.Get("code") != "the-code" || r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" || r.Form.Get("redirect_uri") != googleMeetDefaultRedirectURI {
				http.Error(w, `{"error":"invalid_grant","error_description":"Bad Request"}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3599, "id_token": meetIDToken("andy@example.com")})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-1" {
				http.Error(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`, http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"access_token": "at-2", "expires_in": 3599})
		case googleMeetJWTBearerGrant:
			sub, err := f.verifyAssertion(r.Form.Get("assertion"))
			if err != nil {
				http.Error(w, `{"error":"unauthorized_client","error_description":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			f.mu.Lock()
			f.subjects = append(f.subjects, sub)
			f.mu.Unlock()
			writeJSON(w, map[string]any{"access_token": "sa-" + sub, "expires_in": 3600})
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		unauthorized := f.unauthorized
		f.mu.Unlock()
		if unauthorized || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/meet/v2/spaces/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/meet/v2/spaces/")
		writeJSON(w, map[string]any{"name": "spaces/" + id, "meetingCode": "abc-" + strings.TrimPrefix(id, "space-rec-") + "-xyz", "meetingUri": "https://meet.google.com/abc-xyz"})
	})
	mux.HandleFunc("/meet/v2/conferenceRecords", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		if f.rateLimitOnce && f.rateLimited == 0 {
			f.rateLimited++
			f.mu.Unlock()
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, http.StatusTooManyRequests)
			return
		}
		f.mu.Unlock()
		recs := f.records()
		if r.URL.Query().Get("pageSize") == "1" {
			writeJSON(w, map[string]any{"conferenceRecords": []any{recs["rec-a"]}})
			return
		}
		if r.URL.Query().Get("pageSize") != "100" {
			http.Error(w, "bad page size", http.StatusBadRequest)
			return
		}
		filter := r.URL.Query().Get("filter")
		f.mu.Lock()
		f.listFilters = append(f.listFilters, filter)
		f.mu.Unlock()
		since, _ := time.Parse(time.RFC3339, strings.TrimSuffix(strings.TrimPrefix(filter, `start_time>="`), `"`))
		page := func(ids ...string) []any {
			var out []any
			for _, id := range ids {
				if start, _ := time.Parse(time.RFC3339, recs[id]["startTime"].(string)); !start.Before(since) {
					out = append(out, recs[id])
				}
			}
			return out
		}
		if r.URL.Query().Get("pageToken") == "" {
			writeJSON(w, map[string]any{"conferenceRecords": page("rec-c", "rec-a"), "nextPageToken": "p2"})
			return
		}
		writeJSON(w, map[string]any{"conferenceRecords": page("rec-b", "rec-d")})
	})
	mux.HandleFunc("/meet/v2/conferenceRecords/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/meet/v2/conferenceRecords/"), "/")
		id := parts[0]
		rec, ok := f.records()[id]
		if !ok {
			http.Error(w, `{"error":{"code":404,"status":"NOT_FOUND"}}`, http.StatusNotFound)
			return
		}
		if len(parts) == 1 {
			writeJSON(w, rec)
			return
		}
		switch strings.Join(parts[1:], "/") {
		case "recordings":
			switch id {
			case "rec-a", "rec-b":
				writeJSON(w, map[string]any{"recordings": []any{map[string]any{"name": "conferenceRecords/" + id + "/recordings/r1", "state": "FILE_GENERATED", "driveDestination": map[string]any{"file": "file-rec-" + id, "exportUri": "https://drive.google.com/file/d/file-rec-" + id + "/view"}}}})
			default:
				writeJSON(w, map[string]any{})
			}
		case "transcripts":
			if id != "rec-a" {
				writeJSON(w, map[string]any{})
				return
			}
			writeJSON(w, map[string]any{"transcripts": []any{map[string]any{"name": "conferenceRecords/rec-a/transcripts/t1", "state": "FILE_GENERATED", "docsDestination": map[string]any{"document": "doc-tr-a", "exportUri": "https://docs.google.com/document/d/doc-tr-a"}}}})
		case "smartNotes":
			if id != "rec-a" {
				http.Error(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Gemini is not enabled"}}`, http.StatusForbidden)
				return
			}
			writeJSON(w, map[string]any{"smartNotes": []any{map[string]any{"name": "conferenceRecords/rec-a/smartNotes/n1", "state": "FILE_GENERATED", "docsDestination": map[string]any{"document": "doc-notes-a"}}}})
		case "participants":
			writeJSON(w, map[string]any{"participants": []any{
				map[string]any{"name": "conferenceRecords/" + id + "/participants/p1", "signedinUser": map[string]any{"user": "users/1", "displayName": "Andy"}},
				map[string]any{"name": "conferenceRecords/" + id + "/participants/p2", "anonymousUser": map[string]any{"displayName": "Pat"}},
			}})
		case "transcripts/t1/entries":
			f.mu.Lock()
			empty := f.entriesEmpty
			f.mu.Unlock()
			if empty {
				writeJSON(w, map[string]any{})
				return
			}
			if r.URL.Query().Get("pageToken") == "" {
				writeJSON(w, map[string]any{"transcriptEntries": []any{
					map[string]any{"participant": "conferenceRecords/rec-a/participants/p1", "text": "Hello hive", "startTime": "2026-11-12T15:00:05Z"},
					map[string]any{"participant": "conferenceRecords/rec-a/participants/p2", "text": " ", "startTime": "2026-11-12T15:00:06Z"},
				}, "nextPageToken": "e2"})
				return
			}
			writeJSON(w, map[string]any{"transcriptEntries": []any{
				map[string]any{"participant": "conferenceRecords/rec-a/participants/p2", "text": "Ship it", "startTime": "2026-11-12T16:01:07Z"},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		id, tail, _ := strings.Cut(rest, "/")
		if tail == "export" {
			if r.URL.Query().Get("mimeType") != "text/plain" {
				http.Error(w, "bad mime", http.StatusBadRequest)
				return
			}
			switch id {
			case "doc-notes-a":
				_, _ = w.Write([]byte("\ufeffSummary: the team agreed to ship.\n\nNext steps\n- Andy opens the PR"))
			case "doc-tr-a":
				_, _ = w.Write([]byte("Andy: Hello hive (from the doc)"))
			default:
				http.NotFound(w, r)
			}
			return
		}
		switch id {
		case "doc-notes-a":
			writeJSON(w, map[string]any{"name": "Weekly sync - 2026/11/12 10:00 EST - Notes by Gemini"})
		case "file-rec-rec-b":
			writeJSON(w, map[string]any{"name": "Design review (2026-11-11 04:00 GMT-5)"})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

// verifyAssertion checks the RS256 service-account JWT and returns its sub.
func (f *meetFixture) verifyAssertion(assertion string) (string, error) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed assertion")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.publicKey, crypto.SHA256, sum[:], sig); err != nil {
		return "", errors.New("bad signature")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["iss"] != "hive-reader@proj.iam.gserviceaccount.com" || claims["aud"] != googleTokenURL || !strings.Contains(claims["scope"].(string), "meetings.space.readonly") {
		return "", errors.New("bad claims")
	}
	sub, _ := claims["sub"].(string)
	return sub, nil
}

func newMeetTest(t *testing.T, f *meetFixture, auth Auth, scope map[string]string) *googleMeetConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	oldAPI, oldDrive, oldAuth, oldToken, oldRevoke := googleMeetAPIBase, googleDriveAPI, googleAuthURL, googleTokenURL, googleRevokeURL
	googleMeetAPIBase, googleDriveAPI = srv.URL+"/meet/v2", srv.URL+"/drive/v3"
	googleAuthURL, googleTokenURL, googleRevokeURL = srv.URL+"/auth", srv.URL+"/token", srv.URL+"/revoke"
	t.Cleanup(func() {
		googleMeetAPIBase, googleDriveAPI, googleAuthURL, googleTokenURL, googleRevokeURL = oldAPI, oldDrive, oldAuth, oldToken, oldRevoke
	})
	t.Setenv(meetTestClientIDEnv, "cid")
	t.Setenv(meetTestClientSecretEnv, "csecret")
	if auth.Mode != AuthModeServerToServer && auth.ClientIDEnv == "" {
		auth.ClientIDEnv, auth.ClientSecretEnv = meetTestClientIDEnv, meetTestClientSecretEnv
	}
	cfg := ConnectorConfig{Name: "calls", Type: TypeGoogleMeet, Enabled: true, Layer: "personal", Scope: scope, Auth: auth}
	kdir := t.TempDir()
	deps := Deps{StateDir: t.TempDir(), KnowledgeDir: kdir, HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})}
	c, err := DefaultRegistry().New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	mc := c.(*googleMeetConnector)
	mc.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	return mc
}

func TestGoogleMeetValidate(t *testing.T) {
	reg := DefaultRegistry()
	base := func(auth Auth, scope map[string]string) error {
		_, err := reg.New(ConnectorConfig{Name: "m", Type: TypeGoogleMeet, Layer: "personal", Scope: scope, Auth: auth}, Deps{})
		return err
	}
	user := Auth{ClientIDEnv: "GOOGLE_ID", ClientSecretEnv: "GOOGLE_SECRET"}
	sa := Auth{Mode: AuthModeServerToServer, File: "/secrets/sa.json"}
	cases := []struct {
		name  string
		auth  Auth
		scope map[string]string
		want  string
	}{
		{"user oauth", user, nil, ""},
		{"missing secret", Auth{ClientIDEnv: "A"}, nil, "client_secret_env"},
		{"key without s2s", Auth{Env: "SA_JSON", ClientIDEnv: "A", ClientSecretEnv: "B"}, nil, "server_to_server"},
		{"teams setting", Auth{ClientIDEnv: "A", ClientSecretEnv: "B", TenantIDEnv: "T"}, nil, "teams"},
		{"bad mode", Auth{Mode: "device", ClientIDEnv: "A", ClientSecretEnv: "B"}, nil, "auth.mode"},
		{"s2s without key", Auth{Mode: AuthModeServerToServer}, map[string]string{"users": "a@example.com"}, "service-account key"},
		{"s2s with client env", Auth{Mode: AuthModeServerToServer, Env: "SA", ClientIDEnv: "A"}, map[string]string{"users": "a@example.com"}, "user_oauth settings"},
		{"s2s without users", sa, nil, "scope.users"},
		{"s2s bad user", sa, map[string]string{"users": "alice"}, "not an email"},
		{"s2s ok", sa, map[string]string{"users": "a@example.com, b@example.com"}, ""},
		{"bad redirect", user, map[string]string{"redirect_uri": "nope"}, "redirect_uri"},
		{"bad since", user, map[string]string{"since_days": "-1"}, "since_days"},
	}
	for _, tc := range cases {
		err := base(tc.auth, tc.scope)
		if tc.want == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestGoogleMeetConnectCompleteAndSync(t *testing.T) {
	f := &meetFixture{}
	c := newMeetTest(t, f, Auth{}, nil)
	ctx := context.Background()
	if st := c.AuthStatus(ctx, false); st.Connected {
		t.Fatalf("fresh connector must not be connected: %+v", st)
	}
	st, err := c.Connect(ctx)
	if err != nil || !st.Pending || st.AuthURL == "" {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	u, _ := url.Parse(st.AuthURL)
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("access_type") != "offline" || !strings.Contains(q.Get("scope"), "drive.meet.readonly") || q.Get("redirect_uri") != googleMeetDefaultRedirectURI {
		t.Fatalf("authorization URL = %q", st.AuthURL)
	}
	if st := c.AuthStatus(ctx, true); !st.Pending {
		t.Fatalf("a started sign-in stays pending: %+v", st)
	}
	for _, bad := range []AuthCompletion{
		{Tokens: `{"access_token":"x"}`},
		{RedirectURL: "not a url"},
		{RedirectURL: googleMeetDefaultRedirectURI + "?error=access_denied"},
		{RedirectURL: googleMeetDefaultRedirectURI + "?state=x"},
		{RedirectURL: googleMeetDefaultRedirectURI + "?code=the-code&state=wrong"},
	} {
		if _, err := c.CompleteAuth(ctx, bad); err == nil {
			t.Fatalf("CompleteAuth(%+v) must fail", bad)
		}
	}
	st, err = c.CompleteAuth(ctx, AuthCompletion{RedirectURL: googleMeetDefaultRedirectURI + "?state=" + q.Get("state") + "&code=the-code&scope=x"})
	if err != nil || !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("CompleteAuth = %+v, %v", st, err)
	}
	assertFileMode(t, c.store.path, meetingTokenFileMode)

	var pages []Page
	next, err := c.Sync(withProgressReporter(ctx, func(func(*Progress)) {}), "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 2 || pages[0].ID != "rec-a" || pages[1].ID != "rec-b" {
		t.Fatalf("in-progress and artifact-less records must be skipped: %+v", pages)
	}
	if next != "2026-11-12T15:00:00Z" {
		t.Fatalf("cursor = %q", next)
	}
	f.mu.Lock()
	filter := f.listFilters[0]
	f.mu.Unlock()
	if filter != `start_time>="2026-10-15T12:00:00Z"` {
		t.Fatalf("first sync filter = %q", filter)
	}
	a := pages[0]
	for _, want := range []string{"# Weekly sync", "[00:05] Andy: Hello hive", "[1:01:07] Pat: Ship it", "## Summary (Google Meet)", "the team agreed to ship", "- **Participants:** Andy, Pat", "- **Duration:** 30m0s", "- **Recording page:** https://drive.google.com/file/d/file-rec-rec-a/view"} {
		if !strings.Contains(a.Markdown, want) {
			t.Fatalf("page a lacks %q:\n%s", want, a.Markdown)
		}
	}
	if strings.Contains(a.Markdown, "\ufeff") {
		t.Fatal("the export BOM must be stripped")
	}
	if a.Attrs["google_meet_recording_id"] != "rec-a" || a.Attrs["google_meet_meeting_id"] != "abc-a-xyz" || a.Attrs[meetingContentAttr] != plaudContentReady {
		t.Fatalf("attrs = %v", a.Attrs)
	}
	b := pages[1]
	if b.Title != "Design review" || b.Attrs[meetingContentAttr] != plaudContentPending || !strings.Contains(b.Markdown, MeetingPendingText) {
		t.Fatalf("recording without transcript must be pending: %+v", b)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 2 || recs[0].Topic != "Weekly sync" || !recs[0].HasMedia {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "rec-a")
	if err != nil || content.Pending || !strings.Contains(content.Transcript, "Hello hive") {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.ReadMeeting(ctx, "../etc"); err == nil {
		t.Fatal("an id outside the record id alphabet must be rejected")
	}
	if _, err := c.ReadMeeting(ctx, "rec-zzz"); err == nil || !strings.Contains(err.Error(), "rec-zzz") {
		t.Fatalf("unknown record: %v", err)
	}
	_, err = c.MediaLink(ctx, "rec-a")
	if !errors.Is(err, ErrMeetingNoMedia) || !strings.Contains(err.Error(), "https://drive.google.com/file/d/file-rec-rec-a/view") {
		t.Fatalf("Meet recordings are bearer-only, the error names the Drive page: %v", err)
	}
	if _, err := c.MediaLink(ctx, "rec-d"); !errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("no recording: %v", err)
	}

	// Expired access token: the refresh keeps the original refresh token.
	_ = c.store.save(oauthTokens{AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: c.now().Add(-time.Minute), Account: "andy@example.com"})
	tok, err := c.accessToken(ctx, googleMeetDefaultUser)
	if err != nil || tok.AccessToken != "at-2" {
		t.Fatalf("refresh = %+v, %v", tok, err)
	}
	if stored, _ := c.store.load(); stored.RefreshToken != "rt-1" || stored.Account != "andy@example.com" {
		t.Fatalf("refresh must keep the refresh token and account: %+v", stored)
	}
	_ = c.store.save(oauthTokens{AccessToken: "old", RefreshToken: "dead", ExpiresAt: c.now().Add(-time.Minute)})
	if _, err := c.Sync(ctx, "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Google Meet card") {
		t.Fatalf("dead refresh token: %v", err)
	}
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})

	st, err = c.Disconnect(ctx)
	if err != nil || st.Connected {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
	if _, err := c.store.load(); !os.IsNotExist(err) {
		t.Fatalf("token store must be cleared: %v", err)
	}
	f.mu.Lock()
	revoked := f.revoked
	f.mu.Unlock()
	if revoked != 1 {
		t.Fatalf("revoke calls = %d", revoked)
	}
	if _, err := c.ListMeetings(ctx); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("listing after sign-out: %v", err)
	}
}

func TestGoogleMeetTranscriptDocFallbackAndFlags(t *testing.T) {
	f := &meetFixture{entriesEmpty: true}
	c := newMeetTest(t, f, Auth{}, map[string]string{"include_summary": "false", "include_participants": "false", "include_video_link": "true", "tags": "meetings"})
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	var pages []Page
	if _, err := c.Sync(context.Background(), "2026-11-12T00:00:00Z", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d", len(pages))
	}
	a := pages[0]
	if !strings.Contains(a.Markdown, "Andy: Hello hive (from the doc)") || strings.Contains(a.Markdown, "## Summary") || strings.Contains(a.Markdown, "Participants") {
		t.Fatalf("doc fallback / flags not honoured:\n%s", a.Markdown)
	}
	if !strings.Contains(a.Markdown, "- **Recording (Google Meet):** https://drive.google.com/file/d/file-rec-rec-a/view") || a.Attrs["tags"] != "meetings" {
		t.Fatalf("video link / tags missing:\n%s %v", a.Markdown, a.Attrs)
	}
	// Without the notes file name the title falls back to the meeting code.
	if a.Title != "Google Meet abc-a-xyz" {
		t.Fatalf("title = %q", a.Title)
	}
	f.mu.Lock()
	filter := f.listFilters[0]
	f.mu.Unlock()
	if filter != `start_time>="2026-11-10T00:00:00Z"` {
		t.Fatalf("cursor minus overlap = %q", filter)
	}
}

func TestGoogleMeetUnauthorizedAndRateLimit(t *testing.T) {
	f := &meetFixture{unauthorized: true}
	c := newMeetTest(t, f, Auth{}, nil)
	_ = c.store.save(oauthTokens{AccessToken: "revoked", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	if _, err := c.Sync(context.Background(), "", func(Page) error { return nil }); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "Google Meet card") {
		t.Fatalf("401 must read as not connected: %v", err)
	}
	if st := c.AuthStatus(context.Background(), true); st.Connected || !strings.Contains(st.Message, "not connected") {
		t.Fatalf("probe after 401: %+v", st)
	}
	if _, err := c.MediaLink(context.Background(), "rec-a"); !errors.Is(err, ErrMeetingNotConnected) {
		t.Fatalf("media link on 401: %v", err)
	}

	rl := &meetFixture{rateLimitOnce: true}
	c2 := newMeetTest(t, rl, Auth{}, nil)
	c2.http = NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: 1})
	var waited time.Duration
	c2.http.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	_ = c2.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c2.now().Add(time.Hour)})
	recs, err := c2.ListMeetings(context.Background())
	if err != nil || len(recs) != 2 {
		t.Fatalf("listing after a 429: %+v, %v", recs, err)
	}
	if waited != 7*time.Second {
		t.Fatalf("Retry-After must be honoured, waited %v", waited)
	}
}

func TestGoogleMeetServiceAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	saJSON, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "hive-reader@proj.iam.gserviceaccount.com", "private_key_id": "kid-1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	keyFile := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(keyFile, saJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &meetFixture{publicKey: &key.PublicKey}
	c := newMeetTest(t, f, Auth{Mode: AuthModeServerToServer, File: keyFile}, map[string]string{"users": "alice@example.com, bob@example.com"})
	ctx := context.Background()
	st, err := c.Connect(ctx)
	if err != nil || !st.Connected || st.Account != "2 users (domain-wide delegation)" {
		t.Fatalf("service-account Connect = %+v, %v", st, err)
	}
	var pages []Page
	if _, err := c.Sync(ctx, "", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || pages[0].Attrs["host"] != "alice@example.com" {
		t.Fatalf("records seen by both users are read once, as the first: %+v", pages)
	}
	f.mu.Lock()
	subjects := append([]string(nil), f.subjects...)
	f.mu.Unlock()
	if len(subjects) != 2 || subjects[0] != "alice@example.com" || subjects[1] != "bob@example.com" {
		t.Fatalf("one assertion per impersonated user: %v", subjects)
	}
	if _, err := c.CompleteAuth(ctx, AuthCompletion{RedirectURL: "http://localhost/?code=x"}); err == nil {
		t.Fatal("a service account has no paste flow")
	}
	if _, err := os.Stat(c.store.path); !os.IsNotExist(err) {
		t.Fatal("a service account never writes the token store")
	}
	if _, err := c.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}

	// A key that is not a service account, or not JSON, is refused by name.
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"type":"authorized_user"}`), 0o600)
	c.cfg.Auth.File = bad
	if _, err := c.accessToken(ctx, "carol@example.com"); err == nil || !strings.Contains(err.Error(), "not a service-account key") {
		t.Fatalf("wrong key type: %v", err)
	}
	_ = os.WriteFile(bad, []byte(`nope`), 0o600)
	if _, err := c.accessToken(ctx, "carol@example.com"); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("non-JSON key: %v", err)
	}
	// A rejected assertion reads as not connected with the delegation hint.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.mu.Lock()
	f.publicKey = &other.PublicKey
	f.mu.Unlock()
	c.cfg.Auth.File = keyFile
	if _, err := c.accessToken(ctx, "carol@example.com"); !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "domain-wide delegation") {
		t.Fatalf("rejected assertion: %v", err)
	}
}

func TestGoogleMeetPendingRecordsAreReRead(t *testing.T) {
	f := &meetFixture{}
	c := newMeetTest(t, f, Auth{}, nil)
	_ = c.store.save(oauthTokens{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: c.now().Add(time.Hour)})
	if err := os.MkdirAll(c.vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fact := "---\ntitle: Old\nsource: google-meet\nconnector: calls\nsource_id: rec-b\nattr_meeting_content: pending\nattr_recorded_at: 2026-11-11T09:00:00Z\n---\n\n# Old\n"
	if err := os.WriteFile(filepath.Join(c.vaultDir, slugPrefix(TypeGoogleMeet, "calls")+"rec-b.md"), []byte(fact), 0o600); err != nil {
		t.Fatal(err)
	}
	var ids []string
	// The cursor is past rec-b, so only the pending re-read brings it back.
	if _, err := c.Sync(context.Background(), "2026-11-14T00:00:00Z", func(p Page) error { ids = append(ids, p.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ids, ",") != "rec-a,rec-b" {
		t.Fatalf("pending fact must be re-read: %v", ids)
	}
}

func TestGoogleMeetHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"Weekly sync - 2026/11/12 10:00 EST - Notes by Gemini": "Weekly sync",
		"Design review (2026-11-11 04:00 GMT-5)":               "Design review",
		"Standup (2026-11-11 04:00 GMT-5) - Transcript":        "Standup",
		"Plain name":           "Plain name",
		"  Retro - Recording ": "Retro",
	} {
		if got := cleanMeetTitle(in); got != want {
			t.Errorf("cleanMeetTitle(%q) = %q, want %q", in, got, want)
		}
	}
	for d, want := range map[time.Duration]string{-time.Second: "00:00", 65 * time.Second: "01:05", time.Hour + 2*time.Minute + 3*time.Second: "1:02:03"} {
		if got := meetStamp(d); got != want {
			t.Errorf("meetStamp(%v) = %q, want %q", d, got, want)
		}
	}
	if got := googleIDTokenEmail(meetIDToken("x@example.com")); got != "x@example.com" {
		t.Errorf("id token email = %q", got)
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c"} {
		if got := googleIDTokenEmail(bad); got != "" {
			t.Errorf("googleIDTokenEmail(%q) = %q", bad, got)
		}
	}
	for body, want := range map[string]string{
		`{"error":{"code":403,"message":"Meet API disabled\nmore","status":"PERMISSION_DENIED"}}`: "PERMISSION_DENIED: Meet API disabled",
		`{"error":"invalid_grant","error_description":"Bad Request"}`:                             "invalid_grant: Bad Request",
		`<html>oops</html>`: "<html>oops</html>",
	} {
		if got := googleErrorText(body); got != want {
			t.Errorf("googleErrorText(%q) = %q, want %q", body, got, want)
		}
	}
	if err := googleTrustedURL("https://evil.example/x"); err == nil {
		t.Error("non-Google host must be rejected")
	}
	if err := googleTrustedURL("::"); err == nil {
		t.Error("invalid URL must be rejected")
	}
	if err := googleTrustedURL(googleDriveAPI + "/files/x"); err != nil {
		t.Errorf("Drive host: %v", err)
	}
	if n := meetParticipantName(map[string]any{"phoneUser": map[string]any{"displayName": "+1 ***-***-1234"}}); n != "+1 ***-***-1234" {
		t.Errorf("phone participant = %q", n)
	}
	if n := meetParticipantName(map[string]any{}); n != "" {
		t.Errorf("empty participant = %q", n)
	}
	err := googleMeetAuthError(&StatusError{StatusCode: http.StatusForbidden, Body: `{"error":{"status":"PERMISSION_DENIED","message":"insufficient scopes"}}`})
	if err == nil || !strings.Contains(err.Error(), "insufficient scopes") || errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("403 = %v", err)
	}
	if err := googleMeetAuthError(&StatusError{StatusCode: http.StatusInternalServerError}); err == nil || errors.Is(err, ErrMeetingNotConnected) {
		t.Errorf("500 = %v", err)
	}
	if googleMeetAuthError(nil) != nil {
		t.Error("nil stays nil")
	}
	if _, err := googleServiceAccountJWT(googleServiceAccount{PrivateKey: "not pem"}, "a", "s", "aud", time.Now()); err == nil {
		t.Error("non-PEM key must fail")
	}
	if _, err := googleServiceAccountJWT(googleServiceAccount{PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}))}, "a", "s", "aud", time.Now()); err == nil {
		t.Error("junk key must fail")
	}
	pk1, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(pk1)}))
	if jwt, err := googleServiceAccountJWT(googleServiceAccount{ClientEmail: "x@y", PrivateKey: pkcs1}, "a", "s", "aud", time.Now()); err != nil || strings.Count(jwt, ".") != 2 {
		t.Errorf("PKCS#1 key: %q, %v", jwt, err)
	}
	a := parseMeetArtifact(map[string]any{"name": "n", "state": "ENDED", "docsDestination": map[string]any{"document": "d1", "exportUri": "https://docs"}})
	if a.FileID != "d1" || a.ExportURI != "https://docs" || a.generated() {
		t.Errorf("artifact = %+v", a)
	}
}
