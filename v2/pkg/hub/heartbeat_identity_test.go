package hub

import (
	"log/slog"
	"testing"
)

// N1 (CWE-200/639/862): the heartbeat bearer must be bound to the claimed hive.
//
// On v2 the bearer proved only "some provisioned spoke": provisioning injects
// the RAW master into every spoke, and the fleet-wide derived key is identical
// everywhere too. The handler then trusted payload.HiveID verbatim, and three
// key-delivery lanes read off that claimed ID — so any spoke could beat as any
// victim and be handed the victim's key material.
//
// This is the v2 port of the v4 fix (#3230). It keeps BOTH legacy credentials
// working, because every spoke in the field holds one of them and rejecting
// them would 401 the entire fleet at once.

const n1v2Master = "test-master-secret-n1-v2"

func n1v2Hub() *HubServer { return &HubServer{logger: slog.Default(), hubSecret: n1v2Master} }

// TestHeartbeatBearerIsPerHive is the core property.
func TestHeartbeatBearerIsPerHive(t *testing.T) {
	s := n1v2Hub()
	alpha := s.heartbeatKeyFor("hive-alpha")
	bravo := s.heartbeatKeyFor("hive-bravo")

	if alpha == "" || bravo == "" {
		t.Fatal("per-hive derivation returned empty")
	}
	if alpha == bravo {
		t.Fatal("two hives derived the SAME bearer — identity cannot be bound")
	}
	if !s.heartbeatBearerOK("Bearer "+alpha, "hive-alpha") {
		t.Error("a hive's own per-hive bearer was rejected for its own ID")
	}
	// The IDOR: alpha's bearer must not authenticate a beat CLAIMING to be bravo.
	// Note this only holds because alpha's bearer is not one of the legacy
	// credentials — those are still accepted for any ID during rollout.
	if s.heartbeatBearerOK("Bearer "+alpha, "hive-bravo") {
		t.Fatal("N1: hive-alpha's bearer authenticated a heartbeat claiming to be " +
			"hive-bravo — the victim's key material would go to the attacker")
	}
}

// TestHeartbeatLegacyCredentialsStillAccepted pins the rollout contract. v2
// provisioning ships the RAW master, so both legacy paths must keep working
// until the fleet is re-provisioned (#3234).
func TestHeartbeatLegacyCredentialsStillAccepted(t *testing.T) {
	s := n1v2Hub()

	if !s.heartbeatBearerOK("Bearer "+n1v2Master, "hive-alpha") {
		t.Fatal("raw master bearer rejected — every v2 spoke in the field would 401")
	}
	if !s.heartbeatBearerOK("Bearer "+s.heartbeatKey(), "hive-alpha") {
		t.Fatal("fleet-wide derived bearer rejected — spokes on that path would 401")
	}
	// ...and they are correctly reported as NOT identity-bound, so an operator
	// can tell when the compat lanes are safe to remove.
	if s.heartbeatBearerIsPerHive(n1v2Master, "hive-alpha") {
		t.Error("raw master misreported as per-hive")
	}
	if s.heartbeatBearerIsPerHive(s.heartbeatKey(), "hive-alpha") {
		t.Error("fleet key misreported as per-hive")
	}
	if !s.heartbeatBearerIsPerHive(s.heartbeatKeyFor("hive-alpha"), "hive-alpha") {
		t.Error("per-hive bearer not reported as per-hive")
	}
}

// TestHeartbeatBearerRejectsGarbage — the dual-accept path must not be
// mistaken for accept-anything.
func TestHeartbeatBearerRejectsGarbage(t *testing.T) {
	s := n1v2Hub()
	for _, bad := range []string{"", "Bearer ", "Bearer nope", "notabearer"} {
		if s.heartbeatBearerOK(bad, "hive-alpha") {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestPerHiveKeyEncodingIsUnambiguous covers the 0x00 separator: with plain
// concatenation ("ab","c") and ("a","bc") would hash identically, and hive IDs
// are operator-influenced.
func TestPerHiveKeyEncodingIsUnambiguous(t *testing.T) {
	if derivePerHiveKey(n1v2Master, "ab", "c") == derivePerHiveKey(n1v2Master, "a", "bc") {
		t.Error("domain/identity boundary is not encoded unambiguously")
	}
}

// TestPerHiveKeyFailsClosed: no master or no identity yields no key, never a
// shared fallback.
func TestPerHiveKeyFailsClosed(t *testing.T) {
	if derivePerHiveKey("", infoHeartbeatKey, "hive-alpha") != "" {
		t.Error("empty master derived a usable key")
	}
	if derivePerHiveKey(n1v2Master, infoHeartbeatKey, "") != "" {
		t.Error("empty hiveID derived a usable key — an identity-less caller must " +
			"not silently fall back to a shared key")
	}
}
