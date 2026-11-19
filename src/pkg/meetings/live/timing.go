package live

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Reply timings in the live state (hivecommons/hive#18590): the meeting
// detail API (GET /api/meetings/{id}/live, `live.persona_timings` and
// `live.persona_latency`) shows every recent line's stages, and a summary
// of the answers against the answer budgets. Every line the bot says is
// recorded here, in the log and in the transcript (a `reply` event,
// meetings/replies.go), not only model answers: a stock intro, an
// acknowledgement, a follow-up, a queue note, a progress or failure line
// and hive's own notices all carry when the request was heard, when the
// line was ready, when the say call was made and when the audio started.

// maxPersonaTimings bounds the lines kept with their timings.
const maxPersonaTimings = 40

// Triggers of hive's own lines, beside the persona's (betty.Trigger*).
const (
	// TriggerSpek is a chat post Spektacular asked for; TriggerAsk one of
	// its clarifying questions; TriggerSay a line it asked to be spoken.
	TriggerSpek = "spek"
	TriggerAsk  = "ask"
	TriggerSay  = "say"
	// TriggerNotice is one of hive's notices (the budget notice).
	TriggerNotice = "notice"
	// TriggerResearch is a research summary posted without the persona.
	TriggerResearch = "research"
	// TriggerProgress is the "still looking" note of a deep dive that runs
	// long, TriggerResearchFailed its failure notice and TriggerBusy the
	// "yours is next" line of one queued behind another question.
	TriggerProgress       = "progress"
	TriggerResearchFailed = "research_failed"
	TriggerBusy           = "research_busy"
	// TriggerRestInChat is the spoken "I've put the rest in chat".
	TriggerRestInChat = "rest_in_chat"
	// TriggerChatCopy is the full text of a line posted in chat after its
	// short form was spoken (the intro).
	TriggerChatCopy = "chat_copy"
	// TriggerLinks is the chat line of links posted after a spoken answer
	// (their URLs are never spoken).
	TriggerLinks = "links"
)

// SourceSpek is the source of a line Spektacular wrote.
const SourceSpek = "spek"

// PersonaTiming is one line's stages. The embedded betty.Timing counts
// from HeardAt: when the request's final line was heard, or for a line
// nobody asked for, when it was produced.
type PersonaTiming struct {
	betty.Timing
	Trigger string `json:"trigger"`
	// Kind is the channel the line went out in (voice or chat), or the one
	// it was meant for when it was not delivered.
	Kind string `json:"kind"`
	// ReceivedAt is when the session got the request's final line from the
	// meeting, and InboxMS how long it then waited before the persona
	// looked at it (HeardAt): the first stage of
	// received -> reply ready (answer_ms) -> say called (say_called_ms)
	// -> first audio (speech_start_ms).
	ReceivedAt *time.Time `json:"received_at,omitempty"`
	InboxMS    int64      `json:"inbox_ms,omitempty"`
	// DeliverMS is answer ready to sent (chat) or to the end of the say
	// call (spoken), including the wait behind earlier lines and the chat
	// rate.
	DeliverMS int64 `json:"deliver_ms"`
	// QueueMS is how long the line waited behind earlier lines in its
	// channel's queue before its call was made.
	QueueMS int64 `json:"queue_ms,omitempty"`
	// SayCalledMS is heard to the say call, for a spoken line.
	SayCalledMS int64 `json:"say_called_ms,omitempty"`
	// SayMS is how long the meeting's say call took: on Discord until the
	// clip is synthesised and queued, on the bot platforms until the line
	// was spoken.
	SayMS int64 `json:"say_ms,omitempty"`
	// FirstAudioMS (say request to first audio handed to playback),
	// SynthMS (total synthesis time) and TTSCached are what Quorum's say
	// reported (Quorum 0.5.0 and later; zero before).
	FirstAudioMS int64 `json:"first_audio_ms,omitempty"`
	SynthMS      int64 `json:"synth_ms,omitempty"`
	TTSCached    bool  `json:"tts_cached,omitempty"`
	// SpeechStartMS is heard to first audio: SayCalledMS plus FirstAudioMS.
	SpeechStartMS int64 `json:"speech_start_ms,omitempty"`
	// TotalMS is heard to sent.
	TotalMS int64 `json:"total_ms"`
	// Fallback marks a line delivered in the other channel than asked;
	// Undelivered is why a line reached the meeting in neither.
	Fallback    bool   `json:"fallback,omitempty"`
	Undelivered string `json:"undelivered,omitempty"`
}

// PersonaLatency sums the persona's answer timings: counts over the
// meeting, percentiles over the kept answers. Acknowledgements, notes and
// notices are not answers and are not counted.
type PersonaLatency struct {
	Answers    int   `json:"answers"`
	OverBudget int   `json:"over_budget"`
	P50MS      int64 `json:"p50_ms"`
	MaxMS      int64 `json:"max_ms"`
	// LastAckSayMS is how long the last spoken acknowledgement took to be
	// accepted.
	LastAckSayMS int64 `json:"last_ack_say_ms,omitempty"`
}

// isAnswer reports whether a trigger is an answer to someone, the lines
// the latency summary counts.
func isAnswer(trigger string) bool {
	switch trigger {
	case betty.TriggerAddressed, betty.TriggerQuestion, betty.TriggerIntro, betty.TriggerFollowUp:
		return true
	}
	return false
}

// heardLines bounds the final lines whose arrival time the session keeps.
const heardLines = 32

// heardLine is when the session received one final line.
type heardLine struct {
	at   time.Time
	text string
}

// noteHeard remembers when a final line reached the session.
func (s *session) noteHeard(text string) {
	s.heard = append(s.heard, heardLine{at: s.now(), text: text})
	if n := len(s.heard); n > heardLines {
		s.heard = s.heard[n-heardLines:]
	}
}

// receivedAt is when the session received the line a reply answers: the
// line with that text, else the last one received before the persona
// heard the request.
func (s *session) receivedAt(question string, heardAt time.Time) *time.Time {
	if strings.TrimSpace(question) == "" {
		return nil
	}
	var best *heardLine
	for i := range s.heard {
		h := &s.heard[i]
		if h.at.After(heardAt) {
			continue
		}
		if h.text == question {
			best = h
			continue
		}
		if best == nil || best.text != question {
			best = h
		}
	}
	if best == nil {
		return nil
	}
	at := best.at
	return &at
}

// timingOf is a line's stages once it is out (or known not to be).
func (s *session) timingOf(rep betty.Reply, o outcome, now time.Time) PersonaTiming {
	var t betty.Timing
	if rep.Timing != nil {
		t = *rep.Timing
	}
	if t.HeardAt.IsZero() {
		t.HeardAt = now
	}
	if t.Source == "" {
		t.Source = betty.SourceFixed
	}
	ready := t.HeardAt.Add(time.Duration(t.AnswerMS) * time.Millisecond)
	pt := PersonaTiming{Timing: t, Trigger: rep.Trigger, Kind: rep.Kind,
		DeliverMS: max(0, now.Sub(ready).Milliseconds()), TotalMS: max(0, now.Sub(t.HeardAt).Milliseconds()),
		QueueMS: o.res.waited.Milliseconds(), Fallback: o.fallback}
	if pt.Kind == "" {
		pt.Kind = betty.KindChat
	}
	if at := s.receivedAt(rep.Question, t.HeardAt); at != nil {
		pt.ReceivedAt, pt.InboxMS = at, max(0, t.HeardAt.Sub(*at).Milliseconds())
	}
	if !o.delivered() {
		pt.Undelivered = o.reason
		return pt
	}
	pt.Kind = o.channel
	if o.channel == betty.KindVoice {
		pt.SayMS = o.res.took.Milliseconds()
		pt.SayCalledMS = max(0, o.res.calledAt.Sub(t.HeardAt).Milliseconds())
		pt.FirstAudioMS, pt.SynthMS, pt.TTSCached = o.res.say.FirstAudioMS, o.res.say.SynthMS, o.res.say.Cached
		pt.SpeechStartMS = pt.SayCalledMS + pt.FirstAudioMS
	}
	return pt
}

// record closes the loop on one line the bot said, or tried to: its
// timings go into the live state, one line into the log, the line itself
// into the meeting's transcript as a reply event, and the panel is told.
func (s *session) record(rep betty.Reply, o outcome) {
	now := s.now()
	pt := s.timingOf(rep, o, now)
	s.state.PersonaTimings = append(s.state.PersonaTimings, pt)
	if n := len(s.state.PersonaTimings); n > maxPersonaTimings {
		s.state.PersonaTimings = s.state.PersonaTimings[n-maxPersonaTimings:]
	}
	if isAnswer(rep.Trigger) {
		s.sumLatency(pt)
	}
	if rep.Trigger == betty.TriggerAck && o.delivered() {
		s.latency().LastAckSayMS = pt.SayMS
	}
	text := o.text
	if strings.TrimSpace(text) == "" {
		text = rep.Text
	}
	if o.delivered() {
		s.log().Info("[meetings] reply delivered", "trigger", pt.Trigger, "kind", pt.Kind, "source", pt.Source,
			"inbox_ms", pt.InboxMS, "heard_to_ready_ms", pt.AnswerMS, "deliver_ms", pt.DeliverMS, "queue_ms", pt.QueueMS,
			"say_called_ms", pt.SayCalledMS, "say_ms", pt.SayMS,
			"first_audio_ms", pt.FirstAudioMS, "speech_start_ms", pt.SpeechStartMS, "total_ms", pt.TotalMS, "fallback", pt.Fallback)
	} else {
		s.log().Warn("[meetings] reply not delivered", "trigger", pt.Trigger, "kind", pt.Kind, "source", pt.Source,
			"reason", pt.Undelivered, "heard_to_ready_ms", pt.AnswerMS, "total_ms", pt.TotalMS)
	}
	ev := Event{Kind: EventPersona, Reason: rep.Trigger}
	ev.Intent, _ = json.Marshal(rep)
	ev.Line = s.logReply(rep, text, pt, o, now)
	s.emit(ev)
}

// logReply appends the line to the meeting's transcript as a reply event.
// Nothing is kept from before the consent notice: a line refused because
// the notice is not out yet is not recorded.
func (s *session) logReply(rep betty.Reply, text string, pt PersonaTiming, o outcome, now time.Time) *meetings.Event {
	if errors.Is(o.res.err, meetings.ErrConsentPending) {
		return nil
	}
	info := meetings.ReplyInfo{Kind: pt.Kind, Trigger: rep.Trigger, Source: pt.Source, Question: rep.Question,
		Delivered: o.delivered(), Undelivered: pt.Undelivered, Fallback: pt.Fallback}
	info.Timing, _ = json.Marshal(pt)
	ev, err := s.r.opts.Store.AppendReply(s.id, s.personaName(), text, now, info)
	if err != nil {
		s.log().Warn("[meetings] recording a reply in the transcript", "error", err)
		return nil
	}
	return &ev
}

// sumLatency adds an answer to the latency summary.
func (s *session) sumLatency(pt PersonaTiming) {
	l := s.latency()
	l.Answers++
	if pt.OverBudget {
		l.OverBudget++
	}
	totals := make([]int64, 0, len(s.state.PersonaTimings))
	for _, x := range s.state.PersonaTimings {
		if isAnswer(x.Trigger) {
			totals = append(totals, x.TotalMS)
		}
	}
	if len(totals) == 0 {
		return
	}
	slices.Sort(totals)
	l.P50MS, l.MaxMS = totals[len(totals)/2], totals[len(totals)-1]
}

func (s *session) latency() *PersonaLatency {
	if s.state.PersonaLatency == nil {
		s.state.PersonaLatency = &PersonaLatency{}
	}
	return s.state.PersonaLatency
}

// followUpTiming rebases a deep dive follow-up's timing on the question it
// answers: heard when that question was heard, ready when the follow-up
// was written, and sourced from research.
func (s *session) followUpTiming(rep *betty.Reply) {
	now := s.now()
	t := betty.Timing{HeardAt: now, Source: betty.SourceResearch}
	if rep.Timing != nil && !rep.Timing.HeardAt.IsZero() {
		t.HeardAt = rep.Timing.HeardAt
	}
	ready := t.HeardAt
	for i := len(s.state.Research) - 1; i >= 0; i-- {
		it := &s.state.Research[i]
		if it.Betty && it.Query == rep.Question && it.HeardAt != nil {
			t.HeardAt = *it.HeardAt
			break
		}
	}
	t.AnswerMS = max(0, ready.Sub(t.HeardAt).Milliseconds())
	rep.Timing = &t
}
