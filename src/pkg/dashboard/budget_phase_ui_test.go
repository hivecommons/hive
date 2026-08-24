package dashboard

import (
	"strings"
	"testing"
)

// #4686: an operator who configured "1B tokens every 2 days" asked two things
// the dashboard could not answer — WHEN does the window reset (the phase, not a
// countdown), and how much had been spent at each past reset — and asked them
// from the Cost panel, where the answer did not exist at all.
//
// The data and the per-window chart already existed (#4298/#4320) but lived
// only under Governor, and the reset was rendered solely as "37h until reset",
// which cannot be lined up against a calendar. These tests pin both halves of
// the fix: the phase is stated absolutely, and the Cost panel answers the
// token-budget question in tokens.

// TestBudgetWindowPhaseIsStatedAbsolutely pins the phase fix. A countdown alone
// is what #4686 reported as unanswerable.
func TestBudgetWindowPhaseIsStatedAbsolutely(t *testing.T) {
	html := indexHTML(t)

	for _, def := range []string{
		"function budgetWindowBounds(",
		"function budgetWindowPhaseText(",
		"function budgetWindowPhaseTitle(",
		"function fmtWindowStamp(",
	} {
		if !strings.Contains(html, def) {
			t.Errorf("index.html does not define %q — a referenced-but-undefined name throws at render time", def)
		}
	}

	// The absolute bounds come from the live status fields, which are the only
	// source that knows when THIS window opened.
	for _, field := range []string{"WINDOW_STARTS_AT", "WINDOW_ENDS_AT"} {
		if !strings.Contains(html, field) {
			t.Errorf("index.html never reads %s — the window phase cannot be derived without it", field)
		}
	}

	// The budget bar must render the phase helper rather than a bare countdown.
	if !strings.Contains(html, "budgetWindowPhaseText(budget, hoursLeft)") {
		t.Error("the budget bar does not render the phase — #4686's 'I want to know the phase' is unanswered")
	}
	if strings.Contains(html, "<span>${hoursLeft}h until reset</span>") {
		t.Error("the budget bar still shows only the bare countdown — a countdown cannot be compared against a calendar")
	}
}

// TestBudgetPhaseToleratesMissingBounds: WINDOW_STARTS_AT/ENDS_AT are optional
// (empty until a limit is set and a window has opened). Rendering "Invalid Date"
// at an operator would be worse than the countdown this replaced, so the
// fallback must be present.
func TestBudgetPhaseToleratesMissingBounds(t *testing.T) {
	html := indexHTML(t)

	if !strings.Contains(html, "if (!start && !end) return `${hoursLeft}h until reset`;") {
		t.Error("budgetWindowPhaseText has no no-bounds fallback — an unopened window would render Invalid Date")
	}
	if !strings.Contains(html, "isNaN(d.getTime()) ? null : d") {
		t.Error("budgetWindowBounds does not reject unparseable timestamps")
	}
}

// TestCostPanelAnswersTokenBudget is the discoverability half. The operator went
// to the Cost panel; the token budget must be answerable there.
func TestCostPanelAnswersTokenBudget(t *testing.T) {
	html := indexHTML(t)

	if !strings.Contains(html, "function costTokenBudgetHtml(") {
		t.Fatal("index.html does not define costTokenBudgetHtml")
	}
	if !strings.Contains(html, "${costTokenBudgetHtml()}") {
		t.Error("the Cost panel never renders the token budget — #4686 was filed from that panel")
	}

	// It must reuse the SAME window history the Governor chart draws. A second
	// source of truth here would let the two panels disagree about the same
	// budget, which is the class of bug #4699 was.
	if !strings.Contains(html, "${budgetHistoryChart()}\n        </div>\n      </div>`;") &&
		!strings.Contains(html, "budgetHistoryChart()") {
		t.Error("the Cost panel does not reuse budgetHistoryChart — per-reset spend would be missing or duplicated")
	}
	if !strings.Contains(html, "window._lastBudget") {
		t.Error("the Cost panel does not read _lastBudget — it could disagree with the Governor bar")
	}
}

// TestCostPanelSaysDollarsAreNotTheLimit pins the unit correction. The reporter
// discarded every dollar figure because the hive does not know the true price
// and does not regulate on dollars — so a Cost panel that shows only dollars
// must say what the limit IS denominated in.
func TestCostPanelSaysDollarsAreNotTheLimit(t *testing.T) {
	html := indexHTML(t)

	if !strings.Contains(html, "tokens, not dollars") {
		t.Error("the Cost panel does not distinguish the enforced token limit from the dollar estimates")
	}
	// With no budget configured the panel must say so and point at the setting,
	// rather than silently showing nothing — "I did not find answers" was the
	// report, and an empty panel reproduces it.
	if !strings.Contains(html, "No token budget configured") {
		t.Error("the Cost panel is silent when no token budget is set — the operator cannot tell configured-and-zero from unconfigured")
	}
}

// TestBudgetPhaseAddsNoInlineHandlers guards the CSP contract (#3848/ADR-0016)
// for the markup added here: script-src-attr is 'none', so an inline on*=
// attribute is dead on arrival and its control silently does nothing.
func TestBudgetPhaseAddsNoInlineHandlers(t *testing.T) {
	html := indexHTML(t)

	start := strings.Index(html, "function costTokenBudgetHtml(")
	if start < 0 {
		t.Fatal("costTokenBudgetHtml not found")
	}
	end := strings.Index(html[start:], "function renderCost(")
	if end < 0 {
		t.Fatal("could not bound costTokenBudgetHtml")
	}
	block := html[start : start+end]

	for _, attr := range []string{" onclick=", " onchange=", " oninput=", " onkeyup=", " onmouseover="} {
		if strings.Contains(block, attr) {
			t.Errorf("costTokenBudgetHtml emits an inline %q handler, which CSP blocks — wire it through data-action dispatch", strings.TrimSpace(attr))
		}
	}
}
