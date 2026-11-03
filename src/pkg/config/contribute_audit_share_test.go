package config

import "testing"

func TestNormalizeRelayAuditShareLevel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"lifecycle", "lifecycle"},
		{" Commands ", "commands"},
		{"REASONING", "reasoning"},
		{"off", ""},
		{"everything", ""},
	}
	for _, tt := range tests {
		if got := NormalizeRelayAuditShareLevel(tt.in); got != tt.want {
			t.Errorf("NormalizeRelayAuditShareLevel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestContributeAuditShareRequestOrNone(t *testing.T) {
	if got := (HubConfig{}).ContributeAuditShareRequestOrNone(); got != "" {
		t.Fatalf("default request = %q, want none", got)
	}
	if got := (HubConfig{ContributeAuditShareRequest: "commands"}).ContributeAuditShareRequestOrNone(); got != "commands" {
		t.Fatalf("request = %q, want commands", got)
	}
}
