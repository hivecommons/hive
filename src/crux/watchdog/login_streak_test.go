package watchdog

import (
	"context"
	"strings"
	"testing"
)

// TestAuthLoginStreak pins the reconciler half of #17320: with the default
// LoginStreak, login chrome must persist across consecutive polls before
// Authenticated flips False and an operator is paged. A relaunched CLI that
// flashes login chrome on one poll and is back at its prompt on the next never
// pages; a pane genuinely stuck at login still does once the streak is met.
func TestAuthLoginStreak(t *testing.T) {
	loginObs := Observation{
		Backend: "copilot", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
		StartedAt: launchedLongAgo,
	}
	readyObs := Observation{
		Backend: "copilot", SessionExists: true,
		Pane: "● reviewing PR #12", HasCLIMarker: true,
		StartedAt: launchedLongAgo,
	}
	settings := fastSettings()
	settings.LoginStreak = DefaultLoginStreak

	t.Run("one-poll relaunch flash never pages", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		alerter := newFakeAlerter()
		r := newTestReconciler(t, settings, fleet, alerter, newFakeClock())

		fleet.setObs("a1", loginObs)
		r.Tick(context.Background())
		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Reason == "PaneShowsLogin" {
			t.Fatalf("Authenticated = %+v after one sighting, want not PaneShowsLogin", auth)
		}
		if alerter.has(authAlertID("a1")) {
			t.Fatal("one login sighting paged the operator")
		}
		if got := r.Conditions("a1"); !hasReadyReasonContaining(got, "login chrome seen 1/3 polls") {
			t.Fatalf("Ready condition should carry the streak reason, got %+v", got)
		}

		fleet.setObs("a1", readyObs)
		r.Tick(context.Background())
		fleet.setObs("a1", loginObs)
		r.Tick(context.Background())
		fleet.setObs("a1", loginObs)
		r.Tick(context.Background())
		// Two sightings after a reset are still below the threshold.
		if alerter.has(authAlertID("a1")) {
			t.Fatal("streak must reset when the pane leaves login chrome")
		}
	})

	t.Run("login chrome on every poll pages at the threshold", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		alerter := newFakeAlerter()
		r := newTestReconciler(t, settings, fleet, alerter, newFakeClock())
		fleet.setObs("a1", loginObs)

		for i := 1; i < DefaultLoginStreak; i++ {
			r.Tick(context.Background())
			if alerter.has(authAlertID("a1")) {
				t.Fatalf("paged after %d/%d sightings", i, DefaultLoginStreak)
			}
		}
		r.Tick(context.Background())
		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionFalse || auth.Reason != "PaneShowsLogin" {
			t.Fatalf("Authenticated = %+v at the streak threshold, want False/PaneShowsLogin", auth)
		}
		if !alerter.has(authAlertID("a1")) {
			t.Fatal("a pane stuck at login must still page once the streak is met")
		}
		if fleet.restartCount() != 0 {
			t.Fatal("auth-required must never restart")
		}
	})
}

func hasReadyReasonContaining(conds []Condition, want string) bool {
	ready, ok := FindCondition(conds, ConditionReady)
	return ok && strings.Contains(ready.Message, want)
}
