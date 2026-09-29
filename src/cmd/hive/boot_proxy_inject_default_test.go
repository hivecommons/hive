package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestBootGitHubResolvesProxyInjectDefault (#9586): bootGitHub - the first
// boot phase, before the proxy or any agent token mint - applies the
// hosted-App default by writing the explicit value into the env, and logs
// exactly one line saying which way it resolved and why. Everything that is
// not a hosted spoke with live App auth, or that carries an explicit value,
// leaves the env alone.
func TestBootGitHubResolvesProxyInjectDefault(t *testing.T) {
	const testAppID = 12345
	cases := []struct {
		name     string
		hiveType string
		appID    int64
		appAuth  bool
		value    string
		advisory string
		hubMode  bool
		wantOn   bool
		wantEnv  string
	}{
		{name: "hosted App unset defaults on", hiveType: config.HiveTypeHosted, appID: testAppID, appAuth: true, wantOn: true, wantEnv: config.ProxyInjectGHAuthOnValue},
		{name: "hosted App explicit false wins", hiveType: config.HiveTypeHosted, appID: testAppID, appAuth: true, value: config.ProxyInjectGHAuthOffValue, wantEnv: config.ProxyInjectGHAuthOffValue},
		{name: "hosted PAT stays off", hiveType: config.HiveTypeHosted},
		{name: "hosted App key not delivered yet stays off", hiveType: config.HiveTypeHosted, appID: testAppID},
		{name: "hosted placeholder app_id stays off", hiveType: config.HiveTypeHosted, appID: config.PlaceholderAppID, appAuth: true},
		{name: "hosted App in advisory mode stays off", hiveType: config.HiveTypeHosted, appID: testAppID, appAuth: true, advisory: "true"},
		{name: "self-hosted App stays off", appID: testAppID, appAuth: true},
		{name: "hub mode stays off", hiveType: config.HiveTypeHosted, appID: testAppID, appAuth: true, hubMode: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Hub.HiveType = tc.hiveType
			cfg.GitHub.AppID = tc.appID
			b, log := newDepsTestBoot(t, cfg)

			env := map[string]string{config.ProxyInjectGHAuthEnv: tc.value, config.ProxyAdvisoryOKEnv: tc.advisory}
			if tc.hubMode {
				env["HIVE_MODE"] = "hub"
			}
			b.bootGitHubWith(bootGitHubDeps{
				initGitHubAuth: func(context.Context, *config.Config, *slog.Logger) githubAuth {
					if tc.appAuth {
						return githubAuth{AppAuth: &github.AppAuth{}}
					}
					return githubAuth{}
				},
				getenv: func(k string) string { return env[k] },
				setenv: func(k, v string) error { env[k] = v; return nil },
			})

			if got := env[config.ProxyInjectGHAuthEnv]; got != tc.wantEnv {
				t.Errorf("%s = %q after boot, want %q", config.ProxyInjectGHAuthEnv, got, tc.wantEnv)
			}
			state := "off"
			if tc.wantOn {
				state = "on"
			}
			prefix := "proxy GitHub auth injection: " + state + " ("
			if n := strings.Count(log.String(), "proxy GitHub auth injection: "); n != 1 {
				t.Fatalf("boot logged the injection decision %d times, want 1:\n%s", n, log.String())
			}
			if !strings.Contains(log.String(), prefix) {
				t.Errorf("boot log missing %q:\n%s", prefix, log.String())
			}
			if tc.wantOn && !strings.Contains(log.String(), config.ProxyInjectGHAuthEnv+"="+config.ProxyInjectGHAuthOffValue+" to opt out") {
				t.Errorf("default-on line does not name the opt-out:\n%s", log.String())
			}
		})
	}
}
