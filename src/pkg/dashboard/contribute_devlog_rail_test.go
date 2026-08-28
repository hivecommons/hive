package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Development log was moved out of the Operations right column into a dedicated,
// full-height, collapsible RIGHT RAIL (chat/notifications-panel style): open by
// default, collapse state persisted in localStorage, main area widens on collapse.
// These tests pin that restructure so it cannot silently regress:
//   1. the two-region shell (main area + rail) and the rail markup render;
//   2. the collapse toggle exists and defaults OPEN (aria-expanded="true");
//   3. the collapse choice reads/writes the localStorage key and defaults open;
//   4. the SSE feed handlers + travel animation are still wired;
//   5. the MAIN-area panels (clankers / pipeline / queue / my-work) still render and
//      the travel animation's source (queue) + target (clanker rows) stay in main.

// renderContributePage is a small helper: GET /contribute and return the body.
func renderContributePage(t *testing.T) string {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	return rec.Body.String()
}

// TestOpsDevlogRailMarkup asserts the two-region shell + rail markup + collapse
// toggle are present, and that the dev-log feed container lives INSIDE the rail.
func TestOpsDevlogRailMarkup(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`class="ops-shell"`,        // two-region flex shell
		`class="ops-main"`,         // main area (fleet / pipeline / queue / my-work)
		`class="ops-rail"`,         // dedicated full-height dev-log rail
		`id="ops-rail"`,            // rail element (JS target)
		`id="ops-rail-toggle"`,     // the collapse toggle/handle
		`aria-controls="ops-rail"`, // toggle controls the rail (a11y)
		`Live Activity`,            // rail heading — renamed for consistency with Onboarding (#2591)
		`id="cc-log"`,              // the feed container itself
		`Watching the hive`,        // the empty state, preserved
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing dev-log-rail marker %q", want)
		}
	}

	// The dev-log feed (#cc-log) must sit INSIDE the rail (<aside class="ops-rail">),
	// not back in the main column. Assert the rail open tag precedes #cc-log and the
	// rail close tag (</aside>) follows it.
	railOpen := strings.Index(body, `class="ops-rail"`)
	ccLog := strings.Index(body, `id="cc-log"`)
	railClose := strings.Index(body, `</aside>`)
	if railOpen < 0 || ccLog < 0 || railClose < 0 {
		t.Fatalf("missing rail/feed structure (rail=%d cc-log=%d aside-close=%d)", railOpen, ccLog, railClose)
	}
	if railOpen >= ccLog || ccLog >= railClose {
		t.Errorf("dev-log #cc-log is not nested inside the .ops-rail <aside> (rail=%d cc-log=%d close=%d)", railOpen, ccLog, railClose)
	}
}

// TestOpsDevlogRailDefaultsOpenAndPersists pins the behavior contract: the toggle
// starts aria-expanded="true" (OPEN by default) and the collapse JS reads/writes the
// documented localStorage key, honouring it on load.
func TestOpsDevlogRailDefaultsOpenAndPersists(t *testing.T) {
	body := renderContributePage(t)

	// Open by default: the toggle ships aria-expanded="true".
	if !strings.Contains(body, `id="ops-rail-toggle" aria-expanded="true"`) {
		t.Error("rail toggle does not default to aria-expanded=\"true\" (rail must be OPEN on first load)")
	}

	// The persistence wiring: the documented key, an init that applies the remembered
	// choice, and a read/write pair that the toggle calls.
	for _, want := range []string{
		`hive.ops.devlog.collapsed`,            // the documented localStorage key
		`function initOpsRail`,                 // wires the toggle + applies remembered state
		`localStorage.getItem(OPS_RAIL_KEY)`,   // reads the remembered choice
		`localStorage.setItem(OPS_RAIL_KEY`,    // persists a collapse
		`localStorage.removeItem(OPS_RAIL_KEY`, // persists an expand (key cleared)
		`ccRailApply(rail,btn,ccRailRead())`,   // applies the remembered choice on init
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rail persistence wiring missing %q", want)
		}
	}

	// initOpsRail must be started when the Operations tab activates — and guarded so a
	// throw cannot abort fleet hydration or the SSE feed (regression #2574 invariant).
	if !strings.Contains(body, `try{initOpsRail();}catch`) {
		t.Error("initOpsRail() is not started under an independent try/catch in the Operations tab activation")
	}
}

// TestOpsDevlogRailFeedAndMainIntact proves the move did NOT break the SSE feed, the
// travel animation, or the main-area panels — the whole point of relocating rather
// than rewriting.
func TestOpsDevlogRailFeedAndMainIntact(t *testing.T) {
	body := renderContributePage(t)

	// SSE feed + narration + fallback: still wired, just relocated into the rail.
	for _, want := range []string{
		`/api/contribute/events`, // SSE endpoint
		`EventSource`,            // stream client
		`function ccStart`,       // SSE lifecycle
		`function ccRenderLog`,   // narrated-line render
		`function ccNarrate`,     // ActivityEntry -> line
		`function ccSetLive`,     // live/connecting/polling status pill
		`/api/contribute/queue`,  // graceful poll fallback
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE/feed wiring lost after the rail move: missing %q", want)
		}
	}

	// The travel animation and BOTH its endpoints must remain in the MAIN area:
	// source = #cc-queue, target = .clanker-row[data-clanker]. Both live in .ops-main
	// (the rail holds only the dev-log), so the flight path still resolves.
	mainStart := strings.Index(body, `class="ops-main"`)
	railStart := strings.Index(body, `class="ops-rail"`)
	if mainStart < 0 || railStart < 0 || mainStart >= railStart {
		t.Fatalf("expected .ops-main to precede .ops-rail (main=%d rail=%d)", mainStart, railStart)
	}
	queuePos := strings.Index(body, `id="cc-queue"`)
	clankerPos := strings.Index(body, `id="clanker-list"`)
	if queuePos < mainStart || queuePos >= railStart {
		t.Errorf("travel-animation source #cc-queue is not inside the main area (pos=%d, main=%d, rail=%d)", queuePos, mainStart, railStart)
	}
	if clankerPos < mainStart || clankerPos >= railStart {
		t.Errorf("travel-animation target list #clanker-list is not inside the main area (pos=%d, main=%d, rail=%d)", clankerPos, mainStart, railStart)
	}
	if !strings.Contains(body, `function ccTravel`) || !strings.Contains(body, `.clanker-row[data-clanker=`) {
		t.Error("travel animation (ccTravel / clanker-row target selector) missing after the rail move")
	}

	// Main-area panels: fleet / pipeline / queue / my-work all still render with their
	// placeholders (the panels we kept in the main area).
	for _, want := range []string{
		`id="clanker-list"`, `Loading fleet`,
		`id="policy-body"`, `Loading policy`,
		`id="cc-queue"`, `Ready-work queue`,
		`id="work-list"`, `Loading work`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("main-area panel marker lost after the rail move: %q", want)
		}
	}

	// The owner/RW gating invariant on the controls is untouched.
	for _, want := range []string{`initAdmin`, `/api/role`, `role!=='owner'&&role!=='read-write'`} {
		if !strings.Contains(body, want) {
			t.Errorf("role-gate marker lost after the rail move: %q", want)
		}
	}
}
