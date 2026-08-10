package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
[data-theme="light"] ~ footer{display:none}
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
	for _, scoped := range []string{"#tab-leaderboard{display:none}", "#tab-leaderboard .a, #tab-leaderboard .b{color:blue}"} {
		if !strings.Contains(out, scoped) {
			t.Fatalf("sanitized CSS missing scoped selector %q:\n%s", scoped, out)
		}
	}
	if strings.Contains(out, "footer") {
		t.Fatalf("sanitized CSS allowed a selector outside the leaderboard scope:\n%s", out)
	}
	if _, err := sanitizeLeaderboardCustomStyle([]byte(strings.Repeat("a", customStyleMaxBytes+1))); err == nil {
		t.Fatal("oversized CSS did not fail")
	}
}

func TestSanitizeLeaderboardCustomStylePreservesModernThemeCSS(t *testing.T) {
	css := []byte(`
	/* Header comments should not eat the next rule. */
	:root{
	  --bf-accent:#5c7bd1;
	  --bf-soft:color-mix(in srgb, var(--bf-accent) 20%, transparent);
	}
	[data-theme="light"] .me-card{background:linear-gradient(180deg,#fff,#eef);border-color:var(--bf-accent)}
	.me-card:is(.featured,.mine) .me-card__band{padding:clamp(12px,2vw,24px);width:calc(100% - 2rem)}
	.lb-row[data-rank="1"]::before{content:"★"}
	@media (prefers-reduced-motion:no-preference){
	  .me-medallion{animation:pulse 1s ease-in-out}
	}
	@supports (color: color-mix(in srgb, red, blue)){
	  .me-rankpill__num{color:var(--bf-accent)}
	}
	@container (min-width: 40rem){
	  .me-card .me-sec__title{letter-spacing:.04em}
	}
	@keyframes pulse{
	  from{opacity:.7;transform:scale(.98)}
	  to{opacity:1;transform:scale(1)}
	}
	`)
	got, report, err := sanitizeCustomStyle(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
	if err != nil {
		t.Fatal(err)
	}
	if report.Dropped != 0 {
		t.Fatalf("report.Dropped = %d reasons=%v", report.Dropped, report.Reasons)
	}
	out := string(got)
	for _, want := range []string{
		"#tab-leaderboard{--bf-accent:#5c7bd1;--bf-soft:color-mix(in srgb, var(--bf-accent) 20%, transparent)}",
		`[data-theme="light"] #tab-leaderboard .me-card{background:linear-gradient(180deg,#fff,#eef);border-color:var(--bf-accent)}`,
		"#tab-leaderboard .me-card:is(.featured,.mine) .me-card__band{padding:clamp(12px,2vw,24px);width:calc(100% - 2rem)}",
		`#tab-leaderboard .lb-row[data-rank="1"]::before{content:"★"}`,
		"@media (prefers-reduced-motion:no-preference){#tab-leaderboard .me-medallion{animation:pulse 1s ease-in-out}",
		"@supports (color: color-mix(in srgb, red, blue)){#tab-leaderboard .me-rankpill__num{color:var(--bf-accent)}",
		"@container (min-width: 40rem){#tab-leaderboard .me-card .me-sec__title{letter-spacing:.04em}",
		"@keyframes pulse{from{opacity:.7;transform:scale(.98)}to{opacity:1;transform:scale(1)}}",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sanitized CSS missing %q:\n%s", want, out)
		}
	}
}

func TestSanitizeLeaderboardCustomStyleReportsDroppedConstructs(t *testing.T) {
	css := []byte(`
	@import url("https://evil.example/theme.css");
	@font-face{font-family:Bad;src:url(https://evil.example/font.woff2)}
	@font-face{font-family:Good;src:url(/assets/good.woff2),url(data:font/woff2;base64,abc)}
	.bad{background:url(https://evil.example/pixel.png);color:red}
	.foo\31{color:red}
	@keyframes badstep{fr\6fm{opacity:1}to{opacity:0}}
	@\69mport "https://evil.example/escaped.css";
	`)
	got, report, err := sanitizeCustomStyle(css, customStyleAllowedScopes[customStyleScopeLeaderboard])
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, bad := range []string{"@import", "evil.example", `\69`, `\31`, `\6f`} {
		if strings.Contains(out, bad) {
			t.Fatalf("sanitized CSS still contains %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{
		"@font-face{font-family:Good;src:url(/assets/good.woff2),url(data:font/woff2;base64,abc)}",
		`#tab-leaderboard .bad{background:url("");color:red}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sanitized CSS missing %q:\n%s", want, out)
		}
	}
	if report.Dropped < 6 {
		t.Fatalf("report.Dropped = %d, want at least 6 reasons=%v", report.Dropped, report.Reasons)
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
	if got := rec.Header().Get("X-Hive-Style-Dropped"); got != "1" {
		t.Fatalf("X-Hive-Style-Dropped = %q, want 1", got)
	}

	rec = httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cached status = %d", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("raw hits = %d, want cache hit after first fetch", hits)
	}

	reportReq := httptest.NewRequest(http.MethodGet, "/api/leaderboard/style?src=owner/repo/lb/theme.css@main&report=1", nil)
	rec = httptest.NewRecorder()
	srv.handleLeaderboardStyle(rec, reportReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || !strings.Contains(rec.Body.String(), `"dropped":1`) {
		t.Fatalf("unexpected report response headers=%v body=%s", rec.Header(), rec.Body.String())
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
		`rules removed by sanitizer`,
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
