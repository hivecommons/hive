package live

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// DefaultCallTimeout bounds one call the loop makes (chat post, dispatch,
// fact write).
const DefaultCallTimeout = 30 * time.Second

// Deep dive follow-through defaults (hivecommons/hive#18590): the persona
// says she is still looking once a deep dive passes
// DefaultResearchProgressAfter, and gives up, saying so, past
// DefaultResearchTimeout. The session checks every DefaultResearchCheckEvery.
const (
	// A deep dive is a background job with a hard deadline: past it she
	// says plainly that it took too long rather than leave the room
	// waiting minutes (live tests; research-meeting-bots report item 10).
	DefaultResearchProgressAfter = 20 * time.Second
	DefaultResearchTimeout       = 45 * time.Second
	DefaultResearchCheckEvery    = 5 * time.Second
	// DefaultResearchQueueWait is how much longer a deep dive may take when
	// every research agent had work as it was dispatched: the time allowed
	// for it to reach the head of the queue, on top of the deadline.
	DefaultResearchQueueWait = 90 * time.Second
)

// Options configures a Runner.
type Options struct {
	Meetings Meetings
	Store    *meetings.Store
	NewSpek  SpekFactory
	Research ResearchDispatcher
	Notes    NotesSink
	Policy   Policy
	// Ledger is the hive-wide hourly/daily budget; nil means none.
	Ledger      *Ledger
	Logger      *slog.Logger
	CallTimeout time.Duration
	Now         func() time.Time
	// SpekMode is the panel's description of the model Spek uses.
	SpekMode string
	// UsageFile returns the meeting's model usage ledger written by
	// `hive meeting-llm` (meetllm); nil when Spek reports usage itself or
	// uses no hive-run model.
	UsageFile func(meetingID string) string
	// Persona builds a meeting's attendee persona (Betty); nil, or a nil
	// engine, leaves it off.
	Persona func(m meetings.Meeting) *betty.Engine
	// PersonaName names the attendee in hive's notices when no persona
	// engine runs; empty means "Hive".
	PersonaName string
	// IsLeave reports a leave command (the persona's leave phrases); nil
	// means meetings.IsLeaveCommand.
	IsLeave func(text string) bool
	// ResearchProgressAfter, ResearchTimeout and ResearchCheckEvery time a
	// deep dive's progress note and give-up; zero means the defaults.
	ResearchProgressAfter time.Duration
	ResearchTimeout       time.Duration
	ResearchCheckEvery    time.Duration
	// ResearchQueueWait is the extra time a deep dive queued behind another
	// question gets; zero means DefaultResearchQueueWait.
	ResearchQueueWait time.Duration
	// UnreachableWarnAfter and UnreachableLeaveAfter time the meeting
	// being marked unreachable and the bot leaving it when no reply gets
	// out (reach.go); zero means the defaults.
	UnreachableWarnAfter  time.Duration
	UnreachableLeaveAfter time.Duration
}

// Runner starts a live session when a meeting goes live and ends it when
// the meeting ends. Safe for concurrent use.
type Runner struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	sessions  map[string]*session
	byKick    map[string]string
	early     map[string][]ResearchSignal
	earlyIDs  []string
	observers map[int]func(Event)
	nextObs   int
	unsub     func()
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// actions builds each meeting's draft sink (actions.go, #18406).
	actions actionsHook
}

// NewRunner builds a runner. Start subscribes it to meeting updates.
func NewRunner(opts Options) (*Runner, error) {
	if opts.Meetings == nil || opts.Store == nil || opts.NewSpek == nil {
		return nil, errors.New("meetings: live runner needs meetings, a store and a spek factory")
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ResearchProgressAfter <= 0 {
		opts.ResearchProgressAfter = DefaultResearchProgressAfter
	}
	if opts.ResearchTimeout <= 0 {
		opts.ResearchTimeout = DefaultResearchTimeout
	}
	if opts.ResearchCheckEvery <= 0 {
		opts.ResearchCheckEvery = DefaultResearchCheckEvery
	}
	if opts.ResearchQueueWait <= 0 {
		opts.ResearchQueueWait = DefaultResearchQueueWait
	}
	if opts.UnreachableWarnAfter <= 0 {
		opts.UnreachableWarnAfter = DefaultUnreachableWarnAfter
	}
	if opts.UnreachableLeaveAfter <= 0 {
		opts.UnreachableLeaveAfter = DefaultUnreachableLeaveAfter
	}
	opts.Policy = opts.Policy.withDefaults()
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		opts: opts, log: log,
		sessions: map[string]*session{}, byKick: map[string]string{}, early: map[string][]ResearchSignal{},
		observers: map[int]func(Event){},
		ctx:       ctx, cancel: cancel,
	}, nil
}

// Start subscribes to meeting updates and starts sessions for meetings
// that are already live.
func (r *Runner) Start() {
	r.mu.Lock()
	if r.unsub != nil {
		r.mu.Unlock()
		return
	}
	r.unsub = r.opts.Meetings.Subscribe(r.onUpdate)
	r.mu.Unlock()
	if list, err := r.opts.Store.List(); err == nil {
		for _, m := range list {
			if m.State == meetings.StateLive {
				r.onUpdate(meetings.Update{Meeting: m})
			}
		}
	}
}

// Close ends every session (Spek gets {"type":"end"}) and unsubscribes.
func (r *Runner) Close() {
	r.mu.Lock()
	unsub := r.unsub
	r.unsub = nil
	r.mu.Unlock()
	if unsub != nil {
		unsub()
	}
	r.cancel()
	r.wg.Wait()
}

// Observe registers a live-panel observer; the returned func removes it.
// fn must not block.
func (r *Runner) Observe(fn func(Event)) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextObs
	r.nextObs++
	r.observers[id] = fn
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.observers, id)
	}
}

func (r *Runner) emit(ev Event) {
	r.mu.Lock()
	fns := make([]func(Event), 0, len(r.observers))
	for _, fn := range r.observers {
		fns = append(fns, fn)
	}
	r.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// maxEarlyKicks bounds kicks whose signals arrived before Dispatch returned
// their ID; the oldest is forgotten first.
const maxEarlyKicks = 64

// registerKick maps a dispatched kick to its meeting and replays any
// signals that arrived before the dispatcher returned the kick ID (a fast
// agent can report before Dispatch returns). It runs on the session
// goroutine, so replayed signals are queued without blocking.
func (r *Runner) registerKick(kick, meetingID string) {
	r.mu.Lock()
	r.byKick[kick] = meetingID
	early := r.early[kick]
	delete(r.early, kick)
	s := r.sessions[meetingID]
	r.mu.Unlock()
	if s == nil {
		return
	}
	for _, sig := range early {
		select {
		case s.signals <- sig:
		default:
		}
	}
}

// holdEarly keeps a signal for a kick no meeting has claimed yet; r.mu is
// held.
func (r *Runner) holdEarly(sig ResearchSignal) {
	if _, ok := r.early[sig.KickID]; !ok {
		if len(r.earlyIDs) >= maxEarlyKicks {
			delete(r.early, r.earlyIDs[0])
			r.earlyIDs = r.earlyIDs[1:]
		}
		r.earlyIDs = append(r.earlyIDs, sig.KickID)
	}
	r.early[sig.KickID] = append(r.early[sig.KickID], sig)
}

func (r *Runner) unregisterKick(kick string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byKick, kick)
}

// onUpdate routes one meeting update: start a session when the meeting is
// live, hand everything else to the running session.
func (r *Runner) onUpdate(u meetings.Update) {
	r.mu.Lock()
	s, ok := r.sessions[u.Meeting.ID]
	if !ok && u.Meeting.State == meetings.StateLive && r.ctx.Err() == nil {
		s = r.startLocked(u.Meeting)
		ok = s != nil
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	select {
	case s.updates <- u:
	case <-s.done:
	}
}

// startLocked starts a session; r.mu is held.
func (r *Runner) startLocked(m meetings.Meeting) *session {
	spek := r.opts.NewSpek(m)
	if err := spek.Start(r.ctx); err != nil {
		r.log.Error("[meetings] spektacular live mode did not start; the meeting continues without it", "meeting", m.ID, "error", err)
		return nil
	}
	s := r.newSession(m, spek)
	for _, it := range s.state.Research {
		if it.KickID != "" && (it.Status == ResearchQueued || it.Status == ResearchRunning) {
			r.byKick[it.KickID] = m.ID
		}
	}
	r.sessions[m.ID] = s
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		s.run(r.ctx)
		r.mu.Lock()
		delete(r.sessions, m.ID)
		r.mu.Unlock()
	}()
	r.log.Info("[meetings] live session started", "meeting", m.ID)
	return s
}

func (r *Runner) session(id string) (*session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	return s, ok
}

// DeliverResearch routes a research kick's signal to its meeting. It
// reports whether the kick belongs to a live meeting.
func (r *Runner) DeliverResearch(sig ResearchSignal) bool {
	r.mu.Lock()
	id, ok := r.byKick[sig.KickID]
	s := r.sessions[id]
	if !ok {
		r.holdEarly(sig)
	}
	r.mu.Unlock()
	if !ok || s == nil {
		return false
	}
	select {
	case s.signals <- sig:
		return true
	case <-s.done:
		return false
	}
}

// Approve dispatches a research item that is waiting for approval.
func (r *Runner) Approve(meetingID, rid string) error {
	s, ok := r.session(meetingID)
	if !ok {
		return ErrSessionEnded
	}
	reply := make(chan error, 1)
	select {
	case s.approves <- approveReq{rid: rid, reply: reply}:
		return <-reply
	case <-s.done:
		return ErrSessionEnded
	}
}

// State returns a meeting's live-loop state: the running session's, or
// the stored one after the meeting ended.
func (r *Runner) State(meetingID string) (State, error) {
	if s, ok := r.session(meetingID); ok {
		reply := make(chan State, 1)
		select {
		case s.snaps <- reply:
			return <-reply, nil
		case <-s.done:
		}
	}
	var st State
	err := r.opts.Store.LoadDoc(meetingID, StateDoc, &st)
	return st, err
}
