package dashboard

import (
	"strings"
	"testing"
)

// The Rescan control is three separate things that must all be present for
// the button to do anything: the markup in the REPOSITORIES header, a global
// handler the delegated click dispatcher can resolve by name (there is no
// registry — hiveDispatchAction falls back to window[action]), and the POST
// to the endpoint. Any one of them going missing leaves a button that looks
// live and does nothing, which is exactly the failure csp_inline_handlers's
// doc comment describes for the four controls that broke that way before.
func TestReposRescanButtonWired(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`id="repos-rescan-btn"`,
		`data-action="reposForceRescan"`,
		"async function reposForceRescan()",
		"'/api/repos/rescan'",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("REPOSITORIES rescan UI missing snippet %q", snippet)
		}
	}
}

// The button sits inside the section-header <h2>, whose own data-action
// collapses the section. Without data-stop the click bubbles and pressing
// Rescan folds the very cards it just refreshed — the same reason the gear
// beside it carries data-stop.
func TestReposRescanButtonStopsHeaderToggle(t *testing.T) {
	html := indexHTML(t)
	i := strings.Index(html, `id="repos-rescan-btn"`)
	if i < 0 {
		t.Fatal("rescan button not found")
	}
	// Bound the search to the button's own tag.
	end := strings.Index(html[i:], ">")
	if end < 0 {
		t.Fatal("rescan button tag never closes")
	}
	if !strings.Contains(html[i:i+end], `data-stop="1"`) {
		t.Fatalf("rescan button lacks data-stop=\"1\"; clicking it would also toggle the Repositories section: %s", html[i:i+end])
	}
}
