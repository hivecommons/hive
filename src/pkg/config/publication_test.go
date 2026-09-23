package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPublicationDefaultOffAndACMMFloor(t *testing.T) {
	var cfg Config
	if cfg.Publication.Enabled || cfg.Publication.PublicationEnabled(6) {
		t.Fatal("publication must be off by default at every level")
	}
	tests := []struct {
		name    string
		enabled bool
		level   int
		want    bool
	}{
		{name: "opted in below floor", enabled: true, level: PublicationMinACMMLevel - 1},
		{name: "opted in at floor", enabled: true, level: PublicationMinACMMLevel, want: true},
		{name: "opted in above floor", enabled: true, level: 6, want: true},
		{name: "absent at L6", level: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (PublicationConfig{Enabled: tt.enabled}).PublicationEnabled(tt.level); got != tt.want {
				t.Fatalf("PublicationEnabled(%d) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestPublicationConfigParsesAndValidatesChannels(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("publication:\n  enabled: true\n  private_channel: \"repo:acme/security\"\n  owner: maintainer\n"), &cfg); err != nil {
		t.Fatalf("unmarshal YAML: %v", err)
	}
	if !cfg.Publication.Enabled || cfg.Publication.Owner != "maintainer" {
		t.Fatalf("publication block did not parse: %+v", cfg.Publication)
	}
	kind, target := cfg.Publication.PrivateChannelKind()
	if kind != "repo" || target != "acme/security" {
		t.Fatalf("channel kind/target = %q/%q, want repo/acme/security", kind, target)
	}
	if err := cfg.Publication.Validate(); err != nil {
		t.Fatalf("repo channel must validate: %v", err)
	}
	if kind, _ := (PublicationConfig{PrivateChannel: " notify "}).PrivateChannelKind(); kind != "notify" {
		t.Fatalf("notify channel kind = %q", kind)
	}
	if kind, _ := (PublicationConfig{}).PrivateChannelKind(); kind != "" {
		t.Fatalf("empty channel must have no kind, got %q", kind)
	}
	for _, bad := range []string{"mailto:security@example.com", "repo:", "repo:acme", "repo:acme/a b", "advisory"} {
		err := (PublicationConfig{PrivateChannel: bad}).Validate()
		if err == nil || !strings.Contains(err.Error(), "publication.private_channel") {
			t.Fatalf("channel %q must be rejected with a named error, got %v", bad, err)
		}
	}
}
