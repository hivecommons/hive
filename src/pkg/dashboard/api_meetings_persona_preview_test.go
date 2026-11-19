package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// previewBettyIntro is the introduction Betty's preview speaks: her real
// intro with no pronoun parenthetical.
const previewBettyIntro = `Hi, I'm Betty Bee from the hive. I take notes, keep this meeting's transcript, and answer questions from what the hive knows: its knowledge base and work items. Say my name to ask me something. Say "betty leave" and I'll buzz off.`

// previewTTS is a fake Kokoro service that records what it was asked.
type previewTTS struct {
	mu     sync.Mutex
	calls  int
	last   meetingsTTSSpeechRequest
	apiKey string
	bearer string
	// reply answers a call; nil sends audio.
	reply func(http.ResponseWriter)
	srv   *httptest.Server
}

func newPreviewTTS(t *testing.T) *previewTTS {
	t.Helper()
	f := &previewTTS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meetingsTTSSpeechPath || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body meetingsTTSSpeechRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls++
		f.last, f.apiKey, f.bearer = body, r.Header.Get(meetingsTTSAPIKeyHeader), r.Header.Get("Authorization")
		reply := f.reply
		f.mu.Unlock()
		if reply != nil {
			reply(w)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3" + body.Voice))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *previewTTS) seen() (int, meetingsTTSSpeechRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
}

func (f *previewTTS) setReply(fn func(http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = fn
}

// previewServer is a dashboard with meetings on and the fake TTS wired.
func previewServer(t *testing.T) (*Server, *Dependencies, *previewTTS) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Config.Meetings.Enabled = true
	f := newPreviewTTS(t)
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) {
		return MeetingsTTSConfig{URL: f.srv.URL, Token: "tts-secret"}, nil
	})
	return s, deps, f
}

func previewGet(s *Server, preset, user string) *httptest.ResponseRecorder {
	return personaReq(s, http.MethodGet, "/api/meetings/persona/preview?preset="+url.QueryEscape(preset), "", "read-write", false, "X-Hive-User", user)
}

// previewError decodes a JSON error reply.
func previewError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error content type %q, body %s", ct, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("not a JSON error: %v %s", err, rec.Body.String())
	}
	return body.Error
}

func TestMeetingsPersonaPreviewRoles(t *testing.T) {
	s, deps, tts := previewServer(t)
	const path = "/api/meetings/persona/preview?preset=betty"
	for _, c := range []struct {
		role     string
		verified bool
		want     int
	}{
		{"", false, http.StatusForbidden},
		{"read", false, http.StatusForbidden},
		{"owner", false, http.StatusForbidden}, // an unverified owner header is not trusted
		{"read-write", false, http.StatusOK},
		{"owner", true, http.StatusOK},
	} {
		rec := personaReq(s, http.MethodGet, path, "", c.role, c.verified)
		if rec.Code != c.want {
			t.Errorf("role %q verified=%v = %d, want %d (%s)", c.role, c.verified, rec.Code, c.want, rec.Body.String())
		}
		if c.want != http.StatusOK {
			previewError(t, rec)
		}
	}
	if calls, _ := tts.seen(); calls != 1 {
		t.Errorf("TTS calls = %d, want 1 (refused roles never reach the TTS)", calls)
	}
	deps.Config = nil
	if rec := personaReq(s, http.MethodGet, path, "", "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without config = %d", rec.Code)
	}
}

func TestMeetingsPersonaPreviewSpeaksTheIntroAndCaches(t *testing.T) {
	s, _, tts := previewServer(t)
	rec := previewGet(s, "betty", "octo")
	if rec.Code != http.StatusOK || rec.Body.String() != "ID3af_heart" {
		t.Fatalf("preview = %d %q", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "audio/mpeg" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get(meetingsPreviewCacheHeader) != meetingsPreviewCacheMiss || h.Get("Content-Length") != strconv.Itoa(len("ID3af_heart")) {
		t.Fatalf("headers %v", h)
	}
	calls, last := tts.seen()
	if calls != 1 || last.Voice != "af_heart" || last.Model != meetingsTTSModel || last.ResponseFormat != meetingsTTSFormat || last.Input != previewBettyIntro {
		t.Fatalf("tts call %d %+v", calls, last)
	}
	if tts.apiKey != "tts-secret" || tts.bearer != "Bearer tts-secret" {
		t.Fatalf("token headers: X-API-Key %q, Authorization %q", tts.apiKey, tts.bearer)
	}
	// A repeat, by anyone, is served from the cache: no second TTS call.
	for _, user := range []string{"octo", "mona"} {
		rec = previewGet(s, "BETTY ", user)
		if rec.Code != http.StatusOK || rec.Body.String() != "ID3af_heart" || rec.Header().Get(meetingsPreviewCacheHeader) != meetingsPreviewCacheHit {
			t.Fatalf("repeat by %s = %d %q %v", user, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if calls, _ = tts.seen(); calls != 1 {
		t.Fatalf("TTS calls after repeats = %d, want 1", calls)
	}
	// Another preset is its own clip, in its own voice and language.
	rec = previewGet(s, "mitsu", "octo")
	calls, last = tts.seen()
	mitsu := config.MeetingsPersonaPresets["mitsu"]
	if rec.Code != http.StatusOK || calls != 2 || last.Voice != mitsu.Voice || !strings.Contains(last.Input, mitsu.DisplayName) ||
		!strings.HasPrefix(last.Input, "こんにちは") || strings.ContainsAny(last.Input, "()（）") {
		t.Fatalf("mitsu = %d, calls %d, %+v", rec.Code, calls, last)
	}
	// The preview is the preset, not the hive's saved persona or env.
	t.Setenv(config.MeetingsPersonaDisplayNameEnv, "Somebody Else")
	t.Setenv(config.MeetingsPersonaPresetEnv, "bertie")
	if rec = previewGet(s, "betty", "octo"); rec.Header().Get(meetingsPreviewCacheHeader) != meetingsPreviewCacheHit {
		t.Fatalf("env overrides changed the preset preview: %v", rec.Header())
	}
}

func TestMeetingsPreviewTextForEveryPreset(t *testing.T) {
	for id, pre := range config.MeetingsPersonaPresets {
		text, err := meetingsPreviewText(pre)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if !strings.Contains(text, pre.DisplayName) || !strings.Contains(text, pre.LeavePhrases[0]) {
			t.Errorf("%s: intro lacks the name or the leave phrase: %q", id, text)
		}
		if strings.ContainsAny(text, "()（）{}") || strings.Contains(text, "she/her") || strings.Contains(text, "he/him") {
			t.Errorf("%s: a pronoun parenthetical or placeholder would be spoken: %q", id, text)
		}
		if n := len([]rune(text)); n > meetingsPreviewMaxChars {
			t.Errorf("%s: %d characters", id, n)
		}
	}
	// The plain intro is used for a preset with humor off.
	plain := config.MeetingsPersonaPresets["betty"]
	plain.Humor = config.MeetingsPersonaHumorOff
	if text, err := meetingsPreviewText(plain); err != nil || !strings.HasSuffix(text, `to remove me.`) {
		t.Errorf("plain intro = %q, %v", text, err)
	}
}

func TestMeetingsSpeechIntro(t *testing.T) {
	for in, want := range map[string]string{
		"Hi, I'm Betty Bee from the hive (she/her). I take notes.": "Hi, I'm Betty Bee from the hive. I take notes.",
		"Hi, I'm Betty (). I take notes.":                          "Hi, I'm Betty. I take notes.",
		"I am Sam (they / them / theirs) , hello":                  "I am Sam, hello",
		"こんにちは、ミツです（）。メモを取り":                                       "こんにちは、ミツです。メモを取り",
		"Bonjour, je suis Mélie (elle/elle).":                      "Bonjour, je suis Mélie.",
		"Say \"betty leave\" (or type it) and   I'll go.":          "Say \"betty leave\" (or type it) and I'll go.",
		"notes for issues/PRs stay (work items)":                   "notes for issues/PRs stay (work items)",
	} {
		if got := meetingsSpeechIntro(in); got != want {
			t.Errorf("meetingsSpeechIntro(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMeetingsPersonaPreviewLimits(t *testing.T) {
	s, _, tts := previewServer(t)
	// Text cap.
	long := config.MeetingsPersonaPreset{ID: "long", DisplayName: "L", IntroTemplate: strings.Repeat("a", meetingsPreviewMaxChars+1)}
	if _, err := meetingsPreviewText(long); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("over-long intro = %v", err)
	}
	long.IntroTemplate = strings.Repeat("a", meetingsPreviewMaxChars)
	if _, err := meetingsPreviewText(long); err != nil {
		t.Fatalf("intro at the cap = %v", err)
	}
	if _, err := meetingsPreviewText(config.MeetingsPersonaPreset{ID: "empty", IntroTemplate: " ({pronouns}) "}); err == nil {
		t.Fatal("an empty intro must be refused")
	}
	// Unknown and missing presets never reach the TTS.
	for _, preset := range []string{"", "nobody", "../betty"} {
		rec := previewGet(s, preset, "octo")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("preset %q = %d", preset, rec.Code)
		}
		previewError(t, rec)
	}
	if calls, _ := tts.seen(); calls != 0 {
		t.Fatalf("TTS calls = %d, want 0", calls)
	}
	// Per-user rate limit, cached clips included.
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	s.meetingsAttendee.preview.now = func() time.Time { return now }
	for i := 0; i < meetingsPreviewRateLimit; i++ {
		if rec := previewGet(s, "betty", "octo"); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := previewGet(s, "betty", "octo")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(previewError(t, rec), "too many voice previews") {
		t.Fatalf("over the limit = %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 || secs > int(meetingsPreviewRateWindow/time.Second)+1 {
		t.Fatalf("Retry-After %q", rec.Header().Get("Retry-After"))
	}
	// The same user in another case is the same user; someone else is not limited.
	if rec := previewGet(s, "betty", "OCTO"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("OCTO = %d", rec.Code)
	}
	if rec := previewGet(s, "betty", "mona"); rec.Code != http.StatusOK {
		t.Fatalf("another user = %d", rec.Code)
	}
	if calls, _ := tts.seen(); calls != 1 {
		t.Fatalf("TTS calls = %d, want 1", calls)
	}
	// The window slides.
	now = now.Add(meetingsPreviewRateWindow + time.Second)
	if rec := previewGet(s, "betty", "octo"); rec.Code != http.StatusOK {
		t.Fatalf("after the window = %d", rec.Code)
	}
}

func TestMeetingsPersonaPreviewUnavailable(t *testing.T) {
	s, deps, tts := previewServer(t)
	want := func(name string, rec *httptest.ResponseRecorder, code int, reason string) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s = %d %s, want %d", name, rec.Code, rec.Body.String(), code)
		}
		if msg := previewError(t, rec); !strings.Contains(msg, reason) {
			t.Fatalf("%s error %q lacks %q", name, msg, reason)
		}
	}
	// Meetings off.
	deps.Config.Meetings.Enabled = false
	want("meetings off", previewGet(s, "betty", "octo"), http.StatusServiceUnavailable, "meetings are turned off")
	deps.Config.Meetings.Enabled = true
	// No TTS service.
	s.SetMeetingsTTSResolver(nil)
	want("no resolver", previewGet(s, "betty", "octo"), http.StatusServiceUnavailable, "TTS not configured")
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) { return MeetingsTTSConfig{}, nil })
	want("empty url", previewGet(s, "betty", "octo"), http.StatusServiceUnavailable, "TTS not configured")
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) {
		return MeetingsTTSConfig{}, errors.New("the TTS service token cannot be read")
	})
	want("resolver error", previewGet(s, "betty", "octo"), http.StatusServiceUnavailable, "token cannot be read")
	// The TTS service is not reachable.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) { return MeetingsTTSConfig{URL: deadURL}, nil })
	want("unreachable", previewGet(s, "betty", "octo"), http.StatusBadGateway, "TTS service")
	// The TTS service fails, or sends nothing.
	s.SetMeetingsTTSResolver(func(context.Context) (MeetingsTTSConfig, error) { return MeetingsTTSConfig{URL: tts.srv.URL}, nil })
	tts.setReply(func(w http.ResponseWriter) { http.Error(w, "voice not found", http.StatusInternalServerError) })
	want("tts 500", previewGet(s, "betty", "octo"), http.StatusBadGateway, "voice not found")
	tts.setReply(func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) })
	want("no audio", previewGet(s, "betty", "octo"), http.StatusBadGateway, "no audio")
	// A failure is never cached: once the TTS is back the clip plays.
	before, _ := tts.seen()
	tts.setReply(nil)
	rec := previewGet(s, "betty", "octo")
	after, _ := tts.seen()
	if rec.Code != http.StatusOK || rec.Header().Get(meetingsPreviewCacheHeader) != meetingsPreviewCacheMiss || after != before+1 {
		t.Fatalf("after recovery = %d %v, calls %d -> %d", rec.Code, rec.Header(), before, after)
	}
}

func TestMeetingsPersonaPreviewTimeout(t *testing.T) {
	s, _, tts := previewServer(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tts.setReply(func(http.ResponseWriter) { <-release })
	const shortTimeout = 50 * time.Millisecond
	s.meetingsAttendee.mu.Lock()
	s.meetingsAttendee.client = &http.Client{Timeout: shortTimeout}
	s.meetingsAttendee.mu.Unlock()
	rec := previewGet(s, "betty", "octo")
	if rec.Code != http.StatusBadGateway || !strings.Contains(previewError(t, rec), "TTS service") {
		t.Fatalf("slow TTS = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMeetingsPersonaPreviewSharesOneCall(t *testing.T) {
	s, _, tts := previewServer(t)
	release := make(chan struct{})
	tts.setReply(func(w http.ResponseWriter) {
		<-release
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3shared"))
	})
	const clickers = 8
	var wg sync.WaitGroup
	codes := make([]int, clickers)
	for i := 0; i < clickers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := previewGet(s, "barnaby", "user-"+strconv.Itoa(i))
			codes[i] = rec.Code
			if rec.Body.String() != "ID3shared" {
				codes[i] = -1
			}
		}(i)
	}
	// Let the first request reach the TTS, then answer everyone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _ := tts.seen(); calls > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("clicker %d = %d", i, code)
		}
	}
	if calls, _ := tts.seen(); calls != 1 {
		t.Fatalf("TTS calls = %d, want 1 for %d simultaneous clicks", calls, clickers)
	}
}

func TestMeetingsPreviewCacheIsBounded(t *testing.T) {
	var p meetingsPreviewState
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const extra = 3
	for i := 0; i < meetingsPreviewCacheEntries+extra; i++ {
		p.store(strconv.Itoa(i), meetingsPreviewClip{audio: []byte{byte(i)}, at: base.Add(time.Duration(i) * time.Second)})
	}
	if len(p.clips) != meetingsPreviewCacheEntries {
		t.Fatalf("cache holds %d clips, want %d", len(p.clips), meetingsPreviewCacheEntries)
	}
	for i := 0; i < extra; i++ {
		if _, ok := p.clips[strconv.Itoa(i)]; ok {
			t.Errorf("oldest clip %d was kept", i)
		}
	}
	// Replacing a cached clip evicts nothing.
	p.store(strconv.Itoa(extra), meetingsPreviewClip{at: base})
	if len(p.clips) != meetingsPreviewCacheEntries {
		t.Fatalf("replace changed the size to %d", len(p.clips))
	}
	// The key separates service, preset, voice and text.
	keys := map[string]bool{}
	for _, k := range []string{
		meetingsPreviewKey("http://a", "betty", "af_heart", "hi"),
		meetingsPreviewKey("http://b", "betty", "af_heart", "hi"),
		meetingsPreviewKey("http://a", "barnaby", "af_heart", "hi"),
		meetingsPreviewKey("http://a", "betty", "am_michael", "hi"),
		meetingsPreviewKey("http://a", "betty", "af_heart", "hello"),
	} {
		keys[k] = true
	}
	if len(keys) != 5 {
		t.Fatalf("%d distinct keys, want 5", len(keys))
	}
}

func TestMeetingsPreviewRateLimiterForgetsIdleUsers(t *testing.T) {
	var p meetingsPreviewState
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	for i := 0; i < meetingsPreviewRateUsersMax; i++ {
		if _, ok := p.allow("user-" + strconv.Itoa(i)); !ok {
			t.Fatalf("user %d refused", i)
		}
	}
	now = now.Add(2 * meetingsPreviewRateWindow)
	if _, ok := p.allow("late"); !ok {
		t.Fatal("late user refused")
	}
	if len(p.hits) != 1 {
		t.Fatalf("limiter tracks %d users, want 1", len(p.hits))
	}
}
