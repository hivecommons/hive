// Package betty is the meeting attendee persona (hivecommons/hive#18391,
// phase A #18392): "Betty Bee from the hive" by default. She introduces
// herself on request, answers when addressed, and answers room questions
// nobody else answered, briefly and from fast in-memory sources (knowledge
// vaults, beads, code maps), within strict caps so she never spams a call.
//
// The engine sits beside a live session (src/pkg/meetings/live): the
// session feeds it final transcript lines and chat without blocking, and
// every reply comes back on a channel so the session's own rate limit,
// budget and say fallback still apply.
package betty

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
)

// Reply kinds. A voice reply is spoken through the session's say path,
// which falls back to chat when the meeting cannot hear her.
const (
	KindChat  = "chat"
	KindVoice = "voice"
)

// Reply triggers.
const (
	TriggerIntro     = "intro"
	TriggerAddressed = "addressed"
	TriggerQuestion  = "question"
	TriggerOffer     = "spek_offer"
)

// Reply is one thing the persona wants to say.
type Reply struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Trigger string `json:"trigger"`
	// Question is the line the reply answers, when there is one.
	Question string `json:"question,omitempty"`
	// VoiceWanted records that the request was spoken or asked for a
	// spoken answer (phase B).
	VoiceWanted   bool     `json:"voice_wanted,omitempty"`
	NeedsResearch bool     `json:"needs_research,omitempty"`
	Sources       []string `json:"sources,omitempty"`
	// Research, when set, asks the session to start a deep dive on this
	// query through the meeting's research path and to report back with
	// FollowUp when it completes.
	Research string `json:"research,omitempty"`
}

// Line is one final transcript segment or chat line.
type Line struct {
	Who    string
	Text   string
	Spoken bool
}

// Settings are the engine's knobs, from the resolved persona.
type Settings struct {
	Persona config.ResolvedMeetingsPersona

	AnswerWait    time.Duration
	MinGap        time.Duration
	Stale         time.Duration
	CapWindow     time.Duration
	Per10m        int
	PerMeeting    int
	QueueDepth    int
	MinConfidence float64

	RetrieveTimeout   time.Duration
	UnpromptedTimeout time.Duration
	AddressedTimeout  time.Duration
	RestyleTimeout    time.Duration
	// RecentWindow is how much transcript an addressed request sees.
	RecentWindow time.Duration
}

// Engine timing defaults beyond the persona config.
const (
	// DefaultCapWindow is the window of the per-10-minutes cap.
	DefaultCapWindow = 10 * time.Minute
	// DefaultRecentWindow is the transcript an addressed request sees.
	DefaultRecentWindow = 3 * time.Minute
	// maxRecentLines bounds the kept transcript.
	maxRecentLines = 30
	// inboxDepth is the engine's input queue; lines beyond it are dropped
	// rather than blocking the session.
	inboxDepth = 256
	// outboxDepth is the reply queue.
	outboxDepth = 16
	// maxAnsweredTopics bounds the dedupe memory.
	maxAnsweredTopics = 64
)

// SettingsFrom builds settings from a resolved persona.
func SettingsFrom(p config.ResolvedMeetingsPersona) Settings {
	u := p.Unprompted
	return Settings{
		Persona:           p,
		AnswerWait:        time.Duration(u.AnswerWaitS) * time.Second,
		MinGap:            time.Duration(u.MinGapS) * time.Second,
		Stale:             time.Duration(u.StaleS) * time.Second,
		CapWindow:         DefaultCapWindow,
		Per10m:            u.Per10m,
		PerMeeting:        u.PerMeeting,
		QueueDepth:        u.QueueDepth,
		MinConfidence:     u.MinConfidence,
		RetrieveTimeout:   DefaultRetrieveTimeout,
		UnpromptedTimeout: DefaultUnpromptedTimeout,
		AddressedTimeout:  DefaultAddressedTimeout,
		RestyleTimeout:    DefaultRestyleTimeout,
		RecentWindow:      DefaultRecentWindow,
	}
}

func (s Settings) withDefaults() Settings {
	d := SettingsFrom(s.Persona)
	if s.CapWindow <= 0 {
		s.CapWindow = d.CapWindow
	}
	if s.RetrieveTimeout <= 0 {
		s.RetrieveTimeout = d.RetrieveTimeout
	}
	if s.UnpromptedTimeout <= 0 {
		s.UnpromptedTimeout = d.UnpromptedTimeout
	}
	if s.AddressedTimeout <= 0 {
		s.AddressedTimeout = d.AddressedTimeout
	}
	if s.RestyleTimeout <= 0 {
		s.RestyleTimeout = d.RestyleTimeout
	}
	if s.RecentWindow <= 0 {
		s.RecentWindow = d.RecentWindow
	}
	return s
}

// Deps are the engine's collaborators.
type Deps struct {
	// LLM answers; nil means no model (only intros and plain fallbacks).
	LLM        LLM
	Retrievers []Retriever
	Now        func() time.Time
	Logger     *slog.Logger
	// OnDrop observes a candidate the engine decided not to answer, with
	// the reason; it must not block.
	OnDrop func(reason string)
	// CanResearch reports that the meeting has a research path, so an
	// addressed question the model cannot answer well becomes a deep dive.
	CanResearch bool
}

// input is one queued engine input.
type input struct {
	line  *Line
	offer string
}

// Engine is one meeting's persona. Observe, Offer, Restyle and Skip are
// safe for concurrent use; replies arrive on Replies.
type Engine struct {
	s     Settings
	d     Deps
	match *Matcher
	leave *meetings.LeaveMatcher
	log   *slog.Logger
	// act is the action-request state (actions.go, #18406).
	act actionState

	in        chan input
	out       chan Reply
	timers    chan int
	retrieved chan retrieval
	answers   chan answered
	exhausted atomic.Bool

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	done      chan struct{}

	// loop-owned state
	ctx      context.Context
	pending  map[int]*pending
	nextID   int
	topics   [][]string
	recent   []timedLine
	posts    []time.Time
	lastPost time.Time
	total    int
	acks     int
}

type timedLine struct {
	recentLine
	at time.Time
}

// New builds an engine; Start runs it.
func New(s Settings, d Deps) *Engine {
	s = s.withDefaults()
	if d.Now == nil {
		d.Now = time.Now
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		s: s, d: d, log: log,
		match:     NewMatcher(s.Persona.Name, s.Persona.Aliases),
		leave:     meetings.NewLeaveMatcher(s.Persona.LeavePhrases),
		in:        make(chan input, inboxDepth),
		out:       make(chan Reply, outboxDepth),
		timers:    make(chan int, inboxDepth),
		retrieved: make(chan retrieval, inboxDepth),
		answers:   make(chan answered, inboxDepth),
		pending:   map[int]*pending{},
		done:      make(chan struct{}),
	}
}

// Name is the persona's name.
func (e *Engine) Name() string { return e.s.Persona.Name }

// Start runs the engine until ctx ends or Close.
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		ctx, e.cancel = context.WithCancel(ctx)
		e.ctx = ctx
		go e.run()
	})
}

// Close stops the engine and waits for it.
func (e *Engine) Close() {
	e.closeOnce.Do(func() {
		if e.cancel == nil {
			close(e.done)
		} else {
			e.cancel()
			<-e.done
		}
		// A model holding a process (the warm CLI, #18517) stops with
		// the meeting.
		if c, ok := e.d.LLM.(io.Closer); ok {
			_ = c.Close()
		}
	})
}

// Replies is where the engine's replies arrive.
func (e *Engine) Replies() <-chan Reply { return e.out }

// Observe queues one transcript or chat line without blocking.
func (e *Engine) Observe(l Line) {
	select {
	case e.in <- input{line: &l}:
	default:
		e.log.Warn("[meetings] persona input queue full; line skipped")
	}
}

// Offer queues one of Spek's chat posts as an addressed-class candidate:
// it is posted unless the persona already covered the topic.
func (e *Engine) Offer(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	select {
	case e.in <- input{offer: text}:
	default:
		e.log.Warn("[meetings] persona input queue full; offer skipped")
	}
}

// SetExhausted stops further model calls once the meeting's budget is
// spent.
func (e *Engine) SetExhausted() { e.exhausted.Store(true) }

// Skip reports whether a line is for the persona rather than an answer to
// someone's question: addressed to her, or a leave command.
func (e *Engine) Skip(text string) bool {
	if _, ok := e.actionAddress(text); ok {
		return true
	}
	return e.leave.Match(text) || e.match.Match(text, false).Addressed
}

// Restyle rewrites one of Spek's clarifying questions in the persona's
// voice with one quick model call, falling back to the raw text.
func (e *Engine) Restyle(ctx context.Context, text string) string {
	if e.d.LLM == nil || e.exhausted.Load() || strings.TrimSpace(text) == "" {
		return text
	}
	ctx, cancel := context.WithTimeout(ctx, e.s.RestyleTimeout)
	defer cancel()
	out, err := e.d.LLM.Complete(ctx, SystemPrompt(e.s.Persona), restylePrompt(text))
	out = strings.Trim(strings.TrimSpace(out), "\"")
	if err != nil || out == "" || strings.Contains(out, "{") {
		return text
	}
	return clip(out, restyleMaxChars)
}

// FollowUp reports a finished deep dive: one model call restyles the
// research summary as a short chat answer in the persona's voice, posted
// once on Replies; on failure or timeout the summary is posted as is. It
// does not block.
func (e *Engine) FollowUp(query, summary string) {
	p := phrasesFor(e.s.Persona.Language)
	raw := ClipChat(fmt.Sprintf(p.digged, clip(query, maxFollowUpQuery), strings.TrimSpace(summary)))
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		text := raw
		if e.d.LLM != nil && !e.exhausted.Load() && strings.TrimSpace(summary) != "" {
			cctx, cancel := context.WithTimeout(ctx, e.s.AddressedTimeout)
			out, err := e.d.LLM.Complete(cctx, SystemPrompt(e.s.Persona), followUpPrompt(query, summary))
			cancel()
			out = strings.Trim(strings.TrimSpace(out), "\"")
			if err == nil && out != "" && !strings.Contains(out, "{") {
				text = ClipChat(out)
			}
		}
		select {
		case e.out <- Reply{Kind: KindChat, Text: text, Trigger: TriggerFollowUp, Question: query}:
		case <-ctx.Done():
		}
	}()
}

// maxFollowUpQuery bounds the question quoted in a raw follow-up.
const maxFollowUpQuery = 80

// ack is the next spoken acknowledgement, rotating through the persona
// language's list.
func (e *Engine) ack() string {
	list := phrasesFor(e.s.Persona.Language).acks
	a := list[e.acks%len(list)]
	e.acks++
	return a
}

func (e *Engine) drop(reason string) {
	e.log.Debug("[meetings] persona skipped a candidate", "reason", reason)
	if e.d.OnDrop != nil {
		e.d.OnDrop(reason)
	}
}

func (e *Engine) emit(r Reply) bool {
	if r.Kind == "" {
		r.Kind = KindChat
	}
	select {
	case e.out <- r:
		return true
	default:
		e.drop("reply_backlog")
		return false
	}
}

func (e *Engine) run() {
	defer close(e.done)
	for {
		select {
		case <-e.ctx.Done():
			return
		case in := <-e.in:
			if in.line != nil {
				e.onLine(*in.line)
			} else {
				e.onOffer(in.offer)
			}
		case id := <-e.timers:
			if p := e.pending[id]; p != nil {
				p.waited = true
				e.proceed(p)
			}
		case r := <-e.retrieved:
			if p := e.pending[r.id]; p != nil {
				p.snips, p.retrieved = r.snips, true
				e.proceed(p)
			}
		case a := <-e.answers:
			if p := e.pending[a.id]; p != nil {
				e.onAnswer(p, a)
			}
		}
	}
}
