package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// opsPageBody renders the /contribute page and returns its HTML body. A local
// helper so this file stays self-contained alongside the other dashboard-structure
// tests (mirrors contributePageBody's shape).
func opsPageBody(t *testing.T) string {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// TestQueueNoBlinkGuards pins the ready-work queue no-blink / menu-survival fix (A):
//   - a poll re-render is SKIPPED while a ⋯ menu is open (the guard + deferred flag),
//     so the open "Move to #" dialog is not wiped mid-interaction;
//   - the deferred render is replayed when the menu closes (ccCloseQueueMenus);
//   - the cc-popin enter animation is gated to GENUINELY-new items only (a seen-key
//     set), so existing rows never re-animate on a poll.
func TestQueueNoBlinkGuards(t *testing.T) {
	body := opsPageBody(t)
	for _, want := range []string{
		// The seen-key set that gates the enter animation to new arrivals only.
		"ccKnownQueueKeys",
		// The opt-in enter class + its CSS modifier (replacing the unconditional anim).
		"cc-q-enter",
		".cc-q-item.cc-q-enter{animation:cc-popin",
		// The deferred-render flag + the skip-while-menu-open guard.
		"ccQueueRenderDeferred",
		".cc-q-menu.open",
		// The guard early-returns from ccRenderQueue on a poll (falsy flip) while a
		// menu is open, but still updates the header tally above it.
		"ccQueueRenderDeferred=true;return;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue no-blink guard missing %q", want)
		}
	}
	// The enter animation must NOT be baked unconditionally onto every .cc-q-item any
	// more (that was the blink). The base rule must not carry the cc-popin animation.
	base := "\n.cc-q-item{display:flex;align-items:flex-start;gap:10px;padding:11px 20px;border-bottom:1px solid var(--cc-border-2);position:relative}"
	if !strings.Contains(body, base) {
		t.Errorf("base .cc-q-item rule must not carry the unconditional cc-popin animation (found no animation-free base rule)")
	}
	// ccCloseQueueMenus must replay the deferred render so the queue catches up once
	// the operator closes the menu.
	iClose := strings.Index(body, "function ccCloseQueueMenus")
	iReplay := strings.Index(body, "ccQueueRenderDeferred=false;try{ccRenderQueue();}")
	if iClose < 0 || iReplay < 0 || iReplay < iClose {
		t.Errorf("ccCloseQueueMenus must replay the deferred render: close=%d replay=%d", iClose, iReplay)
	}
}

// TestOwnerLabelAffinityRoster pins the owner-facing Label interests summary (B):
//   - the always-visible explainer with the ⓘ info affordance + its exact copy;
//   - the actionable per-label aggregate render (ccRenderInterestRoster) driven from
//     the fleet payload's per-clanker label_interests, sorted by descending count;
//   - the graceful empty state when no connected contributor has set interests.
func TestOwnerLabelAffinityRoster(t *testing.T) {
	body := opsPageBody(t)
	for _, want := range []string{
		// The card + its info affordance (reusing the info-btn / info-pop pattern).
		`id="label-affinity"`,
		`id="affinity-info-btn"`,
		`id="affinity-info-pop"`,
		`aria-controls="affinity-info-pop"`,
		`class="info-btn"`,
		`function _wireAffinityInfo(`,
		// The explainer copy called out in the issue.
		`Contributors subscribe to labels`,
		`a soft hint, nothing is hidden`,
		// The aggregate render + its hook into the fleet render path.
		`function ccRenderInterestRoster(`,
		`ccRenderInterestRoster(list)`,
		`c.label_interests`,
		`affinity-chip`,
		`affinity-count`,
		`affinity-who`,
		// The graceful empty state.
		`No contributors have set label interests yet`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owner label-affinity roster missing %q", want)
		}
	}
	// The roster is hydrated from the fleet snapshot each poll — the render helper must
	// be invoked from renderClankers (which opsPoll calls every tick).
	iClankers := strings.Index(body, "function renderClankers")
	iInvoke := strings.Index(body, "ccRenderInterestRoster(list)")
	if iClankers < 0 || iInvoke < 0 || iInvoke < iClankers {
		t.Errorf("ccRenderInterestRoster must be invoked from renderClankers: renderClankers=%d invoke=%d", iClankers, iInvoke)
	}
}
