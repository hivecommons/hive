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
// Injection is OPT-IN only: it is on ONLY when this variable is explicitly
// ProxyInjectGHAuthOnValue. Unset (or any other value) is off everywhere -
// hosted or self-hosted, App or PAT, spoke or hub - and the hub renders no
// value for newly provisioned spokes. A brief default-on for hosted App spokes
// (#9597/#9625) was reverted before any spoke ran it (#9586).
const ProxyInjectGHAuthEnv = "HIVE_PROXY_INJECT_GH_AUTH"

// ProxyInjectGHAuthOnValue is the only value that enables injection - the same
// strict "true" match HIVE_PROXY_ADVISORY_OK uses. ProxyInjectGHAuthOffValue is
// the explicit opt-out: it behaves exactly like unset (off) but says so on the
// pod spec, so a spoke that deliberately runs without injection is visibly a
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
// posture (#9586). It returns an error - and the spoke refuses to boot - only
// for a combination that is EXPLOITABLE, not merely wrong:
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
// An unrecognized HIVE_PROXY_INJECT_GH_AUTH value is deliberately NOT fatal
// here: spokes auto-deploy shortly after a merge, and a spoke that already
// carries such a value would crash-loop on the upgrade. That case is reported
// by ProxyInjectGHAuthWarnings instead and keeps today's behavior (off).
//
// getenv is injected (os.Getenv in production) so the boot phase and its tests
// share one implementation.
func ValidateProxyInjectGHAuth(getenv func(string) string) error {
	if strings.TrimSpace(getenv(ProxyInjectGHAuthEnv)) != ProxyInjectGHAuthOnValue {
		return nil
	}
	if strings.TrimSpace(getenv(ProxyAdvisoryOKEnv)) == proxyAdvisoryOKEnabledValue {
		return fmt.Errorf("%w: advisory mode lets the proxy trust a self-asserted Proxy-Authorization agent name, so a caller could claim another agent and receive that agent's injected token; unset %s (restore forced egress) or set %s=%s",
			ErrProxyInjectGHAuthWithSelfAssertedIdentity,
			ProxyAdvisoryOKEnv, ProxyInjectGHAuthEnv, ProxyInjectGHAuthOffValue)
	}
	return nil
}

// ProxyInjectGHAuthWarnings reports the non-fatal credential-posture problems
// (#9586): today, an unrecognized HIVE_PROXY_INJECT_GH_AUTH value ("1", "TRUE",
// "yes", "on", a typo). The readers treat such a value as OFF, which means the
// agent's REAL scoped token is written to its readable cache while the
// operator, who plainly meant something by setting it, may believe injection
// is on. The spoke keeps running with injection off (today's behavior, so an
// auto-deployed upgrade never crash-loops a spoke that already carries such a
// value); the warning is logged at ERROR at boot and surfaced in the dashboard
// Security tab's coherence warnings. Only unset/empty,
// ProxyInjectGHAuthOnValue and ProxyInjectGHAuthOffValue are recognized.
//
// Returns nil when there is nothing to report.
func ProxyInjectGHAuthWarnings(getenv func(string) string) []string {
	raw := strings.TrimSpace(getenv(ProxyInjectGHAuthEnv))
	switch raw {
	case "", ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue:
		return nil
	}
	return []string{fmt.Sprintf("%s=%q is not a recognized value, so proxy-side GitHub credential injection is OFF and agents hold their real GitHub token; set %s=%s to enable injection or %s=%s to opt out explicitly",
		ProxyInjectGHAuthEnv, raw,
		ProxyInjectGHAuthEnv, ProxyInjectGHAuthOnValue,
		ProxyInjectGHAuthEnv, ProxyInjectGHAuthOffValue)}
}

// HiveTypeHosted is the hub.hive_type value the hub's provisioning template
// renders for hub-provisioned ("hosted") spokes (pkg/hub saas_provision.go,
// `hive_type: {{.HiveType}}` with HiveType "hosted"). It is the spoke's own
// signal that it runs on hub-managed infrastructure.
const HiveTypeHosted = "hosted"

// ProxyInjectGHAuthSource says WHY injection resolved the way it did (#9586).
type ProxyInjectGHAuthSource string

const (
	// ProxyInjectGHAuthSourceExplicitOn: HIVE_PROXY_INJECT_GH_AUTH=true, the
	// only way to turn injection on.
	ProxyInjectGHAuthSourceExplicitOn ProxyInjectGHAuthSource = "explicit-on"
	// ProxyInjectGHAuthSourceExplicitOff: HIVE_PROXY_INJECT_GH_AUTH=false,
	// the explicit opt-out.
	ProxyInjectGHAuthSourceExplicitOff ProxyInjectGHAuthSource = "explicit-off"
	// ProxyInjectGHAuthSourceUnrecognized: a value other than true/false/unset;
	// off, and reported by ProxyInjectGHAuthWarnings.
	ProxyInjectGHAuthSourceUnrecognized ProxyInjectGHAuthSource = "unrecognized"
	// ProxyInjectGHAuthSourceDefaultOff: unset - off, because injection is
	// opt-in on every kind of hive.
	ProxyInjectGHAuthSourceDefaultOff ProxyInjectGHAuthSource = "default-off"
)

// ProxyInjectGHAuthOptInReason is the reason an unset variable resolves off,
// shown in the boot log and the dashboard Security tab.
const ProxyInjectGHAuthOptInReason = "opt-in: set " + ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOnValue

// ProxyInjectGHAuthDecision is the resolved injection state and its reason.
type ProxyInjectGHAuthDecision struct {
	Enabled bool                    `json:"enabled"`
	Source  ProxyInjectGHAuthSource `json:"source"`
	Reason  string                  `json:"reason"`
}

// LogLine is the one-line boot summary, e.g. "proxy GitHub auth injection:
// off (opt-in: set HIVE_PROXY_INJECT_GH_AUTH=true)".
func (d ProxyInjectGHAuthDecision) LogLine() string {
	state := "off"
	if d.Enabled {
		state = "on"
	}
	return "proxy GitHub auth injection: " + state + " (" + d.Reason + ")"
}

// ResolveProxyInjectGHAuth reports this process's proxy-side GitHub credential
// injection state and why (#9586). It is a pure reading of the env and agrees
// with ProxyInjectGHAuth by construction: ProxyInjectGHAuthOnValue is on;
// ProxyInjectGHAuthOffValue, unset and any unrecognized value are off. Nothing
// about the hive (hosted or not, App or PAT) changes the answer, and it never
// writes the env.
func ResolveProxyInjectGHAuth(getenv func(string) string) ProxyInjectGHAuthDecision {
	switch raw := strings.TrimSpace(getenv(ProxyInjectGHAuthEnv)); raw {
	case ProxyInjectGHAuthOnValue:
		return ProxyInjectGHAuthDecision{Enabled: true, Source: ProxyInjectGHAuthSourceExplicitOn,
			Reason: ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOnValue}
	case ProxyInjectGHAuthOffValue:
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceExplicitOff,
			Reason: ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOffValue + " (explicit opt-out)"}
	case "":
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceDefaultOff,
			Reason: ProxyInjectGHAuthOptInReason}
	default:
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceUnrecognized,
			Reason: fmt.Sprintf("unrecognized %s=%q; only %s and %s are accepted",
				ProxyInjectGHAuthEnv, raw, ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue)}
	}
}
