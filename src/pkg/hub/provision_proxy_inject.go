package hub

import (
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// hostedProxyInjectGHAuthEnv is the HUB-side, fleet-wide opt-out for #9586:
// what HIVE_PROXY_INJECT_GH_AUTH value newly provisioned hosted spokes are
// born with. Unset (the default) means injection ON for every new
// App-authenticated spoke; config.ProxyInjectGHAuthOffValue provisions new
// spokes with the explicit opt-out instead, for a hub whose clusters cannot
// support injection yet. It never touches an existing spoke: the provisioning
// template is applied once, at creation (security-model.md, "Provisioning-
// template changes reach existing spokes only through a reconcile"), and no
// reconcile carries this variable.
//
// A single spoke opts out after the fact by setting
// HIVE_PROXY_INJECT_GH_AUTH=false on its own Deployment.
const hostedProxyInjectGHAuthEnv = "HIVE_HOSTED_PROXY_INJECT_GH_AUTH"

// provisionProxyInjectGHAuth returns the HIVE_PROXY_INJECT_GH_AUTH value the
// provisioning template renders onto a new hosted spoke's hive container. The
// result is ALWAYS explicit (on or off), so a hosted spoke's credential posture
// is readable straight off its pod spec.
//
// Injection is only rendered ON for App-authenticated spokes. The hub-held
// token the proxy injects is minted per agent from the GitHub App
// (github.AppAuth.WriteAgentToken); a PAT-authenticated spoke never populates
// that registry, so turning injection on there would strip every agent request
// and attach nothing. Such a spoke gets the explicit opt-out.
//
// An unrecognized hub setting keeps the secure default (ON) and logs, rather
// than silently provisioning spokes that hand agents their real tokens - the
// same "no silent fallback to in-process tokens" rule the spoke-side startup
// guard (config.ValidateProxyInjectGHAuth) enforces.
func provisionProxyInjectGHAuth(useApp bool, getenv func(string) string, logger *slog.Logger) string {
	if !useApp {
		return config.ProxyInjectGHAuthOffValue
	}
	switch raw := strings.TrimSpace(getenv(hostedProxyInjectGHAuthEnv)); raw {
	case "", config.ProxyInjectGHAuthOnValue:
		return config.ProxyInjectGHAuthOnValue
	case config.ProxyInjectGHAuthOffValue:
		return config.ProxyInjectGHAuthOffValue
	default:
		if logger != nil {
			logger.Warn("unrecognized hosted proxy-injection setting - provisioning with injection ON (the secure default)",
				"env", hostedProxyInjectGHAuthEnv, "value", raw,
				"accepted", config.ProxyInjectGHAuthOnValue+"|"+config.ProxyInjectGHAuthOffValue)
		}
		return config.ProxyInjectGHAuthOnValue
	}
}

// provisionProxyInjectGHAuthFromEnv is provisionProxyInjectGHAuth against the
// hub process environment - the form provisionHive uses.
func provisionProxyInjectGHAuthFromEnv(useApp bool, logger *slog.Logger) string {
	return provisionProxyInjectGHAuth(useApp, os.Getenv, logger)
}
