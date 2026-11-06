package dashboard

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// publicKnowledgeLoopbackHost reports whether host (with or without a port)
// names this machine: localhost or a loopback IP.
func publicKnowledgeLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// publicKnowledgeBrowserOnHost reports whether the browser that sent r was
// itself on host, going by the page address in its Origin or Referer. A
// browser that reached the Go API directly names the same host; one that
// came through the Node proxy names the proxy's address instead.
func publicKnowledgeBrowserOnHost(r *http.Request, host string) bool {
	for _, h := range []string{"Origin", "Referer"} {
		if u, err := url.Parse(r.Header.Get(h)); err == nil && u.Host != "" {
			return strings.EqualFold(u.Host, host)
		}
	}
	return false
}
