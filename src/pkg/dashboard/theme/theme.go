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
	DefaultDarkID      = "openclaw"
	DefaultLightID     = "openclaw-light"
	CustomID           = "custom"
	MaxCustomCSSBytes  = 32 * 1024
	MaxBackgroundBytes = 256 * 1024
	HoneycombDataURI   = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='56' height='97' viewBox='0 0 56 97'%3E%3Cpath d='M28 1l27 15.5v31L28 63 1 47.5v-31zM28 34l27 15.5v31L28 96 1 80.5v-31z' fill='none' stroke='%23e0a33a' stroke-opacity='.09'/%3E%3C/svg%3E"
)

type Theme struct {
	ID          string            `yaml:"id" json:"id"`
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description" json:"description"`
	Dark        bool              `yaml:"dark" json:"dark"`
	Tokens      map[string]string `yaml:"tokens" json:"tokens"`
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

var tokenAllowList = map[string]struct{}{
	"--accent": {}, "--amber": {}, "--bg": {}, "--bg-soft": {}, "--blue": {}, "--border": {}, "--card-bg": {}, "--cyan": {}, "--fg": {}, "--font-mono": {}, "--font-ui": {}, "--fs-base": {}, "--fs-lg": {}, "--fs-md": {}, "--fs-sm": {}, "--fs-xl": {}, "--fs-xs": {}, "--green": {}, "--green-bg": {}, "--green-border": {}, "--indigo": {}, "--line": {}, "--line-strong": {}, "--muted": {}, "--oc-accent": {}, "--oc-accent-light": {}, "--orange": {}, "--panel": {}, "--panel-strong": {}, "--purple": {}, "--radius": {}, "--radius-lg": {}, "--radius-sm": {}, "--red": {}, "--red-bg": {}, "--red-border": {}, "--shadow-modal": {}, "--shadow-raised": {}, "--sidebar-w": {}, "--surface": {}, "--terminal-bg": {}, "--terminal-cyan": {}, "--terminal-fg": {}, "--terminal-line": {}, "--terminal-muted": {}, "--text": {}, "--yellow": {},
}

func TokenAllowList() map[string]struct{} {
	out := make(map[string]struct{}, len(tokenAllowList))
	for k, v := range tokenAllowList {
		out[k] = v
	}
	return out
}

func Builtin(id string) (Theme, bool) {
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
	th.CustomCSS = css
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
		if _, ok := tokenAllowList[token]; !ok {
			return fmt.Errorf("theme %s uses unsupported token %q", th.ID, token)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("theme %s token %q is empty", th.ID, token)
		}
		if strings.ContainsAny(value, "<>") || strings.Contains(strings.ToLower(value), "</style") {
			return fmt.Errorf("theme %s token %q contains unsafe CSS characters", th.ID, token)
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

func CSS(th Theme) (string, error) {
	if err := ValidateTheme(th); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("/* hive dashboard theme: ")
	b.WriteString(th.ID)
	b.WriteString(" */\n:root{\n")
	keys := make([]string, 0, len(th.Tokens))
	for k := range th.Tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("  ")
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(th.Tokens[k])
		b.WriteString(";\n")
	}
	b.WriteString("}\nbody.light-mode{\n")
	if th.Dark {
		if light, ok := Builtin(DefaultLightID); ok {
			for _, k := range keysFor(light.Tokens) {
				b.WriteString("  ")
				b.WriteString(k)
				b.WriteString(": ")
				b.WriteString(light.Tokens[k])
				b.WriteString(";\n")
			}
		}
	} else {
		for _, k := range keys {
			b.WriteString("  ")
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(th.Tokens[k])
			b.WriteString(";\n")
		}
	}
	b.WriteString("}\n")
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

func ETag(th Theme) (string, error) {
	css, err := CSS(th)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(css))
	return `"` + hex.EncodeToString(sum[:]) + `"`, nil
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

func keysFor(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
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
