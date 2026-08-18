package dashboard

// Coverage tests for the CSS sanitizer's low-coverage internal helpers:
// sanitizeCSSURLToken, scopeRootEscapesWithSiblingCombinator,
// findDeclarationColon, sanitizeCustomAtRule, and fontFaceSrcAllowed.
// These exercise edge cases (quoted strings, bracket/paren nesting,
// escaped identifiers, unterminated blocks) that the higher-level
// integration tests do not reach directly.

import (
	"strings"
	"testing"
)

func TestSanitizeCSSURLToken_Standalone(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"relative path allowed", `url(/assets/bg.png)`, `url(/assets/bg.png)`},
		{"dot-relative allowed", `url(./bg.png)`, `url(./bg.png)`},
		{"data URI allowed", `url(data:image/png;base64,abc)`, `url(data:image/png;base64,abc)`},
		{"external https blocked", `url(https://evil.example/pixel.png)`, `url("")`},
		{"protocol-relative blocked", `url(//evil.example/pixel.png)`, `url("")`},
		{"javascript blocked", `url(javascript:alert(1))`, `url("")`},
		{"quoted relative allowed", `url("/assets/bg.png")`, `url(/assets/bg.png)`},
		{"single-quoted allowed", `url('/assets/bg.png')`, `url(/assets/bg.png)`},
		{"empty url returns empty", `url()`, ``},
		{"non-url prefix still matched by regex", `notaurl(x)`, `url(x)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeCSSURLToken(tt.token)
			if got != tt.want {
				t.Errorf("sanitizeCSSURLToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

func TestScopeRootEscapesWithSiblingCombinator(t *testing.T) {
	root := "#tab-leaderboard"
	tests := []struct {
		name     string
		selector string
		want     bool
	}{
		{"no sibling", "#tab-leaderboard .card", false},
		{"direct sibling +", "#tab-leaderboard + footer", true},
		{"general sibling ~", "#tab-leaderboard ~ footer", true},
		{"sibling after space", "#tab-leaderboard ~ footer", true},
		{"sibling inside parens is safe", "#tab-leaderboard:is(.a + .b)", false},
		{"sibling inside brackets is safe", `#tab-leaderboard[data="a~b"]`, false},
		{"plus inside quoted string is safe", `#tab-leaderboard:is(".a+b")`, false},
		{"child combinator exits scope before sibling", "#tab-leaderboard > .x ~ footer", false},
		{"no prefix match returns false", ".other ~ footer", false},
		{"exact root no continuation", "#tab-leaderboard", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scopeRootEscapesWithSiblingCombinator(tt.selector, root)
			if got != tt.want {
				t.Errorf("scopeRootEscapesWithSiblingCombinator(%q, %q) = %v, want %v",
					tt.selector, root, got, tt.want)
			}
		})
	}
}

func TestFindDeclarationColon(t *testing.T) {
	tests := []struct {
		name string
		decl string
		want int
	}{
		{"simple property", "color:red", 5},
		{"colon inside parens skipped", "background:url(http://x:80/a)", 10},
		{"colon inside double quotes skipped", `content:"a:b"`, 7},
		{"colon inside single quotes skipped", `content:'a:b'`, 7},
		{"nested parens", "grid:repeat(2, minmax(0:1fr))", 4},
		{"no colon", "novalue", -1},
		{"colon after closing paren", "fn(a:b):red", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findDeclarationColon(tt.decl)
			if got != tt.want {
				t.Errorf("findDeclarationColon(%q) = %d, want %d", tt.decl, got, tt.want)
			}
		})
	}
}

func TestSanitizeCustomAtRule_EscapedBlockSkipped(t *testing.T) {
	// An escaped at-rule with a block body must skip the entire block and
	// not output anything — the escape makes the at-rule name untrustworthy.
	css := `@\69mport{.x{color:red}} .after{color:blue}`
	report := &customStyleSanitizeReport{}
	next, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("escaped at-rule with block: got output %q, want empty", out)
	}
	if report.Dropped != 1 {
		t.Errorf("report.Dropped = %d, want 1", report.Dropped)
	}
	// next should be past the closing brace of the block
	if next <= len(`@\69mport`) {
		t.Errorf("next = %d, should advance past the block body", next)
	}
}

func TestSanitizeCustomAtRule_UnterminatedHeader(t *testing.T) {
	// An at-rule header with no { or ; terminator
	css := `@media screen`
	report := &customStyleSanitizeReport{}
	next, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("unterminated at-rule: got output %q, want empty", out)
	}
	if next != len(css) {
		t.Errorf("next = %d, want %d (end of string)", next, len(css))
	}
	if report.Dropped != 1 || !strings.Contains(report.Reasons[0], "unterminated") {
		t.Errorf("report = %+v, want unterminated reason", report)
	}
}

func TestSanitizeCustomAtRule_SemicolonTerminated(t *testing.T) {
	// A statement at-rule terminated by semicolon (e.g. @charset, @namespace)
	css := `@charset "utf-8"; .rest{color:red}`
	report := &customStyleSanitizeReport{}
	next, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("semicolon at-rule: got output %q, want empty", out)
	}
	if next != 17 { // index of character after ';'
		t.Errorf("next = %d, want 17", next)
	}
	if report.Dropped != 1 {
		t.Errorf("report.Dropped = %d, want 1", report.Dropped)
	}
}

func TestSanitizeCustomAtRule_EmptyMediaBody(t *testing.T) {
	css := `@media screen{}`
	report := &customStyleSanitizeReport{}
	next, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("empty @media: got output %q, want empty", out)
	}
	if next != len(css) {
		t.Errorf("next = %d, want %d", next, len(css))
	}
	if report.Dropped < 1 {
		t.Errorf("report.Dropped = %d, want ≥1", report.Dropped)
	}
}

func TestSanitizeCustomAtRule_FontFaceExternalSrcBlocked(t *testing.T) {
	css := `@font-face{font-family:Evil;src:url(https://evil.example/font.woff2)}`
	report := &customStyleSanitizeReport{}
	_, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("@font-face with external src: got %q, want empty", out)
	}
	found := false
	for _, r := range report.Reasons {
		if strings.Contains(r, "font-face") {
			found = true
		}
	}
	if !found {
		t.Errorf("report reasons %v missing font-face reference", report.Reasons)
	}
}

func TestSanitizeCustomAtRule_FontFaceLocalSrcAllowed(t *testing.T) {
	css := `@font-face{font-family:Good;src:url(/assets/good.woff2)}`
	report := &customStyleSanitizeReport{}
	_, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if !strings.Contains(out, "font-family:Good") {
		t.Errorf("@font-face with local src should pass, got %q", out)
	}
}

func TestSanitizeCustomAtRule_EmptyKeyframes(t *testing.T) {
	css := `@keyframes spin{}`
	report := &customStyleSanitizeReport{}
	_, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("empty @keyframes: got %q, want empty", out)
	}
}

func TestSanitizeCustomAtRule_UnknownAtRule(t *testing.T) {
	css := `@layer base{.x{color:red}}`
	report := &customStyleSanitizeReport{}
	_, out := sanitizeCustomAtRule(css, 0, "#tab-leaderboard", report)
	if out != "" {
		t.Errorf("unknown @layer: got %q, want empty", out)
	}
	if report.Dropped != 1 {
		t.Errorf("report.Dropped = %d, want 1", report.Dropped)
	}
}

func TestFontFaceSrcAllowed_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"no src decl is allowed", "font-family:Good;font-weight:bold", true},
		{"local path allowed", "src:url(/fonts/x.woff2)", true},
		{"data URI allowed", "src:url(data:font/woff2;base64,abc)", true},
		{"external https blocked", "src:url(https://evil.example/font.woff2)", false},
		{"protocol-relative blocked", "src:url(//evil.example/font.woff2)", false},
		{"mixed: one external blocks all", "src:url(/good.woff2),url(https://evil.example/bad.woff2)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fontFaceSrcAllowed(tt.body)
			if got != tt.want {
				t.Errorf("fontFaceSrcAllowed(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestScopeRootEscapesWithSiblingCombinator_QuotedStrings(t *testing.T) {
	root := "#tab-leaderboard"
	// A single-quoted string containing ~ should NOT escape
	sel := `#tab-leaderboard:is('.a~b') .card`
	if scopeRootEscapesWithSiblingCombinator(sel, root) {
		t.Errorf("single-quoted ~ falsely detected as sibling combinator")
	}
	// Double-quoted string containing +
	sel = `#tab-leaderboard[attr="a+b"] .card`
	if scopeRootEscapesWithSiblingCombinator(sel, root) {
		t.Errorf("bracket attr containing + falsely detected as sibling combinator")
	}
}

func TestNextNonSpaceIsSiblingCombinator(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		i        int
		want     bool
	}{
		{"plus after spaces", "  + footer", 0, true},
		{"tilde after spaces", "  ~ footer", 0, true},
		{"descendant not sibling", "  .child", 0, false},
		{"empty tail", "", 0, false},
		{"only spaces", "   ", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextNonSpaceIsSiblingCombinator(tt.selector, tt.i)
			if got != tt.want {
				t.Errorf("nextNonSpaceIsSiblingCombinator(%q, %d) = %v, want %v",
					tt.selector, tt.i, got, tt.want)
			}
		})
	}
}

// Integration: a full CSS string exercising multiple gaps in one pass.
func TestSanitizeCustomStyle_SiblingCombinatorBlocked(t *testing.T) {
	css := []byte(`#tab-leaderboard ~ footer{display:none}`)
	got, report, err := sanitizeCustomStyle(css, "#tab-leaderboard")
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if strings.Contains(out, "footer") {
		t.Fatalf("sibling combinator selector should be blocked, got: %s", out)
	}
	if report.Dropped < 1 {
		t.Fatalf("report.Dropped = %d, want ≥1", report.Dropped)
	}
}

func TestSanitizeCustomStyle_UnterminatedAtRuleBlock(t *testing.T) {
	css := []byte(`@media screen{.card{color:red}`)
	got, report, err := sanitizeCustomStyle(css, "#tab-leaderboard")
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	// Unterminated block should be dropped
	if strings.Contains(out, "color:red") {
		t.Fatalf("unterminated @media body should be dropped, got: %s", out)
	}
	if report.Dropped < 1 {
		t.Fatalf("report.Dropped = %d, want ≥1", report.Dropped)
	}
}
