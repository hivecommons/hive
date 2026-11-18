package live

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Outbound lines (live Meet test: a spoken acknowledgement held the answer
// back by about 5 s). On the bot platforms Quorum's say returns only once
// the line was spoken, and a confirmed chat post only once the line is
// seen in the meeting, so a call made on the session goroutine stopped the
// whole loop for seconds: the next reply, the transcript and research
// signals all waited behind it. Every line now goes through one of two
// ordered queues, one for speech and one for chat, each drained by its own
// worker; the result comes back to the session goroutine on outDone, where
// the line's continuation records it, falls back to the other channel, or
// counts it as undelivered.
//
// A session that is not running (tests drive one directly) has no workers:
// push then makes the call inline, so the same code is synchronous there.

const (
	// outQueueDepth is how many lines may wait in one channel's queue.
	outQueueDepth = 64
	// outQueueFull is why a line was not sent when its queue was full.
	outQueueFull = "too many lines are waiting to go out"
	// outSkipped is why a line was not sent when it was cancelled while it
	// waited (an acknowledgement whose answer was ready first).
	outSkipped = "no longer needed"
	// outRateLimited is why a chat line was held back by the chat rate.
	outRateLimited = "rate_limited"
	// chatNotLanded is why a chat post the platform accepted counts as
	// not delivered: it never showed up in the meeting.
	chatNotLanded = "the chat line did not land in the meeting"
)

// Fallback notices (live Teams test: a chat reply failed while voice
// worked, nothing was spoken and nothing told the meeting). A reply that
// cannot go out in the channel it was asked in goes out in the other one,
// and says so.
const (
	// chatFailedPrefix opens a reply spoken because chat could not carry it.
	chatFailedPrefix = "I couldn't post that in chat, so: "
	// sayFailedPrefix opens a reply posted because the say call failed.
	sayFailedPrefix = "(I couldn't say this out loud.) "
)

// Chat rate classes for an outbound line.
const (
	// The zero value: hive's own follow-through lines (a research summary,
	// the budget notice), never rate limited.
	_ = iota
	// rateChat: the chat rate limit (Spek's posts, room answers).
	rateChat
	// rateAnswer: the chat rate plus the answer burst (addressed answers).
	rateAnswer
)

// outJob is one call to the meeting: say text or post it.
type outJob struct {
	kind string
	text string
	// skip cancels the job while it waits; the worker then does not make
	// the call.
	skip atomic.Bool
	// queuedAt is when the job was queued (wall clock, for the queue wait).
	queuedAt time.Time
	// after runs on the session goroutine with the call's result.
	after func(ctx context.Context, res outResult)
}

// outResult is what one call did.
type outResult struct {
	kind string
	text string
	ok   bool
	// reason is why the line was not delivered; err the call's error.
	reason string
	err    error
	// hard marks a failure of the channel itself (the say or post call
	// failed), as opposed to policy: voice off, consent pending, cancelled.
	hard bool
	say  meetings.SayResult
	// waited is how long the job sat in its queue, took how long the call
	// took, and calledAt when the call was made (the session clock).
	waited   time.Duration
	took     time.Duration
	calledAt time.Time
}

// outDone carries a finished job back to the session goroutine.
type outDone struct {
	job *outJob
	res outResult
}

// startOutbound starts the speech and chat workers; from here on push
// queues instead of calling inline.
func (s *session) startOutbound(ctx context.Context) {
	s.voiceQ, s.chatQ = make(chan *outJob, outQueueDepth), make(chan *outJob, outQueueDepth)
	s.async = true
	go s.outWorker(ctx, s.voiceQ)
	go s.outWorker(ctx, s.chatQ)
}

// outWorker makes one queue's calls in order. It touches nothing of the
// session but its immutable options.
func (s *session) outWorker(ctx context.Context, q <-chan *outJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-q:
			res := s.exec(ctx, job)
			select {
			case s.outDone <- outDone{job: job, res: res}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// push queues a job for its channel's worker, or makes the call inline on
// a session without workers. A full queue fails the job at once.
func (s *session) push(ctx context.Context, job *outJob) {
	job.queuedAt = time.Now()
	if !s.async {
		job.after(ctx, s.exec(ctx, job))
		return
	}
	q := s.chatQ
	if job.kind == betty.KindVoice {
		q = s.voiceQ
	}
	select {
	case q <- job:
	default:
		job.after(ctx, outResult{kind: job.kind, text: job.text, reason: outQueueFull, calledAt: s.now()})
	}
}

// exec makes one job's call. It runs on a worker (or inline) and must not
// touch the session's state.
func (s *session) exec(ctx context.Context, job *outJob) outResult {
	start := time.Now()
	res := outResult{kind: job.kind, text: job.text, waited: start.Sub(job.queuedAt), calledAt: s.now()}
	if job.skip.Load() {
		res.reason = outSkipped
		return res
	}
	if job.kind == betty.KindVoice {
		res.reason, res.say, res.err = s.trySpeak(ctx, job.text)
		res.hard = res.reason == speakSayFailed && !notYetOrOver(res.err)
	} else {
		res.reason, res.hard, res.err = s.chatOnce(ctx, job.text)
	}
	res.took = time.Since(start)
	res.ok = res.reason == ""
	return res
}

// chatOnce posts text in the meeting's chat. It returns the empty reason
// when the line is in the meeting; hard reports a failure of chat itself
// rather than a meeting that is over or not yet told it is recorded.
func (s *session) chatOnce(ctx context.Context, text string) (reason string, hard bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, s.r.opts.CallTimeout)
	defer cancel()
	text = truncate(text, ChatMaxChars)
	if cp, ok := s.r.opts.Meetings.(ConfirmedPoster); ok {
		var landed bool
		if landed, err = cp.PostChatConfirmed(cctx, s.id, text); err == nil && !landed {
			s.log().Warn("[meetings] chat post not confirmed in the meeting")
			return chatNotLanded, true, nil
		}
	} else {
		err = s.r.opts.Meetings.PostChat(cctx, s.id, text)
	}
	if err == nil {
		return "", false, nil
	}
	s.log().Warn("[meetings] chat post refused", "error", err)
	return err.Error(), !notYetOrOver(err), err
}

// notYetOrOver reports a call refused because the meeting has not been
// told it is recorded yet, or is over: not a failure of the channel.
func notYetOrOver(err error) bool {
	return errors.Is(err, meetings.ErrConsentPending) || errors.Is(err, meetings.ErrNotLive)
}

// Reasons speak could not say a line, logged so a meeting where she
// answers in chat says why (hivecommons/hive live Discord test).
const (
	speakNoMeeting     = "meeting record unreadable"
	speakVoiceDisabled = "voice is off for this meeting"
	speakPolicyOff     = "meetings.voice_on_address is off"
	speakSayFailed     = "the say call failed"
)

// voiceOff says why the meeting cannot hear the bot, or "" when it can:
// the meeting has voice off, or the voice_on_address policy is off.
func (s *session) voiceOff() (string, error) {
	m, err := s.r.opts.Meetings.Get(s.id)
	switch {
	case err != nil:
		return speakNoMeeting, err
	case !m.VoiceEnabled:
		return speakVoiceDisabled, nil
	case !s.r.opts.Policy.VoiceOnAddress:
		return speakPolicyOff, nil
	}
	return "", nil
}

// trySpeak says text when the meeting can hear the bot. It returns the
// empty reason when the line was spoken, with what the say call reported.
func (s *session) trySpeak(ctx context.Context, text string) (string, meetings.SayResult, error) {
	if reason, err := s.voiceOff(); reason != "" {
		return reason, meetings.SayResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.r.opts.CallTimeout)
	defer cancel()
	res, err := s.r.opts.Meetings.Say(cctx, s.id, text)
	if err != nil {
		return speakSayFailed, res, err
	}
	return "", res, nil
}

// line is one thing the bot says in the meeting and how it goes out.
type line struct {
	// rep is the line as recorded: trigger, wanted kind, question, timing.
	rep betty.Reply
	// text is the chat form, spoken the voice form ("" means text).
	text   string
	spoken string
	// voice asks for the line to be spoken; voiceOnly never falls back to
	// chat (an acknowledgement, the "rest is in chat" note).
	voice     bool
	voiceOnly bool
	// alsoChat posts text in chat once after spoken was said (a short
	// spoken intro with the full one in chat).
	alsoChat bool
	// rate is the chat rate class.
	rate int
	// quiet lines are not recorded when they are not delivered.
	quiet bool
	// done runs once the line is out, or known not to be.
	done func(delivered bool)
	// job is the queued speech, for cancelling an acknowledgement.
	job *outJob
}

// outcome is how a line went out.
type outcome struct {
	// channel is the kind it was delivered in, "" when it was not.
	channel  string
	text     string
	fallback bool
	reason   string
	res      outResult
}

func (o outcome) delivered() bool { return o.channel != "" }

// said is the voice form of a line.
func (l *line) said() string {
	if strings.TrimSpace(l.spoken) != "" {
		return l.spoken
	}
	return l.text
}

// send delivers one line: spoken when asked and the meeting can hear the
// bot, else in chat; a line that fails in its channel goes out in the
// other one, saying so. It reports whether the line was accepted (false
// when the chat rate held it back).
func (s *session) send(ctx context.Context, l *line) bool {
	if strings.TrimSpace(l.text) == "" {
		return false
	}
	if !l.voice {
		return s.sendChat(ctx, l, l.text, true)
	}
	if reason, err := s.voiceOff(); reason != "" {
		s.log().Info("[meetings] not speaking", "reason", reason, "error", err)
		if l.voiceOnly {
			s.finish(ctx, l, outcome{reason: reason, text: l.said()})
			return false
		}
		return s.sendChat(ctx, l, l.text, false)
	}
	l.job = &outJob{kind: betty.KindVoice, text: l.said(), after: func(ctx context.Context, res outResult) {
		switch {
		case res.ok:
			s.reached()
			s.finish(ctx, l, outcome{channel: betty.KindVoice, text: res.text, res: res})
			if l.alsoChat {
				cp := &line{rep: l.rep, text: l.text, rate: l.rate, quiet: true}
				cp.rep.Kind, cp.rep.Spoken, cp.rep.Trigger = betty.KindChat, "", TriggerChatCopy
				s.sendChat(ctx, cp, cp.text, false)
			}
		case l.voiceOnly || res.reason == outSkipped:
			s.finish(ctx, l, outcome{reason: res.reason, text: res.text, res: res})
		default:
			s.log().Info("[meetings] not speaking; answering in chat instead", "reason", res.reason, "error", res.err)
			text := l.text
			if res.hard {
				text = sayFailedPrefix + text
			}
			s.sendChat(ctx, l, text, false)
		}
	}}
	s.push(ctx, l.job)
	return true
}

// sendChat posts a line in chat within its rate class. With voiceFallback
// a post that chat itself refused is spoken instead, shortened, when the
// meeting can hear the bot.
func (s *session) sendChat(ctx context.Context, l *line, text string, voiceFallback bool) bool {
	if !s.chatAllowed(l.rate) {
		s.drop(outRateLimited)
		s.finish(ctx, l, outcome{reason: outRateLimited, text: text})
		return false
	}
	s.push(ctx, &outJob{kind: betty.KindChat, text: text, after: func(ctx context.Context, res outResult) {
		if res.ok {
			s.reached()
			s.finish(ctx, l, outcome{channel: betty.KindChat, text: res.text, fallback: l.voice, res: res})
			return
		}
		s.drop("chat_failed")
		if off, _ := s.voiceOff(); !res.hard || !voiceFallback || off != "" {
			if res.hard {
				s.unreached(ctx, res.reason)
			}
			s.finish(ctx, l, outcome{reason: res.reason, text: res.text, res: res})
			return
		}
		chatRes := res
		s.push(ctx, &outJob{kind: betty.KindVoice, text: chatFailedPrefix + betty.ClipVoice(l.said()), after: func(ctx context.Context, res outResult) {
			if res.ok {
				s.reached()
				s.finish(ctx, l, outcome{channel: betty.KindVoice, text: res.text, fallback: true, res: res})
				return
			}
			s.unreached(ctx, chatRes.reason)
			s.finish(ctx, l, outcome{reason: chatRes.reason, text: chatRes.text, res: chatRes})
		}})
	}})
	return true
}

// chatAllowed takes one slot of a chat rate class.
func (s *session) chatAllowed(rate int) bool {
	now := s.now()
	switch rate {
	case rateChat:
		return s.rate.allow(now)
	case rateAnswer:
		return s.rate.allow(now) || (s.burst.n > 0 && s.burst.allow(now))
	}
	return true
}

// finish closes one line: it is recorded, its continuation runs, and the
// state is saved.
func (s *session) finish(ctx context.Context, l *line, o outcome) {
	if o.delivered() || !l.quiet {
		s.record(l.rep, o)
	}
	if l.done != nil {
		l.done(o.delivered())
	}
	if o.delivered() {
		s.postLinks(ctx, l)
	}
	s.save()
}

// linksChatText is a spoken answer's links as one chat line: a header, then
// one "label: URL" per line. Empty when there are none.
func linksChatText(links []betty.Link) string {
	var b strings.Builder
	for _, k := range links {
		if strings.TrimSpace(k.URL) == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Links for my answer:")
		}
		b.WriteString("\n" + strings.TrimSpace(k.Label) + ": " + strings.TrimSpace(k.URL))
	}
	return b.String()
}

// postLinks posts, once, the links behind an answer the room asked for by
// voice, in chat, however the answer itself reached the meeting. The
// answer's own chat copy and the links line carry no links of their own.
func (s *session) postLinks(ctx context.Context, l *line) {
	r := l.rep
	if !r.VoiceWanted || len(r.Links) == 0 || r.Trigger == TriggerLinks || r.Trigger == TriggerChatCopy {
		return
	}
	text := linksChatText(r.Links)
	if text == "" {
		return
	}
	s.send(ctx, &line{rep: own(TriggerLinks, betty.KindChat, text, betty.SourceFixed), text: text, rate: rateAnswer, quiet: true})
}

// own builds the record of one of hive's own lines (not the persona's).
func own(trigger, kind, text, source string) betty.Reply {
	return betty.Reply{Kind: kind, Text: text, Trigger: trigger, Timing: &betty.Timing{Source: source}}
}

// sayLine delivers one of hive's own lines: spoken when voice is set and
// the meeting can hear her, else in chat, never rate limited.
func (s *session) sayLine(ctx context.Context, trigger string, voice bool, text string) {
	kind := betty.KindChat
	if voice {
		kind = betty.KindVoice
	}
	s.send(ctx, &line{rep: own(trigger, kind, text, betty.SourceFixed), text: text, voice: voice})
}

// post is the rate-limited chat path for Spek-originated text; it reports
// whether the line was accepted.
func (s *session) post(ctx context.Context, text string) bool {
	return s.send(ctx, &line{rep: own(TriggerSpek, betty.KindChat, text, SourceSpek), text: text, rate: rateChat})
}

// postAnswer posts one of the persona's answers to an addressed request:
// through the chat rate limit, and once that is spent through the small
// answer burst, so parallel answers are not held for a minute.
func (s *session) postAnswer(ctx context.Context, text string) bool {
	return s.send(ctx, &line{rep: own(betty.TriggerAddressed, betty.KindChat, text, betty.SourceFixed), text: text, rate: rateAnswer})
}
