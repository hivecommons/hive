package live

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Spoken answers when several are ready (hivecommons/hive#18590). Quorum's
// say returns once the clip is queued and plays clips in call order, so
// she never talks over herself, but hive cannot see when she stops
// talking. The session keeps an estimate of the queued speech instead and
// uses it to:
//
//   - speak answers that are ready together back to back, shortest first,
//     so the quick ones are not held behind a long one;
//   - move answers to chat once the queued speech would pass
//     voiceBacklogMax, with one spoken "I've put the rest in chat";
//   - skip an acknowledgement that would only play after seconds of
//     earlier speech, when it no longer acknowledges anything promptly.
const (
	// voiceCharsPerSecond is the speaking rate the estimate assumes, about
	// 160 words a minute.
	voiceCharsPerSecond = 15
	// voiceMinClip is the least a clip is assumed to take (synthesis and
	// the pauses around it).
	voiceMinClip = time.Second
	// voiceBacklogMax is the most queued speech an answer may wait behind;
	// later answers go to chat.
	voiceBacklogMax = 20 * time.Second
	// ackMaxBacklog is the most queued speech an acknowledgement may wait
	// behind: past it the ack would land seconds after the request, about
	// when the answer itself is due.
	ackMaxBacklog = 8 * time.Second
	// restInChatGap is the least time between two "rest in chat" notes.
	restInChatGap = 30 * time.Second
	// maxReplyDrain bounds the ready replies one turn of the session loop
	// takes together.
	maxReplyDrain = 16
)

// speechTime estimates how long text takes to say.
func speechTime(text string) time.Duration {
	d := time.Duration(utf8.RuneCountInString(text)) * time.Second / voiceCharsPerSecond
	return max(d, voiceMinClip)
}

// voiceQueue is the session's estimate of its queued speech.
type voiceQueue struct {
	busyUntil time.Time
	// restNoted is when "the rest is in chat" was last said.
	restNoted time.Time
	// pending are spoken answers ready this turn, flushed together.
	pending []voiceAnswer
}

// voiceAnswer is one spoken answer waiting for the flush. asker, topic
// and attributed come from the persona's reply (betty.Reply).
type voiceAnswer struct {
	text       string
	seq        int
	asker      string
	topic      string
	attributed bool
}

// backlog is the speech still queued at now.
func (v *voiceQueue) backlog(now time.Time) time.Duration {
	if v.busyUntil.After(now) {
		return v.busyUntil.Sub(now)
	}
	return 0
}

// queued records a clip handed to the meeting at now.
func (v *voiceQueue) queued(now time.Time, text string) {
	start := now
	if v.busyUntil.After(now) {
		start = v.busyUntil
	}
	v.busyUntil = start.Add(speechTime(text))
}

// drainPersona applies every further reply that is ready now, then speaks
// the spoken answers they produced together.
func (s *session) drainPersona(ctx context.Context, replies <-chan betty.Reply) {
	defer s.flushVoice(ctx)
	for range maxReplyDrain {
		select {
		case rep := <-replies:
			s.onPersonaReply(ctx, rep)
		default:
			return
		}
	}
}

// queueVoice holds a spoken answer for the flush.
func (s *session) queueVoice(rep betty.Reply, text string) {
	s.voice.pending = append(s.voice.pending, voiceAnswer{
		text: text, seq: len(s.voice.pending), asker: rep.Asker, topic: rep.Topic, attributed: rep.Attributed,
	})
}

// batchText is a flushed answer as delivered: when several answers go out
// together (or a spoken one lands in chat beside others), each must say
// which question it answers, so one the persona did not attribute gets
// who asked and what about (live Discord test: "Yes, Andy, I can hear you"
// answered an earlier "Betty, are you there?" but was posted with the
// board pressure answer and read as part of it).
func (s *session) batchText(a voiceAnswer, voice, batched bool) string {
	if !batched || a.attributed || strings.TrimSpace(a.asker) == "" || s.persona == nil {
		return a.text
	}
	return s.persona.Attribute(voice, a.asker, a.topic, a.text)
}

// flushVoice speaks the ready answers shortest first; once the queued
// speech would pass voiceBacklogMax the rest go to chat, with one spoken
// note saying so. An answer that cannot be spoken falls back to chat as
// before.
func (s *session) flushVoice(ctx context.Context) {
	batch := s.voice.pending
	s.voice.pending = nil
	if len(batch) == 0 {
		return
	}
	sort.SliceStable(batch, func(i, j int) bool {
		return utf8.RuneCountInString(batch[i].text) < utf8.RuneCountInString(batch[j].text)
	})
	moved := false
	batched := len(batch) > 1
	for _, a := range batch {
		now := s.now()
		if b := s.voice.backlog(now); b > 0 && b+speechTime(a.text) > voiceBacklogMax {
			s.postAnswer(ctx, s.batchText(a, false, true))
			moved = true
			continue
		}
		if said := s.batchText(a, true, batched); s.speak(ctx, said) {
			s.voice.queued(now, said)
			continue
		}
		s.log().Info("[meetings] answering in chat instead (see the not speaking reason above)")
		s.postAnswer(ctx, s.batchText(a, false, batched))
	}
	if moved && s.persona != nil {
		now := s.now()
		if s.voice.restNoted.IsZero() || now.Sub(s.voice.restNoted) >= restInChatGap {
			note := s.persona.RestInChatText()
			if s.speak(ctx, note) {
				s.voice.queued(now, note)
				s.voice.restNoted = now
			}
		}
	}
}

// speakAck says an acknowledgement unless it would wait behind seconds of
// earlier speech; it reports whether it was spoken.
func (s *session) speakAck(ctx context.Context, text string) bool {
	now := s.now()
	if s.voice.backlog(now) > ackMaxBacklog {
		return false
	}
	if !s.speak(ctx, text) {
		return false
	}
	s.voice.queued(now, text)
	return true
}

// postAnswer posts one of the persona's answers to an addressed request:
// through the chat rate limit, and once that is spent through the small
// answer burst, so parallel answers are not held for a minute.
func (s *session) postAnswer(ctx context.Context, text string) bool {
	now := s.now()
	if !s.rate.allow(now) && (s.burst.n <= 0 || !s.burst.allow(now)) {
		s.drop("rate_limited")
		return false
	}
	return s.deliver(ctx, text)
}
