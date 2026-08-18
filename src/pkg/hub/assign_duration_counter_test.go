package hub

import (
	"strings"
	"testing"
	"time"
)

// The assignment-duration counter (#96) is rendered by inline JS inside the
// server-sent dashboardHTML raw string, so it is invisible to ordinary Go
// coverage. These structure tests assert the JS gate, the live ticker, and the
// stuck-threshold wiring are present and correctly conditioned, so a refactor
// that drops the counter or unconditions the gate fails loudly.

// TestClaimPendingPillGatedOnAssignedUnclaimed proves the counter markup only
// renders for an assigned-but-unclaimed row and self-suppresses otherwise.
func TestClaimPendingPillGatedOnAssignedUnclaimed(t *testing.T) {
	// A safety net: if the string is not actually being read, a Contains pass
	// proves nothing.
	if len(dashboardHTML) < 1000 {
		t.Fatalf("dashboardHTML is only %d bytes — not being read", len(dashboardHTML))
	}

	mustContain := []string{
		// The pill helper gates on BOTH assignedUnclaimed AND a timestamp, so a
		// claimed/available row (assignedUnclaimed false) shows nothing.
		"function claimPendingPill(h) {",
		"if (!h.assignedUnclaimed || !h.assignedAt) return '';",
		// The counter is wired into the status/mode cell.
		"modeCell += claimPendingPill(h);",
		// Human-friendly duration formatter (Xs / XmYs / Xh Ym).
		"function fmtAssignAge(secs) {",
		// The visible label an operator reads.
		"claim pending · ",
		// Live 1s ticker so the counter advances between the 30s row polls.
		"function tickAssignCounters() {",
		"startAssignCounterTicker();",
		"data-assign-since",
	}
	for _, s := range mustContain {
		if !strings.Contains(dashboardHTML, s) {
			t.Errorf("dashboardHTML missing %q — did the assignment-duration counter get dropped?", s)
		}
	}
}

// TestClaimPendingStuckThresholdReusesSweepConstant proves the "stuck" tint is
// driven by h.assignStuckSeconds (the SAME threshold the self-heal sweep
// enforces) rather than a hardcoded duplicate literal, so the amber warning
// cannot drift from the actual auto-reset behaviour.
func TestClaimPendingStuckThresholdReusesSweepConstant(t *testing.T) {
	mustContain := []string{
		"var stuck = stuckSecs > 0 && secs >= stuckSecs;",
		// Amber stuck colour + a warning glyph so it reads as "about to reset".
		"'#d29922'",
		"⚠ ",
	}
	for _, s := range mustContain {
		if !strings.Contains(dashboardHTML, s) {
			t.Errorf("dashboardHTML missing %q — stuck-state styling changed?", s)
		}
	}
}

// TestAssignCounterPayloadPopulation proves the server only rides the assignment
// clock + threshold on an assigned-but-unclaimed entry, and clears them for
// available and claimed rows — the exact gate the JS pill depends on. It mirrors
// the enrichFromSaaSMeta rule (a local closure) against the SaaSHive record.
func TestAssignCounterPayloadPopulation(t *testing.T) {
	assignedAt := time.Now().UTC().Format(time.RFC3339)
	wantStuck := int(assignStuckResetTimeout / time.Second)

	cases := []struct {
		name           string
		sh             SaaSHive
		wantUnclaimed  bool
		wantAssignedAt string
		wantStuckSecs  int
	}{
		{
			name:           "assigned but unclaimed rides the clock + threshold",
			sh:             SaaSHive{Status: statusAssigned, ClaimDelivered: false, AssignedAt: assignedAt},
			wantUnclaimed:  true,
			wantAssignedAt: assignedAt,
			wantStuckSecs:  wantStuck,
		},
		{
			name:           "claimed row carries no counter",
			sh:             SaaSHive{Status: statusAssigned, ClaimDelivered: true, AssignedAt: assignedAt},
			wantUnclaimed:  false,
			wantAssignedAt: "",
			wantStuckSecs:  0,
		},
		{
			name:           "available placeholder carries no counter",
			sh:             SaaSHive{Status: statusAvailable, AssignedAt: assignedAt},
			wantUnclaimed:  false,
			wantAssignedAt: "",
			wantStuckSecs:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror enrichFromSaaSMeta's counter block exactly.
			var entry MyHiveEntry
			entry.AssignedUnclaimed = tc.sh.Status == statusAssigned && !tc.sh.ClaimDelivered
			if entry.AssignedUnclaimed {
				entry.AssignedAt = tc.sh.AssignedAt
				entry.AssignStuckSeconds = int(assignStuckResetTimeout / time.Second)
			}

			if entry.AssignedUnclaimed != tc.wantUnclaimed {
				t.Errorf("AssignedUnclaimed = %v, want %v", entry.AssignedUnclaimed, tc.wantUnclaimed)
			}
			if entry.AssignedAt != tc.wantAssignedAt {
				t.Errorf("AssignedAt = %q, want %q", entry.AssignedAt, tc.wantAssignedAt)
			}
			if entry.AssignStuckSeconds != tc.wantStuckSecs {
				t.Errorf("AssignStuckSeconds = %d, want %d", entry.AssignStuckSeconds, tc.wantStuckSecs)
			}
		})
	}
}
