package hub

import (
	"strings"
	"testing"
)

// provisionInviteKey is the hub side of HIVE_INVITE_KEY: it derives the per-hive
// contributor-invite signing key that lets a spoke sign invites WITHOUT holding
// the raw master. These tests pin that it is genuinely per-hive, genuinely
// domain-separated from the other sub-keys, and never the master itself.

const testInviteMasterSecret = "test-master-secret-invite"

// TestProvisionInviteKeyIsPerHive pins that two hives get different invite keys,
// so an invite token minted on one tenant cannot verify on another.
func TestProvisionInviteKeyIsPerHive(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", testInviteMasterSecret)

	a := provisionInviteKey("hive-alpha")
	b := provisionInviteKey("hive-beta")

	// Positive control: derivation actually produced key material. Without this a
	// bug returning "" for everything would satisfy the inequality below.
	if a == "" || b == "" {
		t.Fatalf("provisionInviteKey returned empty: alpha=%q beta=%q", a, b)
	}
	if a == b {
		t.Fatalf("invite key is fleet-uniform: alpha and beta both = %q, want per-hive keys", a)
	}
	// Deterministic: the hub must re-derive the same value on every provision.
	if again := provisionInviteKey("hive-alpha"); again != a {
		t.Fatalf("provisionInviteKey not deterministic: got %q then %q", a, again)
	}
}

// TestProvisionInviteKeyIsNotTheMaster pins the security property that motivated
// the change: the injected value must not be the master, nor equal to any other
// derived sub-key for the same hive (domain separation).
func TestProvisionInviteKeyIsNotTheMaster(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", testInviteMasterSecret)

	const hiveID = "hive-alpha"
	invite := provisionInviteKey(hiveID)
	if invite == "" {
		t.Fatal("provisionInviteKey returned empty; want derived key material")
	}
	if invite == testInviteMasterSecret {
		t.Fatalf("invite key IS the raw master %q — the whole point is that it is not", invite)
	}
	if strings.Contains(invite, testInviteMasterSecret) {
		t.Fatalf("invite key %q leaks the master verbatim", invite)
	}

	for name, other := range map[string]string{
		"heartbeat": provisionHeartbeatKey(hiveID),
		"terminal":  provisionTerminalKey(hiveID),
	} {
		if other == "" {
			t.Fatalf("%s key empty; positive control failed", name)
		}
		if invite == other {
			t.Fatalf("invite key collides with the %s key (%q): domain separation broken", name, invite)
		}
	}
}

// TestProvisionInviteKeyEmptyWithoutMaster pins fail-closed behaviour: with no
// master configured there is nothing to derive from, and the provisioner must
// emit an empty value rather than a guessable constant. The spoke then falls
// through to its own generated secret.
func TestProvisionInviteKeyEmptyWithoutMaster(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "")
	if got := provisionInviteKey("hive-alpha"); got != "" {
		t.Fatalf("provisionInviteKey with no master = %q, want empty", got)
	}
}
