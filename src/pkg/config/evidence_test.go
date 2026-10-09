package config

import "testing"

func TestEvidenceConfig(t *testing.T) {
	tests := []struct {
		name        string
		cfg         EvidenceConfig
		wantEnabled bool
		wantKey     string
	}{
		{name: "unset is on and unsigned", cfg: EvidenceConfig{}, wantEnabled: true},
		{name: "explicit on", cfg: EvidenceConfig{Enabled: boolPtr(true)}, wantEnabled: true},
		{name: "explicit off", cfg: EvidenceConfig{Enabled: boolPtr(false)}},
		{name: "key path trimmed", cfg: EvidenceConfig{SigningKeyFile: "  /etc/hive/evidence.key\n"}, wantEnabled: true, wantKey: "/etc/hive/evidence.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(); got != tt.wantEnabled {
				t.Fatalf("IsEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			if got := tt.cfg.SigningKeyPath(); got != tt.wantKey {
				t.Fatalf("SigningKeyPath() = %q, want %q", got, tt.wantKey)
			}
		})
	}
}
