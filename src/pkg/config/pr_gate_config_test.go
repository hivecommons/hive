package config

import (
	"strings"
	"testing"
)

func TestPRGateModeForIsOffUnlessModeAndRepoBothSet(t *testing.T) {
	scope := []string{"Example-Org/Example-Repo"}
	tests := []struct {
		name string
		cfg  PRGateConfig
		repo string
		want string
	}{
		{"zero value", PRGateConfig{}, "example-org/example-repo", PRGateModeOff},
		{"explicit off", PRGateConfig{Mode: "off", Repos: scope}, "example-org/example-repo", PRGateModeOff},
		{"shadow in scope, case-insensitive", PRGateConfig{Mode: "Shadow", Repos: scope}, "example-org/example-repo", PRGateModeShadow},
		{"enforce in scope", PRGateConfig{Mode: "enforce", Repos: scope}, "EXAMPLE-ORG/example-repo", PRGateModeEnforce},
		{"shadow, other repo", PRGateConfig{Mode: "shadow", Repos: scope}, "example-org/other", PRGateModeOff},
		{"shadow, no scope", PRGateConfig{Mode: "shadow"}, "example-org/example-repo", PRGateModeOff},
		{"unknown mode is off", PRGateConfig{Mode: "on", Repos: scope}, "example-org/example-repo", PRGateModeOff},
	}
	for _, tc := range tests {
		if got := tc.cfg.ModeFor(tc.repo); got != tc.want {
			t.Errorf("%s: ModeFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPRGateValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PRGateConfig
		wantErr string
	}{
		{"zero value", PRGateConfig{}, ""},
		{"off without scope", PRGateConfig{Mode: "off"}, ""},
		{"shadow with scope", PRGateConfig{Mode: "shadow", Repos: []string{"o/r"}}, ""},
		{"shadow without scope", PRGateConfig{Mode: "shadow"}, "pr_gate.repos"},
		{"enforce with blank scope", PRGateConfig{Mode: "enforce", Repos: []string{" "}}, "pr_gate.repos"},
		{"unknown mode", PRGateConfig{Mode: "yes", Repos: []string{"o/r"}}, "must be off, shadow or enforce"},
	}
	for _, tc := range tests {
		err := tc.cfg.Validate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error %v, want it to mention %q", tc.name, err, tc.wantErr)
		}
	}
}
