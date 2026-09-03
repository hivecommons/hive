package watchdog

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/rotation"
)

// RotationAuthProbe adapts a rotation-package provider prober (#4608) into a
// credential probe. The rotation probes exercise exactly the credential path
// the RFC's auth probe wants — `claude /usage`, `codex /status`,
// `agy --print /usage`, the deepseek balance API — so a successful usage
// answer proves the credential is alive, and a failure whose output shows
// auth chrome proves it is dead. When PR #4645 rewrites these probers over
// OAuth/JSON-RPC, this adapter picks the rewrite up unchanged: it depends
// only on the rotation.Prober contract.
//
// Verdict mapping (deny silent failures — unknown ≠ healthy AND ≠ dead):
//   - probe answered           → AuthOK (the credential produced an answer)
//   - probe error mentioning
//     login/auth chrome        → AuthFailed
//   - any other probe error    → AuthUnknown (couldn't probe is not a verdict)
type RotationAuthProbe struct {
	Prober rotation.Prober
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
	h := p.Prober.Probe(ctx)
	errText := h.ProbeError()
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
