package dashboard

import (
	"strings"
	"testing"
)

// TestMobileIconButtonsHaveFixedTapTarget guards the mobile icon-button
// sizing (#11801): a touch-target *minimum* on a 15px round control inflated
// the circle to 44px while the glyph stayed at 9.6px. Icon-only controls must
// instead pin width and height to --tap-target and scale the glyph to
// --tap-glyph, and the topbar must size through the nav-chip tokens rather
// than a hard-coded 44px.
func TestMobileIconButtonsHaveFixedTapTarget(t *testing.T) {
	html := indexHTML(t)

	for _, snippet := range []string{
		"--tap-target: 40px;",
		"--tap-glyph: 16px;",
		"--tap-ring: 28px;",
		".config-info::before {\n        content: \"\";\n        position: absolute;\n        inset: calc((var(--tap-target) - var(--tap-ring)) / 2);",
		".dashboard-grip,\n      .dash-card-header .dashboard-grip,\n      .config-info,\n      .section-settings-gear {\n        width: var(--tap-target);\n        height: var(--tap-target);\n        min-width: var(--tap-target);\n        min-height: var(--tap-target);\n        flex: 0 0 auto;\n        font-size: var(--tap-glyph);\n      }",
		".section-settings-gear-icon {\n        width: var(--tap-glyph);\n        height: var(--tap-glyph);\n      }",
		".oc-topbar {\n        --nav-chip-h: var(--tap-target);\n        --nav-chip-icon: var(--tap-glyph);\n      }",
		".oc-topbar .nav-chip {\n        flex: 0 0 auto;\n        min-width: var(--tap-target);\n      }",
		".gov-pr-models-toggle button { min-height: 36px; padding: var(--sp-2) var(--sp-5); font-size: var(--fs-sm); }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — mobile icon buttons must be a fixed --tap-target square with a --tap-glyph glyph (#11801)", snippet)
		}
	}

	for _, stale := range []string{
		"min-width: 44px !important;",
		".config-info,\n      .section-settings-gear {\n        min-width: 44px;",
	} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html still contains %q — a bare 44px minimum re-inflates round icon buttons into oversized circles on mobile (#11801)", stale)
		}
	}
}
