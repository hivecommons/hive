package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
// Unset resolves per spoke at boot (#9586, ResolveProxyInjectGHAuth): ON for
// a hosted spoke (hub.hive_type == HiveTypeHosted) whose GitHub App auth is
// live and that is not in advisory mode, OFF everywhere else (self-hosted,
// PAT, advisory, hub). An ON default is applied by writing the explicit value
// into the process env (ApplyProxyInjectGHAuthDefault), so every reader below
// sees exactly what an explicitly-set spoke sees. Newly provisioned hosted
// spokes are born with the flag set explicitly: the hub's provisioning
// template renders "true" for App-authenticated spokes and the explicit
// opt-out value ProxyInjectGHAuthOffValue otherwise (see pkg/hub
// provisionProxyInjectGHAuth).
const ProxyInjectGHAuthEnv = "HIVE_PROXY_INJECT_GH_AUTH"

// ProxyInjectGHAuthOnValue is the only value that enables injection - the same
// strict "true" match HIVE_PROXY_ADVISORY_OK uses. ProxyInjectGHAuthOffValue is
// the explicit opt-out: it always wins, including over the hosted-App default
// that unset resolves to (ResolveProxyInjectGHAuth), and says so on the pod
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
	// ProxyInjectGHAuthSourceExplicitOn: HIVE_PROXY_INJECT_GH_AUTH=true.
	ProxyInjectGHAuthSourceExplicitOn ProxyInjectGHAuthSource = "explicit-on"
	// ProxyInjectGHAuthSourceExplicitOff: HIVE_PROXY_INJECT_GH_AUTH=false,
	// the opt-out. Always wins.
	ProxyInjectGHAuthSourceExplicitOff ProxyInjectGHAuthSource = "explicit-off"
	// ProxyInjectGHAuthSourceUnrecognized: a value other than true/false/unset;
	// off, and reported by ProxyInjectGHAuthWarnings.
	ProxyInjectGHAuthSourceUnrecognized ProxyInjectGHAuthSource = "unrecognized"
	// ProxyInjectGHAuthSourceHostedAppDefault: unset on a hosted App spoke
	// outside advisory mode - the default-on case.
	ProxyInjectGHAuthSourceHostedAppDefault ProxyInjectGHAuthSource = "hosted-app-default"
	// ProxyInjectGHAuthSourceDefaultOff: unset anywhere the default does not
	// apply (self-hosted, PAT, no App yet, advisory mode, hub).
	ProxyInjectGHAuthSourceDefaultOff ProxyInjectGHAuthSource = "default-off"
)

// ProxyInjectGHAuthInputs are the process facts the unset default depends on.
type ProxyInjectGHAuthInputs struct {
	// HubMode is true for the hub process (HIVE_MODE=hub). The hub runs no
	// agents; the default never applies to it.
	HubMode bool
	// HiveType is the spoke's hub.hive_type; HiveTypeHosted marks a
	// hub-provisioned spoke.
	HiveType string
	// AppAuth is true when a real GitHub App is this process's credential lane
	// at the moment the decision is made: a real app_id (GitHubConfig.HasApp)
	// AND an App signer built from its key. A PAT-only spoke, a placeholder
	// app_id, or an App whose key has not arrived yet is false.
	AppAuth bool
}

// ProxyInjectGHAuthDecision is the resolved injection state and its reason.
type ProxyInjectGHAuthDecision struct {
	Enabled bool                    `json:"enabled"`
	Source  ProxyInjectGHAuthSource `json:"source"`
	Reason  string                  `json:"reason"`
}

// LogLine is the one-line boot summary, e.g. "proxy GitHub auth injection:
// on (default for hosted App spokes; set HIVE_PROXY_INJECT_GH_AUTH=false to
// opt out)".
func (d ProxyInjectGHAuthDecision) LogLine() string {
	state := "off"
	if d.Enabled {
		state = "on"
	}
	return "proxy GitHub auth injection: " + state + " (" + d.Reason + ")"
}

// ResolveProxyInjectGHAuth decides proxy-side GitHub credential injection for
// this process (#9586). Explicit values behave exactly as before:
// ProxyInjectGHAuthOnValue is on, ProxyInjectGHAuthOffValue is off (the
// opt-out, which always wins), and an unrecognized value is off (and warned
// about by ProxyInjectGHAuthWarnings). UNSET is on only when ALL hold:
//
//   - not the hub process,
//   - a hosted spoke (in.HiveType == HiveTypeHosted),
//   - GitHub App auth is live (in.AppAuth) - never for a PAT spoke, whose
//     agents mint no hub-held token the proxy could inject,
//   - HIVE_PROXY_ADVISORY_OK is not "true" - advisory mode lets the proxy
//     trust a self-asserted agent name, and injection would turn that spoof
//     into a credential grant (ValidateProxyInjectGHAuth). The default steps
//     aside rather than refusing to boot, so it is never fatal.
//
// It never fails; every unmet condition resolves to off with the reason.
func ResolveProxyInjectGHAuth(getenv func(string) string, in ProxyInjectGHAuthInputs) ProxyInjectGHAuthDecision {
	optOut := "set " + ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOffValue + " to opt out"
	switch raw := strings.TrimSpace(getenv(ProxyInjectGHAuthEnv)); raw {
	case ProxyInjectGHAuthOnValue:
		return ProxyInjectGHAuthDecision{Enabled: true, Source: ProxyInjectGHAuthSourceExplicitOn,
			Reason: ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOnValue}
	case ProxyInjectGHAuthOffValue:
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceExplicitOff,
			Reason: ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOffValue + " (explicit opt-out)"}
	case "":
		// Resolved below.
	default:
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceUnrecognized,
			Reason: fmt.Sprintf("unrecognized %s=%q; only %s and %s are accepted",
				ProxyInjectGHAuthEnv, raw, ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue)}
	}
	off := func(why string) ProxyInjectGHAuthDecision {
		return ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceDefaultOff,
			Reason: ProxyInjectGHAuthEnv + " unset and " + why}
	}
	switch {
	case in.HubMode:
		return off("this is the hub process")
	case strings.TrimSpace(in.HiveType) != HiveTypeHosted:
		return off("this is not a hosted spoke (default applies only to hub.hive_type=" + HiveTypeHosted + ")")
	case !in.AppAuth:
		return off("GitHub App auth is not live (PAT spoke, or App not delivered yet; re-evaluated at the next restart)")
	case strings.TrimSpace(getenv(ProxyAdvisoryOKEnv)) == proxyAdvisoryOKEnabledValue:
		return off(ProxyAdvisoryOKEnv + "=" + proxyAdvisoryOKEnabledValue + " (self-asserted agent identity cannot be combined with injection)")
	}
	return ProxyInjectGHAuthDecision{Enabled: true, Source: ProxyInjectGHAuthSourceHostedAppDefault,
		Reason: "default for hosted App spokes; " + optOut}
}

// resolvedProxyInjectGHAuth is the decision ApplyProxyInjectGHAuthDefault
// recorded for this process, read by the dashboard Security tab.
var (
	resolvedProxyInjectGHAuthMu  sync.RWMutex
	resolvedProxyInjectGHAuth    ProxyInjectGHAuthDecision
	resolvedProxyInjectGHAuthSet bool
)

// ApplyProxyInjectGHAuthDefault resolves injection for this spoke and, when
// the hosted-App default turns it on, writes ProxyInjectGHAuthOnValue into
// the process env through setenv. Writing the env (rather than teaching each
// reader the default) keeps every consumer in lockstep with no new plumbing:
// the token-divert path in pkg/github and the proxy snapshot in pkg/proxy
// both read HIVE_PROXY_INJECT_GH_AUTH, and agent subprocesses inherit it - a
// default-on spoke is byte-identical to one provisioned with the explicit
// value. Must run before the proxy is constructed and before any agent token
// is minted (bootGitHub, the first boot phase).
//
// A setenv failure is not fatal: injection stays off and the decision says so.
// The decision is recorded for ProxyInjectGHAuthState.
func ApplyProxyInjectGHAuthDefault(getenv func(string) string, setenv func(string, string) error, in ProxyInjectGHAuthInputs) ProxyInjectGHAuthDecision {
	d := ResolveProxyInjectGHAuth(getenv, in)
	if d.Source == ProxyInjectGHAuthSourceHostedAppDefault {
		if err := setenv(ProxyInjectGHAuthEnv, ProxyInjectGHAuthOnValue); err != nil {
			d = ProxyInjectGHAuthDecision{Source: ProxyInjectGHAuthSourceDefaultOff,
				Reason: "hosted App default could not be applied: " + err.Error()}
		}
	}
	resolvedProxyInjectGHAuthMu.Lock()
	resolvedProxyInjectGHAuth, resolvedProxyInjectGHAuthSet = d, true
	resolvedProxyInjectGHAuthMu.Unlock()
	return d
}

// ProxyInjectGHAuthState returns this process's resolved injection state:
// the decision recorded at boot by ApplyProxyInjectGHAuthDefault, or - when
// none was recorded (hub, tests, tools) - the explicit env reading with the
// unset default treated as off.
func ProxyInjectGHAuthState(getenv func(string) string) ProxyInjectGHAuthDecision {
	resolvedProxyInjectGHAuthMu.RLock()
	d, ok := resolvedProxyInjectGHAuth, resolvedProxyInjectGHAuthSet
	resolvedProxyInjectGHAuthMu.RUnlock()
	if ok {
		return d
	}
	return ResolveProxyInjectGHAuth(getenv, ProxyInjectGHAuthInputs{})
}
