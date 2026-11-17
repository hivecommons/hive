package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hivecommons/hive/internal/testutil"
)

type fakeMeetingsService struct {
	mu        sync.Mutex
	err       error
	approveEr error
	joined    []MeetingJoinRequest
	invitedBy string
	subs      map[int]func(MeetingFrame)
	next      int
	deleted   []string
	cleared   []time.Time
	deleteRes MeetingDeleted
}

func (f *fakeMeetingsService) Join(_ context.Context, req MeetingJoinRequest, by string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined, f.invitedBy = append(f.joined, req), by
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": "mtg-1", "state": "joining"}, nil
}
func (f *fakeMeetingsService) List() (any, error) { return []map[string]string{{"id": "mtg-1"}}, f.err }
func (f *fakeMeetingsService) Get(id string) (any, error) {
	return map[string]string{"id": id}, f.err
}
func (f *fakeMeetingsService) Leave(_ context.Context, id string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]string{"id": id, "state": "ended"}, nil
}
func (f *fakeMeetingsService) SetVoice(id string, on bool) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": id, "voice_enabled": on}, nil
}
func (f *fakeMeetingsService) ApproveResearch(string, string) error { return f.approveEr }
func (f *fakeMeetingsService) ApproveRequest(_ context.Context, id string, voice bool) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": id, "voice_enabled": voice, "state": "joining"}, nil
}
func (f *fakeMeetingsService) DeclineRequest(_ context.Context, id string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": id, "state": "failed"}, nil
}
func (f *fakeMeetingsService) Live(id string) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"meeting": map[string]string{"id": id}, "transcript": []any{}}, nil
}
func (f *fakeMeetingsService) Subscribe(fn func(MeetingFrame)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[int]func(MeetingFrame){}
	}
	id := f.next
	f.next++
	f.subs[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.subs, id)
	}
}
func (f *fakeMeetingsService) push(fr MeetingFrame) {
	f.mu.Lock()
	fns := make([]func(MeetingFrame), 0, len(f.subs))
	for _, fn := range f.subs {
		fns = append(fns, fn)
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn(fr)
	}
}
func (f *fakeMeetingsService) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func meetingsOwnerPost(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "octo")
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestMeetingsAPIUnavailableAndOwnerOnly(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doOwnerGet(s, "/api/meetings"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service = %d", rec.Code)
	}
	for _, path := range []string{"/api/meetings", "/api/meetings/mtg-1", "/api/meetings/settings", "/api/meetings/mtg-1/ws"} {
		if rec := doGetRole(s, path, "read-write", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s as read-write = %d, want 403", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/meetings", "/api/meetings/mtg-1/leave", "/api/meetings/mtg-1/voice", "/api/meetings/mtg-1/research/r1/approve", "/api/meetings/settings"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("X-Hive-Role", "owner") // unverified
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s unverified = %d, want 403", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/meetings/mtg-1/leave", "/api/meetings/mtg-1/voice", "/api/meetings/mtg-1/research/r1/approve", "/api/meetings", "/api/meetings/mtg-1/approve", "/api/meetings/mtg-1/decline"} {
		if rec := meetingsOwnerPost(s, path, `{"enabled":true}`); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s without service = %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/meetings/mtg-1", "/api/meetings/mtg-1/ws"} {
		if rec := doOwnerGet(s, path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s without service = %d", path, rec.Code)
		}
	}
}

func TestMeetingsAPIRoutes(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeMeetingsService{}
	s.SetMeetingsService(f)
	rec := meetingsOwnerPost(s, "/api/meetings", `{"url":"https://meet.google.com/x","voice":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mtg-1") {
		t.Fatalf("join = %d %s", rec.Code, rec.Body.String())
	}
	if f.invitedBy != "octo" || !f.joined[0].Voice {
		t.Fatalf("join request %+v by %q", f.joined, f.invitedBy)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	if rec := doOwnerGet(s, "/api/meetings"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"meetings"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doOwnerGet(s, "/api/meetings/mtg-1"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "transcript") {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/leave", ``); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ended") {
		t.Fatalf("leave = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/voice", `{"enabled":true}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"voice_enabled":true`) {
		t.Fatalf("voice = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/voice", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("voice without enabled = %d", rec.Code)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/research/r1/approve", ``); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/approve", `{"voice":true}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"voice_enabled":true`) {
		t.Fatalf("request approve = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/approve", ``); rec.Code != http.StatusOK {
		t.Fatalf("request approve without body = %d", rec.Code)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/approve", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("request approve bad body = %d", rec.Code)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/decline", ``); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "failed") {
		t.Fatalf("decline = %d %s", rec.Code, rec.Body.String())
	}

	cases := map[error]int{
		fmt.Errorf("%w: x", ErrMeetingNotFound): http.StatusNotFound,
		fmt.Errorf("%w: x", ErrMeetingInvalid):  http.StatusBadRequest,
		fmt.Errorf("%w: x", ErrMeetingConflict): http.StatusConflict,
		errors.New("quorum down"):               http.StatusBadGateway,
	}
	for err, want := range cases {
		f.err, f.approveEr = err, err
		for _, c := range []struct{ method, path string }{
			{http.MethodPost, "/api/meetings"}, {http.MethodGet, "/api/meetings"}, {http.MethodGet, "/api/meetings/mtg-1"},
			{http.MethodPost, "/api/meetings/mtg-1/leave"}, {http.MethodPost, "/api/meetings/mtg-1/voice"},
			{http.MethodPost, "/api/meetings/mtg-1/research/r1/approve"}, {http.MethodGet, "/api/meetings/mtg-1/ws"},
			{http.MethodPost, "/api/meetings/mtg-1/approve"}, {http.MethodPost, "/api/meetings/mtg-1/decline"},
		} {
			var rec *httptest.ResponseRecorder
			if c.method == http.MethodGet {
				rec = doOwnerGet(s, c.path)
			} else {
				rec = meetingsOwnerPost(s, c.path, `{"enabled":false,"url":"https://x"}`)
			}
			if rec.Code != want {
				t.Errorf("%s %s with %v = %d, want %d", c.method, c.path, err, rec.Code, want)
			}
		}
	}
	// Replacing the service unsubscribes the old one.
	before := f.subCount()
	s.SetMeetingsService(nil)
	if f.subCount() != before-1 {
		t.Fatal("old subscription not removed")
	}
}

func TestMeetingsSSEFrame(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeMeetingsService{}
	s.SetMeetingsService(f)
	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()
	f.push(MeetingFrame{MeetingID: "mtg-1", Kind: "line", Data: "secret transcript"})
	f.push(MeetingFrame{MeetingID: "mtg-1", Kind: MeetingFrameMeeting, Data: map[string]string{"state": "live"}})
	select {
	case got := <-ch:
		if !strings.HasPrefix(string(got), "event: meeting\n") || strings.Contains(string(got), "secret") || !strings.Contains(string(got), `"state":"live"`) {
			t.Fatalf("frame %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SSE frame")
	}
}

func TestMeetingsWebSocket(t *testing.T) {
	s, _ := apiServer(t)
	f := &fakeMeetingsService{}
	s.SetMeetingsService(f)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markOwnerRequest(r)
		s.mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/meetings/mtg-1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fr MeetingFrame
	if err := conn.ReadJSON(&fr); err != nil || fr.Kind != "snapshot" {
		t.Fatalf("snapshot = %+v, %v", fr, err)
	}
	testutil.Eventually(t, 5*time.Second, func() bool { return f.subCount() == 2 }, "panel subscription")
	f.push(MeetingFrame{MeetingID: "other", Kind: "line"})
	f.push(MeetingFrame{MeetingID: "mtg-1", Kind: "intent", Data: "x"})
	if err := conn.ReadJSON(&fr); err != nil || fr.Kind != "intent" {
		t.Fatalf("frame = %+v, %v", fr, err)
	}
	_ = conn.Close()
	testutil.Eventually(t, 5*time.Second, func() bool { return f.subCount() == 1 }, "panel subscription released")
	// A plain GET (no upgrade) after the checks fails the handshake.
	if rec := doOwnerGet(s, "/api/meetings/mtg-1/ws"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-upgrade = %d", rec.Code)
	}
}

func TestMeetingsSettingsTranscribeToken(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "transcribe")
	deps.Config.Meetings.TranscribeTokenFile = tokFile
	var got MeetingsSettings
	rec := doOwnerGet(s, "/api/meetings/settings")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	if got.TranscribeTokenSet {
		t.Fatal("the transcription token must default to not set")
	}
	const secret = "tx-secret-value"
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"transcribe_token":"`+secret+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("post = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("the transcription token must never be returned")
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.TranscribeTokenSet || !got.RestartRequired {
		t.Fatalf("after post %+v", got)
	}
	if fi, err := os.Stat(tokFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("transcription token file: %v, %v", fi, err)
	}
	if v, err := deps.Config.Meetings.TranscribeToken(context.Background()); err != nil || v != secret {
		t.Fatalf("stored transcription token = %q, %v", v, err)
	}
	rec = doOwnerGet(s, "/api/meetings/settings")
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("GET must never echo the transcription token")
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.TranscribeTokenSet {
		t.Fatal("GET must report the transcription token as set")
	}
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"transcribe_token":""}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.TranscribeTokenSet {
		t.Fatal("an empty transcription token clears it")
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatalf("cleared transcription token file still present: %v", err)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Config.Meetings.TranscribeTokenFile = filepath.Join(blocker, "t")
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{"transcribe_token":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable transcription token = %d", rec.Code)
	}
}

func TestMeetingsSettings(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Meetings.Vexa.APIKeyFile = filepath.Join(dir, "vexa")
	deps.Config.Meetings.Discord.TokenFile = filepath.Join(dir, "discord")
	rec := doOwnerGet(s, "/api/meetings/settings")
	var got MeetingsSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	if got.Enabled || got.VexaKeySet || got.Running || got.Effective["meetings.bot_name"] != "Betty Bee from the hive" || got.Effective["meetings.persona.name"] != "Betty" {
		t.Fatalf("defaults %+v", got)
	}
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"enabled":true,"vexa_url":"https://vexa.example/","vexa_api_key":"tenant","discord_token":"bot"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("post = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tenant") || strings.Contains(rec.Body.String(), `"bot"`) {
		t.Fatal("secrets must never be returned")
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Enabled || got.VexaURL != "https://vexa.example" || !got.VexaKeySet || !got.DiscordTokenSet || !got.RestartRequired {
		t.Fatalf("after post %+v", got)
	}
	if !deps.Config.Meetings.Enabled || deps.Config.Meetings.Vexa.URL != "https://vexa.example" {
		t.Fatalf("config not updated: %+v", deps.Config.Meetings)
	}
	rec = meetingsOwnerPost(s, "/api/meetings/settings", `{"vexa_api_key":""}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.VexaKeySet {
		t.Fatal("an empty key clears it")
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{"vexa_url":"ftp://x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", rec.Code)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Config.Meetings.Vexa.APIKeyFile = filepath.Join(blocker, "k")
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{"vexa_api_key":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable key = %d", rec.Code)
	}
	deps.Config.Meetings.Vexa.APIKeyFile = filepath.Join(dir, "vexa")
	deps.Config.Meetings.Discord.TokenFile = filepath.Join(blocker, "d")
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{"discord_token":"x"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("unwritable token = %d", rec.Code)
	}
	t.Setenv("HIVE_MEETINGS_ENABLED", "false")
	rec = doOwnerGet(s, "/api/meetings/settings")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.EnabledEnvOverride || got.Enabled {
		t.Fatalf("env override %+v", got)
	}
	deps.Config = nil
	if rec := doOwnerGet(s, "/api/meetings/settings"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no config get = %d", rec.Code)
	}
	if rec := meetingsOwnerPost(s, "/api/meetings/settings", `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no config post = %d", rec.Code)
	}
	if boolString(false) != "false" {
		t.Fatal("boolString")
	}
}
