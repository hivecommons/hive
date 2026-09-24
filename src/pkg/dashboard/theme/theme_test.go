package theme

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestLegacyThemeAliasesRemainValid(t *testing.T) {
	want := map[string]string{
		"openclaw":       "hive-dark",
		"openclaw-light": "hive-light",
		"honeycomb":      "hive",
		"graphite":       "hive-dark",
		"dracula":        "cyberpunk",
		"github-light":   "hive-light",
		"high-contrast":  "terminal",
	}
	for oldID, newID := range want {
		th, ok := Builtin(oldID)
		if !ok || th.ID != newID {
			t.Fatalf("Builtin(%q) = %q, %v; want %q, true", oldID, th.ID, ok, newID)
		}
		if err := ValidateSelection(oldID, Overrides{}); err != nil {
			t.Fatalf("legacy theme %q did not validate: %v", oldID, err)
		}
	}
}

func TestCatalogValidAndLargeEnough(t *testing.T) {
	if len(Catalog()) < 17 {
		t.Fatalf("catalog has %d themes, want at least 17", len(Catalog()))
	}
	if err := ValidateCatalog(); err != nil {
		t.Fatalf("catalog validation failed: %v", err)
	}
	for _, id := range []string{"hive", "hive-dark", "hive-light", "star-wars", "dungeons-and-dragons", "star-trek", "cyberpunk", "terminal", "solarized-dark", "nord", "contributor-rank-metal", "contributor-verdant", "contributor-amber-rank", "contributor-violet-advisor", "contributor-minimal", "contributor-rose", "contributor-roomy-ranked"} {
		th, ok := Builtin(id)
		if !ok {
			t.Fatalf("missing built-in theme %q", id)
		}
		if len(th.Scopes) == 0 {
			t.Fatalf("theme %q has no scopes", id)
		}
	}
}

func TestEveryThemeFileParsesWithUniqueID(t *testing.T) {
	entries, err := os.ReadDir("themes")
	if err != nil {
		t.Fatalf("read themes dir: %v", err)
	}
	files := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yaml") {
			files++
		}
	}
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("load embedded theme catalog: %v", err)
	}
	if len(catalog) != files {
		t.Fatalf("catalog loaded %d themes, want %d yaml files", len(catalog), files)
	}
	seen := map[string]bool{}
	for _, th := range catalog {
		if seen[th.ID] {
			t.Fatalf("duplicate theme id %q", th.ID)
		}
		seen[th.ID] = true
	}
}

func TestTokenAllowListMatchesDashboardRoot(t *testing.T) {
	b, err := os.ReadFile("../static/tokens.css")
	if err != nil {
		t.Fatalf("read dashboard tokens: %v", err)
	}
	root := regexp.MustCompile(`(?s):root\s*\{(.*?)\n\}`).FindSubmatch(b)
	if root == nil {
		t.Fatal("dashboard tokens.css has no :root token block")
	}
	re := regexp.MustCompile(`--[a-zA-Z0-9-]+\s*:`)
	got := map[string]struct{}{}
	for _, m := range re.FindAllString(string(root[1]), -1) {
		got[strings.TrimSuffix(strings.TrimSpace(m), ":")] = struct{}{}
	}
	want := TokenAllowList()
	var missing []string
	for token := range got {
		if _, ok := want[token]; !ok {
			missing = append(missing, token)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("theme token allow-list missing tokens.css :root tokens:\n%s", strings.Join(missing, "\n"))
	}
}

func TestCSSCompilesLegacyTokensToCanonicalAliases(t *testing.T) {
	th := Theme{ID: "legacy", Tokens: map[string]string{
		"--bg":     "#010203",
		"--panel":  "#111213",
		"--line":   "#212223",
		"--amber":  "#313233",
		"--blue":   "#414243",
		"--accent": "#515253",
	}}
	css, err := CSS(th)
	if err != nil {
		t.Fatalf("CSS legacy: %v", err)
	}
	for _, want := range []string{
		"--surface-0: #010203;",
		"--surface-2: #111213;",
		"--line-subtle: #212223;",
		"--brand: #313233;",
		"--status-info: #414243;",
		"--bg: var(--surface-0);",
		"--panel: var(--surface-2);",
		"--line: var(--line-subtle);",
		"--amber: var(--brand);",
		"--blue: var(--status-info);",
		"--accent: #515253;",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("legacy CSS missing %q:\n%s", want, css)
		}
	}
}

func TestCSSCanonicalTokenWinsOverLegacyToken(t *testing.T) {
	th := Theme{ID: "mixed", Tokens: map[string]string{
		"--bg":        "#010203",
		"--surface-0": "#aabbcc",
	}, LightTokens: map[string]string{
		"--bg":        "#111111",
		"--surface-0": "#eeeeee",
	}}
	css, err := CSS(th)
	if err != nil {
		t.Fatalf("CSS mixed: %v", err)
	}
	if !strings.Contains(css, "--surface-0: #aabbcc;") || strings.Contains(css, "--surface-0: #010203;") {
		t.Fatalf("root canonical token did not win:\n%s", css)
	}
	if !strings.Contains(css, "body.light-mode{\n  --surface-0: #eeeeee;") || strings.Contains(css, "--surface-0: #111111;") {
		t.Fatalf("light canonical token did not win:\n%s", css)
	}
	if got := strings.Count(css, "--bg: var(--surface-0);"); got != 2 {
		t.Fatalf("legacy alias count = %d, want 2:\n%s", got, css)
	}
}

func TestDarkThemesUseSharedLightRemapInServedLightMode(t *testing.T) {
	for _, th := range Catalog() {
		if !th.Dark || len(th.LightTokens) > 0 {
			continue
		}
		css, err := CSS(th)
		if err != nil {
			t.Fatalf("CSS(%s): %v", th.ID, err)
		}
		lightBlock := cssBlock(t, css, "body.light-mode")
		tokens := compileTokens(th.Tokens)
		for _, token := range []string{"--surface-0", "--surface-2", "--text"} {
			want := tokens[token]
			if want == "" {
				t.Fatalf("%s missing token %s", th.ID, token)
			}
			if strings.Contains(lightBlock, token+": "+want+";") {
				t.Fatalf("%s served light-mode block re-emits dark %s=%s:\n%s", th.ID, token, want, lightBlock)
			}
		}
		if accent := tokens["--accent"]; accent != "" && !strings.Contains(lightBlock, "--accent: "+accent+";") {
			t.Fatalf("%s served light-mode block should keep accent %s:\n%s", th.ID, accent, lightBlock)
		}
	}
}

func TestPreviewCSSKeepsDarkThemesOwnPaletteInLightMode(t *testing.T) {
	for _, th := range Catalog() {
		if !th.Dark || len(th.LightTokens) > 0 {
			continue
		}
		css, err := PreviewCSS(th)
		if err != nil {
			t.Fatalf("PreviewCSS(%s): %v", th.ID, err)
		}
		lightBlock := cssBlock(t, css, "body.light-mode")
		for _, token := range []string{"--surface-0", "--surface-2", "--text"} {
			want := compileTokens(th.Tokens)[token]
			if want == "" {
				t.Fatalf("%s missing token %s", th.ID, token)
			}
			if !strings.Contains(lightBlock, token+": "+want+";") {
				t.Fatalf("%s preview light-mode block did not keep %s=%s:\n%s", th.ID, token, want, lightBlock)
			}
		}
	}
}

func TestBuiltinThemesDifferFromDefaultCoreTokens(t *testing.T) {
	defaultTheme, ok := Builtin(DefaultDarkID)
	if !ok {
		t.Fatalf("missing default theme %q", DefaultDarkID)
	}
	defaultTokens := compileTokens(defaultTheme.Tokens)
	core := []string{"--surface-0", "--surface-2", "--text", "--accent"}
	for _, th := range Catalog() {
		if th.ID == DefaultDarkID {
			continue
		}
		tokens := compileTokens(th.Tokens)
		different := false
		for _, token := range core {
			if tokens[token] == "" {
				t.Fatalf("%s missing core token %s", th.ID, token)
			}
			if tokens[token] != defaultTokens[token] {
				different = true
			}
		}
		if !different {
			t.Fatalf("%s core tokens match default %s", th.ID, DefaultDarkID)
		}
		css, err := CSS(th)
		if err != nil {
			t.Fatalf("CSS(%s): %v", th.ID, err)
		}
		for _, token := range core {
			if !strings.Contains(css, token+": "+tokens[token]+";") {
				t.Fatalf("%s compiled CSS missing %s=%s", th.ID, token, tokens[token])
			}
		}
	}
}

func cssBlock(t *testing.T, css, selector string) string {
	t.Helper()
	start := strings.Index(css, selector+"{")
	if start < 0 {
		t.Fatalf("CSS missing selector %s", selector)
	}
	start += len(selector) + 1
	end := strings.Index(css[start:], "}")
	if end < 0 {
		t.Fatalf("CSS selector %s has no closing brace", selector)
	}
	return css[start : start+end]
}

func TestSanitizeCSSGuardrails(t *testing.T) {
	safe, err := SanitizeCSS(".panel{background:url(https://example.org/a.svg)} </style><bad")
	if err != nil {
		t.Fatalf("sanitize safe css: %v", err)
	}
	if strings.Contains(safe, "<") || strings.Contains(strings.ToLower(safe), "</style") {
		t.Fatalf("unsafe sequence survived sanitizer: %q", safe)
	}
	for _, css := range []string{
		"@import url(http://example.org/x.css);",
		".x{background:url(//example.org/x.png)}",
		".x{background:url(/local.png)}",
	} {
		if _, err := SanitizeCSS(css); err == nil {
			t.Fatalf("SanitizeCSS(%q) succeeded; want non-https url rejection", css)
		}
	}
}

func TestEffectiveCSSIncludesBackgroundAndETag(t *testing.T) {
	th, err := Effective("hive", Overrides{Tokens: map[string]string{"--accent": "#e0a33a"}})
	if err != nil {
		t.Fatalf("effective hive: %v", err)
	}
	css, err := CSS(th)
	if err != nil {
		t.Fatalf("css: %v", err)
	}
	if !strings.Contains(css, "#e0a33a") || !strings.Contains(css, "body::before") {
		t.Fatalf("css missing override/background: %s", css)
	}
	etag, err := ETag(th)
	if err != nil || !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") {
		t.Fatalf("etag = %q, %v", etag, err)
	}
}

func TestValidationErrorBranches(t *testing.T) {
	if err := ValidateSelection("missing", Overrides{}); err == nil {
		t.Fatal("unknown selection accepted")
	}
	if err := ValidateSelection(CustomID, Overrides{Tokens: map[string]string{"--accent": "#fff"}}); err != nil {
		t.Fatalf("custom selection rejected: %v", err)
	}
	badCSS := strings.Repeat("x", MaxCustomCSSBytes+1)
	cases := []Overrides{
		{Tokens: map[string]string{"--not-a-token": "red"}},
		{Tokens: map[string]string{"--accent": "<red>"}},
		{CustomCSS: badCSS},
		{CustomCSS: ".x{background:url(http://example.org/x)}"},
		{Background: &ThemeBackground{Image: "ftp://example.org/x", Opacity: 0.5}},
	}
	for _, tc := range cases {
		if err := ValidateOverrides(tc); err == nil {
			t.Fatalf("ValidateOverrides(%+v) succeeded; want error", tc)
		}
	}
}

func TestEffectiveBranches(t *testing.T) {
	if _, err := Effective("missing", Overrides{}); err == nil {
		t.Fatal("unknown effective theme accepted")
	}
	th, err := Effective(CustomID, Overrides{
		Tokens: map[string]string{"--accent": "#123456"},
		Fonts:  ThemeFonts{UI: "Custom UI", Mono: "Custom Mono"},
		Background: &ThemeBackground{Image: "https://example.org/bg.svg", Position: "top left", Size: "contain", Opacity: 0.25,
			Attachment: "scroll"},
		CustomCSS: ".x{color:#fff}",
	})
	if err != nil {
		t.Fatalf("custom effective theme: %v", err)
	}
	if th.Tokens["--font-ui"] != "Custom UI" || th.Tokens["--font-mono"] != "Custom Mono" || th.Background.Attachment != "scroll" || th.CustomCSS == "" {
		t.Fatalf("overrides not applied: %+v", th)
	}
	css, err := CSS(th)
	if err != nil {
		t.Fatalf("css custom: %v", err)
	}
	for _, want := range []string{"background-position:top left", "background-size:contain", "background-attachment:scroll", ".x{color:#fff}"} {
		if !strings.Contains(css, want) {
			t.Fatalf("custom css missing %q: %s", want, css)
		}
	}
	light, err := Effective("hive-light", Overrides{})
	if err != nil {
		t.Fatalf("hive-light effective: %v", err)
	}
	css, err = CSS(light)
	if err != nil {
		t.Fatalf("light css: %v", err)
	}
	if !strings.Contains(css, "body.light-mode") || !strings.Contains(css, "--surface-0: #f7f8fa") || !strings.Contains(css, "--bg: var(--surface-0)") {
		t.Fatalf("light css missing body remap: %s", css)
	}
}

func TestValidateThemeAndBackgroundFailures(t *testing.T) {
	for _, th := range []Theme{
		{},
		{ID: "bad", Tokens: map[string]string{"--missing": "red"}},
		{ID: "bad", Tokens: map[string]string{"--accent": ""}},
		{ID: "bad", Tokens: map[string]string{"--accent": "<red>"}},
		{ID: "bad", Tokens: map[string]string{"--accent": "red"}, CustomCSS: strings.Repeat("x", MaxCustomCSSBytes+1)},
		{ID: "bad", Tokens: map[string]string{"--accent": "red"}, Background: &ThemeBackground{Image: "http://example.org/bg.png"}},
	} {
		if err := ValidateTheme(th); err == nil {
			t.Fatalf("ValidateTheme(%+v) succeeded; want error", th)
		}
	}
	if err := ValidateBackground(ThemeBackground{Image: "", Opacity: 0}); err != nil {
		t.Fatalf("empty background rejected: %v", err)
	}
	for _, bg := range []ThemeBackground{
		{Image: strings.Repeat("a", MaxBackgroundBytes+1)},
		{Image: "https://example.org/bg.svg", Opacity: 1.5},
		{Image: "https://example.org/bg.svg", Opacity: 0.5, Attachment: "pinned"},
	} {
		if err := ValidateBackground(bg); err == nil {
			t.Fatalf("ValidateBackground(%+v) succeeded; want error", bg)
		}
	}
}

func TestBuiltinAndCSSFailureBranches(t *testing.T) {
	if _, ok := Builtin("missing"); ok {
		t.Fatal("missing built-in reported present")
	}
	bad := Theme{ID: "bad", Tokens: map[string]string{"--not-real": "red"}}
	if _, err := CSS(bad); err == nil {
		t.Fatal("CSS accepted invalid theme")
	}
	if _, err := ETag(bad); err == nil {
		t.Fatal("ETag accepted invalid theme")
	}
}
