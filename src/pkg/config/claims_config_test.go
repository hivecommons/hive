package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestClaimsConfigDefaults(t *testing.T) {
	var c ClaimsConfig
	if c.IsEnabled() || !c.CommentEnabled() || !c.LabelEnabled() {
		t.Fatal("claims must default off while comment/label default on")
	}
	h, a, ct, m := c.TTLs()
	if h != 0 || a != 0 || ct != 0 || m != 0 {
		t.Fatalf("zero config must yield zero TTLs (package defaults): %v %v %v %v", h, a, ct, m)
	}
}

func TestClaimsConfigYAML(t *testing.T) {
	var cfg Config
	src := "governor:\n  claims:\n    enabled: true\n    ttl_s: 3600\n    human_ttl_s: 7200\n    contributor_ttl_s: 600\n    label: false\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Governor.Claims.IsEnabled() {
		t.Fatal("explicit enabled: true ignored")
	}
	if cfg.Governor.Claims.LabelEnabled() || !cfg.Governor.Claims.CommentEnabled() {
		t.Fatal("label/comment toggles wrong")
	}
	h, _, ct, _ := cfg.Governor.Claims.TTLs()
	if h != 2*time.Hour || ct != 10*time.Minute {
		t.Fatalf("TTLs=%v %v", h, ct)
	}
}
