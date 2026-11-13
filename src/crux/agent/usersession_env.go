package agent

import (
	"sort"
	"strings"
)

// Session environment policy. A user session is a terminal handed to a person
// through the dashboard, so its environment is built from an ALLOWLIST of the
// hive process env rather than by subtracting known secrets: a credential the
// hive later learns to hold is excluded by default instead of leaking until
// someone remembers to add it to a strip list.
//
// The allowlist covers only what a CLI needs to run and reach the network.
// Vendor API keys are NOT inherited; an operator who wants one in sessions
// names it in HIVE_SESSIONS_ENV_PASSTHROUGH, and even then every name that is
// (or looks like) a hive, dashboard, hub, terminal or GitHub credential is
// refused (sessionEnvForbidden).
const (
	// userSessionEnvPassthroughEnv lists extra env var names (comma separated)
	// copied from the hive process into sessions.
	userSessionEnvPassthroughEnv = "HIVE_SESSIONS_ENV_PASSTHROUGH"
	// userSessionDefaultTerm is the TERM sessions run with; ttyd/xterm.js
	// speaks xterm-256color.
	userSessionDefaultTerm = "xterm-256color"
)

// userSessionEnvAllow is the set of hive-process variables copied verbatim.
var userSessionEnvAllow = map[string]bool{
	"PATH":                true,
	"LANG":                true,
	"LC_ALL":              true,
	"LC_CTYPE":            true,
	"TZ":                  true,
	"TMUX_TMPDIR":         true,
	"SSL_CERT_FILE":       true,
	"SSL_CERT_DIR":        true,
	"NODE_EXTRA_CA_CERTS": true,
	"REQUESTS_CA_BUNDLE":  true,
	"CURL_CA_BUNDLE":      true,
}

// userSessionProxyVars are copied only when their value carries no userinfo
// (a proxy URL's user:password is a credential).
var userSessionProxyVars = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
}

// sessionEnvForbiddenNames are refused even through the operator passthrough.
func sessionEnvForbiddenNames() map[string]bool {
	out := map[string]bool{
		"DASHBOARD_AUTH_TOKEN": true,
		"HIVE_DASHBOARD_TOKEN": true,
	}
	for _, k := range inheritedCredentialEnvVars {
		out[k] = true
	}
	for _, k := range githubTokenEnvVars {
		out[k] = true
	}
	return out
}

// sessionEnvForbidden reports whether name must never reach a session: a
// known hive credential, anything in the HIVE_ namespace (hub, heartbeat,
// terminal and ttyd secrets live there), or a GitHub token spelling.
func sessionEnvForbidden(name string) bool {
	if sessionEnvForbiddenNames()[name] {
		return true
	}
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "HIVE_") ||
		strings.HasPrefix(upper, "GITHUB_") ||
		strings.HasPrefix(upper, "GH_") ||
		strings.Contains(upper, "GITHUB_TOKEN")
}

// userSessionStripNames is the sorted list handed to `tmux set-environment -r`
// as a second line of defence: even if the tmux server somehow inherited one,
// the removal hides it from every process the session forks.
func userSessionStripNames() []string {
	names := sessionEnvForbiddenNames()
	out := make([]string, 0, len(names))
	for k := range names {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// userSessionBaseEnv builds the environment a session's tmux server starts
// with from the hive process environ (KEY=VALUE entries) and the operator's
// passthrough list.
func userSessionBaseEnv(environ []string, passthrough string) []string {
	extra := map[string]bool{}
	for _, n := range strings.Split(passthrough, ",") {
		n = strings.TrimSpace(n)
		if n != "" && !sessionEnvForbidden(n) {
			extra[n] = true
		}
	}
	var out []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		switch {
		case sessionEnvForbidden(k):
			continue
		case userSessionEnvAllow[k], extra[k]:
			out = append(out, k+"="+v)
		case userSessionProxyVars[k] && proxyValueHasNoUserinfo(v):
			out = append(out, k+"="+v)
		}
	}
	out = append(out, "TERM="+userSessionDefaultTerm)
	return out
}

// proxyValueHasNoUserinfo reports whether a proxy setting is safe to copy.
// NO_PROXY style lists have no "@" and pass. Anything else is judged on its
// authority component (after an optional scheme, up to the first path, query
// or fragment delimiter) so a schemeless "user:pw@host:port" is caught too;
// url.Parse would read "user" as a scheme and miss the credentials.
func proxyValueHasNoUserinfo(v string) bool {
	if !strings.Contains(v, "@") {
		return true
	}
	rest := v
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+len("://"):]
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	return !strings.Contains(rest, "@")
}
