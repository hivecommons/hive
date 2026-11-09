package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A reset to "follow the environment" stores nil, and nil must not survive in
// the persisted YAML: the overlay and the runtime config are whole-config
// marshals, so a lingering writes_enabled would bring the old value back on
// the next restart. An explicit false, by contrast, must persist.
func TestAdminMCPWritesEnabledPersistsOnlyWhenSet(t *testing.T) {
	cfg := Config{HiveID: "h"}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "admin_mcp") || strings.Contains(string(out), "writes_enabled") {
		t.Fatalf("a nil switch was persisted:\n%s", out)
	}

	off := false
	cfg.AdminMCP.WritesEnabled = &off
	out, err = yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "admin_mcp:\n    writes_enabled: false") {
		t.Fatalf("an explicit false was not persisted:\n%s", out)
	}

	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.AdminMCP.WritesEnabled == nil || *back.AdminMCP.WritesEnabled {
		t.Fatalf("round trip lost the explicit false: %v", back.AdminMCP.WritesEnabled)
	}
}
