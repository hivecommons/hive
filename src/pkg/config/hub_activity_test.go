package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHubActivityEnabledDefaults(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		hub  HubConfig
		want bool
	}{
		{"self-hosted defaults off", HubConfig{}, false},
		{"local hive type defaults off", HubConfig{HiveType: "local"}, false},
		{"hosted spoke defaults on", HubConfig{HiveType: HiveTypeHosted}, true},
		{"hosted match is case and space insensitive", HubConfig{HiveType: " Hosted "}, true},
		{"hosted withholding repos defaults off", HubConfig{HiveType: HiveTypeHosted, HeartbeatOmit: []string{"users", "repos"}}, false},
		{"hosted withholding other classes stays on", HubConfig{HiveType: HiveTypeHosted, HeartbeatOmit: []string{"users", "task_titles"}}, true},
		{"hosted with an invalid omit list fails closed", HubConfig{HiveType: HiveTypeHosted, HeartbeatOmit: []string{"bogus"}}, false},
		{"self-hosted explicit opt-in", HubConfig{Activity: HubActivityConfig{Enabled: &on}}, true},
		{"hosted explicit opt-out", HubConfig{HiveType: HiveTypeHosted, Activity: HubActivityConfig{Enabled: &off}}, false},
		{"explicit opt-in wins over withheld repos", HubConfig{HeartbeatOmit: []string{"repos"}, Activity: HubActivityConfig{Enabled: &on}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hub.ActivityEnabled(); got != tc.want {
				t.Fatalf("ActivityEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHubActivityConfigYAMLKeys(t *testing.T) {
	var cfg Config
	src := "hub:\n  activity:\n    enabled: true\n    max_items: 40\n    share_private: true\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	a := cfg.Hub.Activity
	if a.Enabled == nil || !*a.Enabled || a.MaxItems != 40 || !a.SharePrivate {
		t.Fatalf("hub.activity decoded as %+v", a)
	}

	var empty Config
	if err := yaml.Unmarshal([]byte("hub:\n  url: https://hub.example.test\n"), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Hub.Activity.Enabled != nil || empty.Hub.Activity.MaxItems != 0 || empty.Hub.Activity.SharePrivate {
		t.Fatalf("absent hub.activity must stay unset, got %+v", empty.Hub.Activity)
	}
}
