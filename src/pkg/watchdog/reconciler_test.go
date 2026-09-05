package watchdog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock shared by every reconciler test, so
// backoff and crash-loop progression are asserted deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeFleet is a scripted watchdog.Fleet.
type fakeFleet struct {
	mu           sync.Mutex
	obs          map[string]Observation
	obsErr       map[string]error
	paused       map[string]bool
	production   map[string]time.Time
	restarts     []string
	restartErr   error
	restartBlock chan struct{} // non-nil: Restart blocks until closed
	restartDone  chan string   // receives the agent name when a Restart returns
	pauses       []string
	pauseReasons []string
	conds        map[string][]Condition
	// queued is the actionable-work depth reported to the readiness gate.
	// Tests that care about Producing=False must set it: the default of zero
	// means "nothing queued", under which an idle agent is CORRECT.
	queued       int
	queueUnknown bool
}

func newFakeFleet(agents ...string) *fakeFleet {
	f := &fakeFleet{
		obs:         make(map[string]Observation),
		obsErr:      make(map[string]error),
		paused:      make(map[string]bool),
		production:  make(map[string]time.Time),
		conds:       make(map[string][]Condition),
		restartDone: make(chan string, 16),
	}
	for _, a := range agents {
		f.obs[a] = Observation{Backend: "claude", SessionExists: true, Pane: "❯", HasCLIMarker: true}
	}
	return f
}

func (f *fakeFleet) AgentNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.obs))
	for n := range f.obs {
		names = append(names, n)
	}
	return names
}

func (f *fakeFleet) Observe(name string) (Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.obsErr[name]; err != nil {
		return Observation{}, err
	}
	return f.obs[name], nil
}

func (f *fakeFleet) IsPaused(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused[name]
}

func (f *fakeFleet) Restart(ctx context.Context, name string) error {
	f.mu.Lock()
	f.restarts = append(f.restarts, name)
	block := f.restartBlock
	err := f.restartErr
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			// The scripted wedge deliberately ignores ctx like a real wedged
			// tmux path would — keep blocking.
			<-block
		}
	}
	f.restartDone <- name
	return err
}

func (f *fakeFleet) Pause(name, trigger, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses = append(f.pauses, name+"/"+trigger)
	f.pauseReasons = append(f.pauseReasons, reason)
	f.paused[name] = true
	return nil
}

func (f *fakeFleet) LastProduction(name string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.production[name]
	return t, ok
}

func (f *fakeFleet) QueuedWork(name string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueUnknown {
		return 0, false
	}
	return f.queued, true
}

func (f *fakeFleet) setQueued(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = n
}

func (f *fakeFleet) SetConditions(name string, conds []Condition) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conds[name] = conds
}

func (f *fakeFleet) setObs(name string, obs Observation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.obs[name] = obs
}

func (f *fakeFleet) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restarts)
}

func (f *fakeFleet) pauseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pauses)
}

// waitRestart blocks until one Restart call completes or the test times out.
//
// It takes a reconciler so it can also wait for the restart goroutine to
// SETTLE. The fake's restartDone send happens inside Restart, which returns
// before restartDetached retakes r.mu to clear restartInFlight — so a test
// that only waited on the channel could drive the next Tick while the latch
// was still set, and planRestartLocked would correctly refuse to stack a
// second restart on an apparently in-flight one. That presented as a later
// restart silently never happening and the NEXT waitRestart timing out, which
// is a harness race, not a product bug.
func (f *fakeFleet) waitRestart(t *testing.T, r *Reconciler) {
	t.Helper()
	select {
	case <-f.restartDone:
	case <-time.After(waitBudget):
		t.Fatal("timed out waiting for a restart to complete")
	}
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		rec, ok := r.agents[f.lastRestarted()]
		inFlight := ok && rec.restartInFlight
		r.mu.Unlock()
		if !inFlight {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("restart goroutine never cleared restartInFlight")
}

// lastRestarted names the most recent Restart target.
func (f *fakeFleet) lastRestarted() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.restarts) == 0 {
		return ""
	}
	return f.restarts[len(f.restarts)-1]
}

// auditEntry is one recorded durable audit write.
type auditEntry struct {
	user, action, detail, agent string
}

// fakeAlerter records dashboard system alerts and audit-log writes.
type fakeAlerter struct {
	mu      sync.Mutex
	alerts  map[string]string
	cleared []string
	audits  []auditEntry
}

func newFakeAlerter() *fakeAlerter {
	return &fakeAlerter{alerts: make(map[string]string)}
}

func (a *fakeAlerter) AuditLog(user, action, detail, agent string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.audits = append(a.audits, auditEntry{user, action, detail, agent})
}

// auditsFor returns every audit entry with the given action.
func (a *fakeAlerter) auditsFor(action string) []auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []auditEntry
	for _, e := range a.audits {
		if e.action == action {
			out = append(out, e)
		}
	}
	return out
}

func (a *fakeAlerter) auditCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.audits)
}

func (a *fakeAlerter) AddSystemAlert(id, severity, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alerts[id] = severity + ": " + message
}

func (a *fakeAlerter) ClearSystemAlert(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.alerts, id)
	a.cleared = append(a.cleared, id)
}

func (a *fakeAlerter) has(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.alerts[id]
	return ok
}

type fakeAuthProbe struct {
	provider string
	status   AuthStatus
	detail   string
	mu       sync.Mutex
	calls    int
}

func (p *fakeAuthProbe) Provider() string { return p.provider }
func (p *fakeAuthProbe) ProbeAuth(ctx context.Context) (AuthStatus, string) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return p.status, p.detail
}

func (p *fakeAuthProbe) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fastSettings shrinks the RFC ladder for tests while keeping its shape.
// Mode is heal: these tests assert the ACTIONS the reconciler takes, and
// observe mode has its own tests that use heal as their positive control.
func fastSettings() Settings {
	s := DefaultSettings()
	s.Mode = ModeHeal
	s.ProbeInterval = 0 // every Tick sweeps; interval gating has its own test
	s.RestartTimeout = 50 * time.Millisecond
	return s
}

// launchedLongAgo is a StartedAt comfortably past DefaultBootGrace relative to
// the fake clock, so fixtures are classified on their pane rather than being
// suppressed as still-booting.
var launchedLongAgo = time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

func deadObs() Observation {
	return Observation{
		Backend:       "claude",
		SessionExists: true,
		Pane:          "agent@spoke:~$",
		LastChange:    time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), // an hour stale
		StartedAt:     launchedLongAgo,
	}
}

func readyObs() Observation {
	return Observation{
		Backend:       "claude",
		SessionExists: true,
		Pane:          "❯",
		HasCLIMarker:  true,
		LastChange:    time.Date(2026, 8, 23, 11, 59, 0, 0, time.UTC),
		StartedAt:     launchedLongAgo,
	}
}

func newTestReconciler(t *testing.T, s Settings, fleet *fakeFleet, alerter *fakeAlerter, clock *fakeClock, opts ...Option) *Reconciler {
	t.Helper()
	opts = append([]Option{WithClock(clock.Now)}, opts...)
	return New(s, fleet, alerter, testLogger(), opts...)
}

func TestReadyAgentPublishesTrueConditions(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", readyObs())
	fleet.mu.Lock()
	fleet.production["a1"] = time.Date(2026, 8, 23, 11, 30, 0, 0, time.UTC)
	fleet.mu.Unlock()
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)

	r.Tick(context.Background())

	conds := r.Conditions("a1")
	ready, _ := FindCondition(conds, ConditionReady)
	if ready.Status != ConditionTrue || ready.Reason != string(ClassReady) {
		t.Fatalf("Ready = %+v, want True/ready", ready)
	}
	producing, _ := FindCondition(conds, ConditionProducing)
	if producing.Status != ConditionTrue {
		t.Fatalf("Producing = %+v, want True", producing)
	}
	// No auth probe registered and no per-agent verdict: honestly Unknown.
	auth, _ := FindCondition(conds, ConditionAuthenticated)
	if auth.Status != ConditionUnknown {
		t.Fatalf("Authenticated = %+v, want Unknown (unknown is never healthy)", auth)
	}
	if fleet.restartCount() != 0 {
		t.Fatal("ready agent must not be restarted")
	}
	fleet.mu.Lock()
	published := fleet.conds["a1"]
	fleet.mu.Unlock()
	if len(published) != 3 {
		t.Fatalf("conditions must be published to the fleet, got %v", published)
	}
}

func TestDeadAgentRestartWithExponentialBackoff(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)

	// Failure 1: restart fires immediately.
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if fleet.restartCount() != 1 {
		t.Fatalf("want 1 restart, got %d", fleet.restartCount())
	}

	// Still inside the 1m backoff: no second restart.
	clock.Advance(30 * time.Second)
	r.Tick(context.Background())
	if fleet.restartCount() != 1 {
		t.Fatalf("backoff must gate the second restart, got %d", fleet.restartCount())
	}

	// Past 1m: failure 2 fires; backoff doubles to 2m.
	clock.Advance(31 * time.Second)
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if fleet.restartCount() != 2 {
		t.Fatalf("want 2 restarts after first backoff, got %d", fleet.restartCount())
	}

	// 1m later is still inside the 2m window.
	clock.Advance(time.Minute)
	r.Tick(context.Background())
	if fleet.restartCount() != 2 {
		t.Fatalf("exponential backoff not honored, got %d restarts", fleet.restartCount())
	}

	// Another 61s crosses the 2m window: failure 3.
	clock.Advance(61 * time.Second)
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if fleet.restartCount() != 3 {
		t.Fatalf("want 3 restarts, got %d", fleet.restartCount())
	}

	ready, _ := FindCondition(r.Conditions("a1"), ConditionReady)
	if ready.Status != ConditionFalse || ready.Reason != string(ClassShellPrompt) {
		t.Fatalf("Ready = %+v, want False/shell-prompt", ready)
	}
}

func TestCrashLoopEscalatesToPauseAndAlertOnce(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	alerter := newFakeAlerter()
	s := fastSettings()
	s.CrashLoopAfter = 2
	s.Backoff = []time.Duration{time.Millisecond}
	r := newTestReconciler(t, s, fleet, alerter, clock)

	for i := 0; i < 2; i++ {
		r.Tick(context.Background())
		fleet.waitRestart(t, r)
		clock.Advance(time.Second)
	}
	if fleet.restartCount() != 2 {
		t.Fatalf("want 2 restarts before the cap, got %d", fleet.restartCount())
	}

	// Third dead observation crosses the cap: pause + alert, no restart.
	r.Tick(context.Background())
	if fleet.restartCount() != 2 {
		t.Fatal("crash loop must stop restarting")
	}
	if fleet.pauseCount() != 1 {
		t.Fatalf("want exactly 1 escalation pause, got %d", fleet.pauseCount())
	}
	fleet.mu.Lock()
	pause := fleet.pauses[0]
	fleet.mu.Unlock()
	if pause != "a1/"+CrashLoopTrigger {
		t.Fatalf("pause = %q, want trigger %q", pause, CrashLoopTrigger)
	}
	if !alerter.has(crashLoopAlertID("a1")) {
		t.Fatal("crash loop must raise a dashboard alert")
	}

	// Escalation is latched AND the agent is now paused: further ticks are
	// observation-only.
	clock.Advance(time.Hour)
	r.Tick(context.Background())
	if fleet.pauseCount() != 1 || fleet.restartCount() != 2 {
		t.Fatal("latched crash loop must not pause or restart again")
	}
}

func TestHealthyWindowResetsFailuresAndClearsAlert(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	alerter := newFakeAlerter()
	s := fastSettings()
	s.CrashLoopAfter = 2
	s.Backoff = []time.Duration{time.Millisecond}
	s.HealthyReset = 10 * time.Minute
	r := newTestReconciler(t, s, fleet, alerter, clock)

	// Drive into crash loop.
	for i := 0; i < 2; i++ {
		r.Tick(context.Background())
		fleet.waitRestart(t, r)
		clock.Advance(time.Second)
	}
	r.Tick(context.Background())
	if !alerter.has(crashLoopAlertID("a1")) {
		t.Fatal("precondition: crash loop alert raised")
	}

	// Operator resumes and the agent comes back healthy.
	fleet.mu.Lock()
	fleet.paused["a1"] = false
	fleet.mu.Unlock()
	obs := readyObs()
	obs.LastChange = clock.Now()
	fleet.setObs("a1", obs)

	// One good probe is NOT enough (flap laundering).
	r.Tick(context.Background())
	r.mu.Lock()
	failures := r.agents["a1"].Failures
	r.mu.Unlock()
	if failures == 0 {
		t.Fatal("one healthy probe must not reset the failure counter")
	}

	// A full healthy window is.
	clock.Advance(s.HealthyReset + time.Second)
	r.Tick(context.Background())
	r.mu.Lock()
	failures = r.agents["a1"].Failures
	looping := r.agents["a1"].CrashLooping
	r.mu.Unlock()
	if failures != 0 || looping {
		t.Fatalf("healthy window must reset (failures=%d, looping=%v)", failures, looping)
	}
	if alerter.has(crashLoopAlertID("a1")) {
		t.Fatal("recovery must clear the crash loop alert")
	}
}

func TestWedgedRestartNeverBlocksTickAndNeverStacks(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	fleet.restartBlock = make(chan struct{}) // Restart wedges until closed
	clock := newFakeClock()
	alerter := newFakeAlerter()
	s := fastSettings()
	r := newTestReconciler(t, s, fleet, alerter, clock)

	// The tick that fires the wedged restart must itself return promptly.
	done := make(chan struct{})
	go func() {
		r.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Tick blocked on a wedged restart — the control-plane guard failed")
	}
	waitFor(t, func() bool { return fleet.restartCount() == 1 },
		"wedged restart goroutine must enter Fleet.Restart")

	// Past the hard timeout the wedge is alerted; no restart stacks on it.
	clock.Advance(s.RestartTimeout + time.Second)
	clock.Advance(20 * time.Minute) // well past any backoff window too
	r.Tick(context.Background())
	if fleet.restartCount() != 1 {
		t.Fatalf("a second restart stacked on the wedged one: %d", fleet.restartCount())
	}
	if !alerter.has(wedgeAlertID("a1")) {
		t.Fatal("wedged restart must raise a dashboard alert")
	}

	// Un-wedge: the in-flight flag clears and the alert lifts.
	close(fleet.restartBlock)
	fleet.waitRestart(t, r)
	waitFor(t, func() bool { return !alerter.has(wedgeAlertID("a1")) },
		"wedge alert must clear once the restart returns")
}

func TestAuthConditions(t *testing.T) {
	clock := newFakeClock()

	t.Run("pane login screen flips Authenticated false and alerts", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend: "claude", SessionExists: true,
			Pane: "● Login expired · Please run /login", ShowsLoginPrompt: true,
		})
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())
		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionFalse || auth.Reason != "PaneShowsLogin" {
			t.Fatalf("Authenticated = %+v", auth)
		}
		if !alerter.has(authAlertID("a1")) {
			t.Fatal("credential failure must alert")
		}
		if fleet.restartCount() != 0 {
			t.Fatal("auth-required must NEVER restart (the 1042-restart loop)")
		}
	})

	t.Run("provider probe verdicts map to condition statuses", func(t *testing.T) {
		for _, tc := range []struct {
			status AuthStatus
			want   ConditionStatus
			reason string
		}{
			{AuthOK, ConditionTrue, "ProbeOK"},
			{AuthFailed, ConditionFalse, "ProbeFailed"},
			{AuthUnknown, ConditionUnknown, "ProbeInconclusive"},
		} {
			fleet := newFakeFleet("a1")
			fleet.setObs("a1", readyObs())
			probe := &fakeAuthProbe{provider: "anthropic", status: tc.status, detail: "detail"}
			r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock,
				WithAuthProbes(map[string]AuthProbe{"anthropic": probe}))
			r.Tick(context.Background())
			auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
			if auth.Status != tc.want || auth.Reason != tc.reason {
				t.Fatalf("probe %s: Authenticated = %+v, want %s/%s", tc.status, auth, tc.want, tc.reason)
			}
		}
	})

	t.Run("provider probe runs once per provider per sweep", func(t *testing.T) {
		fleet := newFakeFleet("a1", "a2", "a3")
		for _, a := range []string{"a1", "a2", "a3"} {
			fleet.setObs(a, readyObs())
		}
		probe := &fakeAuthProbe{provider: "anthropic", status: AuthOK}
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock,
			WithAuthProbes(map[string]AuthProbe{"anthropic": probe}))
		r.Tick(context.Background())
		if probe.callCount() != 1 {
			t.Fatalf("3 agents on one provider must cost 1 probe, got %d", probe.callCount())
		}
	})

	t.Run("per-agent credential file verdict", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		obs := readyObs()
		obs.AuthKnown = true
		obs.AuthAvailable = false
		fleet.setObs("a1", obs)
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())
		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionFalse || auth.Reason != "CredentialMissing" {
			t.Fatalf("Authenticated = %+v, want False/CredentialMissing", auth)
		}

		// Credential appears: condition recovers and the alert clears.
		obs.AuthAvailable = true
		fleet.setObs("a1", obs)
		r.Tick(context.Background())
		auth, _ = FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionTrue || auth.Reason != "CredentialPresent" {
			t.Fatalf("Authenticated = %+v, want True/CredentialPresent", auth)
		}
		if alerter.has(authAlertID("a1")) {
			t.Fatal("auth recovery must clear the alert")
		}
	})

	t.Run("auth probe disabled leaves provider unprobed", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", readyObs())
		probe := &fakeAuthProbe{provider: "anthropic", status: AuthFailed}
		s := fastSettings()
		s.AuthProbe = false
		r := newTestReconciler(t, s, fleet, newFakeAlerter(), clock,
			WithAuthProbes(map[string]AuthProbe{"anthropic": probe}))
		r.Tick(context.Background())
		if probe.callCount() != 0 {
			t.Fatal("auth_probe:false must not probe")
		}
		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionUnknown {
			t.Fatalf("Authenticated = %+v, want Unknown", auth)
		}
	})
}

func TestReadinessConditions(t *testing.T) {
	clock := newFakeClock()

	t.Run("no evidence is Unknown, never healthy, never fatal", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", readyObs())
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
		r.Tick(context.Background())
		p, _ := FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status != ConditionUnknown || p.Reason != "NoEvidence" {
			t.Fatalf("Producing = %+v, want Unknown/NoEvidence", p)
		}
		if fleet.restartCount() != 0 || fleet.pauseCount() != 0 {
			t.Fatal("readiness must never restart or pause")
		}
	})

	t.Run("stale production degrades to False with a warning, then recovers", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", readyObs())
		fleet.mu.Lock()
		fleet.production["a1"] = clock.Now().Add(-7 * time.Hour)
		// Silence is only a fault when there is work waiting; without this the
		// agent is correctly idle. See the empty-queue subtest below.
		fleet.queued = 3
		fleet.mu.Unlock()
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())
		p, _ := FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status != ConditionFalse || p.Reason != "NoRecentProduction" {
			t.Fatalf("Producing = %+v, want False/NoRecentProduction", p)
		}
		if !alerter.has(producingAlertID("a1")) {
			t.Fatal("not-producing must raise a warning alert")
		}
		if fleet.restartCount() != 0 || fleet.pauseCount() != 0 {
			t.Fatal("readiness failure alone must never restart or pause (RFC OQ3)")
		}

		fleet.mu.Lock()
		fleet.production["a1"] = clock.Now()
		fleet.mu.Unlock()
		r.Tick(context.Background())
		p, _ = FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status != ConditionTrue {
			t.Fatalf("Producing = %+v, want True after recovery", p)
		}
		if alerter.has(producingAlertID("a1")) {
			t.Fatal("recovery must clear the producing alert")
		}
	})
}

// TestProducingRequiresQueuedWork asserts the noise gate: an agent producing
// nothing is only a fault when there is work waiting for it. The queued case
// is the positive control — it proves the silence itself IS detected, so the
// empty-queue case passes because of the gate rather than because nothing was
// ever observed.
func TestProducingRequiresQueuedWork(t *testing.T) {
	clock := newFakeClock()
	stale := clock.Now().Add(-7 * time.Hour)

	newFleet := func() *fakeFleet {
		f := newFakeFleet("a1")
		f.setObs("a1", readyObs())
		f.mu.Lock()
		f.production["a1"] = stale
		f.mu.Unlock()
		return f
	}

	t.Run("empty queue: idle is correct, not unhealthy", func(t *testing.T) {
		fleet := newFleet()
		fleet.setQueued(0)
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())

		p, _ := FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status == ConditionFalse {
			t.Fatalf("an idle agent with an empty queue must not be a fault, got %+v", p)
		}
		if p.Reason != "IdleNoWorkQueued" {
			t.Fatalf("Producing reason = %q, want IdleNoWorkQueued", p.Reason)
		}
		if alerter.has(producingAlertID("a1")) {
			t.Fatal("an empty queue must not raise a not-producing alert — this is the facet-noise rule")
		}
	})

	t.Run("positive control: same silence WITH queued work is a fault", func(t *testing.T) {
		fleet := newFleet()
		fleet.setQueued(1) // exactly minQueuedForNotProducing
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())

		p, _ := FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status != ConditionFalse || p.Reason != "NoRecentProduction" {
			t.Fatalf("Producing = %+v, want False/NoRecentProduction when work is queued", p)
		}
		if !alerter.has(producingAlertID("a1")) {
			t.Fatal("silence with work queued must alert")
		}
	})

	t.Run("unreadable queue is Unknown, never a fault", func(t *testing.T) {
		fleet := newFleet()
		fleet.mu.Lock()
		fleet.queueUnknown = true
		fleet.mu.Unlock()
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
		r.Tick(context.Background())

		p, _ := FindCondition(r.Conditions("a1"), ConditionProducing)
		if p.Status != ConditionUnknown || p.Reason != "QueueUnknown" {
			t.Fatalf("Producing = %+v, want Unknown/QueueUnknown — an unreadable queue is not evidence of a fault", p)
		}
	})
}

func TestReadinessProviderErrorSurfacesBlockedInferenceLine(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("a1")
	fleet.setQueued(3)
	errLine := `API Error: 502 {"type":"error","error":{"type":"api_error","message":"inference backend unreachable"}}`
	fleet.setObs("a1", Observation{
		Backend:            "vllm",
		SessionExists:      true,
		Pane:               "❯",
		HasCLIMarker:       true,
		ProviderErrorClass: "api_error",
		ProviderErrorLine:  errLine,
	})
	fleet.mu.Lock()
	fleet.production["a1"] = clock.Now().Add(-time.Hour)
	fleet.mu.Unlock()
	alerter := newFakeAlerter()
	r := newTestReconciler(t, Settings{Mode: ModeObserve, ProbeInterval: time.Millisecond, NoProductionFor: time.Minute}, fleet, alerter, clock)

	r.Tick(context.Background())

	cond, ok := FindCondition(fleet.conds["a1"], ConditionProducing)
	if !ok {
		t.Fatal("Producing condition missing")
	}
	if cond.Status != ConditionFalse || cond.Reason != "ProviderInferenceError" {
		t.Fatalf("Producing condition = %+v, want ProviderInferenceError false", cond)
	}
	if !strings.Contains(cond.Message, "blocked: inference (api_error)") || !strings.Contains(cond.Message, errLine) {
		t.Fatalf("condition message %q does not surface provider error", cond.Message)
	}
	alerter.mu.Lock()
	alert := alerter.alerts[producingAlertID("a1")]
	alerter.mu.Unlock()
	if !strings.Contains(alert, "alive but not producing") || !strings.Contains(alert, errLine) {
		t.Fatalf("alert %q does not surface provider error", alert)
	}
}

// TestReconcileDoesNotSelfDeadlock is the regression guard for the observe-mode
// self-deadlock: planRestartLocked runs with r.mu held, and a helper inside it
// reached back for a locked settings snapshot, so the goroutine blocked forever
// waiting for the lock it was itself holding. A non-reentrant sync.Mutex makes
// that a hang, not a panic, so it presented as a 10-minute test timeout — and
// on a live hive it would have wedged the governor tick the watchdog rides.
//
// Asserting "Tick returns" is the whole point: a deadlock cannot be caught by
// inspecting state afterwards, because there is no afterwards. Every mode is
// exercised because the deadlocking branches were observe-only, and heal is
// the positive control proving the input reaches the restart decision at all.
func TestReconcileDoesNotSelfDeadlock(t *testing.T) {
	for _, mode := range []Mode{ModeObserve, ModeHeal} {
		t.Run(string(mode), func(t *testing.T) {
			fleet := newFakeFleet("a1")
			fleet.setObs("a1", deadObs())
			s := fastSettings()
			s.Mode = mode
			// Drive the crash-loop branch too: both it and the plain restart
			// branch had a re-entrant call.
			s.CrashLoopAfter = 1
			r := newTestReconciler(t, s, fleet, newFakeAlerter(), newFakeClock())

			done := make(chan struct{})
			go func() {
				defer close(done)
				r.Tick(context.Background()) // restart decision
				r.Tick(context.Background()) // crash-loop decision
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("Tick deadlocked in mode %q — a locked helper re-entered r.mu", mode)
			}
		})
	}
}

// TestObserveModeTakesNoAction asserts observe mode is genuinely inert. The
// heal subtest is the positive control on the SAME input: it proves the input
// is one the reconciler demonstrably acts on, so observe passing cannot mean
// "nothing would have happened anyway".
func TestObserveModeTakesNoAction(t *testing.T) {
	t.Run("observe: classifies and records, restarts nothing", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", deadObs())
		alerter := newFakeAlerter()
		s := fastSettings()
		s.Mode = ModeObserve
		r := newTestReconciler(t, s, fleet, alerter, newFakeClock())

		r.Tick(context.Background())

		if fleet.restartCount() != 0 {
			t.Fatalf("observe mode must not restart, got %d", fleet.restartCount())
		}
		if fleet.pauseCount() != 0 {
			t.Fatalf("observe mode must not pause, got %d", fleet.pauseCount())
		}
		// Conditions still published: that is the evidence observe exists for.
		ready, _ := FindCondition(r.Conditions("a1"), ConditionReady)
		if ready.Status != ConditionFalse {
			t.Fatalf("observe mode must still publish observed truth, got %+v", ready)
		}
		// The record stays pristine, so promoting to heal starts from a clean
		// ladder rather than one already part-way up.
		r.mu.Lock()
		failures := r.agents["a1"].Failures
		looping := r.agents["a1"].CrashLooping
		r.mu.Unlock()
		if failures != 0 || looping {
			t.Fatalf("observe mode must not advance the ladder (failures=%d looping=%v)", failures, looping)
		}
		// And it is recorded, marked not-taken in the ACTION itself.
		entries := alerter.auditsFor(auditActionRestart + observedSuffix)
		if len(entries) != 1 {
			t.Fatalf("observe mode must record what it would have done, got %d entries", len(entries))
		}
		if alerter.auditsFor(auditActionRestart) != nil {
			t.Fatal("an unsuffixed restart action must never be written in observe mode")
		}
	})

	t.Run("positive control: heal acts on the same input", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", deadObs())
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, newFakeClock())

		r.Tick(context.Background())
		fleet.waitRestart(t, r)

		if fleet.restartCount() != 1 {
			t.Fatalf("heal must restart this input, got %d", fleet.restartCount())
		}
		entries := alerter.auditsFor(auditActionRestart)
		if len(entries) != 1 {
			t.Fatalf("heal must record the restart it took, got %d", len(entries))
		}
		if entries[0].user != auditUser {
			t.Fatalf("audit user = %q, want %q", entries[0].user, auditUser)
		}
		if entries[0].agent != "a1" {
			t.Fatalf("audit entry must carry the agent, got %q", entries[0].agent)
		}
		if !strings.Contains(entries[0].detail, string(ClassShellPrompt)) {
			t.Fatalf("audit detail must carry the WHY, got %q", entries[0].detail)
		}
	})
}

// TestObserveModeRecordsCrashLoopWithoutPausing covers the terminal decision:
// the entry an operator reads to decide whether to enable heal.
func TestObserveModeRecordsCrashLoopWithoutPausing(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	alerter := newFakeAlerter()
	s := fastSettings()
	s.Mode = ModeObserve
	s.CrashLoopAfter = 2
	r := newTestReconciler(t, s, fleet, alerter, clock)

	// Seed a record already at the cap, as a heal-mode run would have left it.
	r.mu.Lock()
	r.recordLocked("a1").Failures = 2
	r.mu.Unlock()

	r.Tick(context.Background())

	if fleet.pauseCount() != 0 {
		t.Fatal("observe mode must never pause")
	}
	r.mu.Lock()
	looping := r.agents["a1"].CrashLooping
	r.mu.Unlock()
	if looping {
		t.Fatal("observe mode must not latch CrashLooping — latching would suppress the evidence observe exists to produce")
	}
	entries := alerter.auditsFor(auditActionPause + observedSuffix)
	if len(entries) != 1 {
		t.Fatalf("observe mode must record the would-be pause, got %d", len(entries))
	}
	if alerter.auditsFor(auditActionPause) != nil {
		t.Fatal("an unsuffixed pause action must never be written in observe mode")
	}
}

// TestAuditRecordsActionsNotSweeps asserts the ring is spent on decisions
// rather than observations: a healthy agent swept repeatedly writes nothing.
func TestAuditRecordsActionsNotSweeps(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", readyObs())
	clock := newFakeClock()
	alerter := newFakeAlerter()
	r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)

	for i := 0; i < 10; i++ {
		r.Tick(context.Background())
		clock.Advance(time.Minute)
	}
	if n := alerter.auditCount(); n != 0 {
		t.Fatalf("sweeps and probes must not write audit entries, got %d — a 500-entry ring would evict real actions within minutes", n)
	}

	// Positive control: the same reconciler DOES record a real action.
	fleet.setObs("a1", deadObs())
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if alerter.auditCount() == 0 {
		t.Fatal("a real action must be recorded (positive control)")
	}
}

// TestCrashLoopPauseIsAudited covers the terminal state an operator must be
// able to reconstruct, including the explicit give-up entry.
func TestCrashLoopPauseIsAudited(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	alerter := newFakeAlerter()
	s := fastSettings()
	s.CrashLoopAfter = 2
	s.Backoff = []time.Duration{time.Millisecond}
	r := newTestReconciler(t, s, fleet, alerter, clock)

	for i := 0; i < 2; i++ {
		r.Tick(context.Background())
		fleet.waitRestart(t, r)
		clock.Advance(time.Second)
	}
	r.Tick(context.Background())

	pauses := alerter.auditsFor(auditActionPause)
	if len(pauses) != 1 {
		t.Fatalf("the crash-loop pause must be audited exactly once, got %d", len(pauses))
	}
	if !strings.Contains(pauses[0].detail, "failures=2") {
		t.Fatalf("pause detail must carry the failure count, got %q", pauses[0].detail)
	}
	if len(alerter.auditsFor(auditActionGiveUp)) != 1 {
		t.Fatal("the give-up must be recorded as its own entry, not inferred from the pause")
	}
}

// TestAuditDetailLeaksNoPaneContent guards the durable, operator-readable log
// against carrying the pane — which is exactly where login screens, tokens and
// credential material render.
func TestAuditDetailLeaksNoPaneContent(t *testing.T) {
	const secret = "sk-ant-SUPERSECRET-TOKEN"
	fleet := newFakeFleet("a1")
	obs := deadObs()
	// The secret sits in scrollback ABOVE the prompt, which is how a leaked
	// credential actually appears — and leaves the last line a bare prompt so
	// the agent still classifies as dead. Putting it ON the prompt line made
	// the line 64 chars and non-$-terminated, so looksLikeShellPrompt refused
	// it, the agent classified Unknown, and nothing was ever restarted: the
	// test then timed out in waitRestart instead of checking anything.
	obs.Pane = "export ANTHROPIC_API_KEY=" + secret + "\nagent@spoke:~$"
	fleet.setObs("a1", obs)
	alerter := newFakeAlerter()
	r := newTestReconciler(t, fastSettings(), fleet, alerter, newFakeClock())

	// Positive control: the fixture must actually reach a restart decision, or
	// "no secret in the audit log" would pass because there was no audit log.
	if cls := Classify(obs, r.now(), fastSettings()); !cls.Class.Dead() {
		t.Fatalf("fixture must classify as dead to produce an audited action, got %s", cls.Class)
	}

	r.Tick(context.Background())
	fleet.waitRestart(t, r)

	if alerter.auditCount() == 0 {
		t.Fatal("precondition: an action must have been audited")
	}
	alerter.mu.Lock()
	defer alerter.mu.Unlock()
	for _, e := range alerter.audits {
		if strings.Contains(e.detail, secret) || strings.Contains(e.detail, obs.Pane) {
			t.Fatalf("audit detail leaked pane content: %q", e.detail)
		}
	}
}

// TestBootGraceSuppressesDeadVerdicts asserts a just-launched agent is never
// restarted underneath itself — the race that spawns a second concurrent CLI.
// The past-grace case is the positive control.
func TestBootGraceSuppressesDeadVerdicts(t *testing.T) {
	clock := newFakeClock()

	t.Run("no session inside boot grace is Unknown, not dead", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend:       "claude",
			SessionExists: false,
			StartedAt:     clock.Now().Add(-10 * time.Second), // well inside 60s
		})
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
		r.Tick(context.Background())

		if fleet.restartCount() != 0 {
			t.Fatal("a booting agent must never be restarted underneath itself")
		}
		ready, _ := FindCondition(r.Conditions("a1"), ConditionReady)
		if ready.Status != ConditionUnknown {
			t.Fatalf("Ready = %+v, want Unknown inside boot grace", ready)
		}
	})

	t.Run("positive control: no session past boot grace IS dead", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend:       "claude",
			SessionExists: false,
			StartedAt:     clock.Now().Add(-10 * time.Minute),
		})
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
		r.Tick(context.Background())
		fleet.waitRestart(t, r)

		if fleet.restartCount() != 1 {
			t.Fatalf("a dead session past grace must be restarted, got %d", fleet.restartCount())
		}
	})
}

// TestDeadSessionRestartsOncePerBackoffNotPerTick is the invariant the
// ownership transfer exists for: the watchdog owns dead-session recovery, so
// it must throttle. Sweeping every tick for an hour must produce the backoff
// ladder's worth of restarts, not one per tick.
func TestDeadSessionRestartsOncePerBackoffNotPerTick(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", Observation{
		Backend:       "claude",
		SessionExists: false,
		StartedAt:     launchedLongAgo,
	})
	clock := newFakeClock()
	s := fastSettings()
	s.CrashLoopAfter = 1000 // not the subject here; keep the ladder running
	r := newTestReconciler(t, s, fleet, newFakeAlerter(), clock)

	// 30 sweeps at 10s intervals = 5 minutes of wall clock. The ladder
	// (1m, 2m, 4m, ...) permits 3 restarts in that window, never 30.
	const (
		sweeps       = 30
		sweepGap     = 10 * time.Second
		wantRestarts = 3
	)
	for i := 0; i < sweeps; i++ {
		r.mu.Lock()
		failuresBefore := 0
		if rec := r.agents["a1"]; rec != nil {
			failuresBefore = rec.Failures
		}
		r.mu.Unlock()
		r.Tick(context.Background())
		r.mu.Lock()
		failuresAfter := r.agents["a1"].Failures
		r.mu.Unlock()
		if failuresAfter > failuresBefore {
			// Tick records the attempt synchronously, then runs Restart in a
			// detached goroutine. Let that attempt settle before advancing the
			// fake clock so scheduler timing cannot collapse the backoff ladder.
			fleet.waitRestart(t, r)
		}
		clock.Advance(sweepGap)
	}
	// Detached restarts settle asynchronously, so wait for the ladder's count
	// rather than sampling immediately.
	waitFor(t, func() bool { return fleet.restartCount() >= wantRestarts },
		fmt.Sprintf("want %d restarts across %d sweeps (the backoff ladder)", wantRestarts, sweeps))
	if got := fleet.restartCount(); got != wantRestarts {
		t.Fatalf("want exactly %d restarts across %d sweeps (the backoff ladder), got %d — one per tick would be %d",
			wantRestarts, sweeps, got, sweeps)
	}

	// Positive control: it IS restarting — the assertion above must not pass
	// by nothing ever happening.
	if fleet.restartCount() == 0 {
		t.Fatal("dead-session recovery must actually restart the agent")
	}
}

func TestPausedAgentIsObservedButNeverActedOn(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	fleet.mu.Lock()
	fleet.paused["a1"] = true
	fleet.mu.Unlock()
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r.Tick(context.Background())
	if fleet.restartCount() != 0 {
		t.Fatal("paused agent must never be restarted")
	}
	ready, _ := FindCondition(r.Conditions("a1"), ConditionReady)
	if ready.Status != ConditionFalse {
		t.Fatalf("paused agent conditions must still report truth, got %+v", ready)
	}
}

func TestProbeIntervalGatesSweeps(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	s := fastSettings()
	s.ProbeInterval = 5 * time.Minute
	r := newTestReconciler(t, s, fleet, newFakeAlerter(), clock)

	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	clock.Advance(time.Minute)
	r.Tick(context.Background()) // inside the interval: no sweep at all
	clock.Advance(5 * time.Minute)
	r.Tick(context.Background())
	// The second SWEEP happened (restart still gated by backoff, but the
	// failure counter moved) — assert via the sweep-visible failure count.
	r.mu.Lock()
	failures := r.agents["a1"].Failures
	r.mu.Unlock()
	if failures != 2 {
		t.Fatalf("want 2 sweeps' worth of failures, got %d", failures)
	}
}

func TestDisabledReconcilerDoesNothing(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	s := fastSettings()
	s.Mode = ModeOff
	r := newTestReconciler(t, s, fleet, newFakeAlerter(), newFakeClock())
	if r.Enabled() {
		t.Fatal("Enabled() must report the setting")
	}
	r.Tick(context.Background())
	if fleet.restartCount() != 0 || len(r.Conditions("a1")) != 0 {
		t.Fatal("disabled watchdog must not observe or act")
	}
}

func TestUnobservableAgentIsSkippedLoudly(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.mu.Lock()
	fleet.obsErr["a1"] = errors.New("agent removed mid-tick")
	fleet.mu.Unlock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), newFakeClock())
	r.Tick(context.Background())
	if fleet.restartCount() != 0 {
		t.Fatal("unobservable agent must not be acted on")
	}
	if len(r.Conditions("a1")) != 0 {
		t.Fatal("unobservable agent must not gain fabricated conditions")
	}
}

func TestRestartFailureIsCountedNotHidden(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	fleet.mu.Lock()
	fleet.restartErr = fmt.Errorf("tmux exploded")
	fleet.mu.Unlock()
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	r.mu.Lock()
	failures := r.agents["a1"].Failures
	r.mu.Unlock()
	if failures != 1 {
		t.Fatalf("failed restart must still count toward the crash loop, got %d", failures)
	}
}

// TestTakeRestartedReportsSuccessesOnce asserts the resume-kick handoff: an
// agent the watchdog revived is reported exactly once, and a FAILED restart is
// never reported (resume-kicking an agent that did not come back would kick
// into a dead session).
func TestTakeRestartedReportsSuccessesOnce(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), newFakeClock())

	r.Tick(context.Background())
	fleet.waitRestart(t, r)

	// The restart goroutine records the success just after Restart returns, so
	// drain until it lands rather than sampling once.
	var got []string
	waitFor(t, func() bool {
		got = append(got, r.TakeRestarted()...)
		return len(got) == 1
	}, "a completed restart must be reported for the resume kick")

	if got[0] != "a1" {
		t.Fatalf("TakeRestarted = %v, want [a1]", got)
	}
	if again := r.TakeRestarted(); len(again) != 0 {
		t.Fatalf("TakeRestarted must drain; second call returned %v", again)
	}
}

func TestTakeRestartedSkipsFailedRestarts(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	fleet.mu.Lock()
	fleet.restartErr = fmt.Errorf("tmux exploded")
	fleet.mu.Unlock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), newFakeClock())

	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if got := r.TakeRestarted(); len(got) != 0 {
		t.Fatalf("a failed restart must not be resume-kicked, got %v", got)
	}
}

// TestSetSettingsSwapsModeLive asserts an operator changing the mode from the
// dashboard takes effect on the next sweep, in both directions, and that
// dropping out of heal clears a ladder built while it had authority — so
// re-enabling heal does not resume part-way up, or escalate straight to a pause.
func TestSetSettingsSwapsModeLive(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	heal := fastSettings()
	r := newTestReconciler(t, heal, fleet, newFakeAlerter(), clock)

	// Heal: acts.
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if fleet.restartCount() != 1 {
		t.Fatalf("precondition: heal must restart, got %d", fleet.restartCount())
	}

	// Operator drops to observe.
	observe := fastSettings()
	observe.Mode = ModeObserve
	r.SetSettings(observe)
	if r.Mode() != ModeObserve {
		t.Fatalf("Mode() = %q, want observe", r.Mode())
	}

	r.mu.Lock()
	failures := r.agents["a1"].Failures
	r.mu.Unlock()
	if failures != 0 {
		t.Fatalf("leaving heal must clear the ladder, got %d failures — re-enabling heal would resume part-way up", failures)
	}

	clock.Advance(time.Hour)
	r.Tick(context.Background())
	if fleet.restartCount() != 1 {
		t.Fatalf("observe must not restart after the swap, got %d", fleet.restartCount())
	}

	// And back to heal: acts again.
	r.SetSettings(heal)
	clock.Advance(time.Hour)
	r.Tick(context.Background())
	fleet.waitRestart(t, r)
	if fleet.restartCount() != 2 {
		t.Fatalf("returning to heal must restore healing, got %d", fleet.restartCount())
	}
}

func TestSnapshotRestoreRoundtrip(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r.Tick(context.Background())
	fleet.waitRestart(t, r)

	snap := r.Snapshot()
	if snap["a1"].Failures != 1 || snap["a1"].BackoffUntil == nil || len(snap["a1"].Conditions) == 0 {
		t.Fatalf("snapshot incomplete: %+v", snap["a1"])
	}

	r2 := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r2.Restore(snap)
	r2.mu.Lock()
	rec := r2.agents["a1"]
	failures, backoff := rec.Failures, rec.BackoffUntil
	conds := len(rec.Conditions)
	r2.mu.Unlock()
	if failures != 1 || backoff.IsZero() || conds == 0 {
		t.Fatal("restore must rehydrate failures, backoff, and conditions")
	}
	// The restored backoff still gates: no immediate re-restart.
	r2.Tick(context.Background())
	if fleet.restartCount() != 1 {
		t.Fatal("restored backoff must gate restarts across a process restart")
	}

	// A reconciler that may NOT act does not inherit a ladder it has no
	// authority to have built. Conditions still restore — they are
	// observations, valid in every mode. (heal above is the positive control.)
	observe := fastSettings()
	observe.Mode = ModeObserve
	r3 := newTestReconciler(t, observe, fleet, newFakeAlerter(), clock)
	r3.Restore(snap)
	r3.mu.Lock()
	rec3 := r3.agents["a1"]
	f3, looping3, back3, conds3 := rec3.Failures, rec3.CrashLooping, rec3.BackoffUntil, len(rec3.Conditions)
	r3.mu.Unlock()
	if f3 != 0 || looping3 || !back3.IsZero() {
		t.Fatalf("observe must not inherit the restart ladder (failures=%d looping=%v backoff=%v)", f3, looping3, back3)
	}
	if conds3 == 0 {
		t.Fatal("conditions are observations and must restore in every mode")
	}
}

func TestConditionsReturnsACopy(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", readyObs())
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), newFakeClock())
	r.Tick(context.Background())
	got := r.Conditions("a1")
	got[0].Status = "tampered"
	fresh := r.Conditions("a1")
	if fresh[0].Status == "tampered" {
		t.Fatal("Conditions must return a copy")
	}
	if r.Conditions("nope") != nil {
		t.Fatal("unknown agent has no conditions")
	}
}

func TestDefaultBackendProvider(t *testing.T) {
	cases := map[string]string{
		"claude": "anthropic", "codex": "openai", "agy": "google",
		"deepseek": "deepseek", "bob": "", "litellm": "",
	}
	for backend, want := range cases {
		if got := DefaultBackendProvider(backend); got != want {
			t.Errorf("DefaultBackendProvider(%q) = %q, want %q", backend, got, want)
		}
	}
}

// waitBudget bounds how long the helpers below poll real wall-clock time for
// a detached restart goroutine to settle. It is intentionally generous: the
// condition each helper polls is a real, observed state transition
// (restartInFlight clearing, restartCount advancing) rather than a fixed
// sleep, so widening this budget only buys headroom against scheduler
// contention on coverage-instrumented CI runners — it cannot mask a restart
// that never happens, and it does not change what is being asserted (see
// TestDeadSessionRestartsOncePerBackoffNotPerTick's exact-count check).
const waitBudget = 20 * time.Second

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
