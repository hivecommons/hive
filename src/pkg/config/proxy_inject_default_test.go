package config

import (
	"os"
	"strings"
	"testing"
)

// TestResolveProxyInjectGHAuthOptInOnly (#9586): injection is opt-in on every
// kind of hive. Only the explicit value turns it on; unset, the explicit
// opt-out and any unrecognized value are off - and the resolution agrees with
// the ProxyInjectGHAuth reader the token-divert path and the proxy use, so no
// consumer can see a different answer.
func TestResolveProxyInjectGHAuthOptInOnly(t *testing.T) {
	cases := []struct {
		value      string
		wantOn     bool
		wantSource ProxyInjectGHAuthSource
	}{
		{value: "", wantSource: ProxyInjectGHAuthSourceDefaultOff},
		{value: "   ", wantSource: ProxyInjectGHAuthSourceDefaultOff},
		{value: ProxyInjectGHAuthOnValue, wantOn: true, wantSource: ProxyInjectGHAuthSourceExplicitOn},
		{value: " true\n", wantOn: true, wantSource: ProxyInjectGHAuthSourceExplicitOn},
		{value: ProxyInjectGHAuthOffValue, wantSource: ProxyInjectGHAuthSourceExplicitOff},
		{value: " false ", wantSource: ProxyInjectGHAuthSourceExplicitOff},
		{value: "TRUE", wantSource: ProxyInjectGHAuthSourceUnrecognized},
		{value: "1", wantSource: ProxyInjectGHAuthSourceUnrecognized},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			env := map[string]string{ProxyInjectGHAuthEnv: tc.value}
			d := ResolveProxyInjectGHAuth(func(k string) string { return env[k] })
			if d.Enabled != tc.wantOn || d.Source != tc.wantSource {
				t.Fatalf("resolved %+v, want enabled=%v source=%s", d, tc.wantOn, tc.wantSource)
			}
			if d.Reason == "" {
				t.Error("decision carries no reason")
			}
			t.Setenv(ProxyInjectGHAuthEnv, tc.value)
			if got := ProxyInjectGHAuth(); got != d.Enabled {
				t.Errorf("ProxyInjectGHAuth() = %v disagrees with resolution %+v", got, d)
			}
		})
	}
}

// TestResolveProxyInjectGHAuthUnsetOffForEveryHiveType: nothing about the
// hive - hosted or self-hosted, hub mode, advisory mode - turns an unset
// variable on. The reverted #9625 default (hosted App spokes) must not return.
func TestResolveProxyInjectGHAuthUnsetOffForEveryHiveType(t *testing.T) {
	for _, hiveType := range []string{HiveTypeHosted, "self-hosted", ""} {
		for _, mode := range []string{"", "hub", "spoke"} {
			for _, advisory := range []string{"", proxyAdvisoryOKEnabledValue} {
				env := map[string]string{"HIVE_MODE": mode, ProxyAdvisoryOKEnv: advisory, "HIVE_TYPE": hiveType}
				d := ResolveProxyInjectGHAuth(func(k string) string { return env[k] })
				if d.Enabled || d.Source != ProxyInjectGHAuthSourceDefaultOff {
					t.Errorf("type=%q mode=%q advisory=%q: unset resolved %+v, want default-off", hiveType, mode, advisory, d)
				}
			}
		}
	}
}

// TestResolveProxyInjectGHAuthNeverWritesEnv: resolving is a pure read; an
// unset variable stays unset in the process env (the reverted default wrote
// "true" into it at boot).
func TestResolveProxyInjectGHAuthNeverWritesEnv(t *testing.T) {
	t.Setenv(ProxyInjectGHAuthEnv, "")
	if err := os.Unsetenv(ProxyInjectGHAuthEnv); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	_ = ResolveProxyInjectGHAuth(os.Getenv)
	if v, ok := os.LookupEnv(ProxyInjectGHAuthEnv); ok {
		t.Fatalf("%s was written (%q) by resolution", ProxyInjectGHAuthEnv, v)
	}
}

// TestProxyInjectGHAuthDecisionLogLine pins the boot line and dashboard text
// an operator reads for the default: off, naming the opt-in.
func TestProxyInjectGHAuthDecisionLogLine(t *testing.T) {
	off := ResolveProxyInjectGHAuth(func(string) string { return "" })
	want := "proxy GitHub auth injection: off (opt-in: set " + ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOnValue + ")"
	if got := off.LogLine(); got != want {
		t.Errorf("LogLine() = %q, want %q", got, want)
	}
	on := ResolveProxyInjectGHAuth(func(k string) string {
		if k == ProxyInjectGHAuthEnv {
			return ProxyInjectGHAuthOnValue
		}
		return ""
	})
	if got := on.LogLine(); !strings.HasPrefix(got, "proxy GitHub auth injection: on (") {
		t.Errorf("LogLine() = %q, want an on line", got)
	}
}
