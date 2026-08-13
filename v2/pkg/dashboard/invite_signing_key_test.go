package dashboard

import (
	"testing"
	"time"
)

// Invite tokens are HMAC-signed with inviteSigningSecret(). Historically that key
// was the RAW master HIVE_HUB_SECRET, which made the invite lane the last
// spoke-side consumer that actually needed the master to function. These tests
// pin the new resolution order — HIVE_INVITE_KEY, then HIVE_HUB_SECRET, then a
// persisted per-instance random file — and pin that the lanes are cryptographically
// distinct, so a token signed under one does not verify under another.

const (
	testInviteKey    = "per-hive-invite-key-aaaaaaaaaaaaaaaa"
	testInviteMaster = "master-hub-secret-bbbbbbbbbbbbbbbb"
)

// inviteTestNow is a fixed instant well inside inviteTokenTTL, so expiry never
// confounds a signature assertion.
var inviteTestNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// TestInviteSigningSecretPrefersInviteKey pins that the dedicated per-hive key
// wins over the master when BOTH are set. This is the whole point of the change:
// a re-provisioned spoke stops using the master even if it still carries one.
func TestInviteSigningSecretPrefersInviteKey(t *testing.T) {
	t.Setenv("HIVE_INVITE_KEY", testInviteKey)
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetInviteSecretForTest(t)

	if got := string(inviteSigningSecret()); got != testInviteKey {
		t.Fatalf("inviteSigningSecret() = %q, want the dedicated HIVE_INVITE_KEY %q", got, testInviteKey)
	}
}

// TestInviteSigningSecretFallsBackToMaster pins the transitional lane: a spoke on
// an older Deployment that has no HIVE_INVITE_KEY must keep signing with the
// master, so existing hives are not broken by this change.
func TestInviteSigningSecretFallsBackToMaster(t *testing.T) {
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetInviteSecretForTest(t)

	if got := string(inviteSigningSecret()); got != testInviteMaster {
		t.Fatalf("inviteSigningSecret() = %q, want the HIVE_HUB_SECRET fallback %q", got, testInviteMaster)
	}
}

// TestInviteSigningSecretFallsBackToRandomFile pins the self-hosted lane where no
// key material is configured at all: a random secret is generated and PERSISTED,
// so invite links survive a restart.
func TestInviteSigningSecretFallsBackToRandomFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", "")
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	resetInviteSecretForTest(t)

	first := string(inviteSigningSecret())
	if first == "" {
		t.Fatal("inviteSigningSecret() returned empty with no env key configured; want a generated secret")
	}
	if first == testInviteKey || first == testInviteMaster {
		t.Fatalf("inviteSigningSecret() = %q, want a generated secret, not an env value", first)
	}

	// Persisted: a fresh process (simulated by resetting the once) reads the same
	// secret back off disk rather than minting a new one.
	resetInviteSecretForTest(t)
	if second := string(inviteSigningSecret()); second != first {
		t.Fatalf("generated invite secret not persisted: got %q on reload, want %q", second, first)
	}
}

// TestInviteTokenRoundTripUnderEachLane is the POSITIVE CONTROL for the
// cross-lane test below. If sign/verify were broken outright — or if
// verifyInviteToken always returned "" — the cross-lane assertions would pass
// for entirely the wrong reason. This proves each lane genuinely works.
func TestInviteTokenRoundTripUnderEachLane(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inviteKey string
		master    string
	}{
		{name: "invite key lane", inviteKey: testInviteKey, master: ""},
		{name: "master fallback lane", inviteKey: "", master: testInviteMaster},
		{name: "generated file lane", inviteKey: "", master: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_INVITE_KEY", tc.inviteKey)
			t.Setenv("HIVE_HUB_SECRET", tc.master)
			t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
			resetInviteSecretForTest(t)

			const inviter = "octocat"
			token := mintInviteToken(inviter, inviteTestNow)
			if got := verifyInviteToken(token, inviteTestNow); got != inviter {
				t.Fatalf("round trip failed: verifyInviteToken = %q, want %q", got, inviter)
			}
		})
	}
}

// TestInviteTokenDoesNotVerifyAcrossKeys pins the security property: the lanes are
// cryptographically distinct. A token signed while the spoke used the master must
// NOT verify once the spoke is re-provisioned with a per-hive HIVE_INVITE_KEY, and
// a token from one hive's invite key must not verify under another's. This is the
// documented one-time re-key, asserted rather than assumed.
func TestInviteTokenDoesNotVerifyAcrossKeys(t *testing.T) {
	const inviter = "octocat"

	// Sign under the master lane.
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetInviteSecretForTest(t)
	masterToken := mintInviteToken(inviter, inviteTestNow)

	// Verify under the per-hive invite lane: must fail.
	t.Setenv("HIVE_INVITE_KEY", testInviteKey)
	resetInviteSecretForTest(t)
	if got := verifyInviteToken(masterToken, inviteTestNow); got != "" {
		t.Fatalf("master-signed token verified under HIVE_INVITE_KEY as %q, want rejection", got)
	}

	// And a token minted under hive A's invite key must not verify under hive B's.
	aToken := mintInviteToken(inviter, inviteTestNow)
	t.Setenv("HIVE_INVITE_KEY", testInviteKey+"-different-hive")
	resetInviteSecretForTest(t)
	if got := verifyInviteToken(aToken, inviteTestNow); got != "" {
		t.Fatalf("hive A invite token verified under hive B's key as %q, want rejection", got)
	}
}
