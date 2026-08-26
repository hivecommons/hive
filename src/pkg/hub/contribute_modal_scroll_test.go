package hub

import (
	"os"
	"strings"
	"testing"
)

func TestContributeModalScrollContainment(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static/index.html: %v", err)
	}
	html := string(b)
	for _, snippet := range []string{
		"body.modal-open { overflow: hidden; }",
		"document.body.classList.add('modal-open');",
		"function closeContributeInfo() {",
		"document.body.classList.remove('modal-open');",
		`id="contrib-modal-panel"`,
		`onclick="closeContributeInfo()"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("static/index.html is missing %q — contribute modal scroll containment regressed", snippet)
		}
	}
	idx := strings.Index(html, "#contrib-modal-panel {")
	if idx < 0 {
		t.Fatal("static/index.html has no #contrib-modal-panel CSS rule")
	}
	end := strings.Index(html[idx:], "}")
	if end < 0 || !strings.Contains(html[idx:idx+end], "overscroll-behavior: contain;") {
		t.Error("#contrib-modal-panel rule is missing overscroll containment")
	}
}
