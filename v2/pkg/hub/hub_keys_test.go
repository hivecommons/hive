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
	ssoKey := deriveDomainKey(master, infoSSOKey)
	impersonateKey := deriveDomainKey(master, infoImpersonateKey)

	// What a spoke actually holds (all it is injected).
	spokeHeartbeatKey := deriveDomainKey(master, infoHeartbeatKey)

	// 1. Session cookie: a cookie signed with the spoke's heartbeat key must NOT
	//    verify against the hub's session key.
	forgedSession := mintHubUserCookieValue(spokeHeartbeatKey, "attacker-admin")
	if _, ok := verifyHubUserCookieValue(sessionKey, forgedSession); ok {
		t.Error("spoke heartbeat key forged a valid hub SESSION cookie")
	}

	// 2. SSO token: a token minted with the spoke's heartbeat key must NOT verify
	//    against the hub's SSO key.
	forgedSSO := MintSSOToken(spokeHeartbeatKey, "victim-owner", "owner", "hive-x", now)
	if _, _, err := VerifySSOToken(ssoKey, forgedSSO, "hive-x", now); err == nil {
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

	// Legacy path: only the master is present → derive.
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHeartbeatKey, "")
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
