package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubNotificationsConfig(t *testing.T) {
	for mode, quiet := range map[string]bool{"": true, "quiet": true, "Quiet": true, "normal": false, "NORMAL": false} {
		c := GitHubNotificationsConfig{Mode: mode}
		if !c.Valid() {
			t.Errorf("mode %q must be valid", mode)
		}
		if c.Quiet() != quiet {
			t.Errorf("mode %q: Quiet() = %v, want %v", mode, c.Quiet(), quiet)
		}
	}
	if (GitHubNotificationsConfig{Mode: "loud"}).Valid() {
		t.Error("unknown mode must be invalid")
	}
}

func TestGitHubNotificationsShadowDefaultsOff(t *testing.T) {
	if (GitHubNotificationsConfig{}).Shadow {
		t.Fatal("github.notifications.shadow must default to off")
	}
}

func TestGitHubNotificationsAttentionPilotDefaultsOff(t *testing.T) {
	if (GitHubNotificationsConfig{}).AttentionPilot {
		t.Fatal("github.notifications.attention_pilot must default to off")
	}
	var c struct {
		GitHub struct {
			Notifications GitHubNotificationsConfig `yaml:"notifications"`
		} `yaml:"github"`
	}
	if err := yaml.Unmarshal([]byte("github:\n  notifications:\n    attention_pilot: true\n"), &c); err != nil {
		t.Fatal(err)
	}
	if !c.GitHub.Notifications.AttentionPilot {
		t.Fatal("github.notifications.attention_pilot: true must parse as on")
	}
}
