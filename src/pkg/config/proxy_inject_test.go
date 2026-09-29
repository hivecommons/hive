package config

import (
	"errors"
	"strings"
	"testing"
)

// TestProxyInjectGHAuth: the #1861 injection flag must be a strict opt-in —
// only the exact value "true" (whitespace-trimmed) enables it, so a typo or a
// truthy-looking value fails safe with injection OFF and fleet behavior
// unchanged.
func TestProxyInjectGHAuth(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"true", true},
		{"  true \n", true},
		{"TRUE", false},
		{"1", false},
		{"false", false},
		{"yes", false},
	}
	for _, tc := range cases {
		t.Setenv(ProxyInjectGHAuthEnv, tc.value)
		if got := ProxyInjectGHAuth(); got != tc.want {
			t.Errorf("ProxyInjectGHAuth() with %s=%q = %v, want %v", ProxyInjectGHAuthEnv, tc.value, got, tc.want)
		}
	}
}

// TestValidateProxyInjectGHAuth pins the #9586 startup guard. The invariant is
// that no accepted configuration leaves an operator believing injection is on
// while the process would actually deliver the real token to the agent, and
// that injection is never combined with a self-asserted agent identity.
func TestValidateProxyInjectGHAuth(t *testing.T) {
	cases := []struct {
		name     string
		inject   string
		advisory string
		wantErr  error
	}{
		{name: "unset is today's default", inject: "", advisory: ""},
		{name: "explicit opt-out", inject: ProxyInjectGHAuthOffValue, advisory: ""},
		{name: "explicit opt-out with advisory mode", inject: ProxyInjectGHAuthOffValue, advisory: "true"},
		{name: "unset with advisory mode", inject: "", advisory: "true"},
		{name: "enabled", inject: ProxyInjectGHAuthOnValue, advisory: ""},
		{name: "enabled with whitespace", inject: "  true \n", advisory: ""},
		{name: "enabled with advisory explicitly false", inject: ProxyInjectGHAuthOnValue, advisory: "false"},
		{name: "enabled with near-miss advisory value", inject: ProxyInjectGHAuthOnValue, advisory: "TRUE"},
		{name: "enabled with advisory mode", inject: ProxyInjectGHAuthOnValue, advisory: "true", wantErr: ErrProxyInjectGHAuthWithSelfAssertedIdentity},
		{name: "enabled with padded advisory mode", inject: ProxyInjectGHAuthOnValue, advisory: " true ", wantErr: ErrProxyInjectGHAuthWithSelfAssertedIdentity},
		{name: "uppercase TRUE", inject: "TRUE", wantErr: ErrProxyInjectGHAuthInvalidValue},
		{name: "numeric 1", inject: "1", wantErr: ErrProxyInjectGHAuthInvalidValue},
		{name: "yes", inject: "yes", wantErr: ErrProxyInjectGHAuthInvalidValue},
		{name: "on", inject: "on", wantErr: ErrProxyInjectGHAuthInvalidValue},
		{name: "numeric 0", inject: "0", wantErr: ErrProxyInjectGHAuthInvalidValue},
		{name: "typo", inject: "ture", wantErr: ErrProxyInjectGHAuthInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{ProxyInjectGHAuthEnv: tc.inject, ProxyAdvisoryOKEnv: tc.advisory}
			err := ValidateProxyInjectGHAuth(func(k string) string { return env[k] })
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateProxyInjectGHAuth(%q, advisory=%q) = %v, want nil", tc.inject, tc.advisory, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateProxyInjectGHAuth(%q, advisory=%q) = %v, want %v", tc.inject, tc.advisory, err, tc.wantErr)
			}
			// The error must name the fix, not just the failure: an operator
			// reading a crash-looping pod's log needs the variable and the
			// accepted opt-out value.
			if msg := err.Error(); !strings.Contains(msg, ProxyInjectGHAuthEnv) || !strings.Contains(msg, ProxyInjectGHAuthOffValue) {
				t.Errorf("error %q does not name %s and its opt-out value", msg, ProxyInjectGHAuthEnv)
			}
		})
	}
}

// TestValidateProxyInjectGHAuthAcceptsOnlyWhatTheReaderUnderstands ties the
// guard to the reader: every value the guard accepts must be one whose reader
// result is unambiguous (on for the on value, off for unset and the off
// value). A value the guard accepted but the reader treated as off would be the
// silent fallback the guard exists to prevent.
func TestValidateProxyInjectGHAuthAcceptsOnlyWhatTheReaderUnderstands(t *testing.T) {
	for _, v := range []string{"", " ", ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue, "TRUE", "1", "yes", "False"} {
		env := map[string]string{ProxyInjectGHAuthEnv: v}
		if err := ValidateProxyInjectGHAuth(func(k string) string { return env[k] }); err != nil {
			continue
		}
		t.Setenv(ProxyInjectGHAuthEnv, v)
		wantOn := strings.TrimSpace(v) == ProxyInjectGHAuthOnValue
		if got := ProxyInjectGHAuth(); got != wantOn {
			t.Errorf("guard accepted %q but ProxyInjectGHAuth() = %v, want %v", v, got, wantOn)
		}
	}
}
