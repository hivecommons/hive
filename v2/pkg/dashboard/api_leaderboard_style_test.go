package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dropReasonFor returns the reason recorded for the first dropped rule whose
// excerpt contains needle, or "" if none.
func dropReasonFor(dropped []droppedCSSRule, needle string) string {
	for _, d := range dropped {
		if strings.Contains(d.Rule, needle) {
			return d.Reason
		}
	}
	return ""
}

// TestSanitizeCustomPropertiesAndVarSurvive covers #2972: custom-property
// declarations and var() references must pass through, but a custom property
// whose value is an unsafe url() must still be neutralised.
func TestSanitizeCustomPropertiesAndVarSurvive(t *testing.T) {
	css := []byte(`
:root { --bf-accent: #5c7bd1; --bf-soft: rgba(92,123,209,.2); }
.me-card { border: 1px solid var(--bf-accent); color: var(--bf-soft); }
.evilvar { --leak: url(https://evil.example/x.png); }
`)
	got, dropped, err := sanitizeLeaderboardCustomStyleWithDropped(css)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, want := range []string{"--bf-accent: #5c7bd1", "var(--bf-accent)", "var(--bf-soft)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("custom property theming dropped, missing %q:\n%s", want, out)
		}
	}
	// The custom property is kept, but its unsafe url() target is emptied.
	if strings.Contains(out, "evil.example") {
		t.Fatalf("unsafe url() in custom property survived:\n%s", out)
	}
	if !strings.Contains(out, "--leak: url(\"\")") {
		t.Fatalf("expected neutralised --leak, got:\n%s", out)
	}
	_ = dropped
}

// TestSanitizeSafeAtRulesSurvive covers #2972: @media/@supports/@keyframes must
// survive with their inner declarations sanitized, and their inner rules scoped
// (for @media/@supports). @import stays rejected.
func TestSanitizeSafeAtRulesSurvive(t *testing.T) {
	css := []byte(`
@import "https://evil.example/x.css";
@media (prefers-color-scheme: light) { .me-row { color: #111; } }
@supports (display: grid) { .me-grid { display: grid; } }
@keyframes pulse { from { opacity: 0; } to { opacity: 1; } }
@font-face { font-family: x; src: url(https://evil.example/f.woff); }
`)
	got, dropped, err := sanitizeLeaderboardCustomStyleWithDropped(css)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, want := range []string{
		"@media (prefers-color-scheme: light)",
		"#tab-leaderboard .me-row{ color: #111; }",
		"@supports (display: grid)",
		"#tab-leaderboard .me-grid{ display: grid; }",
		"@keyframes pulse",
		"from{ opacity: 0; }",
		"to{ opacity: 1; }",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("safe at-rule content missing %q:\n%s", want, out)
		}
	}
	// @import and @font-face must be gone.
	if strings.Contains(out, "@import") || strings.Contains(out, "evil.example") {
		t.Fatalf("@import survived:\n%s", out)
	}
	if strings.Contains(out, "@font-face") {
		t.Fatalf("@font-face survived (network src exfil vector):\n%s", out)
	}
	// @font-face rejection must be REPORTED, not silent.
	if r := dropReasonFor(dropped, "@font-face"); r == "" {
		t.Fatalf("@font-face rejection not reported in dropped list: %+v", dropped)
	}
}

// TestSanitizeDangerousVectorsStillBlockedAndReported covers the security
// contract plus the anti-silent-drop guarantee: each dangerous vector is both
// removed AND surfaced with a reason.
func TestSanitizeDangerousVectorsStillBlockedAndReported(t *testing.T) {
	css := []byte(`
.expr { width: expression(alert(1)); color: red; }
.moz { -moz-binding: url(/x.xml); }
.beh { behavior: url(/x.htc); }
.iset { background-image: image-set("https://evil.example/p.png" 1x); }
.remote { background: url(https://evil.example/p.png); }
`)
	got, dropped, err := sanitizeLeaderboardCustomStyleWithDropped(css)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, bad := range []string{"expression(", "-moz-binding", "behavior:", "image-set(", "evil.example"} {
		if strings.Contains(out, bad) {
			t.Fatalf("dangerous token %q survived:\n%s", bad, out)
		}
	}
	// The .expr rule keeps its safe sibling declaration; only expression() drops.
	if !strings.Contains(out, "#tab-leaderboard .expr{color: red;}") {
		t.Fatalf("safe declaration in mixed rule was lost:\n%s", out)
	}
	// Each dangerous drop is reported.
	for needle, mustMention := range map[string]string{
		"expression":   "expression",
		"-moz-binding": "-moz-binding",
		"behavior":     "behavior",
		"image-set":    "exfil",
	} {
		if r := dropReasonFor(dropped, needle); !strings.Contains(strings.ToLower(r), mustMention) {
			t.Fatalf("drop for %q not reported with reason mentioning %q: %+v", needle, mustMention, dropped)
		}
	}
}

// TestSanitizeNestedAtRuleDoesNotCorruptFollowingRules is the specific bug from
// the report: the old string parser desynced on the first nested brace and
// corrupted every rule after an @media block.
func TestSanitizeNestedAtRuleDoesNotCorruptFollowingRules(t *testing.T) {
	css := []byte(`
@media (min-width: 600px) { .a { color: red; } }
.after { color: blue; }
`)
	got, err := sanitizeLeaderboardCustomStyle(css)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if !strings.Contains(out, "#tab-leaderboard .after{ color: blue; }") {
		t.Fatalf("rule after @media was corrupted/lost:\n%s", out)
	}
	if strings.Contains(out, "#tab-leaderboard }") {
		t.Fatalf("stray brace from parser desync:\n%s", out)
	}
}

// TestStyleHandlerReportsDroppedHeader covers the X-Style-Rules-Dropped header
// contract for curl-based authors (#2972 suggestion 1).
func TestStyleHandlerReportsDroppedHeader(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`.ok{color:red}.bad{width:expression(alert(1))}`))
	}))
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/style?src=owner/repo/lb/theme.css@main", nil)
	rec := httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Style-Rules-Dropped"); got != "1" {
		t.Fatalf("X-Style-Rules-Dropped = %q, want 1", got)
	}
	detail := rec.Header().Get("X-Style-Rules-Dropped-Detail")
	if !strings.Contains(detail, "expression") {
		t.Fatalf("dropped detail missing reason: %q", detail)
	}
}

// TestContributeNoticeListsDroppedRules covers the anti-silent-drop guarantee in
// the /contribute UI notice.
func TestContributeNoticeListsDroppedRules(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`.ok{color:red}@import "https://evil.example/x.css";.bad{behavior:url(/x.htc)}`))
	}))
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/contribute/leaderboard?style=owner/repo/lb/theme.css@main", nil)
	rec := httptest.NewRecorder()
	srv.handleContributeLanding(rec, req)
	html := rec.Body.String()
	if !strings.Contains(html, "removed by the sanitizer") {
		t.Fatalf("contribute notice does not report dropped rules:\n%s", html)
	}
	if !strings.Contains(html, "lb-custom-style-dropped") {
		t.Fatalf("contribute notice missing dropped list markup")
	}
}

func resetLeaderboardStyleTestState(t *testing.T) {
	t.Helper()
	origBase := customStyleRawBaseURL
	customStyleCacheMu.Lock()
	origCache := customStyleCache
	customStyleCache = map[string]customStyleCacheEntry{}
	customStyleCacheMu.Unlock()
	t.Cleanup(func() {
		customStyleRawBaseURL = origBase
		customStyleCacheMu.Lock()
		customStyleCache = origCache
		customStyleCacheMu.Unlock()
	})
}

func TestValidateLeaderboardCustomStyleSource(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"simple", "owner/repo/path/theme.css", false},
		{"with ref", "owner/repo/path/theme.css@main", false},
		{"full url rejected", "https://github.com/owner/repo/theme.css", true},
		{"protocol relative rejected", "//github.com/owner/repo/theme.css", true},
		{"path traversal rejected", "owner/repo/../theme.css", true},
		{"script path rejected", "owner/repo/</script><script>alert(1)</script>.css", true},
		{"non css rejected", "owner/repo/theme.txt", true},
		{"oversized ref rejected", "owner/repo/theme.css@" + strings.Repeat("a", 101), true},
		{"bad owner chars rejected", "bad_owner/repo/theme.css", true},
		{"leading owner dash rejected", "-owner/repo/theme.css", true},
		{"repo punctuation allowed", "owner/repo.name_theme/theme.css", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateLeaderboardCustomStyleSource(tt.src)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateLeaderboardCustomStyleSource(%q) error = %v, wantErr %v", tt.src, err, tt.wantErr)
			}
		})
	}
}

func TestSanitizeLeaderboardCustomStyle(t *testing.T) {
	css := []byte(`
@import url("https://evil.example/x.css");
@\69mport "https://evil.example/escaped.css";
.external{background:url(https://evil.example/pixel.png)}
.proto{background:url(//evil.example/pixel.png)}
.escaped{background:url(https\3a//evil.example/pixel.png)}
.multiline{background:url(
https://evil.example/wrap.png
)}
.imageset{background-image:image-set("https://evil.example/pixel.png" 1x)}
.scheme{background:url(javascript:alert(1))}
body{display:none}
.a,.b{color:blue}
#tab-leaderboard ~ footer{display:none}
.data{background:url(data:image/png;base64,abc)}
.relative{background:url(/assets/bg.png)}
.dot{background:url(./bg.png)}
.expr{width:expression(alert(1))}
.moz{-moz-binding:url(/x.xml)}
.behavior{behavior:url(/x.htc)}
`)
	got, err := sanitizeLeaderboardCustomStyle(css)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, bad := range []string{"@import", "mport", "evil.example", `\3a`, "image-set(", "javascript:", "expression(", "-moz-binding", "behavior:"} {
		if strings.Contains(out, bad) {
			t.Fatalf("sanitized CSS still contains %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{"url(data:image/png;base64,abc)", "url(/assets/bg.png)", "url(./bg.png)"} {
		if !strings.Contains(out, good) {
			t.Fatalf("sanitized CSS missing %q:\n%s", good, out)
		}
	}
	for _, scoped := range []string{"#tab-leaderboard body{display:none}", "#tab-leaderboard .a, #tab-leaderboard .b{color:blue}", "#tab-leaderboard #tab-leaderboard ~ footer{display:none}"} {
		if !strings.Contains(out, scoped) {
			t.Fatalf("sanitized CSS missing scoped selector %q:\n%s", scoped, out)
		}
	}
	if _, err := sanitizeLeaderboardCustomStyle([]byte(strings.Repeat("a", customStyleMaxBytes+1))); err == nil {
		t.Fatal("oversized CSS did not fail")
	}
}

func TestHandleLeaderboardStyleFetchesSanitizesAndCaches(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	hits := 0
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/owner/repo/main/lb/theme.css" {
			t.Fatalf("raw path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`.ok{color:red}.bad{background:url(https://evil.example/pixel.png)}`))
	}))
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/style?src=owner/repo/lb/theme.css@main", nil)
	rec := httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "#tab-leaderboard .ok{color:red}") || strings.Contains(body, "evil.example") {
		t.Fatalf("unexpected sanitized body: %s", body)
	}

	rec = httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached status = %d", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("raw hits = %d, want cache hit after first fetch", hits)
	}
}

func TestHandleLeaderboardStyle404(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	raw := httptest.NewServer(http.NotFoundHandler())
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/leaderboard/style?src=owner/repo/theme.css@main", nil)
	rec := httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestContributeLeaderboardCustomStyleMarkup(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`.lb-row{border-color:hotpink}`))
	}))
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/contribute/leaderboard?style=owner/repo/lb/theme.css@main", nil)
	rec := httptest.NewRecorder()
	srv.handleContributeLanding(rec, req)
	html := rec.Body.String()
	for _, snippet := range []string{
		`id="leaderboard-custom-style-link" rel="stylesheet" href="/api/leaderboard/style?src=owner%2Frepo%2Flb%2Ftheme.css%40main"`,
		`window.HIVE_LEADERBOARD_CUSTOM_STYLE_SRC="owner/repo/lb/theme.css@main";`,
		`Custom (` + `'+esc(leaderboardCustomStyleLabel(customStyleSrc))+'` + `)`,
		`clearLeaderboardCustomStyleParam();`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("contribute HTML missing %q", snippet)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/contribute?tab=leaderboard&style=owner/repo/missing.css@main", nil)
	rec = httptest.NewRecorder()
	missingRaw := httptest.NewServer(http.NotFoundHandler())
	defer missingRaw.Close()
	customStyleRawBaseURL = missingRaw.URL
	srv.handleContributeLanding(rec, req)
	if !strings.Contains(rec.Body.String(), "Custom style could not be loaded — using default") {
		t.Fatal("missing custom style fallback notice")
	}
}

func TestHandleDashboardStyleScopesToDashboardRoot(t *testing.T) {
	resetLeaderboardStyleTestState(t)
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`:root{--bg:#111}body{background:#111}.card{color:red}#hive-dashboard-root .ready{color:green}`))
	}))
	defer raw.Close()
	customStyleRawBaseURL = raw.URL

	srv := newFullServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/style?src=owner/repo/dashboard/theme.css@main&scope=dashboard", nil)
	rec := httptest.NewRecorder()
	srv.handleStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"#hive-dashboard-root{--bg:#111}", "#hive-dashboard-root{background:#111}", "#hive-dashboard-root .card{color:red}", "#hive-dashboard-root .ready{color:green}"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard CSS missing scoped selector %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "#tab-leaderboard") {
		t.Fatalf("dashboard CSS used leaderboard scope:\n%s", body)
	}
}

func TestHandleDashboardStyleRejectsInvalidScopeAndSource(t *testing.T) {
	srv := newFullServer(t)
	for _, path := range []string{
		"/api/style?src=https://evil.example/theme.css&scope=dashboard",
		"/api/style?src=owner/repo/theme.txt&scope=dashboard",
		"/api/style?src=owner/repo/theme.css&scope=login",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.handleStyle(rec, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d, want 422", path, rec.Code)
		}
	}
}

func TestDashboardCustomStyleMarkupAndPreviewSupport(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`<div id="hive-dashboard-root" class="hive-dashboard-root">`,
		`link.id='dashboard-custom-style-link';`,
		`/api/style?src=`,
		`&scope=`,
		`id="dashboard-custom-style-note"`,
		`Custom style active: `,
		`clearDashboardCustomStyleParam`,
		`window.hiveURLWithHash`,
		`window.hiveURLWithHash('#section-' + section)`,
		`window.hiveURLWithHash(name ? '#' + name : '')`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("dashboard HTML missing %q", snippet)
		}
	}
	if !isPublicPath("/api/style") {
		t.Fatal("/api/style must be public so /snapshot read-only previews can load sanitized CSS")
	}
}
