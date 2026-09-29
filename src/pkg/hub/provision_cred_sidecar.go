package hub

import (
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/credsidecar"
)

// hostedCredSidecarEnv is the HUB-side switch for rendering the isolated
// credential sidecar (#9586 phase 2) into NEWLY provisioned hosted spokes.
// Default OFF: unset, "false", or any unrecognized value renders exactly the
// pod spec phase 1 renders. Only "true" adds the sidecar container, its HMAC
// key and the NetworkPolicy - and even then only for spokes that can run it
// (credSidecarRenderable). Like HIVE_HOSTED_PROXY_INJECT_GH_AUTH it never
// touches an existing spoke: the template is applied once, and no reconcile
// carries the sidecar. Turning it on for any spoke is an operator decision.
const hostedCredSidecarEnv = "HIVE_HOSTED_CRED_SIDECAR"

// hostedCredSidecarOnValue is the only value that enables it.
const hostedCredSidecarOnValue = "true"

// infoCredSidecarKey is the domain-separation label for the per-spoke HMAC key
// the proxy signs with and the sidecar verifies with. Derived per hive from
// the current hub master, like the terminal and invite keys, so a
// re-provision renders the same key and no two spokes share one.
const infoCredSidecarKey = "hive-cred-sidecar-hmac-v1"

// credSidecarSecretKey is the hive-secrets entry holding the HMAC key. It is
// the basename of credsidecar.DefaultKeyFile, which is where the /secrets
// projection lands it in both containers.
const credSidecarSecretKey = "cred-sidecar-hmac-key"

// credSidecarAppKeySecretKey is the hive-secrets entry holding the App key
// (the name the template's UseAppFull branch writes).
const credSidecarAppKeySecretKey = "gh-app-key.pem"

// credSidecarUID is the UID the sidecar container runs as: dev, the proxy
// user. The hive container's forced-egress rule exempts that UID's :443 dials
// (owner match), and the pod shares one network namespace, so running as any
// other UID would redirect the sidecar's own GitHub traffic back into the
// hive's MITM proxy. The container is still its own PID and mount namespace,
// so the hive process and the agents cannot read its memory or its files.
const credSidecarUID = 1001

// Sidecar container resources. It is a small relay: one goroutine per request
// in flight and a handful of cached tokens.
const (
	credSidecarCPURequest = "25m"
	credSidecarCPULimit   = "250m"
	credSidecarMemRequest = "32Mi"
	credSidecarMemLimit   = "128Mi"
)

// credSidecarInputs is what decides whether a spoke can run the sidecar.
type credSidecarInputs struct {
	// appKeyInSecret: the App private key is delivered inline in hive-secrets
	// (UseAppFull). The sidecar mounts it from there; without it the pod
	// could not even start (a missing projected key fails the mount).
	appKeyInSecret bool
	// proxyInject is the HIVE_PROXY_INJECT_GH_AUTH value rendered onto the
	// hive container. The spoke's boot guard refuses the sidecar without
	// injection, so rendering one without the other would crash-loop the pod.
	proxyInject    string
	appID          string
	installationID string
	// requiresSCC: the OpenShift SCC lane assigns container UIDs, so the
	// sidecar cannot run as the forced-egress-exempt UID there. Not
	// supported in this phase.
	requiresSCC bool
	hmacKey     string
}

// provisionCredSidecar reports whether a new hosted spoke's manifest gets the
// credential sidecar. It is true only when the hub switch is on AND the spoke
// can run it; every refusal logs why, so an operator who turned the switch on
// can tell which spokes were left without it.
func provisionCredSidecar(in credSidecarInputs, getenv func(string) string, logger *slog.Logger) bool {
	raw := strings.TrimSpace(getenv(hostedCredSidecarEnv))
	if raw != hostedCredSidecarOnValue {
		if raw != "" && raw != "false" && logger != nil {
			logger.Warn("unrecognized hosted credential-sidecar setting - provisioning WITHOUT the sidecar (the default)",
				"env", hostedCredSidecarEnv, "value", raw, "accepted", hostedCredSidecarOnValue+"|false")
		}
		return false
	}
	reason := ""
	switch {
	case !in.appKeyInSecret:
		reason = "the GitHub App key is not delivered in hive-secrets"
	case in.proxyInject != config.ProxyInjectGHAuthOnValue:
		reason = config.ProxyInjectGHAuthEnv + " is not " + config.ProxyInjectGHAuthOnValue + " for this spoke"
	case !positiveID(in.appID) || !positiveID(in.installationID):
		reason = "the App ID or installation ID is not a concrete number yet"
	case in.requiresSCC:
		reason = "the OpenShift SCC lane is not supported yet (the sidecar must run as the forced-egress-exempt UID)"
	case len(in.hmacKey) < credsidecar.MinKeyBytes:
		reason = "no hub master secret to derive the HMAC key from"
	}
	if reason != "" {
		if logger != nil {
			logger.Warn("hosted credential sidecar requested but not rendered for this spoke", "env", hostedCredSidecarEnv, "reason", reason)
		}
		return false
	}
	return true
}

// provisionCredSidecarFromEnv is provisionCredSidecar against the hub process
// environment - the form provisionHive uses.
func provisionCredSidecarFromEnv(in credSidecarInputs, logger *slog.Logger) bool {
	return provisionCredSidecar(in, os.Getenv, logger)
}

// provisionCredSidecarKey is the per-hive HMAC key, hex (64 characters).
func provisionCredSidecarKey(hiveID string) string {
	return derivePerHiveKey(provisionCurrentSecret(), infoCredSidecarKey, hiveID)
}

func positiveID(raw string) bool {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return err == nil && n > 0
}

// credSidecarGitHubHosts is the sidecar's extra forwarding allowlist for a GHE
// hive: the hostnames of its GitHub base and API URLs, deduplicated. Empty for
// github.com hives, whose hosts the sidecar always allows.
func credSidecarGitHubHosts(urls ...string) string {
	seen := map[string]bool{}
	var hosts []string
	for _, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return strings.Join(hosts, ",")
}

// credSidecarTemplateData is the template data the sidecar blocks read. The
// keys are always present (enabled or not), so the template never renders a
// "<no value>" placeholder.
func credSidecarTemplateData(enabled bool, hmacKey, githubBaseURL, githubAPIURL string) map[string]any {
	key := ""
	if enabled {
		key = hmacKey
	}
	return map[string]any{
		"CredSidecar":            enabled,
		"CredSidecarKey":         key,
		"CredSidecarSecretKey":   credSidecarSecretKey,
		"CredSidecarAppKeyName":  credSidecarAppKeySecretKey,
		"CredSidecarURL":         credsidecar.DefaultURL,
		"CredSidecarListen":      credsidecar.DefaultListenAddr,
		"CredSidecarKeyFile":     credsidecar.DefaultKeyFile,
		"CredSidecarAppKeyFile":  credsidecar.DefaultAppKeyFile,
		"CredSidecarUID":         credSidecarUID,
		"CredSidecarGitHubHosts": credSidecarGitHubHosts(githubBaseURL, githubAPIURL),
		"CredSidecarCPURequest":  credSidecarCPURequest,
		"CredSidecarCPULimit":    credSidecarCPULimit,
		"CredSidecarMemRequest":  credSidecarMemRequest,
		"CredSidecarMemLimit":    credSidecarMemLimit,
	}
}
