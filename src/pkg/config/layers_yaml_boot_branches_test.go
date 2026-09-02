package config

import (
	"strings"
	"testing"
)

// Boot-path branch tests for MergeLayersYAML / MergeLayersWithFallback
// (pkg/config/layers_yaml.go) plus the PauseIsOperatorOwned ownership
// predicate (pkg/config/config.go).
//
// These pin the DEGRADED-boot branches the existing layers_guard_test.go
// suite does not reach: an unparsable seed, a missing overlay, an overlay
// that is not YAML at all (as opposed to parseable-but-implausible), an
// unparsable last-good backup, and the unparsable-probe branch of
// seedHasIsPublic. Every one of these decides what config a hive boots
// with after a bad write, so each branch's contract is pinned explicitly.

const bootSeedYAML = "hub:\n  is_public: true\nproject:\n  org: acme\nagents:\n  scanner: {}\n"

// TestMergeLayersYAMLSeedUnparsable: a seed that is not YAML must surface an
// error rather than booting an empty config. The seed is the ConfigMap the
// operator provisioned; silently proceeding without it would run a hive with
// no identity at all.
func TestMergeLayersYAMLSeedUnparsable(t *testing.T) {
	cfg, prov, err := MergeLayersYAML([]byte(":\tnot yaml ["), nil)
	if err == nil {
		t.Fatal("MergeLayersYAML accepted an unparsable seed, want an error")
	}
	if cfg != nil || prov != nil {
		t.Errorf("cfg=%v prov=%v for an unparsable seed, want nil,nil", cfg, prov)
	}
}

// TestMergeLayersYAMLNoOverlay: an empty overlay means "no overlay on disk"
// (first boot). The seed must be used as-is and the provenance must NOT
// report a rejection — there was nothing to reject.
func TestMergeLayersYAMLNoOverlay(t *testing.T) {
	cfg, prov, err := MergeLayersYAML([]byte(bootSeedYAML), nil)
	if err != nil {
		t.Fatalf("MergeLayersYAML: %v", err)
	}
	if prov.OverlayRejected {
		t.Error("OverlayRejected = true with no overlay present, want false")
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("seed agent missing from merged config on the no-overlay path")
	}
}

// TestMergeLayersYAMLMalformedOverlay: an overlay that fails to PARSE (a
// truncated or garbage file, distinct from parseable-but-implausible) must be
// treated as absent — boot from the seed, and report the rejection with the
// parse error so the substitution is never silent. Failing the boot instead
// would turn one bad write into a down hive.
func TestMergeLayersYAMLMalformedOverlay(t *testing.T) {
	cfg, prov, err := MergeLayersYAML([]byte(bootSeedYAML), []byte("{{ not yaml"))
	if err != nil {
		t.Fatalf("MergeLayersYAML returned an error for a malformed overlay, want seed fallback: %v", err)
	}
	if !prov.OverlayRejected {
		t.Fatal("OverlayRejected = false for a malformed overlay, want true")
	}
	if !strings.Contains(prov.OverlayRejectReason, "overlay is not valid YAML") {
		t.Errorf("OverlayRejectReason = %q, want it to name the YAML parse failure",
			prov.OverlayRejectReason)
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("seed agent missing: malformed overlay did not fall back to the seed")
	}
}

// TestFallbackIgnoresUnparsableLastGood: when the overlay is rejected AND the
// rolling backup is itself unparsable, the seed must be kept. An unparsable
// backup is no better than the seed, and LastGoodUsed must stay false so
// nobody believes a recovery happened.
func TestFallbackIgnoresUnparsableLastGood(t *testing.T) {
	badOverlay := []byte("agents:\n  scanner: {}\n") // no project.org → rejected
	merged, prov, err := MergeLayersWithFallback(
		[]byte(bootSeedYAML), badOverlay, []byte(":\tnot yaml ["))
	if err != nil {
		t.Fatalf("MergeLayersWithFallback: %v", err)
	}
	if !prov.OverlayRejected {
		t.Error("OverlayRejected = false, want the rejection still reported")
	}
	if prov.LastGoodUsed {
		t.Error("LastGoodUsed = true for an unparsable backup, want the seed kept")
	}
	if len(merged.Agents) != 1 {
		t.Errorf("merged roster = %d agents, want the seed's 1", len(merged.Agents))
	}
}

// TestSeedHasIsPublicUnparsableSeed: an unparsable seed must report
// is_public as PRESENT — every provisioned seed on the fleet emits
// hub.is_public, so "present" is the assumption that keeps the merged
// config's visibility semantics unchanged when the probe cannot run.
func TestSeedHasIsPublicUnparsableSeed(t *testing.T) {
	if !seedHasIsPublic([]byte(":\tnot yaml [")) {
		t.Error("seedHasIsPublic = false for an unparsable seed, want true (assume present)")
	}
	if !seedHasIsPublic([]byte(bootSeedYAML)) {
		t.Error("seedHasIsPublic = false for a seed that spells hub.is_public")
	}
	if seedHasIsPublic([]byte("project:\n  org: acme\n")) {
		t.Error("seedHasIsPublic = true for a seed without hub.is_public")
	}
}

// TestPauseIsOperatorOwned pins the ownership predicate the ACMM pack
// visibility sweep consults before pausing an agent (#5706): only the exact
// "operator" marker grants immunity; empty, unknown, and differently-cased
// owners do not.
func TestPauseIsOperatorOwned(t *testing.T) {
	cases := []struct {
		owner string
		want  bool
	}{
		{FieldOwnerOperator, true},
		{"", false},
		{"pack", false},
		{"Operator", false}, // marker is exact, not case-insensitive
	}
	for _, tc := range cases {
		got := AgentConfig{PauseOwner: tc.owner}.PauseIsOperatorOwned()
		if got != tc.want {
			t.Errorf("PauseIsOperatorOwned() with PauseOwner=%q = %v, want %v",
				tc.owner, got, tc.want)
		}
	}
}
