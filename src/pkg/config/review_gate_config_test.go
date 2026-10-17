package config

import (
	"strings"
	"testing"
)

func TestReviewGateConfigModeOrDefault(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{mode: "", want: ReviewGateModeShadow},
		{mode: ReviewGateModeShadow, want: ReviewGateModeShadow},
		{mode: ReviewGateModeOff, want: ReviewGateModeOff},
		{mode: ReviewGateModeEnforce, want: ReviewGateModeEnforce},
		{mode: "bogus", want: ReviewGateModeShadow},
	}
	for _, tt := range tests {
		if got := (ReviewGateConfig{Mode: tt.mode}).ModeOrDefault(); got != tt.want {
			t.Errorf("ModeOrDefault(%q) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestValidate_ReviewGateMode(t *testing.T) {
	tests := []struct {
		mode    string
		wantErr bool
	}{
		{mode: ""},
		{mode: ReviewGateModeOff},
		{mode: ReviewGateModeShadow},
		{mode: ReviewGateModeEnforce},
		{mode: "Enforce", wantErr: true},
		{mode: "block", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			c := &Config{
				Project:  ProjectConfig{Org: "my-org"},
				GitHub:   GitHubConfig{Token: "t"},
				Agents:   map[string]AgentConfig{"scanner": {Backend: "claude"}},
				Governor: GovernorConfig{ReviewGate: ReviewGateConfig{Mode: tt.mode}},
			}
			err := c.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "review_gate.mode") {
				t.Fatalf("error %q does not name review_gate.mode", err)
			}
		})
	}
}
