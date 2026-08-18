package config

import (
	"os"
	"strings"
)

// ProxyInjectGHAuthEnv is the opt-in switch for proxy-side GitHub credential
// injection (#1861): when "true", the hub keeps every agent's tier-scoped
// GitHub App token to itself and the MITM proxy attaches it to each proxied
// GitHub request, while the agent-visible token cache receives a clearly-fake
// placeholder instead (see github.AgentDummyToken). The attack this closes:
// a prompt-injected agent exfiltrating its OWN credential — today the scoped
// token sits in the agent-readable cache / gh credential env, so any agent
// that can be talked into printing it hands out a usable token. With
// injection on, nothing an agent holds authenticates anywhere.
//
// Default OFF: with the flag unset the token delivery and proxy behavior are
// byte-identical to before this flag existed. It must stay opt-in until the
// injection path has soaked on a real spoke; the follow-up that removes
// agent-side token delivery entirely (and closes #1861) flips it.
const ProxyInjectGHAuthEnv = "HIVE_PROXY_INJECT_GH_AUTH"

// proxyInjectGHAuthEnabledValue is the only value that enables injection —
// the same strict "true" match HIVE_PROXY_ADVISORY_OK uses, so a typo fails
// safe (injection stays off and behavior stays exactly as today).
const proxyInjectGHAuthEnabledValue = "true"

// ProxyInjectGHAuth reports whether proxy-side GitHub credential injection
// (#1861) is enabled for this process. Read live from the environment so the
// token-minting path (pkg/github) and any test can gate on it without extra
// plumbing; the proxy itself snapshots it once at construction, mirroring how
// it treats HIVE_PROXY_ADVISORY_OK (a boot-time deployment choice).
func ProxyInjectGHAuth() bool {
	return strings.TrimSpace(os.Getenv(ProxyInjectGHAuthEnv)) == proxyInjectGHAuthEnabledValue
}
