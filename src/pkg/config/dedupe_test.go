package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDedupeAdvisoryAutocloseDefaultsOn(t *testing.T) {
	var d DedupeLaneConfig
	if !d.AdvisoryAutocloseEnabled() {
		t.Fatal("advisory_autoclose must default on: the checklist never waits on a person")
	}
	off := false
	d.AdvisoryAutoclose = &off
	if d.AdvisoryAutocloseEnabled() {
		t.Fatal("advisory_autoclose: false must disable the sweep")
	}
	on := true
	d.AdvisoryAutoclose = &on
	if !d.AdvisoryAutocloseEnabled() {
		t.Fatal("advisory_autoclose: true must enable the sweep")
	}
}

func TestDedupeConfigParsesFromYAML(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("dedupe:\n  advisory_autoclose: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Dedupe.AdvisoryAutocloseEnabled() {
		t.Fatal("dedupe.advisory_autoclose: false did not parse")
	}
}
