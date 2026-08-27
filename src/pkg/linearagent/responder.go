package linearagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// AgentSession responder (RFC #4492 Part 2, component C) and activity emitter
// (component D).
//
// THE LATENCY PROBLEM: Linear drops a session that has not received an
// activity within 10 seconds of creation. The governor loop polls at
// eval_interval_s (default 300s) and a hive agent is a tmux CLI session that
// takes minutes to emit anything. Waiting for either before answering Linear
// would lose every session.
//
// So the ack is decoupled from the work, exactly as the RFC prescribes: this
// responder posts the `thought` activity SYNCHRONOUSLY on webhook receipt —
// before the kick is even attempted, bounded by its own deadline, touching
// nothing that can block on an agent — and only then delivers the kick, which
// may take as long as tmux takes. The answer to the RFC's open question 3 is
// therefore "ack before the kick is admitted": an ack that claims receipt is
// honest even if the kick is then refused, because the refusal itself is
// reported into the session as an `error` activity, where the user can see it.
//
// Activity volume (open question 4): start / delegated / finished / failed,
// not every kick-log line. The session feed is a user surface, not a mirror
// of the scrollback.

// ackDeadline bounds the synchronous thought post. Linear's budget is 10
// seconds from session creation; 8 leaves margin for the webhook hop that
// already happened before this code runs.
const ackDeadline = 8 * time.Second

// responderKickLimit bounds the kick message built from a session, matching
// the dashboard kick endpoint's maxKickPromptLen.
const responderKickLimit = 10000

// ActivityPoster is the slice of Client the responder needs. An interface so
// tests substitute a recording fake without an HTTP server.
type ActivityPoster interface {
	CreateActivity(ctx context.Context, sessionID string, content ActivityContent) error
}

// KickFunc delivers a kick message to a named agent (agent.Manager.SendKick).
type KickFunc func(agent, message string) error

// AgentResolver names the agent that handles Linear sessions. Resolution is
// pure config (see the dashboard wiring), so it is safe to call before the
// ack — it can name the agent in the thought without risking the deadline.
type AgentResolver func() (string, error)

// Responder turns verified AgentSessionEvents into an immediate ack, a kick,
// and follow-up activities.
type Responder struct {
	activities ActivityPoster
	kick       KickFunc
	resolve    AgentResolver
	tracker    *Tracker
	logger     *slog.Logger
	ackTimeout time.Duration
}

// NewResponder wires a responder.
func NewResponder(activities ActivityPoster, kick KickFunc, resolve AgentResolver, tracker *Tracker, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	if tracker == nil {
		tracker = NewTracker()
	}
	return &Responder{
		activities: activities,
		kick:       kick,
		resolve:    resolve,
		tracker:    tracker,
		logger:     logger,
		ackTimeout: ackDeadline,
	}
}

// Tracker exposes the session registry for the dashboard.
func (r *Responder) Tracker() *Tracker { return r.tracker }

// SetAckTimeout overrides the ack deadline. Tests only.
func (r *Responder) SetAckTimeout(d time.Duration) { r.ackTimeout = d }

// HandleSessionEvent processes one created/prompted event. The webhook
// receiver calls it on a dedicated goroutine; everything here may take time
// EXCEPT the ack, which runs first under its own deadline and never waits on
// the kick, the governor, or an agent.
func (r *Responder) HandleSessionEvent(ev SessionEvent) {
	r.tracker.Observe(ev)
	sessionID := ev.AgentSession.ID

	agentName, resolveErr := r.resolve()

	// ── The ack. First, synchronous, deadline-bounded, dependent on nothing.
	ackCtx, cancel := context.WithTimeout(context.Background(), r.ackTimeout)
	ack := ActivityContent{Type: ActivityThought, Body: r.ackBody(ev, agentName, resolveErr)}
	if err := r.activities.CreateActivity(ackCtx, sessionID, ack); err != nil {
		// The session may already be marked unresponsive on Linear's side;
		// still deliver the work — the assignment is real even if the ack was
		// lost.
		r.logger.Warn("linearagent: session ack failed", "session", sessionID, "error", err)
	}
	cancel()
	r.tracker.SetState(sessionID, SessionStateAcked, ActivityThought)

	// ── The work. Everything past this point is allowed to be slow.
	if resolveErr != nil {
		r.emit(sessionID, ActivityContent{
			Type: ActivityError,
			Body: "No hive agent is configured to take Linear sessions: " + resolveErr.Error(),
		})
		r.tracker.SetState(sessionID, SessionStateFailed, ActivityError)
		return
	}

	if err := r.kick(agentName, buildKickMessage(ev)); err != nil {
		r.logger.Warn("linearagent: session kick failed", "session", sessionID, "agent", agentName, "error", err)
		r.emit(sessionID, ActivityContent{
			Type: ActivityError,
			Body: fmt.Sprintf("Could not start agent `%s`: %s", agentName, err.Error()),
		})
		r.tracker.SetState(sessionID, SessionStateFailed, ActivityError)
		return
	}

	r.tracker.SetAgent(sessionID, agentName)
	r.emit(sessionID, ActivityContent{
		Type:      ActivityAction,
		Action:    "Delegated",
		Parameter: "hive agent " + agentName,
	})
	r.tracker.SetState(sessionID, SessionStateWorking, ActivityAction)
}

// HandleAgentEvent is the kick-lifecycle hook (component D): wired to
// agent.Manager's kick observer, it maps the run's end back onto the session.
// The archive of an agent's kick log is the durable "this run's output is
// complete" moment in the existing infrastructure (#4296) — kick logs rotate
// when a new kick, a restart, or a shutdown closes out the previous run — so
// it is the honest completion signal without inventing a new one.
func (r *Responder) HandleAgentEvent(agentName, event, detail string) {
	if event != "kick-log-archived" {
		return
	}
	sessionID, ok := r.tracker.ActiveSessionForAgent(agentName)
	if !ok {
		return
	}
	r.emit(sessionID, ActivityContent{
		Type: ActivityResponse,
		Body: fmt.Sprintf("Agent `%s` finished this run (kick log archived: %s). The full log is retained on the hive dashboard.", agentName, detail),
	})
	r.tracker.SetState(sessionID, SessionStateFinished, ActivityResponse)
}

// emit posts one non-ack activity with a fresh deadline, logging failures.
func (r *Responder) emit(sessionID string, content ActivityContent) {
	ctx, cancel := context.WithTimeout(context.Background(), r.ackTimeout)
	defer cancel()
	if err := r.activities.CreateActivity(ctx, sessionID, content); err != nil {
		r.logger.Warn("linearagent: activity post failed", "session", sessionID, "type", content.Type, "error", err)
	}
}

// ackBody phrases the thought. It names the agent when one resolved; when
// none did, it still acknowledges receipt — the error detail follows as an
// `error` activity.
func (r *Responder) ackBody(ev SessionEvent, agentName string, resolveErr error) string {
	subject := "this session"
	if id := ev.AgentSession.Issue.Identifier; id != "" {
		subject = id
	}
	if resolveErr != nil {
		return fmt.Sprintf("Hive received %s.", subject)
	}
	if ev.Action == actionPrompted {
		return fmt.Sprintf("Hive received the follow-up on %s; forwarding it to agent `%s`.", subject, agentName)
	}
	return fmt.Sprintf("Hive received %s; delegating to agent `%s`.", subject, agentName)
}

// buildKickMessage renders the session into a kick prompt.
func buildKickMessage(ev SessionEvent) string {
	var b strings.Builder
	if ev.Action == actionPrompted {
		b.WriteString("A user sent a follow-up message in Linear agent session ")
	} else {
		b.WriteString("You have been delegated work through Linear agent session ")
	}
	b.WriteString(ev.AgentSession.ID)
	if id := ev.AgentSession.Issue.Identifier; id != "" {
		fmt.Fprintf(&b, " for issue %s", id)
		if t := ev.AgentSession.Issue.Title; t != "" {
			fmt.Fprintf(&b, " (%s)", t)
		}
		if u := ev.AgentSession.Issue.URL; u != "" {
			fmt.Fprintf(&b, " — %s", u)
		}
	}
	b.WriteString(".\n\n")
	if txt := ev.PromptText(); txt != "" {
		b.WriteString(txt)
		b.WriteString("\n")
	}
	return truncate(b.String(), responderKickLimit)
}
