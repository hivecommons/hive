package dashboard

import (
	"strings"
	"testing"
)

// TestPromptHistoryScrollContainment pins the scroll geometry of the Prior
// Prompts tab (#4801). An opened prompt body must be its own bounded scroller:
// its inline overflow-x:auto forces a computed overflow-y of auto, making it a
// scroll container, and the #2766 containment on `.config-overlay pre` makes
// that container a scroll-chain boundary. Without a max-height the body can
// never scroll, so wheel/touch over an opened prompt latched on it and went
// nowhere — or, on short viewports, leaked to the dashboard behind the modal
// instead of scrolling the prompt. Verified in headless Chromium: before the
// fix 40 wheel ticks over the opened prompt left every scroller at 0 (1400x900)
// or scrolled the page behind the modal (800x600); after, the prompt body
// scrolls and the page stays put.
func TestPromptHistoryScrollContainment(t *testing.T) {
	html := indexHTML(t)

	// The opened prompt body: bounded height, own vertical scroll, and
	// overscroll containment inline (so it holds even if the shared
	// `.config-overlay pre` rule is ever reorganized).
	preStyle := `class="prompt-history-body" data-idx="' + i + '" style="`
	idx := strings.Index(html, preStyle)
	if idx < 0 {
		t.Fatal("index.html no longer renders .prompt-history-body with an inline style — update this test alongside the markup")
	}
	styleEnd := strings.Index(html[idx+len(preStyle):], `"`)
	if styleEnd < 0 {
		t.Fatal(".prompt-history-body inline style is unterminated")
	}
	style := html[idx+len(preStyle) : idx+len(preStyle)+styleEnd]
	for _, want := range []string{
		"max-height:60vh",
		"overflow-y:auto",
		"overscroll-behavior:contain",
		"overscroll-behavior-y:contain",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("prompt-history-body inline style is missing %q — an opened prior prompt cannot scroll (or chains to the dashboard) again (#4801); style: %s", want, style)
		}
	}

	// The list around the entries scrolls too and must not chain past itself
	// when it reaches an edge.
	listTag := `id="prompt-history-list" class="config-pre-fill" style="`
	idx = strings.Index(html, listTag)
	if idx < 0 {
		t.Fatal("index.html no longer renders #prompt-history-list with an inline style — update this test alongside the markup")
	}
	styleEnd = strings.Index(html[idx+len(listTag):], `"`)
	if styleEnd < 0 {
		t.Fatal("#prompt-history-list inline style is unterminated")
	}
	style = html[idx+len(listTag) : idx+len(listTag)+styleEnd]
	for _, want := range []string{
		"overflow-y:auto",
		"overscroll-behavior:contain",
		"overscroll-behavior-y:contain",
	} {
		if !strings.Contains(style, want) {
			t.Errorf("#prompt-history-list inline style is missing %q — prompt-list scrolling chains to the dashboard behind the modal (#4801); style: %s", want, style)
		}
	}
}
