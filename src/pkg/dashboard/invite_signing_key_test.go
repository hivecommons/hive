package dashboard

import (
	"testing"
	"time"

	hub "github.com/hivecommons/hive/pkg/hub/spoke"
)

// Invite tokens are HMAC-signed with inviteSigningSecret(). Historically that key
// was the RAW master HIVE_HUB_SECRET — used directly AS the HMAC key — which made
// the invite lane the last spoke-side consumer that actually needed the master,
// and which (because the master is fleet-uniform) meant every spoke signed
// invites with an identical key.
//
// That lane is now DELETED. These tests pin the new resolution order —
// HIVE_INVITE_KEY, then the SELF-DERIVED per-hive key from HIVE_HUB_SECRET +
// HIVE_ID, then a persisted per-instance random file — and pin that the lanes are
// cryptographically distinct, so a token signed under one does not verify under
// another.

const (
	testInviteKey    = "per-hive-invite-key-aaaaaaaaaaaaaaaa"
	testInviteMaster = "master-hub-secret-bbbbbbbbbbbbbbbb"
	testInviteHiveID = "hive-under-test"
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

// TestInviteSigningSecretSelfDerivesPerHive pins the in-place cutover lane: a
// spoke with no HIVE_INVITE_KEY but holding the master and its own HIVE_ID
// derives the CORRECT per-hive invite key itself, with no hub action and no
// re-provision. This is what makes deleting the raw-master lane safe.
func TestInviteSigningSecretSelfDerivesPerHive(t *testing.T) {
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_ID", testInviteHiveID)
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetInviteSecretForTest(t)

	got := string(inviteSigningSecret())
	want := hub.SpokeInviteKey()
	if got != want {
		t.Fatalf("inviteSigningSecret() = %q, want the self-derived per-hive key %q", got, want)
	}
	// The RAW MASTER must never be the signing key.
	if got == testInviteMaster {
		t.Fatal("REGRESSION: the raw master is being used directly as the invite HMAC key")
	}
	// Per-hive: a different HIVE_ID under the same master yields a different key.
	t.Setenv("HIVE_ID", testInviteHiveID+"-other")
	resetInviteSecretForTest(t)
	if other := string(inviteSigningSecret()); other == got {
		t.Fatal("invite key must differ per hive under the same master")
	}
}

// TestInviteSigningSecretNoIdentityFallsToFile pins that a spoke holding the
// master but UNABLE to identify itself does not fall back to any shared key: it
// gets the per-instance generated secret instead. derivePerHiveKey returns "" for
// an empty hive ID precisely so this case is detected rather than assumed away.
func TestInviteSigningSecretNoIdentityFallsToFile(t *testing.T) {
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_ID", "")
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	resetInviteSecretForTest(t)

	got := string(inviteSigningSecret())
	if got == "" {
		t.Fatal("expected a generated secret")
	}
	if got == testInviteMaster {
		t.Fatal("REGRESSION: fell back to the raw fleet-uniform master with no hive identity")
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
		hiveID    string
	}{
		{name: "invite key lane", inviteKey: testInviteKey, master: "", hiveID: ""},
		{name: "self-derive per-hive lane", inviteKey: "", master: testInviteMaster, hiveID: testInviteHiveID},
		{name: "generated file lane", inviteKey: "", master: "", hiveID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_INVITE_KEY", tc.inviteKey)
			t.Setenv("HIVE_HUB_SECRET", tc.master)
			t.Setenv("HIVE_ID", tc.hiveID)
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

	// Sign under the self-derived per-hive lane.
	t.Setenv("HIVE_INVITE_KEY", "")
	t.Setenv("HIVE_HUB_SECRET", testInviteMaster)
	t.Setenv("HIVE_ID", testInviteHiveID)
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
