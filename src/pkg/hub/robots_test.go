package hub

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

func TestHubRobotsTxtPublicHostAllowsLandingAndDisallowsGatedGroups(t *testing.T) {
	s := &HubServer{}
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	req.Host = defaultHubCanonicalHost
	rec := httptest.NewRecorder()
	s.handleRobotsTxt(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "User-agent: *\nAllow: /\n") {
		t.Fatalf("robots.txt must open with a wildcard Allow: /, got:\n%s", body)
	}
	for _, p := range hubNoIndexPrefixes {
		if !strings.Contains(body, "\nDisallow: "+p+"\n") {
			t.Errorf("robots.txt missing Disallow: %s\n%s", p, body)
		}
	}
	for _, want := range []string{"Disallow: /api/", "Disallow: /metrics", "Disallow: /openapi.json", "Disallow: /login", "Disallow: /dashboard", "Disallow: /static/"} {
		if !strings.Contains(body, want) {
			t.Errorf("robots.txt missing %q", want)
		}
	}
	if !strings.Contains(body, "\nSitemap: https://hive.hivecommons.dev/sitemap.xml\n") {
		t.Errorf("robots.txt missing sitemap pointer:\n%s", body)
	}
	if strings.Contains(body, "Disallow: /\n") {
		t.Errorf("public hub must not disallow the whole site:\n%s", body)
	}
}

func TestHubRobotsTxtHostedHostDisallowsEverything(t *testing.T) {
	s := &HubServer{}
	for _, host := range []string{"abc123.hive.hivecommons.dev", "abc123.hive.hivecommons.dev:443", "hub.hive.svc.cluster.local", ""} {
		req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.handleRobotsTxt(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d, want 200", host, rec.Code)
		}
		if got := rec.Body.String(); got != robotsDisallowAll {
			t.Errorf("host %q: robots.txt = %q, want %q", host, got, robotsDisallowAll)
		}
	}
}

func TestIsHubPublicRequestHost(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.test")
	cases := map[string]bool{
		"hub.example.test":      true,
		"HUB.example.test:8443": true,
		"hub.example.test.":     true,
		"localhost:8080":        true,
		"127.0.0.1":             true,
		"hive.hivecommons.dev":  false,
		"t1.hub.example.test":   false,
		"":                      false,
	}
	for host, want := range cases {
		if got := isHubPublicRequestHost(host); got != want {
			t.Errorf("isHubPublicRequestHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestHubNoIndexPath(t *testing.T) {
	cases := map[string]bool{
		"/":                     false,
		"/learn":                false,
		"/contribute":           false,
		"/contribute/":          false,
		"/reading":              false,
		"/api":                  true,
		"/api/":                 true,
		"/api/hub/stats":        true,
		"/metrics":              true,
		"/metrics/":             true,
		"/metricsx":             false,
		"/openapi.json":         true,
		"/login":                true,
		"/login/github":         true,
		"/loginx":               false,
		"/logout":               true,
		"/auth/return":          true,
		"/oauth":                true,
		"/oauth/callback":       true,
		"/openrouter/callback":  true,
		"/dashboard":            true,
		"/access-denied":        true,
		"/error/503":            true,
		"/fleet":                true,
		"/my-hives":             true,
		"/gh-setup":             true,
		"/ws":                   true,
		"/static/app.js":        true,
		"/cncf-reference-arch…": false,
	}
	for path, want := range cases {
		if got := hubNoIndexPath(path); got != want {
			t.Errorf("hubNoIndexPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSearchEngineGuardStampsNoIndex pins the middleware-level X-Robots-Tag:
// present on 401, 403, any redirect into /login and every gated route group;
// absent on a public page answering 200 or redirecting elsewhere.
func TestSearchEngineGuardStampsNoIndex(t *testing.T) {
	type tc struct {
		name     string
		path     string
		handler  http.HandlerFunc
		wantTag  bool
		wantCode int
	}
	cases := []tc{
		{"public landing 200", "/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }, false, http.StatusOK},
		{"public page explicit 200", "/learn", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }, false, http.StatusOK},
		{"public 404", "/nope", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, false, http.StatusNotFound},
		{"public redirect elsewhere", "/my-hives-old", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/fleet", http.StatusMovedPermanently)
		}, false, http.StatusMovedPermanently},
		{"api 200", "/api/hub/stats", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }, true, http.StatusOK},
		{"metrics 200", "/metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("# HELP")) }, true, http.StatusOK},
		{"openapi 200", "/openapi.json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }, true, http.StatusOK},
		{"401 anywhere", "/something", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, true, http.StatusUnauthorized},
		{"403 anywhere", "/something", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, true, http.StatusForbidden},
		{"307 to /login", "/dashboard", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		}, true, http.StatusTemporaryRedirect},
		{"303 to /login?redirect=", "/somewhere", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/login?redirect=https%3A%2F%2Fx.hive.hivecommons.dev%2F", http.StatusSeeOther)
		}, true, http.StatusSeeOther},
		{"302 to absolute hub /login", "/somewhere", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://hive.hivecommons.dev/login/github", http.StatusFound)
		}, true, http.StatusFound},
		{"302 to absolute host root", "/somewhere", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://hive.hivecommons.dev", http.StatusFound)
		}, false, http.StatusFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			searchEngineGuard(c.handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantCode)
			}
			got := rec.Header().Get(xRobotsTagHeader)
			if c.wantTag && got != xRobotsTagNoIndex {
				t.Fatalf("X-Robots-Tag = %q, want %q", got, xRobotsTagNoIndex)
			}
			if !c.wantTag && got != "" {
				t.Fatalf("X-Robots-Tag = %q, want unset", got)
			}
		})
	}
}

func TestRobotsTagWriterFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	var unwrapped http.ResponseWriter
	h := searchEngineGuard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		unwrapped = w.(interface{ Unwrap() http.ResponseWriter }).Unwrap()
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil))
	if !rec.Flushed {
		t.Fatal("Flush did not reach the underlying writer")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (implicit on Flush)", rec.Code)
	}
	if unwrapped != rec {
		t.Fatal("Unwrap must return the underlying ResponseWriter for http.ResponseController")
	}
	if got := rec.Header().Get(xRobotsTagHeader); got != xRobotsTagNoIndex {
		t.Fatalf("X-Robots-Tag = %q on an /api/ stream, want %q", got, xRobotsTagNoIndex)
	}
}

// TestHubHandlerEndToEndNoIndex drives the real mux through Handler(): the
// anonymous answers for the gated routes carry the tag, the landing does not,
// and robots.txt depends on the request host.
func TestHubHandlerEndToEndNoIndex(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "test-client")
	srv := NewHubServer(0, slog.Default(), "test", "v5")
	t.Cleanup(srv.StopSaveLoop)
	h := srv.Handler()

	get := func(path, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		req.Header.Set("User-Agent", "Mozilla/5.0 (test)")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("/", defaultHubCanonicalHost); rec.Code != http.StatusOK || rec.Header().Get(xRobotsTagHeader) != "" {
		t.Fatalf("GET / = %d, X-Robots-Tag %q; want 200 and no tag", rec.Code, rec.Header().Get(xRobotsTagHeader))
	}
	if rec := get("/learn", defaultHubCanonicalHost); rec.Code != http.StatusOK || rec.Header().Get(xRobotsTagHeader) != "" {
		t.Fatalf("GET /learn = %d, X-Robots-Tag %q; want 200 and no tag", rec.Code, rec.Header().Get(xRobotsTagHeader))
	}
	for _, path := range []string{"/api/saas/my-hives", "/api/hub/stats", "/dashboard", "/login", "/api/hub/compliance"} {
		rec := get(path, defaultHubCanonicalHost)
		if rec.Header().Get(xRobotsTagHeader) != xRobotsTagNoIndex {
			t.Errorf("GET %s = %d without X-Robots-Tag", path, rec.Code)
		}
	}
	if rec := get("/dashboard", defaultHubCanonicalHost); rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous /dashboard = %d → %q, want 307 → /login", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get("/api/saas/my-hives", defaultHubCanonicalHost); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/saas/my-hives = %d, want 401", rec.Code)
	}
	if rec := get("/robots.txt", defaultHubCanonicalHost); !strings.Contains(rec.Body.String(), "Disallow: /api/") || strings.Contains(rec.Body.String(), "Disallow: /\n") {
		t.Fatalf("public robots.txt:\n%s", rec.Body.String())
	}
	if rec := get("/robots.txt", "tenant.hive.hivecommons.dev"); rec.Body.String() != robotsDisallowAll {
		t.Fatalf("hosted robots.txt = %q", rec.Body.String())
	}
}

func TestHubLoginPagesCarryNoIndexMeta(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLinkPreview(rec)
	if !strings.Contains(rec.Body.String(), robotsMetaNoIndex) {
		t.Fatalf("link-preview card (served to Googlebot on /login) lacks %s", robotsMetaNoIndex)
	}
	rec = httptest.NewRecorder()
	(&HubServer{}).writeProviderPicker(rec, []*auth.Provider{
		{Name: "github", DisplayName: "GitHub"},
		{Name: "google", DisplayName: "Google"},
	}, "")
	if !strings.Contains(rec.Body.String(), robotsMetaNoIndex) {
		t.Fatalf("login provider picker lacks %s", robotsMetaNoIndex)
	}
}

func TestHubSitemapListsOnlyPublicPages(t *testing.T) {
	b, err := staticFS.ReadFile("static/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(b, &set); err != nil {
		t.Fatalf("sitemap.xml does not parse: %v", err)
	}
	if len(set.URLs) == 0 {
		t.Fatal("sitemap.xml lists no URLs")
	}
	indexable := map[string]bool{}
	for _, p := range hubIndexablePaths {
		indexable[p] = true
	}
	for _, u := range set.URLs {
		prefix := defaultHubPublicURL
		if !strings.HasPrefix(u.Loc, prefix) {
			t.Errorf("sitemap loc %q is not under %s", u.Loc, prefix)
			continue
		}
		path := strings.TrimPrefix(u.Loc, prefix)
		if path == "" {
			path = "/"
		}
		if !indexable[path] {
			t.Errorf("sitemap lists %q, which is not in hubIndexablePaths %v", path, hubIndexablePaths)
		}
		if hubNoIndexPath(path) {
			t.Errorf("sitemap lists %q, which robots.txt disallows", path)
		}
	}
	for _, p := range hubIndexablePaths {
		if hubNoIndexPath(p) {
			t.Errorf("hubIndexablePaths entry %q is also disallowed", p)
		}
	}
}

// TestHubRobotsCoversEveryAuthGatedRoute derives the gated route set from the
// mux registrations in this package's source — every HandleFunc whose handler
// is wrapped in requireAuth/requireAdmin/requireAuthOrSpoke*, plus the pages
// that redirect anonymous callers to /login — and asserts each one falls under
// a hubNoIndexPrefixes entry. A new gated top-level route group therefore
// cannot ship without a matching Disallow line.
func TestHubRobotsCoversEveryAuthGatedRoute(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	consts := map[string]string{}
	constRe := regexp.MustCompile(`(?m)^\s*(\w+)\s*=\s*"(/[^"]*)"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		sources = append(sources, src)
		for _, m := range constRe.FindAllStringSubmatch(src, -1) {
			consts[m[1]] = m[2]
		}
	}
	// Gated registration: HandleFunc("METHOD <path>"|"METHOD "+const, s.require…(
	gatedRe := regexp.MustCompile(`mux\.HandleFunc\("(?:GET|POST|PUT|PATCH|DELETE) (?:(/[^"]*)"|"\+(\w+)),\s*s\.require(?:Auth|Admin|AuthOrSpoke\w*)\(`)
	var gated []string
	for _, src := range sources {
		for _, m := range gatedRe.FindAllStringSubmatch(src, -1) {
			switch {
			case m[1] != "":
				gated = append(gated, m[1])
			case m[2] != "":
				p, ok := consts[m[2]]
				if !ok {
					t.Fatalf("gated route uses constant %s whose value could not be resolved from package sources", m[2])
				}
				gated = append(gated, p)
			}
		}
	}
	// Pages that gate by redirecting an anonymous browser to /login rather than
	// via a require* wrapper.
	gated = append(gated, "/dashboard", "/login", "/access-denied")
	if len(gated) < 20 {
		t.Fatalf("found only %d gated routes in package sources; the registration regex has drifted:\n%v", len(gated), gated)
	}
	for _, p := range gated {
		if !hubNoIndexPath(p) {
			t.Errorf("auth-gated route %q is not covered by hubNoIndexPrefixes %v — add its top-level prefix", p, hubNoIndexPrefixes)
		}
	}
	// And the inverse sanity check: nothing the sitemap advertises is gated.
	for _, p := range hubIndexablePaths {
		for _, g := range gated {
			if g == p {
				t.Errorf("%q is both indexable and auth-gated", p)
			}
		}
	}
}
