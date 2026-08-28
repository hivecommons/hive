package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Operations tab's two command-center panels (My work / Ready-work queue)
// were swapped vertically: My work now renders ABOVE the Ready-work queue (was
// below it). This is a pure DOM reorder — same region (.ops-main), same ids,
// same behavior, same travel/drag animations. These tests pin the new order and
// prove both panels still hydrate (ids intact) after the swap.

// TestOpsPanelOrderMyWorkAboveQueue pins the new vertical order: the "My work"
// card head must precede the "Ready-work queue" card head in the served markup,
// and both must still sit inside .ops-main (the main command-center area).
func TestOpsPanelOrderMyWorkAboveQueue(t *testing.T) {
	body := renderContributePage(t)

	mainStart := strings.Index(body, `class="ops-main"`)
	railStart := strings.Index(body, `class="ops-rail"`)
	myWork := strings.Index(body, `<h3>My work</h3>`)
	queue := strings.Index(body, `<h3>Ready-work queue</h3>`)

	if mainStart < 0 || railStart < 0 || myWork < 0 || queue < 0 {
		t.Fatalf("missing structure markers (main=%d rail=%d mywork=%d queue=%d)", mainStart, railStart, myWork, queue)
	}
	if mainStart >= myWork || myWork >= railStart {
		t.Errorf("My work heading is not inside .ops-main (main=%d mywork=%d rail=%d)", mainStart, myWork, railStart)
	}
	if mainStart >= queue || queue >= railStart {
		t.Errorf("Ready-work queue heading is not inside .ops-main (main=%d queue=%d rail=%d)", mainStart, queue, railStart)
	}
	if myWork >= queue {
		t.Errorf("expected My work panel to render ABOVE Ready-work queue (mywork=%d, queue=%d)", myWork, queue)
	}
}

// TestOpsPanelOrderHydrationIntact proves the reorder did not disturb the ids the
// render/hydration JS targets by getElementById, the filter buttons, the queue's
// live pill, or the gating/animation wiring.
func TestOpsPanelOrderHydrationIntact(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		// My work panel: id, count badge, filter tabs, list container.
		`id="work-count"`,
		`data-filter="all"`,
		`data-filter="active"`,
		`data-filter="review"`,
		`data-filter="done"`,
		`id="work-list"`,
		// Ready-work queue panel: id, live pill, queue container.
		`id="cc-live"`,
		`id="cc-live-label"`,
		`id="cc-queue"`,
		// Render/hydration entry points still wired.
		`function renderWork`,
		`function ccRenderQueue`,
		`function ccBindQueueDrag`,
		`function ccTravel`,
		// Drag-reorder + FLIP glide wiring (grip, drag model, FLIP helpers).
		`cc-q-grip`,
		`function ccFlipFirst`,
		`function ccFlipPlay`,
		`ccRenderQueue(true)`, // drop handler requests the FLIP glide, not a hard jump
	} {
		if !strings.Contains(body, want) {
			t.Errorf("panel-swap broke hydration wiring: missing %q", want)
		}
	}
}

// TestOpsPanelOrderFlipRespectsReducedMotion proves the FLIP glide is gated by
// prefers-reduced-motion, consistent with the rest of this page's motion (travel
// animation, achievement pops, dev-log lines all already respect it).
func TestOpsPanelOrderFlipRespectsReducedMotion(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, `function ccFlipPlay`) {
		t.Fatal("ccFlipPlay missing")
	}
	// ccFlipPlay must bail out under reduced motion before touching any transform.
	idx := strings.Index(body, `function ccFlipPlay`)
	end := strings.Index(body[idx:], "\n}\n")
	if end < 0 {
		t.Fatal("could not locate end of ccFlipPlay body")
	}
	fn := body[idx : idx+end]
	if !strings.Contains(fn, `prefers-reduced-motion:reduce`) {
		t.Error("ccFlipPlay does not check prefers-reduced-motion")
	}

	// The CSS reduced-motion media block must also list .cc-q-item.cc-q-flip among
	// the selectors neutralized with transition:none!important, so even a stray
	// inline style is inert.
	rmIdx := strings.Index(body, `@media(prefers-reduced-motion:reduce)`)
	if rmIdx < 0 {
		t.Fatal("reduced-motion media block missing")
	}
	rmEnd := strings.Index(body[rmIdx:], "</style>")
	rmBlock := body[rmIdx:]
	if rmEnd > 0 {
		rmBlock = body[rmIdx : rmIdx+rmEnd]
	}
	if !strings.Contains(rmBlock, `cc-q-item.cc-q-flip`) {
		t.Error("reduced-motion CSS block does not neutralize .cc-q-item.cc-q-flip")
	}
}

// TestOpsPanelOrderPageStillRenders is a smoke test that the full /contribute page
// still returns 200 with the swap in place (no accidental unclosed tag / structural
// break from the reorder).
func TestOpsPanelOrderPageStillRenders(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()
	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
