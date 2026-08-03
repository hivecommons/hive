package config

import "testing"

// TestConfiguredButUninstalled pins the config-truth rule that drives the
// "GitHub App not installed" banner and the github_auth health failure: a REAL
// app_id with no installation must read as uninstalled, while placeholder
// apps, installed apps, and app-less configs must not.
func TestConfiguredButUninstalled(t *testing.T) {
	const realAppID int64 = 123456

	cases := []struct {
		name string
		g    GitHubConfig
		want bool
	}{
		{
			name: "real app with no installation is uninstalled",
			g:    GitHubConfig{AppID: realAppID, InstallationID: 0},
			want: true,
		},
		{
			name: "placeholder app never reads as uninstalled",
			g:    GitHubConfig{AppID: PlaceholderAppID, InstallationID: 0},
			want: false,
		},
		{
			name: "installed app is not uninstalled",
			g:    GitHubConfig{AppID: realAppID, InstallationID: 42980},
			want: false,
		},
		{
			name: "no app configured is not uninstalled",
			g:    GitHubConfig{AppID: 0, InstallationID: 0},
			want: false,
		},
		{
			name: "installation without app is not uninstalled",
			g:    GitHubConfig{AppID: 0, InstallationID: 42980},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.ConfiguredButUninstalled(); got != tc.want {
				t.Errorf("ConfiguredButUninstalled() = %v, want %v (config %+v)", got, tc.want, tc.g)
			}
		})
	}
}
