package hub

import (
	"net/url"
	"strconv"
	"strings"
)

// Sanitizers for the spoke-reported forge of a hive's primary repo
// (HeartbeatPayload.ForgeKind/ForgeBaseURL/ForgeLogin, hivecommons/hive#20181).
//
// A Forgejo-only spoke has an empty github block, so its github_host is the
// github.com default and, before these fields, the hub drew it as a GitHub
// hive linked to a github.com repo that does not exist. The dashboard now
// builds the Location pill, the repo link and the "as:" line from these
// values whenever the kind is not GitHub. They are display-only: nothing on
// the hub routes, authenticates or classifies a hive by them.

// sanitizeHeartbeatForgeKind keeps a known forge kind, lower-cased, and drops
// anything else. An unknown kind is dropped rather than recorded so the row
// falls back to its GitHub rendering instead of naming a forge nobody
// configured.
func sanitizeHeartbeatForgeKind(kind string) string {
	switch k := ForgeKind(strings.ToLower(strings.TrimSpace(kind))); k {
	case ForgeGitHub, ForgeGitLab, ForgeGitea, ForgeForgejo:
		return string(k)
	}
	return ""
}

// isNonGitHubForgeKind reports whether a sanitized kind names a forge whose
// host is not carried by github_host.
func isNonGitHubForgeKind(kind string) bool {
	switch ForgeKind(kind) {
	case ForgeGitLab, ForgeGitea, ForgeForgejo:
		return true
	}
	return false
}

// sanitizeHeartbeatForgeBaseURL returns the instance root of a non-GitHub
// forge as scheme://host[:port][/path], or "" when the kind is GitHub or the
// value is not a plain http(s) URL with a valid host. Userinfo, query and
// fragment are never kept: the value becomes an href on the hub dashboard.
func sanitizeHeartbeatForgeBaseURL(kind, raw string) string {
	if !isNonGitHubForgeKind(sanitizeHeartbeatForgeKind(kind)) {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2*maxForgeHostLen {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if len(host) > maxForgeHostLen || !isValidForgeHostLabel(host) {
		return ""
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
		host += ":" + strconv.Itoa(n)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path != "" && sanitizeHeartbeatField(path) != path {
		return ""
	}
	return u.Scheme + "://" + host + path
}

// sanitizeHeartbeatForgeLogin returns the hive's user on a non-GitHub forge,
// restricted to the characters a forge username can hold, or "" for a GitHub
// hive, whose identity is AIAuthorEffective.
func sanitizeHeartbeatForgeLogin(kind, login string) string {
	if !isNonGitHubForgeKind(sanitizeHeartbeatForgeKind(kind)) {
		return ""
	}
	login = strings.ReplaceAll(sanitizeHeartbeatField(strings.TrimSpace(login)), "/", "")
	if len(login) > 100 {
		return ""
	}
	return login
}
