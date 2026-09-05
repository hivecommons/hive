package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// defaultTtydPort is where the container's ttyd process listens (see
// deploy/entrypoint.sh, which starts ttyd alongside this server). Overridable
// via HIVE_TTYD_PORT, mirroring the Node proxy's knob in proxy/server.js.
const defaultTtydPort = 7681

// terminalPathPrefix is the public path the dashboard's "▶ terminal" links
// point at (see terminalUrl() in static/index.html). It is stripped before
// forwarding because ttyd serves its UI and websocket at the root.
const terminalPathPrefix = "/terminal"

// ttydPort resolves the local ttyd port, falling back to the default on an
// unset or malformed HIVE_TTYD_PORT.
func ttydPort() int {
	if v := os.Getenv("HIVE_TTYD_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return defaultTtydPort
}

// registerTerminalProxy mounts /terminal on the Go dashboard mux, reverse
// proxying to the in-container ttyd (HTTP assets and the terminal
// websocket — httputil.ReverseProxy passes protocol upgrades through).
//
// Why the Go server must serve this itself: hosted spokes route the
// dashboard host's "/" straight to this server (port 3002), while the
// Ingress/Route that is supposed to peel off /terminal to the Node proxy
// (port 3001) does not exist on every spoke — clicking "▶ terminal" then
// landed on the static FileServer here and returned a bare Go
// "404 page not found" (task #39, heartbeat-only-cluster hosted spokes). Serving /terminal
// from this server makes the link work regardless of which Service port the
// cluster's route actually targets, and puts terminal access behind the same
// authenticate() middleware as the rest of the dashboard (session cookie, hub-injected identity, terminal assertion cookie,
// or a short-lived terminal handoff code minted by an authenticated dashboard
// request).
//
// Registered with an explicit GET method: everything ttyd serves (assets,
// auth token endpoint, websocket handshake) is a GET, and a method-less
// "/terminal/" pattern would conflict with Start()'s "GET /" under the Go
// 1.22 ServeMux precedence rules (overlapping, neither more specific).
func (s *Server) registerTerminalProxy() {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", ttydPort())}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Strip the /terminal prefix: ttyd serves at the root. Mirrors the
			// Node proxy's pathRewrite (p.replace(/^\/terminal/, '') || '/').
			p := strings.TrimPrefix(pr.In.URL.Path, terminalPathPrefix)
			if p == "" || !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			pr.Out.URL.Path = p
			pr.Out.URL.RawPath = ""
			q := pr.Out.URL.Query()
			q.Del("token")
			q.Del(terminalHandoffCodeParam)
			pr.Out.URL.RawQuery = q.Encode()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Error("terminal proxy error", "request", redactedRequestURI(r.URL), "error", err)
			// Same body the Node proxy returns when ttyd is down — a clear
			// signal, not a bare 404.
			http.Error(w, "Terminal unavailable", http.StatusBadGateway)
		},
	}
	s.mux.Handle("GET "+terminalPathPrefix, proxy)
	s.mux.Handle("GET "+terminalPathPrefix+"/", proxy)
}
