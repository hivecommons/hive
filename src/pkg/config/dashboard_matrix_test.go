package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDashboardMatrixRemainsSeparateFromNotificationMatrix(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(`
dashboard:
  matrix:
    enabled: true
    repos: [owner/repo]
notifications:
  matrix:
    enabled: true
    homeserver_url: https://matrix.example
    access_token: fixture-token
    room_id: "!room:example"
    allowed_users: ["@owner:example"]
`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Dashboard.Matrix.Active() || !cfg.Dashboard.Matrix.RepoAllowed("OWNER/REPO") {
		t.Fatal("public dashboard Matrix lost its explicit repository scope")
	}
	chat := cfg.Notifications.Matrix
	if chat == nil || chat.HomeserverURL != "https://matrix.example" || chat.AccessToken != "fixture-token" || len(chat.AllowedUsers) != 1 {
		t.Fatal("v6 notification Matrix lost its credential or command-allowlist schema")
	}
	data, err := json.Marshal(cfg.Dashboard.Matrix)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access_token", "homeserver_url", "allowed_users", "fixture-token"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("dashboard Matrix exposes notification-only field %q", forbidden)
		}
	}
}

func TestMatrixConfigDefaultsOff(t *testing.T) {
	cfg := &Config{}
	if err := yaml.Unmarshal([]byte("dashboard:\n  port: 1\n"), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dashboard.Matrix.Enabled || cfg.Dashboard.Matrix.Active() {
		t.Fatal("dashboard.matrix must default off")
	}
}

func TestMatrixConfigActive(t *testing.T) {
	tests := []struct {
		name string
		m    DashboardMatrixConfig
		want bool
	}{
		{"zero", DashboardMatrixConfig{}, false},
		{"enabled no repos", DashboardMatrixConfig{Enabled: true}, false},
		{"repos but disabled", DashboardMatrixConfig{Repos: []string{"o/r"}}, false},
		{"enabled with repos", DashboardMatrixConfig{Enabled: true, Repos: []string{"o/r"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Active(); got != tt.want {
				t.Fatalf("Active() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatrixConfigRepoAllowed(t *testing.T) {
	m := DashboardMatrixConfig{Repos: []string{"HiveCommons/Hive"}}
	tests := []struct {
		repo string
		want bool
	}{
		{"hivecommons/hive", true},
		{"HIVECOMMONS/HIVE", true},
		{"hivecommons/other", false},
		{"hivecommons", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := m.RepoAllowed(tt.repo); got != tt.want {
			t.Errorf("RepoAllowed(%q) = %v, want %v", tt.repo, got, tt.want)
		}
	}
}

func TestMatrixConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       DashboardMatrixConfig
		wantErr string
	}{
		{"zero", DashboardMatrixConfig{}, ""},
		{"valid", DashboardMatrixConfig{Repos: []string{"o/r", "my-org/my.repo_1"}, ExcludePaths: []string{"secrets/**"}}, ""},
		{"wildcard name", DashboardMatrixConfig{Repos: []string{"org/*"}}, "dashboard.matrix.repos[0]"},
		{"star", DashboardMatrixConfig{Repos: []string{"*"}}, "dashboard.matrix.repos[0]"},
		{"url", DashboardMatrixConfig{Repos: []string{"https://github.com/o/r"}}, "dashboard.matrix.repos[0]"},
		{"bare name", DashboardMatrixConfig{Repos: []string{"o"}}, "dashboard.matrix.repos[0]"},
		{"empty repo", DashboardMatrixConfig{Repos: []string{"o/r", ""}}, "dashboard.matrix.repos[1]"},
		{"empty parts", DashboardMatrixConfig{Repos: []string{"/r"}}, "dashboard.matrix.repos[0]"},
		{"empty exclude", DashboardMatrixConfig{ExcludePaths: []string{"a", " "}}, "dashboard.matrix.exclude_paths[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMatrixConfigOmittedWhenZero(t *testing.T) {
	out, err := yaml.Marshal(DashboardConfig{Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "matrix") {
		t.Fatalf("zero-valued matrix block was emitted: %s", out)
	}
}
