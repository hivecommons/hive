package github

import "time"

// Webhook-outage fallback to polling (hivecommons/hive#13381, ForgeState
// phase 4).
//
// The governor's reconcile cadence has two settings: governor.eval_interval_s
// (legacy polling) and governor.eval_interval_webhook_s (the stretched tick
// that is safe while verified deliveries carry the latency). Which one is in
// force is the webhook *mode* below. It is a small state machine layered on
// the raw health window in webhook_cache.go so that a quiet repository does
// not bounce the cadence every night and a real outage does not flap the
// cadence back as soon as a single delivery lands.

// WebhookMode names which cadence the governor's reconcile tick follows.
type WebhookMode string

const (
	// WebhookModeWebhook: deliveries are arriving; the base tick is
	// governor.eval_interval_webhook_s and webhook wakes drive early cycles.
	WebhookModeWebhook WebhookMode = "webhook"
	// WebhookModePolling: webhooks are absent or degraded; the base tick is
	// governor.eval_interval_s (the legacy polling cadence).
	WebhookModePolling WebhookMode = "polling"
)

// WebhookRecoveryHealthyWindows is the hysteresis on the way back: after a
// degradation the cadence returns to webhook mode only once deliveries have
// kept arriving for this many consecutive health windows (the first healthy
// observation counts as window one, so recovery needs
// (N-1) × window of uninterrupted health after it). The first time webhooks
// ever come up — at boot, before any degradation — there is no hysteresis.
const WebhookRecoveryHealthyWindows = 2

// webhookActivityClockSkew is the tolerance between a GitHub `updated_at`
// and the local time a delivery for that change was recorded. A reconcile
// that lists an item updated more than this long after the newest delivery
// proves GitHub was busy while webhooks were silent.
const webhookActivityClockSkew = 30 * time.Second

// Reasons recorded on a degradation, reported as webhooks.mode_reason.
const (
	WebhookDegradedReasonMissedActivity = "no deliveries while GitHub activity continued"
	WebhookDegradedReasonFailures       = "deliveries failing verification"
)

// WebhookModeStatus is the mode block /api/status serves inside `webhooks`.
type WebhookModeStatus struct {
	Mode  WebhookMode `json:"mode"`
	Since *time.Time  `json:"since,omitempty"`
	// Reason is set while polling after a degradation (empty at boot before
	// webhooks were ever healthy, and in webhook mode).
	Reason string `json:"mode_reason,omitempty"`
	// HealthyWindows counts the consecutive healthy windows observed while
	// polling after a degradation; recovery happens at
	// WebhookRecoveryHealthyWindows. Non-zero means "recovering".
	HealthyWindows int `json:"healthy_windows,omitempty"`
}

// webhookModeInput is one observation fed to the machine.
type webhookModeInput struct {
	// healthy: a verified delivery arrived within the health window.
	healthy bool
	// activity: a poll/reconcile listed an item GitHub updated after the
	// newest delivery (plus clock skew), i.e. a change webhooks missed.
	activity bool
	// failures: deliveries reached the receiver but failed (bad signature,
	// malformed payload) since the newest good delivery, inside the window.
	failures bool
}

// webhookModeMachine holds the cadence mode. Zero value starts polling.
type webhookModeMachine struct {
	mode         WebhookMode
	since        time.Time
	reason       string
	everDegraded bool
	// healthySince is the start of the current uninterrupted healthy streak
	// while polling; zero when not healthy.
	healthySince time.Time
}

// webhookModeTransition reports what one observation did.
type webhookModeTransition struct {
	Status    WebhookModeStatus
	Degraded  bool // webhook → polling this observation
	Recovered bool // polling → webhook this observation
}

func (m *webhookModeMachine) init(now time.Time) {
	if m.mode == "" {
		m.mode = WebhookModePolling
		m.since = now
	}
}

// observe advances the machine by one observation taken at now with the
// given health window.
func (m *webhookModeMachine) observe(in webhookModeInput, window time.Duration, now time.Time) webhookModeTransition {
	m.init(now)
	var tr webhookModeTransition
	switch m.mode {
	case WebhookModeWebhook:
		switch {
		case in.failures:
			m.degrade(WebhookDegradedReasonFailures, now)
			tr.Degraded = true
		case !in.healthy && in.activity:
			m.degrade(WebhookDegradedReasonMissedActivity, now)
			tr.Degraded = true
		}
	case WebhookModePolling:
		if !in.healthy || in.failures {
			m.healthySince = time.Time{}
			break
		}
		if m.healthySince.IsZero() {
			m.healthySince = now
		}
		if !m.everDegraded || m.healthyWindows(window, now) >= WebhookRecoveryHealthyWindows {
			m.mode = WebhookModeWebhook
			m.since = now
			m.reason = ""
			m.healthySince = time.Time{}
			tr.Recovered = true
		}
	}
	tr.Status = m.status(window, now)
	return tr
}

func (m *webhookModeMachine) degrade(reason string, now time.Time) {
	m.mode = WebhookModePolling
	m.since = now
	m.reason = reason
	m.everDegraded = true
	m.healthySince = time.Time{}
}

// healthyWindows is how many consecutive health windows the current streak
// spans: 1 at the first healthy observation, +1 per further window elapsed.
func (m *webhookModeMachine) healthyWindows(window time.Duration, now time.Time) int {
	if m.healthySince.IsZero() {
		return 0
	}
	if window <= 0 {
		window = DefaultWebhookHealthWindow
	}
	return 1 + int(now.Sub(m.healthySince)/window)
}

func (m *webhookModeMachine) status(window time.Duration, now time.Time) WebhookModeStatus {
	m.init(now)
	out := WebhookModeStatus{Mode: m.mode, Reason: m.reason}
	if !m.since.IsZero() {
		at := m.since.UTC()
		out.Since = &at
	}
	if m.mode == WebhookModePolling && m.everDegraded {
		out.HealthyWindows = m.healthyWindows(window, now)
	}
	return out
}

// ObserveWebhookMode advances the cadence mode by one observation (the
// governor calls it wherever it recomputes the eval interval) and reports the
// mode plus whether this observation degraded to polling or recovered to
// webhook mode, so the caller logs exactly once per transition.
func ObserveWebhookMode() (status WebhookModeStatus, degraded, recovered bool) {
	tr := sharedWebhookTracker.observeMode()
	return tr.Status, tr.Degraded, tr.Recovered
}

// WebhookModeSnapshot reports the cadence mode without advancing it.
func WebhookModeSnapshot() WebhookModeStatus {
	return sharedWebhookTracker.modeSnapshot()
}

// RecordGitHubActivity notes the newest `updated_at` a poll or reconcile
// listed. Activity newer than the newest delivery is the evidence that
// silent webhooks are degraded rather than merely idle.
func RecordGitHubActivity(updatedAt time.Time) {
	if updatedAt.IsZero() {
		return
	}
	t := sharedWebhookTracker
	t.mu.Lock()
	if updatedAt.After(t.lastActivity) {
		t.lastActivity = updatedAt
	}
	t.mu.Unlock()
}

// RecordWebhookDeliveryFailure notes a delivery that reached the receiver
// (directly or via the hub relay) but could not be applied — a signature
// that did not verify or a payload that did not decode. Failures newer than
// the newest good delivery degrade the cadence to polling.
func RecordWebhookDeliveryFailure() {
	t := sharedWebhookTracker
	t.mu.Lock()
	t.lastFailure = t.now()
	t.mu.Unlock()
}

func (t *webhookTracker) modeInputLocked(now time.Time) webhookModeInput {
	healthy, latest := t.healthyLocked(now)
	in := webhookModeInput{healthy: healthy}
	if !t.lastActivity.IsZero() && t.lastActivity.After(latest.Add(webhookActivityClockSkew)) {
		in.activity = true
	}
	if !t.lastFailure.IsZero() && now.Sub(t.lastFailure) < t.window && t.lastFailure.After(latest) {
		in.failures = true
	}
	return in
}

func (t *webhookTracker) observeMode() webhookModeTransition {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	return t.mode.observe(t.modeInputLocked(now), t.window, now)
}

func (t *webhookTracker) modeSnapshot() WebhookModeStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mode.status(t.window, t.now())
}

// newestEnumeratedUpdate is the newest `updated_at` across one repo's listed
// issues and PRs, the activity signal a reconcile pass feeds the mode machine.
func newestEnumeratedUpdate(issues []Issue, prLists ...[]PullRequest) time.Time {
	var newest time.Time
	for _, is := range issues {
		if is.UpdatedAt.After(newest) {
			newest = is.UpdatedAt
		}
	}
	for _, prs := range prLists {
		for _, pr := range prs {
			if pr.UpdatedAt.After(newest) {
				newest = pr.UpdatedAt
			}
		}
	}
	return newest
}
