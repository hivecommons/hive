package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Fleet is the reconciler's window onto the agent manager. Implemented by
// pkg/agent's watchdog adapter so every observation and action reuses the
// manager's existing pane-capture, restart, and pause machinery — the
// watchdog never grows a second uncoordinated control loop.
type Fleet interface {
	// AgentNames lists the agents to reconcile.
	AgentNames() []string
	// Observe gathers one agent's observed truth. An error means the agent
	// could not be observed at all (removed mid-tick).
	Observe(name string) (Observation, error)
	// IsPaused reports the operator/escalation pause state; paused agents
	// are observed but never acted on.
	IsPaused(name string) bool
	// Restart kills and relaunches the agent's session. It must honor ctx as
	// far as the underlying machinery allows; the reconciler additionally
	// hard-bounds the call and never blocks on it.
	Restart(ctx context.Context, name string) error
	// Pause pauses the agent, recording the trigger/reason (crash-loop
	// escalation).
	Pause(name, trigger, reason string) error
	// LastProduction returns the newest production evidence timestamp for
	// the agent (state-file mtimes, pane activity). ok=false means no
	// evidence source is readable — reported as Producing=Unknown, never
	// silently as healthy or as failed.
	LastProduction(name string) (time.Time, bool)
	// QueuedWork reports how much actionable work is waiting for this agent,
	// and whether the count is known at all. An agent producing nothing while
	// nothing is queued is CORRECT, not unhealthy, so Producing=False requires
	// queued work — the hub's rule at agent_inactivity.go. known=false means
	// the queue could not be read, which is reported as Unknown rather than
	// assumed empty (which would silence a real fault) or assumed full (which
	// would manufacture one).
	QueuedWork(name string) (count int, known bool)
	// SetConditions publishes the agent's observed conditions so the
	// dashboard renders truth instead of the state echo.
	SetConditions(name string, conds []Condition)
}

// AuthStatus is the tri-state outcome of one provider credential probe.
type AuthStatus string

const (
	AuthOK      AuthStatus = "ok"
	AuthFailed  AuthStatus = "failed"
	AuthUnknown AuthStatus = "unknown"
)

// AuthProbe verifies one provider's credential source independent of the CLI
// (RFC #4665: a dead refresh token behind a live-looking pane). Implemented
// by adapting the rotation package's provider probers — when the #4645
// OAuth/JSON-RPC probe rewrite lands, it plugs in through this same seam.
type AuthProbe interface {
	// Provider is the provider key this probe answers for (e.g. "anthropic").
	Provider() string
	// ProbeAuth returns the credential verdict. It must be bounded by ctx.
	ProbeAuth(ctx context.Context) (AuthStatus, string)
}

// Alerter is the escalation surface: the dashboard's system-alert banner plus
// the durable audit log. Alerts are the transient "look at this now" channel;
// the audit log is the durable record an operator reconstructs a decision from
// weeks later, which a banner and a log line cannot do.
type Alerter interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlert(id string)
	// AuditLog appends a durable entry (dashboard.Server.AuditLog):
	// /data/audit.jsonl, 90-day retention, with a per-agent field.
	AuditLog(user, action, detail, agent string)
}

// auditUser is the actor recorded for every watchdog-initiated audit entry.
//
// It is deliberately NOT added to dashboard.auditPseudoUsers. That map does
// not gate what is written to the log — it gates the lastAction map, the
// "did a REAL person do something" signal the heartbeat reports hub-ward.
// The watchdog is a real actor whose restarts and pauses must be auditable,
// but it is not a person, and its actions must never be mistaken for human
// engagement. Writing under a name absent from that map achieves exactly
// that: the entry is recorded, and it does not count as user activity.
const auditUser = "watchdog"

// Audit action values. Actions the watchdog TAKES are named plainly; actions
// it only WOULD have taken in observe mode carry the "-observed" suffix, so
// the distinction lives in the action field itself and survives any reader
// that does not parse detail text. A reader must never mistake "would have
// paused" for "paused".
const (
	auditActionRestart      = "watchdog-restart"
	auditActionPause        = "watchdog-crashloop-pause"
	auditActionHealthyReset = "watchdog-healthy-reset"
	auditActionGiveUp       = "watchdog-giveup"
	// observedSuffix marks an action the watchdog declined to take because it
	// is running in observe mode.
	observedSuffix = "-observed"
)

// auditActionFor returns the action name for a decision, marking it not-taken
// when the watchdog is only observing.
//
// It takes `acting` rather than reading the settings itself. Every caller is
// inside a *Locked helper that already holds r.mu and has already resolved
// MayAct(); reaching back for a locked snapshot from there self-deadlocks on a
// non-reentrant sync.Mutex — the goroutine blocks waiting for the lock it is
// itself holding, wedging the reconciler on a live hive, not just in test.
func auditActionFor(base string, acting bool) string {
	if acting {
		return base
	}
	return base + observedSuffix
}

// audit writes one durable audit entry, if an audit surface exists. Only
// ACTIONS and transitions are recorded — never per-sweep observations: the
// in-memory ring is 500 entries, so sweep noise would evict the restarts and
// pauses that matter within minutes.
//
// detail carries the decision's reasoning (pane class, failure count, backoff
// step) and never pane content, tokens, or credential material: the pane is
// where login screens and secrets render, and the audit log is long-lived and
// operator-readable.
func (r *Reconciler) audit(action, detail, agent string) {
	if r.alerter == nil {
		return
	}
	r.alerter.AuditLog(auditUser, action, detail, agent)
}

// Alert/trigger identity strings. Named so the dashboard, audit log, and
// tests agree on the exact values.
const (
	// CrashLoopTrigger is the PausedTrigger stamped on a crash-loop pause.
	CrashLoopTrigger = "watchdog-crashloop"
	// alertPrefix namespaces every watchdog system alert id.
	alertPrefix = "watchdog-"

	severityError   = "error"
	severityWarning = "warning"

	// minQueuedForNotProducing is how much actionable work must be waiting
	// before an agent producing nothing is called a fault. Deliberately equal
	// to the hub's inactiveAgentMinQueued (agent_inactivity.go): a hive with
	// an empty queue is SUPPOSED to have idle agents, and one waiting item is
	// enough to make "and nothing is happening" wrong.
	minQueuedForNotProducing = 1
)

// agentRecord is the reconciler's per-agent memory. Guarded by Reconciler.mu.
type agentRecord struct {
	// Failures counts consecutive dead classifications that led (or would
	// lead) to a restart — the CrashLoopBackOff counter.
	Failures int
	// BackoffUntil gates the next restart attempt.
	BackoffUntil time.Time
	// HealthySince is when the agent was first observed continuously ready;
	// zero while unhealthy. Failures reset after HealthyReset of this.
	HealthySince time.Time
	// CrashLooping latches once the crash-loop cap escalated, so the pause +
	// alert fire once, not every tick.
	CrashLooping bool
	// LastClass is the previous liveness verdict, for transition logging.
	LastClass PaneClass
	// restartInFlight is true while a restart goroutine is running; the
	// reconciler never stacks a second restart on a wedged one.
	restartInFlight bool
	// restartStartedAt dates the in-flight restart so a wedge past the hard
	// timeout is alerted on (the control-plane guard for failure mode 4).
	restartStartedAt time.Time
	// wedgeAlerted latches the wedged-restart alert.
	wedgeAlerted bool
	// Conditions is the agent's published condition set.
	Conditions []Condition
}

// PersistedAgent is the durable subset of agentRecord, stored in the
// dashboard state file so backoff/crash-loop progress survives pod restarts
// (RFC open question 2).
type PersistedAgent struct {
	Failures     int         `json:"failures,omitempty"`
	BackoffUntil *time.Time  `json:"backoff_until,omitempty"`
	HealthySince *time.Time  `json:"healthy_since,omitempty"`
	CrashLooping bool        `json:"crash_looping,omitempty"`
	Conditions   []Condition `json:"conditions,omitempty"`
}

// Reconciler is the watchdog control loop. One instance per hive process; Tick is
// called from the governor eval tick and self-gates to ProbeInterval.
type Reconciler struct {
	settings Settings
	fleet    Fleet
	alerter  Alerter
	logger   *slog.Logger
	now      func() time.Time

	// authProbes maps provider key → probe. Nil/empty disables auth probing
	// for providers without one (verdict: Unknown, never fabricated).
	authProbes map[string]AuthProbe
	// backendProvider maps a backend name to its provider key.
	backendProvider func(backend string) string

	mu       sync.Mutex
	agents   map[string]*agentRecord
	lastTick time.Time
	// restarted accumulates agents whose restart COMPLETED since the last
	// TakeRestarted call, so the eval loop can resume-kick them exactly as it
	// resume-kicks the manager's crash-recovery restarts. Without this, an
	// agent the watchdog revived would sit idle until its next cadence slot —
	// the work it was interrupted mid-task would not resume.
	restarted []string
}

// TakeRestarted returns and clears the agents the watchdog has successfully
// restarted since the previous call. The caller feeds them to the same
// governor-gated resume-kick path that crash-recovery restarts use, so a
// watchdog restart and a crash-loop restart are indistinguishable downstream.
func (r *Reconciler) TakeRestarted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.restarted) == 0 {
		return nil
	}
	out := r.restarted
	r.restarted = nil
	return out
}

// Option customizes a Reconciler.
type Option func(*Reconciler)

// WithClock injects a fake clock for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Reconciler) { r.now = now }
}

// WithAuthProbes registers per-provider credential probes.
func WithAuthProbes(probes map[string]AuthProbe) Option {
	return func(r *Reconciler) { r.authProbes = probes }
}

// WithBackendProvider injects the backend→provider mapping.
func WithBackendProvider(f func(string) string) Option {
	return func(r *Reconciler) { r.backendProvider = f }
}

// New builds a Reconciler. alerter may be nil (alerts go to the journal via
// logger only); logger must not be nil.
func New(settings Settings, fleet Fleet, alerter Alerter, logger *slog.Logger, opts ...Option) *Reconciler {
	r := &Reconciler{
		settings:        settings,
		fleet:           fleet,
		alerter:         alerter,
		logger:          logger,
		now:             time.Now,
		agents:          make(map[string]*agentRecord),
		backendProvider: DefaultBackendProvider,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// DefaultBackendProvider maps agent backends to the provider keys the
// rotation package's probers answer for.
func DefaultBackendProvider(backend string) string {
	switch backend {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	case "agy":
		return "google"
	case "deepseek":
		return "deepseek"
	}
	return ""
}

// Enabled reports whether the reconciler runs at all (observe still runs).
func (r *Reconciler) Enabled() bool { return r.snapshotSettings().Enabled() }

// snapshotSettings returns the current settings under the lock, for the paths
// that read them outside a *Locked helper. Settings are swappable at runtime
// (SetSettings), so they must never be read unsynchronized.
func (r *Reconciler) snapshotSettings() Settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

// Mode reports the reconciler's current authority level.
func (r *Reconciler) Mode() Mode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings.Mode
}

// SetSettings swaps the resolved settings live, so an operator changing the
// mode from the dashboard takes effect on the next sweep rather than at the
// next pod restart.
//
// Dropping from heal to a mode that cannot act clears the in-memory ladder:
// the failure counts and backoff deadlines describe restarts this reconciler
// is no longer performing, and keeping them would mean that re-enabling heal
// resumed part-way up a ladder built while it had no authority — including
// escalating straight to a pause. Restart bookkeeping already in flight is
// left alone; it settles on its own goroutine.
func (r *Reconciler) SetSettings(s Settings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wasActing := r.settings.MayAct()
	r.settings = s
	if wasActing && !s.MayAct() {
		for _, rec := range r.agents {
			rec.Failures = 0
			rec.CrashLooping = false
			rec.BackoffUntil = time.Time{}
		}
	}
}

// Conditions returns a copy of the agent's current condition set.
func (r *Reconciler) Conditions(name string) []Condition {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.agents[name]
	if !ok {
		return nil
	}
	out := make([]Condition, len(rec.Conditions))
	copy(out, rec.Conditions)
	return out
}

// Snapshot exports the durable per-agent state for the dashboard state file.
func (r *Reconciler) Snapshot() map[string]PersistedAgent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]PersistedAgent, len(r.agents))
	for name, rec := range r.agents {
		p := PersistedAgent{
			Failures:     rec.Failures,
			CrashLooping: rec.CrashLooping,
		}
		if !rec.BackoffUntil.IsZero() {
			t := rec.BackoffUntil
			p.BackoffUntil = &t
		}
		if !rec.HealthySince.IsZero() {
			t := rec.HealthySince
			p.HealthySince = &t
		}
		if len(rec.Conditions) > 0 {
			p.Conditions = make([]Condition, len(rec.Conditions))
			copy(p.Conditions, rec.Conditions)
		}
		out[name] = p
	}
	return out
}

// Restore seeds per-agent state from a persisted snapshot, so a pod restart
// neither forgets a crash-loop nor re-runs a backoff ladder from the top.
//
// The restart ladder is restored ONLY when this reconciler may act. A hive that
// ran in heal, escalated an agent, then moved to observe would otherwise come
// back latched into a crash-loop it has no authority to have caused and cannot
// clear by restarting — and would resume mid-ladder if heal were re-enabled.
// Conditions are always restored: they are observations, valid in every mode.
func (r *Reconciler) Restore(saved map[string]PersistedAgent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acting := r.settings.MayAct()
	for name, p := range saved {
		rec := r.recordLocked(name)
		if acting {
			rec.Failures = p.Failures
			rec.CrashLooping = p.CrashLooping
			if p.BackoffUntil != nil {
				rec.BackoffUntil = *p.BackoffUntil
			}
		}
		if p.HealthySince != nil {
			rec.HealthySince = *p.HealthySince
		}
		if len(p.Conditions) > 0 {
			rec.Conditions = make([]Condition, len(p.Conditions))
			copy(rec.Conditions, p.Conditions)
		}
	}
}

func (r *Reconciler) recordLocked(name string) *agentRecord {
	rec, ok := r.agents[name]
	if !ok {
		rec = &agentRecord{}
		r.agents[name] = rec
	}
	return rec
}

// Tick runs one reconciliation sweep. It is synchronous and bounded: every
// probe carries a deadline and every restart runs detached under the hard
// RestartTimeout, so the governor tick that hosts it can never wedge on a
// stuck agent (the RFC's control-plane guard). Calls inside ProbeInterval of
// the previous sweep are no-ops.
func (r *Reconciler) Tick(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	if !r.settings.Enabled() {
		r.mu.Unlock()
		return
	}
	if !r.lastTick.IsZero() && now.Sub(r.lastTick) < r.settings.ProbeInterval {
		r.mu.Unlock()
		return
	}
	r.lastTick = now
	r.mu.Unlock()

	authCache := make(map[string]authVerdict)
	for _, name := range r.fleet.AgentNames() {
		r.reconcileAgent(ctx, name, now, authCache)
	}
}

type authVerdict struct {
	status AuthStatus
	detail string
}

func (r *Reconciler) reconcileAgent(ctx context.Context, name string, now time.Time, authCache map[string]authVerdict) {
	obs, err := r.fleet.Observe(name)
	if err != nil {
		r.logger.Warn("watchdog: agent unobservable", "agent", name, "error", err)
		return
	}
	cls := Classify(obs, now, r.snapshotSettings())
	paused := r.fleet.IsPaused(name)

	r.mu.Lock()
	rec := r.recordLocked(name)
	if cls.Class != rec.LastClass {
		r.logger.Info("watchdog: liveness transition",
			"agent", name, "from", string(rec.LastClass), "to", string(cls.Class), "reason", cls.Reason)
	}
	rec.LastClass = cls.Class

	r.updateReadyConditionLocked(rec, cls, now)
	r.updateHealthyWindowLocked(name, rec, cls, now)
	r.checkWedgedRestartLocked(name, rec, now)

	var act func()
	switch {
	case paused:
		// Operator intent outranks the watchdog: observe, publish, never act.
	case cls.Class.Dead():
		act = r.planRestartLocked(name, rec, cls, now)
	case cls.Class == ClassAuthRequired:
		// Handled below via the auth condition; the pane already IS the
		// evidence. No restart: see PaneClass.Dead.
	}
	r.mu.Unlock()

	r.reconcileAuth(ctx, name, obs, cls, now, authCache)
	r.reconcileReadiness(name, obs, now)

	if act != nil {
		act()
	}

	r.publish(name)
}

// updateReadyConditionLocked maps the liveness class onto the Ready condition.
func (r *Reconciler) updateReadyConditionLocked(rec *agentRecord, cls Classification, now time.Time) {
	status := ConditionUnknown
	switch {
	case cls.Class == ClassReady:
		status = ConditionTrue
	case cls.Class.Dead() || cls.Class == ClassAuthRequired:
		status = ConditionFalse
	}
	rec.Conditions = setCondition(rec.Conditions, Condition{
		Type:    ConditionReady,
		Status:  status,
		Reason:  string(cls.Class),
		Message: cls.Reason,
	}, now)
}

// updateHealthyWindowLocked implements the healthyReset half of the
// CrashLoopBackOff analog: Failures clear only after a continuous ready
// window, so a flapping agent cannot launder its counter with one good probe.
func (r *Reconciler) updateHealthyWindowLocked(name string, rec *agentRecord, cls Classification, now time.Time) {
	if cls.Class != ClassReady {
		rec.HealthySince = time.Time{}
		return
	}
	if rec.HealthySince.IsZero() {
		rec.HealthySince = now
		return
	}
	if now.Sub(rec.HealthySince) >= r.settings.HealthyReset && (rec.Failures > 0 || rec.CrashLooping) {
		r.logger.Info("watchdog: healthy window elapsed, failure counter reset",
			"agent", name, "failures_were", rec.Failures)
		r.audit(auditActionHealthyReset,
			fmt.Sprintf("continuously ready for %v; failure counter cleared (was %d, crash_looping=%v)",
				r.settings.HealthyReset, rec.Failures, rec.CrashLooping), name)
		rec.Failures = 0
		rec.CrashLooping = false
		rec.BackoffUntil = time.Time{}
		if r.alerter != nil {
			r.alerter.ClearSystemAlert(crashLoopAlertID(name))
		}
	}
}

// checkWedgedRestartLocked alerts when a detached restart has run past the
// hard timeout — the observable form of RFC failure mode 4 (wedged kick API).
func (r *Reconciler) checkWedgedRestartLocked(name string, rec *agentRecord, now time.Time) {
	if !rec.restartInFlight || rec.wedgeAlerted {
		return
	}
	if now.Sub(rec.restartStartedAt) < r.settings.RestartTimeout {
		return
	}
	rec.wedgeAlerted = true
	msg := fmt.Sprintf("Watchdog restart of agent %q has been running for over %v — the restart path may be wedged; no further restarts will stack on it.", name, r.settings.RestartTimeout)
	r.logger.Error("watchdog: restart wedged", "agent", name, "timeout", r.settings.RestartTimeout)
	if r.alerter != nil {
		r.alerter.AddSystemAlert(wedgeAlertID(name), severityError, msg)
	}
}

// planRestartLocked decides the reconciliation action for a dead agent and
// returns the side-effecting closure to run OUTSIDE the lock (nil = no-op).
func (r *Reconciler) planRestartLocked(name string, rec *agentRecord, cls Classification, now time.Time) func() {
	if rec.restartInFlight {
		return nil
	}
	if rec.CrashLooping {
		return nil
	}
	acting := r.settings.MayAct()

	if rec.Failures >= r.settings.CrashLoopAfter {
		reason := fmt.Sprintf("crash loop: %d consecutive failed restarts (last: %s — %s)", rec.Failures, cls.Class, cls.Reason)
		detail := fmt.Sprintf("class=%s failures=%d cap=%d reason=%s",
			cls.Class, rec.Failures, r.settings.CrashLoopAfter, cls.Reason)

		if !acting {
			// Observe mode: report the terminal decision, take none of it. The
			// CrashLooping latch is deliberately NOT set — latching would
			// suppress this evidence on every later sweep, and observe mode
			// exists to keep producing it.
			r.logger.Warn("watchdog: would escalate to pause (observe mode; no action taken)",
				"agent", name, "failures", rec.Failures, "class", string(cls.Class), "mode", string(r.settings.Mode))
			r.audit(auditActionFor(auditActionPause, acting), "would pause: "+detail, name)
			return nil
		}

		rec.CrashLooping = true
		r.logger.Error("watchdog: crash loop, escalating to pause",
			"agent", name, "failures", rec.Failures, "class", string(cls.Class))
		if r.alerter != nil {
			r.alerter.AddSystemAlert(crashLoopAlertID(name), severityError,
				fmt.Sprintf("Agent %q is crash-looping (%d failed restarts) and has been paused by the watchdog. Investigate, then resume it from the dashboard.", name, rec.Failures))
		}
		r.audit(auditActionPause, "paused: "+detail, name)
		// The give-up is the terminal state an operator must be able to
		// reconstruct, so it is recorded as its own entry rather than being
		// inferred from the pause.
		r.audit(auditActionGiveUp,
			fmt.Sprintf("no further restarts will be attempted; %s", detail), name)
		return func() {
			if err := r.fleet.Pause(name, CrashLoopTrigger, reason); err != nil {
				r.logger.Error("watchdog: crash-loop pause failed", "agent", name, "error", err)
			}
		}
	}
	if now.Before(rec.BackoffUntil) {
		return nil
	}

	if !acting {
		// Observe mode: no counter advance, no backoff arming, no restart —
		// the record stays pristine so promoting to heal starts from a clean
		// ladder rather than one already part-way up.
		r.logger.Warn("watchdog: would restart dead agent (observe mode; no action taken)",
			"agent", name, "class", string(cls.Class), "reason", cls.Reason, "mode", string(r.settings.Mode))
		r.audit(auditActionFor(auditActionRestart, acting),
			fmt.Sprintf("would restart: class=%s reason=%s", cls.Class, cls.Reason), name)
		return nil
	}

	rec.Failures++
	rec.BackoffUntil = now.Add(r.settings.backoffFor(rec.Failures))
	rec.restartInFlight = true
	rec.restartStartedAt = now
	rec.wedgeAlerted = false
	failures := rec.Failures
	backoff := r.settings.backoffFor(failures)
	r.logger.Warn("watchdog: restarting dead agent",
		"agent", name, "class", string(cls.Class), "reason", cls.Reason,
		"failure", failures, "next_backoff", r.settings.backoffFor(failures+1))
	r.audit(auditActionRestart,
		fmt.Sprintf("class=%s reason=%s failure=%d/%d backoff=%v",
			cls.Class, cls.Reason, failures, r.settings.CrashLoopAfter, backoff), name)
	timeout := r.settings.RestartTimeout
	return func() { r.restartDetached(name, timeout) }
}

// restartDetached runs the restart under the hard timeout without ever
// blocking the reconciler. If the underlying restart ignores its context and
// wedges, restartInFlight stays true — the next tick alerts (see
// checkWedgedRestartLocked) and no second restart stacks on it. At most one
// goroutine exists per agent.
func (r *Reconciler) restartDetached(name string, timeout time.Duration) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err := r.fleet.Restart(ctx, name)
		r.mu.Lock()
		rec := r.recordLocked(name)
		rec.restartInFlight = false
		if err == nil {
			r.restarted = append(r.restarted, name)
		}
		r.mu.Unlock()
		if r.alerter != nil {
			r.alerter.ClearSystemAlert(wedgeAlertID(name))
		}
		if err != nil {
			r.logger.Error("watchdog: restart failed", "agent", name, "error", err)
			return
		}
		r.logger.Info("watchdog: restart completed", "agent", name)
	}()
}

// reconcileAuth updates the Authenticated condition from (a) the pane's own
// evidence and (b) the provider credential probe, cached per provider per
// sweep so N agents on one provider cost one probe.
func (r *Reconciler) reconcileAuth(ctx context.Context, name string, obs Observation, cls Classification, now time.Time, cache map[string]authVerdict) {
	settings := r.snapshotSettings()
	status := ConditionUnknown
	reason := "NoProbe"
	message := "no credential probe available for backend " + obs.Backend

	// healable: the pane shows login chrome, but the backend credential is
	// demonstrably still usable, so a CLI restart recovers it and no human is
	// needed. Hoisted to a variable because two later steps must honour it —
	// the CredentialPresent upgrade must not overwrite the verdict, and a
	// standing operator alert must be cleared.
	healable := cls.Class == ClassAuthRequired && obs.CredentialProven

	if healable {
		// A login screen over a credential that demonstrably still works is
		// the token-restart heal's case, not an operator's — the same division
		// of labour #5291 established for the governor's login detector, which
		// was paused for exactly this misreading. The routine cause is a
		// Claude access token that aged out under a session older than its
		// 8h life: the CLI pins the token it read at startup, so the pane
		// 401s while the refresh grant on disk is still good for weeks, and
		// the next restart mints a fresh token with no human involved.
		//
		// Unknown, not True: the credential is proven, the SESSION is not.
		// Claiming Authenticated=true here would vouch for a pane that is
		// visibly not working. Unknown clears the operator alert (there is
		// nothing for a human to do) without asserting health the reconciler
		// cannot see.
		status = ConditionUnknown
		reason = "LoginPromptWithUsableCredential"
		message = cls.Reason + "; the backend credential is still usable, so a CLI restart recovers this without an operator login"
	} else if cls.Class == ClassAuthRequired {
		status = ConditionFalse
		reason = "PaneShowsLogin"
		message = cls.Reason
	} else if obs.AuthKnown && !obs.AuthAvailable {
		// The manager's owner-aware per-agent credential probe (#4619/#4641)
		// says the credential file is definitively absent for an agent whose
		// backend requires interactive auth.
		status = ConditionFalse
		reason = "CredentialMissing"
		message = "per-agent credential probe reports no credential for backend " + obs.Backend
	} else if settings.AuthProbe {
		provider := r.backendProvider(obs.Backend)
		if probe, ok := r.authProbes[provider]; ok && provider != "" {
			v, cached := cache[provider]
			if !cached {
				st, detail := probe.ProbeAuth(ctx)
				v = authVerdict{status: st, detail: detail}
				cache[provider] = v
			}
			switch v.status {
			case AuthOK:
				status, reason, message = ConditionTrue, "ProbeOK", v.detail
			case AuthFailed:
				status, reason, message = ConditionFalse, "ProbeFailed", v.detail
			default:
				status, reason, message = ConditionUnknown, "ProbeInconclusive", v.detail
			}
		}
	}

	// With no provider verdict, the per-agent credential-file probe's
	// positive answer still beats "unknown" — EXCEPT over a healable pane,
	// whose Unknown is a deliberate verdict rather than an absent one.
	// Upgrading it to True there would report Authenticated on an agent
	// visibly sitting at a login prompt.
	if status == ConditionUnknown && !healable && obs.AuthKnown && obs.AuthAvailable {
		status, reason = ConditionTrue, "CredentialPresent"
		message = "per-agent credential probe reports a live credential for backend " + obs.Backend
	}

	r.mu.Lock()
	rec := r.recordLocked(name)
	prev, _ := FindCondition(rec.Conditions, ConditionAuthenticated)
	rec.Conditions = setCondition(rec.Conditions, Condition{
		Type: ConditionAuthenticated, Status: status, Reason: reason, Message: message,
	}, now)
	r.mu.Unlock()

	if status == ConditionFalse && prev.Status != ConditionFalse {
		// Credential re-seed is an operator action (RFC): alert loudly, never
		// restart into the dead credential.
		r.logger.Error("watchdog: agent credential failure", "agent", name, "backend", obs.Backend, "reason", reason, "detail", message)
		if r.alerter != nil {
			r.alerter.AddSystemAlert(authAlertID(name), severityError,
				fmt.Sprintf("Agent %q needs re-authentication (%s): %s", name, reason, message))
		}
	}
	// Clear on a healable pane as well as on a True verdict. Without this the
	// alert is sticky in exactly the case it is most wrong: an operator who
	// re-authenticates leaves login chrome on the pane, the verdict moves
	// False -> Unknown rather than False -> True, and the "needs
	// re-authentication" banner would stand over a credential that no longer
	// needs anything from them.
	if r.alerter != nil && prev.Status == ConditionFalse && (status == ConditionTrue || healable) {
		r.alerter.ClearSystemAlert(authAlertID(name))
	}
}

// reconcileReadiness updates the Producing condition from production
// evidence. A readiness failure alone NEVER pauses or restarts (design
// decision on RFC open question 3): it publishes Producing=False and a
// warning alert, degrading the agent's standing rather than its life.
func (r *Reconciler) reconcileReadiness(name string, obs Observation, now time.Time) {
	settings := r.snapshotSettings()
	last, ok := r.fleet.LastProduction(name)
	status := ConditionUnknown
	reason := "NoEvidence"
	message := "no production evidence source is readable for this agent"
	if obs.ProviderErrorClass != "" {
		status = ConditionFalse
		reason = "ProviderInferenceError"
		message = fmt.Sprintf("blocked: inference (%s)", obs.ProviderErrorClass)
		if line := strings.TrimSpace(obs.ProviderErrorLine); line != "" {
			message += ": " + line
		}
	} else if ok {
		age := now.Sub(last)
		switch {
		case age < settings.NoProductionFor:
			status = ConditionTrue
			reason = "RecentProduction"
			message = fmt.Sprintf("last production evidence %v ago", age.Round(time.Minute))
		default:
			// Silence is only a fault when there is work to be silent about.
			// An agent with an empty queue producing nothing is behaving
			// correctly, and reporting it would light this condition on every
			// healthy quiet hive — the failure mode the hub's inactivity rule
			// exists to prevent (agent_inactivity.go: a facet which lights up
			// constantly gets ignored). Same threshold as the hub's
			// inactiveAgentMinQueued: one waiting item is enough to make "and
			// nothing is happening" wrong.
			queued, queueKnown := r.fleet.QueuedWork(name)
			switch {
			case !queueKnown:
				status = ConditionUnknown
				reason = "QueueUnknown"
				message = fmt.Sprintf("no production for %v, but the work queue could not be read — not called a fault without knowing there was work to do", age.Round(time.Minute))
			case queued < minQueuedForNotProducing:
				status = ConditionTrue
				reason = "IdleNoWorkQueued"
				message = fmt.Sprintf("no production for %v, and nothing is queued — an agent with no work to do is correct, not unhealthy", age.Round(time.Minute))
			default:
				status = ConditionFalse
				reason = "NoRecentProduction"
				message = fmt.Sprintf("no production evidence for %v (threshold %v) while %d item(s) are queued", age.Round(time.Minute), settings.NoProductionFor, queued)
			}
		}
	}

	r.mu.Lock()
	rec := r.recordLocked(name)
	prev, _ := FindCondition(rec.Conditions, ConditionProducing)
	rec.Conditions = setCondition(rec.Conditions, Condition{
		Type: ConditionProducing, Status: status, Reason: reason, Message: message,
	}, now)
	r.mu.Unlock()

	if status == ConditionFalse && prev.Status != ConditionFalse {
		r.logger.Warn("watchdog: agent not producing", "agent", name, "detail", message)
		if r.alerter != nil {
			r.alerter.AddSystemAlert(producingAlertID(name), severityWarning,
				fmt.Sprintf("Agent %q is alive but not producing: %s", name, message))
		}
	}
	if status == ConditionTrue && prev.Status == ConditionFalse && r.alerter != nil {
		r.alerter.ClearSystemAlert(producingAlertID(name))
	}
}

// publish pushes the agent's condition set to the fleet for the dashboard.
func (r *Reconciler) publish(name string) {
	r.fleet.SetConditions(name, r.Conditions(name))
}

func crashLoopAlertID(name string) string { return alertPrefix + "crashloop-" + name }
func wedgeAlertID(name string) string     { return alertPrefix + "wedged-restart-" + name }
func authAlertID(name string) string      { return alertPrefix + "auth-" + name }
func producingAlertID(name string) string { return alertPrefix + "producing-" + name }
