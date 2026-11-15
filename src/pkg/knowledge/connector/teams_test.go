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

const (
	teamsTestTenantEnv = "HIVE_TEST_TEAMS_TENANT_ID"
	teamsTestClientEnv = "HIVE_TEST_TEAMS_CLIENT_ID"
	teamsTestSecretEnv = "HIVE_TEST_TEAMS_CLIENT_SECRET"
	teamsTestUser      = "8b081ef6-4792-4def-b2c9-c363a1bf41d5"
	teamsTestVTT       = "WEBVTT\n\n00:00:00.100 --> 00:00:02.000\n<v Pat Example>Good morning everyone</v>\n\n00:00:05.000 --> 00:00:07.000\n<v Sam Example>Let's start.</v>\n"
)

// teamsFixture is an in-memory Entra token endpoint + Graph subset.
type teamsFixture struct {
	mu          sync.Mutex
	tokens      int
	badSecret   bool
	noConsent   bool
	noPolicy    bool
	noDetails   bool
	noSpeakers  bool
	listedUsers []string
}

func (f *teamsFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokens++
		bad := f.badSecret
		f.mu.Unlock()
		if bad || r.Form.Get("client_id") != "app-1" || r.Form.Get("client_secret") != "s3cret" || r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != teamsGraphScope {
			http.Error(w, `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "graph-token"})
	})
	graphBase := "/v1.0/users/" + teamsTestUser + "/onlineMeetings/"
	mux.HandleFunc("/v1.0/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer graph-token" {
			http.Error(w, `{"error":{"code":"InvalidAuthenticationToken","message":"Access token is empty."}}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		noConsent, noPolicy, noDetails, noSpeakers := f.noConsent, f.noPolicy, f.noDetails, f.noSpeakers
		f.mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.Contains(path, "getAllRecordings") || strings.Contains(path, "getAllTranscripts"):
			if noConsent {
				http.Error(w, `{"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges to complete the operation."}}`, http.StatusForbidden)
				return
			}
			if noPolicy {
				http.Error(w, `{"error":{"code":"Forbidden","message":"No application access policy found for this app"}}`, http.StatusForbidden)
				return
			}
			if !strings.Contains(r.URL.RawQuery, "$top=") && !strings.Contains(r.URL.RawQuery, "%24top=") {
				http.Error(w, `{"error":{"code":"BadRequest","message":"missing $top"}}`, http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.listedUsers = append(f.listedUsers, strings.SplitN(strings.TrimPrefix(path, "/v1.0/users/"), "/", 2)[0])
			f.mu.Unlock()
			if strings.Contains(path, "getAllRecordings") {
				if r.URL.Query().Get("skipToken") == "" {
					writeJSON(w, map[string]any{
						"@odata.nextLink": "http://" + r.Host + path + "?$top=50&skipToken=page2",
						"value": []any{map[string]any{
							"id": "rec-1", "meetingId": "mtg-1", "createdDateTime": "2026-11-12T15:00:00Z", "endDateTime": "2026-11-12T15:30:00Z",
							"recordingContentUrl": "http://" + r.Host + graphBase + "mtg-1/recordings/rec-1/content",
							"meetingOrganizer":    map[string]any{"user": map[string]any{"id": teamsTestUser, "displayName": "Andy Organizer"}},
						}},
					})
					return
				}
				writeJSON(w, map[string]any{"value": []any{}})
				return
			}
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "tr-1", "meetingId": "mtg-1", "createdDateTime": "2026-11-12T15:00:30Z", "transcriptContentUrl": "http://" + r.Host + graphBase + "mtg-1/transcripts/tr-1/content"},
				map[string]any{"id": "tr-2", "meetingId": "mtg-2", "createdDateTime": "2026-11-13T09:00:00Z", "transcriptContentUrl": "http://" + r.Host + graphBase + "mtg-2/transcripts/tr-2/content"},
			}})
		case strings.HasSuffix(path, "/transcripts/tr-1/content"), strings.HasSuffix(path, "/transcripts/tr-2/content"):
			if r.Header.Get("Accept") == teamsVTTFormat && noSpeakers {
				http.Error(w, `{"error":{"code":"Forbidden","message":"Speaker-attributed transcript content is disabled for this tenant.","innerError":{"code":"SpeakerAttributionNotAllowed"}}}`, http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", r.Header.Get("Accept"))
			if r.Header.Get("Accept") == teamsPlainTranscriptFormat {
				_, _ = w.Write([]byte("WEBVTT\n\n00:00:00.100 --> 00:00:02.000\nGood morning everyone\n"))
				return
			}
			_, _ = w.Write([]byte(teamsTestVTT))
		case path == graphBase+"mtg-1", path == graphBase+"mtg-2":
			if noDetails {
				http.Error(w, `{"error":{"code":"Forbidden","message":"Insufficient privileges"}}`, http.StatusForbidden)
				return
			}
			subject, start, end := "Design review", "2026-11-12T14:58:00Z", "2026-11-12T15:30:00Z"
			if strings.HasSuffix(path, "mtg-2") {
				subject, start, end = "Standup", "2026-11-13T08:58:00Z", "2026-11-13T09:15:00Z"
			}
			writeJSON(w, map[string]any{
				"id": path[strings.LastIndex(path, "/")+1:], "subject": subject, "joinWebUrl": "https://teams.microsoft.com/l/meetup-join/x",
				"startDateTime": start, "endDateTime": end,
				"participants": map[string]any{
					"organizer": map[string]any{"upn": "andy@contoso.example", "identity": map[string]any{"user": map[string]any{"displayName": "Andy Organizer"}}},
					"attendees": []any{map[string]any{"upn": "pat@contoso.example", "identity": map[string]any{"user": map[string]any{"displayName": "Pat Example"}}}, map[string]any{"upn": "sam@contoso.example"}},
				},
			})
		case path == "/v1.0/users/someone@contoso.example":
			writeJSON(w, map[string]any{"id": teamsTestUser})
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func newTeamsTest(t *testing.T, f *teamsFixture, scope map[string]string) *teamsConnector {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	oldGraph, oldLogin := teamsGraphBase, teamsLoginBase
	teamsGraphBase, teamsLoginBase = srv.URL+"/v1.0", srv.URL
	t.Cleanup(func() { teamsGraphBase, teamsLoginBase = oldGraph, oldLogin })
	t.Setenv(teamsTestTenantEnv, "tenant-1")
	t.Setenv(teamsTestClientEnv, "app-1")
	t.Setenv(teamsTestSecretEnv, "s3cret")
	if scope == nil {
		scope = map[string]string{"users": teamsTestUser}
	}
	cfg := ConnectorConfig{Name: "meetings", Type: TypeTeams, Enabled: true, Layer: "project", Scope: scope, Auth: Auth{TenantIDEnv: teamsTestTenantEnv, ClientIDEnv: teamsTestClientEnv, ClientSecretEnv: teamsTestSecretEnv}}
	c, err := DefaultRegistry().New(cfg, Deps{StateDir: t.TempDir(), HTTP: NewHTTPClient(HTTPOptions{AllowPrivate: true, MaxRetries: -1})})
	if err != nil {
		t.Fatal(err)
	}
	tc := c.(*teamsConnector)
	tc.now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	return tc
}

func TestTeamsValidate(t *testing.T) {
	reg := DefaultRegistry()
	base := func(auth Auth, scope map[string]string) error {
		_, err := reg.New(ConnectorConfig{Name: "t", Type: TypeTeams, Layer: "project", Scope: scope, Auth: auth}, Deps{})
		return err
	}
	ok := Auth{TenantIDEnv: "T", ClientIDEnv: "C", ClientSecretEnv: "S"}
	if err := base(ok, map[string]string{"users": teamsTestUser}); err != nil {
		t.Fatalf("minimal config must validate: %v", err)
	}
	if err := base(ok, nil); err == nil || !strings.Contains(err.Error(), "scope.users") {
		t.Fatalf("users or group required: %v", err)
	}
	if err := base(ok, map[string]string{"group": "not-a-guid"}); err == nil || !strings.Contains(err.Error(), "scope.group") {
		t.Fatalf("bad group id: %v", err)
	}
	if err := base(Auth{ClientIDEnv: "C", ClientSecretEnv: "S"}, map[string]string{"users": "x"}); err == nil || !strings.Contains(err.Error(), "tenant_id_env") {
		t.Fatalf("tenant required: %v", err)
	}
	if err := base(Auth{Env: "TOKEN", TenantIDEnv: "T", ClientIDEnv: "C", ClientSecretEnv: "S"}, map[string]string{"users": "x"}); err == nil || !strings.Contains(err.Error(), "auth.env/auth.file are not used") {
		t.Fatalf("auth.env rejected: %v", err)
	}
	if err := base(Auth{Mode: AuthModeUserOAuth, TenantIDEnv: "T", ClientIDEnv: "C", ClientSecretEnv: "S"}, map[string]string{"users": "x"}); err == nil || !strings.Contains(err.Error(), "auth.mode") {
		t.Fatalf("user_oauth is not a teams mode: %v", err)
	}
}

func TestTeamsSyncListsRecordingsAndTranscripts(t *testing.T) {
	f := &teamsFixture{}
	c := newTeamsTest(t, f, map[string]string{"users": teamsTestUser + ", someone@contoso.example", "include_video_link": "true"})
	ctx := context.Background()

	st := c.AuthStatus(ctx, true)
	if !st.Connected || st.Account == "" || st.AuthURL != "" {
		t.Fatalf("AuthStatus(refresh) = %+v", st)
	}

	var pages []Page
	next, err := c.Sync(ctx, "", func(p Page) error { pages = append(pages, p); return nil })
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d: %+v", len(pages), pages)
	}
	// Newest first: the transcript-only meeting (mtg-2) then mtg-1.
	p2, p1 := pages[0], pages[1]
	if p2.ID != teamsTranscriptIDPrefix+"tr-2" || p2.Title != "Standup" || p2.Attrs["teams_meeting_id"] != "mtg-2" {
		t.Fatalf("transcript-only page = %+v", p2)
	}
	if p1.ID != "rec-1" || p1.Title != "Design review" || p1.Attrs["teams_recording_id"] != "rec-1" || p1.Attrs["teams_meeting_id"] != "mtg-1" || p1.Attrs["duration_ms"] != "1800000" {
		t.Fatalf("recording page = %+v", p1)
	}
	for _, want := range []string{"[00:00] Pat Example: Good morning everyone", "[00:05] Sam Example: Let's start.", "- **Participants:** Pat Example, sam@contoso.example", "- **Host:** Andy Organizer", "- **Meeting (Teams):** https://teams.microsoft.com/l/meetup-join/x", "## Transcript"} {
		if !strings.Contains(p1.Markdown, want) {
			t.Fatalf("page lacks %q:\n%s", want, p1.Markdown)
		}
	}
	if p1.Attrs[meetingContentAttr] != plaudContentReady || p1.URL != "https://teams.microsoft.com/l/meetup-join/x" {
		t.Fatalf("attrs/url = %v %q", p1.Attrs, p1.URL)
	}
	if next != "2026-11-13T08:58:00Z" {
		t.Fatalf("cursor = %q", next)
	}
	f.mu.Lock()
	users, tokens := append([]string(nil), f.listedUsers...), f.tokens
	f.mu.Unlock()
	// The UPN resolved to the same object id, so each listing ran once for
	// it: the status probe, then recordings (two pages) and transcripts.
	if len(users) != 4 {
		t.Fatalf("listings = %v", users)
	}
	for _, u := range users {
		if u != teamsTestUser {
			t.Fatalf("listing for unexpected organizer %q (UPN not resolved to its object id)", u)
		}
	}
	if tokens != 1 {
		t.Fatalf("token requests = %d, want one cached token", tokens)
	}

	recs, err := c.ListMeetings(ctx)
	if err != nil || len(recs) != 2 || recs[0].ID != teamsTranscriptIDPrefix+"tr-2" || recs[1].Topic != "Design review" || !recs[1].HasMedia || recs[0].HasMedia {
		t.Fatalf("ListMeetings = %+v, %v", recs, err)
	}
	content, err := c.ReadMeeting(ctx, "rec-1")
	if err != nil || !strings.Contains(content.Transcript, "Pat Example") || content.Pending {
		t.Fatalf("ReadMeeting = %+v, %v", content, err)
	}
	if _, err := c.MediaLink(ctx, "rec-1"); !errors.Is(err, ErrMeetingNoMedia) {
		t.Fatalf("Teams media must report no playable link: %v", err)
	}
}

func TestTeamsSpeakerAttributionFallbackAndNoDetails(t *testing.T) {
	f := &teamsFixture{noDetails: true, noSpeakers: true}
	c := newTeamsTest(t, f, nil)
	var pages []Page
	if _, err := c.Sync(context.Background(), "", func(p Page) error { pages = append(pages, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages = %d", len(pages))
	}
	p := pages[1]
	if p.Title != "Teams meeting 2026-11-12 15:00" || !strings.Contains(p.Markdown, "[00:00] Good morning everyone") || strings.Contains(p.Markdown, "Pat Example") {
		t.Fatalf("fallback page = %+v", p)
	}
}

func TestTeamsConsentFailure(t *testing.T) {
	f := &teamsFixture{noConsent: true}
	c := newTeamsTest(t, f, nil)
	ctx := context.Background()
	st, err := c.Connect(ctx)
	if err != nil || st.Connected {
		t.Fatalf("Connect = %+v, %v", st, err)
	}
	if !strings.Contains(st.Message, "requires Entra admin consent for OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All") {
		t.Fatalf("consent message missing: %q", st.Message)
	}
	if !strings.HasPrefix(st.AuthURL, teamsLoginBase+"/tenant-1/adminconsent?client_id=app-1") {
		t.Fatalf("admin consent URL = %q", st.AuthURL)
	}
	_, err = c.Sync(ctx, "", func(Page) error { return nil })
	if !errors.Is(err, ErrTeamsConsentRequired) {
		t.Fatalf("Sync without consent: %v", err)
	}

	f.mu.Lock()
	f.noConsent, f.noPolicy = false, true
	f.mu.Unlock()
	st = c.AuthStatus(ctx, true)
	if st.Connected || !strings.Contains(st.Message, "application access policy") || !strings.Contains(st.Message, "Grant-CsApplicationAccessPolicy") {
		t.Fatalf("policy message missing: %+v", st)
	}
}

func TestTeamsTokenFailureIsNotConnected(t *testing.T) {
	f := &teamsFixture{badSecret: true}
	c := newTeamsTest(t, f, nil)
	_, err := c.Sync(context.Background(), "", func(Page) error { return nil })
	if !errors.Is(err, ErrMeetingNotConnected) || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("bad secret: %v", err)
	}
	st := c.AuthStatus(context.Background(), true)
	if st.Connected || st.Message == "" || st.AuthURL != "" {
		t.Fatalf("status after token failure = %+v", st)
	}
	st, err = c.Disconnect(context.Background())
	if err != nil || st.Connected || st.Message == "" {
		t.Fatalf("Disconnect = %+v, %v", st, err)
	}
}
