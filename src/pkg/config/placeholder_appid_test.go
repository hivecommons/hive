package config

import "testing"

// TestHasAppRejectsPlaceholder pins the distinction the whole crashloop fix
// rests on. A bare `AppID != 0` cannot tell a real App from the placeholder
// sentinel, because the sentinel is non-zero — that is precisely how a hive
// with no GitHub App talked itself into App auth and exited before its HTTP
// listener bound.
func TestHasAppRejectsPlaceholder(t *testing.T) {
	const realAppID int64 = 3568013

	cases := []struct {
		name            string
		appID           int64
		installationID  int64
		wantPlaceholder bool
		wantHasApp      bool
		wantUsable      bool
	}{
		{
			name:  "no app at all",
			appID: 0,
		},
		{
			name:            "placeholder alone — a freshly provisioned pool hive",
			appID:           PlaceholderAppID,
			wantPlaceholder: true,
		},
		{
			// The exact production config that crashlooped: the placeholder was
			// never replaced, but installing the App delivered a real
			// installation_id. Both HasApp and HasUsableApp must stay false.
			name:            "placeholder plus a real installation — the crashloop case",
			appID:           PlaceholderAppID,
			installationID:  149757667,
			wantPlaceholder: true,
		},
		{
			name:       "real app, not yet installed",
			appID:      realAppID,
			wantHasApp: true,
		},
		{
			name:           "real app, installed",
			appID:          realAppID,
			installationID: 149757667,
			wantHasApp:     true,
			wantUsable:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := GitHubConfig{AppID: tc.appID, InstallationID: tc.installationID}

			if got := g.IsPlaceholderApp(); got != tc.wantPlaceholder {
				t.Errorf("IsPlaceholderApp() = %v, want %v", got, tc.wantPlaceholder)
			}
			if got := g.HasApp(); got != tc.wantHasApp {
				t.Errorf("HasApp() = %v, want %v", got, tc.wantHasApp)
			}
			if got := g.HasUsableApp(); got != tc.wantUsable {
				t.Errorf("HasUsableApp() = %v, want %v", got, tc.wantUsable)
			}
		})
	}
}

// TestPlaceholderAppIDSatisfiesValidation locks in the sentinel's reason for
// existing. Config validation demands github.token or github.app_id, so a hive
// awaiting its real App needs *something* in app_id to boot at all — and
// validate() must therefore keep using a bare zero-test rather than HasApp(),
// or every pre-provisioned hive would refuse to start.
func TestPlaceholderAppIDSatisfiesValidation(t *testing.T) {
	newCfg := func(appID int64) *Config {
		c := &Config{}
		c.Project.Org = "jeejz"
		c.GitHub.AppID = appID
		c.Agents = map[string]AgentConfig{
			"guide": {Backend: "claude", Enabled: true},
		}
		return c
	}

	if err := newCfg(PlaceholderAppID).validate(); err != nil {
		t.Fatalf("placeholder app_id must satisfy validation so the hive can boot: %v", err)
	}
	if err := newCfg(0).validate(); err == nil {
		t.Fatal("a config with neither a token nor any app_id must still be rejected")
	}
}
