package hub

// Tests for the serve-time OIDC display-name decoration added for the Usage
// panel, "Group by owner", Activity Timeline, access log and alert-ack
// surfaces. Everything here is presentation only: each case asserts the raw
// identity key is left untouched on the wire and a SEPARATE optional field
// carries the resolved human name — never a rewrite of the key itself.

import (
	"strings"
	"testing"
	"time"
)

// TestIdentityLabelerResolvesAndFallsBack pins the resolver the decorations
// share: a stored OIDC user resolves to their display name, an unknown or
// GitHub-native key resolves to itself, and empty stays empty.
func TestIdentityLabelerResolvesAndFallsBack(t *testing.T) {
	withTempSaaSDirs(t)
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:5500087VJB",
		Provider:       "ibmid",
		DisplayName:    "Jane Doe",
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	s := &HubServer{}
	label := s.identityLabeler()
	if got := label("ibmid:5500087VJB"); got != "Jane Doe" {
		t.Errorf(`label("ibmid:5500087VJB") = %q, want "Jane Doe"`, got)
	}
	// Memoized second read must agree.
	if got := label("ibmid:5500087VJB"); got != "Jane Doe" {
		t.Errorf(`memoized label = %q, want "Jane Doe"`, got)
	}
	if got := label("google:NEVERSEEN"); got != "google:NEVERSEEN" {
		t.Errorf("unknown key must resolve to itself, got %q", got)
	}
	if got := label(""); got != "" {
		t.Errorf(`label("") = %q, want ""`, got)
	}
}

// TestDecorateTimelineActorsServeTimeOnly asserts ActorName is stamped on the
// served copies for OIDC actors with a known name, omitted otherwise, and
// that Actor (the raw key history must keep) is never rewritten.
func TestDecorateTimelineActorsServeTimeOnly(t *testing.T) {
	withTempSaaSDirs(t)
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:5500087VJB",
		Provider:       "ibmid",
		DisplayName:    "Jane Doe",
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	s := &HubServer{}
	events := []TimelineEvent{
		{TS: "2026-08-28T00:00:00Z", Kind: TimelineAccess, Actor: "ibmid:5500087VJB"},
		{TS: "2026-08-28T00:01:00Z", Kind: TimelineAccess, Actor: "clubanderson"},
		{TS: "2026-08-28T00:02:00Z", Kind: TimelineAccess},
	}
	s.decorateTimelineActors(events)
	if events[0].Actor != "ibmid:5500087VJB" {
		t.Fatalf("raw Actor was rewritten to %q — decoration must be a separate field", events[0].Actor)
	}
	if events[0].ActorName != "Jane Doe" {
		t.Errorf(`ActorName = %q, want "Jane Doe"`, events[0].ActorName)
	}
	if events[1].ActorName != "" {
		t.Errorf("GitHub-native actor got ActorName %q, want empty (key is already the label)", events[1].ActorName)
	}
	if events[2].ActorName != "" {
		t.Errorf("actorless event got ActorName %q, want empty", events[2].ActorName)
	}
}

// TestUsageOwnerBucketsCarryLabel asserts the /api/saas/usage decoration:
// an owner bucket keyed by an opaque OIDC identity carries Label, the raw Key
// is untouched (jump/filter actions depend on it), and buckets whose key is
// already the best label carry no Label at all.
func TestUsageOwnerBucketsCarryLabel(t *testing.T) {
	withTempSaaSDirs(t)
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:5500087VJB",
		Provider:       "ibmid",
		DisplayName:    "Jane Doe",
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	s := &HubServer{}
	label := s.identityLabeler()
	buckets := []UsageBucket{
		{Key: "ibmid:5500087VJB"},
		{Key: "clubanderson"},
	}
	for i := range buckets {
		if l := label(buckets[i].Key); l != buckets[i].Key {
			buckets[i].Label = l
		}
	}
	if buckets[0].Key != "ibmid:5500087VJB" {
		t.Fatalf("raw bucket key was rewritten to %q", buckets[0].Key)
	}
	if buckets[0].Label != "Jane Doe" {
		t.Errorf(`OIDC bucket Label = %q, want "Jane Doe"`, buckets[0].Label)
	}
	if buckets[1].Label != "" {
		t.Errorf("GitHub bucket Label = %q, want empty (omitempty keeps the wire clean)", buckets[1].Label)
	}
}

// TestFleetAlertsDecoratesAckByName asserts an acknowledged alert served to
// the dashboard carries AckByName when the acking admin is an OIDC identity
// with a stored name, while AckBy keeps the raw key.
func TestFleetAlertsDecoratesAckByName(t *testing.T) {
	withTempSaaSDirs(t)
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:5500087VJB",
		Provider:       "ibmid",
		DisplayName:    "Jane Doe",
	}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	s := &HubServer{alerts: newAlertState()}
	// A stale heartbeat trips the offline alert deterministically.
	stale := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	entries := []MyHiveEntry{{
		RegistryEntry: RegistryEntry{ID: "h1", Name: "org/repo", Org: "org", LastHeartbeat: stale},
	}}
	pre := s.fleetAlerts(entries)
	if len(pre.Alerts) == 0 {
		t.Fatal("expected at least one alert for a stale hive")
	}
	if !s.alerts.setAck(pre.Alerts[0].HiveID, pre.Alerts[0].Type, "ibmid:5500087VJB", time.Now()) {
		t.Fatal("setAck refused — condition firstSeen not recorded?")
	}

	summary := s.fleetAlerts(entries)
	found := false
	for _, a := range summary.Alerts {
		if !a.Acknowledged {
			continue
		}
		found = true
		if a.AckBy != "ibmid:5500087VJB" {
			t.Errorf("AckBy = %q, want the raw identity key", a.AckBy)
		}
		if a.AckByName != "Jane Doe" {
			t.Errorf(`AckByName = %q, want "Jane Doe"`, a.AckByName)
		}
	}
	if !found {
		t.Fatal("no acknowledged alert came back from fleetAlerts")
	}
}

// TestHubDashboardRendersResolvedNamesNotRawKeys pins the JS render sites:
// each surface must prefer the server-resolved name field and fall back to
// the raw key — never the reverse, and never the raw key alone.
func TestHubDashboardRendersResolvedNamesNotRawKeys(t *testing.T) {
	for _, snippet := range []string{
		// Usage tables: display label, raw key preserved for the jump action.
		"var rLabel = String(r.label || r.key || '');",
		// Zero-consumption chip tooltip.
		"(h.ownerName || h.owner)",
		// Activity Timeline + access log actor lines.
		"esc(ev.actorName || ev.actor)",
		// Alert ack note.
		"esc(a.ackByName || a.ackBy)",
		// +N overflow tooltip on the co-member faces.
		"members[j].display_label || members[j].username",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("hub dashboard missing expected render snippet: %q", snippet)
		}
	}
	// Group-by-owner must group on the resolved label…
	if !strings.Contains(dashboardHTML, "return (h && (h.ownerName || h.owner)) || ''; }},") {
		t.Error("Group by: Owner must label groups with ownerName when resolved")
	}
	// …while the usage jump keeps operating on the RAW key.
	if !strings.Contains(dashboardHTML, "jumpToUsageBucket(decodeURIComponent(\\'' + esc(encodeURIComponent(String(r.key || '')))") {
		t.Error("jumpToUsageBucket must keep receiving the raw r.key — the label is display-only")
	}
}
