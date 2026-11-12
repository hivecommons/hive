package github

import (
	"os"
	"strings"
	"time"
)

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

// Machine-readable causes for polling, reported as webhooks.mode_reason_code
// so an operator (or a tool) can tell "this spoke cannot verify deliveries"
// from "GitHub is not sending any". They are also the reasons
// RecordWebhookDeliveryFailure takes for a rejected delivery.
const (
	// WebhookReasonNoSecret: GITHUB_WEBHOOK_SECRET is unset on this process,
	// so every delivery (direct or hub-relayed) is answered 503.
	WebhookReasonNoSecret = "no_secret"
	// WebhookReasonSignatureFailed: deliveries arrive but their
	// X-Hub-Signature-256 does not verify against the configured secret.
	WebhookReasonSignatureFailed = "signature_failed"
	// WebhookReasonDeliveriesRejected: deliveries arrive signed but are
	// rejected for another reason (the payload did not decode).
	WebhookReasonDeliveriesRejected = "deliveries_rejected"
	// WebhookReasonNoDeliveries: the secret is set but no verified delivery
	// is inside the health window (none ever, or none while GitHub was busy).
	WebhookReasonNoDeliveries = "no_deliveries"
)

// webhookReasonNoSecret*Text are the plain-words mode_reason for
// WebhookReasonNoSecret: with deliveries arriving, and with none yet.
const (
	webhookReasonNoSecretRejectedText = "no GITHUB_WEBHOOK_SECRET on this spoke; relayed deliveries are rejected with 503"
	webhookReasonNoSecretIdleText     = "no GITHUB_WEBHOOK_SECRET on this spoke; webhooks are off"
)

// WebhookSecretEnvVar names the GitHub App webhook HMAC secret a spoke
// verifies /api/webhook/github deliveries with.
const WebhookSecretEnvVar = "GITHUB_WEBHOOK_SECRET"

// WebhookSecretConfigured reports whether GITHUB_WEBHOOK_SECRET is set (non-blank)
// in this process. Only the yes/no ever leaves this function: the value, its
// length and any digest of it must never reach status, logs or health output.
func WebhookSecretConfigured() bool {
	return strings.TrimSpace(os.Getenv(WebhookSecretEnvVar)) != ""
}

// WebhookModeStatus is the mode block /api/status serves inside `webhooks`.
type WebhookModeStatus struct {
	Mode  WebhookMode `json:"mode"`
	Since *time.Time  `json:"since,omitempty"`
	// Reason is the plain-words cause while polling: the degradation reason
	// after a degradation, or why webhooks cannot work at all (no secret,
	// deliveries failing). Empty in webhook mode and while polling only
	// because no delivery has arrived yet.
	Reason string `json:"mode_reason,omitempty"`
	// ReasonCode is the machine-readable cause while polling: one of
	// WebhookReasonNoSecret, WebhookReasonSignatureFailed,
	// WebhookReasonDeliveriesRejected or WebhookReasonNoDeliveries. Empty in
	// webhook mode, and while polling with a verified delivery in the window
	// and no newer failure (about to engage webhook mode).
	ReasonCode string `json:"mode_reason_code,omitempty"`
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
	tr := sharedWebhookTracker().observeMode()
	return tr.Status, tr.Degraded, tr.Recovered
}

// WebhookModeSnapshot reports the cadence mode without advancing it.
func WebhookModeSnapshot() WebhookModeStatus {
	return sharedWebhookTracker().modeSnapshot()
}

// RecordGitHubActivity notes the newest `updated_at` a poll or reconcile
// listed. Activity newer than the newest delivery is the evidence that
// silent webhooks are degraded rather than merely idle.
func RecordGitHubActivity(updatedAt time.Time) {
	if updatedAt.IsZero() {
		return
	}
	t := sharedWebhookTracker()
	t.mu.Lock()
	if updatedAt.After(t.lastActivity) {
		t.lastActivity = updatedAt
	}
	t.mu.Unlock()
}

// RecordWebhookDeliveryFailure notes a delivery that reached the receiver
// (directly or via the hub relay) but could not be applied, with why:
// WebhookReasonNoSecret (no secret to verify with), WebhookReasonSignatureFailed
// or WebhookReasonDeliveriesRejected (payload did not decode). Failures newer
// than the newest good delivery degrade the cadence to polling. Each call
// costs one counter bump in a fixed per-minute ring, never an allocation, so
// a flood of rejected deliveries cannot grow memory.
func RecordWebhookDeliveryFailure(reason string) {
	t := sharedWebhookTracker()
	t.mu.Lock()
	now := t.now()
	t.lastFailure = now
	t.lastFailureReason = reason
	t.rejected.add(now)
	t.mu.Unlock()
}

// annotateModeLocked fills the polling cause (mode_reason_code, and a
// plain-words mode_reason where the machine has none) on a mode status.
func (t *webhookTracker) annotateModeLocked(st *WebhookModeStatus, now time.Time) {
	if st.Mode != WebhookModePolling {
		return
	}
	healthy, latest := t.healthyLocked(now)
	recentFailure := !t.lastFailure.IsZero() && now.Sub(t.lastFailure) < t.window && t.lastFailure.After(latest)
	switch {
	case WebhookVerification() == WebhookVerificationNone:
		// A hosted spoke with a hub relay key verifies relays without the
		// secret (#17911), so only a spoke with neither has no_secret.
		st.ReasonCode = WebhookReasonNoSecret
		if st.Reason == "" {
			st.Reason = webhookReasonNoSecretIdleText
			if !t.lastFailure.IsZero() && t.lastFailureReason == WebhookReasonNoSecret {
				st.Reason = webhookReasonNoSecretRejectedText
			}
		}
	case recentFailure:
		st.ReasonCode = webhookFailureCode(t.lastFailureReason)
		if st.Reason == "" {
			st.Reason = WebhookDegradedReasonFailures
		}
	case !healthy:
		st.ReasonCode = WebhookReasonNoDeliveries
	}
}

// webhookFailureCode maps a recorded failure reason to the reported code; a
// failure recorded without a known reason reads as a rejected delivery.
func webhookFailureCode(reason string) string {
	switch reason {
	case WebhookReasonSignatureFailed, WebhookReasonRelaySignatureFailed, WebhookReasonRelayDeliveryMissing,
		WebhookReasonRelayStale, WebhookReasonRelayReplayed:
		return reason
	}
	return WebhookReasonDeliveriesRejected
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
	tr := t.mode.observe(t.modeInputLocked(now), t.window, now)
	t.annotateModeLocked(&tr.Status, now)
	return tr
}

func (t *webhookTracker) modeSnapshot() WebhookModeStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	st := t.mode.status(t.window, now)
	t.annotateModeLocked(&st, now)
	return st
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
