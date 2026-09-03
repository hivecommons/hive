package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

// A freshly-approved placeholder still reports its pool org over the heartbeat
// until the spoke adopts the pushed config. It must NOT be classified as
// inventory during that window, or the dashboard parks it under "Unassigned
// hives" right after the operator approved it.
func TestIsPlaceholderEntry_AssignedStillPoolOrg(t *testing.T) {
	h := MyHiveEntry{
		RegistryEntry: RegistryEntry{ID: "hosted-available-vllmd-14", Org: placeholderOrgPrefix + "vllmd-14"},
		ProvStatus:    statusAssigned,
	}
	if isPlaceholderEntry(h) {
		t.Fatal("assigned hive classified as placeholder despite statusAssigned")
	}
	online := h
	online.Online = true
	if !driftEligibleForNorm(online) {
		t.Fatal("assigned online hive excluded from the fleet norm")
	}
}

// The same window seen through the meta-derived signal: a legacy wedge carries
// a null/empty Status but a real identity, which enrichFromSaaSMeta surfaces as
// AssignedUnclaimed.
func TestIsPlaceholderEntry_AssignedUnclaimedPoolOrg(t *testing.T) {
	h := MyHiveEntry{
		RegistryEntry:     RegistryEntry{ID: "hosted-available-vllmd-14", Org: placeholderOrgPrefix + "vllmd-14"},
		AssignedUnclaimed: true,
	}
	if isPlaceholderEntry(h) {
		t.Fatal("assigned-unclaimed hive classified as placeholder")
	}
}

// statusAvailable stays authoritative: a slot that was reset back to the pool is
// inventory again even though AssignedUnclaimed may still be stale on the row.
func TestIsPlaceholderEntry_AvailableWins(t *testing.T) {
	h := MyHiveEntry{
		RegistryEntry:     RegistryEntry{ID: "hosted-available-vllmd-14", Org: "flashsystems"},
		ProvStatus:        statusAvailable,
		AssignedUnclaimed: true,
	}
	if !isPlaceholderEntry(h) {
		t.Fatal("statusAvailable slot not classified as placeholder")
	}
}

// A clean unclaimed slot with no provStatus at all must still fall through to
// the org-prefix fallback.
func TestIsPlaceholderEntry_PrefixFallbackIntact(t *testing.T) {
	h := MyHiveEntry{RegistryEntry: RegistryEntry{ID: "hosted-available-oke-13", Org: placeholderOrgPrefix + "oke-13"}}
	if !isPlaceholderEntry(h) {
		t.Fatal("bare pool-org slot no longer recognised as placeholder")
	}
}

func TestMyHiveEntryUnassignedMirrorsPlaceholderEntry(t *testing.T) {
	cases := []MyHiveEntry{
		{RegistryEntry: RegistryEntry{ID: "hosted-available-oke-13", Org: placeholderOrgPrefix + "oke-13"}},
		{RegistryEntry: RegistryEntry{ID: "hosted-reset", Org: "real-org"}, ProvStatus: statusAvailable},
		{RegistryEntry: RegistryEntry{ID: "hosted-available-vllmd-14", Org: placeholderOrgPrefix + "vllmd-14"}, ProvStatus: statusAssigned},
		{RegistryEntry: RegistryEntry{ID: "hosted-wedged", Org: placeholderOrgPrefix + "wedged"}, AssignedUnclaimed: true},
	}
	for _, h := range cases {
		h.Unassigned = isPlaceholderEntry(h)
		wire, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		hasField := strings.Contains(string(wire), `"unassigned":true`)
		if hasField != isPlaceholderEntry(h) {
			t.Fatalf("hive %s wire unassigned=%v, want %v (%s)", h.ID, hasField, isPlaceholderEntry(h), wire)
		}
	}
}

// The landing-page mirror must move in lockstep with isPlaceholderEntry, or the
// fleet tiles and the dashboard sections disagree about the same hive.
func TestIsAvailableRegistryEntry_LockstepWithPlaceholderEntry(t *testing.T) {
	cases := []RegistryEntry{
		{Org: placeholderOrgPrefix + "vllmd-14", ProvStatus: statusAssigned},
		{Org: placeholderOrgPrefix + "oke-13"},
		{Org: "flashsystems", ProvStatus: statusAvailable},
		{Org: "flashsystems", ProvStatus: statusAssigned},
		{Org: "z-innersource"},
	}
	for _, re := range cases {
		mine := MyHiveEntry{RegistryEntry: re, ProvStatus: re.ProvStatus}
		if got, want := isAvailableRegistryEntry(re), isPlaceholderEntry(mine); got != want {
			t.Fatalf("org=%q provStatus=%q: isAvailableRegistryEntry=%v isPlaceholderEntry=%v", re.Org, re.ProvStatus, got, want)
		}
	}
}

// The embedded dashboard JS is not executed by Go tests, so pin the ordering
// that keeps it in lockstep with the Go predicates: statusAvailable wins before
// assigned/assignedUnclaimed can short-circuit the prefix fallback.
func TestEmbeddedPlaceholderHiveAvailableWins(t *testing.T) {
	bodyStart := strings.Index(dashboardHTML, "function isPlaceholderHive(h) {")
	if bodyStart < 0 {
		t.Fatal("isPlaceholderHive not found")
	}
	bodyEnd := strings.Index(dashboardHTML[bodyStart:], "function hiveNamespace")
	if bodyEnd < 0 {
		t.Fatal("isPlaceholderHive body end not found")
	}
	body := dashboardHTML[bodyStart : bodyStart+bodyEnd]
	availableAt := strings.Index(body, "h.provStatus === 'available'")
	assignedAt := strings.Index(body, "h.provStatus === 'assigned'")
	if availableAt < 0 || assignedAt < 0 {
		t.Fatalf("expected available and assigned checks in isPlaceholderHive, got body:\n%s", body)
	}
	if availableAt > assignedAt {
		t.Fatal("isPlaceholderHive checks assigned before available; statusAvailable must remain authoritative")
	}
}

// A placeholder that has been assigned is a real hive, so its failing
// auth-class checks must stop being sanitized away to green — the row should
// surface as claim-pending, not as healthy inventory.
func TestSanitizePlaceholderRows_SkipsAssigned(t *testing.T) {
	rows := []MyHiveEntry{{
		RegistryEntry: RegistryEntry{
			ID:     "hosted-available-vllmd-14",
			Org:    placeholderOrgPrefix + "vllmd-14",
			Health: map[string]any{"status": "degraded"},
		},
		ProvStatus: statusAssigned,
	}}
	sanitizePlaceholderRows(rows)
	if got := rows[0].Health["status"]; got != "degraded" {
		t.Fatalf("assigned hive health sanitized: got %v, want degraded", got)
	}
}
