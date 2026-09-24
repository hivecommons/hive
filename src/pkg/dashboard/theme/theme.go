package theme

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

const (
	DefaultDarkID      = "hive"
	DefaultLightID     = "hive-light"
	CustomID           = "custom"
	MaxCustomCSSBytes  = 32 * 1024
	MaxBackgroundBytes = 256 * 1024
	HoneycombDataURI   = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='56' height='97' viewBox='0 0 56 97'%3E%3Cpath d='M28 1l27 15.5v31L28 63 1 47.5v-31zM28 34l27 15.5v31L28 96 1 80.5v-31z' fill='none' stroke='%23e0a33a' stroke-opacity='.09'/%3E%3C/svg%3E"
)

type Theme struct {
	ID          string            `yaml:"id" json:"id"`
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description" json:"description"`
	Author      string            `yaml:"author,omitempty" json:"author,omitempty"`
	Scopes      []string          `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	Dark        bool              `yaml:"dark" json:"dark"`
	Tokens      map[string]string `yaml:"tokens" json:"tokens"`
	LightTokens map[string]string `yaml:"light_tokens,omitempty" json:"light_tokens,omitempty"`
	Fonts       ThemeFonts        `yaml:"fonts" json:"fonts"`
	Background  *ThemeBackground  `yaml:"background,omitempty" json:"background,omitempty"`
	CustomCSS   string            `yaml:"custom_css,omitempty" json:"custom_css,omitempty"`
}

type ThemeFonts struct {
	UI   string `yaml:"ui" json:"ui"`
	Mono string `yaml:"mono" json:"mono"`
}

type ThemeBackground struct {
	Image      string  `yaml:"image,omitempty" json:"image,omitempty"`
	Position   string  `yaml:"position,omitempty" json:"position,omitempty"`
	Size       string  `yaml:"size,omitempty" json:"size,omitempty"`
	Opacity    float64 `yaml:"opacity,omitempty" json:"opacity,omitempty"`
	Attachment string  `yaml:"attachment,omitempty" json:"attachment,omitempty"`
}

type Overrides struct {
	Tokens     map[string]string `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	Fonts      ThemeFonts        `yaml:"fonts,omitempty" json:"fonts,omitempty"`
	Background *ThemeBackground  `yaml:"background,omitempty" json:"background,omitempty"`
	CustomCSS  string            `yaml:"custom_css,omitempty" json:"custom_css,omitempty"`
}

var publicTokenAllowList = map[string]struct{}{
	"--accent": {}, "--acmm-level-1": {}, "--acmm-level-2": {}, "--acmm-level-3": {}, "--acmm-level-4": {}, "--acmm-level-5": {}, "--acmm-level-6": {}, "--amber": {}, "--badge-count-min-w": {}, "--badge-min-h": {}, "--badge-pad-x": {}, "--badge-pad-y": {}, "--bg": {}, "--bg-soft": {}, "--blue": {}, "--border": {}, "--brand": {}, "--btn-pad-x": {}, "--btn-pad-y": {}, "--btn-sm-pad-x": {}, "--btn-sm-pad-y": {}, "--card-bg": {}, "--cc-amber": {}, "--component-border": {}, "--component-border-emphasis": {}, "--component-fill-hover": {}, "--component-status": {}, "--component-tint": {}, "--component-tint-soft": {}, "--component-tint-strong": {}, "--control-compact-min-h": {}, "--control-min-h": {}, "--cyan": {}, "--duration-fast": {}, "--duration-status-pulse": {}, "--empty-state-pad-y": {}, "--fg": {}, "--focus-ring-width": {}, "--font-mono": {}, "--font-ui": {}, "--fs-2xl": {}, "--fs-2xs": {}, "--fs-3xl": {}, "--fs-base": {}, "--fs-lg": {}, "--fs-md": {}, "--fs-sm": {}, "--fs-xl": {}, "--fs-xs": {}, "--full-size": {}, "--fw-bold": {}, "--infra-copy-max": {}, "--infra-logo-chip-bg": {}, "--infra-logo-chip-border": {}, "--infra-logo-chip-fg": {}, "--infra-logo-max-w": {}, "--fw-medium": {}, "--fw-semibold": {}, "--green": {}, "--indigo": {}, "--lh-control": {}, "--lh-tight": {}, "--line": {}, "--line-strong": {}, "--line-subtle": {}, "--line-width": {}, "--metric-tile-min": {}, "--muted": {}, "--oc-accent": {}, "--opacity-disabled": {}, "--orange": {}, "--overlay-scrim": {}, "--panel": {}, "--panel-strong": {}, "--preview-column-min": {}, "--purple": {}, "--r": {}, "--r-lg": {}, "--r-pill": {}, "--r-sm": {}, "--radius": {}, "--radius-lg": {}, "--radius-sm": {}, "--red": {}, "--shadow-card": {}, "--shadow-modal": {}, "--shadow-raised": {}, "--shadow-status-pulse-active": {}, "--shadow-status-pulse-rest": {}, "--sp-0": {}, "--sp-1": {}, "--sp-2": {}, "--sp-3": {}, "--sp-4": {}, "--sp-5": {}, "--sp-6": {}, "--sp-7": {}, "--sp-8": {}, "--sp-9": {}, "--status-attention": {}, "--status-dot-size": {}, "--status-error": {}, "--status-info": {}, "--status-neutral": {}, "--status-ok": {}, "--status-warn": {}, "--surface": {}, "--surface-0": {}, "--surface-1": {}, "--surface-2": {}, "--surface-3": {}, "--surface-terminal": {}, "--table-cell-pad-x": {}, "--table-cell-pad-y": {}, "--terminal-bg": {}, "--terminal-cyan": {}, "--text": {}, "--text-faint": {}, "--text-muted": {}, "--tracking-eyebrow": {}, "--vendor-anthropic": {}, "--vendor-google": {}, "--vendor-openai": {}, "--yellow": {}, "--z-sticky": {},
}

var legacyTokenAllowList = map[string]struct{}{
	"--green-bg": {}, "--green-border": {}, "--oc-accent-light": {}, "--red-bg": {}, "--red-border": {}, "--sidebar-w": {}, "--terminal-fg": {}, "--terminal-line": {}, "--terminal-muted": {},
}

var legacyAliases = map[string]string{
	"--amber":         "--brand",
	"--bg":            "--surface-0",
	"--bg-soft":       "--surface-1",
	"--blue":          "--status-info",
	"--border":        "--line-subtle",
	"--card-bg":       "--surface-2",
	"--cyan":          "--acmm-level-2",
	"--fg":            "--text",
	"--green":         "--status-ok",
	"--indigo":        "--acmm-level-4",
	"--line":          "--line-subtle",
	"--muted":         "--text-muted",
	"--oc-accent":     "--status-error",
	"--orange":        "--status-attention",
	"--panel":         "--surface-2",
	"--panel-strong":  "--surface-3",
	"--purple":        "--acmm-level-5",
	"--radius":        "--r",
	"--radius-lg":     "--r-lg",
	"--radius-sm":     "--r-sm",
	"--red":           "--status-error",
	"--surface":       "--surface-2",
	"--terminal-bg":   "--surface-terminal",
	"--terminal-cyan": "--acmm-level-2",
	"--yellow":        "--status-warn",
}

var tokenAllowList = buildTokenAllowList()

func buildTokenAllowList() map[string]struct{} {
	out := make(map[string]struct{}, len(publicTokenAllowList)+len(legacyTokenAllowList))
	for k := range publicTokenAllowList {
		out[k] = struct{}{}
	}
	for k := range legacyTokenAllowList {
		out[k] = struct{}{}
	}
	return out
}

func TokenAllowList() map[string]struct{} {
	out := make(map[string]struct{}, len(tokenAllowList))
	for k, v := range tokenAllowList {
		out[k] = v
	}
	return out
}

func Builtin(id string) (Theme, bool) {
	id = canonicalThemeID(id)
	for _, th := range Catalog() {
		if th.ID == id {
			return th, true
		}
	}
	return Theme{}, false
}

func ValidateSelection(id string, overrides Overrides) error {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultDarkID
	}
	id = canonicalThemeID(id)
	if id != CustomID {
		if _, ok := Builtin(id); !ok {
			return fmt.Errorf("dashboard.theme %q is not a built-in theme id or %q", id, CustomID)
		}
	}
	return ValidateOverrides(overrides)
}

func ValidateOverrides(overrides Overrides) error {
	for token, value := range overrides.Tokens {
		if _, ok := tokenAllowList[token]; !ok {
			return fmt.Errorf("dashboard.theme_overrides.tokens contains unsupported token %q", token)
		}
		if strings.ContainsAny(value, "<>") || strings.Contains(strings.ToLower(value), "</style") {
			return fmt.Errorf("dashboard.theme_overrides.tokens[%s] contains unsafe CSS characters", token)
		}
	}
	if len([]byte(overrides.CustomCSS)) > MaxCustomCSSBytes {
		return fmt.Errorf("dashboard.theme_overrides.custom_css exceeds %d bytes", MaxCustomCSSBytes)
	}
	if _, err := SanitizeCSS(overrides.CustomCSS); err != nil {
		return fmt.Errorf("dashboard.theme_overrides.custom_css: %w", err)
	}
	if overrides.Background != nil {
		if err := ValidateBackground(*overrides.Background); err != nil {
			return fmt.Errorf("dashboard.theme_overrides.background: %w", err)
		}
	}
	return nil
}

func Effective(id string, overrides Overrides) (Theme, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = DefaultDarkID
	}
	id = canonicalThemeID(id)
	var th Theme
	if id == CustomID {
		th = Theme{ID: CustomID, Name: "Custom", Description: "Operator-defined dashboard theme", Dark: true, Tokens: map[string]string{}}
	} else {
		var ok bool
		th, ok = Builtin(id)
		if !ok {
			return Theme{}, fmt.Errorf("unknown dashboard theme %q", id)
		}
	}
	th.Tokens = cloneMap(th.Tokens)
	th.LightTokens = cloneMap(th.LightTokens)
	for k, v := range overrides.Tokens {
		th.Tokens[k] = strings.TrimSpace(v)
	}
	if strings.TrimSpace(overrides.Fonts.UI) != "" {
		th.Fonts.UI = strings.TrimSpace(overrides.Fonts.UI)
		th.Tokens["--font-ui"] = th.Fonts.UI
	}
	if strings.TrimSpace(overrides.Fonts.Mono) != "" {
		th.Fonts.Mono = strings.TrimSpace(overrides.Fonts.Mono)
		th.Tokens["--font-mono"] = th.Fonts.Mono
	}
	if overrides.Background != nil {
		bg := *overrides.Background
		th.Background = &bg
	}
	css, err := SanitizeCSS(overrides.CustomCSS)
	if err != nil {
		return Theme{}, err
	}
	baseCSS, err := SanitizeCSS(th.CustomCSS)
	if err != nil {
		return Theme{}, err
	}
	if strings.TrimSpace(css) != "" && strings.TrimSpace(baseCSS) != "" {
		th.CustomCSS = baseCSS + "\n" + css
	} else if strings.TrimSpace(css) != "" {
		th.CustomCSS = css
	} else {
		th.CustomCSS = baseCSS
	}
	if err := ValidateTheme(th); err != nil {
		return Theme{}, err
	}
	return th, nil
}

func ValidateTheme(th Theme) error {
	if strings.TrimSpace(th.ID) == "" {
		return fmt.Errorf("theme id is required")
	}
	for token, value := range th.Tokens {
		if err := validateTokenValue("theme "+th.ID, "token", token, value); err != nil {
			return err
		}
	}
	for token, value := range th.LightTokens {
		if err := validateTokenValue("theme "+th.ID, "light token", token, value); err != nil {
			return err
		}
	}
	if len(th.Scopes) > 0 {
		seenScopes := map[string]bool{}
		for _, scope := range th.Scopes {
			scope = strings.TrimSpace(scope)
			if scope != "dashboard" && scope != "contributor" {
				return fmt.Errorf("theme %s has unsupported scope %q", th.ID, scope)
			}
			if seenScopes[scope] {
				return fmt.Errorf("theme %s repeats scope %q", th.ID, scope)
			}
			seenScopes[scope] = true
		}
	}
	if len([]byte(th.CustomCSS)) > MaxCustomCSSBytes {
		return fmt.Errorf("theme %s custom CSS exceeds %d bytes", th.ID, MaxCustomCSSBytes)
	}
	if _, err := SanitizeCSS(th.CustomCSS); err != nil {
		return fmt.Errorf("theme %s custom CSS: %w", th.ID, err)
	}
	if th.Background != nil {
		if err := ValidateBackground(*th.Background); err != nil {
			return fmt.Errorf("theme %s background: %w", th.ID, err)
		}
	}
	return nil
}

func ValidateCatalog() error {
	if err := CatalogError(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, th := range Catalog() {
		if seen[th.ID] {
			return fmt.Errorf("duplicate theme id %q", th.ID)
		}
		seen[th.ID] = true
		if err := ValidateTheme(th); err != nil {
			return err
		}
	}
	return nil
}

func validateTokenValue(context, label, token, value string) error {
	if _, ok := tokenAllowList[token]; !ok {
		return fmt.Errorf("%s uses unsupported %s %q", context, label, token)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s %s %q is empty", context, label, token)
	}
	if strings.ContainsAny(value, "<>") || strings.Contains(strings.ToLower(value), "</style") {
		return fmt.Errorf("%s %s %q contains unsafe CSS characters", context, label, token)
	}
	return nil
}

func CSS(th Theme) (string, error) {
	return css(th, false)
}

func PreviewCSS(th Theme) (string, error) {
	return css(th, true)
}

func css(th Theme, keepDarkLightMode bool) (string, error) {
	if err := ValidateTheme(th); err != nil {
		return "", err
	}
	rootTokens := compileTokens(th.Tokens)
	var b strings.Builder
	b.WriteString("/* hive dashboard theme: ")
	b.WriteString(th.ID)
	b.WriteString(" */\n:root{\n")
	writeTokenBlock(&b, rootTokens)
	writeContributorAliases(&b)
	b.WriteString("}\n")
	if len(th.LightTokens) > 0 {
		b.WriteString("body.light-mode{\n")
		writeTokenBlock(&b, compileTokens(th.LightTokens))
		writeContributorAliases(&b)
		b.WriteString("}\n")
	} else if !th.Dark || keepDarkLightMode {
		b.WriteString("body.light-mode{\n")
		writeTokenBlock(&b, rootTokens)
		writeContributorAliases(&b)
		b.WriteString("}\n")
	} else {
		writeLightModeAccentFallback(&b, rootTokens)
	}
	if th.Background != nil && strings.TrimSpace(th.Background.Image) != "" {
		opacity := th.Background.Opacity
		if opacity <= 0 || opacity > 1 {
			opacity = 0.08
		}
		pos := defaultString(th.Background.Position, "center")
		size := defaultString(th.Background.Size, "cover")
		attach := defaultString(th.Background.Attachment, "fixed")
		b.WriteString("body::before{content:\"\";position:fixed;inset:0;pointer-events:none;z-index:-1;background-image:url(\"")
		b.WriteString(th.Background.Image)
		b.WriteString("\");background-position:")
		b.WriteString(pos)
		b.WriteString(";background-size:")
		b.WriteString(size)
		b.WriteString(";background-repeat:repeat;background-attachment:")
		b.WriteString(attach)
		b.WriteString(";opacity:")
		b.WriteString(fmt.Sprintf("%.3g", opacity))
		b.WriteString(";}\n")
	}
	if th.CustomCSS != "" {
		b.WriteString(th.CustomCSS)
		if !strings.HasSuffix(th.CustomCSS, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

func compileTokens(tokens map[string]string) map[string]string {
	out := make(map[string]string, len(tokens)+len(legacyAliases))
	for token, value := range tokens {
		value = strings.TrimSpace(value)
		if _, deprecated := legacyAliases[token]; !deprecated {
			out[token] = value
		}
	}
	for token, value := range tokens {
		canonical, deprecated := legacyAliases[token]
		if !deprecated {
			continue
		}
		if _, canonicalSet := out[canonical]; !canonicalSet {
			out[canonical] = strings.TrimSpace(value)
		}
	}
	return out
}

func writeTokenBlock(b *strings.Builder, tokens map[string]string) {
	for _, k := range keysFor(tokens) {
		b.WriteString("  ")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(tokens[k])
		b.WriteString(";\n")
	}
	for _, legacy := range legacyAliasKeysFor(tokens) {
		b.WriteString("  ")
		b.WriteString(legacy)
		b.WriteString(": var(")
		b.WriteString(legacyAliases[legacy])
		b.WriteString(");\n")
	}
}

func ETag(th Theme) (string, error) {
	css, err := CSS(th)
	if err != nil {
		return "", err
	}
	return cssETag(css), nil
}

func PreviewETag(th Theme) (string, error) {
	css, err := PreviewCSS(th)
	if err != nil {
		return "", err
	}
	return cssETag(css), nil
}

func cssETag(css string) string {
	sum := sha256.Sum256([]byte(css))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

var cssURLRe = regexp.MustCompile(`(?is)(@import\s+)(?:url\()?['"]?([^'"\)\s;]+)|url\(\s*['"]?([^'"\)]+)['"]?\s*\)`)

func SanitizeCSS(css string) (string, error) {
	if len([]byte(css)) > MaxCustomCSSBytes {
		return "", fmt.Errorf("exceeds %d bytes", MaxCustomCSSBytes)
	}
	css = strings.ReplaceAll(css, "</style", "/style")
	css = strings.ReplaceAll(css, "<", "")
	css = strings.ReplaceAll(css, ">", "")
	for _, m := range cssURLRe.FindAllStringSubmatch(css, -1) {
		raw := m[2]
		if raw == "" {
			raw = m[3]
		}
		if err := validateCSSURL(raw); err != nil {
			return "", err
		}
	}
	return css, nil
}

func ValidateBackground(bg ThemeBackground) error {
	if strings.TrimSpace(bg.Image) == "" {
		return nil
	}
	if len([]byte(bg.Image)) > MaxBackgroundBytes {
		return fmt.Errorf("image exceeds %d bytes", MaxBackgroundBytes)
	}
	if err := validateCSSURL(bg.Image); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	if bg.Opacity < 0 || bg.Opacity > 1 {
		return fmt.Errorf("opacity must be between 0 and 1")
	}
	if bg.Attachment != "" && bg.Attachment != "fixed" && bg.Attachment != "scroll" && bg.Attachment != "local" {
		return fmt.Errorf("attachment must be fixed, scroll, or local")
	}
	return nil
}

func validateCSSURL(raw string) error {
	raw = strings.TrimSpace(strings.Trim(raw, "'\""))
	if raw == "" {
		return nil
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:") {
		if len([]byte(raw)) > MaxBackgroundBytes {
			return fmt.Errorf("data URI exceeds %d bytes", MaxBackgroundBytes)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("url %q must be absolute https or data URI", raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("url %q must use https", raw)
	}
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeLightModeAccentFallback(b *strings.Builder, tokens map[string]string) {
	accentTokens := map[string]string{}
	for _, token := range []string{"--accent", "--brand"} {
		if value := strings.TrimSpace(tokens[token]); value != "" {
			accentTokens[token] = value
		}
	}
	if len(accentTokens) == 0 {
		return
	}
	b.WriteString("body.light-mode{\n")
	writeTokenBlock(b, accentTokens)
	writeContributorAliases(b)
	b.WriteString("}\n")
}

func writeContributorAliases(b *strings.Builder) {
	b.WriteString("  --cc-bg: var(--surface-0);\n")
	b.WriteString("  --cc-bg-deep: var(--surface-1);\n")
	b.WriteString("  --cc-surface: var(--surface-2);\n")
	b.WriteString("  --cc-border: var(--line-subtle);\n")
	b.WriteString("  --cc-border-2: var(--line-strong);\n")
	b.WriteString("  --cc-text: var(--text);\n")
	b.WriteString("  --cc-text-2: var(--text);\n")
	b.WriteString("  --cc-muted: var(--text-muted);\n")
	b.WriteString("  --cc-muted-2: var(--text-muted);\n")
	b.WriteString("  --cc-code-bg: var(--surface-1);\n")
	b.WriteString("  --cc-accent: var(--accent);\n")
	b.WriteString("  --cc-accent-2: var(--status-info);\n")
	b.WriteString("  --cc-accent-fg: var(--accent);\n")
	b.WriteString("  --cc-green: var(--status-ok);\n")
	b.WriteString("  --cc-amber: var(--brand);\n")
	b.WriteString("  --cc-red: var(--status-error);\n")
	b.WriteString("  --cc-pink: var(--acmm-level-5);\n")
	b.WriteString("  --cc-purple: var(--acmm-level-5);\n")
}

func keysFor(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func legacyAliasKeysFor(tokens map[string]string) []string {
	keys := make([]string, 0, len(legacyAliases))
	for legacy, canonical := range legacyAliases {
		if _, ok := tokens[canonical]; ok {
			keys = append(keys, legacy)
		}
	}
	sort.Strings(keys)
	return keys
}

func defaultString(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
}
