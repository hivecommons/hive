package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBackendsConfig_Allowed(t *testing.T) {
	tests := []struct {
		name    string
		cfg     BackendsConfig
		backend string
		want    bool
	}{
		{"zero value allows everything", BackendsConfig{}, "copilot", true},
		{"empty backend is the hive default", BackendsConfig{Allow: []string{"claude"}, Deny: []string{"copilot"}}, "", true},
		{"denied", BackendsConfig{Deny: []string{"copilot"}}, "copilot", false},
		{"deny is case-insensitive", BackendsConfig{Deny: []string{"Copilot"}}, "copilot", false},
		{"not denied", BackendsConfig{Deny: []string{"copilot"}}, "claude", true},
		{"in allow", BackendsConfig{Allow: []string{"claude", "codex"}}, "codex", true},
		{"not in allow", BackendsConfig{Allow: []string{"claude"}}, "copilot", false},
		{"deny wins over allow", BackendsConfig{Allow: []string{"copilot"}, Deny: []string{"copilot"}}, "copilot", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Allowed(tt.backend); got != tt.want {
				t.Errorf("Allowed(%q) = %v, want %v", tt.backend, got, tt.want)
			}
			c := &Config{Backends: tt.cfg}
			if got := c.BackendAllowed(tt.backend); got != tt.want {
				t.Errorf("Config.BackendAllowed(%q) = %v, want %v", tt.backend, got, tt.want)
			}
		})
	}
}

func TestBackendsConfig_Fallback(t *testing.T) {
	tests := []struct {
		name string
		cfg  BackendsConfig
		want string
	}{
		{"zero value is hive default", BackendsConfig{}, ""},
		{"deny only is hive default", BackendsConfig{Deny: []string{"copilot"}}, ""},
		{"first allowed entry", BackendsConfig{Allow: []string{"claude", "codex"}}, "claude"},
		{"skips denied allow entries", BackendsConfig{Allow: []string{"copilot", "codex"}, Deny: []string{"copilot"}}, "codex"},
		{"nothing usable is hive default", BackendsConfig{Allow: []string{"copilot"}, Deny: []string{"copilot"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Fallback(); got != tt.want {
				t.Errorf("Fallback() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackendsConfig_YAML(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("backends:\n  allow: [claude, codex]\n  deny: [copilot]\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(c.Backends.Allow) != 2 || c.Backends.Allow[0] != "claude" || c.Backends.Allow[1] != "codex" {
		t.Errorf("Backends.Allow = %v, want [claude codex]", c.Backends.Allow)
	}
	if len(c.Backends.Deny) != 1 || c.Backends.Deny[0] != "copilot" {
		t.Errorf("Backends.Deny = %v, want [copilot]", c.Backends.Deny)
	}
	if c.BackendAllowed("copilot") {
		t.Error("BackendAllowed(copilot) = true, want false")
	}
}
