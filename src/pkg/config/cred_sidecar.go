package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/credsidecar"
)

// ErrCredSidecarWithInProcessToken is returned by ValidateCredSidecar when the
// credential sidecar is configured (HIVE_CRED_SIDECAR_URL) but proxy-side
// injection is not on, so the hive process would still mint each agent's real
// token and write it into the agent's readable cache: two token holders, one
// of them in the agents' own container.
var ErrCredSidecarWithInProcessToken = errors.New(credsidecar.URLEnv + " is set but " + ProxyInjectGHAuthEnv + " is not " + ProxyInjectGHAuthOnValue)

// ErrCredSidecarMisconfigured wraps a sidecar-mode setting the proxy could not
// use (a non-loopback URL, a missing or short HMAC key, a bad body limit).
var ErrCredSidecarMisconfigured = errors.New("credential sidecar configuration is unusable")

// ValidateCredSidecar is the startup guard for sidecar mode (#9586 phase 2).
// It returns nil whenever HIVE_CRED_SIDECAR_URL is unset, so no existing
// configuration is affected. With it set - an explicit, new opt-in - the spoke
// refuses to start unless the whole posture holds:
//
//   - HIVE_PROXY_INJECT_GH_AUTH must be exactly "true". Anything else means the
//     hive process keeps an in-process token source for agents (the real token
//     in each agent's cache file), so a misconfiguration would silently fall
//     back to in-process tokens while the operator believes the sidecar holds
//     them. That is the mutual exclusion #9586 asks for: sidecar OR in-process,
//     never both.
//   - The sidecar URL, HMAC key and body limit must be usable
//     (credsidecar.LoadClientConfig, the same loader the proxy runs), so a
//     spoke that boots is a spoke whose proxy can actually sign.
//
// The advisory-identity combination is already refused by
// ValidateProxyInjectGHAuth, which runs first.
func ValidateCredSidecar(getenv func(string) string) error {
	if !credsidecar.Enabled(getenv) {
		return nil
	}
	if strings.TrimSpace(getenv(ProxyInjectGHAuthEnv)) != ProxyInjectGHAuthOnValue {
		return fmt.Errorf("%w: with the credential sidecar configured the hive process must not hold agent tokens, but injection off means it mints each agent's real token into the agent's readable cache; set %s=%s, or unset %s to run without the sidecar",
			ErrCredSidecarWithInProcessToken,
			ProxyInjectGHAuthEnv, ProxyInjectGHAuthOnValue, credsidecar.URLEnv)
	}
	if _, err := credsidecar.LoadClientConfig(getenv); err != nil {
		return fmt.Errorf("%w: %v", ErrCredSidecarMisconfigured, err)
	}
	return nil
}
