package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
		m    MatrixConfig
		want bool
	}{
		{"zero", MatrixConfig{}, false},
		{"enabled no repos", MatrixConfig{Enabled: true}, false},
		{"repos but disabled", MatrixConfig{Repos: []string{"o/r"}}, false},
		{"enabled with repos", MatrixConfig{Enabled: true, Repos: []string{"o/r"}}, true},
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
	m := MatrixConfig{Repos: []string{"HiveCommons/Hive"}}
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
		m       MatrixConfig
		wantErr string
	}{
		{"zero", MatrixConfig{}, ""},
		{"valid", MatrixConfig{Repos: []string{"o/r", "my-org/my.repo_1"}, ExcludePaths: []string{"secrets/**"}}, ""},
		{"wildcard name", MatrixConfig{Repos: []string{"org/*"}}, "dashboard.matrix.repos[0]"},
		{"star", MatrixConfig{Repos: []string{"*"}}, "dashboard.matrix.repos[0]"},
		{"url", MatrixConfig{Repos: []string{"https://github.com/o/r"}}, "dashboard.matrix.repos[0]"},
		{"bare name", MatrixConfig{Repos: []string{"o"}}, "dashboard.matrix.repos[0]"},
		{"empty repo", MatrixConfig{Repos: []string{"o/r", ""}}, "dashboard.matrix.repos[1]"},
		{"empty parts", MatrixConfig{Repos: []string{"/r"}}, "dashboard.matrix.repos[0]"},
		{"empty exclude", MatrixConfig{ExcludePaths: []string{"a", " "}}, "dashboard.matrix.exclude_paths[1]"},
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
