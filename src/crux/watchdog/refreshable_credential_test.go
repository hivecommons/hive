package watchdog

import (
	"context"
	"testing"
)

// loginPane is the pane text a Claude CLI paints when the access token it
// pinned at process start ages out mid-session. Verbatim from the 2026-09-01
// incident, where six agents showed it overnight while the refresh grant in
// the shared credentials file was still four weeks from its own expiry and a
// single CLI restart brought the whole fleet back.
const loginPane = "● Please run /login · API Error: 401 OAuth access token has expired. Re-authenticate to continue."

// TestAuthLoginPromptWithUsableCredential is the incident's own shape: login
// chrome on the pane over a credential the fleet probe proves can still
// authenticate. The recovery is a CLI restart (the token-restart heal's job),
// so no operator may be paged.
func TestAuthLoginPromptWithUsableCredential(t *testing.T) {
	clock := newFakeClock()

	t.Run("proven credential clears the re-authentication alert", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend: "claude", SessionExists: true,
			Pane: loginPane, ShowsLoginPrompt: true,
			CredentialProven: true,
		})
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())

		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionUnknown {
			t.Fatalf("Authenticated = %+v, want Unknown: the credential is proven but the session is visibly not working", auth)
		}
		if auth.Reason != "LoginPromptWithUsableCredential" {
			t.Fatalf("reason = %q, want LoginPromptWithUsableCredential", auth.Reason)
		}
		if alerter.has(authAlertID("a1")) {
			t.Fatal("paged an operator for a credential no human needs to touch")
		}
		if fleet.restartCount() != 0 {
			t.Fatal("the reconciler still must not restart: healing an auth-required pane belongs to the manager's token-restart path")
		}
	})

	// The half that must not regress. Without proof the credential can
	// authenticate, a login prompt is a real logout and the loud alert is the
	// entire point of this branch.
	t.Run("unproven credential still pages the operator", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend: "claude", SessionExists: true,
			Pane: loginPane, ShowsLoginPrompt: true,
			CredentialProven: false,
		})
		alerter := newFakeAlerter()
		r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)
		r.Tick(context.Background())

		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Status != ConditionFalse || auth.Reason != "PaneShowsLogin" {
			t.Fatalf("Authenticated = %+v, want False/PaneShowsLogin", auth)
		}
		if !alerter.has(authAlertID("a1")) {
			t.Fatal("a genuine logout must still raise the credential-failure alert")
		}
	})

	// CredentialProven may only ever soften an auth-required verdict. Outside
	// that branch it must not manufacture health the reconciler never measured.
	t.Run("proof never overrides the per-agent file probe", func(t *testing.T) {
		fleet := newFakeFleet("a1")
		fleet.setObs("a1", Observation{
			Backend: "claude", SessionExists: true,
			Pane: "❯", HasCLIMarker: true,
			CredentialProven: true,
			AuthKnown:        true, AuthAvailable: false,
		})
		r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
		r.Tick(context.Background())

		auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
		if auth.Reason != "CredentialMissing" {
			t.Fatalf("reason = %q, want CredentialMissing", auth.Reason)
		}
	})
}

// TestAuthHealablePaneDoesNotClaimAuthenticated guards the collision between
// the deliberate Unknown above and the CredentialPresent fall-through further
// down reconcileAuth, which upgrades an Unknown verdict to True whenever the
// per-agent file probe answers positively. That fall-through was written for an
// ABSENT verdict; applied to this one it would report Authenticated=true for an
// agent visibly parked at a login prompt.
func TestAuthHealablePaneDoesNotClaimAuthenticated(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("a1")
	fleet.setObs("a1", Observation{
		Backend: "claude", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
		CredentialProven: true,
		// The file probe is positive at the same time — the exact overlap.
		AuthKnown: true, AuthAvailable: true,
	})
	r := newTestReconciler(t, fastSettings(), fleet, newFakeAlerter(), clock)
	r.Tick(context.Background())

	auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
	if auth.Status != ConditionUnknown || auth.Reason != "LoginPromptWithUsableCredential" {
		t.Fatalf("Authenticated = %+v, want Unknown/LoginPromptWithUsableCredential: a positive file probe must not vouch for a pane stuck at login", auth)
	}
}

// TestAuthAlertClearsWhenLoginChromeLeavesWithoutPositiveProof covers the
// copilot shape: no credential signal can ever prove it True, so recovery is
// False -> Unknown. The banner must still lift once the pane stops showing
// login chrome, or one transient sighting pins it over a working agent.
func TestAuthAlertClearsWhenLoginChromeLeavesWithoutPositiveProof(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("a1")
	alerter := newFakeAlerter()
	r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)

	fleet.setObs("a1", Observation{
		Backend: "copilot", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
	})
	r.Tick(context.Background())
	if !alerter.has(authAlertID("a1")) {
		t.Fatal("precondition: a login screen must raise the alert")
	}

	fleet.setObs("a1", Observation{
		Backend: "copilot", SessionExists: true,
		Pane: "● reviewing PR #12", HasCLIMarker: true,
	})
	r.Tick(context.Background())
	auth, _ := FindCondition(r.Conditions("a1"), ConditionAuthenticated)
	if auth.Status == ConditionFalse {
		t.Fatalf("Authenticated = %+v, want non-False once the login chrome is gone", auth)
	}
	if alerter.has(authAlertID("a1")) {
		t.Fatal("alert is sticky: it must clear when the verdict leaves False, even if nothing can prove it True")
	}

	fleet.setObs("a1", Observation{
		Backend: "copilot", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
	})
	r.Tick(context.Background())
	if !alerter.has(authAlertID("a1")) {
		t.Fatal("a genuine relapse into login must re-raise the alert")
	}
}

// TestAuthHealableClearsAStandingAlert is the recovery half. An operator who
// re-authenticates leaves login chrome on the pane, so the verdict moves
// False -> Unknown, never False -> True. Clearing only on True would leave the
// "needs re-authentication" banner standing over a credential that is fine —
// the sticky-alert shape #5291 was filed about.
func TestAuthHealableClearsAStandingAlert(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("a1")
	alerter := newFakeAlerter()
	r := newTestReconciler(t, fastSettings(), fleet, alerter, clock)

	// Sweep 1: genuinely logged out — alert raised.
	fleet.setObs("a1", Observation{
		Backend: "claude", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
		CredentialProven: false,
	})
	r.Tick(context.Background())
	if !alerter.has(authAlertID("a1")) {
		t.Fatal("precondition: a logged-out agent must raise the alert")
	}

	// Sweep 2: credential restored, pane still carrying login residue.
	fleet.setObs("a1", Observation{
		Backend: "claude", SessionExists: true,
		Pane: loginPane, ShowsLoginPrompt: true,
		CredentialProven: true,
	})
	r.Tick(context.Background())
	if alerter.has(authAlertID("a1")) {
		t.Fatal("alert is sticky: it must clear once the credential is usable again, not wait for the pane to stop showing login chrome")
	}
}
