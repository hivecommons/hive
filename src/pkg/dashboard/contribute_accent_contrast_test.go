package dashboard

import (
	"strings"
	"testing"
)

// #4560: the dark-tuned accents (#58a6ff, #d29922, #3fb950, …) sat below
// 3.0:1 on light surfaces once #4554 made light user-reachable. A9 removes the
// private neutral/status ramp; only the intentional GitHub-blue action accent
// and merger pink remain portal-local, with explicit light values.
var contributorAccentAliases = map[string]struct{ dark, light string }{
	"--cc-accent":    {"#58a6ff", "#0969da"},
	"--cc-green":     {"#3fb950", "#1a7f37"},
	"--cc-amber":     {"#d29922", "#9a6700"},
	"--cc-red":       {"#f85149", "#cf222e"},
	"--cc-accent-fg": {"#1f6feb", "#0969da"},
}

// paletteBlock extracts the CSS rule body that starts at the given selector
// prefix, so each theme's token values are asserted in THEIR block rather than
// anywhere in the document.
func paletteBlock(t *testing.T, body, sel string) string {
	t.Helper()
	i := strings.Index(body, sel)
	if i < 0 {
		t.Fatalf("palette block %q not found", sel)
	}
	rest := body[i:]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("palette block %q not terminated", sel)
	}
	return rest[:end]
}

func TestContributeAccentTokensUseSharedTheme(t *testing.T) {
	body := contributeBody(t)

	dark := paletteBlock(t, body, ":root{")
	lightAuto := paletteBlock(t, body, "@media(prefers-color-scheme:light){:root:not([data-theme=\"dark\"]){")
	lightPinned := paletteBlock(t, body, ":root[data-theme=\"light\"]{")
	for token, value := range contributorAccentAliases {
		if want := token + ":" + value.dark + ";"; !strings.Contains(dark, want) {
			t.Errorf("contributor alias missing %q", want)
		}
		if want := token + ":" + value.light + ";"; !strings.Contains(lightAuto, want) {
			t.Errorf("OS-light contributor alias missing %q", want)
		}
		if want := token + ":" + value.light + ";"; !strings.Contains(lightPinned, want) {
			t.Errorf("pinned-light contributor alias missing %q", want)
		}
	}
	for _, old := range []string{
		"--cc-accent-2:", "--cc-pink:", "--cc-purple:",
	} {
		if strings.Contains(body, old) {
			t.Errorf("contributor page still carries private accent/light ramp fragment %q", old)
		}
	}
}

// Regression guard: a tokenized accent hex must never reappear as a TEXT color
// (color:) or status-indicator fill (background: on dots) — an accent literal
// there is unreachable by the light ramp and reintroduces the sub-3.0:1 pairs.
// Deliberate literals elsewhere (button fills with white text, SVG icon
// strokes, the me-card metal system, rgba washes) are out of scope.
func TestContributeNoAccentLiteralTextColors(t *testing.T) {
	body := contributeBody(t)

	for _, hex := range []string{"#79c0ff", "#bc8cff"} {
		for _, prop := range []string{"color:", "background:", "outline:2px solid "} {
			if bad := prop + hex; strings.Contains(body, bad) {
				t.Errorf("accent literal %q found; use shared tokens so the light ramp reaches it", bad)
			}
		}
	}
}

// The tier stat numbers are emitted from Go with inline styles; they were the
// worst offenders (2.3:1 on the light surface) and must route through tokens.
func TestContributeTierStatsUseAccentTokens(t *testing.T) {
	body := contributeBody(t)

	for _, want := range []string{
		`style="color:var(--cc-green)"`,
		`style="color:var(--cc-amber)"`,
		`style="color:var(--cc-accent)"`,
		`style="color:var(--acmm-level-5)"`,
		`style="color:var(--cc-red)"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tier stat missing tokenized inline color %q", want)
		}
	}
}

// The live-status pill's dark-tuned rgba washes go faint on white, and amber
// text at pill size needs the darker light step to clear 4.5:1. Both light
// hooks must carry the retuned washes.
func TestContributeLivePillLightRetune(t *testing.T) {
	body := contributeBody(t)

	for _, want := range []string{
		`:root:not([data-theme="dark"]) .cc-live.stale{background:rgba(154,103,0,.08);border-color:rgba(154,103,0,.30);color:#7d4e00}`,
		`:root[data-theme="light"] .cc-live.stale{background:rgba(154,103,0,.08);border-color:rgba(154,103,0,.30);color:#7d4e00}`,
		`:root:not([data-theme="dark"]) .cc-live{background:rgba(26,127,55,.08);border-color:rgba(26,127,55,.30);color:#116329}`,
		`:root[data-theme="light"] .cc-live{background:rgba(26,127,55,.08);border-color:rgba(26,127,55,.30);color:#116329}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("live pill light retune missing %q", want)
		}
	}
}
