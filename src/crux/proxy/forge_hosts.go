package proxy

import "sync"

// forgeHosts are the hosts of the non-GitHub forges (Forgejo) the hive's
// repos live on. The proxy terminates TLS for them so the mode rules and the
// Gitea/Forgejo hard denies (rules_deny.go) apply to an agent's direct API
// calls instead of the traffic passing through an opaque tunnel
// (hivecommons/hive#19494).
//
// A forge host is deliberately NOT a GitHub host: injectsGitHubAuthForHost
// and the #1861 placeholder rewrite stay GitHub-only, so the proxy never
// attaches a GitHub token, or any token, to a request for a forge host. Only
// hosts the operator configured are registered (RegisterForgeHost from the
// config's forge URLs), so a GitHub-only hive registers none and its proxy
// behaves exactly as before.
var (
	forgeHostsMu sync.RWMutex
	forgeHosts   = map[string]bool{}
)

// RegisterForgeHost adds the hostname of a configured non-GitHub forge to the
// set the proxy inspects. A host already registered as a GitHub host, or the
// empty host, is ignored, so a forge entry can never widen GitHub handling or
// change it.
func RegisterForgeHost(host string) {
	host = canonicalHost(host)
	if host == "" || IsGitHubHost(host) {
		return
	}
	forgeHostsMu.Lock()
	forgeHosts[host] = true
	forgeHostsMu.Unlock()
}

// SetForgeHosts replaces the registered forge hosts with hosts (the boot-time
// registration from config).
func SetForgeHosts(hosts []string) {
	next := map[string]bool{}
	for _, h := range hosts {
		h = canonicalHost(h)
		if h == "" || IsGitHubHost(h) {
			continue
		}
		next[h] = true
	}
	forgeHostsMu.Lock()
	forgeHosts = next
	forgeHostsMu.Unlock()
}

// IsForgeHost reports whether host is a registered non-GitHub forge host.
func IsForgeHost(host string) bool {
	host = canonicalHost(host)
	if host == "" {
		return false
	}
	forgeHostsMu.RLock()
	defer forgeHostsMu.RUnlock()
	return forgeHosts[host]
}
