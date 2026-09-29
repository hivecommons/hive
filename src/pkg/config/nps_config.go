package config

import "strings"

// NPSEnabledEnvVar overrides Hub.NPSEnabled (issue #9610). Accepts the same
// boolean spellings as the other HIVE_* feature gates (1/true/yes/on and
// 0/false/no/off); anything else is ignored and the config value applies.
const NPSEnabledEnvVar = "HIVE_NPS_ENABLED"

// The hosted default below keys off HiveTypeHosted (proxy_inject.go), the
// hub.hive_type value the hub's provisioning template stamps into every hosted
// spoke's config (pkg/hub saas_provision.go).

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
// forwarded over: hub reporting enabled and a hub URL configured.
func (h HubConfig) NPSHubLinked() bool {
	return h.Enabled && strings.TrimSpace(h.URL) != ""
}
