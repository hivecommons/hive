package hub

import (
	"strings"
	"testing"
	"time"
)

// appBlocked builds a stage-1-stalled hive that has been stalled long enough to
// have crossed the de-provision threshold, reporting the given app state token.
func appBlocked(state string, age time.Duration) *RegistryEntry {
	return hive(age, func(h *RegistryEntry) {
		h.GitHubAppRequired = true
		h.GitHubAppPermIssue = "credentials not valid"
		h.GitHubAppState = state
	})
}

// TestGitHubAppBlockedByOperator locks which wire tokens count as operator-side.
func TestGitHubAppBlockedByOperator(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{"missing key is operator-side", "key-missing", true},
		{"invalid key is operator-side", "key-invalid", true},
		{"padded token still matches", "  key-invalid  ", true},
		{"not installed is the user's to fix", "not-installed", false},
		{"wrong installation is the user's to fix", "wrong-installation", false},
		{"insufficient permissions is the org owner's to fix", "insufficient-permissions", false},
		{"ok is not blocked", "ok", false},
		// An old spoke that never reports the field must NOT be treated as
		// operator-blocked, or genuine nudges go silent fleet-wide.
		{"empty (old spoke) falls through", "", false},
		{"unknown token falls through", "some-future-state", false},
		{"unknown falls through", "unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &RegistryEntry{GitHubAppState: tt.state}
			if got := githubAppBlockedByOperator(h); got != tt.want {
				t.Errorf("githubAppBlockedByOperator(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
	if githubAppBlockedByOperator(nil) {
		t.Error("nil entry must not be reported as operator-blocked")
	}
}

// TestNextNudge_OperatorBlockedHiveIsNeverNudged is the core regression: a hive
// whose App key the operator has not delivered must receive NO stage-1 nudge at
// any age — least of all a de-provision warning.
func TestNextNudge_OperatorBlockedHiveIsNeverNudged(t *testing.T) {
	// Ages spanning gentle, firm and de-provision thresholds, plus far beyond.
	ages := []time.Duration{
		stage1GentleAfter, stage1FirmAfter, stage1WarnAfter,
		stage1WarnAfter * 4,
	}
	for _, state := range []string{"key-missing", "key-invalid"} {
		for _, age := range ages {
			n := nextNudge(appBlocked(state, age), baseTime)
			if n.Stage == StageGitHubApp {
				t.Errorf("state=%s age=%s produced a stage-1 nudge (severity %s): %q",
					state, age, n.Severity, n.Message)
			}
			if n.Severity == SeverityDeprovisionWarning {
				t.Errorf("state=%s age=%s produced a DE-PROVISION WARNING for an operator-side failure: %q",
					state, age, n.Message)
			}
			if strings.Contains(strings.ToLower(n.Message), "de-provision") {
				t.Errorf("state=%s age=%s message threatens de-provisioning: %q", state, age, n.Message)
			}
		}
	}
}

// TestNextNudge_UserBlockedHiveStillEscalates is the counterpart guard: the fix
// must not weaken genuine warnings. A hive that truly has not installed the App
// must still be told to, and must still escalate.
func TestNextNudge_UserBlockedHiveStillEscalates(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		// An old spoke that reports no state at all keeps the old behaviour.
		{"unreported state", ""},
		{"explicitly not installed", "not-installed"},
		{"wrong installation id", "wrong-installation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := nextNudge(appBlocked(tt.state, stage1WarnAfter), baseTime)
			if n.Stage != StageGitHubApp {
				t.Fatalf("Stage = %v, want the GitHub App stage to still fire", n.Stage)
			}
			if n.Severity != SeverityDeprovisionWarning {
				t.Errorf("Severity = %v, want the de-provision warning to still escalate", n.Severity)
			}
			if n.Message == "" {
				t.Error("a genuine stage-1 stall must still produce a message")
			}
		})
	}
}

// TestNextNudge_OperatorBlockStillAllowsLaterStages proves the guard is scoped
// to stage 1 only: it suppresses the GitHub App nudge, not the whole journey.
func TestNextNudge_OperatorBlockedDoesNotSuppressOtherStages(t *testing.T) {
	// A hive that is NOT stage-1 stalled but reports an operator-side token
	// (e.g. a stale field) must still be evaluated for stages 2 and 3.
	h := hive(stage2WarnAfter, func(e *RegistryEntry) {
		e.GitHubAppState = "key-missing"
		// Stage 1 satisfied — no perm issue recorded.
		e.GitHubAppRequired = false
		e.GitHubAppPermIssue = ""
		// Stage 2 unsatisfied.
		e.AgentsWithModel = intPtr(0)
	})
	n := nextNudge(h, baseTime)
	if n.Stage != StageMethodModel {
		t.Errorf("Stage = %v, want stage 2 to still be evaluated for an operator-blocked token", n.Stage)
	}
}

// TestOperatorSideAppStateTokensMatchSpoke locks the duplicated wire tokens.
// pkg/hub deliberately does not import pkg/github, so these literals must stay
// in sync with github.AppAuthState.String() by test, not by compiler.
func TestOperatorSideAppStateTokensMatchSpoke(t *testing.T) {
	// These are exactly the tokens github.AppStateKeyMissing.String(),
	// github.AppStateKeyInvalid.String() and
	// github.AppStateNoAppAssigned.String() produce; pkg/github's
	// TestAppAuthState_WireRoundTrip locks the other side.
	//
	// "no-app-assigned" is operator-side: a hive still on the placeholder
	// app_id cannot be fixed by its owner at all, so the journey must never
	// nudge — let alone threaten de-provisioning — over it.
	want := map[string]bool{"key-missing": true, "key-invalid": true, "no-app-assigned": true}
	if len(operatorSideAppStates) != len(want) {
		t.Fatalf("operatorSideAppStates = %v, want exactly %v", operatorSideAppStates, want)
	}
	for token := range want {
		if !operatorSideAppStates[token] {
			t.Errorf("operatorSideAppStates is missing the %q token", token)
		}
	}
}
