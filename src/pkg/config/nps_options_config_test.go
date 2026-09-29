package config

import (
	"strings"
	"testing"
)

// TestEffectiveNPSTimingDefaults pins that an unset hub.nps_timing resolves to
// exactly the console-identical constants the dashboard used before overrides
// existed (issue #9610), so adding the knob changed nothing for anyone.
func TestEffectiveNPSTimingDefaults(t *testing.T) {
	got := HubConfig{}.EffectiveNPSTiming()
	want := NPSTimingConfig{
		MinSessions:                    2,
		SecondSessionEngagementSeconds: 300,
		ReturningEngagementSeconds:     60,
		RepromptDays:                   30,
		DismissRetryDays:               7,
		MaxDismissals:                  3,
	}
	if got != want {
		t.Fatalf("EffectiveNPSTiming() = %+v, want %+v", got, want)
	}
}

// TestEffectiveNPSTimingOverrides: each in-range override wins; an
// out-of-range one falls back to its default (fail safe), independently.
func TestEffectiveNPSTimingOverrides(t *testing.T) {
	h := HubConfig{NPSTiming: NPSTimingConfig{
		MinSessions:                    3,
		SecondSessionEngagementSeconds: 120,
		ReturningEngagementSeconds:     -5,
		RepromptDays:                   90,
		DismissRetryDays:               maxNPSDays + 1,
		MaxDismissals:                  5,
	}}
	got := h.EffectiveNPSTiming()
	want := NPSTimingConfig{
		MinSessions:                    3,
		SecondSessionEngagementSeconds: 120,
		ReturningEngagementSeconds:     DefaultNPSReturningEngagementSeconds,
		RepromptDays:                   90,
		DismissRetryDays:               DefaultNPSDismissRetryDays,
		MaxDismissals:                  5,
	}
	if got != want {
		t.Fatalf("EffectiveNPSTiming() = %+v, want %+v", got, want)
	}
}

func TestNPSTimingValidate(t *testing.T) {
	if err := (NPSTimingConfig{}).Validate(); err != nil {
		t.Fatalf("zero timing must validate (it means defaults): %v", err)
	}
	if err := (NPSTimingConfig{MinSessions: 1, RepromptDays: maxNPSDays, MaxDismissals: 1}).Validate(); err != nil {
		t.Fatalf("in-range overrides must validate: %v", err)
	}
	bad := []struct {
		name string
		cfg  NPSTimingConfig
		key  string
	}{
		{"negative sessions", NPSTimingConfig{MinSessions: -1}, "min_sessions"},
		{"too many sessions", NPSTimingConfig{MinSessions: maxNPSMinSessions + 1}, "min_sessions"},
		{"engagement over a day", NPSTimingConfig{SecondSessionEngagementSeconds: maxNPSEngagementSeconds + 1}, "second_session_engagement_seconds"},
		{"negative returning engagement", NPSTimingConfig{ReturningEngagementSeconds: -1}, "returning_engagement_seconds"},
		{"reprompt over a year", NPSTimingConfig{RepromptDays: maxNPSDays + 1}, "reprompt_days"},
		{"negative dismiss retry", NPSTimingConfig{DismissRetryDays: -7}, "dismiss_retry_days"},
		{"too many dismissals", NPSTimingConfig{MaxDismissals: maxNPSMaxDismissals + 1}, "max_dismissals"},
	}
	for _, tc := range bad {
		err := tc.cfg.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "hub.nps_timing."+tc.key) {
			t.Errorf("%s: error %q does not name hub.nps_timing.%s", tc.name, err, tc.key)
		}
	}
}

// TestConfigLoadRejectsBadNPSOptions proves ValidateNPS is actually wired
// into config loading (a validator nobody calls protects nothing), and that
// valid overrides survive the YAML round trip.
func TestConfigLoadRejectsBadNPSOptions(t *testing.T) {
	const base = `
project:
  org: my-org
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	cfg, err := Load(writeTempConfig(t, base+`hub:
  nps_timing:
    reprompt_days: 60
    max_dismissals: 2
  nps_detractor_issues:
    enabled: true
    repo: acme/widgets
`))
	if err != nil {
		t.Fatalf("positive control: valid NPS options must load: %v", err)
	}
	if got := cfg.Hub.EffectiveNPSTiming(); got.RepromptDays != 60 || got.MaxDismissals != 2 || got.MinSessions != DefaultNPSMinSessions {
		t.Errorf("loaded timing = %+v, want reprompt 60, max dismissals 2, other fields default", got)
	}
	if got := cfg.Hub.NPSDetractorIssueRepo(); got != "acme/widgets" {
		t.Errorf("loaded detractor repo = %q, want acme/widgets", got)
	}

	if _, err := Load(writeTempConfig(t, base+`hub:
  nps_timing:
    reprompt_days: -1
`)); err == nil || !strings.Contains(err.Error(), "nps_timing") {
		t.Errorf("negative reprompt_days: Load() error = %v, want an nps_timing error", err)
	}
	if _, err := Load(writeTempConfig(t, base+`hub:
  nps_detractor_issues:
    enabled: true
`)); err == nil || !strings.Contains(err.Error(), "nps_detractor_issues") {
		t.Errorf("enabled detractor issues without a repo: Load() error = %v, want an nps_detractor_issues error", err)
	}
}

// TestNPSDetractorIssueRepoOffByDefault: the public-issue path is opt-in and
// fails closed on a missing or malformed repo.
func TestNPSDetractorIssueRepoOffByDefault(t *testing.T) {
	cases := []struct {
		name string
		cfg  NPSDetractorIssuesConfig
		want string
	}{
		{"unset is off", NPSDetractorIssuesConfig{}, ""},
		{"repo without enabled is off", NPSDetractorIssuesConfig{Repo: "acme/widgets"}, ""},
		{"enabled without repo is off", NPSDetractorIssuesConfig{Enabled: true}, ""},
		{"enabled with bare name is off", NPSDetractorIssuesConfig{Enabled: true, Repo: "widgets"}, ""},
		{"enabled with URL is off", NPSDetractorIssuesConfig{Enabled: true, Repo: "https://github.com/acme/widgets"}, ""},
		{"enabled with owner/name", NPSDetractorIssuesConfig{Enabled: true, Repo: " acme/widgets "}, "acme/widgets"},
	}
	for _, tc := range cases {
		if got := (HubConfig{NPSDetractorIssues: tc.cfg}).NPSDetractorIssueRepo(); got != tc.want {
			t.Errorf("%s: NPSDetractorIssueRepo() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNPSGA4MeasurementID: GA4 is off unless a well-formed ID is set, so a
// malformed value can never reach the gtag URL or the CSP.
func TestNPSGA4MeasurementID(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"G-ABC123XYZ", "G-ABC123XYZ"},
		{" G-ABC123XYZ ", "G-ABC123XYZ"},
		{"UA-12345-1", ""},
		{"G-abc123", ""},
		{"G-ABC\"onload=x", ""},
		{"G-AB", ""},
		{"G-ABC123'; script-src *", ""},
	}
	for _, tc := range cases {
		t.Setenv(NPSGA4MeasurementIDEnvVar, tc.env)
		if got := NPSGA4MeasurementID(); got != tc.want {
			t.Errorf("%s=%q: NPSGA4MeasurementID() = %q, want %q", NPSGA4MeasurementIDEnvVar, tc.env, got, tc.want)
		}
	}
}
