package hub

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// Search-engine indexing policy for the hub (#search-console 401/403 errors).
//
// Google Search Console reported hub pages as "Blocked due to unauthorized
// request (401)" and "Blocked due to access forbidden (403)": robots.txt said
// `Allow: /`, so crawlers walked /api/*, /login, /dashboard and every other
// auth-gated route, each of which answers 401/403 or bounces to /login for an
// anonymous caller. The hub now tells crawlers which route groups are not
// content (robots.txt), and says so again on the wire (X-Robots-Tag) for any
// response that proves it — a 401, a 403, or a redirect into the login flow —
// so a stale or ignored robots.txt still cannot get a gated page indexed.

// hubIndexablePaths are the ONLY hub documents a search engine is invited to
// index: the public landing and its read-only companion pages. The sitemap
// may list a subset of these and nothing else (see TestHubSitemapListsOnlyPublicPages).
var hubIndexablePaths = []string{"/", "/learn", "/get-started", "/reading", "/contribute"}

// hubNoIndexPrefixes are route groups a search engine must neither crawl nor
// index. Each entry is a path prefix; a trailing slash means "the tree under
// it" while a bare name also matches the exact path and any child.
//
// The list is the single source of truth for both robots.txt (Disallow lines)
// and the X-Robots-Tag middleware. TestHubRobotsCoversEveryAuthGatedRoute
// walks the mux registrations in this package's source and fails when a
// requireAuth/requireAdmin-wrapped route is not covered, so adding a new gated
// top-level route group without listing it here breaks CI rather than
// silently leaking into an index.
var hubNoIndexPrefixes = []string{
	"/api/",
	"/metrics",
	"/openapi.json",
	"/login",
	"/logout",
	"/auth/",
	"/oauth",
	"/openrouter/",
	"/dashboard",
	"/access-denied",
	"/error/",
	"/fleet",
	"/my-hives",
	"/gh-setup",
	"/ws",
	"/static/",
}

const (
	// xRobotsTagHeader is the HTTP equivalent of <meta name="robots">; unlike
	// the meta tag it also applies to JSON, redirects and error bodies.
	xRobotsTagHeader = "X-Robots-Tag"
	// xRobotsTagNoIndex is set on every response a search engine must not
	// index: gated route groups, 401/403 answers and login redirects.
	xRobotsTagNoIndex = "noindex, nofollow"
	// robotsMetaNoIndex is the in-document twin of xRobotsTagNoIndex for the
	// HTML pages in the login flow, which a crawler may fetch via a redirect
	// chain whose headers it has already discarded.
	robotsMetaNoIndex = `<meta name="robots" content="noindex, nofollow">`
	// robotsDisallowAll is what every hub host OTHER than the configured public
	// hub serves: a hosted tenant, an internal service name, a preview host.
	robotsDisallowAll = "User-agent: *\nDisallow: /\n"
)

// hubNoIndexPath reports whether path belongs to a route group that must never
// be indexed, regardless of how the handler answers.
func hubNoIndexPath(path string) bool {
	for _, p := range hubNoIndexPrefixes {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) || path == strings.TrimSuffix(p, "/") {
				return true
			}
			continue
		}
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// hubPublicRobotsTxt renders robots.txt for the public hub host: the landing
// pages stay crawlable, every gated or machine-only route group is disallowed,
// and the sitemap pointer survives because the sitemap lists public pages only.
func hubPublicRobotsTxt() string {
	prefixes := append([]string(nil), hubNoIndexPrefixes...)
	sort.Strings(prefixes)
	var b strings.Builder
	b.WriteString("User-agent: *\nAllow: /\n")
	for _, p := range prefixes {
		b.WriteString("Disallow: ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("\nSitemap: ")
	b.WriteString(hubPublicURL())
	b.WriteString("/sitemap.xml\n")
	return b.String()
}

// isHubPublicRequestHost reports whether the request reached the hub on its
// configured public host (HIVE_HUB_PUBLIC_URL, default hive.hivecommons.dev) or
// a local development host. Anything else — a hosted tenant's host, an
// internal Service name, a preview hostname — is private and gets Disallow: /.
func isHubPublicRequestHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.TrimSuffix(h, ".")
	return h == hubCanonicalHost() || h == "localhost" || h == "127.0.0.1"
}

// handleRobotsTxt serves robots.txt. The public hub host gets the allow-list
// plus Disallow lines derived from hubNoIndexPrefixes; every other host gets
// Disallow: /. The response is deliberately uncached-by-ETag plain text: it is
// tiny and must reflect the running binary's route policy immediately.
func (s *HubServer) handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	body := robotsDisallowAll
	if isHubPublicRequestHost(r.Host) {
		body = hubPublicRobotsTxt()
	}
	_, _ = w.Write([]byte(body))
}

// Handler is the hub's full HTTP handler: the mux wrapped in the search-engine
// guard. Start serves this; tests that need the X-Robots-Tag behaviour go
// through it rather than s.mux.
func (s *HubServer) Handler() http.Handler {
	return searchEngineGuard(s.mux)
}

// searchEngineGuard stamps X-Robots-Tag: noindex, nofollow on every response
// that a search engine must not index: anything under a hubNoIndexPrefixes
// route group, every 401 and 403, and every redirect whose target is the
// login flow. Public pages answering 200 are left untouched so the landing
// pages stay indexable.
func searchEngineGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&robotsTagWriter{ResponseWriter: w, path: r.URL.Path}, r)
	})
}

// robotsTagWriter decides at WriteHeader time — when the status and Location
// are both known — whether the response needs X-Robots-Tag, and sets it before
// the headers are flushed. It unwraps for http.ResponseController so the
// contribute WebSocket reverse proxy can still hijack the connection.
type robotsTagWriter struct {
	http.ResponseWriter
	path        string
	wroteHeader bool
}

func (w *robotsTagWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		if hubNoIndexResponse(w.path, code, w.Header().Get("Location")) {
			w.Header().Set(xRobotsTagHeader, xRobotsTagNoIndex)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *robotsTagWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *robotsTagWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *robotsTagWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// hubNoIndexResponse is the per-response decision behind searchEngineGuard.
func hubNoIndexResponse(path string, code int, location string) bool {
	if hubNoIndexPath(path) {
		return true
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	if code >= 300 && code < 400 && location != "" {
		loc := location
		if i := strings.Index(loc, "://"); i >= 0 {
			loc = loc[i+3:]
			if j := strings.IndexByte(loc, '/'); j >= 0 {
				loc = loc[j:]
			} else {
				loc = "/"
			}
		}
		if loc == hubLoginPath || strings.HasPrefix(loc, hubLoginPath+"/") || strings.HasPrefix(loc, hubLoginPath+"?") {
			return true
		}
	}
	return false
}
