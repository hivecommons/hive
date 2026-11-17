package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpektacularRecheckBlockIsAcceptedAndIgnored pins the retirement of
// campaign recheck: an existing hive.yaml that still carries
// runs.spektacular.recheck (even with sources the old validator rejected)
// keeps loading, keeps validating, and is reported as deprecated.
func TestSpektacularRecheckBlockIsAcceptedAndIgnored(t *testing.T) {
	const withRecheck = `
runs:
  spektacular:
    enabled: true
    recheck:
      enabled: true
      default_interval: 72h
      max_delta_tasks: 5
      discovery_proxy: ""
      sources:
        - {name: outside, kind: release, url: "https://outside.example/news"}
      discovery:
        enabled: true
        sources:
          - {kind: search_engine, name: bad, url_or_repo: "https://blocked.example.com/feed.xml"}
`
	var c Config
	if err := yaml.Unmarshal([]byte(withRecheck), &c); err != nil {
		t.Fatalf("hive.yaml with the retired recheck block must load: %v", err)
	}
	if !c.Runs.Spektacular.Enabled {
		t.Fatal("sibling runs.spektacular keys must still decode")
	}
	if !c.WarnDeprecatedSpektacularRecheck() {
		t.Fatal("a recheck block must be reported as deprecated")
	}

	var plain Config
	if err := yaml.Unmarshal([]byte("runs:\n  spektacular:\n    enabled: true\n"), &plain); err != nil {
		t.Fatalf("load: %v", err)
	}
	if plain.WarnDeprecatedSpektacularRecheck() {
		t.Fatal("no recheck block must not be reported as deprecated")
	}
	var nilCfg *Config
	if nilCfg.WarnDeprecatedSpektacularRecheck() {
		t.Fatal("nil config must not report deprecation")
	}
}
