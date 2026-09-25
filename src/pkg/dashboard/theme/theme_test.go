package theme

import (
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	wcagAAMinContrastRatio = 4.5
	srgbMax                = 255
	srgbLinearThreshold    = 0.03928
	srgbLinearDivisor      = 12.92
	srgbGammaOffset        = 0.055
	srgbGammaScale         = 1.055
	srgbGamma              = 2.4
	luminanceRedWeight     = 0.2126
	luminanceGreenWeight   = 0.7152
	luminanceBlueWeight    = 0.0722
	luminanceContrastBias  = 0.05
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
	th := Theme{ID: "legacy", Dark: true, Tokens: map[string]string{
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
	th := Theme{ID: "mixed", Dark: true, Tokens: map[string]string{
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

func TestBuiltinThemesProvideAccessibleLightAndDarkModes(t *testing.T) {
	for _, th := range Catalog() {
		dark := darkModeTokens(th)
		light := lightModeTokens(th)
		assertModeContrast(t, th.ID, "dark", dark)
		assertModeContrast(t, th.ID, "light", light)
		if dark["--surface-0"] == light["--surface-0"] && dark["--text"] == light["--text"] {
			t.Fatalf("%s dark and light modes use the same main palette", th.ID)
		}
		accent := strings.TrimSpace(compileTokens(th.Tokens)["--accent"])
		if accent != "" && strings.TrimSpace(dark["--accent"]) != accent {
			t.Fatalf("%s dark mode lost accent: got %q want %q", th.ID, dark["--accent"], accent)
		}
		if accent != "" && strings.TrimSpace(light["--accent"]) != accent {
			t.Fatalf("%s light mode lost accent: got %q want %q", th.ID, light["--accent"], accent)
		}
		css, err := CSS(th)
		if err != nil {
			t.Fatalf("CSS(%s): %v", th.ID, err)
		}
		if !strings.Contains(css, ":root{\n") || !strings.Contains(css, "body.light-mode{\n") {
			t.Fatalf("%s CSS must emit both mode blocks:\n%s", th.ID, css)
		}
	}
}

func assertModeContrast(t *testing.T, themeID, mode string, tokens map[string]string) {
	t.Helper()
	bg := strings.TrimSpace(tokens["--surface-0"])
	text := strings.TrimSpace(tokens["--text"])
	if bg == "" || text == "" {
		t.Fatalf("%s %s mode missing --surface-0/--text: %#v", themeID, mode, tokens)
	}
	ratio, ok := contrastRatio(text, bg)
	if !ok {
		t.Fatalf("%s %s mode must use hex --surface-0/--text, got %s on %s", themeID, mode, text, bg)
	}
	if ratio < wcagAAMinContrastRatio {
		t.Fatalf("%s %s mode contrast %.2f:1 for %s on %s, want at least %.1f:1", themeID, mode, ratio, text, bg, wcagAAMinContrastRatio)
	}
}

func contrastRatio(fg, bg string) (float64, bool) {
	frgb, ok := parseHexColor(fg)
	if !ok {
		return 0, false
	}
	brgb, ok := parseHexColor(bg)
	if !ok {
		return 0, false
	}
	fl := relativeLuminance(frgb)
	bl := relativeLuminance(brgb)
	if fl < bl {
		fl, bl = bl, fl
	}
	return (fl + luminanceContrastBias) / (bl + luminanceContrastBias), true
}

func relativeLuminance(rgb [3]int) float64 {
	channel := func(v int) float64 {
		c := float64(v) / srgbMax
		if c <= srgbLinearThreshold {
			return c / srgbLinearDivisor
		}
		return math.Pow((c+srgbGammaOffset)/srgbGammaScale, srgbGamma)
	}
	return luminanceRedWeight*channel(rgb[0]) + luminanceGreenWeight*channel(rgb[1]) + luminanceBlueWeight*channel(rgb[2])
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
	for _, want := range []string{
		"body{isolation:isolate;}",
		"body::before{content:\"\";position:fixed;inset:0;pointer-events:none;z-index:-1;",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("background layer CSS missing %q: %s", want, css)
		}
	}
	// A stacking context on body's children traps modal overlays (Settings,
	// ACMM, dialogs) inside their wrappers so later siblings paint over them.
	for _, forbidden := range []string{"body>*", "body > *", "body>div", "body > div"} {
		if strings.Contains(css, forbidden) {
			t.Fatalf("theme CSS must not style body's children (%q traps modal overlays): %s", forbidden, css)
		}
	}
	etag, err := ETag(th)
	if err != nil || !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") {
		t.Fatalf("etag = %q, %v", etag, err)
	}
}

func TestBackgroundScopeCSS(t *testing.T) {
	cases := []struct {
		name          string
		scope         string
		wantPageLayer bool
		wantCardLayer bool
	}{
		{name: "default page", wantPageLayer: true},
		{name: "page", scope: "page", wantPageLayer: true},
		{name: "cards", scope: "cards", wantCardLayer: true},
		{name: "both", scope: "both", wantPageLayer: true, wantCardLayer: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			th, err := Effective("hive", Overrides{Background: &ThemeBackground{
				Image:      HoneycombDataURI,
				Scope:      tc.scope,
				Opacity:    0.1,
				Attachment: "fixed",
			}})
			if err != nil {
				t.Fatalf("effective: %v", err)
			}
			css, err := CSS(th)
			if err != nil {
				t.Fatalf("css: %v", err)
			}
			if got := strings.Contains(css, "body::before"); got != tc.wantPageLayer {
				t.Fatalf("body::before presence = %v, want %v:\n%s", got, tc.wantPageLayer, css)
			}
			if got := strings.Contains(css, ".agent-card::before,.repo-card::before,.card-inset::before,.card-tile::before,.row-card::before,.campaigns-card::before,.nous-card::before,.kb-setup-card::before"); got != tc.wantCardLayer {
				t.Fatalf("card ::before presence = %v, want %v:\n%s", got, tc.wantCardLayer, css)
			}
			if tc.wantCardLayer {
				for _, want := range []string{
					".agent-card,.repo-card,.card-inset,.card-tile,.row-card,.campaigns-card,.nous-card,.kb-setup-card{position:relative;isolation:isolate;}",
					".agent-card:hover,.agent-card:focus-within,.repo-card:hover,.repo-card:focus-within,.card-inset:hover,.card-inset:focus-within,.card-tile:hover,.card-tile:focus-within,.row-card:hover,.row-card:focus-within,.campaigns-card:hover,.campaigns-card:focus-within,.nous-card:hover,.nous-card:focus-within,.kb-setup-card:hover,.kb-setup-card:focus-within{z-index:1;}",
					".agent-card::before,.repo-card::before,.card-inset::before,.card-tile::before,.row-card::before,.campaigns-card::before,.nous-card::before,.kb-setup-card::before{content:\"\";position:absolute;inset:0;pointer-events:none;z-index:-1;border-radius:inherit;",
					"background-repeat:repeat;opacity:0.06;",
				} {
					if !strings.Contains(css, want) {
						t.Fatalf("card watermark CSS missing %q:\n%s", want, css)
					}
				}
			}
			for _, forbidden := range []string{"body>*", "body > *", "body>div", "body > div"} {
				if strings.Contains(css, forbidden) {
					t.Fatalf("theme CSS must not style body's children (%q traps modal overlays): %s", forbidden, css)
				}
			}
		})
	}
}

func TestBackgroundScopeValidation(t *testing.T) {
	err := ValidateBackground(ThemeBackground{Image: HoneycombDataURI, Scope: "sidebar"})
	if err == nil || !strings.Contains(err.Error(), "scope must be page, cards, or both") {
		t.Fatalf("invalid background scope error = %v", err)
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

func TestPreviewHelpersAndDerivedModeBranches(t *testing.T) {
	if got := CanonicalID(" openclaw "); got != "hive-dark" {
		t.Fatalf("CanonicalID(openclaw) = %q, want hive-dark", got)
	}
	th := Theme{ID: "preview", Dark: true, Tokens: map[string]string{"--surface-0": "#000000", "--text": "#ffffff"}}
	css, err := PreviewCSS(th)
	if err != nil {
		t.Fatalf("PreviewCSS: %v", err)
	}
	if !strings.Contains(css, "body.light-mode") || !strings.Contains(css, "--surface-0: #f1f6fc;") {
		t.Fatalf("PreviewCSS did not derive a light palette with fallback accent:\n%s", css)
	}
	etag, err := PreviewETag(th)
	if err != nil {
		t.Fatalf("PreviewETag: %v", err)
	}
	if etag == "" || !strings.HasPrefix(etag, "\"") || !strings.HasSuffix(etag, "\"") {
		t.Fatalf("PreviewETag = %q", etag)
	}
	dark := deriveModeTokens(map[string]string{"--accent": "not-a-color"}, false)
	if dark["--accent"] != "not-a-color" || dark["--surface-0"] == "" {
		t.Fatalf("dark derived palette lost invalid-but-allowed accent context: %#v", dark)
	}
	if got := mixHex("#000000", "#ffffff", -1); got != "#000000" {
		t.Fatalf("mixHex negative ratio = %s, want base", got)
	}
	if got := mixHex("#000000", "#ffffff", 2); got != "#ffffff" {
		t.Fatalf("mixHex high ratio = %s, want accent", got)
	}
	if got := mixHex("bad", "#ffffff", 0.5); got != "bad" {
		t.Fatalf("mixHex invalid base = %s, want original base", got)
	}
	if _, ok := parseHexColor("#gggggg"); ok {
		t.Fatal("parseHexColor accepted invalid hex")
	}
}
