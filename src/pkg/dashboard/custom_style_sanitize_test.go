package dashboard

import "testing"

// ============================================================
// sanitizeCSSURLToken — url() rewriting for XSS/data-exfil prevention
// ============================================================

func TestSanitizeCSSURLToken_DataImageAllowed(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"png", `url("data:image/png;base64,abc123")`, `url(data:image/png;base64,abc123)`},
		{"svg", `url('data:image/svg+xml,<svg></svg>')`, `url(data:image/svg+xml,<svg></svg>)`},
		{"gif", `url(data:image/gif;base64,R0lGODl)`, `url(data:image/gif;base64,R0lGODl)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCSSURLToken(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeCSSURLToken(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeCSSURLToken_BlocksExternalURLs(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"https", `url("https://evil.example/pixel.png")`},
		{"http", `url(http://evil.example/pixel.png)`},
		{"protocol_relative", `url(//evil.example/pixel.png)`},
		{"data_non_image", `url(data:text/html,<script>alert(1)</script>)`},
		{"ftp", `url(ftp://evil.example/x)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCSSURLToken(tc.input)
			if got != `url("")` {
				t.Errorf("sanitizeCSSURLToken(%q) = %q, want url(\"\")", tc.input, got)
			}
		})
	}
}

func TestSanitizeCSSURLToken_RelativePathsPreserved(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", `url(icons/check.svg)`, `url(icons/check.svg)`},
		{"quoted", `url("../images/bg.png")`, `url(../images/bg.png)`},
		{"hash_fragment", `url(#gradient)`, `url(#gradient)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCSSURLToken(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeCSSURLToken(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeCSSURLToken_MalformedInput(t *testing.T) {
	got := sanitizeCSSURLToken("not-a-url-token")
	if got != "" {
		t.Errorf("expected empty for malformed input, got %q", got)
	}
}

// ============================================================
// scopeCustomStyleSelectors — CSS selector confinement
// ============================================================

func TestScopeCustomStyleSelectors_PrefixesSelectors(t *testing.T) {
	root := "#hive-dashboard-root"
	tests := []struct {
		name     string
		selector string
		want     string
	}{
		{"class", ".my-class", root + " .my-class"},
		{"id", "#my-id", root + " #my-id"},
		{"tag", "div", root + " div"},
		{"multi", ".a, .b", root + " .a, " + root + " .b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scopeCustomStyleSelectors(tc.selector, root)
			if got != tc.want {
				t.Errorf("scopeCustomStyleSelectors(%q, %q) = %q, want %q", tc.selector, root, got, tc.want)
			}
		})
	}
}

func TestScopeCustomStyleSelectors_PassthroughRootSelector(t *testing.T) {
	root := "#hive-dashboard-root"
	tests := []struct {
		name     string
		selector string
	}{
		{"exact_root", root},
		{"root_space", root + " .child"},
		{"root_class", root + ".active"},
		{"root_pseudo", root + ":hover"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scopeCustomStyleSelectors(tc.selector, root)
			if got != tc.selector {
				t.Errorf("expected passthrough %q, got %q", tc.selector, got)
			}
		})
	}
}

func TestScopeCustomStyleSelectors_RewritesBodyHTMLRoot(t *testing.T) {
	root := "#hive-dashboard-root"
	tests := []struct {
		name     string
		selector string
		want     string
	}{
		{"body", "body", root},
		{"html", "html", root},
		{"root_pseudo", ":root", root},
		{"body_class", "body.dark-mode", root + ".dark-mode"},
		{"html_id", "html#app", root + "#app"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scopeCustomStyleSelectors(tc.selector, root)
			if got != tc.want {
				t.Errorf("scopeCustomStyleSelectors(%q, %q) = %q, want %q", tc.selector, root, got, tc.want)
			}
		})
	}
}

func TestScopeCustomStyleSelectors_LeaderboardScope(t *testing.T) {
	root := "#tab-leaderboard"
	got := scopeCustomStyleSelectors(".entry", root)
	want := root + " .entry"
	if got != want {
		t.Errorf("leaderboard scope: got %q, want %q", got, want)
	}
}

func TestScopeCustomStyleSelectors_EmptySelector(t *testing.T) {
	got := scopeCustomStyleSelectors("", "#hive-dashboard-root")
	if got != "" {
		t.Errorf("empty selector should produce empty, got %q", got)
	}
}

// ============================================================
// validateCustomStylePath — path traversal prevention
// ============================================================

func TestValidateCustomStylePath_ValidPaths(t *testing.T) {
	tests := []string{
		"style.css",
		"themes/dark.css",
		"a/b/c/deep.css",
		"my-theme_v2.css",
	}
	for _, path := range tests {
		if err := validateCustomStylePath(path); err != nil {
			t.Errorf("validateCustomStylePath(%q) unexpected error: %v", path, err)
		}
	}
}

func TestValidateCustomStylePath_BlocksTraversal(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"dotdot", "../etc/passwd.css"},
		{"mid_dotdot", "a/../b.css"},
		{"dot_segment", "a/./b.css"},
		{"leading_slash", "/absolute.css"},
		{"backslash", `a\b.css`},
		{"empty", ""},
		{"protocol", "https://evil.css"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCustomStylePath(tc.path); err == nil {
				t.Errorf("validateCustomStylePath(%q) should have returned error", tc.path)
			}
		})
	}
}

func TestValidateCustomStylePath_RequiresCSSExtension(t *testing.T) {
	tests := []string{
		"style.js",
		"style.html",
		"style",
		"style.CSS.bak",
	}
	for _, path := range tests {
		if err := validateCustomStylePath(path); err == nil {
			t.Errorf("validateCustomStylePath(%q) should reject non-.css path", path)
		}
	}
}

// ============================================================
// normalizeCustomStyleScope — scope validation
// ============================================================

func TestNormalizeCustomStyleScope_ValidScopes(t *testing.T) {
	tests := []struct {
		input     string
		wantScope string
		wantRoot  string
	}{
		{"dashboard", "dashboard", "#hive-dashboard-root"},
		{"leaderboard", "leaderboard", "#tab-leaderboard"},
		{"", "dashboard", "#hive-dashboard-root"}, // default
		{"  dashboard  ", "dashboard", "#hive-dashboard-root"},
	}
	for _, tc := range tests {
		scope, root, err := normalizeCustomStyleScope(tc.input)
		if err != nil {
			t.Errorf("normalizeCustomStyleScope(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if scope != tc.wantScope {
			t.Errorf("scope = %q, want %q", scope, tc.wantScope)
		}
		if root != tc.wantRoot {
			t.Errorf("root = %q, want %q", root, tc.wantRoot)
		}
	}
}

func TestNormalizeCustomStyleScope_RejectsInvalid(t *testing.T) {
	tests := []string{"admin", "global", "body", "*"}
	for _, scope := range tests {
		_, _, err := normalizeCustomStyleScope(scope)
		if err == nil {
			t.Errorf("normalizeCustomStyleScope(%q) should reject invalid scope", scope)
		}
	}
}

// ============================================================
// customStyleRawURL — URL construction correctness
// ============================================================

func TestCustomStyleRawURL(t *testing.T) {
	src := customStyleSource{
		Owner: "kubestellar",
		Repo:  "hive",
		Path:  "themes/dark.css",
		Ref:   "main",
	}
	got := customStyleRawURL(src)
	want := "https://raw.githubusercontent.com/hivecommons/hive/main/themes/dark.css"
	if got != want {
		t.Errorf("customStyleRawURL() = %q, want %q", got, want)
	}
}

func TestCustomStyleRawURL_HeadRef(t *testing.T) {
	src := customStyleSource{
		Owner: "org",
		Repo:  "repo",
		Path:  "style.css",
		Ref:   "HEAD",
	}
	got := customStyleRawURL(src)
	want := "https://raw.githubusercontent.com/org/repo/HEAD/style.css"
	if got != want {
		t.Errorf("customStyleRawURL() = %q, want %q", got, want)
	}
}

func TestCustomStyleRawURL_NestedPath(t *testing.T) {
	src := customStyleSource{
		Owner: "owner",
		Repo:  "repo",
		Path:  "a/b/c/deep.css",
		Ref:   "v2",
	}
	got := customStyleRawURL(src)
	want := "https://raw.githubusercontent.com/owner/repo/v2/a/b/c/deep.css"
	if got != want {
		t.Errorf("customStyleRawURL() = %q, want %q", got, want)
	}
}

// ============================================================
// customStyleCacheKey — key uniqueness
// ============================================================

func TestCustomStyleCacheKey_IncludesScope(t *testing.T) {
	src := customStyleSource{Owner: "o", Repo: "r", Path: "s.css", Ref: "HEAD"}
	k1 := customStyleCacheKey(src, "dashboard")
	k2 := customStyleCacheKey(src, "leaderboard")
	if k1 == k2 {
		t.Error("cache keys for different scopes must differ")
	}
}

func TestCustomStyleCacheKey_IncludesRef(t *testing.T) {
	src1 := customStyleSource{Owner: "o", Repo: "r", Path: "s.css", Ref: "main"}
	src2 := customStyleSource{Owner: "o", Repo: "r", Path: "s.css", Ref: "v2"}
	if customStyleCacheKey(src1, "dashboard") == customStyleCacheKey(src2, "dashboard") {
		t.Error("cache keys for different refs must differ")
	}
}
