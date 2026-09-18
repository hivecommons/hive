package config

import (
	"strings"
	"testing"
)

func TestEscalationSurfaceValidationDefaultsOff(t *testing.T) {
	if err := (EscalationConfig{}).ValidateSurfaces(); err != nil {
		t.Fatalf("zero config should validate: %v", err)
	}
}

func TestEscalationEmailValidation(t *testing.T) {
	base := EscalationConfig{Email: EscalationEmailConfig{Enabled: true, SMTP: EscalationSMTPConfig{Host: "smtp.example.com"}, From: "hive@example.com", To: []string{"ops@example.com"}}}
	if err := base.ValidateSurfaces(); err != nil {
		t.Fatalf("valid email config: %v", err)
	}
	for name, cfg := range map[string]EscalationConfig{
		"host": {Email: EscalationEmailConfig{Enabled: true, From: "hive@example.com", To: []string{"ops@example.com"}}},
		"from": {Email: EscalationEmailConfig{Enabled: true, SMTP: EscalationSMTPConfig{Host: "smtp.example.com"}, To: []string{"ops@example.com"}}},
		"to":   {Email: EscalationEmailConfig{Enabled: true, SMTP: EscalationSMTPConfig{Host: "smtp.example.com"}, From: "hive@example.com"}},
		"time": {Email: EscalationEmailConfig{Enabled: true, SMTP: EscalationSMTPConfig{Host: "smtp.example.com"}, From: "hive@example.com", To: []string{"ops@example.com"}, Digest: EscalationDigestConfig{At: "8am"}}},
	} {
		err := cfg.ValidateSurfaces()
		if err == nil || !strings.Contains(err.Error(), "escalation.email") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}

func TestEscalationPushValidation(t *testing.T) {
	valid := EscalationConfig{Push: EscalationPushConfig{Enabled: true, MinSeverity: "decision", Ntfy: EscalationNtfyConfig{URL: "https://ntfy.sh/hive"}}}
	if err := valid.ValidateSurfaces(); err != nil {
		t.Fatalf("valid push config: %v", err)
	}
	for name, cfg := range map[string]EscalationConfig{
		"severity": {Push: EscalationPushConfig{Enabled: true, MinSeverity: "info", Ntfy: EscalationNtfyConfig{URL: "https://ntfy.sh/hive"}}},
		"provider": {Push: EscalationPushConfig{Enabled: true}},
		"pushover": {Push: EscalationPushConfig{Enabled: true, Pushover: EscalationPushoverConfig{AppToken: "app"}}},
	} {
		err := cfg.ValidateSurfaces()
		if err == nil || !strings.Contains(err.Error(), "escalation.push") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}
