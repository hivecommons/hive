package config

import (
	"errors"
	"fmt"
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
// Process default OFF: with the flag unset the token delivery and proxy
// behavior are byte-identical to before this flag existed, so existing spokes
// and self-hosted installs are unchanged. Newly provisioned hosted spokes are
// born with the flag set explicitly (#9586): the hub's provisioning template
// renders "true" for App-authenticated spokes and the explicit opt-out value
// ProxyInjectGHAuthOffValue otherwise (see pkg/hub provisionProxyInjectGHAuth).
const ProxyInjectGHAuthEnv = "HIVE_PROXY_INJECT_GH_AUTH"

// ProxyInjectGHAuthOnValue is the only value that enables injection - the same
// strict "true" match HIVE_PROXY_ADVISORY_OK uses. ProxyInjectGHAuthOffValue is
// the explicit opt-out: it behaves exactly like unset, but says so on the pod
// spec, so a spoke that deliberately runs without injection is visibly a
// decision rather than an omission (#9586).
const (
	ProxyInjectGHAuthOnValue  = "true"
	ProxyInjectGHAuthOffValue = "false"
)

// proxyInjectGHAuthEnabledValue is kept as the reader's comparand so the
// strict-match semantics pinned by pkg/github's parity test stay in one place.
const proxyInjectGHAuthEnabledValue = ProxyInjectGHAuthOnValue

// ProxyAdvisoryOKEnv is the operator escape hatch for a degraded forced-egress
// gate (entrypoint.sh) that ALSO makes the proxy trust the caller-controlled
// Proxy-Authorization header as agent identity whenever UID identification
// fails (pkg/proxy fallbackAgentName, N7 #3841). Named here so the startup
// guard below reads the exact variable the proxy reads.
const ProxyAdvisoryOKEnv = "HIVE_PROXY_ADVISORY_OK"

// proxyAdvisoryOKEnabledValue mirrors the proxy's strict "true" comparison.
const proxyAdvisoryOKEnabledValue = "true"

// ErrProxyInjectGHAuthInvalidValue is returned by ValidateProxyInjectGHAuth
// when HIVE_PROXY_INJECT_GH_AUTH holds anything other than unset, "true" or
// "false".
var ErrProxyInjectGHAuthInvalidValue = errors.New("unrecognized " + ProxyInjectGHAuthEnv + " value")

// ErrProxyInjectGHAuthWithSelfAssertedIdentity is returned by
// ValidateProxyInjectGHAuth when injection is enabled together with
// HIVE_PROXY_ADVISORY_OK=true.
var ErrProxyInjectGHAuthWithSelfAssertedIdentity = errors.New(ProxyInjectGHAuthEnv + "=true cannot be combined with " + ProxyAdvisoryOKEnv + "=true")

// ProxyInjectGHAuth reports whether proxy-side GitHub credential injection
// (#1861) is enabled for this process. Read live from the environment so the
// token-minting path (pkg/github) and any test can gate on it without extra
// plumbing; the proxy itself snapshots it once at construction, mirroring how
// it treats HIVE_PROXY_ADVISORY_OK (a boot-time deployment choice).
func ProxyInjectGHAuth() bool {
	return strings.TrimSpace(os.Getenv(ProxyInjectGHAuthEnv)) == proxyInjectGHAuthEnabledValue
}

// ValidateProxyInjectGHAuth is the spoke startup guard for the credential
// posture (#9586). The readers above fail SAFE on a bad value in the narrow
// sense that injection stays off - but "off" means the agent's REAL scoped
// token is written to its readable cache file, which is exactly the posture an
// operator setting the flag was trying to leave. A silent fallback to
// in-process token delivery is the failure mode this guard exists to prevent,
// so each contradictory combination refuses to start with a message naming
// the fix instead:
//
//   - An unrecognized value ("1", "TRUE", "yes", "on", a typo). The operator
//     plainly meant something, the reader would treat it as off, and the agent
//     would hold its real token while the pod spec claims otherwise. Only
//     unset/empty, ProxyInjectGHAuthOnValue and ProxyInjectGHAuthOffValue are
//     accepted.
//
//   - Injection ON together with HIVE_PROXY_ADVISORY_OK=true. Advisory mode
//     makes the proxy accept a self-asserted Proxy-Authorization agent name
//     whenever UID identification fails (no UID map, or an unmapped UID). Under
//     injection that name selects whose hub-held token is attached, so any
//     process that reaches the proxy could claim a more privileged agent and
//     have ITS real token injected - injection would turn an identity spoof
//     into a credential grant (the exact escalation fallbackAgentName's doc
//     warns about). The two settings cannot both hold their promise.
//
// getenv is injected (os.Getenv in production) so the boot phase and its tests
// share one implementation.
func ValidateProxyInjectGHAuth(getenv func(string) string) error {
	raw := strings.TrimSpace(getenv(ProxyInjectGHAuthEnv))
	switch raw {
	case "", ProxyInjectGHAuthOffValue:
		return nil
	case ProxyInjectGHAuthOnValue:
	default:
		return fmt.Errorf("%w %q: set %s=%s to enable proxy-side GitHub credential injection or %s=%s to opt out explicitly (any other value would silently leave the agent's real token in its readable cache)",
			ErrProxyInjectGHAuthInvalidValue, raw,
			ProxyInjectGHAuthEnv, ProxyInjectGHAuthOnValue,
			ProxyInjectGHAuthEnv, ProxyInjectGHAuthOffValue)
	}
	if strings.TrimSpace(getenv(ProxyAdvisoryOKEnv)) == proxyAdvisoryOKEnabledValue {
		return fmt.Errorf("%w: advisory mode lets the proxy trust a self-asserted Proxy-Authorization agent name, so a caller could claim another agent and receive that agent's injected token; unset %s (restore forced egress) or set %s=%s",
			ErrProxyInjectGHAuthWithSelfAssertedIdentity,
			ProxyAdvisoryOKEnv, ProxyInjectGHAuthEnv, ProxyInjectGHAuthOffValue)
	}
	return nil
}
