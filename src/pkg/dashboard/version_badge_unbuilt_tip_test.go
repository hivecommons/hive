package dashboard

import (
	"strings"
	"testing"
)

// TestVersionBadgeUnbuiltTipWording pins the #4804 frontend contract: when
// commitsBehind is null because the stable v4 tip has no published image
// (stableV4ImageReady === false), the badge must render a muted "tip not built
// yet" note — never the yellow "? behind" that contradicts the green ✓. The
// "?" wording is reserved for a genuine compare failure (image exists but
// commitsBehind is still null), and older servers that do not send the field
// keep the "?" via the strict === false comparison.
func TestVersionBadgeUnbuiltTipWording(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// The state discriminator, strict so an absent field keeps "?".
		"v.stableV4ImageReady === false",
		// The known-state arm: muted, no question mark, says why.
		`has no published image yet — nothing to upgrade to">tip not built yet</span>`,
		// The genuine-unknown arm survives for real compare failures.
		`Could not compare this commit with stable v4 tip ${escapeHtml(v.stableV4Short || '?')}">? behind</span>`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the badge again renders contradictory ✓ + ? behind for an unbuilt tip (#4804)", snippet)
		}
	}
	if strings.Contains(html, `class="git-behind"`) && strings.Contains(html, `tip not built yet</span>`) {
		// The muted arm must not reuse the yellow .git-behind styling.
		idx := strings.Index(html, "tip not built yet</span>")
		start := strings.LastIndex(html[:idx], "<span")
		if start >= 0 && strings.Contains(html[start:idx], "git-behind") {
			t.Error(`the "tip not built yet" note uses class="git-behind" — it must be muted, not warning-yellow (#4804)`)
		}
	}
}
