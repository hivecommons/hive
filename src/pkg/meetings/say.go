package meetings

import (
	"context"
	"errors"
	"time"
)

// Speech results, pre-rendering and the goodbye line.

// SayResult is what Quorum reports about one spoken line (quorum 0.5.0+).
// Every field is zero when Quorum, or the platform under it, did not report
// it.
type SayResult struct {
	// DurationMS is the clip's real length.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// FirstAudioMS is the say request to the first audio handed to
	// playback: the wait before she is heard.
	FirstAudioMS int64 `json:"first_audio_ms,omitempty"`
	// SynthMS is the time spent synthesizing before the first audio; zero
	// when the clip came from the cache.
	SynthMS int64 `json:"synth_ms,omitempty"`
	// Cached reports that every piece of the line was already synthesized.
	Cached bool `json:"cached,omitempty"`
}

// TimedSayer is a Quorum whose say reports how the line went out. The real
// Client implements it; a Quorum without it (in-process fakes) is asked
// through Say and answers a zero SayResult.
type TimedSayer interface {
	SayTimed(ctx context.Context, nativeID, text string) (SayResult, error)
}

// Prewarmer is a Quorum that can get fixed lines ready to be spoken at once
// (say.prewarm, quorum 0.5.0+). The real Client implements it.
type Prewarmer interface {
	SayPrewarm(ctx context.Context, nativeID string, texts []string) error
}

// ReplyTriggerConsent is the trigger recorded for the consent notice.
const ReplyTriggerConsent = "consent"

const (
	// goodbyeTimeout bounds the "Leaving now." line: a leave is never held
	// longer than this by a chat that cannot confirm or a voice that hangs.
	goodbyeTimeout = 8 * time.Second
	// prewarmTimeout bounds the say.prewarm call made when a meeting with
	// voice goes live; the synthesis itself runs in Quorum's background.
	prewarmTimeout = 5 * time.Second
)

// errGoodbyeUnconfirmed: Quorum accepted the goodbye but could not confirm
// it reached the meeting before the leave.
var errGoodbyeUnconfirmed = errors.New("the platform did not confirm the line reached the meeting")

// say speaks text through Quorum and returns what it reported.
func (m *Manager) say(ctx context.Context, nativeID, text string) (SayResult, error) {
	if ts, ok := m.opts.Quorum.(TimedSayer); ok {
		return ts.SayTimed(ctx, nativeID, SpeechText(text))
	}
	return SayResult{}, m.opts.Quorum.Say(ctx, nativeID, SpeechText(text))
}

// Say speaks text in a live meeting and returns how it went out. The caller
// applies the voice policy.
func (m *Manager) Say(ctx context.Context, id, text string) (SayResult, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return SayResult{}, err
	}
	if mt.State != StateLive {
		return SayResult{}, ErrNotLive
	}
	if mt.ConsentPostedAt == nil {
		return SayResult{}, ErrConsentPending
	}
	return m.say(ctx, mt.NativeID, text)
}

// PostChatConfirmed posts text into a live meeting's chat and reports
// whether the platform confirmed the line is in the meeting. A Quorum that
// cannot confirm is trusted on a nil error. Like PostChat it refuses until
// the consent notice is posted.
func (m *Manager) PostChatConfirmed(ctx context.Context, id, text string) (bool, error) {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return false, err
	}
	if mt.State != StateLive {
		return false, ErrNotLive
	}
	if mt.ConsentPostedAt == nil {
		return false, ErrConsentPending
	}
	cq, ok := m.opts.Quorum.(ConfirmingChat)
	if !ok {
		return true, m.opts.Quorum.ChatPost(ctx, mt.NativeID, text)
	}
	return cq.ChatPostConfirmed(ctx, mt.NativeID, text)
}

// goodbye tells the meeting hive is leaving, in the channel the leave was
// asked in, and waits (at most goodbyeTimeout) until the line is known to
// be out, so the bot is never gone before its last line: a spoken request
// in a meeting with voice is answered aloud (Quorum's say settles once the
// line was spoken, and its leave waits for speech still playing); anything
// else, or a voice that fails, is answered in chat with delivery confirmed
// (on a companion meeting that is the companion window fetching the line).
// A chat that fails falls back to voice. The line is recorded in the
// meeting transcript either way.
func (m *Manager) goodbye(ctx context.Context, mt Meeting, ev Event, heardAt time.Time) {
	cctx, cancel := context.WithTimeout(ctx, goodbyeTimeout)
	defer cancel()
	spoken := ev.Type == EventSegment && mt.VoiceEnabled
	if spoken {
		start := time.Now()
		_, err := m.say(cctx, mt.NativeID, LeavingText)
		if err == nil {
			m.RecordReply(mt.ID, LeavingText, ReplyKindVoice, ReplyTriggerLeave, heardAt, time.Since(start), nil)
			return
		}
		m.log.Info("[meetings] leaving confirmation not spoken; posting it in chat", "meeting", mt.ID, "error", err)
	}
	start := time.Now()
	err := m.goodbyeChat(cctx, mt)
	if err != nil && !spoken && mt.VoiceEnabled {
		m.log.Info("[meetings] leaving confirmation not posted; speaking it", "meeting", mt.ID, "error", err)
		start = time.Now()
		if _, sayErr := m.say(cctx, mt.NativeID, LeavingText); sayErr == nil {
			m.RecordReply(mt.ID, LeavingText, ReplyKindVoice, ReplyTriggerLeave, heardAt, time.Since(start), nil)
			return
		}
	}
	if err != nil {
		m.log.Info("[meetings] leaving confirmation not delivered", "meeting", mt.ID, "error", err)
	}
	m.RecordReply(mt.ID, LeavingText, ReplyKindChat, ReplyTriggerLeave, heardAt, time.Since(start), err)
}

// goodbyeChat posts the goodbye in chat; nil means it is known to be in the
// meeting.
func (m *Manager) goodbyeChat(ctx context.Context, mt Meeting) error {
	cq, ok := m.opts.Quorum.(ConfirmingChat)
	if !ok {
		return m.opts.Quorum.ChatPost(ctx, mt.NativeID, LeavingText)
	}
	confirmed, err := cq.ChatPostConfirmed(ctx, mt.NativeID, LeavingText)
	if err != nil {
		return err
	}
	if !confirmed {
		return errGoodbyeUnconfirmed
	}
	return nil
}

// prewarmSpeech asks Quorum to get the persona's fixed spoken lines ready
// when a meeting that can speak goes live, so the introduction, the
// acknowledgements and the goodbye start without a synthesis wait. It runs
// off the event loop and never fails the meeting.
func (m *Manager) prewarmSpeech(mt Meeting) {
	pw, ok := m.opts.Quorum.(Prewarmer)
	if !ok || m.opts.FixedSpokenLines == nil || !mt.VoiceCapable || mt.NativeID == "" {
		return
	}
	lines := m.opts.FixedSpokenLines(mt)
	if len(lines) == 0 {
		return
	}
	// The consent notice goes first: it is the first thing said in a
	// meeting and, when chat cannot carry it, nothing else is said until
	// it is out (live Teams test: 10.7 s of synthesis, uncached, on top of
	// the chat wait).
	spoken := make([]string, 0, len(lines)+1)
	if t := SpeechText(m.opts.ConsentText); t != "" {
		spoken = append(spoken, t)
	}
	for _, l := range lines {
		if t := SpeechText(l); t != "" {
			spoken = append(spoken, t)
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, prewarmTimeout)
		defer cancel()
		if err := pw.SayPrewarm(ctx, mt.NativeID, spoken); err != nil {
			m.log.Info("[meetings] speech pre-rendering not started", "meeting", mt.ID, "error", err)
		}
	}()
}
