package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Request-target hygiene for the parsed agent relays (#19223, egress audit
// section 4).
//
// Framing ambiguity (CL+TE, duplicate or malformed Content-Length, unknown
// Transfer-Encoding, folded headers, bare LF) is settled by net/http before a
// request reaches any gate: ReadRequest refuses what it cannot frame and
// req.Write regenerates one framing for the next hop, so the gates and the
// upstream always agree on where a request ends. relay_smuggling_test.go pins
// that, per relay.
//
// What net/http does NOT settle is which resource a request names, and two
// spellings let the gates and the upstream disagree:
//
//   - A Host header or absolute-form URL naming a host other than the one the
//     TLS session was forged for. The rule engine (GitHub vs Linear) and the
//     credential were chosen for the CONNECT host; a request naming another
//     host must not ride that decision.
//   - A dot segment ("." or "..", in any percent-encoded spelling) in the path.
//     The gates match the literal path, but an upstream that resolves dot
//     segments acts on a different one: /repos/allowed/r/../../paused/r/merge
//     is gated as allowed/r and merged as paused/r. No legitimate client sends
//     one, so it is refused rather than resolved.
//
// Both refusals are fail-closed: a target that cannot be decoded is refused.

// relayPathHasDotSegment reports whether an escaped request path contains a
// "." or ".." segment in any percent-encoded spelling, or a segment that
// cannot be decoded at all.
func relayPathHasDotSegment(escapedPath string) bool {
	for _, seg := range strings.Split(escapedPath, "/") {
		decoded, err := url.PathUnescape(seg)
		if err != nil {
			return true
		}
		// A decoded segment can still carry an encoded slash ("..%2F"): split
		// again so each piece the upstream might see is checked.
		for _, piece := range strings.Split(decoded, "/") {
			if piece == "." || piece == ".." {
				return true
			}
		}
	}
	return false
}

// relayRequestTargetRefusal reports why a request parsed off a terminated TLS
// session for host must not be forwarded. An empty host is the GitHub-implied
// proxyHTTP form, which names no CONNECT target to compare against, so only
// the path check applies there.
func relayRequestTargetRefusal(req *http.Request, host string) (string, bool) {
	if req.URL == nil {
		return "the request target could not be parsed", true
	}
	if host != "" && !sandboxReadRequestTargetsHost(req, host) {
		return "the request names a different host than the CONNECT target", true
	}
	if relayPathHasDotSegment(req.URL.EscapedPath()) {
		return "the request path contains a dot segment", true
	}
	return "", false
}

// writeRelayBadRequest answers a refused request target. The connection is
// closed after it, so nothing pipelined behind the request is read.
func writeRelayBadRequest(w net.Conn, reason string) error {
	body := "blocked by hive: " + reason + "\n"
	_, err := fmt.Fprintf(w, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\nX-Hive-Proxy-Blocked: true\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	return err
}
