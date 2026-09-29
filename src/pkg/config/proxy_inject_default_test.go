package config

import (
	"errors"
	"strings"
	"testing"
)

// resetRecordedProxyInjectGHAuth clears the process-level decision so tests
// that record one do not leak it into ProxyInjectGHAuthState elsewhere.
func resetRecordedProxyInjectGHAuth(t *testing.T) {
	t.Helper()
	reset := func() {
		resolvedProxyInjectGHAuthMu.Lock()
		resolvedProxyInjectGHAuth, resolvedProxyInjectGHAuthSet = ProxyInjectGHAuthDecision{}, false
		resolvedProxyInjectGHAuthMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// credentialLane is the GitHub credential shape of a spoke at decision time.
type credentialLane string

const (
	laneApp      credentialLane = "app"
	lanePAT      credentialLane = "pat"
	laneNoAppYet credentialLane = "no-app-yet"
)

// TestResolveProxyInjectGHAuthMatrix (#9586) walks the full cross product
// (value x hive type x credential lane x advisory mode). Explicit values keep
// their meaning everywhere; UNSET is on only for a hosted spoke with live App
// auth outside advisory mode. PAT, not-yet-App, self-hosted and advisory never
// default on, and explicit false always wins.
func TestResolveProxyInjectGHAuthMatrix(t *testing.T) {
	values := []string{"", ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue, "yes-please"}
	hiveTypes := []string{HiveTypeHosted, "self-hosted"}
	lanes := []credentialLane{laneApp, lanePAT, laneNoAppYet}
	advisories := []string{"", "true"}

	for _, value := range values {
		for _, hiveType := range hiveTypes {
			for _, lane := range lanes {
				for _, advisory := range advisories {
					name := "value=" + value + "/type=" + hiveType + "/lane=" + string(lane) + "/advisory=" + advisory
					t.Run(name, func(t *testing.T) {
						env := map[string]string{ProxyInjectGHAuthEnv: value, ProxyAdvisoryOKEnv: advisory}
						d := ResolveProxyInjectGHAuth(func(k string) string { return env[k] }, ProxyInjectGHAuthInputs{
							HiveType: hiveType,
							AppAuth:  lane == laneApp,
						})

						var want bool
						var wantSource ProxyInjectGHAuthSource
						switch value {
						case ProxyInjectGHAuthOnValue:
							want, wantSource = true, ProxyInjectGHAuthSourceExplicitOn
						case ProxyInjectGHAuthOffValue:
							want, wantSource = false, ProxyInjectGHAuthSourceExplicitOff
						case "":
							want = hiveType == HiveTypeHosted && lane == laneApp && advisory != "true"
							wantSource = ProxyInjectGHAuthSourceDefaultOff
							if want {
								wantSource = ProxyInjectGHAuthSourceHostedAppDefault
							}
						default:
							want, wantSource = false, ProxyInjectGHAuthSourceUnrecognized
						}
						if d.Enabled != want || d.Source != wantSource {
							t.Fatalf("resolved %+v, want enabled=%v source=%s", d, want, wantSource)
						}
						if d.Reason == "" {
							t.Error("decision carries no reason")
						}
						// Safety invariants stated independently of the table.
						if value == "" && lane != laneApp && d.Enabled {
							t.Error("a spoke without live App auth defaulted ON")
						}
						if value == "" && advisory == "true" && d.Enabled {
							t.Error("advisory mode defaulted ON")
						}
						if value == ProxyInjectGHAuthOffValue && d.Enabled {
							t.Error("explicit opt-out did not win")
						}
					})
				}
			}
		}
	}
}

// TestResolveProxyInjectGHAuthHubModeUnaffected: the hub process never takes
// the default, even if its config looked like a hosted App spoke.
func TestResolveProxyInjectGHAuthHubModeUnaffected(t *testing.T) {
	d := ResolveProxyInjectGHAuth(func(string) string { return "" }, ProxyInjectGHAuthInputs{
		HubMode: true, HiveType: HiveTypeHosted, AppAuth: true,
	})
	if d.Enabled || d.Source != ProxyInjectGHAuthSourceDefaultOff || !strings.Contains(d.Reason, "hub") {
		t.Fatalf("hub mode resolved %+v, want default-off naming the hub", d)
	}
}

// TestResolveProxyInjectGHAuthTrimsWhitespace: padded values mean what they
// say, matching ProxyInjectGHAuth's trimming.
func TestResolveProxyInjectGHAuthTrimsWhitespace(t *testing.T) {
	in := ProxyInjectGHAuthInputs{HiveType: " " + HiveTypeHosted + " ", AppAuth: true}
	for value, want := range map[string]bool{" false ": false, " true\n": true, "  ": true} {
		env := map[string]string{ProxyInjectGHAuthEnv: value}
		if d := ResolveProxyInjectGHAuth(func(k string) string { return env[k] }, in); d.Enabled != want {
			t.Errorf("value %q resolved %+v, want enabled=%v", value, d, want)
		}
	}
}

// TestApplyProxyInjectGHAuthDefaultWritesEnvOnlyForDefault: the hosted-App
// default is applied by writing the explicit value into the env (so the
// token-divert path, the proxy snapshot and agent subprocesses all agree);
// nothing else ever writes the env.
func TestApplyProxyInjectGHAuthDefaultWritesEnvOnlyForDefault(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		in        ProxyInjectGHAuthInputs
		wantWrite bool
	}{
		{name: "hosted App unset", in: ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted, AppAuth: true}, wantWrite: true},
		{name: "hosted App explicit false", value: ProxyInjectGHAuthOffValue, in: ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted, AppAuth: true}},
		{name: "hosted App explicit true", value: ProxyInjectGHAuthOnValue, in: ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted, AppAuth: true}},
		{name: "hosted PAT unset", in: ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted}},
		{name: "self-hosted App unset", in: ProxyInjectGHAuthInputs{AppAuth: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetRecordedProxyInjectGHAuth(t)
			env := map[string]string{ProxyInjectGHAuthEnv: tc.value}
			var writes int
			setenv := func(k, v string) error {
				writes++
				env[k] = v
				return nil
			}
			d := ApplyProxyInjectGHAuthDefault(func(k string) string { return env[k] }, setenv, tc.in)
			if (writes == 1) != tc.wantWrite || writes > 1 {
				t.Fatalf("setenv called %d times, want write=%v", writes, tc.wantWrite)
			}
			if tc.wantWrite {
				if env[ProxyInjectGHAuthEnv] != ProxyInjectGHAuthOnValue {
					t.Fatalf("env = %q, want %q", env[ProxyInjectGHAuthEnv], ProxyInjectGHAuthOnValue)
				}
				// The existing readers now agree with the decision.
				t.Setenv(ProxyInjectGHAuthEnv, env[ProxyInjectGHAuthEnv])
				if !ProxyInjectGHAuth() {
					t.Error("ProxyInjectGHAuth() disagrees with the applied default")
				}
			}
			if got := ProxyInjectGHAuthState(func(string) string { return "" }); got != d {
				t.Errorf("recorded state %+v, want the applied decision %+v", got, d)
			}
		})
	}
}

// TestApplyProxyInjectGHAuthDefaultSetenvFailureIsNotFatal: if the env cannot
// be written, injection stays off (the readers still see unset) and the
// decision says so - never a boot failure.
func TestApplyProxyInjectGHAuthDefaultSetenvFailureIsNotFatal(t *testing.T) {
	resetRecordedProxyInjectGHAuth(t)
	d := ApplyProxyInjectGHAuthDefault(func(string) string { return "" },
		func(string, string) error { return errors.New("read-only env") },
		ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted, AppAuth: true})
	if d.Enabled || d.Source != ProxyInjectGHAuthSourceDefaultOff || !strings.Contains(d.Reason, "read-only env") {
		t.Fatalf("setenv failure resolved %+v, want default-off carrying the error", d)
	}
}

// TestProxyInjectGHAuthStateFallsBackToEnv: with no recorded decision (hub,
// tests) the state is the explicit env reading, unset meaning off.
func TestProxyInjectGHAuthStateFallsBackToEnv(t *testing.T) {
	resetRecordedProxyInjectGHAuth(t)
	for value, want := range map[string]bool{"": false, ProxyInjectGHAuthOnValue: true, ProxyInjectGHAuthOffValue: false} {
		env := map[string]string{ProxyInjectGHAuthEnv: value}
		if d := ProxyInjectGHAuthState(func(k string) string { return env[k] }); d.Enabled != want {
			t.Errorf("state for %q = %+v, want enabled=%v", value, d, want)
		}
	}
}

// TestProxyInjectGHAuthDecisionLogLine pins the boot line an operator greps
// for, including the opt-out hint on the default-on path.
func TestProxyInjectGHAuthDecisionLogLine(t *testing.T) {
	on := ResolveProxyInjectGHAuth(func(string) string { return "" }, ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted, AppAuth: true})
	want := "proxy GitHub auth injection: on (default for hosted App spokes; set " + ProxyInjectGHAuthEnv + "=" + ProxyInjectGHAuthOffValue + " to opt out)"
	if got := on.LogLine(); got != want {
		t.Errorf("LogLine() = %q, want %q", got, want)
	}
	off := ResolveProxyInjectGHAuth(func(string) string { return "" }, ProxyInjectGHAuthInputs{HiveType: HiveTypeHosted})
	if got := off.LogLine(); !strings.HasPrefix(got, "proxy GitHub auth injection: off (") {
		t.Errorf("LogLine() = %q, want an off line", got)
	}
}
