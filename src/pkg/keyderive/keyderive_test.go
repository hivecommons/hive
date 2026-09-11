package keyderive

import "testing"

// Fixed vectors below were computed from the pre-refactor copies (pkg/hub's
// deriveDomainKey/derivePerHiveKey/ssoPublicKeyFromSeed) BEFORE they were
// changed to delegate here, so they pin today's behavior rather than merely
// asserting self-consistency.
const (
	vecMaster = "master-secret-1"
	vecInfo   = "hive-heartbeat-v1"
	vecHiveID = "hive-a"

	vecDomainKey  = "d56ce38695aa64d729d1200280e3a884777b26c243e07eb05b57f90d1e200fe2"
	vecPerHiveKey = "7d735c54714a91ded8c91282ae9b0a9d850619c686370659987cc74aae1bc6a8"

	vecSeedInfo = "hive-sso-ed25519-v1"
	vecSeed     = "a4fdc94a5ca6d10c8b6539608e0ad4584853e8d2fa16064ec47768deadb48384"
	vecPubKey   = "6f4f77ae623587d43f54576c48c178f7574c1d1f790b1d4f3dcd1073e192dc59"
)

func TestDomainKeyFixedVector(t *testing.T) {
	if got := DomainKey(vecMaster, vecInfo); got != vecDomainKey {
		t.Fatalf("DomainKey(%q, %q) = %q, want %q", vecMaster, vecInfo, got, vecDomainKey)
	}
}

func TestDomainKeyEmptyMaster(t *testing.T) {
	if got := DomainKey("", vecInfo); got != "" {
		t.Fatalf("DomainKey(\"\", %q) = %q, want empty", vecInfo, got)
	}
}

func TestPerHiveKeyFixedVector(t *testing.T) {
	if got := PerHiveKey(vecMaster, vecInfo, vecHiveID); got != vecPerHiveKey {
		t.Fatalf("PerHiveKey(%q, %q, %q) = %q, want %q", vecMaster, vecInfo, vecHiveID, got, vecPerHiveKey)
	}
}

func TestPerHiveKeyFailsClosed(t *testing.T) {
	if got := PerHiveKey("", vecInfo, vecHiveID); got != "" {
		t.Fatalf("PerHiveKey with empty master = %q, want empty", got)
	}
	if got := PerHiveKey(vecMaster, vecInfo, ""); got != "" {
		t.Fatalf("PerHiveKey with empty hiveID = %q, want empty", got)
	}
}

func TestPerHiveKeyDistinctFromDomainKey(t *testing.T) {
	// Binding a hive ID must not collapse back to the fleet-wide value: the
	// per-hive key must differ from the domain key derived with the same
	// master/info but no hive ID.
	if PerHiveKey(vecMaster, vecInfo, vecHiveID) == DomainKey(vecMaster, vecInfo) {
		t.Fatal("PerHiveKey collided with DomainKey for the same master/info")
	}
}

func TestEd25519PublicKeyFromSeedFixedVector(t *testing.T) {
	seed := DomainKey(vecMaster, vecSeedInfo)
	if seed != vecSeed {
		t.Fatalf("seed = %q, want %q", seed, vecSeed)
	}
	if got := Ed25519PublicKeyFromSeed(seed); got != vecPubKey {
		t.Fatalf("Ed25519PublicKeyFromSeed(%q) = %q, want %q", seed, got, vecPubKey)
	}
}

func TestEd25519PublicKeyFromSeedRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"", "deadbeef", "not-hex-at-all", vecSeed + "00"} {
		if got := Ed25519PublicKeyFromSeed(bad); got != "" {
			t.Fatalf("Ed25519PublicKeyFromSeed(%q) = %q, want empty", bad, got)
		}
	}
}
