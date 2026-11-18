package live

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// The listening tone (meetings.listening_tone, which decides whether the
// engine asks for it at all, betty.Settings.ListeningTone): a soft chime played into
// the meeting when Betty decided she was addressed and will answer. It
// rides beside the speak queue, never in it: it is asked for in the
// background, never delays an answer, never waits behind speech, and every
// failure is dropped silently.

// toneCrosstalk is how recently someone else's partial transcript counts as
// them still talking: she does not chime into the middle of their sentence.
const toneCrosstalk = 800 * time.Millisecond

// Toner is a Meetings that can play the listening tone (Manager.Tone).
type Toner interface {
	Tone(ctx context.Context, id string) error
}

// partialHeard is the last partial transcript from anyone, for the
// cross-talk check.
type partialHeard struct {
	who string
	at  time.Time
}

// notePartial records a partial spoken line.
func (s *session) notePartial(who string) {
	s.lastPartial = partialHeard{who: who, at: s.now()}
}

// toneAllowed reports whether the tone may play now for a request asked by
// asker, and why not otherwise (for the log). The meeting's
// voice (chat-only fallback plays nothing), speech already queued or
// playing, and someone else mid-sentence each stop it.
func (s *session) toneAllowed(asker string) (bool, string) {
	if off := s.toneVoiceOff(); off != "" {
		return false, off
	}
	now := s.now()
	if s.voice.backlog(now) > 0 {
		return false, "speech queued or playing"
	}
	p := s.lastPartial
	if p.who != "" && !strings.EqualFold(p.who, asker) && now.Sub(p.at) < toneCrosstalk {
		return false, "someone else is speaking"
	}
	return true, ""
}

// toneVoiceOff is voiceOff for the tone. A companion meeting has no voice
// in the call at all: the desktop companion plays the chime on the host's
// own computer, never into the call, so the meeting's voice setting does
// not apply there and only meetings.voice_on_address does.
func (s *session) toneVoiceOff() string {
	if m, err := s.r.opts.Meetings.Get(s.id); err == nil && m.Platform == meetings.PlatformCompanion {
		if !s.r.opts.Policy.VoiceOnAddress {
			return speakPolicyOff
		}
		return ""
	}
	off, _ := s.voiceOff()
	return off
}

// playTone asks for the tone in the background. The answer never waits for
// it: the call runs on its own goroutine under meetings.ToneTimeout and its
// error is only logged.
func (s *session) playTone(rep betty.Reply) {
	if ok, why := s.toneAllowed(rep.Asker); !ok {
		s.log().Debug("[meetings] listening tone not played", "reason", why)
		return
	}
	t, ok := s.r.opts.Meetings.(Toner)
	if !ok {
		return
	}
	id, log := s.id, s.log()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), meetings.ToneTimeout)
		defer cancel()
		if err := t.Tone(ctx, id); err != nil {
			log.Debug("[meetings] listening tone dropped", "error", err)
		}
	}()
}
