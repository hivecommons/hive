package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestBootConfigLogsProxyInjectOptIn (#9586): the spoke boot logs exactly one
// line saying which way proxy GitHub auth injection resolved. Injection is
// opt-in only: unset is off (naming the opt-in), only the explicit value is on.
func TestBootConfigLogsProxyInjectOptIn(t *testing.T) {
	cases := []struct {
		value  string
		wantOn bool
	}{
		{value: ""},
		{value: config.ProxyInjectGHAuthOffValue},
		{value: config.ProxyInjectGHAuthOnValue, wantOn: true},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			f := newBootConfigFake(t)
			f.env[config.ProxyInjectGHAuthEnv] = tc.value
			if returned, ok, code := runBootConfig(t, &boot{}, f.deps); !returned || !ok {
				t.Fatalf("returned=%v ok=%v code=%d, want a normal boot", returned, ok, code)
			}
			log := f.log.String()
			if n := strings.Count(log, "proxy GitHub auth injection: "); n != 1 {
				t.Fatalf("boot logged the injection decision %d times, want 1:\n%s", n, log)
			}
			state := "off"
			if tc.wantOn {
				state = "on"
			}
			if !strings.Contains(log, "proxy GitHub auth injection: "+state+" (") {
				t.Errorf("boot log does not say injection is %s:\n%s", state, log)
			}
			if tc.value == "" && !strings.Contains(log, config.ProxyInjectGHAuthOptInReason) {
				t.Errorf("unset boot line does not name the opt-in %q:\n%s", config.ProxyInjectGHAuthOptInReason, log)
			}
			if got := f.env[config.ProxyInjectGHAuthEnv]; got != tc.value {
				t.Errorf("%s = %q after boot, want it untouched (%q)", config.ProxyInjectGHAuthEnv, got, tc.value)
			}
		})
	}
}

// TestBootGitHubDoesNotDefaultProxyInjectOn (#9586): the reverted #9625
// default turned injection on during bootGitHub for a hosted spoke with live
// GitHub App auth by writing HIVE_PROXY_INJECT_GH_AUTH=true into the process
// env. That spoke must now boot with the variable still unset and injection
// off.
func TestBootGitHubDoesNotDefaultProxyInjectOn(t *testing.T) {
	const testAppID = 12345
	t.Setenv(config.ProxyInjectGHAuthEnv, "")
	if err := os.Unsetenv(config.ProxyInjectGHAuthEnv); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}

	cfg := &config.Config{}
	cfg.Hub.HiveType = config.HiveTypeHosted
	cfg.GitHub.AppID = testAppID
	b, _ := newDepsTestBoot(t, cfg)
	b.bootGitHubWith(bootGitHubDeps{
		initGitHubAuth: func(context.Context, *config.Config, *slog.Logger) githubAuth {
			return githubAuth{AppAuth: &github.AppAuth{}}
		},
	})

	if v, ok := os.LookupEnv(config.ProxyInjectGHAuthEnv); ok {
		t.Fatalf("hosted App spoke boot wrote %s=%q; injection must stay opt-in", config.ProxyInjectGHAuthEnv, v)
	}
	if config.ProxyInjectGHAuth() {
		t.Fatal("hosted App spoke resolved injection ON with the variable unset")
	}
}
