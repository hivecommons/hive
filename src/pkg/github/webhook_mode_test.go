package github

import (
	"testing"
	"time"
)

// TestWebhookModeMachineTransitions walks the cadence mode through
// healthy → degraded → recovering → healthy (hivecommons/hive#13381): the
// first healthy window engages webhook mode at once, silence alone never
// degrades, silence during GitHub activity or failing deliveries does, and
// the way back needs WebhookRecoveryHealthyWindows consecutive healthy
// windows.
func TestWebhookModeMachineTransitions(t *testing.T) {
	const window = 10 * time.Minute
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	type step struct {
		name      string
		at        time.Duration
		in        webhookModeInput
		wantMode  WebhookMode
		degraded  bool
		recovered bool
		windows   int
		reason    string
	}
	steps := []step{
		{name: "boot: nothing yet", in: webhookModeInput{}, wantMode: WebhookModePolling},
		{name: "boot: activity but no deliveries stays polling", at: time.Minute, in: webhookModeInput{activity: true}, wantMode: WebhookModePolling},
		{name: "first delivery engages webhook mode without hysteresis", at: 2 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModeWebhook, recovered: true},
		{name: "steady healthy", at: 5 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModeWebhook},
		{name: "quiet repo: window lapsed, no activity, stays webhook", at: 20 * time.Minute, in: webhookModeInput{}, wantMode: WebhookModeWebhook},
		{name: "activity while silent degrades", at: 25 * time.Minute, in: webhookModeInput{activity: true}, wantMode: WebhookModePolling, degraded: true, reason: WebhookDegradedReasonMissedActivity},
		{name: "degraded is reported once", at: 26 * time.Minute, in: webhookModeInput{activity: true}, wantMode: WebhookModePolling, reason: WebhookDegradedReasonMissedActivity},
		{name: "one delivery: recovering, window 1 of 2", at: 30 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModePolling, windows: 1, reason: WebhookDegradedReasonMissedActivity},
		{name: "still inside the first window", at: 35 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModePolling, windows: 1, reason: WebhookDegradedReasonMissedActivity},
		{name: "a lapse resets the streak", at: 38 * time.Minute, in: webhookModeInput{}, wantMode: WebhookModePolling, reason: WebhookDegradedReasonMissedActivity},
		{name: "healthy again: streak restarts at window 1", at: 40 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModePolling, windows: 1, reason: WebhookDegradedReasonMissedActivity},
		{name: "second consecutive healthy window recovers", at: 50 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModeWebhook, recovered: true},
		{name: "delivery failures degrade even inside a healthy window", at: 52 * time.Minute, in: webhookModeInput{healthy: true, failures: true}, wantMode: WebhookModePolling, degraded: true, reason: WebhookDegradedReasonFailures},
		{name: "failures hold the streak at zero", at: 55 * time.Minute, in: webhookModeInput{healthy: true, failures: true}, wantMode: WebhookModePolling, reason: WebhookDegradedReasonFailures},
		{name: "clean deliveries start the streak", at: 60 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModePolling, windows: 1, reason: WebhookDegradedReasonFailures},
		{name: "recovered after the hysteresis", at: 70 * time.Minute, in: webhookModeInput{healthy: true}, wantMode: WebhookModeWebhook, recovered: true},
	}
	var m webhookModeMachine
	var lastSince time.Time
	for _, st := range steps {
		now := start.Add(st.at)
		tr := m.observe(st.in, window, now)
		if tr.Status.Mode != st.wantMode || tr.Degraded != st.degraded || tr.Recovered != st.recovered {
			t.Fatalf("%s: mode=%q degraded=%v recovered=%v, want %q/%v/%v", st.name, tr.Status.Mode, tr.Degraded, tr.Recovered, st.wantMode, st.degraded, st.recovered)
		}
		if tr.Status.HealthyWindows != st.windows {
			t.Fatalf("%s: healthy_windows = %d, want %d", st.name, tr.Status.HealthyWindows, st.windows)
		}
		if tr.Status.Reason != st.reason {
			t.Fatalf("%s: reason = %q, want %q", st.name, tr.Status.Reason, st.reason)
		}
		if tr.Status.Since == nil {
			t.Fatalf("%s: since must be set", st.name)
		}
		if tr.Degraded || tr.Recovered {
			if !tr.Status.Since.Equal(now) {
				t.Fatalf("%s: since = %v, want transition time %v", st.name, tr.Status.Since, now)
			}
			lastSince = now
		} else if !lastSince.IsZero() && !tr.Status.Since.Equal(lastSince) {
			t.Fatalf("%s: since moved to %v without a transition (was %v)", st.name, tr.Status.Since, lastSince)
		}
	}
}

// TestWebhookTrackerModeInputs covers how the tracker derives the machine's
// inputs: activity counts only when newer than the last delivery plus clock
// skew, and a failure counts only inside the window and after the last good
// delivery.
func TestWebhookTrackerModeInputs(t *testing.T) {
	clock := &webhookTestClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	SetWebhookHealthWindow(10 * time.Minute)

	status, _, recovered := ObserveWebhookMode()
	if status.Mode != WebhookModePolling || recovered {
		t.Fatalf("boot: %+v recovered=%v, want polling", status, recovered)
	}
	RecordWebhookEventForTest("org/repo")
	if status, _, recovered = ObserveWebhookMode(); status.Mode != WebhookModeWebhook || !recovered {
		t.Fatalf("first delivery: %+v recovered=%v, want webhook", status, recovered)
	}

	// An item updated just before its delivery is not "missed" activity.
	RecordGitHubActivity(clock.now.Add(-time.Second))
	clock.now = clock.now.Add(11 * time.Minute)
	if status, degraded, _ := ObserveWebhookMode(); status.Mode != WebhookModeWebhook || degraded {
		t.Fatalf("activity before the delivery: %+v degraded=%v, want webhook", status, degraded)
	}
	// Activity after the delivery (beyond skew) while silent is.
	RecordGitHubActivity(clock.now.Add(-5 * time.Minute))
	status, degraded, _ := ObserveWebhookMode()
	if status.Mode != WebhookModePolling || !degraded || status.Reason != WebhookDegradedReasonMissedActivity {
		t.Fatalf("missed activity: %+v degraded=%v", status, degraded)
	}
	if snap := WebhookHealthSnapshot(); snap.Mode != WebhookModePolling || snap.Since == nil || !snap.Since.Equal(clock.now) {
		t.Fatalf("snapshot mode block = %+v, want polling since %v", snap.WebhookModeStatus, clock.now)
	}

	// Recover: two consecutive healthy windows.
	RecordWebhookEventForTest("org/repo")
	if status, _, recovered = ObserveWebhookMode(); status.Mode != WebhookModePolling || status.HealthyWindows != 1 || recovered {
		t.Fatalf("recovering: %+v recovered=%v", status, recovered)
	}
	clock.now = clock.now.Add(9 * time.Minute)
	RecordWebhookEventForTest("org/repo")
	clock.now = clock.now.Add(2 * time.Minute)
	if status, _, recovered = ObserveWebhookMode(); status.Mode != WebhookModeWebhook || !recovered {
		t.Fatalf("recovered: %+v recovered=%v", status, recovered)
	}

	// A failure newer than the last good delivery degrades; one older than a
	// later good delivery does not.
	RecordWebhookDeliveryFailure()
	if status, degraded, _ = ObserveWebhookMode(); status.Mode != WebhookModePolling || !degraded || status.Reason != WebhookDegradedReasonFailures {
		t.Fatalf("failure: %+v degraded=%v", status, degraded)
	}
	clock.now = clock.now.Add(time.Minute)
	RecordWebhookEventForTest("org/repo")
	if status, _, _ = ObserveWebhookMode(); status.HealthyWindows != 1 {
		t.Fatalf("good delivery after the failure should start the streak: %+v", status)
	}
}
