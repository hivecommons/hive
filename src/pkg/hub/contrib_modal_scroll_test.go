package hub

import (
	"os"
	"strings"
	"testing"
)

// TestContribModalScrollLock pins the #4805 fix on the hub landing page's
// contribute modal: the page behind the overlay must be scroll-locked while
// the modal is open (and restored on every close path — button, backdrop,
// Escape), and both the backdrop and the 80vh panel must contain scroll
// chaining so a wheel at the panel's boundary cannot scroll the hub page.
func TestContribModalScrollLock(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read hub static html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		// One close function shared by button, backdrop, and Escape — it is
		// what restores the body scroll.
		"function closeContribModal() {",
		"document.body.style.overflow = '';",
		// Lock on open.
		"document.body.style.overflow = 'hidden';",
		// Backdrop click and Escape close paths.
		"if(event.target===this)closeContribModal()",
		"if (e.key === 'Escape' && document.getElementById('contrib-modal').style.display === 'flex') closeContribModal();",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("hub static/index.html is missing %q — contribute-modal scroll lock regressed (#4805)", snippet)
		}
	}

	// Containment on the backdrop and the scrollable panel.
	idx := strings.Index(html, `id="contrib-modal"`)
	if idx < 0 {
		t.Fatal("hub static/index.html has no contrib-modal")
	}
	region := html[idx:]
	if cut := strings.Index(region, "</h2>"); cut > 0 {
		region = region[:cut]
	}
	if strings.Count(region, "overscroll-behavior:contain") < 2 {
		t.Error("contrib-modal backdrop/panel are missing overscroll-behavior:contain — modal scroll chains to the hub page (#4805)")
	}
}
