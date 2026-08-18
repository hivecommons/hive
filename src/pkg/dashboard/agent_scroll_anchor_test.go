package dashboard

import (
	"strings"
	"testing"
)

// These tests pin the STRUCTURE of the agent-click scroll anchoring in the
// embedded static/index.html. The scroll logic itself is pure browser JS
// (requestAnimationFrame, scrollTo, live DOM rects) and cannot be executed
// here — what these tests honestly guarantee is only that the settle helper,
// its user-input cancellation, and its call sites survive future edits to the
// file. Behavior (no jump-fighting, correct final anchor) is manual-verify.

// TestAgentScrollSettleHelperPresent asserts the settle helper and its named
// constants exist. Losing any of these regresses #45: clicks land the scroll
// on stale coordinates whenever content renders after the click.
func TestAgentScrollSettleHelperPresent(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function ocScrollToAgentDetail(behavior)",
		"const OC_SCROLL_SETTLE_MS = 700;",
		"const OC_SCROLL_TOLERANCE_PX = 4;",
		"const OC_SCROLL_CANCEL_EVENTS = ['wheel', 'touchstart', 'mousedown', 'keydown'];",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — agent-click scroll settle logic regressed", snippet)
		}
	}
}

// TestAgentScrollSettleCancelsOnUserInput asserts the correction loop is
// cancellable by user scroll input and that the cancel listeners are passive.
// Without this, the settle loop would fight a user who scrolls right after
// clicking an agent — the one failure mode the feature must never have.
func TestAgentScrollSettleCancelsOnUserInput(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"OC_SCROLL_CANCEL_EVENTS.forEach(t => window.addEventListener(t, cancel, { passive: true }));",
		"OC_SCROLL_CANCEL_EVENTS.forEach(t => window.removeEventListener(t, cancel));",
		"if (performance.now() > deadline) { cancel(); return; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — settle loop lost its user-input cancellation or its hard time cap", snippet)
		}
	}
}

// TestAgentScrollCallSitesUseHelper asserts both scroll-to-agent paths route
// through the settle helper instead of a raw one-shot scrollTo: the click path
// (smooth) and the hash-restore path on first load (instant).
func TestAgentScrollCallSitesUseHelper(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"ocScrollToAgentDetail('smooth');",
		"ocScrollToAgentDetail('instant');",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — a scroll-to-agent call site bypasses the settle helper", snippet)
		}
	}
	// The click path must not have kept a competing raw scroll to the panel.
	if strings.Count(html, "detail.getBoundingClientRect().top + window.scrollY - OC_TOPBAR_HEIGHT") != 1 {
		t.Error("expected exactly one raw detail-panel Y computation (inside ocScrollToAgentDetail); a call site regrew its own scrollTo")
	}
}
