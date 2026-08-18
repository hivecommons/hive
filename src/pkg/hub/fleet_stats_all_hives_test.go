package hub

import (
	"strings"
	"testing"
)

// TestFleetStatsCountAllHivesNotJustPublic pins that aggregate totals cover the
// WHOLE fleet while the public listing stays opt-in.
//
// The filter used to be `if !h.IsPublic || !h.Online { continue }`, so every
// published figure described only the public subset — roughly 18 of 51 live
// spokes. "Hives", "repos managed", "agents running" and the PR/CVE counts all
// understated the fleet by about two thirds, which is the opposite of what a
// fleet total is for.
//
// This is safe because these are ANONYMOUS SUMS: every FleetStats field is a
// scalar, so a private hive contributing to a count discloses nothing about
// itself. Naming a hive is a different disclosure and remains gated on IsPublic
// in the registry listing.
func TestFleetStatsCountAllHivesNotJustPublic(t *testing.T) {
	s := &HubServer{}
	pub := ptrInt(10)
	priv := ptrInt(7)
	s.registry.Hives = []RegistryEntry{
		{ID: "pub", IsPublic: true, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "o", Repos: []string{"r1"}, PRsMerged90d: pub,
			AgentCount: 3, ActiveContributors: 2},
		{ID: "priv", IsPublic: false, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "o", Repos: []string{"r2"}, PRsMerged90d: priv,
			AgentCount: 4, ActiveContributors: 5},
	}

	fs := s.computeFleetStats()

	if fs.Hives != 2 {
		t.Errorf("Hives = %d, want 2 — a private hive is still a hive", fs.Hives)
	}
	if fs.PRsMerged != 17 {
		t.Errorf("PRsMerged = %d, want 17 (10 public + 7 private)", fs.PRsMerged)
	}
	if fs.ReposManaged != 2 {
		t.Errorf("ReposManaged = %d, want 2", fs.ReposManaged)
	}
	if fs.AgentsRunning != 7 {
		t.Errorf("AgentsRunning = %d, want 7 (3+4)", fs.AgentsRunning)
	}
	if fs.Contributors != 7 {
		t.Errorf("Contributors = %d, want 7 (2+5)", fs.Contributors)
	}
	if fs.Eligible != 2 || fs.Reporting != 2 {
		t.Errorf("Reporting/Eligible = %d/%d, want 2/2 — coverage must count private hives too, or the published ratio lies",
			fs.Reporting, fs.Eligible)
	}
}

// TestFleetStatsPublishesNoIdentifiers is the privacy boundary. Widening the
// source to private hives is only defensible while every published field is a
// scalar: a count discloses nothing, a name discloses everything.
//
// If a future change adds a name, org, repo, owner or URL to FleetStats, this
// test is where it should be caught — the struct is serialised straight to a
// public, unauthenticated endpoint.
func TestFleetStatsPublishesNoIdentifiers(t *testing.T) {
	s := &HubServer{}
	s.registry.Hives = []RegistryEntry{
		{ID: "secret-hive", IsPublic: false, Online: true, LastHeartbeat: freshHeartbeat(),
			Org: "acme-internal", Owner: "alice", Name: "Project Nimbus",
			DashboardURL: "https://nimbus.internal.example.com",
			Repos:        []string{"acme-internal/skunkworks"}, PRsMerged90d: ptrInt(4)},
	}

	fs := s.computeFleetStats()
	if fs.PRsMerged != 4 {
		t.Fatalf("the private hive did not contribute: PRsMerged = %d", fs.PRsMerged)
	}

	blob := mustJSON(t, fs)
	for _, secret := range []string{
		"secret-hive", "acme-internal", "alice", "Project Nimbus",
		"nimbus.internal.example.com", "skunkworks",
	} {
		if strings.Contains(strings.ToLower(blob), strings.ToLower(secret)) {
			t.Errorf("fleet-stats response leaked %q — counts may be shared, identifiers may not:\n%s", secret, blob)
		}
	}
}
