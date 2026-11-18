package proxy

import (
	"net/http"
	"strings"
)

// stripAgentHopIdentityHeaders removes, from a request an agent sent, every
// header that asserts something about the hop or the caller rather than about
// the request itself (#18993):
//
//   - Proxy-Authorization and Proxy-Connection address THIS proxy. The identity
//     they carry is the one the proxy already decided whether to believe, and
//     in the Basic form the password field is whatever the agent put there.
//   - X-Forwarded-* and Forwarded describe the client a reverse proxy saw. An
//     origin that trusts them (rate limiting, audit, allowlists) would record
//     the address or scheme the agent chose instead of the real one.
//   - X-Hive-* is the hive's own trust namespace: the dashboard reads
//     X-Hive-User and X-Hive-Role as set by its auth layer. An agent request
//     carrying them is an attempt to speak with the hive's voice, and no hop
//     past this proxy has any business receiving one from an agent.
//
// Names are matched on their canonical form so a hand-written lower-case
// header (`x-hive-role:`) is removed exactly as the canonical one is.
func stripAgentHopIdentityHeaders(h http.Header) {
	for name := range h {
		if agentHopIdentityHeader(name) {
			delete(h, name)
		}
	}
}

// agentHopIdentityHeader reports whether a header name is one
// stripAgentHopIdentityHeaders removes.
func agentHopIdentityHeader(name string) bool {
	canon := http.CanonicalHeaderKey(name)
	switch canon {
	case "Proxy-Authorization", "Proxy-Connection", "Forwarded":
		return true
	}
	return strings.HasPrefix(canon, "X-Forwarded-") || strings.HasPrefix(canon, "X-Hive-")
}
