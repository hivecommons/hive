package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contributePageBody renders the /contribute page and returns its HTML body.
func contributePageBody(t *testing.T) string {
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

// TestQueueHeaderRendersCooldownCounts pins the queue-header count wiring: the JS
// consumes the fleet payload's cooldown/in-flight counts and renders the compact
// "N ready · M in cooldown · K in flight" segments (each omitted when 0).
func TestQueueHeaderRendersCooldownCounts(t *testing.T) {
	body := contributePageBody(t)
	for _, want := range []string{
		// State stashed from the fleet payload's new fields.
		`data.cooldown_count`,
		`data.in_flight_count`,
		`var ccCooldownCount`,
		`var ccInFlightCount`,
		// The count string segments built in ccRenderQueue.
		`ready`,
		`in cooldown`,
		`in flight`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("queue header count wiring missing %q", want)
		}
	}
}

// TestCooldownInfoButtonAndExplainer pins the ⓘ info affordance next to the
// queue header and the explainer copy. The copy must state how an issue enters
// cooldown using the REAL server constants (168h with-PR, ~4h no-PR, ~6h
// quarantine after 3 consecutive failures) — no invented numbers.
func TestCooldownInfoButtonAndExplainer(t *testing.T) {
	body := contributePageBody(t)
	for _, want := range []string{
		// The button + popover markup (mirrors title=/aria tooltip patterns).
		`id="cooldown-info-btn"`,
		`id="cooldown-info-pop"`,
		`class="info-btn"`,
		`What is cooldown?`,
		`aria-controls="cooldown-info-pop"`,
		// The wiring that toggles the popover.
		`function _wireCooldownInfo(`,
		// Explainer substance: what cooldown is + how an issue enters it.
		`out of the ready queue`,
		`verified PR`,
		`no PR`,
		`quarantined`,
		`clears automatically`,
		// The REAL constants, verbatim.
		`168h`,
		`4h`,
		`6h`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cooldown info button / explainer missing %q", want)
		}
	}
	// The "3 consecutive failures" threshold must appear in the failure line. The
	// glyph is rendered as an HTML entity, so pin the code-styled constant instead.
	if !strings.Contains(body, `<code>3</code>`) {
		t.Errorf("explainer must reference the 3-consecutive-failures quarantine threshold")
	}
}

// TestManagementTabShowsCooldownCount pins the Management-tab tally element and
// its renderer (#2649 companion): "M issues currently cooling down".
func TestManagementTabShowsCooldownCount(t *testing.T) {
	body := contributePageBody(t)
	for _, want := range []string{
		`id="admin-cooldown-count"`,
		`function ccRenderCooldownCount(`,
		`currently cooling down`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Management-tab cooldown count missing %q", want)
		}
	}
}
