package proxy

import (
	"net/http"
	"strings"
)

// stripRelayHopHeaders removes metadata scoped to the inbound connection.
// The request/response writers generate framing for the outbound connection.
func stripRelayHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range [...]string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		delete(h, name)
	}
}

func stripRelayRequestHeaders(req *http.Request) {
	stripRelayHopHeaders(req.Header)
	for name := range req.Header {
		if strings.HasPrefix(name, "X-Forwarded-") || strings.HasPrefix(name, "X-Hive-") {
			delete(req.Header, name)
		}
	}
	// Trailers arrive after the headers have been filtered; do not forward them.
	req.Trailer = nil
}

func stripRelayResponseHeaders(resp *http.Response) {
	stripRelayHopHeaders(resp.Header)
	resp.Trailer = nil
}
