package dashboard

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Session startup auto-answer. A new (or resumed) session used to open on the
// vendor CLI's first-run questions, and whatever was queued for it (an Inbox
// seed for spek, a first Send) waited for a person, or was pasted into one of
// those screens when the queue's fail-open deadline passed. Once a session is
// running the hive now takes its CLI to the input prompt itself, with the
// detector the agents use, and only then lets the queue deliver.
//
// The boundaries, each enforced by the runtime's StartupStep (crux/agent
// usersession_startup.go) and bounded here:
//
//   - only known STARTUP screens are answered: a first-run notice, the trust
//     question for the session's own directory, onboarding, an update menu;
//   - a tool or command permission prompt is never answered: sessions run with
//     the CLI's permission prompts on, and those belong to the person;
//   - a login or OAuth screen is never answered: the tab says "needs sign-in";
//   - answering stops at the input prompt, after sessionAutostartWindow, after
//     sessionAutostartMaxAnswers keystroke answers, or the moment a person has
//     typed into the session.
//
// When it cannot finish, the state says which screen is up and the terminal is
// left to the person. The hive then only WATCHES (no keys) so the tab and the
// queue move on once the person is past the screen.
const (
	auditSessionAutostart = "session_autostart"

	// The `startup.state` values of a running session in GET /api/sessions.
	sessionStartupPreparing   = "preparing"    // the hive is getting the CLI to its prompt
	sessionStartupStarted     = "started"      // the CLI reached its input prompt
	sessionStartupNeedsSignIn = "needs_signin" // a login screen; never answered
	sessionStartupBlocked     = "blocked"      // a screen the hive did not get past
	sessionStartupManual      = "manual"       // a person typed; left to them
	sessionStartupUnknown     = "unknown"      // readiness could not be told

	// sessionAutostartWindowEnv overrides, in seconds, how long the hive
	// answers startup screens for after a session starts.
	sessionAutostartWindowEnv     = "HIVE_SESSIONS_AUTOSTART_WINDOW_SEC"
	defaultSessionAutostartWindow = 90 * time.Second
	// sessionAutostartMaxAnswers bounds the answers of one start; a CLI shows
	// two or three startup screens at most.
	sessionAutostartMaxAnswers = 8
	// sessionAutostartMaxPerScreen bounds the answers to one screen: the
	// second covers a keystroke that landed while the screen was painting.
	sessionAutostartMaxPerScreen = 2
	// sessionAutostartUnknownPolls is how many consecutive watch polls may show
	// nothing recognisable before the hive stops holding the queue for a
	// session it was watching.
	sessionAutostartUnknownPolls = 3

	defaultSessionAutostartPoll = 700 * time.Millisecond
	// defaultSessionAutostartPostAnswer lets the CLI draw its next screen
	// before the pane is read again, so a fading menu is never answered twice.
	defaultSessionAutostartPostAnswer = 1500 * time.Millisecond
	defaultSessionAutostartWatchPoll  = 3 * time.Second
)

// sessionAutostartPacing overrides the loop's pacing (tests); zero = defaults.
type sessionAutostartPacing struct {
	poll, postAnswer, watchPoll, window time.Duration
}

func (s *Server) sessionAutostartPoll() time.Duration {
	if s.sessionAutostartTiming.poll > 0 {
		return s.sessionAutostartTiming.poll
	}
	return defaultSessionAutostartPoll
}

func (s *Server) sessionAutostartPostAnswer() time.Duration {
	if s.sessionAutostartTiming.postAnswer > 0 {
		return s.sessionAutostartTiming.postAnswer
	}
	return defaultSessionAutostartPostAnswer
}

func (s *Server) sessionAutostartWatchPoll() time.Duration {
	if s.sessionAutostartTiming.watchPoll > 0 {
		return s.sessionAutostartTiming.watchPoll
	}
	return defaultSessionAutostartWatchPoll
}

func (s *Server) sessionAutostartWindow() time.Duration {
	if s.sessionAutostartTiming.window > 0 {
		return s.sessionAutostartTiming.window
	}
	return time.Duration(sessionEnvInt64(sessionAutostartWindowEnv, int64(defaultSessionAutostartWindow/time.Second))) * time.Second
}

// sessionStartup is the startup state of one running session. Guarded by the
// server's sessionStartupMu; the pointer identifies one start, so a loop left
// over from an earlier start of the same session cannot touch a newer one.
type sessionStartup struct {
	state  string
	screen string
	// refused is true when screen is a prompt the hive never answers.
	refused  bool
	answered []string
	since    time.Time
}

// sessionStartupView is the `startup` block of a running session.
type sessionStartupView struct {
	State string `json:"state"`
	// Screen names the screen the session is stopped at ("" when none).
	Screen string `json:"screen"`
	// Message is the line the session tab shows.
	Message string `json:"message"`
	// Answered lists the startup screens the hive answered for this start.
	Answered []string `json:"answered"`
	Since    string   `json:"since"`
}

// sessionStartupMessage is the human line for a state.
func sessionStartupMessage(e sessionStartup) string {
	switch e.state {
	case sessionStartupPreparing:
		return "Getting the CLI ready…"
	case sessionStartupStarted:
		return "Started"
	case sessionStartupNeedsSignIn:
		return "Needs sign-in. Sign in from the terminal; Hive never signs in for you."
	case sessionStartupBlocked:
		if e.refused {
			return "Waiting for you: " + e.screen + " is on screen, and Hive does not answer it."
		}
		return "Could not get past " + e.screen + ". The terminal is yours."
	case sessionStartupManual:
		return "Someone is typing in this session, so its startup screens are left to them."
	case sessionStartupUnknown:
		return "Could not tell whether the CLI is ready. The terminal is yours."
	}
	return ""
}

// sessionStartupViewOf renders the startup state of id, nil when the session
// has none (auto-answer off, or the hive restarted since it started).
func (s *Server) sessionStartupViewOf(id string) *sessionStartupView {
	s.sessionStartupMu.Lock()
	defer s.sessionStartupMu.Unlock()
	e := s.sessionStartups[id]
	if e == nil {
		return nil
	}
	return &sessionStartupView{State: e.state, Screen: e.screen, Message: sessionStartupMessage(*e),
		Answered: append([]string{}, e.answered...), Since: e.since.UTC().Format(time.RFC3339)}
}

// sessionStartupHolds reports whether the Send queue must wait: the hive is
// still getting the CLI ready, or the CLI is known to be on a screen that is
// not its input prompt. Pasting a queued request there would answer that
// screen with it.
func (s *Server) sessionStartupHolds(id string) bool {
	s.sessionStartupMu.Lock()
	defer s.sessionStartupMu.Unlock()
	e := s.sessionStartups[id]
	if e == nil {
		return false
	}
	switch e.state {
	case sessionStartupPreparing, sessionStartupNeedsSignIn, sessionStartupBlocked, sessionStartupManual:
		return true
	}
	return false
}

// waitSessionStartup blocks a queue worker while the session's startup holds
// the queue and itemID is still its head.
func (s *Server) waitSessionStartup(id, itemID string) {
	for s.sessionStartupHolds(id) && s.sessionQueueHeadIs(id, itemID) {
		time.Sleep(s.sessionQueuePoll())
	}
}

// setSessionStartup records a state for the start e belongs to. It reports
// false when e is no longer the session's current start.
func (s *Server) setSessionStartup(id string, e *sessionStartup, state, screen string, refused bool) bool {
	s.sessionStartupMu.Lock()
	defer s.sessionStartupMu.Unlock()
	if s.sessionStartups[id] != e {
		return false
	}
	if e.state != state || e.screen != screen {
		e.since = time.Now()
	}
	e.state, e.screen, e.refused = state, screen, refused
	return true
}

func (s *Server) noteSessionStartupAnswer(id string, e *sessionStartup, screen string) {
	s.sessionStartupMu.Lock()
	defer s.sessionStartupMu.Unlock()
	if s.sessionStartups[id] == e {
		e.answered = append(e.answered, screen)
	}
}

// clearSessionStartup forgets the state of the start e belongs to.
func (s *Server) clearSessionStartup(id string, e *sessionStartup) {
	s.sessionStartupMu.Lock()
	defer s.sessionStartupMu.Unlock()
	if s.sessionStartups[id] == e {
		delete(s.sessionStartups, id)
	}
}

// sessionAutostartLive reports whether e is still the current start of a
// running session.
func (s *Server) sessionAutostartLive(st *sessionStore, id string, e *sessionStartup) bool {
	s.sessionStartupMu.Lock()
	current := s.sessionStartups[id] == e
	s.sessionStartupMu.Unlock()
	if !current {
		return false
	}
	rec, found := st.get(id)
	return found && rec.Status == sessionStatusRunning
}

// beginSessionAutostart starts the auto-answer for a session that has just
// become running. With the setting off it only forgets any earlier state, and
// the session behaves as before (the person answers, the queue fails open).
func (s *Server) beginSessionAutostart(rt SessionRuntime, st *sessionStore, id, vendor, user string) {
	on := s.deps != nil && s.deps.Config != nil && s.deps.Config.SessionsAutostart()
	e := &sessionStartup{state: sessionStartupPreparing, since: time.Now()}
	s.sessionStartupMu.Lock()
	if s.sessionStartups == nil {
		s.sessionStartups = map[string]*sessionStartup{}
	}
	if on {
		s.sessionStartups[id] = e
	} else {
		delete(s.sessionStartups, id)
	}
	s.sessionStartupMu.Unlock()
	if on {
		go s.runSessionAutostart(rt, st, id, vendor, user, e)
	}
}

// sessionAutostartOutcome maps one read of the screen to a settled state, or
// "" while the hive should keep going. answerable is false once the answer
// budget for the screen on show is spent.
func sessionAutostartOutcome(step crux.UserSessionStartupStep, answerable bool) (state, screen string, refused bool) {
	switch {
	case step.Kind == crux.UserSessionStartupReady:
		return sessionStartupStarted, "", false
	case step.Typed:
		// A person is at the keyboard: nothing more is typed, whatever is up.
		return sessionStartupManual, step.Screen, false
	case step.Kind == crux.UserSessionStartupSignIn:
		return sessionStartupNeedsSignIn, "", false
	case step.Kind == crux.UserSessionStartupRefused:
		return sessionStartupBlocked, step.Screen, true
	case step.Kind == crux.UserSessionStartupScreen && !answerable:
		return sessionStartupBlocked, step.Screen, false
	}
	return "", "", false
}

// runSessionAutostart is the bounded loop of one start: read the screen,
// answer a known startup screen, stop at the input prompt. Each answer is a
// second StartupStep that re-reads the screen itself, so a key is only ever
// sent to a screen that is a startup screen at that moment.
func (s *Server) runSessionAutostart(rt SessionRuntime, st *sessionStore, id, vendor, user string, e *sessionStartup) {
	begin := time.Now()
	deadline := begin.Add(s.sessionAutostartWindow())
	perScreen := map[string]int{}
	answers := 0
	state, screen, refused := "", "", false
	for state == "" {
		if !s.sessionAutostartLive(st, id, e) {
			s.clearSessionStartup(id, e)
			return
		}
		step := rt.StartupStep(id, vendor, false)
		if step.Kind == crux.UserSessionStartupGone {
			s.clearSessionStartup(id, e)
			return
		}
		answerable := answers < sessionAutostartMaxAnswers && perScreen[step.Screen] < sessionAutostartMaxPerScreen
		if state, screen, refused = sessionAutostartOutcome(step, answerable); state != "" {
			break
		}
		if step.Kind == crux.UserSessionStartupScreen {
			if done := rt.StartupStep(id, vendor, true); done.Answered {
				answers++
				perScreen[done.Screen]++
				s.noteSessionStartupAnswer(id, e, done.Screen)
				time.Sleep(s.sessionAutostartPostAnswer())
				continue
			}
		}
		if !time.Now().Before(deadline) {
			// Out of time on a screen the hive knows is a blocked start; on
			// anything else it simply cannot tell.
			if step.Kind == crux.UserSessionStartupScreen {
				state, screen = sessionStartupBlocked, step.Screen
			} else {
				state = sessionStartupUnknown
			}
			break
		}
		time.Sleep(s.sessionAutostartPoll())
	}
	if !s.setSessionStartup(id, e, state, screen, refused) {
		return
	}
	s.recordSessionAutostart(id, e, user, vendor, state, screen, "", begin)
	if state == sessionStartupStarted || state == sessionStartupUnknown {
		return
	}
	s.watchSessionStartup(rt, st, id, vendor, user, e, begin)
}

// watchSessionStartup follows a session the hive stopped answering for (a
// login screen, a prompt it refuses, a person typing). It sends NOTHING: it
// only reads the screen, so the tab says what is up now and the queue is
// released once the CLI is at its input prompt, or once the pane shows
// nothing the hive recognises for sessionAutostartUnknownPolls polls.
func (s *Server) watchSessionStartup(rt SessionRuntime, st *sessionStore, id, vendor, user string, e *sessionStartup, begin time.Time) {
	loading := 0
	for {
		time.Sleep(s.sessionAutostartWatchPoll())
		if !s.sessionAutostartLive(st, id, e) {
			s.clearSessionStartup(id, e)
			return
		}
		s.sessionStartupMu.Lock()
		before := e.state
		s.sessionStartupMu.Unlock()
		step := rt.StartupStep(id, vendor, false)
		final := ""
		switch step.Kind {
		case crux.UserSessionStartupGone:
			s.clearSessionStartup(id, e)
			return
		case crux.UserSessionStartupReady:
			final = sessionStartupStarted
		case crux.UserSessionStartupSignIn:
			loading = 0
			s.setSessionStartup(id, e, sessionStartupNeedsSignIn, "", false)
		case crux.UserSessionStartupRefused:
			loading = 0
			s.setSessionStartup(id, e, sessionStartupBlocked, step.Screen, true)
		case crux.UserSessionStartupScreen:
			loading = 0
			if before != sessionStartupManual {
				s.setSessionStartup(id, e, sessionStartupBlocked, step.Screen, false)
			}
		default:
			if loading++; loading >= sessionAutostartUnknownPolls {
				final = sessionStartupUnknown
			}
		}
		if final != "" {
			if s.setSessionStartup(id, e, final, "", false) {
				s.recordSessionAutostart(id, e, user, vendor, final, "", before, begin)
			}
			return
		}
	}
}

// recordSessionAutostart audits how a start's auto-answer ended: the outcome,
// the screen it stopped at, the screens it answered and how long it took.
// after is the state a watched session was in before it moved on.
func (s *Server) recordSessionAutostart(id string, e *sessionStartup, user, vendor, outcome, screen, after string, begin time.Time) {
	s.sessionStartupMu.Lock()
	answered := strings.Join(e.answered, "+")
	s.sessionStartupMu.Unlock()
	detail := []string{"session", id, "vendor", vendor, "outcome", outcome,
		"answered", answered, "secs", strconv.Itoa(int(time.Since(begin) / time.Second))}
	if screen != "" {
		detail = append(detail, "screen", screen)
	}
	if after != "" {
		detail = append(detail, "after", after)
	}
	s.auditSession(user, auditSessionAutostart, auditDetail(detail...))
	if s.logger != nil {
		s.logger.Info("sessions: startup auto-answer finished", "id", id, "vendor", vendor, "outcome", outcome, "screen", screen, "answered", answered)
	}
}
