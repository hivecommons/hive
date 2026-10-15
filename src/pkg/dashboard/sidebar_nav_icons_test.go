package dashboard

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// sidebarNavEmojis returns every statically rendered sidebar entry's icon keyed
// by its label, in document order. Agent tree rows are rendered by JS and are
// intentionally not part of the static set.
func sidebarNavEmojis(html string) (labels []string, icons map[string]string) {
	re := regexp.MustCompile(`<a class="oc-nav-item[^>]*>\s*<span class="oc-nav-emoji">([^<]+)</span><span class="oc-nav-text">([^<]+)</span>`)
	icons = map[string]string{}
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		label := strings.TrimSpace(m[2])
		labels = append(labels, label)
		icons[label] = strings.TrimSpace(m[1])
	}
	return labels, icons
}

// TestSidebarNavIconsAreUnique guards against two sidebar sections sharing an
// icon (Governor/Throughput both 📊, Advisory/Audit Log both 📋 ...), which made
// the collapsed rail and the section card headers ambiguous.
func TestSidebarNavIconsAreUnique(t *testing.T) {
	html := indexHTML(t)
	labels, icons := sidebarNavEmojis(html)
	if len(labels) < 15 {
		t.Fatalf("expected the static sidebar to expose at least 15 nav entries, parsed %d", len(labels))
	}
	seen := map[string]string{}
	for _, label := range labels {
		icon := icons[label]
		if icon == "" {
			t.Fatalf("sidebar entry %q has an empty icon", label)
		}
		if prev, dup := seen[icon]; dup {
			t.Fatalf("sidebar icon %q is used by both %q and %q; pick a section-specific icon", icon, prev, label)
		}
		seen[icon] = label
	}
}

// TestSidebarNavIconsAreSingleCodepoint keeps the rail aligned: icons that need
// a variation selector or a ZWJ sequence render at different widths on macOS
// and Linux Chrome, so every sidebar icon must be a single emoji codepoint.
func TestSidebarNavIconsAreSingleCodepoint(t *testing.T) {
	html := indexHTML(t)
	labels, icons := sidebarNavEmojis(html)
	for _, label := range labels {
		icon := icons[label]
		if n := utf8.RuneCountInString(icon); n != 1 {
			t.Fatalf("sidebar entry %q icon %q is %d codepoints; use a single-codepoint emoji", label, icon, n)
		}
	}
}

// TestSidebarNavIconsMatchSectionHeaders asserts the in-page <h2> section
// headers carry the same icon as the sidebar entry that navigates to them, so a
// sidebar icon change cannot silently leave the old glyph on the section card.
func TestSidebarNavIconsMatchSectionHeaders(t *testing.T) {
	html := indexHTML(t)
	headerRe := regexp.MustCompile(`data-action="toggleSection" data-arg0="([^"]+)"[^\n]*?<span class="section-chevron">▼</span>\s*(\S+)\s+([^<]+)</h2>`)
	headers := map[string]string{}
	for _, m := range headerRe.FindAllStringSubmatch(html, -1) {
		headers[m[1]] = m[2]
	}
	for sectionID, icon := range sidebarSectionEmojis(html) {
		if got, ok := headers[sectionID]; ok && got != icon {
			t.Fatalf("section %q header icon %q does not match sidebar icon %q", sectionID, got, icon)
		}
	}
}
