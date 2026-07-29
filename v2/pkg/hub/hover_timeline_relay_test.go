package hub

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The consolidated status hover (health + relay + access + recent activity)
// is built in the inline dashboardHTML JS, so most of what can break here is
// invisible to the Go compiler. These tests pin the pieces that matter.

// TestHoverPanelRendersConsolidatedSections asserts the panel composes every
// section. A rebase that drops one leaves the hover silently missing a surface
// rather than failing anywhere else.
func TestHoverPanelRendersConsolidatedSections(t *testing.T) {
	cases := []struct {
		name    string
		snippet string
	}{
		{"relay line is composed into the panel", "relayLine(h) +"},
		{"recent activity is composed into the panel", "hoverEventRows(h) + '</span></span>';"},
		{"relay helper exists", "function relayLine(h)"},
		{"recent activity helper exists", "function hoverEventRows(h)"},
		// The events must come from the payload, never a per-hover fetch.
		{"events read from the payload", "(h.recentEvents || [])"},
		// Relay state is read from the SAME field the Journey column uses.
		{"relay reads journey.viaRelay", "if (!j.viaRelay) return '';"},
		{"contributor count is shown", "registered + ' contributors'"},
		// "View all" must reach the existing modal.
		{"view-all opens the timeline modal", "openTimelineModal(btn.getAttribute('data-hive-id')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// TestHoverPanelDoesNotFetchOnHover guards the design decision: recent activity
// rides the My Hives payload, so hovering rows must never issue a request. A
// fetch inside the hover helpers would be a request storm at fleet scale.
func TestHoverPanelDoesNotFetchOnHover(t *testing.T) {
	start := strings.Index(dashboardHTML, "function hoverEventRows(h)")
	if start < 0 {
		t.Fatal("hoverEventRows not found in dashboardHTML")
	}
	end := strings.Index(dashboardHTML[start:], "\n    function healthBadge(h)")
	if end < 0 {
		t.Fatal("could not bound hoverEventRows")
	}
	body := dashboardHTML[start : start+end]
	for _, bad := range []string{"fetch(", "XMLHttpRequest"} {
		if strings.Contains(body, bad) {
			t.Errorf("hoverEventRows performs %q — recent activity must come from "+
				"the My Hives payload, not a per-hover request", bad)
		}
	}
}

// TestHoverViewAllUsesDelegationNotInlineHandler guards against building an
// inline onclick from the hive name. esc() does not escape apostrophes, so a
// name containing one would break out of the handler string.
func TestHoverViewAllUsesDelegationNotInlineHandler(t *testing.T) {
	if !strings.Contains(dashboardHTML, `class="hover-view-timeline" `) {
		t.Error("view-all button is missing its delegation class")
	}
	// Scoped to hoverEventRows: the row's kebab menu legitimately builds an
	// inline openTimelineModal handler elsewhere, so a whole-file search would
	// match that instead of the button under test.
	start := strings.Index(dashboardHTML, "function hoverEventRows(h)")
	if start < 0 {
		t.Fatal("hoverEventRows not found in dashboardHTML")
	}
	end := strings.Index(dashboardHTML[start:], "\n    function healthBadge(h)")
	if end < 0 {
		t.Fatal("could not bound hoverEventRows")
	}
	if body := dashboardHTML[start : start+end]; strings.Contains(body, "onclick=") {
		t.Error("hover view-all button builds an inline handler; the hive name is " +
			"user-controlled and esc() does not escape apostrophes")
	}
	if !strings.Contains(dashboardHTML, "e.target.closest('.hover-view-timeline')") {
		t.Error("no delegated listener for the view-all button")
	}
}

// TestHoverViewAllEscapesAttributes guards the attribute-injection vector on
// the view-all button. The hive id and name are spoke-reported and land inside
// double-quoted data attributes. esc() goes through textContent -> innerHTML,
// which escapes & < > but NOT quotes, so esc() alone would let a name
// containing a '"' close the attribute and inject an event handler. The values
// must go through escAttr().
func TestHoverViewAllEscapesAttributes(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function escAttr(s)") {
		t.Fatal("escAttr helper is missing; attribute contexts have no quote-safe escaper")
	}
	for _, want := range []string{
		`'data-hive-id="' + escAttr(h.id) + '"`,
		`data-hive-name="' + escAttr(h.name || h.id) + '"`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("view-all button does not escape its attribute with escAttr: missing %q", want)
		}
	}
	// escAttr must actually neutralise both quote characters.
	for _, want := range []string{`replace(/"/g, '&quot;')`, `replace(/'/g, '&#39;')`} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("escAttr does not neutralise a quote character: missing %q", want)
		}
	}
}

// TestRelayRenderingIsConsistentWithJourneyColumn asserts the hover and the
// Journey column are driven by the same signal and the same colour, so one
// hive can never appear to say two different things in two places.
func TestRelayRenderingIsConsistentWithJourneyColumn(t *testing.T) {
	// Both surfaces key off journey.viaRelay.
	if !strings.Contains(dashboardHTML, "if (j.viaRelay) {") {
		t.Error("Journey column no longer keys off j.viaRelay")
	}
	if !strings.Contains(dashboardHTML, "if (!j.viaRelay) return '';") {
		t.Error("hover relay line no longer keys off j.viaRelay")
	}
	// The hover uses the same teal as the .journey-relay badge.
	if !strings.Contains(dashboardHTML, "var RELAY_COLOR = '#2dd4bf';") {
		t.Error("hover relay colour constant missing")
	}
	if !strings.Contains(dashboardHTML, "color: #2dd4bf;") {
		t.Error(".journey-relay badge colour changed; the hover's RELAY_COLOR must track it")
	}
	// The relay must read as its own state, not folded into "assigned"/"OK".
	if !strings.Contains(dashboardHTML, "satisfies the method/model requirement") {
		t.Error("hover does not explain that the relay satisfies the method/model requirement")
	}
}

// TestRelayInUseDrivesHoverSignal is a table-driven check of the shared
// relayInUse() helper this feature reuses from journey.go, covering the
// registered / active / in-flight-task paths the hover copy branches on.
func TestRelayInUseDrivesHoverSignal(t *testing.T) {
	cases := []struct {
		name string
		h    *RegistryEntry
		want bool
	}{
		{"nil entry", nil, false},
		{"no contributors at all", &RegistryEntry{}, false},
		{"registered contributors", &RegistryEntry{ContributorCount: 3}, true},
		{"only live contributors", &RegistryEntry{ActiveContributors: 1}, true},
		{"leaderboard task in flight", &RegistryEntry{
			Leaderboard: []LeaderboardEntry{{CurrentTask: "fix-123"}},
		}, true},
		{"leaderboard with blank tasks only", &RegistryEntry{
			Leaderboard: []LeaderboardEntry{{CurrentTask: "   "}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayInUse(tc.h); got != tc.want {
				t.Errorf("relayInUse() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMyHivesEmbedsRecentEvents asserts the payload carries the hover's events
// under the same owner/admin rule as the access list, and trims to the
// documented count.
func TestMyHivesEmbedsRecentEvents(t *testing.T) {
	const owner = "hover-events-owner"
	const hiveID = "hosted-relaytest"
	const token = "ghp_hover_events"

	dir := t.TempDir()
	origUsers, origTimeline := saasUsersDir, timelineDir
	saasUsersDir = dir + "/users"
	timelineDir = dir + "/timeline"
	t.Cleanup(func() { saasUsersDir, timelineDir = origUsers, origTimeline })

	cleanup := helperSetupAuthUser(t, token, owner)
	defer cleanup()

	s := NewHubServer(0, slog.Default(), "test", "v2")
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{{
		ID:            hiveID,
		Name:          "acme/widget",
		Owner:         owner,
		Online:        true,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}
	s.mu.Unlock()

	// More events than the hover shows, so the trim is exercised.
	for i := 0; i < myHivesRecentEventCount+4; i++ {
		s.timeline.append(hiveID, TimelineHealthChanged, "health flipped", "")
	}

	req := httptest.NewRequest("GET", "/api/saas/my-hives", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handleMyHives(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handleMyHives = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got *MyHiveEntry
	for i := range resp.Hives {
		if resp.Hives[i].ID == hiveID {
			got = &resp.Hives[i]
		}
	}
	if got == nil {
		t.Fatalf("hive %s missing from my-hives response", hiveID)
	}
	if len(got.RecentEvents) != myHivesRecentEventCount {
		t.Errorf("RecentEvents = %d events, want %d (the hover's cap)",
			len(got.RecentEvents), myHivesRecentEventCount)
	}
}
