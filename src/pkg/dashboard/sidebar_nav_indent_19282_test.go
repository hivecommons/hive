package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// cssRuleBody returns the declaration block of the first rule whose selector
// list is exactly selector, or "" when there is none.
func cssRuleBody(css, selector string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestSidebarNavItemsShareOneIndent guards #19282: only Dashboard entries got
// a reorder grip, and that hidden grip was the only thing indenting them, so
// Collaborate, Resources, Intelligence, Admin, Help and Filters entries sat
// flush left. Every entry now reserves the same grip-wide gutter through one
// .oc-nav-item rule, and a grip draws inside that gutter instead of adding to it.
func TestSidebarNavItemsShareOneIndent(t *testing.T) {
	html := indexHTML(t)

	for _, want := range []string{
		"--oc-nav-grip-w:",
		"--oc-nav-child-inset: calc(var(--oc-nav-grip-w) + var(--oc-nav-item-gap))",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("sidebar tokens missing %q", want)
		}
	}

	item := cssRuleBody(html, ".oc-nav-item")
	if item == "" {
		t.Fatal("no .oc-nav-item rule found")
	}
	if !strings.Contains(item, "calc(var(--oc-nav-item-pad-x) + var(--oc-nav-child-inset))") {
		t.Errorf(".oc-nav-item must reserve the shared grip gutter in its left padding; got %q", item)
	}
	if !strings.Contains(item, "gap: var(--oc-nav-item-gap)") {
		t.Errorf(".oc-nav-item gap must use --oc-nav-item-gap so the gutter matches the grip spacing; got %q", item)
	}

	grip := cssRuleBody(html, ".oc-nav-item > .oc-nav-drag-handle:first-child")
	if !strings.Contains(grip, "margin-left: calc(-1 * var(--oc-nav-child-inset))") {
		t.Errorf("a leading grip must sit inside the shared gutter (negative inset margin); got %q", grip)
	}

	handle := cssRuleBody(html, ".oc-nav-drag-handle, .oc-nav-hide-toggle")
	if !strings.Contains(handle, "width: var(--oc-nav-grip-w)") {
		t.Errorf("grip width must come from --oc-nav-grip-w so the gutter tracks it; got %q", handle)
	}

	// No per-group indentation: the fix is one shared rule, not a pixel
	// override per sidebar group.
	perGroup := regexp.MustCompile(`#(dashboard-section|collaborate|hf)-nav-group[^{]*\.oc-nav-item[^{]*\{[^}]*padding`)
	if perGroup.MatchString(html) {
		t.Error("sidebar groups must not carry their own .oc-nav-item padding; use the shared gutter")
	}
}
