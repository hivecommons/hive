package watchdog

import (
	"context"
	"strings"
)

// RotationAuthProbe adapts a rotation-package provider prober (#4608) into a
// credential probe. The rotation probes exercise exactly the credential path
// the RFC's auth probe wants — `claude /usage`, `codex /status`,
// `agy --print /usage`, the deepseek balance API — so a successful usage
// answer proves the credential is alive, and a failure whose output shows
// auth chrome proves it is dead. When PR #4645 rewrites these probers over
// OAuth/JSON-RPC, this adapter picks the rewrite up unchanged: it depends
// only on the UsageProber slice of the rotation.Prober contract, which
// cmd/hive adapts (the watchdog does not import pkg/rotation).
//
// Verdict mapping (deny silent failures — unknown ≠ healthy AND ≠ dead):
//   - probe answered           → AuthOK (the credential produced an answer)
//   - probe error mentioning
//     login/auth chrome        → AuthFailed
//   - any other probe error    → AuthUnknown (couldn't probe is not a verdict)
type RotationAuthProbe struct {
	Prober UsageProber
}

// UsageProber is what the auth probe reads from a provider usage prober:
// its provider key and the sanitised text of the probe's error, "" when the
// probe answered. It is the two-method slice of rotation.HeadroomSource the
// adapter needs; hive wraps each rotation prober in cmd/hive.
type UsageProber interface {
	Provider() string
	ProbeError(ctx context.Context) string
}

// authErrorMarkers are substrings in a probe error that prove the failure is
// a CREDENTIAL failure rather than a transport/parse one. Matched
// case-insensitively against the probe's own error text.
var authErrorMarkers = []string{
	"login expired",
	"please run /login",
	"/login",
	"not logged in",
	"unauthorized",
	"401",
	"invalid api key",
	"authentication",
}

func (p RotationAuthProbe) Provider() string { return p.Prober.Provider() }

func (p RotationAuthProbe) ProbeAuth(ctx context.Context) (AuthStatus, string) {
	errText := p.Prober.ProbeError(ctx)
	if errText == "" {
		return AuthOK, "provider usage probe answered"
	}
	lower := strings.ToLower(errText)
	for _, marker := range authErrorMarkers {
		if strings.Contains(lower, marker) {
			return AuthFailed, "provider probe reported an auth failure: " + errText
		}
	}
	return AuthUnknown, "provider probe inconclusive: " + errText
}
