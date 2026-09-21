package dashboard

import (
	"strings"
	"testing"
)

// Repositories cards can be dragged wider or narrower (hivecommons/hive#8000).
// The feature is several loosely coupled pieces — the handle markup renderRepos
// emits, the flex-wrap grid that lets a card carry its own width, the
// localStorage map the width is re-read from on every governor repaint, the
// clip-lifting for pill titles on a wide card, and the header-level "Reset
// layout" control the delegated click dispatcher resolves by name. Any one
// missing leaves a handle that looks live and does nothing, so each is pinned.
func TestRepoCardResizeWired(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// Grid must be flex-wrap: CSS grid auto-fit sizes tracks, not items.
		".repo-grid { --repo-card-basis: 240px; display: flex; flex-wrap: wrap;",
		"body.light-mode .repo-grid { --repo-card-basis: 200px; }",
		".repo-card.repo-card-sized { flex-grow: 0; flex-shrink: 0; }",
		".repo-resize-handle {",
		"cursor: col-resize;",
		// Handle is emitted per card by renderRepos, focusable, and names its repo.
		`<div class="repo-resize-handle" tabindex="0" role="separator" aria-orientation="vertical"`,
		`data-repo="${esc(r.full || r.name)}"`,
		"<div class=\"repo-card${cardW ? ' repo-card-sized' : ''}\" data-repo=\"${esc(r.full || r.name)}\"${cardW ? ` style=\"flex-basis:${cardW}px\"` : ''}>",
		// Width is re-read from the stored map on every repaint, not set once.
		"const REPO_CARD_WIDTHS_KEY = 'hive-repo-card-widths';",
		"const cardW = repoCardStoredWidth(r.full || r.name);",
		"const MAX_PILL_TITLE_LEN = cardW >= REPO_CARD_FULL_TITLE_W ? Infinity : REPO_CARD_DEFAULT_PILL_TITLE_LEN;",
		// A repaint mid-drag would replace the captured handle.
		"if (_repoCardDrag) return;",
		// Pointer drag, double-click reset, keyboard nudge.
		"document.addEventListener('pointerdown', (e) => {\n      const hit = repoCardHandleFor(e);",
		"document.addEventListener('dblclick', (e) => {\n      const hit = repoCardHandleFor(e);",
		"if (e.key === 'ArrowRight') next = cur + step;",
		"else if (e.key === 'ArrowLeft') next = cur - step;",
		// Header-level reset, resolved by the dispatcher's window[action] fallback.
		`id="repos-reset-layout-btn"`,
		`data-action="reposResetLayout"`,
		"function reposResetLayout()",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("repo card resize UI missing snippet %q", snippet)
		}
	}
	if strings.Contains(html, "grid-template-columns: repeat(auto-fit, minmax(240px, 1fr))") {
		t.Fatal(".repo-grid still uses grid auto-fit; per-card widths cannot take effect")
	}
}

// The reset button sits in the section-header <h2> whose own data-action
// collapses the section; without data-stop a click folds the cards it just
// reset (same reason Rescan and the gear carry it).
func TestRepoCardResetLayoutButtonStopsHeaderToggle(t *testing.T) {
	html := indexHTML(t)
	i := strings.Index(html, `id="repos-reset-layout-btn"`)
	if i < 0 {
		t.Fatal("reset layout button not found")
	}
	end := strings.Index(html[i:], ">")
	if end < 0 {
		t.Fatal("reset layout button tag never closes")
	}
	if !strings.Contains(html[i:i+end], `data-stop="1"`) {
		t.Fatalf("reset layout button lacks data-stop=\"1\": %s", html[i:i+end])
	}
}
