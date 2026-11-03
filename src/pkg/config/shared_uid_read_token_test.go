package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestSharedUIDReadTokenOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := Config{GitHub: GitHubConfig{SharedUIDReadToken: enabled}}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var loaded Config
		if err := yaml.Unmarshal(data, &loaded); err != nil {
			t.Fatal(err)
		}
		if loaded.GitHub.SharedUIDReadToken != enabled {
			t.Fatalf("opt-in changed on round trip: %v", loaded.GitHub.SharedUIDReadToken)
		}
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("github: {}"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.SharedUIDReadToken {
		t.Fatal("shared UID read tokens must default off")
	}
}
