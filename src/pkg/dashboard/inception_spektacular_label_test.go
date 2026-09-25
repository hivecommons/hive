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
		`data-section="inception-section" data-action="ocNavigate" data-arg0="inception-section"><span class="oc-nav-emoji">💡</span><span class="oc-nav-text">Inception (spektacular)</span>`,
		`Project Inception · powered by Spektacular`,
		`Settings → Extensions → Spektacular`,
		`const SIDEBAR_MIN_W = 280;`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
}
