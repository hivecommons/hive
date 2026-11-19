package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestInboxRecordingDefaults(t *testing.T) {
	var r InboxRecording
	if r.EffectiveEnabled() != DefaultInboxRecordingEnabled {
		t.Fatal("enabled default")
	}
	if r.EffectiveRetentionDays() != 5 || r.EffectiveRetention() != 5*24*time.Hour {
		t.Fatalf("retention = %d", r.EffectiveRetentionDays())
	}
	if r.EffectiveMaxSeconds() != 3*60*60 || r.EffectiveMaxBytes() != 200<<20 || r.EffectiveQuotaBytes() != 2<<30 || r.EffectiveIdleFinalize() != 2*time.Hour {
		t.Fatalf("caps = %d %d %d %v", r.EffectiveMaxSeconds(), r.EffectiveMaxBytes(), r.EffectiveQuotaBytes(), r.EffectiveIdleFinalize())
	}
	if err := ValidateInboxRecording(r); err != nil {
		t.Fatal(err)
	}
}

func TestInboxRecordingYAMLAndValidation(t *testing.T) {
	var c InboxConfig
	src := "recording:\n  enabled: true\n  retention_days: 7\n  max_seconds: 600\n  max_bytes: 1000\n  quota_bytes: 5000\n  idle_finalize_minutes: 30\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	r := c.Recording
	if !r.EffectiveEnabled() || r.EffectiveRetentionDays() != 7 || r.EffectiveMaxSeconds() != 600 || r.EffectiveMaxBytes() != 1000 || r.EffectiveQuotaBytes() != 5000 || r.EffectiveIdleFinalize() != 30*time.Minute {
		t.Fatalf("parsed = %+v", r)
	}
	if err := ValidateInbox(c); err != nil {
		t.Fatal(err)
	}
	c.Recording.MaxBytes = 6000
	if err := ValidateInbox(c); err == nil {
		t.Fatal("max_bytes over quota_bytes accepted")
	}
	c.Recording = InboxRecording{RetentionDays: -1}
	if err := ValidateInbox(c); err == nil {
		t.Fatal("negative retention accepted")
	}
}
