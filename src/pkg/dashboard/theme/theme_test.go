package theme

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestCatalogValidAndLargeEnough(t *testing.T) {
	if len(Catalog()) < 8 {
		t.Fatalf("catalog has %d themes, want at least 8", len(Catalog()))
	}
	if err := ValidateCatalog(); err != nil {
		t.Fatalf("catalog validation failed: %v", err)
	}
	for _, id := range []string{"openclaw", "openclaw-light", "honeycomb", "nord", "dracula", "solarized-dark", "github-light", "high-contrast"} {
		if _, ok := Builtin(id); !ok {
			t.Fatalf("missing built-in theme %q", id)
		}
	}
}

func TestTokenAllowListMatchesDashboardRoot(t *testing.T) {
	b, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatalf("read dashboard index: %v", err)
	}
	root := regexp.MustCompile(`(?s):root\s*\{(.*?)\n\s*\}`).FindSubmatch(b)
	if root == nil {
		t.Fatal("dashboard index has no :root token block")
	}
	re := regexp.MustCompile(`--[a-zA-Z0-9-]+\s*:`)
	got := map[string]struct{}{}
	for _, m := range re.FindAllString(string(root[1]), -1) {
		got[strings.TrimSuffix(strings.TrimSpace(m), ":")] = struct{}{}
	}
	want := TokenAllowList()
	if diff := tokenDiff(got, want); diff != "" {
		t.Fatalf("theme token allow-list drifted from static/index.html :root:\n%s", diff)
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
	th, err := Effective("honeycomb", Overrides{Tokens: map[string]string{"--accent": "#e0a33a"}})
	if err != nil {
		t.Fatalf("effective honeycomb: %v", err)
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

func tokenDiff(got, want map[string]struct{}) string {
	var lines []string
	for k := range got {
		if _, ok := want[k]; !ok {
			lines = append(lines, "+ "+k)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			lines = append(lines, "- "+k)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
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
	light, err := Effective("github-light", Overrides{})
	if err != nil {
		t.Fatalf("github-light effective: %v", err)
	}
	css, err = CSS(light)
	if err != nil {
		t.Fatalf("light css: %v", err)
	}
	if !strings.Contains(css, "body.light-mode") || !strings.Contains(css, "--bg: #ffffff") {
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
