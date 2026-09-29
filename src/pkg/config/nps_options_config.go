package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// NPS prompt options beyond the on/off switch (issue #9610): operator
// overrides for the eligibility timing, the optional GA4 measurement ID, and
// the opt-in detractor public issue.

// Default NPS eligibility timing. These are kubestellar/console's
// useNPSSurvey.ts values and match the dashboard's npsTiming() constants one
// for one; a zero field in NPSTimingConfig resolves to these.
const (
	DefaultNPSMinSessions                    = 2
	DefaultNPSSecondSessionEngagementSeconds = 5 * 60
	DefaultNPSReturningEngagementSeconds     = 1 * 60
	DefaultNPSRepromptDays                   = 30
	DefaultNPSDismissRetryDays               = 7
	DefaultNPSMaxDismissals                  = 3
)

// Upper bounds for the timing overrides. Generous enough for any sane cadence;
// they exist so a typo (an extra zero or three) is rejected at load time
// instead of silently turning the prompt off for years.
const (
	maxNPSMinSessions       = 100
	maxNPSEngagementSeconds = 24 * 60 * 60
	maxNPSDays              = 365
	maxNPSMaxDismissals     = 100
)

// NPSTimingConfig is hub.nps_timing. Zero (or absent) means "use the default".
type NPSTimingConfig struct {
	// MinSessions: never prompt before this browser session.
	MinSessions int `yaml:"min_sessions,omitempty" json:"min_sessions,omitempty"`
	// SecondSessionEngagementSeconds: engaged time required in the first
	// eligible session.
	SecondSessionEngagementSeconds int `yaml:"second_session_engagement_seconds,omitempty" json:"second_session_engagement_seconds,omitempty"`
	// ReturningEngagementSeconds: engaged time required from the 3rd session on.
	ReturningEngagementSeconds int `yaml:"returning_engagement_seconds,omitempty" json:"returning_engagement_seconds,omitempty"`
	// RepromptDays: wait after a response, and after MaxDismissals dismissals.
	RepromptDays int `yaml:"reprompt_days,omitempty" json:"reprompt_days,omitempty"`
	// DismissRetryDays: wait after a single dismissal.
	DismissRetryDays int `yaml:"dismiss_retry_days,omitempty" json:"dismiss_retry_days,omitempty"`
	// MaxDismissals: dismissals after which the wait becomes RepromptDays.
	MaxDismissals int `yaml:"max_dismissals,omitempty" json:"max_dismissals,omitempty"`
}

// npsTimingField describes one override for validation and resolution.
type npsTimingField struct {
	key   string
	value int
	def   int
	max   int
}

func (t NPSTimingConfig) fields() []npsTimingField {
	return []npsTimingField{
		{"min_sessions", t.MinSessions, DefaultNPSMinSessions, maxNPSMinSessions},
		{"second_session_engagement_seconds", t.SecondSessionEngagementSeconds, DefaultNPSSecondSessionEngagementSeconds, maxNPSEngagementSeconds},
		{"returning_engagement_seconds", t.ReturningEngagementSeconds, DefaultNPSReturningEngagementSeconds, maxNPSEngagementSeconds},
		{"reprompt_days", t.RepromptDays, DefaultNPSRepromptDays, maxNPSDays},
		{"dismiss_retry_days", t.DismissRetryDays, DefaultNPSDismissRetryDays, maxNPSDays},
		{"max_dismissals", t.MaxDismissals, DefaultNPSMaxDismissals, maxNPSMaxDismissals},
	}
}

// Validate rejects a negative or out-of-range override. Zero is valid (it
// means the default).
func (t NPSTimingConfig) Validate() error {
	for _, f := range t.fields() {
		if f.value < 0 || f.value > f.max {
			return fmt.Errorf("hub.nps_timing.%s must be between 1 and %d (or 0/unset for the default %d), got %d", f.key, f.max, f.def, f.value)
		}
	}
	return nil
}

// resolve returns f's effective value: the override when in range, else the
// default. Validate already refuses out-of-range values at load; this keeps a
// config that bypassed it (a hand-built struct, a hot reload) from reaching
// the browser.
func (f npsTimingField) resolve() int {
	if f.value < 1 || f.value > f.max {
		return f.def
	}
	return f.value
}

// EffectiveNPSTiming returns hub.nps_timing with every unset or invalid field
// replaced by its default. The result has no zero fields.
func (h HubConfig) EffectiveNPSTiming() NPSTimingConfig {
	f := h.NPSTiming.fields()
	return NPSTimingConfig{
		MinSessions:                    f[0].resolve(),
		SecondSessionEngagementSeconds: f[1].resolve(),
		ReturningEngagementSeconds:     f[2].resolve(),
		RepromptDays:                   f[3].resolve(),
		DismissRetryDays:               f[4].resolve(),
		MaxDismissals:                  f[5].resolve(),
	}
}

// NPSGA4MeasurementIDEnvVar names the GA4 measurement ID the dashboard sends
// the hive_nps_* funnel events to. Unset (the default) disables GA4 entirely:
// no script is loaded, no event is sent, and the CSP is not widened.
//
// It is deliberately an environment variable and not a hive.yaml key: the SPA
// document is served by the Node proxy in the combined image, which reads only
// its environment, and both it and the Go server must widen their CSP from the
// same source or gtag.js would be allowed by one and blocked by the other.
const NPSGA4MeasurementIDEnvVar = "HIVE_NPS_GA4_MEASUREMENT_ID"

// npsGA4MeasurementIDRe is the shape of a GA4 measurement ID ("G-" plus an
// uppercase alphanumeric suffix). The proxy (src/proxy/server.js) applies the
// same pattern. Anything else is ignored, so a malformed value cannot be
// interpolated into the gtag URL or the CSP.
var npsGA4MeasurementIDRe = regexp.MustCompile(`^G-[A-Z0-9]{4,20}$`)

// NPSGA4MeasurementID returns the configured GA4 measurement ID, or "" when
// GA4 is disabled (unset or malformed).
func NPSGA4MeasurementID() string {
	id := strings.TrimSpace(os.Getenv(NPSGA4MeasurementIDEnvVar))
	if !npsGA4MeasurementIDRe.MatchString(id) {
		return ""
	}
	return id
}

// NPSDetractorIssuesConfig is hub.nps_detractor_issues: whether a detractor
// may turn their NPS feedback into a PUBLIC issue, and where it is filed.
type NPSDetractorIssuesConfig struct {
	// Enabled opts the hive in. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the "owner/name" repository the issue is filed in, by this
	// hive's App. Required when Enabled; the App must be installed there.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
}

// npsIssueRepoRe accepts an "owner/name" repository reference.
var npsIssueRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// Validate requires a well-formed repo when the feature is enabled.
func (d NPSDetractorIssuesConfig) Validate() error {
	if !d.Enabled {
		return nil
	}
	if !npsIssueRepoRe.MatchString(strings.TrimSpace(d.Repo)) {
		return fmt.Errorf("hub.nps_detractor_issues.repo must be an owner/name repository when hub.nps_detractor_issues.enabled is true, got %q", d.Repo)
	}
	return nil
}

// NPSDetractorIssueRepo returns the repository detractor issues are filed in,
// or "" when the feature is off or misconfigured (fail closed).
func (h HubConfig) NPSDetractorIssueRepo() string {
	d := h.NPSDetractorIssues
	if d.Validate() != nil || !d.Enabled {
		return ""
	}
	return strings.TrimSpace(d.Repo)
}

// ValidateNPS validates every NPS option under hub.
func (h HubConfig) ValidateNPS() error {
	if err := h.NPSTiming.Validate(); err != nil {
		return err
	}
	return h.NPSDetractorIssues.Validate()
}
