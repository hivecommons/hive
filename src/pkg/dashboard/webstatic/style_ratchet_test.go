package webstatic

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type styleRatchetCounts struct {
	inlineStyles   int
	rawColors      int
	rawFontSizes   int
	rawPadding     int
	rawBorderRadii int
}

// These baselines are the current dashboard styling-debt counts measured on
// 2026-09-23 in PR #8584 for #8536, immediately after the
// shared tokens.css layer landed in #8579. They are ratchets, not targets:
// counts may go DOWN freely — lower the matching constant in the same PR that
// removes raw styling — but they must not go UP.
var styleRatchetBaselines = map[string]styleRatchetCounts{
	"operator static/index.html": {
		inlineStyles:   2101,
		rawColors:      195,
		rawFontSizes:   1088,
		rawPadding:     710,
		rawBorderRadii: 473,
	},
	"contributor landing": {
		inlineStyles:   111,
		rawColors:      202,
		rawFontSizes:   279,
		rawPadding:     206,
		rawBorderRadii: 143,
	},
	"hub static pages": {
		inlineStyles:   1189,
		rawColors:      584,
		rawFontSizes:   652,
		rawPadding:     391,
		rawBorderRadii: 309,
	},
	"design system preview": {},
}

var (
	styleAttributeStartRE  = regexp.MustCompile(`(?i)\bstyle\s*=\s*["']`)
	doubleStyleAttributeRE = regexp.MustCompile(`(?is)\bstyle\s*=\s*"([^"]*)"`)
	singleStyleAttributeRE = regexp.MustCompile(`(?is)\bstyle\s*=\s*'([^']*)'`)
	styleBlockRE           = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
	cssCommentRE           = regexp.MustCompile(`(?s)/\*.*?\*/`)
	hexColorRE             = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	colorFunctionRE        = regexp.MustCompile(`(?i)\b(?:rgb|rgba|hsl|hsla)\s*\(`)
	urlFragmentRE          = regexp.MustCompile(`(?i)url\(\s*["']?#[^)]+\)`)
	hrefFragmentRE         = regexp.MustCompile(`(?i)href\s*=\s*["']#[^"']*["']`)
	customPropertyLineRE   = regexp.MustCompile(`^\s*--[-\w]+\s*:`)
	fontSizeRE             = regexp.MustCompile(`(?ims)(^|[;{])\s*font-size\s*:\s*([^;{}]+)`)
	paddingRE              = regexp.MustCompile(`(?ims)(^|[;{])\s*padding(?:-(?:top|right|bottom|left|block|block-start|block-end|inline|inline-start|inline-end))?\s*:\s*([^;{}]+)`)
	borderRadiusRE         = regexp.MustCompile(`(?ims)(^|[;{])\s*border-radius\s*:\s*([^;{}]+)`)
	pxOrRemRE              = regexp.MustCompile(`(?i)[-+]?\d*\.?\d+(?:px|rem)\b`)
	tokenDefinitionRE      = regexp.MustCompile(`(?m)^\s*(--[-_a-zA-Z0-9]+)\s*:`)
	tokenReferenceRE       = regexp.MustCompile(`var\(\s*(--[-_a-zA-Z0-9]+)`)
)

// TestStyleRatchet fails when dashboard raw styling grows on any surface. The
// counters are deliberately simple regexes so the test stays deterministic and
// fast; they count style= attributes even inside scripts/templates because
// those snippets end up in the DOM. Raw value counters inspect inline <style>
// blocks and style attributes; custom-property declaration lines are excluded
// so the legacy token aliases can keep their current literals until migrated.
func TestStyleRatchet(t *testing.T) {
	surfaces := loadStyleRatchetSurfaces(t)
	for _, surface := range surfaces {
		counts := countStyleRatchet(surface.contents)
		t.Logf("%s: style=%d raw-colors=%d raw-font-size=%d raw-padding=%d raw-border-radius=%d",
			surface.name, counts.inlineStyles, counts.rawColors, counts.rawFontSizes, counts.rawPadding, counts.rawBorderRadii)
		checkStyleRatchet(t, surface.name, counts, styleRatchetBaselines[surface.name])
	}
}

func TestStyleRatchetTokensCSS(t *testing.T) {
	data, err := os.ReadFile("../static/tokens.css")
	if err != nil {
		t.Fatalf("reading tokens.css: %v", err)
	}
	css := string(data)
	if n := len(styleAttributeStartRE.FindAllStringIndex(css, -1)); n != 0 {
		t.Fatalf("tokens.css contains %d style= attributes; token stylesheets must not emit inline style attributes", n)
	}

	defs := map[string]bool{}
	for _, match := range tokenDefinitionRE.FindAllStringSubmatch(css, -1) {
		defs[match[1]] = true
	}
	refs := map[string]bool{}
	for _, match := range tokenReferenceRE.FindAllStringSubmatch(css, -1) {
		refs[match[1]] = true
	}
	var missing []string
	for ref := range refs {
		if !defs[ref] {
			missing = append(missing, ref)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("tokens.css references undefined custom properties via var(): %s", strings.Join(missing, ", "))
	}
	t.Logf("tokens.css: %d custom properties defined, %d var() references checked", len(defs), len(refs))
}

func TestStyleRatchetComponentsCSS(t *testing.T) {
	tokenData, err := os.ReadFile("../static/tokens.css")
	if err != nil {
		t.Fatalf("reading tokens.css: %v", err)
	}
	componentData, err := os.ReadFile("../static/components.css")
	if err != nil {
		t.Fatalf("reading components.css: %v", err)
	}
	css := string(componentData)
	if n := len(styleAttributeStartRE.FindAllStringIndex(css, -1)); n != 0 {
		t.Fatalf("components.css contains %d style= attributes; component stylesheets must not emit inline style attributes", n)
	}
	if n := countRawColors(css); n != 0 {
		t.Fatalf("components.css contains %d raw color literals/functions; component recipes must use tokens", n)
	}
	if n := countRawDeclarations(css, fontSizeRE, false); n != 0 {
		t.Fatalf("components.css contains %d raw font-size declarations; component recipes must use tokens", n)
	}
	if n := countRawDeclarations(css, paddingRE, true); n != 0 {
		t.Fatalf("components.css contains %d raw padding declarations; component recipes must use tokens", n)
	}
	if n := countRawDeclarations(css, borderRadiusRE, false); n != 0 {
		t.Fatalf("components.css contains %d raw border-radius declarations; component recipes must use tokens", n)
	}
	if matches := pxOrRemRE.FindAllString(cssCommentRE.ReplaceAllString(css, ""), -1); len(matches) != 0 {
		t.Fatalf("components.css contains raw px/rem values outside tokens: %s", strings.Join(matches, ", "))
	}

	defs := map[string]bool{}
	for _, match := range tokenDefinitionRE.FindAllStringSubmatch(string(tokenData)+"\n"+css, -1) {
		defs[match[1]] = true
	}
	refs := map[string]bool{}
	for _, match := range tokenReferenceRE.FindAllStringSubmatch(css, -1) {
		refs[match[1]] = true
	}
	var missing []string
	for ref := range refs {
		if !defs[ref] {
			missing = append(missing, ref)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("components.css references undefined custom properties via var(): %s", strings.Join(missing, ", "))
	}
	t.Logf("components.css: %d var() references checked", len(refs))
}

type styleRatchetSurface struct {
	name     string
	contents string
}

func loadStyleRatchetSurfaces(t *testing.T) []styleRatchetSurface {
	t.Helper()
	return []styleRatchetSurface{
		{
			name:     "operator static/index.html",
			contents: readStyleRatchetFile(t, "../static/index.html"),
		},
		{
			name:     "contributor landing",
			contents: readStyleRatchetFile(t, "../contribute_landing.go"),
		},
		{
			name:     "hub static pages",
			contents: readStyleRatchetGlob(t, "../../hub/static/*.html", "../../hub/assets/*.html"),
		},
		{
			name:     "design system preview",
			contents: readStyleRatchetFile(t, "../static/design-system.html"),
		},
	}
}

func readStyleRatchetFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func readStyleRatchetGlob(t *testing.T, patterns ...string) string {
	t.Helper()
	var paths []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("globbing %s: %v", pattern, err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatalf("no hub HTML files matched %v", patterns)
	}
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(readStyleRatchetFile(t, path))
		b.WriteByte('\n')
	}
	return b.String()
}

func countStyleRatchet(contents string) styleRatchetCounts {
	css := styleContexts(contents)
	return styleRatchetCounts{
		inlineStyles:   len(styleAttributeStartRE.FindAllStringIndex(contents, -1)),
		rawColors:      countRawColors(css),
		rawFontSizes:   countRawDeclarations(css, fontSizeRE, false),
		rawPadding:     countRawDeclarations(css, paddingRE, true),
		rawBorderRadii: countRawDeclarations(css, borderRadiusRE, false),
	}
}

func styleContexts(contents string) string {
	var contexts []string
	for _, match := range styleBlockRE.FindAllStringSubmatch(contents, -1) {
		contexts = append(contexts, match[1])
	}
	for _, re := range []*regexp.Regexp{doubleStyleAttributeRE, singleStyleAttributeRE} {
		for _, match := range re.FindAllStringSubmatch(contents, -1) {
			contexts = append(contexts, match[1])
		}
	}
	return strings.Join(contexts, "\n")
}

func countRawColors(css string) int {
	css = cssCommentRE.ReplaceAllString(css, "")
	total := 0
	for _, line := range strings.Split(css, "\n") {
		if customPropertyLineRE.MatchString(line) {
			continue
		}
		line = urlFragmentRE.ReplaceAllString(line, "")
		line = hrefFragmentRE.ReplaceAllString(line, "")
		total += len(hexColorRE.FindAllStringIndex(line, -1))
		total += len(colorFunctionRE.FindAllStringIndex(line, -1))
	}
	return total
}

func countRawDeclarations(css string, re *regexp.Regexp, requirePxOrRem bool) int {
	css = cssCommentRE.ReplaceAllString(css, "")
	total := 0
	for _, match := range re.FindAllStringSubmatch(css, -1) {
		value := strings.TrimSpace(strings.ToLower(match[2]))
		if strings.HasPrefix(value, "var(") {
			continue
		}
		if requirePxOrRem && !pxOrRemRE.MatchString(value) {
			continue
		}
		total++
	}
	return total
}

func checkStyleRatchet(t *testing.T, surface string, got, baseline styleRatchetCounts) {
	t.Helper()
	for _, counter := range []struct {
		name     string
		got      int
		baseline int
	}{
		{"style= attributes", got.inlineStyles, baseline.inlineStyles},
		{"raw color literals/functions", got.rawColors, baseline.rawColors},
		{"raw font-size declarations", got.rawFontSizes, baseline.rawFontSizes},
		{"raw padding declarations", got.rawPadding, baseline.rawPadding},
		{"raw border-radius declarations", got.rawBorderRadii, baseline.rawBorderRadii},
	} {
		switch {
		case counter.got > counter.baseline:
			t.Errorf("%s has %d %s, above the baseline of %d.\n"+
				"Dashboard styling debt may only go down: remove the new inline/raw styling or convert existing styling to tokens so the count stays at or below the baseline. "+
				"When cleanup makes this count lower, edit styleRatchetBaselines in pkg/dashboard/webstatic/style_ratchet_test.go to the new value in the same PR.",
				surface, counter.got, counter.name, counter.baseline)
		case counter.got < counter.baseline:
			t.Logf("%s has %d %s, below the baseline of %d — lower styleRatchetBaselines[%q].%s to %d so the ratchet keeps biting",
				surface, counter.got, counter.name, counter.baseline, surface, counterFieldName(counter.name), counter.got)
		}
	}
}

func counterFieldName(counter string) string {
	switch counter {
	case "style= attributes":
		return "inlineStyles"
	case "raw color literals/functions":
		return "rawColors"
	case "raw font-size declarations":
		return "rawFontSizes"
	case "raw padding declarations":
		return "rawPadding"
	case "raw border-radius declarations":
		return "rawBorderRadii"
	default:
		return fmt.Sprintf("%q", counter)
	}
}
