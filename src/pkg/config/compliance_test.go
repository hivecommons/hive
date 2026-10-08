package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestComplianceConfigDefaults(t *testing.T) {
	var c ComplianceConfig
	if c.IsEnabled() {
		t.Fatal("unset compliance block must be off")
	}
	if got := c.SelectedFrameworks(); got != nil {
		t.Fatalf("SelectedFrameworks = %v, want nil", got)
	}
	if got := c.PostureIntervalOrDefault(); got != DefaultCompliancePostureInterval {
		t.Fatalf("interval = %s, want %s", got, DefaultCompliancePostureInterval)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("zero value must validate: %v", err)
	}
}

func TestComplianceConfigSelectedFrameworksNormalizes(t *testing.T) {
	c := ComplianceConfig{
		Frameworks:    []string{" SOC2-Type2 ", "", "soc2-type2", "iso-27001"},
		PostureChecks: CompliancePostureChecksConfig{Interval: 30 * time.Minute},
	}
	if !c.IsEnabled() {
		t.Fatal("a selected framework enables the block")
	}
	want := []string{"soc2-type2", "iso-27001"}
	if got := c.SelectedFrameworks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectedFrameworks = %v, want %v", got, want)
	}
	if got := c.PostureIntervalOrDefault(); got != 30*time.Minute {
		t.Fatalf("interval = %s", got)
	}
}

func TestIsKnownComplianceFramework(t *testing.T) {
	if !IsKnownComplianceFramework(" SOC2-TYPE2 ") {
		t.Fatal("soc2-type2 must be known regardless of case/space")
	}
	if IsKnownComplianceFramework("fedramp-high") {
		t.Fatal("fedramp-high is not shipped")
	}
	for _, id := range []string{"fedramp-moderate", "iso27001-annex-a"} {
		if !IsKnownComplianceFramework(id) {
			t.Fatalf("%s must be known", id)
		}
	}
}

func TestComplianceConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ComplianceConfig
		wantErr string
	}{
		{name: "fedramp and iso", cfg: ComplianceConfig{Frameworks: []string{"fedramp-moderate", "iso27001-annex-a"}}},
		{name: "soc2", cfg: ComplianceConfig{Frameworks: []string{"soc2-type2"}}},
		{name: "case and space tolerated", cfg: ComplianceConfig{Frameworks: []string{" SOC2-Type2 "}}},
		{name: "min interval", cfg: ComplianceConfig{PostureChecks: CompliancePostureChecksConfig{Interval: MinCompliancePostureInterval}}},
		{name: "max interval", cfg: ComplianceConfig{PostureChecks: CompliancePostureChecksConfig{Interval: MaxCompliancePostureInterval}}},
		{name: "blank", cfg: ComplianceConfig{Frameworks: []string{" "}}, wantErr: "must not be blank"},
		{name: "malformed", cfg: ComplianceConfig{Frameworks: []string{"soc2_type2"}}, wantErr: "not a valid profile id"},
		{name: "unknown", cfg: ComplianceConfig{Frameworks: []string{"fedramp-high"}}, wantErr: "unknown framework"},
		{name: "duplicate", cfg: ComplianceConfig{Frameworks: []string{"soc2-type2", "SOC2-TYPE2"}}, wantErr: "duplicate framework"},
		{name: "negative interval", cfg: ComplianceConfig{PostureChecks: CompliancePostureChecksConfig{Interval: -time.Minute}}, wantErr: "must not be negative"},
		{name: "too short", cfg: ComplianceConfig{PostureChecks: CompliancePostureChecksConfig{Interval: time.Second}}, wantErr: "at least"},
		{name: "too long", cfg: ComplianceConfig{PostureChecks: CompliancePostureChecksConfig{Interval: MaxCompliancePostureInterval + time.Hour}}, wantErr: "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestComplianceConfigYAMLRoundTrip(t *testing.T) {
	var cfg Config
	src := "compliance:\n  frameworks: [soc2-type2]\n  posture_checks:\n    interval: 1h\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg.Compliance.Frameworks, []string{"soc2-type2"}) {
		t.Fatalf("frameworks = %v", cfg.Compliance.Frameworks)
	}
	if cfg.Compliance.PostureChecks.Interval != time.Hour {
		t.Fatalf("interval = %s", cfg.Compliance.PostureChecks.Interval)
	}
	if err := cfg.Compliance.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateRejectsUnknownComplianceFramework(t *testing.T) {
	cfg := &Config{
		Project:    ProjectConfig{Org: "o"},
		GitHub:     GitHubConfig{Token: "t"},
		Compliance: ComplianceConfig{Frameworks: []string{"not-a-framework"}},
	}
	err := cfg.ValidateWithOptions(ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "compliance.frameworks[0]") {
		t.Fatalf("Validate = %v, want compliance.frameworks error", err)
	}
}
