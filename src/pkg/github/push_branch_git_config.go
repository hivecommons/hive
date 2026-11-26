package github

import (
	"fmt"
	"strings"
)

// The push-branch relays (the GitHub watcher and, since #19939, the forge
// push relay in cmd/hive) run git inside a checkout the requesting agent owns
// and can rewrite at will. Git reads that checkout's .git/config (and any file
// it includes) before it sends the push, and several keys there decide WHERE
// the credentialed request goes (hivecommons/hive#19959):
//
//   - url.<base>.insteadOf / pushInsteadOf rewrite the explicit remote URL the
//     relay passes, and the longest match wins, so no command-scope value can
//     outrank an agent's rewrite of the same URL;
//   - http.<url>.* keys beat a plain http.* key whatever its scope, because
//     git ranks a more specific URL match above config precedence, so a
//     command-scope http.sslVerify=true loses to a local
//     http.https://github.com/.sslVerify=false;
//   - a remote section NAMED after the URL (remote."https://host/o/r.git")
//     is used by `git push <url>`, and its pushurl redirects the push;
//   - http.proxy, http.sslCAInfo and friends have no neutral command-scope
//     value: an empty http.proxy also disables the hive's own proxy
//     environment, and an empty http.sslCAInfo breaks TLS.
//
// pushBranchGitEnv pins what can be pinned (see there). For the rest the relay
// reads the repository-scoped config first and refuses the push when it sets
// any of the keys above.

// pushBranchConfigListArgs lists every config entry git will read, with the
// scope it came from and NUL-separated records so no key or value can forge a
// record boundary. Includes are followed, so an include.path cannot hide a key.
var pushBranchConfigListArgs = []string{"config", "--list", "-z", "--show-scope"}

// pushBranchConfigRefusalMaxKeys caps how many offending keys a refusal
// names, so an agent cannot inflate the result file with a huge config.
const pushBranchConfigRefusalMaxKeys = 5

// pushBranchSafeHTTPKeys are the plain http.* keys an agent checkout may
// carry. They tune the transport and cannot change the host, the proxy or the
// TLS trust the push uses; sslVerify and followRedirects are also pinned at
// command scope by pushBranchGitEnv, which outranks a plain local value.
var pushBranchSafeHTTPKeys = map[string]struct{}{
	"postbuffer":      {},
	"lowspeedlimit":   {},
	"lowspeedtime":    {},
	"maxrequests":     {},
	"minsessions":     {},
	"version":         {},
	"sslverify":       {},
	"followredirects": {},
}

// pushBranchConfigScopes are the scopes `git config --show-scope` prints.
// Only local and worktree are written by the agent: system and global are
// turned off by pushBranchGitEnv, and command scope is the relay's own.
var pushBranchConfigScopes = map[string]bool{
	"local":    true,
	"worktree": true,
	"system":   false,
	"global":   false,
	"command":  false,
}

// pushBranchConfigOffenders returns the repository-scoped keys in listing
// (the output of pushBranchConfigListArgs) that could redirect or intercept
// the relay's push. A record it cannot parse is an offender too: the listing
// is the only view of the checkout's config, so it fails closed. Values are
// never returned; only key names reach the refusal.
func pushBranchConfigOffenders(listing string) []string {
	var offenders []string
	fields := strings.Split(listing, "\x00")
	// A complete listing ends with a NUL, which leaves one empty trailing field.
	if n := len(fields); n > 0 && fields[n-1] == "" {
		fields = fields[:n-1]
	}
	if len(fields)%2 != 0 {
		return []string{"(unparseable git config listing)"}
	}
	for i := 0; i < len(fields); i += 2 {
		agentScope, known := pushBranchConfigScopes[fields[i]]
		// A key with no value (an implicit true) is printed without the
		// newline; -z already keeps it from forging a record.
		key, _, _ := strings.Cut(fields[i+1], "\n")
		if !known || key == "" {
			return []string{"(unparseable git config listing)"}
		}
		if agentScope && pushBranchConfigKeyHostile(strings.ToLower(key)) {
			offenders = append(offenders, key)
		}
	}
	return offenders
}

// pushBranchConfigKeyHostile reports whether a lowercased config key, set in
// the agent's checkout, could send the push somewhere other than the URL the
// relay chose or weaken how it is protected on the way there.
func pushBranchConfigKeyHostile(key string) bool {
	section, rest, _ := strings.Cut(key, ".")
	sub, name := "", rest
	if i := strings.LastIndex(rest, "."); i >= 0 {
		sub, name = rest[:i], rest[i+1:]
	}
	switch section {
	case "url":
		return name == "insteadof" || name == "pushinsteadof"
	case "http":
		if sub != "" {
			return true
		}
		_, safe := pushBranchSafeHTTPKeys[name]
		return !safe
	case "credential":
		// credential.helper is reset at command scope; a URL-scoped helper
		// would outrank that reset the same way http.<url>.* does.
		return sub != ""
	case "remote":
		// `git push <url>` uses a remote whose NAME is that URL, and a
		// remote's own proxy applies to any fetch git starts from it.
		return strings.ContainsAny(sub, ":/") || name == "proxy" || name == "proxyauthmethod"
	}
	return false
}

// pushBranchConfigRefusal is the reason the relay gives for refusing a push
// from a checkout whose config sets any offending key, or "" when the config
// is acceptable.
func pushBranchConfigRefusal(listing string) string {
	offenders := pushBranchConfigOffenders(listing)
	if len(offenders) == 0 {
		return ""
	}
	named := offenders
	more := ""
	if len(named) > pushBranchConfigRefusalMaxKeys {
		more = fmt.Sprintf(" and %d more", len(named)-pushBranchConfigRefusalMaxKeys)
		named = named[:pushBranchConfigRefusalMaxKeys]
	}
	return fmt.Sprintf("the checkout's git config sets %s%s, which can redirect or intercept the relay's credentialed push (url.*.insteadOf/pushInsteadOf, any http.* key other than transport tuning, URL-scoped http.<url>.* or credential.<url>.* keys, or a remote named by URL); remove them with `git config --unset` and request the push again", strings.Join(named, ", "), more)
}
