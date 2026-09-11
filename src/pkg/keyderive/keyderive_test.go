package keyderive

import "testing"

// These vectors pin the exact wire format. If any of them changes, every
// deployed hub/spoke pair disagrees on heartbeat, invite, and SSO keys, so a
// failure here means "you are about to break every fleet", not "update the
// test".
func TestDomainKeyVectors(t *testing.T) {
	cases := []struct {
		master, info, want string
	}{
		{"", "hive-heartbeat-v1", ""},
		{"master-secret", "", "ad7c390d59ce99c2695f3a8a97bba53d61b5afd013c0ecbec51671ebc5374e03"},
		{
			"master-secret", "hive-heartbeat-v1",
			"848800bfd0192d687fa87919b8c1c31ca59ef46d0740ee01cfd3ff64eb993cc5",
		},
	}
	for _, c := range cases {
		if got := DomainKey(c.master, c.info); got != c.want {
			t.Errorf("DomainKey(%q, %q) = %q, want %q", c.master, c.info, got, c.want)
		}
	}
}

func TestPerHiveKeyVectors(t *testing.T) {
	if got := PerHiveKey("", "hive-invite-v1", "h1"); got != "" {
		t.Errorf("empty master: got %q, want \"\"", got)
	}
	if got := PerHiveKey("master-secret", "hive-invite-v1", ""); got != "" {
		t.Errorf("empty hiveID: got %q, want \"\"", got)
	}
	want := "8565082b0973460d33ddecff1be7ec20fe949ec4b97d2bcdbfa8ec2d062ae3c7"
	if got := PerHiveKey("master-secret", "hive-invite-v1", "h1"); got != want {
		t.Errorf("PerHiveKey = %q, want %q", got, want)
	}
	// The 0x00 separator must keep (info, hiveID) unambiguous.
	if PerHiveKey("m", "ab", "c") == PerHiveKey("m", "a", "bc") {
		t.Error("PerHiveKey collides across (info, hiveID) boundary shift")
	}
}

func TestEd25519PublicKeyFromSeed(t *testing.T) {
	// Seed of 32 zero bytes has a well-known public key.
	const zeroSeed = "0000000000000000000000000000000000000000000000000000000000000000"
	const wantPub = "3b6a27bcceb6a42d62a3a8d02a6f0d73653215771de243a63ac048a18b59da29"
	if got := Ed25519PublicKeyFromSeed(zeroSeed); got != wantPub {
		t.Errorf("zero seed pub = %q, want %q", got, wantPub)
	}
	if got := Ed25519PublicKeyFromSeed(" " + zeroSeed + "\n"); got != wantPub {
		t.Errorf("whitespace-trimmed seed pub = %q, want %q", got, wantPub)
	}
	for _, bad := range []string{"", "zz", "abcd", zeroSeed + "00"} {
		if got := Ed25519PublicKeyFromSeed(bad); got != "" {
			t.Errorf("Ed25519PublicKeyFromSeed(%q) = %q, want \"\"", bad, got)
		}
	}
}
