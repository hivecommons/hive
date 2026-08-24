package watchdog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
func (f *fakeFleet) waitRestart(t *testing.T) {
	t.Helper()
	select {
	case <-f.restartDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a restart to complete")
	}
}

// fakeAlerter records dashboard system alerts.
type fakeAlerter struct {
	mu      sync.Mutex
	alerts  map[string]string
	cleared []string
}

func newFakeAlerter() *fakeAlerter {
	return &fakeAlerter{alerts: make(map[string]string)}
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
func fastSettings() Settings {
	s := DefaultSettings()
	s.ProbeInterval = 0 // every Tick sweeps; interval gating has its own test
	s.RestartTimeout = 50 * time.Millisecond
	return s
}

func deadObs() Observation {
	return Observation{
		Backend:       "claude",
		SessionExists: true,
		Pane:          "agent@spoke:~$",
		LastChange:    time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), // an hour stale
	}
}

func readyObs() Observation {
	return Observation{
		Backend:       "claude",
		SessionExists: true,
		Pane:          "❯",
		HasCLIMarker:  true,
		LastChange:    time.Date(2026, 8, 23, 11, 59, 0, 0, time.UTC),
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
	fleet.waitRestart(t)
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
	fleet.waitRestart(t)
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
	fleet.waitRestart(t)
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
		fleet.waitRestart(t)
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
		fleet.waitRestart(t)
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
	fleet.waitRestart(t)
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
	fleet.waitRestart(t)
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
	s.Enabled = false
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
	fleet.waitRestart(t)
	r.mu.Lock()
	failures := r.agents["a1"].Failures
	r.mu.Unlock()
	if failures != 1 {
		t.Fatalf("failed restart must still count toward the crash loop, got %d", failures)
	}
}

func TestSnapshotRestoreRoundtrip(t *testing.T) {
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", deadObs())
	clock := newFakeClock()
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r.Tick(context.Background())
	fleet.waitRestart(t)

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

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
