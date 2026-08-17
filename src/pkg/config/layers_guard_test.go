package config

import (
	"strings"
	"testing"
)

// Plausibility-guard tests.
//
// The risk runs in BOTH directions and the tests are organised that way:
//
//   - Rejecting a VALID overlay is severe and silent: the hive boots on the
//     bare ConfigMap seed, which on the live fleet is 4–11% of the running
//     config and omits whole top-level blocks. A false rejection can bring a
//     hive up with a fraction of its agent roster.
//   - Accepting a CORRUPT overlay defeats the guard's reason to exist and
//     lets a truncated write become the running config.
//
// Every test below states which direction it protects.

// ─────────────────────────────────────────────────────────────────────────
// DIRECTION 1: valid overlays must be ACCEPTED
// ─────────────────────────────────────────────────────────────────────────

// TestZeroAgentOverlayWithTombstonesIsValid is the case that motivated this
// change. #2361 made agent deletion durable via RemovedAgents tombstones, so
// an operator who deletes their last agent produces a legitimately zero-agent
// overlay. The old guard called that corrupt and discarded it, booting from a
// seed that still lists the deleted agents — silently undoing the deletion,
// which is exactly the bug #2361 fixed.
func TestZeroAgentOverlayWithTombstonesIsValid(t *testing.T) {
	overlay := &Config{
		Project:       ProjectConfig{Org: "acme"},
		Agents:        map[string]AgentConfig{},
		RemovedAgents: []string{"scanner", "guide"},
	}

	if !OverlayIsPlausible(overlay) {
		t.Fatalf("a zero-agent overlay with tombstones was rejected (%q); "+
			"deleting the last agent is a legitimate operator action since #2361",
			overlayRejectReason(overlay))
	}

	seed := &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents:  map[string]AgentConfig{"scanner": {}, "guide": {}},
	}
	merged, prov := MergeLayers(seed, overlay)

	if prov.OverlayRejected {
		t.Error("OverlayRejected = true for a valid tombstoned overlay")
	}
	// The deletion must survive the merge; resurrecting the agents from the
	// seed is the regression this guards.
	if len(merged.Agents) != 0 {
		t.Errorf("merged roster has %d agents, want 0 — the seed resurrected deleted agents",
			len(merged.Agents))
	}
}

// TestSaveGuardAllowsAllTombstonedConfig covers the same conflict on the WRITE
// path. If validateSaveGuard refuses an all-tombstoned config, the last
// deletion cannot be persisted at all: the roster empties in memory, the save
// is refused, and the next reload restores the agents.
func TestSaveGuardAllowsAllTombstonedConfig(t *testing.T) {
	c := &Config{
		Project:       ProjectConfig{Org: "acme"},
		Agents:        map[string]AgentConfig{},
		RemovedAgents: []string{"scanner"},
	}
	if err := c.validateSaveGuard(); err != nil {
		t.Fatalf("validateSaveGuard rejected an all-tombstoned config: %v; "+
			"the operator's last deletion would be unpersistable", err)
	}
}

// TestMinimalButCompleteOverlayIsValid: a hive mid-provision, or a deliberately
// spare one, carries only project.org and a single agent. Nothing about that is
// corrupt, and the guard must not demand richness.
func TestMinimalButCompleteOverlayIsValid(t *testing.T) {
	overlay := &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents:  map[string]AgentConfig{"scanner": {}},
	}
	if !OverlayIsPlausible(overlay) {
		t.Errorf("a minimal but complete overlay was rejected: %q", overlayRejectReason(overlay))
	}
}

// TestOverlayWithoutOptionalBlocksIsValid: policies, data, knowledge,
// notifications and hive_id are all absent from many real overlays. Requiring
// any of them would reject valid configs fleet-wide.
func TestOverlayWithoutOptionalBlocksIsValid(t *testing.T) {
	overlay := &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents:  map[string]AgentConfig{"scanner": {}},
		// No Policies, Data, Knowledge, Notifications, HiveID, GitHub, Hub.
	}
	if !OverlayIsPlausible(overlay) {
		t.Errorf("an overlay lacking optional blocks was rejected: %q",
			overlayRejectReason(overlay))
	}
}

// TestPausedAgentsOverlayIsValid: every agent paused is a normal operator
// state and leaves a fully populated roster.
func TestPausedAgentsOverlayIsValid(t *testing.T) {
	overlay := &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents: map[string]AgentConfig{
			"scanner": {Paused: true},
			"guide":   {Paused: true},
		},
	}
	if !OverlayIsPlausible(overlay) {
		t.Errorf("an all-paused overlay was rejected: %q", overlayRejectReason(overlay))
	}
}

// ─────────────────────────────────────────────────────────────────────────
// DIRECTION 2: corrupt overlays must still be REJECTED
// ─────────────────────────────────────────────────────────────────────────

// TestTruncatedOverlayIsRejected: the case the guard exists for. A write
// interrupted by SIGKILL can leave a file with no project.org.
func TestTruncatedOverlayIsRejected(t *testing.T) {
	overlay := &Config{Agents: map[string]AgentConfig{"scanner": {}}}
	if OverlayIsPlausible(overlay) {
		t.Fatal("an overlay with no project.org was accepted; a truncated write " +
			"must not become the running config")
	}
	if r := overlayRejectReason(overlay); !strings.Contains(r, "project.org") {
		t.Errorf("reason = %q, want it to name project.org", r)
	}
}

// TestEmptyOverlayWithoutTombstonesIsRejected: zero agents AND no tombstones
// is the truncated case, not a deliberate deletion. The distinction between
// this and TestZeroAgentOverlayWithTombstonesIsValid is the whole point of the
// new predicate — if these two ever agree, the guard has lost its meaning.
func TestEmptyOverlayWithoutTombstonesIsRejected(t *testing.T) {
	overlay := &Config{Project: ProjectConfig{Org: "acme"}}
	if OverlayIsPlausible(overlay) {
		t.Fatal("an overlay with no agents and no tombstones was accepted; " +
			"that is the truncated-write signature")
	}
	if r := overlayRejectReason(overlay); !strings.Contains(r, "removed_agents") {
		t.Errorf("reason = %q, want it to explain the tombstone distinction", r)
	}
}

// TestSaveGuardStillBlocksEmptyConfig: the write-path twin of the above.
func TestSaveGuardStillBlocksEmptyConfig(t *testing.T) {
	c := &Config{Project: ProjectConfig{Org: "acme"}}
	if err := c.validateSaveGuard(); err == nil {
		t.Fatal("validateSaveGuard accepted a config with no agents and no tombstones")
	}
}

// TestRejectionReasonIsSpecific: a generic bail is what made this condition
// undiagnosable. The reason must name the failing field.
func TestRejectionReasonIsSpecific(t *testing.T) {
	cases := []struct {
		name    string
		overlay *Config
		want    string
	}{
		{"no org", &Config{Agents: map[string]AgentConfig{"a": {}}}, "project.org"},
		{"no agents", &Config{Project: ProjectConfig{Org: "acme"}}, "removed_agents"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := overlayRejectReason(tc.overlay)
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Fail-safe fallback (opt-in, behaviour change — see MergeLayersWithFallback)
// ─────────────────────────────────────────────────────────────────────────

// TestFallbackPrefersLastGoodOverSparseSeed: falling back to the seed is
// failing empty. The rolling backup is much closer to what the hive was
// actually running.
func TestFallbackPrefersLastGoodOverSparseSeed(t *testing.T) {
	seed := []byte("project:\n  org: acme\nagents:\n  scanner: {}\n")
	badOverlay := []byte("agents:\n  scanner: {}\n") // no project.org
	lastGood := []byte("project:\n  org: acme\nagents:\n  scanner: {}\n  guide: {}\n  quality: {}\n")

	merged, prov, err := MergeLayersWithFallback(seed, badOverlay, lastGood)
	if err != nil {
		t.Fatalf("MergeLayersWithFallback: %v", err)
	}
	if !prov.OverlayRejected {
		t.Error("OverlayRejected = false, want the rejection still reported")
	}
	if !prov.LastGoodUsed {
		t.Fatal("LastGoodUsed = false, want the recovery to be reported")
	}
	if len(merged.Agents) != 3 {
		t.Errorf("merged roster = %d agents, want the last-good 3 rather than the seed's 1",
			len(merged.Agents))
	}
}

// TestFallbackIgnoresCorruptLastGood: never trade a valid seed for a backup
// that is itself implausible.
func TestFallbackIgnoresCorruptLastGood(t *testing.T) {
	seed := []byte("project:\n  org: acme\nagents:\n  scanner: {}\n")
	badOverlay := []byte("agents:\n  scanner: {}\n")
	corruptLastGood := []byte("agents:\n  guide: {}\n") // no project.org either

	merged, prov, err := MergeLayersWithFallback(seed, badOverlay, corruptLastGood)
	if err != nil {
		t.Fatalf("MergeLayersWithFallback: %v", err)
	}
	if prov.LastGoodUsed {
		t.Error("LastGoodUsed = true for a corrupt backup, want the seed kept")
	}
	if len(merged.Agents) != 1 {
		t.Errorf("merged roster = %d agents, want the seed's 1", len(merged.Agents))
	}
}

// TestFallbackInertWhenOverlayIsValid: the fallback must never fire on the
// healthy path, or it would mask the operator's actual overlay.
func TestFallbackInertWhenOverlayIsValid(t *testing.T) {
	seed := []byte("project:\n  org: acme\nagents:\n  scanner: {}\n")
	goodOverlay := []byte("project:\n  org: acme\nagents:\n  scanner: {}\n  guide: {}\n")
	lastGood := []byte("project:\n  org: acme\nagents:\n  stale: {}\n")

	merged, prov, err := MergeLayersWithFallback(seed, goodOverlay, lastGood)
	if err != nil {
		t.Fatalf("MergeLayersWithFallback: %v", err)
	}
	if prov.LastGoodUsed || prov.OverlayRejected {
		t.Error("fallback fired for a valid overlay")
	}
	if _, ok := merged.Agents["stale"]; ok {
		t.Error("the last-good config leaked into a healthy merge")
	}
}
