// Command authproxy is the streamer's local credential injector
// (src/deploy/stream, hivecommons/hive#15594).
//
// Chromium cannot attach an Authorization header to a kiosk URL, and putting
// the token in the URL would paint it on the stream. So the browser talks to
// this proxy on loopback and the proxy forwards to the dashboard origin with
// `Authorization: Bearer $DASHBOARD_TOKEN` added. The token never reaches the
// renderer, the URL bar, or a screenshot. Stdlib only; one binary, one flag.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultListen        = "127.0.0.1:8099"
	dialTimeout          = 10 * time.Second
	responseHeaderWindow = 60 * time.Second
	idleConnWindow       = 90 * time.Second
	// Hop-by-hop Upgrade requests (ttyd websockets) are passed straight
	// through by httputil; FlushInterval -1 keeps SSE (/api/events) flowing.
	sseFlushInterval = -1
)

func main() {
	listen := flag.String("listen", envOr("AUTHPROXY_LISTEN", defaultListen), "loopback address to serve the browser on")
	target := flag.String("target", os.Getenv("DASHBOARD_URL"), "dashboard origin, e.g. https://hive.example.com (path is ignored)")
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("DASHBOARD_TOKEN"))
	rp, err := newProxy(*target, token)
	if err != nil {
		log.Fatalf("authproxy: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/-/authproxy/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("/", rp)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: dialTimeout}
	log.Printf("authproxy: %s -> %s (token %s)", *listen, redactURL(*target), yesNo(token != ""))
	log.Fatal(srv.ListenAndServe())
}

// newProxy builds the reverse proxy. It is split from main so a unit test can
// assert the header is injected and the Host rewritten without a listener.
func newProxy(target, token string) (*httputil.ReverseProxy, error) {
	if target == "" {
		return nil, fmt.Errorf("DASHBOARD_URL (or -target) is required")
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("DASHBOARD_URL %q must be an absolute http(s) origin", redactURL(target))
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(origin)
			pr.Out.Host = origin.Host
			// Never forward the loopback client's own headers as if they
			// were the user's — this proxy IS the client.
			pr.Out.Header.Del("X-Forwarded-For")
			if token != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+token)
			}
		},
		FlushInterval: sseFlushInterval,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			ResponseHeaderTimeout: responseHeaderWindow,
			IdleConnTimeout:       idleConnWindow,
			ForceAttemptHTTP2:     true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("authproxy: upstream error for %s: %v", r.URL.Path, err)
			http.Error(w, "dashboard unreachable", http.StatusBadGateway)
		},
	}
	// Cookies the dashboard sets are scoped to the loopback origin by the
	// browser, which is what we want; nothing else needs rewriting.
	rp.ModifyResponse = func(resp *http.Response) error {
		// A hub login redirect means the token was rejected: surface it in the
		// logs instead of letting Chromium bounce to github.com forever.
		if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && strings.Contains(loc, "/login") {
			log.Printf("authproxy: upstream redirected %s to login (%s) — DASHBOARD_TOKEN rejected or missing", resp.Request.URL.Path, redactURL(loc))
		}
		return nil
	}
	return rp, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func yesNo(b bool) string {
	if b {
		return "set"
	}
	return "UNSET"
}

// redactURL drops the query so a token someone put in DASHBOARD_URL by
// mistake does not end up in the logs.
func redactURL(s string) string {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i] + "?…"
	}
	return s
}
