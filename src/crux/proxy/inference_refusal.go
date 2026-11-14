package proxy

import (
	"net/http"
	"strings"
)

// internalTranslatorUID reports whether uid is the hive's own control plane
// for the translator's purposes: root always, and the proxy UID only when the
// proxy cannot relay to private addresses (see translatorAgentName).
func (p *GitHubProxy) internalTranslatorUID(uid int) bool {
	if !p.loadedUIDMap().IsInternalUID(uid) {
		return false
	}
	return uid == 0 || !p.allowPrivateForward
}

// sharedUIDClaimsIsolatedAgent reports whether a caller at the proxy UID is
// asserting the name of an agent that has its own per-agent UID. The proxy
// UID is shared by the hive process and every UID-less agent, so the header
// is the only thing distinguishing those callers from each other; an agent
// that owns a UID never connects from the proxy UID, so such a claim can only
// be another process borrowing that agent's inference route (#18115). Root is
// the control plane and is not subject to this check, and other unmapped UIDs
// keep the HIVE_PROXY_ADVISORY_OK semantics the GitHub path documents.
func (p *GitHubProxy) sharedUIDClaimsIsolatedAgent(uid int, claimed string) bool {
	return uid != 0 && uid == p.loadedUIDMap().ProxyUID && claimed != "" && p.lookupUIDByName(claimed) > 0
}

// translatorRefusalAttrs are the log attributes for a refused translator
// request. Beyond remote/method/path they say why identification failed (no UID
// map, socket not resolvable, proxy UID refused because private forwarding is
// enabled, proxy UID claiming a UID-isolated agent's name, or UID not mapped
// to an agent), the socket owner's UID when known, and whether an x-api-key
// was sent, so the refusal is actionable without weakening it. The key value
// is never logged; the claimed agent NAME is (it is public inside the pod).
func (p *GitHubProxy) translatorRefusalAttrs(r *http.Request) []any {
	attrs := []any{"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path}
	switch {
	case p.loadedUIDMap() == nil:
		attrs = append(attrs, "reason", "no_uid_map")
	default:
		uid, err := uidForRemoteAddr(r.RemoteAddr)
		claimed := strings.TrimPrefix(r.Header.Get("x-api-key"), inferenceAPIKeyPrefix)
		if err != nil {
			attrs = append(attrs, "reason", "socket_owner_unresolved", "error", err.Error())
		} else if p.loadedUIDMap().IsInternalUID(uid) && p.allowPrivateForward {
			attrs = append(attrs, "reason", "proxy_uid_private_forward", "uid", uid)
		} else if p.loadedUIDMap().IsInternalUID(uid) && p.sharedUIDClaimsIsolatedAgent(uid, claimed) {
			attrs = append(attrs, "reason", "proxy_uid_claims_isolated_agent", "uid", uid, "claimed", claimed)
		} else {
			attrs = append(attrs, "reason", "uid_not_mapped", "uid", uid)
		}
	}
	return append(attrs, "has_api_key", r.Header.Get("x-api-key") != "")
}
