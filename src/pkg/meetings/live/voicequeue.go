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
	// voiceRestInChat closes a long answer said in brief.
	voiceRestInChat = "The details are in chat."
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
	// acks are the acknowledgements queued for the speech worker and not
	// yet spoken.
	acks []*line
}

// voiceAnswer is one spoken answer waiting for the flush. asker, topic
// and attributed come from the persona's reply (betty.Reply). then runs
// once the answer is on its way (a deep dive starts after its "I'll dig
// into that" is queued, so the lines keep their order).
type voiceAnswer struct {
	rep        betty.Reply
	text       string
	seq        int
	asker      string
	topic      string
	attributed bool
	then       func(accepted bool)
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
	s.queueVoiceThen(rep, text, nil)
}

// queueVoiceThen is queueVoice with a continuation run once the answer is
// queued to go out.
func (s *session) queueVoiceThen(rep betty.Reply, text string, then func(accepted bool)) {
	s.cancelAcks()
	s.voice.pending = append(s.voice.pending, voiceAnswer{
		rep: rep, text: text, seq: len(s.voice.pending), asker: rep.Asker, topic: rep.Topic, attributed: rep.Attributed, then: then,
	})
}

// batchText is a flushed answer as delivered: when several answers go out
// together (or a spoken one lands in chat beside others), each must say
// which question it answers, so one the persona did not attribute opens
// with its topic (live Discord test: "Yes, Andy, I can hear you"
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
// before. Nothing here waits for the meeting: each answer is queued for
// the speech worker (outbound.go) and recorded when its call returns.
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
		l := &line{rep: a.rep, rate: rateAnswer}
		accepted := false
		if b := s.voice.backlog(now); b > 0 && b+speechTime(a.text) > voiceBacklogMax {
			l.text = s.batchText(a, false, true)
			accepted = s.sendReply(ctx, l)
			moved = true
		} else {
			l.voice, l.text, l.spoken = true, s.batchText(a, false, batched), s.batchText(a, true, batched)
			if short := strings.TrimSpace(a.rep.Spoken); short != "" && short != strings.TrimSpace(a.rep.Text) {
				// A short spoken form with the full text in chat (the
				// intro): never the whole line in both channels.
				l.spoken, l.text, l.alsoChat = short, a.text, true
			}
			if strings.TrimSpace(l.rep.Spoken) == "" {
				// A long answer is said as its first sentence, the whole
				// of it goes to chat once: first audio and the say call
				// stay well inside their limits.
				if brief, ok := betty.BriefVoice(l.spoken); ok {
					l.spoken, l.alsoChat = brief+" "+voiceRestInChat, true
				}
			}
			if accepted = s.sendReply(ctx, l); accepted && l.job != nil {
				s.voice.queued(now, l.said())
			}
		}
		if a.then != nil {
			a.then(accepted)
		}
	}
	if moved && s.persona != nil {
		now := s.now()
		if s.voice.restNoted.IsZero() || now.Sub(s.voice.restNoted) >= restInChatGap {
			note := s.persona.RestInChatText()
			l := &line{rep: own(TriggerRestInChat, betty.KindVoice, note, betty.SourceFixed), text: note, voice: true, voiceOnly: true, quiet: true}
			if s.send(ctx, l) {
				s.voice.queued(now, note)
				s.voice.restNoted = now
			}
		}
	}
}

// sendReply sends one of the persona's replies and counts it in
// State.PersonaReplies: counted when it is accepted, and taken back if it
// then reaches the meeting in neither channel.
func (s *session) sendReply(ctx context.Context, l *line) bool {
	counted, failed := false, false
	l.done = func(delivered bool) {
		failed = !delivered
		if failed && counted {
			s.state.PersonaReplies--
		}
	}
	accepted := s.send(ctx, l)
	if accepted && !failed {
		s.state.PersonaReplies++
		counted = true
	}
	return accepted
}

// speakAck queues an acknowledgement unless it would wait behind seconds
// of earlier speech or the meeting cannot hear her; it reports whether it
// was queued. The acknowledgement is dropped again, unsaid, when an answer
// is ready before the speech worker reaches it (cancelAcks): an ack never
// holds an answer back.
func (s *session) speakAck(ctx context.Context, rep betty.Reply, text string) bool {
	now := s.now()
	if s.voice.backlog(now) > ackMaxBacklog {
		return false
	}
	l := &line{rep: rep, text: text, voice: true, voiceOnly: true, quiet: true}
	l.done = func(bool) { s.forgetAck(l) }
	s.voice.acks = append(s.voice.acks, l)
	if !s.send(ctx, l) || l.job == nil {
		s.forgetAck(l)
		return false
	}
	s.voice.queued(now, text)
	return true
}

// cancelAcks drops the acknowledgements still waiting to be spoken: an
// answer is ready, so "let me check on that" would only delay it.
func (s *session) cancelAcks() {
	for _, l := range s.voice.acks {
		if l.job != nil {
			l.job.skip.Store(true)
		}
	}
}

// forgetAck removes a finished acknowledgement from the waiting list.
func (s *session) forgetAck(done *line) {
	kept := s.voice.acks[:0]
	for _, l := range s.voice.acks {
		if l != done {
			kept = append(kept, l)
		}
	}
	s.voice.acks = kept
}
