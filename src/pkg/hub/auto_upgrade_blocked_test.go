package hub

import (
	"strings"
	"testing"
	"time"
)

// The fleet row used to render "Queued for auto-upgrade · 1pm ET" from
// autoUpgradeMode alone. Nothing in that expression consulted eligibility, so a
// hive the hub had permanently REFUSED — upgradeCollectible() false, recorded on
// the timeline by noteUncollectibleUpgrade() — kept advertising a queued upgrade
// that would never fire. These tests pin the read-side decision that replaces
// the client's re-derivation.

// TestAutoUpgradeBlockedMatchesTriggerGate is THE regression: the read-side
// answer must agree with the gate triggerAutoUpgrades() actually applies. If
// these two ever disagree, the badge is lying again — which is the whole bug.
func TestAutoUpgradeBlockedMatchesTriggerGate(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name          string
		lastHeartbeat string
	}{
		{"never heartbeated", ""},
		{"unparseable timestamp", "not-a-timestamp"},
		{"beating just now", now.Format(time.RFC3339)},
		{"one hour late", now.Add(-time.Hour).Format(time.RFC3339)},
		{"inside staleRemoveAge", now.Add(-(staleRemoveAge - time.Hour)).Format(time.RFC3339)},
		{"past staleRemoveAge", now.Add(-(staleRemoveAge + time.Hour)).Format(time.RFC3339)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collectible := upgradeCollectible(tc.lastHeartbeat, now)
			blocked, _ := autoUpgradeBlocked(true, tc.lastHeartbeat, now)
			if blocked == collectible {
				t.Errorf("autoUpgradeBlocked = %v but upgradeCollectible = %v; the badge "+
					"must report exactly what triggerAutoUpgrades() will do",
					blocked, collectible)
			}
		})
	}
}

// TestAutoUpgradeBlockedRequiresAutoUpgrade pins the gating. A manual hive is
// not "blocked" — it never asked the hub to upgrade it — and must not carry the
// badge no matter how long it has been silent.
func TestAutoUpgradeBlockedRequiresAutoUpgrade(t *testing.T) {
	now := time.Now()
	dead := now.Add(-(staleRemoveAge + time.Hour)).Format(time.RFC3339)

	if blocked, _ := autoUpgradeBlocked(false, dead, now); blocked {
		t.Error("a manual (auto-upgrade OFF) hive must never report blocked — it is " +
			"manual, not refused")
	}
	if blocked, _ := autoUpgradeBlocked(false, "", now); blocked {
		t.Error("a manual hive that never heartbeated must never report blocked")
	}
	// Sanity: the same silent hive WITH auto-upgrade on is blocked, so the test
	// above is proving the gate and not merely a dead predicate.
	if blocked, _ := autoUpgradeBlocked(true, dead, now); !blocked {
		t.Error("auto-upgrade ON plus an uncollectible hive must report blocked")
	}
}

// TestAutoUpgradeBlockedReasonIsOperatorSafe pins the tooltip contract. The
// reason string is rendered into the fleet row, so it must be non-empty (an
// empty tooltip explains nothing) and must not leak credentials or kubeconfig
// paths — the property uncollectibleUpgradeReason() documents.
func TestAutoUpgradeBlockedReasonIsOperatorSafe(t *testing.T) {
	now := time.Now()

	for _, hb := range []string{"", now.Add(-(staleRemoveAge + time.Hour)).Format(time.RFC3339)} {
		blocked, reason := autoUpgradeBlocked(true, hb, now)
		if !blocked {
			t.Fatalf("last_heartbeat %q should be blocked", hb)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("blocked hive (last_heartbeat %q) has an empty reason; the badge "+
				"tooltip would explain nothing", hb)
		}
		if reason != uncollectibleUpgradeReason(hb) {
			t.Errorf("reason = %q, want the SAME string noteUncollectibleUpgrade() "+
				"writes to the timeline — the two surfaces must not diverge", reason)
		}
		for _, leak := range []string{"kubeconfig", "token", "secret", "password", "/root/", ".kube"} {
			if strings.Contains(strings.ToLower(reason), leak) {
				t.Errorf("reason %q contains %q; this string is rendered in an "+
					"operator-facing tooltip", reason, leak)
			}
		}
	}

	// A collectible hive carries no reason at all.
	if _, reason := autoUpgradeBlocked(true, now.Format(time.RFC3339), now); reason != "" {
		t.Errorf("healthy hive reason = %q, want empty", reason)
	}
}
