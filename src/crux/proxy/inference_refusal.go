package proxy

import "net/http"

// translatorRefusalAttrs are the log attributes for a refused translator
// request. Beyond remote/method/path they say why identification failed (no UID
// map, socket not resolvable, or UID not mapped to an agent), the socket owner's
// UID when known, and whether an x-api-key was sent, so the refusal is
// actionable without weakening it. The key value is never logged.
func (p *GitHubProxy) translatorRefusalAttrs(r *http.Request) []any {
	attrs := []any{"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path}
	switch {
	case p.uidMap == nil:
		attrs = append(attrs, "reason", "no_uid_map")
	default:
		uid, err := uidForRemoteAddr(r.RemoteAddr)
		if err != nil {
			attrs = append(attrs, "reason", "socket_owner_unresolved", "error", err.Error())
		} else {
			attrs = append(attrs, "reason", "uid_not_mapped", "uid", uid)
		}
	}
	return append(attrs, "has_api_key", r.Header.Get("x-api-key") != "")
}
