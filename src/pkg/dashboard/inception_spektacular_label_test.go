package dashboard

import (
	"strings"
	"testing"
)

func TestInceptionSidebarLabelsSpektacular(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		// The old Inception wizard section is gone (hivecommons/hive#18268);
		// its sidebar entry is now the Inception (spek) sessions section.
		`data-section="sessions-section" data-action="ocNavigate" data-arg0="sessions-section" hidden><span class="oc-nav-emoji">💡</span><span class="oc-nav-text">Inception (spek)</span>`,
		`const SIDEBAR_MIN_W = 280;`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}
