package webstatic

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestStaticTrackerChevronGlyphsDoNotRotate: the owner (2026-10-28) — "i
// dont like the status symbols rotating — remove them from all chevrons".
// Every @keyframes in the page that rotates is collected, and no CSS rule
// addressing an Activity Monitor chevron, pill or glyph may animate with
// one of them. The classes stay (sibling cue code composes on them); only
// the rotation is gone (hivecommons/hive#15187).
func TestStaticTrackerChevronGlyphsDoNotRotate(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if strings.Contains(html, "@keyframes cue-spin") {
		t.Error("the cue-spin keyframe must be gone: nothing on a chevron rotates")
	}
	var css strings.Builder
	for _, m := range regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`).FindAllStringSubmatch(html, -1) {
		css.WriteString(m[1])
		css.WriteString("\n")
	}
	rotating := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?is)@keyframes\s+([-\w]+)\s*\{(.*?)\}\s*\}`).FindAllStringSubmatch(css.String(), -1) {
		if strings.Contains(strings.ToLower(m[2]), "rotate(") {
			rotating[m[1]] = true
		}
	}
	if len(rotating) == 0 {
		t.Fatal("expected at least one rotating keyframe elsewhere on the page (toast spinner); the scan found none — check the parser")
	}
	chevron := regexp.MustCompile(`\.(ham-seg|ham-ico|ham-active-ico|ham-gate-ico|cue-spin|cue-acting-spin|ham-seg-t|ham-detour)\b`)
	rule := regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	anim := regexp.MustCompile(`(?i)animation(?:-name)?\s*:\s*([^;}]+)`)
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(css.String(), "")
	for _, m := range rule.FindAllStringSubmatch(stripped, -1) {
		sel, decl := strings.TrimSpace(m[1]), m[2]
		if !chevron.MatchString(sel) {
			continue
		}
		for _, a := range anim.FindAllStringSubmatch(decl, -1) {
			for _, tok := range strings.Fields(strings.ReplaceAll(a[1], ",", " ")) {
				if rotating[tok] {
					t.Errorf("chevron/glyph rule %q animates with rotating keyframe %q: %s", sel, tok, strings.TrimSpace(decl))
				}
			}
		}
	}
}
