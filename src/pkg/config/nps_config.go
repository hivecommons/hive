package config

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// NPSEnabledEnvVar overrides Hub.NPSEnabled (issue #9610). Accepts the same
// boolean spellings as the other HIVE_* feature gates (1/true/yes/on and
// 0/false/no/off); anything else is ignored and the config value applies.
const NPSEnabledEnvVar = "HIVE_NPS_ENABLED"

// HiveTypeHosted is the hub.hive_type value the hub's provisioning template
// stamps into every hosted spoke's config (pkg/hub saas_provision.go).
const HiveTypeHosted = "hosted"

// NPSFeedbackEnabled reports whether the dashboard NPS prompt is enabled for
// this hive and may forward responses to the hub.
//
// Precedence: HIVE_NPS_ENABLED, then hub.nps_enabled, then the default. The
// default is ON only for a hosted spoke (hub.hive_type == "hosted"): the hub
// already operates that hive, so the response never leaves the operator's
// own infrastructure. Every other install defaults OFF, per the rule that a
// hive never sends data off-box without an explicit operator opt-in.
//
// Enabling this is necessary but not sufficient: responses travel only over
// the spoke's authenticated hub link, so a hive with no hub URL sends nothing
// even when this returns true (see NPSHubLinked).
func (h HubConfig) NPSFeedbackEnabled() bool {
	if v, ok := parseBoolEnv(NPSEnabledEnvVar); ok {
		return v
	}
	if h.NPSEnabled != nil {
		return *h.NPSEnabled
	}
	return strings.EqualFold(strings.TrimSpace(h.HiveType), HiveTypeHosted)
}

// NPSHubLinked reports whether this spoke has a hub link NPS responses can be
// forwarded over: hub reporting enabled and a hub URL configured. A hub link
// always wins over the relay (see NPSRelayConfigured).
func (h HubConfig) NPSHubLinked() bool {
	return h.Enabled && strings.TrimSpace(h.URL) != ""
}

// NPS relay for standalone hives (issue #9619). A hive with NPS enabled but no
// hub link posts to a hivecommons-operated relay instead; the hub pulls the
// relay's entries into its own NPS store. Every value below defaults to empty,
// which disables the relay path.
const (
	// NPSRelayURLEnvVar overrides Hub.NPSRelayURL.
	NPSRelayURLEnvVar = "HIVE_NPS_RELAY_URL"
	// NPSRelayTokenEnvVar overrides Hub.NPSRelayToken (spoke side).
	NPSRelayTokenEnvVar = "HIVE_NPS_RELAY_TOKEN"
	// NPSRelayPullSecretEnvVar overrides Hub.NPSRelayPullSecret (hub side).
	NPSRelayPullSecretEnvVar = "HIVE_NPS_RELAY_PULL_SECRET"
)

// npsEnvOr returns the trimmed env var when set, else the trimmed fallback.
func npsEnvOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return strings.TrimSpace(fallback)
}

// ValidNPSRelayURL returns raw with any trailing slash removed when it is an
// acceptable relay base URL, else "". The relay carries a bearer token, so
// only https is accepted, except plain http to a loopback host (local
// development and tests). Query strings, fragments and credentials in the URL
// are refused so the token can never end up somewhere it is logged.
func ValidNPSRelayURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return ""
		}
	default:
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// EffectiveNPSRelayURL resolves the relay base URL: HIVE_NPS_RELAY_URL, then
// hub.nps_relay_url, validated by ValidNPSRelayURL. "" means no relay.
func (h HubConfig) EffectiveNPSRelayURL() string {
	return ValidNPSRelayURL(npsEnvOr(NPSRelayURLEnvVar, h.NPSRelayURL))
}

// EffectiveNPSRelayToken resolves this install's relay token:
// HIVE_NPS_RELAY_TOKEN, then hub.nps_relay_token. A secret; never log it.
func (h HubConfig) EffectiveNPSRelayToken() string {
	return npsEnvOr(NPSRelayTokenEnvVar, h.NPSRelayToken)
}

// EffectiveNPSRelayPullSecret resolves the hub's relay pull secret:
// HIVE_NPS_RELAY_PULL_SECRET, then hub.nps_relay_pull_secret. A secret; never
// log it.
func (h HubConfig) EffectiveNPSRelayPullSecret() string {
	return npsEnvOr(NPSRelayPullSecretEnvVar, h.NPSRelayPullSecret)
}

// NPSRelayConfigured reports whether a standalone spoke has everything it
// needs to submit to the relay: a valid relay URL and an install token. It
// says nothing about opt-in; callers must still check NPSFeedbackEnabled.
func (h HubConfig) NPSRelayConfigured() bool {
	return h.EffectiveNPSRelayURL() != "" && h.EffectiveNPSRelayToken() != ""
}
