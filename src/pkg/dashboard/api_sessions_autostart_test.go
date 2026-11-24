package dashboard

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

// fakeStartup scripts what a session's CLI shows to the startup auto-answer.
// screens[0] is on screen; answering a startup screen closes it (the next one
// appears) unless sticky. Guarded by the fake runtime's mutex.
type fakeStartup struct {
	screens []crux.UserSessionStartupStep
	// sticky keeps an answered screen on show.
	sticky bool
	// answered are the screens answered, in order; answerCalls counts every
	// call that was allowed to answer; reads counts every call.
	answered    []string
	answerCalls int
	reads       int
}

// StartupStep mirrors the runtime's contract: it answers only a startup
// screen nobody has typed into, and only when asked to.
func (f *fakeSessionRuntime) StartupStep(id, vendor string, answer bool) crux.UserSessionStartupStep {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Unscripted (every test that is not about startup): the CLI is at its
	// prompt from the first read, and nothing else of the fake is consulted,
	// so tests that poke the fake's state directly are not read from here.
	st := f.startup
	if st == nil || len(st.screens) == 0 {
		return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupReady}
	}
	if p, ok := f.alive[id]; !ok || !p.Exists || p.Dead {
		return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupGone}
	}
	st.reads++
	if answer {
		st.answerCalls++
	}
	cur := st.screens[0]
	if answer && cur.Kind == crux.UserSessionStartupScreen && !cur.Typed {
		st.answered = append(st.answered, cur.Screen)
		if !st.sticky && len(st.screens) > 1 {
			st.screens = st.screens[1:]
		}
		cur.Answered = true
	}
	return cur
}

// showStartup replaces what the CLI shows from now on.
func (f *fakeSessionRuntime) showStartup(screens ...crux.UserSessionStartupStep) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startup == nil {
		f.startup = &fakeStartup{}
	}
	f.startup.screens = screens
}

// startupCounts copies the script's counters.
func (f *fakeSessionRuntime) startupCounts() (answered []string, answerCalls, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startup == nil {
		return nil, 0, 0
	}
	return append([]string(nil), f.startup.answered...), f.startup.answerCalls, f.startup.reads
}

const (
	testStartupTrust  = "claude workspace trust"
	testStartupNotice = "press-enter notice"
	testStartupDialog = "claude permission dialog"
)

var (
	startupTrustScreen  = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupScreen, Screen: testStartupTrust}
	startupNoticeScreen = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupScreen, Screen: testStartupNotice}
	startupReady        = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupReady}
	startupSignIn       = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupSignIn}
	startupPermission   = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupRefused, Screen: testStartupDialog}
	startupLoading      = crux.UserSessionStartupStep{Kind: crux.UserSessionStartupLoading}
)

// newAutostartEnv is a sessions env with fast auto-answer pacing whose CLI
// shows screens. Every session is ended when the test finishes, so no watch
// loop outlives it.
func newAutostartEnv(t *testing.T, screens ...crux.UserSessionStartupStep) *sessionTestEnv {
	t.Helper()
	t.Setenv(config.EnvSessionsAutostart, "")
	e := newSessionTestEnv(t, true)
	e.s.sessionAutostartTiming = sessionAutostartPacing{poll: time.Millisecond, postAnswer: time.Millisecond, watchPoll: time.Millisecond, window: time.Minute}
	e.rt.showStartup(screens...)
	t.Cleanup(func() {
		e.rt.mu.Lock()
		e.rt.alive = map[string]crux.UserSessionProbe{}
		e.rt.mu.Unlock()
		waitSessionsFor(t, "every startup loop to stop", func() bool {
			e.s.sessionStartupMu.Lock()
			defer e.s.sessionStartupMu.Unlock()
			for _, st := range e.s.sessionStartups {
				if st.state != sessionStartupStarted && st.state != sessionStartupUnknown {
					return false
				}
			}
			return true
		})
	})
	return e
}

// waitStartup waits for the session's startup state and returns its view.
func (e *sessionTestEnv) waitStartup(t *testing.T, id, state string) sessionStartupView {
	t.Helper()
	var v sessionStartupView
	waitSessionsFor(t, "startup state "+state, func() bool {
		got := e.s.sessionStartupViewOf(id)
		if got == nil || got.State != state {
			return false
		}
		v = *got
		return true
	})
	return v
}

// waitStartupReads waits for n more reads of the screen: real polls for a
// "nothing was typed meanwhile" assertion to be wrong in, without a sleep.
func (e *sessionTestEnv) waitStartupReads(t *testing.T, n int) {
	t.Helper()
	_, _, base := e.rt.startupCounts()
	waitSessionsFor(t, "more startup reads", func() bool {
		_, _, reads := e.rt.startupCounts()
		return reads >= base+n
	})
}

// autostartAudit returns the detail of the session_autostart entry carrying
// want, waiting for it to be written.
func (e *sessionTestEnv) autostartAudit(t *testing.T, want string) string {
	t.Helper()
	detail := ""
	waitSessionsFor(t, "the session_autostart audit with "+want, func() bool {
		for _, a := range e.auditActions() {
			if a.Action == auditSessionAutostart && strings.Contains(a.Detail, want) {
				detail = a.Detail
				return true
			}
		}
		return false
	})
	return detail
}

// A session whose CLI opens on startup screens is taken through them to its
// input prompt: each is answered once, the tab says Started, and the outcome
// is audited with the screens that were answered.
func TestSessionAutostartAnswersStartupScreensThenStarts(t *testing.T) {
	e := newAutostartEnv(t, startupNoticeScreen, startupTrustScreen, startupReady)
	id := e.create(t, callerRW, "spek")
	v := e.waitStartup(t, id, sessionStartupStarted)
	if want := []string{testStartupNotice, testStartupTrust}; !reflect.DeepEqual(v.Answered, want) {
		t.Errorf("answered = %v, want %v", v.Answered, want)
	}
	if v.Message != "Started" || v.Screen != "" {
		t.Errorf("view = %+v", v)
	}
	answered, _, _ := e.rt.startupCounts()
	if len(answered) != 2 {
		t.Errorf("runtime answered %v, want each startup screen once", answered)
	}
	detail := e.autostartAudit(t, "outcome="+sessionStartupStarted)
	for _, want := range []string{"session=" + id, "vendor=claude", "answered=" + testStartupNotice + "+" + testStartupTrust} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit %q lacks %q", detail, want)
		}
	}
	startup, _ := sessionOf(t, e, callerRW, id)["startup"].(map[string]any)
	if startup["state"] != sessionStartupStarted || startup["message"] != "Started" {
		t.Errorf("GET /api/sessions startup = %v", startup)
	}
	if started := e.rt.startedSpecs(); len(started) != 1 || !started[0].SeedOnboarding {
		t.Errorf("the session HOME was not seeded: %+v", started)
	}
}

// The boundary: a tool or command permission prompt is NOT answered. The hive
// never even asks the runtime to answer, the tab names the prompt, and the
// terminal is left to the person.
func TestSessionAutostartNeverAnswersAPermissionPrompt(t *testing.T) {
	e := newAutostartEnv(t, startupPermission)
	id := e.create(t, callerRW, "working")
	v := e.waitStartup(t, id, sessionStartupBlocked)
	if v.Screen != testStartupDialog || !strings.Contains(v.Message, testStartupDialog) || !strings.Contains(v.Message, "does not answer") {
		t.Errorf("view = %+v, want the permission prompt named as not answered", v)
	}
	e.waitStartupReads(t, 10)
	answered, answerCalls, _ := e.rt.startupCounts()
	if len(answered) != 0 || answerCalls != 0 {
		t.Fatalf("permission prompt was answered: answered=%v answer calls=%d", answered, answerCalls)
	}
	if typed := e.rt.typedTexts(); len(typed) != 0 {
		t.Fatalf("text was typed into a permission prompt: %v", typed)
	}
	detail := e.autostartAudit(t, "outcome="+sessionStartupBlocked)
	if !strings.Contains(detail, "screen="+testStartupDialog) {
		t.Errorf("audit %q does not name the screen", detail)
	}
}

// A startup screen answered first does not license the permission prompt that
// follows it: the hive stops there.
func TestSessionAutostartStopsAtAPermissionPromptAfterStartupScreens(t *testing.T) {
	e := newAutostartEnv(t, startupTrustScreen, startupPermission)
	id := e.create(t, callerRW, "mixed")
	v := e.waitStartup(t, id, sessionStartupBlocked)
	if !reflect.DeepEqual(v.Answered, []string{testStartupTrust}) || v.Screen != testStartupDialog {
		t.Errorf("view = %+v", v)
	}
	e.waitStartupReads(t, 10)
	if answered, _, _ := e.rt.startupCounts(); !reflect.DeepEqual(answered, []string{testStartupTrust}) {
		t.Fatalf("answered = %v, want only the trust screen", answered)
	}
}

// A login screen is never answered: the tab says needs sign-in, the queued
// request waits, and once the person has signed in (the CLI reaches its
// prompt) the request is delivered.
func TestSessionAutostartSurfacesSignInAndDeliversAfterReady(t *testing.T) {
	e := newAutostartEnv(t, startupSignIn)
	withSeedInbox(t, e)
	rec := e.do(t, callerRW, "POST", "/api/sessions", map[string]any{"name": "spek", "group": "inbox", "vendor": "claude", "seed_items": []map[string]string{{"source": "plaud", "id": "rec-2"}}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sess, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)
	id, _ := sess["id"].(string)
	v := e.waitStartup(t, id, sessionStartupNeedsSignIn)
	if !strings.Contains(v.Message, "Needs sign-in") {
		t.Errorf("message = %q", v.Message)
	}
	// The seed is queued, the pane fake is idle ("›"), and still nothing is
	// typed: the queue waits for the startup state, not for its own deadline.
	waitSessionsFor(t, "the seed to be queued", func() bool { return e.s.sessionQueueSnapshot(id, callerRW.user).Length == 1 })
	e.waitStartupReads(t, 20)
	if typed := e.rt.typedTexts(); len(typed) != 0 {
		t.Fatalf("the request was typed into a login screen: %v", typed)
	}
	if _, answerCalls, _ := e.rt.startupCounts(); answerCalls != 0 {
		t.Fatalf("the login screen was offered an answer %d times", answerCalls)
	}
	// The person signs in; the CLI reaches its input prompt.
	e.rt.showStartup(startupReady)
	e.waitStartup(t, id, sessionStartupStarted)
	waitSessionsFor(t, "the initial request to be delivered", func() bool { return len(e.rt.typedTexts()) == 1 })
	if typed := e.rt.typedTexts()[0]; !strings.Contains(typed, ".hive/inbox/") {
		t.Errorf("delivered %q, want the seed instruction", typed)
	}
	detail := e.autostartAudit(t, "after="+sessionStartupNeedsSignIn)
	if !strings.Contains(detail, "outcome="+sessionStartupStarted) {
		t.Errorf("audit = %q", detail)
	}
	waitSeedAudit(t, e)
}

// The initial request of a session that starts on startup screens is
// delivered once they are answered, with no person involved.
func TestSessionAutostartDeliversTheInitialRequestAfterStartupScreens(t *testing.T) {
	e := newAutostartEnv(t, startupNoticeScreen, startupTrustScreen, startupReady)
	withSeedInbox(t, e)
	rec := e.do(t, callerRW, "POST", "/api/sessions", map[string]any{"name": "spek", "group": "inbox", "vendor": "claude", "seed_items": []map[string]string{{"source": "plaud", "id": "rec-2"}}})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sess, _ := decodeSessionJSON(t, rec)["session"].(map[string]any)
	id, _ := sess["id"].(string)
	e.waitStartup(t, id, sessionStartupStarted)
	waitSessionsFor(t, "the initial request to be delivered", func() bool { return len(e.rt.typedTexts()) == 1 })
	if answered, _, _ := e.rt.startupCounts(); len(answered) != 2 {
		t.Errorf("answered = %v", answered)
	}
	waitSeedAudit(t, e)
}

// Once a person has typed into the session nothing is answered, even on a
// screen the hive would otherwise answer.
func TestSessionAutostartLeavesASessionAPersonTypedIn(t *testing.T) {
	typedTrust := startupTrustScreen
	typedTrust.Typed = true
	e := newAutostartEnv(t, typedTrust)
	id := e.create(t, callerRW, "mine")
	v := e.waitStartup(t, id, sessionStartupManual)
	if !strings.Contains(v.Message, "typing") {
		t.Errorf("message = %q", v.Message)
	}
	e.waitStartupReads(t, 10)
	if answered, answerCalls, _ := e.rt.startupCounts(); len(answered) != 0 || answerCalls != 0 {
		t.Fatalf("answered %v (%d answer calls) after a person typed", answered, answerCalls)
	}
	e.autostartAudit(t, "outcome="+sessionStartupManual)
}

// Answering is bounded: a screen that does not close is answered at most
// sessionAutostartMaxPerScreen times, then named and left to the person.
func TestSessionAutostartGivesUpOnAScreenThatDoesNotClose(t *testing.T) {
	e := newAutostartEnv(t, startupTrustScreen)
	e.rt.mu.Lock()
	e.rt.startup.sticky = true
	e.rt.mu.Unlock()
	id := e.create(t, callerRW, "stuck")
	v := e.waitStartup(t, id, sessionStartupBlocked)
	if v.Screen != testStartupTrust || !strings.Contains(v.Message, "Could not get past "+testStartupTrust) {
		t.Errorf("view = %+v", v)
	}
	e.waitStartupReads(t, 10)
	if answered, _, _ := e.rt.startupCounts(); len(answered) != sessionAutostartMaxPerScreen {
		t.Fatalf("answered %d times, want %d", len(answered), sessionAutostartMaxPerScreen)
	}
}

// The window is short and bounded: a pane the hive cannot read as ready within
// it ends as unknown, nothing was answered, and the queue is released to its
// ordinary idle check (a vendor whose prompt the hive does not know must not
// hold a Send forever).
func TestSessionAutostartWindowEndsAsUnknownAndReleasesTheQueue(t *testing.T) {
	e := newAutostartEnv(t, startupLoading)
	e.s.sessionAutostartTiming.window = time.Nanosecond // already spent at the first read: no wall-clock race
	fastSessionQueue(t, e)
	e.rt.setCapture(claudeIdlePane)
	id := e.create(t, callerRW, "slow")
	v := e.waitStartup(t, id, sessionStartupUnknown)
	if !strings.Contains(v.Message, "Could not tell") {
		t.Errorf("message = %q", v.Message)
	}
	if _, answerCalls, _ := e.rt.startupCounts(); answerCalls != 0 {
		t.Errorf("an unrecognised screen was offered an answer %d times", answerCalls)
	}
	// The outcome is audited right after the state is set; wait for it so the
	// start loop has finished writing before the test ends.
	e.autostartAudit(t, "outcome="+sessionStartupUnknown)
	if got := e.statusOf(t, callerRW, "POST", "/api/sessions/"+id+"/input", map[string]any{"text": "hello"}); got != http.StatusAccepted {
		t.Fatalf("input = %d", got)
	}
	waitSessionsFor(t, "the Send to be delivered", func() bool { return len(e.rt.typedTexts()) == 1 })
	// SendText records the text before the worker persists the session and
	// exits. Wait for the worker, not just delivery, before TempDir cleanup.
	waitSessionsFor(t, "the Send worker to stop", func() bool {
		e.s.sessionQueuesMu.Lock()
		defer e.s.sessionQueuesMu.Unlock()
		q := e.s.sessionQueueFor(id, false)
		return q != nil && !q.running
	})
	if typed := e.rt.typedTexts(); len(typed) != 1 || typed[0] != "hello" {
		t.Errorf("typed = %v, want exactly one delivery of hello", typed)
	}
}

// dashboard.sessions.autostart: false turns the whole thing off: the HOME is
// not seeded, the screen is never read, and the session has no startup state.
func TestSessionAutostartOffByConfig(t *testing.T) {
	e := newAutostartEnv(t, startupTrustScreen)
	off := false
	e.s.deps.Config.Dashboard.Sessions.Autostart = &off
	id := e.create(t, callerRW, "manual")
	if started := e.rt.startedSpecs(); len(started) != 1 || started[0].SeedOnboarding {
		t.Fatalf("specs = %+v, want one start without seeding", started)
	}
	if v := e.s.sessionStartupViewOf(id); v != nil {
		t.Errorf("startup state = %+v, want none", v)
	}
	if _, _, reads := e.rt.startupCounts(); reads != 0 {
		t.Errorf("the screen was read %d times with autostart off", reads)
	}
	if _, has := sessionOf(t, e, callerRW, id)["startup"]; has {
		t.Error("GET /api/sessions carries a startup block with autostart off")
	}
	if e.s.sessionStartupHolds(id) {
		t.Error("the queue is held with autostart off")
	}
}

// The states the tab can show, from one read of the screen.
func TestSessionAutostartOutcome(t *testing.T) {
	typedReady, typedSignIn := startupReady, startupSignIn
	typedReady.Typed, typedSignIn.Typed = true, true
	cases := []struct {
		name       string
		step       crux.UserSessionStartupStep
		answerable bool
		state      string
		screen     string
		refused    bool
	}{
		{"input prompt", startupReady, true, sessionStartupStarted, "", false},
		{"input prompt, person typing", typedReady, true, sessionStartupStarted, "", false},
		{"login screen", startupSignIn, true, sessionStartupNeedsSignIn, "", false},
		{"login screen, person typing", typedSignIn, true, sessionStartupManual, "", false},
		{"permission prompt", startupPermission, true, sessionStartupBlocked, testStartupDialog, true},
		{"startup screen, budget left", startupTrustScreen, true, "", "", false},
		{"startup screen, budget spent", startupTrustScreen, false, sessionStartupBlocked, testStartupTrust, false},
		{"still loading", startupLoading, true, "", "", false},
	}
	for _, c := range cases {
		state, screen, refused := sessionAutostartOutcome(c.step, c.answerable)
		if state != c.state || screen != c.screen || refused != c.refused {
			t.Errorf("%s: (%q, %q, %v), want (%q, %q, %v)", c.name, state, screen, refused, c.state, c.screen, c.refused)
		}
	}
}

// Every state has a line for the tab, and only the states that mean "the CLI
// is not at its prompt" hold the queue.
func TestSessionStartupMessagesAndQueueHold(t *testing.T) {
	holds := map[string]bool{
		sessionStartupPreparing: true, sessionStartupNeedsSignIn: true, sessionStartupBlocked: true, sessionStartupManual: true,
		sessionStartupStarted: false, sessionStartupUnknown: false,
	}
	s := NewServer(0, nil)
	s.sessionStartups = map[string]*sessionStartup{}
	for state, want := range holds {
		st := &sessionStartup{state: state, screen: testStartupTrust}
		if sessionStartupMessage(*st) == "" {
			t.Errorf("state %q has no message", state)
		}
		s.sessionStartups["a1"] = st
		if got := s.sessionStartupHolds("a1"); got != want {
			t.Errorf("state %q holds the queue = %v, want %v", state, got, want)
		}
	}
	if s.sessionStartupHolds("none") {
		t.Error("a session without startup state holds the queue")
	}
	if got := sessionStartupMessage(sessionStartup{state: sessionStartupPreparing}); got != "Getting the CLI ready…" {
		t.Errorf("preparing message = %q", got)
	}
}
