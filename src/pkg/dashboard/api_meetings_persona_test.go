package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func personaReq(s *Server, method, path, body, role string, verified bool, hdr ...string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "hive.example.org:8443"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "octo")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func personaGet(t *testing.T, s *Server) MeetingsPersonaSettings {
	t.Helper()
	rec := personaReq(s, http.MethodGet, "/api/meetings/persona", "", "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var got MeetingsPersonaSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func personaPut(t *testing.T, s *Server, body string, want int) MeetingsPersonaSettings {
	t.Helper()
	rec := personaReq(s, http.MethodPut, "/api/meetings/persona", body, "read-write", false)
	if rec.Code != want {
		t.Fatalf("put %s = %d %s", body, rec.Code, rec.Body.String())
	}
	var got MeetingsPersonaSettings
	if want == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func TestMeetingsPersonaAuth(t *testing.T) {
	s, deps := apiServer(t)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/meetings/persona"},
		{http.MethodPut, "/api/meetings/persona"},
		{http.MethodPost, "/api/meetings/persona/sample"},
	}
	for _, c := range cases {
		if rec := personaReq(s, c.method, c.path, "{}", "read", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as read = %d", c.method, c.path, rec.Code)
		}
		if rec := personaReq(s, c.method, c.path, "{}", "owner", false); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as unverified owner = %d", c.method, c.path, rec.Code)
		}
	}
	if rec := personaReq(s, http.MethodGet, "/api/meetings/persona", "", "read-write", false); rec.Code != http.StatusOK {
		t.Fatalf("read-write get = %d", rec.Code)
	}
	deps.Config = nil
	for _, c := range cases {
		if rec := personaReq(s, c.method, c.path, "{}", "owner", true); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without config = %d", c.method, c.path, rec.Code)
		}
	}
}

func TestMeetingsPersonaGetDefaults(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"alice", "bob:read-write", "carol:read", "dave:merger", ""}
	deps.Config.Dashboard.AuthorizedUserNames = map[string]string{"bob": " Bob B "}
	got := personaGet(t, s)
	e := got.Effective
	if !e.Enabled || e.Preset != "betty" || e.Name != "Betty" || e.Voice != "af_heart" || e.Language != "en-US" ||
		!strings.Contains(e.Intro, "Betty Bee from the hive") || e.Unprompted.AnswerWaitS != config.DefaultMeetingsPersonaAnswerWaitS {
		t.Fatalf("effective %+v", e)
	}
	if len(got.Presets) != len(config.MeetingsPersonaPresets) || got.Presets[0].ID != "betty" {
		t.Fatalf("presets %d, first %q", len(got.Presets), got.Presets[0].ID)
	}
	if len(got.Overridden) != 0 || got.RestartRequired || got.Running || got.TTSConfigured {
		t.Fatalf("flags %+v", got)
	}
	if len(got.Voices) == 0 || len(got.VoiceTypes) != 3 {
		t.Fatalf("voices %d, types %d", len(got.Voices), len(got.VoiceTypes))
	}
	em := got.Email
	if em.Available || em.Address != "hive@hive.example.org" || len(em.Approvals) != 0 {
		t.Fatalf("email %+v", em)
	}
	roles := map[string]string{}
	for _, in := range em.Inviters {
		roles[in.Login] = in.Role
		if in.Emails == nil {
			t.Fatalf("%s emails must be [] not null", in.Login)
		}
	}
	if len(roles) != 4 || roles["alice"] != "owner" || roles["bob"] != "read-write" || roles["carol"] != "read" || roles["dave"] != "merger" {
		t.Fatalf("inviters %+v", em.Inviters)
	}
	if em.Inviters[1].DisplayName != "Bob B" {
		t.Fatalf("display name %+v", em.Inviters[1])
	}
	// Behind a proxy the forwarded host names the address.
	rec := personaReq(s, http.MethodGet, "/api/meetings/persona", "", "owner", true, "X-Forwarded-Host", "Spoke.Hivecommons.dev, inner")
	var fwd MeetingsPersonaSettings
	_ = json.Unmarshal(rec.Body.Bytes(), &fwd)
	if fwd.Email.Address != "hive@spoke.hivecommons.dev" {
		t.Fatalf("forwarded address %q", fwd.Email.Address)
	}
}

type fakeAttendeeEmail struct {
	snap MeetingsAttendeeEmailSnapshot
	err  error
}

func (f fakeAttendeeEmail) Snapshot(context.Context) (MeetingsAttendeeEmailSnapshot, error) {
	return f.snap, f.err
}

func TestMeetingsPersonaEmailProvider(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"alice", "bob:read-write"}
	s.SetMeetingsAttendeeEmail(fakeAttendeeEmail{snap: MeetingsAttendeeEmailSnapshot{
		Emails:          map[string][]MeetingsCalendarEmail{"bob": {{Address: "bob@example.com", Proof: "verified"}}},
		Approvals:       []MeetingsEmailApproval{{ID: "a1", Kind: "invite", From: "x@example.net"}},
		CalendarInvites: true, MailInFiles: true,
	}})
	em := personaGet(t, s).Email
	if !em.Available || !em.CalendarInvites || !em.MailInFiles || len(em.Approvals) != 1 || em.Error != "" {
		t.Fatalf("email %+v", em)
	}
	if em.Inviters[1].Login != "bob" || len(em.Inviters[1].Emails) != 1 || em.Inviters[1].Emails[0].Proof != "verified" || len(em.Inviters[0].Emails) != 0 {
		t.Fatalf("inviters %+v", em.Inviters)
	}
	s.SetMeetingsAttendeeEmail(fakeAttendeeEmail{err: errors.New("mail store down")})
	em = personaGet(t, s).Email
	if em.Available || em.Error != "mail store down" || len(em.Inviters) != 2 {
		t.Fatalf("email error %+v", em)
	}
	s.SetMeetingsAttendeeEmail(nil)
	if personaGet(t, s).Email.Available {
		t.Fatal("removed email provider still available")
	}
}

func TestMeetingsPersonaPut(t *testing.T) {
	s, deps := apiServer(t)
	// Picking a preset card drops field overrides and needs a restart.
	deps.Config.Meetings.Persona.Name = "Old"
	got := personaPut(t, s, `{"preset":" Melie ","clear_overrides":true}`, http.StatusOK)
	if got.Effective.Name != "Mélie" || got.Effective.Language != "fr-FR" || got.Effective.Voice != "ff_siwis" || !got.RestartRequired {
		t.Fatalf("melie %+v", got.Effective)
	}
	if len(got.Overridden) != 1 || got.Overridden["preset"] != "config" {
		t.Fatalf("overridden %v", got.Overridden)
	}
	// A value equal to the preset's is not an override.
	got = personaPut(t, s, `{"name":"Mélie","aliases":["Mélie","melie","abeille","hive"],"intro_template":`+jsonString(config.MeetingsPersonaPresets["melie"].PlainIntroTemplate)+`}`, http.StatusOK)
	if len(got.Overridden) != 1 {
		t.Fatalf("preset-equal values stored as overrides: %v", got.Overridden)
	}
	got = personaPut(t, s, `{"enabled":false,"name":" Zed ","display_name":"Zed Bee","pronouns":"they/them","language":"fr-CA","voice":"ff_siwis",
		"voice_type":"male","accent":"Québécois","humor":"off","intro_template":"Salut, {name}. {leave}",
		"aliases":["zed"," ",""],"leave_phrases":["zed leave"],
		"unprompted":{"per_10m":4,"min_gap_s":30,"per_meeting":5,"answer_wait_s":3,"min_confidence":0.9,"queue_depth":2,"stale_s":20},
		"voice_on_address":false}`, http.StatusOK)
	e := got.Effective
	if e.VoiceOnAddress || deps.Config.Meetings.VoiceOnAddress == nil || *deps.Config.Meetings.VoiceOnAddress {
		t.Fatalf("voice_on_address %+v", e)
	}
	if e.Enabled || e.Name != "Zed" || e.DisplayName != "Zed Bee" || e.VoiceType != "male" || e.Humor != "off" ||
		strings.Join(e.Aliases, ",") != "zed" || strings.Join(e.LeavePhrases, ",") != "zed leave" ||
		e.Unprompted.Per10m != 4 || e.Unprompted.MinConfidence != 0.9 || e.Unprompted.StaleS != 20 {
		t.Fatalf("overrides %+v", e)
	}
	for _, k := range []string{"enabled", "name", "display_name", "pronouns", "language", "voice_type", "accent", "humor",
		"intro_template", "aliases", "leave_phrases", "unprompted.per_10m", "unprompted.min_confidence"} {
		if got.Overridden[k] != "config" {
			t.Errorf("%s not reported overridden: %v", k, got.Overridden)
		}
	}
	if _, ok := got.Overridden["voice"]; ok {
		t.Error("voice equal to the preset reported as overridden")
	}
	p := deps.Config.Meetings.Persona
	if p.Preset != "melie" || p.Name != "Zed" || p.Voice != "" || p.Enabled == nil || *p.Enabled {
		t.Fatalf("stored %+v", p)
	}
	// Empty values clear the overrides; clear_overrides keeps the caps.
	got = personaPut(t, s, `{"name":"","aliases":[],"enabled":true}`, http.StatusOK)
	if got.Effective.Name != "Mélie" || strings.Join(got.Effective.Aliases, ",") != "mélie,melie,abeille,hive" {
		t.Fatalf("cleared %+v", got.Effective)
	}
	got = personaPut(t, s, `{"preset":"bertie","clear_overrides":true}`, http.StatusOK)
	if got.Effective.Name != "Bertie" || got.Effective.Humor != "light" || got.Effective.Unprompted.Per10m != 4 {
		t.Fatalf("bertie %+v", got.Effective)
	}
	for _, bad := range []string{`{"preset":"nobody"}`, `{"humor":"loud"}`, `{"voice_type":"robot"}`, `{"language":"not a tag"}`,
		`{"unprompted":{"min_confidence":2}}`, `{"unprompted":{"per_10m":-1}}`, `{`} {
		personaPut(t, s, bad, http.StatusBadRequest)
	}
	if deps.Config.Meetings.Persona.Preset != "bertie" {
		t.Fatal("a rejected save must not change the config")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func samplePost(s *Server, body string) *httptest.ResponseRecorder {
	return personaReq(s, http.MethodPost, "/api/meetings/persona/sample", body, "owner", true)
}

func TestMeetingsPersonaSampleNotConfigured(t *testing.T) {
	s, _ := apiServer(t)
	rec := samplePost(s, "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "TTS not configured") {
		t.Fatalf("no resolver = %d %s", rec.Code, rec.Body.String())
	}
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) { return MeetingsTTSConfig{}, nil })
	if rec := samplePost(s, `{}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "TTS not configured") {
		t.Fatalf("empty url = %d %s", rec.Code, rec.Body.String())
	}
	if personaGet(t, s).TTSConfigured {
		t.Fatal("an empty TTS URL is not configured")
	}
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) {
		return MeetingsTTSConfig{}, errors.New("tts token unreadable")
	})
	if rec := samplePost(s, `{}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "unreadable") {
		t.Fatalf("resolver error = %d %s", rec.Code, rec.Body.String())
	}
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) { return MeetingsTTSConfig{URL: "http://[::1"}, nil })
	if rec := samplePost(s, `{}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bad url = %d %s", rec.Code, rec.Body.String())
	}
	if rec := samplePost(s, `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
}

func TestMeetingsPersonaSample(t *testing.T) {
	s, _ := apiServer(t)
	var (
		mu    sync.Mutex
		seen  meetingsTTSSpeechRequest
		token string
	)
	reply := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3audio"))
	}
	setReply := func(fn func(http.ResponseWriter)) {
		mu.Lock()
		defer mu.Unlock()
		reply = fn
	}
	lastReq := func() (meetingsTTSSpeechRequest, string) {
		mu.Lock()
		defer mu.Unlock()
		return seen, token
	}
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meetingsTTSSpeechPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body meetingsTTSSpeechRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen, token = body, r.Header.Get("Authorization")
		fn := reply
		mu.Unlock()
		fn(w)
	}))
	t.Cleanup(tts.Close)
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) {
		return MeetingsTTSConfig{URL: tts.URL + "/", Token: "tts-secret"}, nil
	})
	if !personaGet(t, s).TTSConfigured {
		t.Fatal("tts_configured")
	}
	rec := samplePost(s, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "ID3audio" || rec.Header().Get("Content-Type") != "audio/mpeg" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("sample = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	last, auth := lastReq()
	if auth != "Bearer tts-secret" || last.Model != meetingsTTSModel || last.Voice != "af_heart" || last.ResponseFormat != "mp3" ||
		!strings.HasPrefix(last.Input, "Hi, I'm Betty Bee from the hive.") {
		t.Fatalf("tts request %+v auth %q", last, auth)
	}
	// The form's unsaved voice, language and name are sampled.
	rec = samplePost(s, `{"voice":"jf_alpha","language":"ja-JP","display_name":"ミツ"}`)
	last, _ = lastReq()
	if rec.Code != http.StatusOK || last.Voice != "jf_alpha" || !strings.Contains(last.Input, "ミツです") {
		t.Fatalf("japanese sample %d %+v", rec.Code, last)
	}
	// The <audio> element's GET form takes the same fields as query parameters.
	rec = personaReq(s, http.MethodGet, "/api/meetings/persona/sample?voice=bm_george&language=en-GB&display_name=Bertie", "", "read-write", false)
	last, _ = lastReq()
	if rec.Code != http.StatusOK || last.Voice != "bm_george" || !strings.HasPrefix(last.Input, "Hi, I'm Bertie.") {
		t.Fatalf("get sample %d %+v", rec.Code, last)
	}
	if rec := personaReq(s, http.MethodGet, "/api/meetings/persona/sample", "", "read", false); rec.Code != http.StatusForbidden {
		t.Fatalf("get sample as read = %d", rec.Code)
	}
	rec = samplePost(s, `{"text":"  hello   there  "}`)
	last, _ = lastReq()
	if rec.Code != http.StatusOK || last.Input != "hello there" {
		t.Fatalf("custom text %d %+v", rec.Code, last)
	}
	if rec := samplePost(s, `{"text":"`+strings.Repeat("a", meetingsSampleMaxChars+1)+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("long text = %d", rec.Code)
	}
	failures := map[string]func(http.ResponseWriter){
		"upstream error": func(w http.ResponseWriter) { http.Error(w, "voice not found", http.StatusBadRequest) },
		"empty audio":    func(w http.ResponseWriter) {},
		"oversized audio": func(w http.ResponseWriter) {
			_, _ = w.Write(make([]byte, meetingsSampleMaxBytes+1))
		},
	}
	for name, fn := range failures {
		setReply(fn)
		if rec := samplePost(s, `{}`); rec.Code != http.StatusBadGateway {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// A non-audio content type is served as mp3.
	setReply(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("raw"))
	})
	if rec := samplePost(s, `{}`); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != meetingsTTSDefaultContentType {
		t.Fatalf("octet stream = %d %v", rec.Code, rec.Header())
	}
	tts.Close()
	if rec := samplePost(s, `{}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable = %d", rec.Code)
	}
}
