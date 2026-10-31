package config

import "testing"

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
