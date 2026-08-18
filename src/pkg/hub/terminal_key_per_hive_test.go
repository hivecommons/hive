package hub

import (
	"testing"
	"time"
)

// N3 (CWE-862): the terminal signing key must be PER-HIVE.
//
// The assertion is minted on a spoke (dashboard/session.go, at login) and
// verified on that same spoke (proxy/server.js, on /terminal), so a symmetric
// key is architecturally correct — this is NOT the SSO case, where a second
// party has to verify and therefore the hub must hold a private signer.
//
// What was wrong is that TerminalSigningKey() fell through to HIVE_SESSION_KEY,
// which provisioning derives with a FIXED label from the one hub master and so
// is byte-identical on every spoke in the fleet. An assertion minted on spoke A
// therefore verified on spoke B: one hostile tenant operator could read the key
// out of their own pod env and forge a shell grant for an arbitrary username on
// an arbitrary hive. The hiveID claim is no obstacle — the forger simply mints
// with the victim's hive ID.

const n3TestMaster = "test-master-secret-n3"

// TestPerHiveKeysDifferAcrossHives is the core property: two hives provisioned
// from the SAME master must not receive the same terminal key.
func TestPerHiveKeysDifferAcrossHives(t *testing.T) {
	a := derivePerHiveKey(n3TestMaster, infoTerminalKey, "hive-alpha")
	b := derivePerHiveKey(n3TestMaster, infoTerminalKey, "hive-bravo")

	if a == "" || b == "" {
		t.Fatal("derivePerHiveKey returned empty for valid inputs")
	}
	if a == b {
		t.Fatal("two hives derived the SAME terminal key — this is exactly N3: " +
			"an assertion minted on one spoke would verify on the other")
	}
	// Deterministic: re-provisioning the same hive must not rotate its key, or
	// every re-provision would invalidate live terminal sessions.
	if again := derivePerHiveKey(n3TestMaster, infoTerminalKey, "hive-alpha"); again != a {
		t.Error("derivation is not deterministic — a re-provision would rotate the key")
	}
}

// TestPerHiveKeyDiffersFromFleetKey guards the actual regression: the per-hive
// key must not coincide with the fleet-wide session key it replaces.
func TestPerHiveKeyDiffersFromFleetKey(t *testing.T) {
	fleetSession := deriveDomainKey(n3TestMaster, infoSessionKey)
	fleetTerminal := deriveDomainKey(n3TestMaster, infoTerminalKey)
	perHive := derivePerHiveKey(n3TestMaster, infoTerminalKey, "hive-alpha")

	if perHive == fleetSession {
		t.Error("per-hive terminal key equals the fleet session key — the N3 lane is still open")
	}
	if perHive == fleetTerminal {
		t.Error("per-hive terminal key equals the fleet-wide terminal key — hiveID is not being mixed in")
	}
}

// TestPerHiveKeyDomainSeparation proves the key is bound to its trust domain as
// well as the hive, so a terminal key can never double as a heartbeat bearer.
func TestPerHiveKeyDomainSeparation(t *testing.T) {
	term := derivePerHiveKey(n3TestMaster, infoTerminalKey, "hive-alpha")
	beat := derivePerHiveKey(n3TestMaster, infoHeartbeatKey, "hive-alpha")
	if term == beat {
		t.Error("same hive derived identical keys for two different domains")
	}
}

// TestPerHiveKeyEncodingIsUnambiguous covers the reason for the 0x00 separator.
// With plain concatenation ("hive-terminal-v1" + "a|b") and ("hive-terminal-v1|a"
// + "b") would hash identically. Hive IDs are operator-influenced, so an
// ambiguous encoding is a real, if narrow, collision lane.
func TestPerHiveKeyEncodingIsUnambiguous(t *testing.T) {
	x := derivePerHiveKey(n3TestMaster, "ab", "c")
	y := derivePerHiveKey(n3TestMaster, "a", "bc")
	if x == y {
		t.Error("(info=ab,hive=c) and (info=a,hive=bc) collide — the domain/identity " +
			"boundary is not encoded unambiguously")
	}
}

// TestPerHiveKeyFailsClosed: no master or no hive identity must yield no key,
// never a shared fallback.
func TestPerHiveKeyFailsClosed(t *testing.T) {
	if got := derivePerHiveKey("", infoTerminalKey, "hive-alpha"); got != "" {
		t.Errorf("empty master derived %q, want \"\" (fail closed)", got)
	}
	if got := derivePerHiveKey(n3TestMaster, infoTerminalKey, ""); got != "" {
		t.Errorf("empty hiveID derived %q, want \"\" — an identity-less caller must not "+
			"silently fall back to a shared key", got)
	}
}

// TestCrossHiveAssertionForgeryFails is the end-to-end statement of N3, driven
// through the real mint/verify pair.
//
// Attacker holds spoke A's terminal key (they operate that tenant) and mints an
// assertion naming a victim hive and an arbitrary username. Under the old
// fleet-shared key that assertion verified on the victim's spoke. With per-hive
// keys it must not.
func TestCrossHiveAssertionForgeryFails(t *testing.T) {
	now := time.Now()
	attackerKey := derivePerHiveKey(n3TestMaster, infoTerminalKey, "attacker-hive")
	victimKey := derivePerHiveKey(n3TestMaster, infoTerminalKey, "victim-hive")

	// The forger mints for the VICTIM hive — the hiveID claim alone was never
	// the protection, since an attacker just writes the victim's ID.
	forged := MintTerminalAssertion(attackerKey, "mallory-not-registered", "owner", "victim-hive", now)
	if forged == "" {
		t.Fatal("mint returned empty; test setup is wrong")
	}

	// The victim spoke verifies with ITS OWN key.
	if _, _, err := VerifyTerminalAssertion(victimKey, forged, "victim-hive", now); err == nil {
		t.Fatal("N3: an assertion forged with ANOTHER hive's key verified on the victim hive — " +
			"a hostile tenant can open a shell as any user on any hive")
	}

	// Control: the victim's own key still mints assertions that verify, so the
	// fix does not break legitimate terminal access.
	legit := MintTerminalAssertion(victimKey, "alice", "owner", "victim-hive", now)
	user, role, err := VerifyTerminalAssertion(victimKey, legit, "victim-hive", now)
	if err != nil || user != "alice" || role != "owner" {
		t.Fatalf("legitimate assertion failed to verify: user=%q role=%q err=%v", user, role, err)
	}
}

// TestProvisionTerminalKeyIsPerHive checks the PROVISIONING accessor, not just
// the derivation primitive. This is the wiring that actually ships the key into
// the Deployment, and it is where an empty value would silently reintroduce the
// bug: HIVE_TERMINAL_KEY="" makes TerminalSigningKey() fall straight back to the
// fleet-wide HIVE_SESSION_KEY, so the manifest would look fixed while the spoke
// behaved exactly as before.
func TestProvisionTerminalKeyIsPerHive(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", n3TestMaster)

	a := provisionTerminalKey("hive-alpha")
	b := provisionTerminalKey("hive-bravo")

	if a == "" {
		t.Fatal("provisionTerminalKey returned empty — HIVE_TERMINAL_KEY would be unset in the " +
			"Deployment and TerminalSigningKey() would fall back to the fleet-wide session key")
	}
	if a == b {
		t.Fatal("provisioning derived the same terminal key for two hives")
	}
	if a == deriveDomainKey(n3TestMaster, infoSessionKey) {
		t.Fatal("provisioned terminal key equals the fleet session key")
	}
}

// TestFleetSharedKeyWouldHaveForged documents the pre-fix behaviour explicitly,
// so the test file states what the vulnerability WAS rather than only asserting
// the fixed state. If this ever stops holding, the shared-key lane is back.
func TestFleetSharedKeyWouldHaveForged(t *testing.T) {
	now := time.Now()
	shared := deriveDomainKey(n3TestMaster, infoSessionKey) // what both spokes used to hold

	forged := MintTerminalAssertion(shared, "mallory", "owner", "victim-hive", now)
	if _, _, err := VerifyTerminalAssertion(shared, forged, "victim-hive", now); err != nil {
		t.Skip("shared-key forgery no longer reproduces; mint/verify shape changed")
	}
	t.Log("confirmed: under a fleet-shared key, an assertion minted anywhere verifies " +
		"on the victim hive — this is the lane per-hive keys close")
}
