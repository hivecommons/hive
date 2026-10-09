package main

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestReviewEvidenceSettings(t *testing.T) {
	level := 5
	cfg := &config.Config{
		ACMMLevel: &level,
		Review:    config.ReviewConfig{RequireApproval: true},
		AutoMerge: config.AutoMergeConfig{HumanMergePaths: map[string][]string{"o/r": {"OWNERS"}}},
		Evidence:  config.EvidenceConfig{SigningKeyFile: " /keys/evidence.key "},
	}

	got := reviewEvidenceSettings(cfg, "o/r")
	if !got.Enabled || got.SigningKeyFile != "/keys/evidence.key" {
		t.Fatalf("enabled/key = %v/%q", got.Enabled, got.SigningKeyFile)
	}
	if got.Policy.ACMMLevel != 5 || !got.Policy.RequireApproval || !reflect.DeepEqual(got.Policy.HumanMergePaths, []string{"OWNERS"}) {
		t.Fatalf("policy = %+v", got.Policy)
	}
	if got.Policy.SentinelConfigHash == "" || got.Policy.SentinelConfigHash != sentinelConfigHash(cfg.Sentinel) {
		t.Fatalf("sentinel hash = %q", got.Policy.SentinelConfigHash)
	}
	if other := reviewEvidenceSettings(cfg, "o/other"); other.Policy.HumanMergePaths != nil {
		t.Fatalf("other repo human merge paths = %v, want none", other.Policy.HumanMergePaths)
	}

	off := false
	cfg.Evidence.Enabled = &off
	cfg.Sentinel.Label = "custom"
	got2 := reviewEvidenceSettings(cfg, "o/r")
	if got2.Enabled {
		t.Fatal("evidence.enabled: false still enabled")
	}
	if got2.Policy.SentinelConfigHash == got.Policy.SentinelConfigHash {
		t.Fatal("sentinel hash did not change with the sentinel config")
	}
	if nilCfg := reviewEvidenceSettings(nil, "o/r"); !reflect.DeepEqual(nilCfg, github.ReviewEvidenceSettings{}) {
		t.Fatal("nil config produced settings")
	}
}
