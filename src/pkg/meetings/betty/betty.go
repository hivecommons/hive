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
	// TriggerTone is not a line: it asks the session to play the listening
	// tone (tone.go). It carries no text and is never recorded or charged.
	TriggerTone = "tone"
)

// Reply is one thing the persona wants to say.
type Reply struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Trigger string `json:"trigger"`
	// Spoken, when set on a voice reply, is the short form to say out
	// loud; Text is then posted in chat once (the intro: a short spoken
	// greeting with the full introduction in chat), never both in full.
	Spoken string `json:"spoken,omitempty"`
	// Question is the line the reply answers, when there is one.
	Question string `json:"question,omitempty"`
	// VoiceWanted records that the request was spoken or asked for a
	// spoken answer (phase B).
	VoiceWanted   bool     `json:"voice_wanted,omitempty"`
	NeedsResearch bool     `json:"needs_research,omitempty"`
	Sources       []string `json:"sources,omitempty"`
	// Links are the pages behind a spoken answer, posted in chat after it
	// is said and never spoken (links.go).
	Links []Link `json:"links,omitempty"`
	// Research, when set, asks the session to start a deep dive on this
	// query through the meeting's research path and to report back with
	// FollowUp when it completes.
	Research string `json:"research,omitempty"`
	// Timing is the reply's stage timings (timing.go). Every reply has
	// one: a model answer carries its stages, any other line when the
	// request was heard (or the line produced) and its source.
	Timing *Timing `json:"timing,omitempty"`
	// Asker and Topic are who asked and what about, on answers to a
	// request; Attributed is set when Text already opens with them. The
	// session uses them to attribute an answer it delivers late or in a
	// batch (a spoken answer moved to chat with others), so the room can
	// tell which question it answers (live Discord test).
	Asker      string `json:"asker,omitempty"`
	Topic      string `json:"topic,omitempty"`
	Attributed bool   `json:"attributed,omitempty"`
}

// Attribute opens text with the topic it answers, in the persona's
// language ("On open PRs: ..."), and names who asked only when two people
// asked about the same topic (attribution.go).
func (e *Engine) Attribute(voice bool, who, topic, text string) string {
	return attribute(e.s.Persona.Language, voice, who, topic, text)
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
	// AckDelay is the ack watchdog: a spoken request is acknowledged only
	// when its answer is not ready this long after the line ended, so a
	// fast answer comes with no filler and no request is acked twice. Zero
	// acks at once (and allows early acks from partial lines).
	AckDelay time.Duration
	// ListeningTone asks the session for the soft chime when she was
	// addressed by voice and will answer (meetings.listening_tone). Off in
	// the zero value; the live wiring sets it from the config, default on.
	ListeningTone bool
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
		AckDelay:          DefaultAckDelay,
	}
}

// DefaultAckDelay is the ack watchdog: a spoken request is acknowledged
// only when its answer is still not ready this long after the line ended.
// It was 0.7 s; a live Meet test showed what that costs: an answer ready
// 2.6 s after the request waited behind "let me check on that" until 6 s,
// because on the bot platforms a spoken line takes its own length plus
// about 2.7 s of overhead and lines play one after another. An ack only
// helps when the answer takes longer than about 3 s, so a normal 1 to
// 2.6 s answer now gets none. (The engine also widens the delay for
// questions expected to be fast: fastAckDelayFactor in queue.go.)
const DefaultAckDelay = 3 * time.Second

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
	line    *Line
	partial *Line
	offer   string
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
	ackTimers chan int
	retrieved chan retrieval
	answers   chan answered
	exhausted atomic.Bool
	// degraded is set at BudgetDegradePercent of the meeting budget:
	// shorter answers, no deep dives (budget.go).
	degraded atomic.Bool
	// onSkip observes a question dropped for budget; set before Start.
	onSkip func(BudgetSkip)

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
	// earlyAck is when each speaker was acknowledged from a partial
	// transcript; their final line then gets no second ack.
	earlyAck map[string]time.Time
	// lastSpoken is each speaker's last spoken request to her (repeat.go):
	// the same request said again while the first is answered is dropped.
	lastSpoken map[string]spokenRequest
	// queueNoteAt is when the last "I have N questions" note went out;
	// lastAnswerAt is when she last answered anything (attribution.go).
	queueNoteAt  time.Time
	lastAnswerAt time.Time
	// lastToneAt is when the last listening tone was asked for (tone.go).
	lastToneAt time.Time
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
		match:  NewMatcher(s.Persona.Name, s.Persona.Aliases),
		leave:  meetings.NewLeaveMatcher(s.Persona.LeavePhrases),
		in:     make(chan input, inboxDepth),
		out:    make(chan Reply, outboxDepth),
		timers: make(chan int, inboxDepth), ackTimers: make(chan int, inboxDepth),
		retrieved: make(chan retrieval, inboxDepth),
		answers:   make(chan answered, inboxDepth),
		pending:   map[int]*pending{},
		earlyAck:  map[string]time.Time{}, lastSpoken: map[string]spokenRequest{},
		done: make(chan struct{}),
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

// ObservePartial offers one partial (not yet final) spoken line. When it
// already addresses the persona with a request and a spoken answer is
// wanted, the acknowledgement is spoken at once instead of after the
// final transcript; the answer still waits for the final line. Lines that
// do not address her are dropped here, so partials never crowd the queue.
func (e *Engine) ObservePartial(l Line) {
	if !l.Spoken || strings.TrimSpace(l.Text) == "" {
		return
	}
	if t := e.match.Match(l.Text, true); !t.Addressed || t.Intro {
		return
	}
	select {
	case e.in <- input{partial: &l}:
	default:
		// A partial is only an early hint; the final line follows.
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

// SetDegraded shortens answers and stops asking for deep dives once the
// meeting's budget is nearly spent.
func (e *Engine) SetDegraded() { e.degraded.Store(true) }

// BudgetSkip is a question the engine did not answer because the meeting's
// budget is spent. The text is meeting speech: the session keeps it in the
// meeting's own records, never in logs.
type BudgetSkip struct {
	Who       string
	Text      string
	Addressed bool
}

// OnBudgetSkip sets the observer of questions dropped for budget. It must
// be set before Start and must not block.
func (e *Engine) OnBudgetSkip(fn func(BudgetSkip)) { e.onSkip = fn }

func (e *Engine) budgetSkip(l Line, addressed bool) {
	e.drop("budget")
	if e.onSkip != nil {
		e.onSkip(BudgetSkip{Who: l.Who, Text: l.Text, Addressed: addressed})
	}
}

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
func (e *Engine) FollowUp(query, summary string) { e.FollowUpIn(query, summary, false) }

// FollowUpIn is FollowUp in the channel the question was asked in: voice
// (two sentences, no links) when voice is true, else chat.
func (e *Engine) FollowUpIn(query, summary string, voice bool) {
	kind, clipFn := KindChat, ClipChat
	if voice {
		kind, clipFn = KindVoice, ClipVoice
	}
	p := phrasesFor(e.s.Persona.Language)
	raw := clipFn(fmt.Sprintf(p.digged, clip(query, maxFollowUpQuery), strings.TrimSpace(summary)))
	if hasUnsourcedFigure(summary) {
		// A figure with no source behind it is not stated (live test:
		// an attendance figure with nothing to check it against).
		raw = clipFn(p.unsourcedText())
	}
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		text := raw
		if e.d.LLM != nil && !e.exhausted.Load() && strings.TrimSpace(summary) != "" && !hasUnsourcedFigure(summary) {
			cctx, cancel := context.WithTimeout(ctx, e.s.AddressedTimeout)
			out, err := e.d.LLM.Complete(cctx, SystemPrompt(e.s.Persona), followUpPrompt(query, summary))
			cancel()
			out = strings.Trim(strings.TrimSpace(out), "\"")
			if err == nil && out != "" && !strings.Contains(out, "{") {
				text = clipFn(out)
			}
		}
		select {
		case e.out <- Reply{Kind: kind, Text: text, Trigger: TriggerFollowUp, Question: query, VoiceWanted: voice,
			Timing: &Timing{HeardAt: e.d.Now(), Source: SourceResearch}}:
		case <-ctx.Done():
		}
	}()
}

// ResearchFailedText is what she says when a deep dive could not start or
// finish, in the persona's language: "I couldn't look that up: <reason>."
// It is one short sentence: a research agent's reason can be a paragraph
// with links, which took 25 s to speak (live Meet test), so only the first
// sentence is kept, links removed, clipped to maxFailReason.
func (e *Engine) ResearchFailedText(reason string) string {
	reason = shortReason(reason)
	if reason == "" {
		reason = "something went wrong"
	}
	return fmt.Sprintf(phrasesFor(e.s.Persona.Language).failed, reason)
}

// shortReason is the first sentence of a failure reason, without links or
// Markdown, no longer than maxFailReason, and without its closing period.
func shortReason(reason string) string {
	reason = strings.Join(strings.Fields(speakable(reason)), " ")
	if i := strings.IndexAny(reason, ".!?\n"); i > 0 && (i+1 >= len(reason) || reason[i+1] == ' ') {
		reason = reason[:i]
	}
	return strings.TrimRight(clip(strings.TrimSpace(reason), maxFailReason), ". …")
}

// ResearchProgressText is the one progress note for a deep dive that runs
// long.
func (e *Engine) ResearchProgressText() string {
	return phrasesFor(e.s.Persona.Language).still
}

// maxFailReason bounds the reason quoted in a failure notice.
const maxFailReason = 90

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
	if r.Timing == nil {
		// A stock line (intro, hello, acknowledgement, queue note, an
		// action reply): heard and ready now, from no model or lookup.
		r.Timing = fixedTiming(e.d.Now())
	}
	select {
	case e.out <- r:
		if r.Trigger != TriggerAck && r.Trigger != TriggerQueue && r.Trigger != TriggerTone {
			e.lastAnswerAt = e.d.Now()
		}
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
			switch {
			case in.line != nil:
				e.onLine(*in.line)
			case in.partial != nil:
				e.onPartial(*in.partial)
			default:
				e.onOffer(in.offer)
			}
		case id := <-e.timers:
			if p := e.pending[id]; p != nil {
				p.waited = true
				e.proceed(p)
			}
		case id := <-e.ackTimers:
			if p := e.pending[id]; p != nil {
				e.watchdogAck(p)
			}
		case r := <-e.retrieved:
			if p := e.pending[r.id]; p != nil {
				p.snips, p.retrieved = r.snips, true
				p.stages.retrievedAt = e.d.Now()
				e.proceed(p)
			}
		case a := <-e.answers:
			if p := e.pending[a.id]; p != nil {
				p.stages.modelDone = a.done
				e.onAnswer(p, a)
			}
		}
	}
}
