package webstatic

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type cssColor struct {
	r float64
	g float64
	b float64
	a float64
}

type contrastCase struct {
	mode      string
	fg        string
	bg        string
	minRatio  float64
	ratio     float64
	component string
}

func TestDashboardTokenContrast(t *testing.T) {
	tokenData, err := os.ReadFile("../static/tokens.css")
	if err != nil {
		t.Fatalf("reading tokens.css: %v", err)
	}
	componentData, err := os.ReadFile("../static/components.css")
	if err != nil {
		t.Fatalf("reading components.css: %v", err)
	}

	root := parseCustomProperties(t, string(tokenData), ":root")
	lightOverrides := parseCustomProperties(t, string(tokenData), "body.light-mode")
	modes := map[string]map[string]string{
		"dark":  root,
		"light": mergeCustomProperties(root, lightOverrides),
	}

	var cases []contrastCase
	for mode, vars := range modes {
		for _, bg := range []string{"--surface-0", "--surface-1", "--surface-2", "--surface-3"} {
			cases = append(cases, contrastCase{mode: mode, fg: "--text", bg: bg, minRatio: 7})
		}
		for _, bg := range []string{"--surface-0", "--surface-1", "--surface-2"} {
			cases = append(cases, contrastCase{mode: mode, fg: "--text-muted", bg: bg, minRatio: 4.5})
			cases = append(cases, contrastCase{mode: mode, fg: "--text-faint", bg: bg, minRatio: 3})
		}
		for _, fg := range []string{"--status-ok", "--status-warn", "--status-attention", "--status-error", "--status-info", "--status-neutral", "--brand", "--acmm-level-1", "--acmm-level-2", "--acmm-level-3", "--acmm-level-4", "--acmm-level-5", "--acmm-level-6"} {
			cases = append(cases, contrastCase{mode: mode, fg: fg, bg: "--surface-2", minRatio: 3})
		}
		cases = append(cases, contrastCase{mode: mode, fg: "--terminal-cyan", bg: "--surface-terminal", minRatio: 4.5})

		primary := parseRuleDeclarations(t, string(componentData), ".hv-btn.btn-primary")
		if bg, ok := primary["background"]; ok {
			if color, ok := primary["color"]; ok {
				vars["--contrast-btn-primary-bg"] = bg
				vars["--contrast-btn-primary-fg"] = color
				cases = append(cases, contrastCase{mode: mode, fg: "--contrast-btn-primary-fg", bg: "--contrast-btn-primary-bg", minRatio: 4.5, component: ".hv-btn.btn-primary"})
			}
		}
	}

	var rows []string
	for i := range cases {
		vars := modes[cases[i].mode]
		fg, bg := resolveTokenColor(t, vars, cases[i].fg), resolveTokenColor(t, vars, cases[i].bg)
		cases[i].ratio = contrastRatio(composite(fg, bg), bg)
		label := cases[i].component
		if label == "" {
			label = cases[i].fg + " on " + cases[i].bg
		}
		rows = append(rows, fmt.Sprintf("%-5s %-34s %5.2f >= %.1f", cases[i].mode, label, cases[i].ratio, cases[i].minRatio))
		if cases[i].ratio+1e-9 < cases[i].minRatio {
			t.Errorf("%s %s contrast %.2f < %.1f", cases[i].mode, label, cases[i].ratio, cases[i].minRatio)
		}
	}
	sort.Strings(rows)
	for _, row := range rows {
		t.Log(row)
	}
}

func parseCustomProperties(t *testing.T, css, selectorNeedle string) map[string]string {
	t.Helper()
	needle := selectorNeedle
	if selectorNeedle == ":root" {
		needle = "\n:root"
	}
	start := strings.Index(css, needle)
	if start < 0 {
		t.Fatalf("selector %q not found", selectorNeedle)
	}
	open := strings.Index(css[start:], "{")
	if open < 0 {
		t.Fatalf("selector %q has no declaration block", selectorNeedle)
	}
	bodyStart := start + open + 1
	depth := 1
	for i := bodyStart; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return customProperties(css[bodyStart:i])
			}
		}
	}
	t.Fatalf("selector %q declaration block was not closed", selectorNeedle)
	return nil
}

func customProperties(block string) map[string]string {
	props := map[string]string{}
	block = cssCommentRE.ReplaceAllString(block, "")
	for _, decl := range strings.Split(block, ";") {
		decl = strings.TrimSpace(decl)
		if !strings.HasPrefix(decl, "--") {
			continue
		}
		name, value, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		props[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return props
}

func mergeCustomProperties(base, overrides map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func parseRuleDeclarations(t *testing.T, css, selector string) map[string]string {
	t.Helper()
	idx := strings.Index(css, selector)
	if idx < 0 {
		t.Fatalf("component rule %q not found", selector)
	}
	open := strings.Index(css[idx:], "{")
	close := strings.Index(css[idx+open:], "}")
	if open < 0 || close < 0 {
		t.Fatalf("component rule %q is malformed", selector)
	}
	block := css[idx+open+1 : idx+open+close]
	decls := map[string]string{}
	for _, decl := range strings.Split(block, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(decl), ":")
		if ok {
			decls[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return decls
}

func resolveTokenColor(t *testing.T, vars map[string]string, token string) cssColor {
	t.Helper()
	return resolveColor(t, vars, token, map[string]bool{})
}

func resolveColor(t *testing.T, vars map[string]string, value string, seen map[string]bool) cssColor {
	t.Helper()
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "--") {
		if seen[value] {
			t.Fatalf("cyclic token reference resolving %s", value)
		}
		raw, ok := vars[value]
		if !ok {
			t.Fatalf("unresolvable color token %s: define it in tokens.css or use a plain color", value)
		}
		seen[value] = true
		return resolveColor(t, vars, raw, seen)
	}
	if strings.HasPrefix(value, "var(") {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "var("), ")"))
		return resolveColor(t, vars, name, seen)
	}
	if strings.HasPrefix(value, "#") {
		return parseHexColor(t, value)
	}
	if strings.HasPrefix(value, "rgba(") || strings.HasPrefix(value, "rgb(") {
		return parseRGBColor(t, value)
	}
	if strings.HasPrefix(value, "color-mix(") {
		return parseColorMix(t, vars, value, seen)
	}
	t.Fatalf("unresolvable color value %q: use a plain hex/rgb/rgba color or color-mix(in srgb, X%%, transparent)", value)
	return cssColor{}
}

func parseHexColor(t *testing.T, value string) cssColor {
	t.Helper()
	h := strings.TrimPrefix(value, "#")
	if len(h) == 3 || len(h) == 4 {
		var b strings.Builder
		for _, r := range h {
			b.WriteRune(r)
			b.WriteRune(r)
		}
		h = b.String()
	}
	if len(h) != 6 && len(h) != 8 {
		t.Fatalf("unresolvable hex color %q: use #rgb, #rgba, #rrggbb, or #rrggbbaa", value)
	}
	parse := func(part string) float64 {
		n, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			t.Fatalf("unresolvable hex color %q: %v", value, err)
		}
		return float64(n) / 255
	}
	c := cssColor{r: parse(h[0:2]), g: parse(h[2:4]), b: parse(h[4:6]), a: 1}
	if len(h) == 8 {
		c.a = parse(h[6:8])
	}
	return c
}

func parseRGBColor(t *testing.T, value string) cssColor {
	t.Helper()
	inner := value[strings.Index(value, "(")+1 : strings.LastIndex(value, ")")]
	parts := strings.Split(inner, ",")
	if len(parts) != 3 && len(parts) != 4 {
		t.Fatalf("unresolvable rgb color %q: expected rgb(r,g,b) or rgba(r,g,b,a)", value)
	}
	parse255 := func(part string) float64 {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			t.Fatalf("unresolvable rgb color %q: %v", value, err)
		}
		return n / 255
	}
	c := cssColor{r: parse255(parts[0]), g: parse255(parts[1]), b: parse255(parts[2]), a: 1}
	if len(parts) == 4 {
		a, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		if err != nil {
			t.Fatalf("unresolvable rgba alpha %q: %v", value, err)
		}
		c.a = a
	}
	return c
}

var colorMixRE = regexp.MustCompile(`^color-mix\(in srgb,\s*(.+?)\s+([0-9.]+%|var\(--[-\w]+\))\s*,\s*transparent\s*\)$`)

func parseColorMix(t *testing.T, vars map[string]string, value string, seen map[string]bool) cssColor {
	t.Helper()
	match := colorMixRE.FindStringSubmatch(value)
	if match == nil {
		t.Fatalf("unresolvable color-mix %q: use color-mix(in srgb, <color> <percent>, transparent)", value)
	}
	base := resolveColor(t, vars, strings.TrimSpace(match[1]), seen)
	percentText := match[2]
	if strings.HasPrefix(percentText, "var(") {
		percentText = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(percentText, "var("), ")"))
		raw, ok := vars[percentText]
		if !ok || !strings.HasSuffix(strings.TrimSpace(raw), "%") {
			t.Fatalf("unresolvable color-mix percentage %q: use a plain percent token", percentText)
		}
		percentText = strings.TrimSpace(raw)
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(percentText, "%"), 64)
	if err != nil {
		t.Fatalf("unresolvable color-mix percentage %q: %v", percentText, err)
	}
	base.a *= pct / 100
	return base
}

func composite(fg, bg cssColor) cssColor {
	alpha := fg.a + bg.a*(1-fg.a)
	if alpha == 0 {
		return cssColor{}
	}
	return cssColor{
		r: (fg.r*fg.a + bg.r*bg.a*(1-fg.a)) / alpha,
		g: (fg.g*fg.a + bg.g*bg.a*(1-fg.a)) / alpha,
		b: (fg.b*fg.a + bg.b*bg.a*(1-fg.a)) / alpha,
		a: alpha,
	}
}

func contrastRatio(fg, bg cssColor) float64 {
	l1, l2 := relativeLuminance(fg), relativeLuminance(bg)
	if l2 > l1 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

func relativeLuminance(c cssColor) float64 {
	linear := func(v float64) float64 {
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.r) + 0.7152*linear(c.g) + 0.0722*linear(c.b)
}
