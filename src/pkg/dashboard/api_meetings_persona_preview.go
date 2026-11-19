package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Persona preset voice preview (hivecommons/hive#18710): clicking a preset
// card in Settings, Meeting attendee plays that preset's introduction in
// its own voice. GET /api/meetings/persona/preview?preset=<id> speaks the
// preset's intro through the meetings TTS service (the Kokoro service the
// attendee speaks with in a meeting) and returns the audio, so what the
// user hears is the real voice.
//
// Clips are cached in memory, keyed by the TTS service, preset, voice and
// text, so a repeated click is instant and does not call the TTS again.
// The route is bounded by a text length cap, a per-user rate limit and the
// TTS timeout. Nothing is written to disk and nothing leaves the hive
// except the call to its own TTS service.

const (
	// meetingsPreviewMaxChars caps the intro text one preview speaks.
	meetingsPreviewMaxChars = 600
	// meetingsPreviewRateLimit is how many previews one user may ask for
	// per meetingsPreviewRateWindow, cached or not.
	meetingsPreviewRateLimit = 30
	// meetingsPreviewRateWindow is the rate limit's sliding window.
	meetingsPreviewRateWindow = time.Minute
	// meetingsPreviewCacheEntries bounds the cached clips; the oldest is
	// dropped first. Each clip is at most meetingsSampleMaxBytes.
	meetingsPreviewCacheEntries = 32
	// meetingsPreviewRateUsersMax bounds the users the limiter tracks
	// before it drops the ones with no recent request.
	meetingsPreviewRateUsersMax = 1024
	// meetingsPreviewCacheHeader says whether the clip came from the cache.
	meetingsPreviewCacheHeader = "X-Hive-Preview-Cache"
	meetingsPreviewCacheHit    = "hit"
	meetingsPreviewCacheMiss   = "miss"
	// meetingsTTSAPIKeyHeader carries the TTS service token. The shared
	// Kokoro service checks this header (Quorum and the meeting bots send
	// it); the bearer form is sent as well for a plain OpenAI-compatible
	// service.
	meetingsTTSAPIKeyHeader = "X-API-Key"
	// meetingsPreviewUnknownUser keys the rate limit for a request that
	// names no user.
	meetingsPreviewUnknownUser = "(unknown)"
)

var (
	// meetingsPreviewParen is a pronoun parenthetical ("(she/her)") or the
	// empty parentheses a blank {pronouns} leaves, in ASCII or full-width
	// brackets.
	meetingsPreviewParen = regexp.MustCompile(`\s*[(（]\s*(?:\pL+(?:\s*/\s*\pL+){1,2})?\s*[)）]`)
	// meetingsPreviewPunctSpace is whitespace left before punctuation.
	meetingsPreviewPunctSpace = regexp.MustCompile(`\s+([.,;:!?。，、])`)
)

// meetingsPreviewClip is one cached clip.
type meetingsPreviewClip struct {
	audio []byte
	ctype string
	at    time.Time
}

// meetingsPreviewCall is one TTS call in flight; requests for the same
// clip wait for it instead of calling the TTS again.
type meetingsPreviewCall struct {
	done   chan struct{}
	clip   meetingsPreviewClip
	status int
	err    error
}

// meetingsPreviewState is the preview cache and rate limiter. The zero
// value is ready to use.
type meetingsPreviewState struct {
	mu    sync.Mutex
	clips map[string]meetingsPreviewClip
	calls map[string]*meetingsPreviewCall
	hits  map[string][]time.Time
	// now is the clock; nil uses time.Now.
	now func() time.Time
}

func (p *meetingsPreviewState) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// allow records one request by user, or reports how long until the user
// may ask again.
func (p *meetingsPreviewState) allow(user string) (retryAfter time.Duration, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	cutoff := now.Add(-meetingsPreviewRateWindow)
	prune := func(times []time.Time) []time.Time {
		kept := times[:0]
		for _, at := range times {
			if at.After(cutoff) {
				kept = append(kept, at)
			}
		}
		return kept
	}
	if p.hits == nil {
		p.hits = map[string][]time.Time{}
	}
	if len(p.hits) >= meetingsPreviewRateUsersMax {
		for u, times := range p.hits {
			if times = prune(times); len(times) == 0 {
				delete(p.hits, u)
			} else {
				p.hits[u] = times
			}
		}
	}
	times := prune(p.hits[user])
	if len(times) >= meetingsPreviewRateLimit {
		p.hits[user] = times
		return times[0].Add(meetingsPreviewRateWindow).Sub(now), false
	}
	p.hits[user] = append(times, now)
	return 0, true
}

// store caches a clip, dropping the oldest when the cache is full.
func (p *meetingsPreviewState) store(key string, clip meetingsPreviewClip) {
	if p.clips == nil {
		p.clips = map[string]meetingsPreviewClip{}
	}
	if _, ok := p.clips[key]; !ok && len(p.clips) >= meetingsPreviewCacheEntries {
		oldest, first := "", true
		for k, c := range p.clips {
			if first || c.at.Before(p.clips[oldest].at) {
				oldest, first = k, false
			}
		}
		delete(p.clips, oldest)
	}
	p.clips[key] = clip
}

// clip returns the cached clip for key, or speaks it once: concurrent
// requests for the same clip share one TTS call. A failed call is not
// cached. On failure status is the HTTP status to answer with.
func (p *meetingsPreviewState) clip(ctx context.Context, key string, speak func(context.Context) ([]byte, string, int, error)) (clip meetingsPreviewClip, cached bool, status int, err error) {
	p.mu.Lock()
	if c, ok := p.clips[key]; ok {
		p.mu.Unlock()
		return c, true, http.StatusOK, nil
	}
	if call, ok := p.calls[key]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			return call.clip, false, call.status, call.err
		case <-ctx.Done():
			return meetingsPreviewClip{}, false, http.StatusGatewayTimeout, fmt.Errorf("TTS service: %w", ctx.Err())
		}
	}
	call := &meetingsPreviewCall{done: make(chan struct{})}
	if p.calls == nil {
		p.calls = map[string]*meetingsPreviewCall{}
	}
	p.calls[key] = call
	p.mu.Unlock()

	// The call outlives a cancelled request (a click on another card
	// aborts this one), so the clip is still cached for the next click.
	// speak applies its own timeout.
	audio, ctype, st, speakErr := speak(context.WithoutCancel(ctx))
	p.mu.Lock()
	call.status, call.err = st, speakErr
	if speakErr == nil {
		call.clip = meetingsPreviewClip{audio: audio, ctype: ctype, at: p.clock()}
		p.store(key, call.clip)
	}
	delete(p.calls, key)
	p.mu.Unlock()
	close(call.done)
	return call.clip, false, st, speakErr
}

// meetingsPreviewKey is the cache key for one clip: the TTS service, the
// preset, its voice and the text spoken.
func meetingsPreviewKey(ttsURL, preset, voice, text string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{ttsURL, preset, voice, text}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// meetingsSpeechIntro is an intro as it should be spoken: a pronoun
// parenthetical read aloud comes out as "she slash her", so it is dropped
// along with the space it leaves before punctuation.
func meetingsSpeechIntro(text string) string {
	s := meetingsPreviewParen.ReplaceAllString(text, "")
	s = meetingsPreviewPunctSpace.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(s), " ")
}

// meetingsPreviewText is the introduction a preset's preview speaks: the
// preset's own intro (the text the persona says when asked to introduce
// itself), in the preset's language, with the preset's name and first
// leave phrase and no pronouns. It ignores the hive's saved overrides and
// HIVE_MEETINGS_PERSONA_* variables: the card previews the preset.
func meetingsPreviewText(pre config.MeetingsPersonaPreset) (string, error) {
	tmpl := pre.IntroTemplate
	if pre.Humor == config.MeetingsPersonaHumorOff && pre.PlainIntroTemplate != "" {
		tmpl = pre.PlainIntroTemplate
	}
	r := config.ResolvedMeetingsPersona{
		Enabled: true, Preset: pre.ID, Name: pre.Name, DisplayName: pre.DisplayName,
		Language: pre.Language, Voice: pre.Voice, VoiceType: pre.VoiceType, Accent: pre.Accent,
		IntroTemplate: tmpl, LeavePhrases: pre.LeavePhrases,
	}
	text := meetingsSpeechIntro(r.Intro())
	if text == "" {
		return "", fmt.Errorf("preset %q has no introduction", pre.ID)
	}
	if n := len([]rune(text)); n > meetingsPreviewMaxChars {
		return "", fmt.Errorf("the introduction is %d characters, a preview speaks at most %d", n, meetingsPreviewMaxChars)
	}
	return text, nil
}

// meetingsPreviewUser is the rate limit key: the signed-in user.
func meetingsPreviewUser(r *http.Request) string {
	if u := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Hive-User"))); u != "" {
		return u
	}
	return meetingsPreviewUnknownUser
}

// handleMeetingsPersonaPreview returns a preset's spoken introduction.
// Every failure is a JSON {error}: the dashboard shows it as "Voice preview
// is unavailable: <reason>" and never falls back to the browser's own
// speech synthesis, which would misrepresent the voice.
func (s *Server) handleMeetingsPersonaPreview(w http.ResponseWriter, r *http.Request) {
	if !s.meetingsPersonaConfigReady(w, r) {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("preset")))
	if id == "" {
		jsonError(w, "preset is required", http.StatusBadRequest)
		return
	}
	pre, ok := config.MeetingsPersonaPresets[id]
	if !ok {
		jsonError(w, "unknown persona preset", http.StatusBadRequest)
		return
	}
	preview := &s.meetingsAttendee.preview
	if wait, ok := preview.allow(meetingsPreviewUser(r)); !ok {
		secs := int(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		jsonError(w, fmt.Sprintf("too many voice previews, try again in %d seconds", secs), http.StatusTooManyRequests)
		return
	}
	if !s.deps.Config.Meetings.IsEnabled() {
		jsonError(w, "meetings are turned off on this hive (meetings.enabled)", http.StatusServiceUnavailable)
		return
	}
	tts, _, client := s.meetingsAttendeeDeps()
	if tts == nil {
		jsonError(w, ErrMeetingsTTSNotConfigured.Error(), http.StatusServiceUnavailable)
		return
	}
	cfg, err := tts(r.Context())
	if err == nil && strings.TrimSpace(cfg.URL) == "" {
		err = ErrMeetingsTTSNotConfigured
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	text, err := meetingsPreviewText(pre)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := meetingsPreviewKey(cfg.URL, pre.ID, pre.Voice, text)
	clip, cached, status, err := preview.clip(r.Context(), key, func(ctx context.Context) ([]byte, string, int, error) {
		return meetingsSpeak(ctx, client, cfg, pre.Voice, text)
	})
	if err != nil {
		jsonError(w, err.Error(), status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", clip.ctype)
	h.Set("Content-Length", strconv.Itoa(len(clip.audio)))
	// The browser keeps no copy; the hive's cache makes a repeat instant.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if cached {
		h.Set(meetingsPreviewCacheHeader, meetingsPreviewCacheHit)
	} else {
		h.Set(meetingsPreviewCacheHeader, meetingsPreviewCacheMiss)
	}
	_, _ = w.Write(clip.audio)
}
