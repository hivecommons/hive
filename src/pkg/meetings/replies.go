package meetings

import (
	"encoding/json"
	"strings"
	"time"
)

// The bot's own lines in the meeting record. transcript.jsonl holds what
// people said and typed; every line the bot says or posts is appended to it
// as a `reply` event so the meeting can be reviewed with her answers in
// place:
//
//	{"type":"reply","meeting_id":"mtg-1","author":{"name":"Betty"},
//	 "text":"...","at":"...","reply":{"kind":"voice","trigger":"addressed",
//	 "source":"hive-status","delivered":true,"timing":{...}}}
//
// A reply line never carries Quorum's seq: it is written by hive, not
// replayed from Quorum, so the replay cursor (Meeting.LastSeq) and the
// consent fences never see it.

// EventReply is the transcript event type of one of the bot's own lines.
const EventReply = "reply"

// Reply kinds: the channel a reply went out in.
const (
	ReplyKindChat  = "chat"
	ReplyKindVoice = "voice"
)

// Reply sources and triggers for the lines the manager itself says.
const (
	// ReplySourceFixed is a stock line: no model and no lookup made it.
	ReplySourceFixed = "fixed"
	// ReplyTriggerLeave is the line posted when a leave command is heard.
	ReplyTriggerLeave = "leave"
)

// ReasonUnreachable is the state reason when hive left because none of its
// replies could be delivered for a long time (live.DefaultUnreachableLeaveAfter).
const ReasonUnreachable = "unreachable"

// ReplyInfo describes one of the bot's own lines.
type ReplyInfo struct {
	// Kind is the channel the line went out in (ReplyKindVoice or
	// ReplyKindChat), or the one it was meant for when it was not delivered.
	Kind string `json:"kind"`
	// Trigger is why she said it: intro, addressed, question, ack,
	// follow_up, queue, progress, leave and so on.
	Trigger string `json:"trigger"`
	// Source is what the line was made from: hive-status, knowledge, web,
	// research, model, or fixed for a stock line.
	Source string `json:"source,omitempty"`
	// Question is the line the reply answers, when there is one.
	Question string `json:"question,omitempty"`
	// Delivered reports whether the line reached the meeting; Undelivered
	// is why not.
	Delivered   bool   `json:"delivered"`
	Undelivered string `json:"undelivered,omitempty"`
	// Fallback marks a line delivered in the other channel than the one it
	// was asked in.
	Fallback bool `json:"fallback,omitempty"`
	// Timing is the reply's stage timings (ReplyTiming, or the live loop's
	// richer PersonaTiming with the same field names).
	Timing json.RawMessage `json:"timing,omitempty"`
}

// ReplyTiming is the least a reply's timing carries: when the request was
// heard and how long the line took to go out.
type ReplyTiming struct {
	HeardAt   time.Time `json:"heard_at"`
	DeliverMS int64     `json:"deliver_ms"`
	SayMS     int64     `json:"say_ms,omitempty"`
	TotalMS   int64     `json:"total_ms"`
}

// IsReply reports whether a transcript event is one of the bot's own lines.
func (e Event) IsReply() bool { return e.Type == EventReply }

// ReplyLabel is how a reply line is attributed in a rendered transcript:
// "Betty (assistant, spoken; source: hive-status)", so a person or a model
// reading it can tell the assistant's words from the participants'.
func (e Event) ReplyLabel() string {
	who := strings.TrimSpace(e.Who())
	if who == "" {
		who = "hive"
	}
	if e.Reply == nil {
		return who + " (assistant)"
	}
	how := "chat"
	switch {
	case !e.Reply.Delivered:
		how = "not delivered"
		if e.Reply.Undelivered != "" {
			how += ": " + e.Reply.Undelivered
		}
	case e.Reply.Kind == ReplyKindVoice:
		how = "spoken"
	}
	label := who + " (assistant, " + how
	if e.Reply.Source != "" {
		label += "; source: " + e.Reply.Source
	}
	return label + ")"
}

// AppendReply appends one of the bot's own lines to the meeting's
// transcript as a reply event and returns the stored event.
func (s *Store) AppendReply(id, author, text string, at time.Time, info ReplyInfo) (Event, error) {
	stamp, err := json.Marshal(at.UTC())
	if err != nil {
		return Event{}, err
	}
	ev := Event{Type: EventReply, MeetingID: id, Author: &Person{Name: author}, Text: text, At: stamp, Reply: &info}
	raw, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}
	ev.Raw = raw
	s.echoes.remember(id, text, at)
	return ev, s.AppendTranscript(id, ev)
}

// RecordReply stores a line the manager itself said in the meeting (the
// "Leaving now." confirmation) in the meeting record. heardAt is when the
// line it answers was heard, took how long the post or say call took, and
// sendErr the call's error (nil when it was delivered).
func (m *Manager) RecordReply(id, text, kind, trigger string, heardAt time.Time, took time.Duration, sendErr error) {
	now := m.opts.Now()
	if heardAt.IsZero() {
		heardAt = now
	}
	tm := ReplyTiming{HeardAt: heardAt, DeliverMS: took.Milliseconds(), TotalMS: max(0, now.Sub(heardAt).Milliseconds())}
	if kind == ReplyKindVoice {
		tm.SayMS = took.Milliseconds()
	}
	info := ReplyInfo{Kind: kind, Trigger: trigger, Source: ReplySourceFixed, Delivered: sendErr == nil}
	if sendErr != nil {
		info.Undelivered = sendErr.Error()
	}
	info.Timing, _ = json.Marshal(tm)
	if _, err := m.opts.Store.AppendReply(id, m.opts.BotName, text, now, info); err != nil {
		m.log.Warn("[meetings] recording a reply", "meeting", id, "error", err)
		return
	}
	m.log.Info("[meetings] reply delivered", "meeting", id, "trigger", trigger, "kind", kind, "source", ReplySourceFixed,
		"heard_to_ready_ms", int64(0), "deliver_ms", tm.DeliverMS, "say_ms", tm.SayMS, "first_audio_ms", int64(0),
		"total_ms", tm.TotalMS, "delivered", sendErr == nil)
}
