package hub

import (
	"strings"
	"testing"
)

// ── app-creds-undelivered (#4316) ───────────────────────────────────────────
//
// kelly-headwaters was provisioned 2026-08-12 and found degraded 2026-08-20 —
// eight days, zero tokens, nobody told. The states involved are operator-side,
// so the owner banner, the journey nudges and advisory staleness all correctly
// stand down; the effect was that the one actor who could fix it was the one
// actor no signal reached. These tests pin that the fleet alert closes that gap
// without re-opening the surfaces that were right to stay quiet.

func appCredsHive(state string, required bool) alertHive {
	return alertHive{
		ID:                "hosted-kelly-headwaters",
		Name:              "kelly-headwaters",
		Online:            true,
		LastHeartbeat:     rfc3339(fixedNow),
		GitHubAppRequired: required,
		GitHubAppState:    state,
		ClusterID:         "oke-260812",
	}
}

// TestAppCredsAlertFiresForEveryOperatorSideState is the core of the fix: all
// three states the owner cannot repair must reach the operator.
func TestAppCredsAlertFiresForEveryOperatorSideState(t *testing.T) {
	for _, state := range []string{appStateKeyMissingToken, appStateKeyInvalidToken, appStateNoAppAssignedToken} {
		t.Run(state, func(t *testing.T) {
			got := evaluateAlerts(newAlertState(), []alertHive{appCredsHive(state, true)}, nil, fixedNow)

			a, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered)
			if !ok {
				t.Fatalf("no app-creds-undelivered alert for state %q — the operator gets no signal, which is the bug", state)
			}
			if a.Severity != AlertSeverityCritical {
				t.Errorf("severity = %q, want critical: the hive provably cannot work at all", a.Severity)
			}
			if !strings.Contains(a.Reason, "cluster-app-keys/oke-260812") {
				t.Errorf("reason does not name the remedy endpoint with the cluster id: %q", a.Reason)
			}
		})
	}
}

// TestAppCredsAlertIgnoresOwnerFixableStates is the guard that keeps this from
// re-opening what the other surfaces deliberately closed. A state the OWNER can
// fix (App not installed yet) is not an operator problem and must not page an
// operator — the journey nudge already handles it.
func TestAppCredsAlertIgnoresOwnerFixableStates(t *testing.T) {
	for _, state := range []string{"not-installed", "installed", "", "ok", "suspended"} {
		t.Run("state="+state, func(t *testing.T) {
			got := evaluateAlerts(newAlertState(), []alertHive{appCredsHive(state, true)}, nil, fixedNow)
			if a, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); ok {
				t.Fatalf("state %q is not operator-side; alerting on it pages an operator for something the OWNER fixes: %q", state, a.Reason)
			}
		})
	}
}

// TestAppCredsAlertRequiresTheAppToBeRequired pins that a hive not using a
// GitHub App at all never raises this, whatever stale state string it carries.
func TestAppCredsAlertRequiresTheAppToBeRequired(t *testing.T) {
	got := evaluateAlerts(newAlertState(), []alertHive{appCredsHive(appStateKeyMissingToken, false)}, nil, fixedNow)
	if a, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); ok {
		t.Fatalf("a hive that does not require an App must not alert: %q", a.Reason)
	}
}

// TestAppCredsAlertSkipsPlaceholders pins the guard every claimed-hive rule
// applies: an unassigned pool slot has no App and no owner, and flagging it
// would bury the real ones.
func TestAppCredsAlertSkipsPlaceholders(t *testing.T) {
	h := appCredsHive(appStateKeyMissingToken, true)
	h.IsPlaceholder = true
	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	if a, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); ok {
		t.Fatalf("an unassigned pool slot must not alert: %q", a.Reason)
	}
}

// TestAppCredsAlertSelfHeals pins that uploading a key clears the alert on the
// next beat, with no hub-side timer to keep in sync.
func TestAppCredsAlertSelfHeals(t *testing.T) {
	state := newAlertState()

	got := evaluateAlerts(state, []alertHive{appCredsHive(appStateKeyMissingToken, true)}, nil, fixedNow)
	if _, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); !ok {
		t.Fatal("expected the alert to fire while the key is missing")
	}

	// Operator uploads the key; the spoke now reports a healthy state.
	got = evaluateAlerts(state, []alertHive{appCredsHive("installed", true)}, nil, fixedNow)
	if a, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); ok {
		t.Fatalf("the alert must clear once the credential lands, got %q", a.Reason)
	}
}

// TestAppCredsAlertFiresWhileOffline pins a deliberate deviation from the
// issue's wording, which said "claimed, ONLINE hive".
//
// Gating on online would drop the credential alert exactly when the hive is
// worst off — still keyless AND no longer heartbeating. The offline rule raises
// its own alert for the second half; this one is the only thing that says WHY
// the hive never worked, and an operator needs both.
func TestAppCredsAlertFiresWhileOffline(t *testing.T) {
	h := appCredsHive(appStateKeyMissingToken, true)
	h.Online = false
	h.LastHeartbeat = rfc3339(fixedNow.Add(-2 * alertOfflineThreshold))

	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	if _, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeAppCredsUndelivered); !ok {
		t.Fatal("an offline keyless hive still needs its key uploaded; the credential alert must not be suppressed by being offline")
	}
	if _, ok := findAlert(got.Alerts, "hosted-kelly-headwaters", AlertTypeOffline); !ok {
		t.Error("the offline rule should still fire independently — the two answer different questions")
	}
}

// TestAppCredsReasonNamesTheRightRepair pins that the three states get
// different sentences. They have genuinely different repairs, and a generic
// "credentials undelivered" would leave an operator as stuck as the silence
// did — the upload endpoint is documented nowhere and has no UI.
func TestAppCredsReasonNamesTheRightRepair(t *testing.T) {
	missing := appCredsUndeliveredReason(appStateKeyMissingToken, "c1")
	invalid := appCredsUndeliveredReason(appStateKeyInvalidToken, "c1")
	noApp := appCredsUndeliveredReason(appStateNoAppAssignedToken, "c1")

	if missing == invalid || invalid == noApp || missing == noApp {
		t.Fatal("the three states have different repairs and must not share one sentence")
	}
	// key-invalid must not read as "upload the key" — the same key again is not
	// the fix.
	if !strings.Contains(invalid, "different App") {
		t.Errorf("key-invalid reason should say the key belongs to another App: %q", invalid)
	}
	// no-app-assigned needs an App assigned first; a key upload alone is not enough.
	if !strings.Contains(noApp, "assign") {
		t.Errorf("no-app-assigned reason should say to assign an App: %q", noApp)
	}
	for _, r := range []string{missing, invalid, noApp} {
		if !strings.Contains(r, "PUT /api/saas/admin/cluster-app-keys/c1") {
			t.Errorf("reason does not carry the actionable endpoint: %q", r)
		}
	}
}

// TestAppCredsReasonWithoutClusterID pins that a record with no cluster id
// still yields a usable sentence rather than a path with an empty segment.
func TestAppCredsReasonWithoutClusterID(t *testing.T) {
	r := appCredsUndeliveredReason(appStateKeyMissingToken, "")
	if strings.Contains(r, "cluster-app-keys/ ") || strings.HasSuffix(r, "cluster-app-keys/") {
		t.Fatalf("empty cluster id produced a malformed endpoint: %q", r)
	}
	if !strings.Contains(r, "{clusterID}") {
		t.Errorf("expected the placeholder when the cluster is unknown: %q", r)
	}
}

// TestAppCredsAlertTypeIsRegistered pins the ack/severity pipeline wiring: an
// unregistered type is dropped, so the alert would never reach an operator.
func TestAppCredsAlertTypeIsRegistered(t *testing.T) {
	if !knownAlertTypes[AlertTypeAppCredsUndelivered] {
		t.Fatal("app-creds-undelivered is not in knownAlertTypes — it would be filtered out before anyone saw it")
	}
}
