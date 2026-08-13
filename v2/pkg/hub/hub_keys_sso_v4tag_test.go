package hub

import "testing"

// hiveWantsEd25519SSO decides which SSO token format the hub mints. Getting it
// wrong is a live outage in both directions: mint symmetric for a v4 spoke and
// the handoff dies with "sso: bad signature" (the incident these tests pin);
// mint Ed25519 for a v2 spoke and every old hive breaks instead.

// TestHiveWantsEd25519SSO_RollingV4Tag is the regression for the outage. Before
// the fix, classification depended on a single frozen commit (12c34e5). Every
// v4 spoke has rolled past that build, so none matched, all silently fell back
// to the legacy symmetric mint, and their users were bounced to the hub login.
func TestHiveWantsEd25519SSO_RollingV4Tag(t *testing.T) {
	entry := &RegistryEntry{ImageRef: "ghcr.io/kubestellar/hive:v4-latest"}
	if !hiveWantsEd25519SSO("hosted-some-rolled-v4-hive", entry) {
		t.Error("a spoke running the rolling v4-latest tag must get an Ed25519 token")
	}
}

// TestHiveWantsEd25519SSO_LegacyFleetUnaffected is the other half of the
// contract and the one that protects the ~60 hives still on v2: anything not
// positively identified as v4 must stay on the legacy symmetric mint.
func TestHiveWantsEd25519SSO_LegacyFleetUnaffected(t *testing.T) {
	for _, img := range []string{
		"ghcr.io/kubestellar/hive:v2-latest",
		"ghcr.io/kubestellar/hive:mk-latest",
		"ghcr.io/kubestellar/hive:dd-latest",
		"", // no image reported at all
	} {
		if hiveWantsEd25519SSO("hosted-some-v2-hive", &RegistryEntry{ImageRef: img}) {
			t.Errorf("image %q must NOT be classified v4 — the legacy fleet cannot verify Ed25519", img)
		}
	}
}

// TestHiveWantsEd25519SSO_NilEntryFailsSafe: a hive the hub has no record for
// is unclassifiable and must fall back to legacy rather than guess. This is
// exactly why the transitional allowlist exists — the image-flipped spokes have
// no registry entry, so the tag check below never runs for them.
func TestHiveWantsEd25519SSO_NilEntryFailsSafe(t *testing.T) {
	if hiveWantsEd25519SSO("hosted-unknown-hive", nil) {
		t.Error("an unknown hive must fail safe to the legacy symmetric mint")
	}
}

// TestHiveWantsEd25519SSO_AllowlistWinsWithoutEntry pins the immediate unblock:
// an allowlisted hive is classified v4 even with no registry entry.
func TestHiveWantsEd25519SSO_AllowlistWinsWithoutEntry(t *testing.T) {
	if !hiveWantsEd25519SSO("hosted-kubestellar-console-4vkt", nil) {
		t.Error("an allowlisted hive must be classified v4 even with a nil entry")
	}
	if !hiveWantsEd25519SSO("hosted-available-oke-11-placeholder-r05x", nil) {
		t.Error("the image-flipped spokes must be classified v4 while they have no registry entry")
	}
}

// TestHiveWantsEd25519SSO_TargetCommitStillMatches proves the fix is ADDITIVE:
// the original frozen-commit signal keeps working for any hive pinned to it.
func TestHiveWantsEd25519SSO_TargetCommitStillMatches(t *testing.T) {
	if !hiveWantsEd25519SSO("hosted-pinned-hive", &RegistryEntry{GitHash: "12c34e5"}) {
		t.Error("the original target-commit signal must keep working")
	}
	if !hiveWantsEd25519SSO("hosted-pinned-hive", &RegistryEntry{ImageRef: "ghcr.io/kubestellar/hive:12c34e5"}) {
		t.Error("the original target-commit image signal must keep working")
	}
}
