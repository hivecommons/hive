package dashboard

import (
	"strings"
	"testing"
)

// The Rescan control is three separate things that must all be present for
// the button to do anything: the markup in the expanded Projects toolbar, a global
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

// The button stays out of the collapsed header so repo pills get the width, but
// it still carries data-stop because the delegated action contract depends on it.
func TestReposRescanButtonLivesInExpandedToolbar(t *testing.T) {
	html := indexHTML(t)
	toolbar := strings.Index(html, `id="repos-toolbar"`)
	i := strings.Index(html, `id="repos-rescan-btn"`)
	if i < 0 {
		t.Fatal("rescan button not found")
	}
	if toolbar < 0 || toolbar > i {
		t.Fatalf("rescan button must live in the expanded Projects toolbar: toolbar=%d button=%d", toolbar, i)
	}
	headerStart := strings.Index(html, `<h2 class="section-header-toggle" data-action="toggleSection" data-arg0="repos-section"`)
	bodyRel := -1
	if headerStart >= 0 {
		bodyRel = strings.Index(html[headerStart:], `<div class="section-body">`)
	}
	if headerStart < 0 || bodyRel < 0 {
		t.Fatal("Projects header/body anchors not found")
	}
	bodyStart := headerStart + bodyRel
	for _, id := range []string{`repos-clear-pill-filter-btn`, `repos-rescan-btn`, `repos-reset-layout-btn`} {
		if strings.Contains(html[headerStart:bodyStart], `id="`+id+`"`) {
			t.Fatalf("%s must not render in the collapsed Projects header", id)
		}
	}
	// Bound the search to the button's own tag.
	end := strings.Index(html[i:], ">")
	if end < 0 {
		t.Fatal("rescan button tag never closes")
	}
	if !strings.Contains(html[i:i+end], `data-stop="1"`) {
		t.Fatalf("rescan button lacks data-stop=\"1\": %s", html[i:i+end])
	}
}
