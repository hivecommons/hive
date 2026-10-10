package config

import (
	"strings"
	"testing"
)

func TestValidateBobDisplayName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty inherits agent name", "", false},
		{"hive prefix", "hive-scanner", false},
		{"dots and underscores", "hive.scanner_01", false},
		{"max length", strings.Repeat("a", MaxBobDisplayNameLen), false},
		{"too long", strings.Repeat("a", MaxBobDisplayNameLen+1), true},
		{"space", "hive scanner", true},
		{"newline", "hive\nscanner", true},
		{"slash", "hive/scanner", true},
		{"shell metachar", "hive$(id)", true},
		{"non-ascii", "hivé", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBobDisplayName(tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateBobDisplayName(%q) error = %v, wantErr %v", tc.value, err, tc.wantErr)
			}
		})
	}
}

func TestValidateBobSessionPrefixAndLabel(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "default off",
			cfg: Config{
				Project: ProjectConfig{Org: "hivecommons"},
				GitHub:  GitHubConfig{Token: "ghp_test"},
				Agents:  map[string]AgentConfig{"scanner": {Backend: "claude"}},
			},
		},
		{
			name: "global prefix",
			cfg: Config{
				Project:  ProjectConfig{Org: "hivecommons"},
				GitHub:   GitHubConfig{Token: "ghp_test"},
				Governor: GovernorConfig{Bob: BobConfig{SessionPrefix: "hive-"}},
				Agents:   map[string]AgentConfig{"scanner": {Backend: "bob"}},
			},
		},
		{
			name: "per agent label",
			cfg: Config{
				Project: ProjectConfig{Org: "hivecommons"},
				GitHub:  GitHubConfig{Token: "ghp_test"},
				Agents:  map[string]AgentConfig{"scanner": {Backend: "bob", Bob: AgentBobConfig{SessionLabel: "hive-scanner"}}},
			},
		},
		{
			name: "bad prefix",
			cfg: Config{
				Project:  ProjectConfig{Org: "hivecommons"},
				GitHub:   GitHubConfig{Token: "ghp_test"},
				Governor: GovernorConfig{Bob: BobConfig{SessionPrefix: "hive/"}},
				Agents:   map[string]AgentConfig{"scanner": {Backend: "bob"}},
			},
			wantErr: "governor.bob.session_prefix",
		},
		{
			name: "bad per agent label",
			cfg: Config{
				Project: ProjectConfig{Org: "hivecommons"},
				GitHub:  GitHubConfig{Token: "ghp_test"},
				Agents:  map[string]AgentConfig{"scanner": {Backend: "bob", Bob: AgentBobConfig{SessionLabel: "hive scanner"}}},
			},
			wantErr: "bob.session_label",
		},
		{
			name: "combined label too long",
			cfg: Config{
				Project:  ProjectConfig{Org: "hivecommons"},
				GitHub:   GitHubConfig{Token: "ghp_test"},
				Governor: GovernorConfig{Bob: BobConfig{SessionPrefix: strings.Repeat("a", MaxBobSessionLabelLen)}},
				Agents:   map[string]AgentConfig{"scanner": {Backend: "bob"}},
			},
			wantErr: "longer than",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.ValidateWithOptions(ValidateOptions{RequireAgents: true})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateWithOptions() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateWithOptions() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
