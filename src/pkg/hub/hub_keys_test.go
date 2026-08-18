package hub

import (
	"testing"
	"time"
)

// TestDomainKeysAreDistinct pins the C2 guarantee at the primitive level: the
// four per-domain sub-keys derived from one master are all different from each
// other AND from the master itself. If any two collided, a holder of one domain's
// key could operate in another's.
func TestDomainKeysAreDistinct(t *testing.T) {
	const master = "master-secret-under-test"
	keys := map[string]string{
		"heartbeat":   deriveDomainKey(master, infoHeartbeatKey),
		"session":     deriveDomainKey(master, infoSessionKey),
		"sso":         deriveDomainKey(master, infoSSOKey),
		"impersonate": deriveDomainKey(master, infoImpersonateKey),
	}
	seen := map[string]string{master: "master"}
	for name, k := range keys {
		if k == "" {
			t.Fatalf("%s key derived empty", name)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("%s key collides with %s", name, prev)
		}
		seen[k] = name
	}
	// An empty master must never yield a usable key (fail-closed).
	if deriveDomainKey("", infoSessionKey) != "" {
		t.Fatal("empty master must derive to empty key")
	}
}

// TestSpokeHeartbeatKeyCannotForgeOtherDomains is the heart of the fix: the ONLY
// signing material a spoke holds is the heartbeat sub-key. This test proves that
// key cannot be used to mint OR verify anything in the session, SSO, or
// impersonation domains — the exact forgeries the single-key design allowed.
func TestSpokeHeartbeatKeyCannotForgeOtherDomains(t *testing.T) {
	const master = "the-hub-master-secret"
	now := time.Now()

	// What the hub verifies with, per domain.
	sessionKey := deriveDomainKey(master, infoSessionKey)
	ssoPubKey := ssoPublicKeyFromSeed(SSOSigningSeedFromMaster(master))
	impersonateKey := deriveDomainKey(master, infoImpersonateKey)

	// What a spoke actually holds (all it is injected).
	spokeHeartbeatKey := deriveDomainKey(master, infoHeartbeatKey)

	// 1. Session cookie: a cookie signed with the spoke's heartbeat key must NOT
	//    verify against the hub's session key.
	forgedSession := mintHubUserCookieValue(spokeHeartbeatKey, "attacker-admin")
	if _, ok := verifyHubUserCookieValue(sessionKey, forgedSession); ok {
		t.Error("spoke heartbeat key forged a valid hub SESSION cookie")
	}

	// 2. SSO token: a token the spoke tries to mint using its heartbeat key as a
	//    signing seed must NOT verify against the hub's SSO PUBLIC key. (SSO is now
	//    asymmetric: the spoke holds only the public key and no signing seed at all,
	//    so this also stands in for "spoke has no private material to sign with".)
	forgedSSO := MintSSOToken(spokeHeartbeatKey, "victim-owner", "owner", "hive-x", now)
	if _, _, err := VerifySSOToken(ssoPubKey, forgedSSO, "hive-x", now); err == nil {
		t.Error("spoke heartbeat key forged a valid SSO handoff token")
	}

	// 3. Impersonation cookie: minted with the heartbeat key must NOT verify
	//    against the hub's impersonation key. (Spokes never even hold this key.)
	forgedImp := mintImpersonateCookieValue(spokeHeartbeatKey, hubAdminUsername, "victim", now)
	if _, ok := verifyImpersonateCookieValue(impersonateKey, forgedImp, now); ok {
		t.Error("spoke heartbeat key forged a valid impersonation grant")
	}

	// Sanity: each domain's own key DOES round-trip, so the negatives above are
	// about domain separation, not a broken primitive.
	if v := mintHubUserCookieValue(sessionKey, "alice"); v == "" {
		t.Fatal("session key failed to mint")
	} else if u, ok := verifyHubUserCookieValue(sessionKey, v); !ok || u != "alice" {
		t.Fatal("session key round-trip failed")
	}
}

// TestSpokeKeyResolutionPrefersExplicitEnv confirms the spoke helpers use the
// dedicated per-domain env var when present, and otherwise derive from the master
// — so both the modern (least-privilege) and legacy provisioning paths yield the
// key the hub expects.
func TestSpokeKeyResolutionPrefersExplicitEnv(t *testing.T) {
	const master = "legacy-master"

	// Legacy path: the master is present but the spoke cannot identify itself
	// (no HIVE_ID) → fleet-wide derivation. This is now the LAST resort; see
	// TestSpokeHeartbeatKeySelfDerivesPerHive for the identity-bound lane that
	// takes precedence whenever HIVE_ID is set.
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHeartbeatKey, "")
	t.Setenv(EnvHiveID, "")
	if got, want := SpokeHeartbeatKey(), deriveDomainKey(master, infoHeartbeatKey); got != want {
		t.Errorf("legacy heartbeat key = %q, want derived %q", got, want)
	}

	// Modern path: explicit key wins over any master-derivation.
	t.Setenv(EnvHeartbeatKey, "explicit-injected-key")
	if got := SpokeHeartbeatKey(); got != "explicit-injected-key" {
		t.Errorf("explicit heartbeat key = %q, want the injected value", got)
	}

	// The master itself is NOT a valid spoke bearer: the hub verifies the derived
	// key, so presenting the raw master would fail. Prove they differ.
	if SpokeHeartbeatKey() == master {
		t.Error("spoke bearer must never equal the raw master secret")
	}
}

// SpokeInviteKey resolution: HIVE_INVITE_KEY, then the SELF-DERIVED per-hive key
// from HIVE_HUB_SECRET + HIVE_ID. Both lanes are per-hive; the RAW MASTER is
// never returned.
//
// The raw-master lane it replaces was not a theoretical concern: measured on the
// live fleet, HIVE_INVITE_KEY is absent on 65/65 spokes (the provisioning
// template emits it but the reconcile sweep did not carry it), so every spoke
// was signing invites with the fleet-uniform master itself.
func TestSpokeInviteKeyResolution(t *testing.T) {
	const master = "fleet-uniform-master"

	t.Setenv(EnvInviteKey, "")
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv(EnvHiveID, "")
	if got := SpokeInviteKey(); got != "" {
		t.Fatalf("no sources → empty (fail closed), got %q", got)
	}

	// Master but no identity: must NOT resolve to anything shared.
	t.Setenv("HIVE_HUB_SECRET", master)
	if got := SpokeInviteKey(); got != "" {
		t.Fatalf("master without HIVE_ID must not resolve a key, got %q", got)
	}

	// Self-derive lane.
	t.Setenv(EnvHiveID, "hive-a")
	got := SpokeInviteKey()
	if want := derivePerHiveKey(master, infoInviteKey, "hive-a"); got != want {
		t.Fatalf("self-derive lane = %q, want per-hive %q", got, want)
	}
	if got == master {
		t.Fatal("REGRESSION: the raw master must never be the invite key")
	}

	// Per-hive isolation: hive B under the SAME master gets a different key.
	t.Setenv(EnvHiveID, "hive-b")
	if other := SpokeInviteKey(); other == got {
		t.Fatal("invite key must differ per hive — an invite must not travel between tenants")
	}

	// Injected key wins over the derivation.
	t.Setenv(EnvInviteKey, "injected-invite-key")
	if SpokeInviteKey() != "injected-invite-key" {
		t.Fatal("injected HIVE_INVITE_KEY must win")
	}
}

// The invite key must be domain-separated from every other spoke-side key
// derived from the same master and hive ID. If it collided with the terminal or
// heartbeat key, holding one would grant the other.
func TestSpokeInviteKeyIsDomainSeparated(t *testing.T) {
	const master = "master-under-test"
	const hiveID = "hive-1"

	invite := derivePerHiveKey(master, infoInviteKey, hiveID)
	terminal := derivePerHiveKey(master, infoTerminalKey, hiveID)
	heartbeat := derivePerHiveKey(master, infoHeartbeatKey, hiveID)

	for name, v := range map[string]string{"invite": invite, "terminal": terminal, "heartbeat": heartbeat} {
		if v == "" {
			t.Fatalf("%s key derived empty", name)
		}
		if v == master {
			t.Fatalf("%s key equals the raw master", name)
		}
	}
	if invite == terminal || invite == heartbeat || terminal == heartbeat {
		t.Fatal("per-hive sub-keys must be pairwise distinct under one master+hive")
	}
}
