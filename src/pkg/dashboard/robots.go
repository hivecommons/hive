package dashboard

import "net/http"

// A spoke dashboard is an operator tool, never a search-engine document: on a
// hosted hive the nginx auth_request gate bounces an anonymous crawler to the
// hub's /login (Search Console then reports the page as "blocked due to
// unauthorized request (401)"), and on a standalone spoke the device-flow login
// page answers 401 itself. Both the robots.txt and the response header below
// say the same thing so neither a stale robots cache nor a crawler that reads
// headers only can get a dashboard URL indexed.
//
// /robots.txt is served BEFORE authentication: the hub lists it in
// publicExactPaths so the hosted ingress forwards it here unauthenticated, and
// a Disallow: / that lives behind a login redirect is no robots.txt at all.
const (
	dashboardRobotsTxt      = "User-agent: *\nDisallow: /\n"
	xRobotsTagHeader        = "X-Robots-Tag"
	xRobotsTagNoIndex       = "noindex, nofollow"
	dashboardRobotsMetaTag  = `<meta name="robots" content="noindex, nofollow">`
	dashboardRobotsTxtRoute = "/robots.txt"
)

// noIndex is the outermost dashboard middleware: it answers /robots.txt with
// Disallow: / and stamps X-Robots-Tag: noindex, nofollow on every other
// response, including the 401 login page and the proxy-proof rejections that
// authenticate writes before any handler runs.
func (s *Server) noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == dashboardRobotsTxtRoute && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set(xRobotsTagHeader, xRobotsTagNoIndex)
			_, _ = w.Write([]byte(dashboardRobotsTxt))
			return
		}
		w.Header().Set(xRobotsTagHeader, xRobotsTagNoIndex)
		next.ServeHTTP(w, r)
	})
}
