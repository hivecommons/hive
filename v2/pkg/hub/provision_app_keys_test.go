package hub

import (
	"strings"
	"testing"
)

// The provisioning-time both-keys delivery. These tests pin the property that
// matters operationally: a spoke is born holding EVERY App key the fleet knows,
// each named by the app_id it belongs to, and never a second copy of its own
// primary key under a different name.

// appIDsOf reduces the rendered result to the app_ids it names, which is the
// only part a caller reasons about. Key material is never compared or logged.
func appIDsOf(keys []provisionAppKey) []int64 {
	out := make([]int64, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.AppID)
	}
	return out
}

func testFleetKeys(t *testing.T) map[int64]fleetAppKey {
	t.Helper()
	return map[int64]fleetAppKey{
		3568013: {AppID: 3568013, AppSlug: "kubestellar-hive", PrivateKey: testAppKeyPEM(t)},
		5686:    {AppID: 5686, AppSlug: "kubestellar-hive-ghe", PrivateKey: testAppKeyPEM(t)},
	}
}

// A public-GitHub hive must be provisioned holding the GHE key as an additional
// key — that is what makes a later forge switch instant — but never a duplicate
// of its own primary.
func TestProvisionAdditionalAppKeysExcludesPrimary(t *testing.T) {
	got := provisionAdditionalAppKeys(testFleetKeys(t), "3568013")

	ids := appIDsOf(got)
	if len(ids) != 1 || ids[0] != 5686 {
		t.Fatalf("public hive should receive only the GHE key as additional, got %v", ids)
	}
}

// The mirror case: a GHE hive receives the github.com key, not its own.
func TestProvisionAdditionalAppKeysGHEPrimary(t *testing.T) {
	got := provisionAdditionalAppKeys(testFleetKeys(t), "5686")

	ids := appIDsOf(got)
	if len(ids) != 1 || ids[0] != 3568013 {
		t.Fatalf("GHE hive should receive only the public key as additional, got %v", ids)
	}
}

// A hive with no App configured (token auth, or a placeholder awaiting a key)
// has no primary to exclude, so it receives the whole fleet set.
func TestProvisionAdditionalAppKeysNoPrimary(t *testing.T) {
	for _, primary := range []string{"", "0", "not-a-number"} {
		got := provisionAdditionalAppKeys(testFleetKeys(t), primary)
		if len(got) != 2 {
			t.Fatalf("primary %q: want both fleet keys, got %d", primary, len(got))
		}
	}
}

// Ordering must be deterministic or every reconcile renders a different manifest
// and looks like a change.
func TestProvisionAdditionalAppKeysDeterministicOrder(t *testing.T) {
	fleet := testFleetKeys(t)
	first := appIDsOf(provisionAdditionalAppKeys(fleet, ""))

	for i := 0; i < 10; i++ {
		got := appIDsOf(provisionAdditionalAppKeys(fleet, ""))
		if len(got) != len(first) {
			t.Fatalf("length changed between runs: %v vs %v", first, got)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("order changed between runs: %v vs %v", first, got)
			}
		}
	}
	if first[0] >= first[1] {
		t.Fatalf("want ascending app_id order, got %v", first)
	}
}

// A truncated or garbage key must never be rendered: written into the Secret it
// would shadow a good key of the same app_id and break signing.
func TestProvisionAdditionalAppKeysSkipsNonPEM(t *testing.T) {
	fleet := map[int64]fleetAppKey{
		3568013: {AppID: 3568013, PrivateKey: testAppKeyPEM(t)},
		5686:    {AppID: 5686, PrivateKey: "PLACEHOLDER-awaiting-github-app-key"},
	}

	got := provisionAdditionalAppKeys(fleet, "")

	ids := appIDsOf(got)
	if len(ids) != 1 || ids[0] != 3568013 {
		t.Fatalf("non-PEM key must be skipped, got %v", ids)
	}
}

func TestProvisionAdditionalAppKeysEmptyFleet(t *testing.T) {
	if got := provisionAdditionalAppKeys(nil, "3568013"); got != nil {
		t.Fatalf("nil fleet should yield nil, got %v", appIDsOf(got))
	}
}

// The PEM is rendered as a YAML block scalar, so every line must carry the
// block's indentation. An unindented line silently truncates the key.
func TestProvisionAdditionalAppKeysIndentsForYAMLBlock(t *testing.T) {
	got := provisionAdditionalAppKeys(testFleetKeys(t), "3568013")
	if len(got) != 1 {
		t.Fatalf("want 1 additional key, got %d", len(got))
	}

	for i, line := range strings.Split(got[0].PEM, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("line %d is not indented for the YAML block: %q", i, line)
		}
		if strings.TrimSpace(line) == "" {
			t.Fatalf("line %d is blank, which would terminate the block scalar", i)
		}
	}
}
