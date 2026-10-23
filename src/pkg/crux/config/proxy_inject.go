package config

import (
	"os"
	"strings"
)

// ProxyInjectGHAuthEnv is the switch for proxy-side GitHub credential
// injection (#1861): when "true", the hub keeps every agent's tier-scoped
// GitHub App token to itself and the MITM proxy attaches it to each proxied
// GitHub request, while the agent-visible token cache receives a clearly-fake
// placeholder instead (see github.AgentDummyToken). The attack this closes:
// a prompt-injected agent exfiltrating its OWN credential - without injection
// the scoped token sits in the agent-readable cache / gh credential env, so any
// agent that can be talked into printing it hands out a usable token. With
// injection on, nothing an agent holds authenticates anywhere.
//
// Injection is OPT-IN only: it is on ONLY when this variable is explicitly
// ProxyInjectGHAuthOnValue. Unset (or any other value) is off everywhere -
// hosted or self-hosted, App or PAT, spoke or hub - and the hub renders no
// value for newly provisioned spokes. A brief default-on for hosted App spokes
// (#9597/#9625) was reverted before any spoke ran it (#9586).
const ProxyInjectGHAuthEnv = "HIVE_PROXY_INJECT_GH_AUTH"

const (
	ProxyInjectGHAuthOnValue = "true"
)

// proxyInjectGHAuthEnabledValue is kept as the reader's comparand so the
// strict-match semantics pinned by pkg/github's parity test stay in one place.
const proxyInjectGHAuthEnabledValue = ProxyInjectGHAuthOnValue

// ProxyInjectGHAuth reports whether proxy-side GitHub credential injection
// (#1861) is enabled for this process. Read live from the environment so the
// token-minting path (pkg/github) and any test can gate on it without extra
// plumbing; the proxy itself snapshots it once at construction, mirroring how
// it treats HIVE_PROXY_ADVISORY_OK (a boot-time deployment choice).
func ProxyInjectGHAuth() bool {
	return strings.TrimSpace(os.Getenv(ProxyInjectGHAuthEnv)) == proxyInjectGHAuthEnabledValue
}
