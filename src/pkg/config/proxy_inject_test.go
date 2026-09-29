package config

import (
	"errors"
	"fmt"
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

// TestValidateProxyInjectGHAuth pins the #9586 FATAL startup guard: only the
// exploitable combination (injection on + self-asserted identity) refuses to
// boot. Everything else - including an unrecognized value - must boot, so an
// auto-deployed upgrade never crash-loops an existing spoke.
func TestValidateProxyInjectGHAuth(t *testing.T) {
	cases := []struct {
		name     string
		inject   string
		advisory string
		fatal    bool
	}{
		{name: "unset is today's default", inject: "", advisory: ""},
		{name: "explicit opt-out", inject: ProxyInjectGHAuthOffValue, advisory: ""},
		{name: "explicit opt-out with advisory mode", inject: ProxyInjectGHAuthOffValue, advisory: "true"},
		{name: "unset with advisory mode", inject: "", advisory: "true"},
		{name: "enabled", inject: ProxyInjectGHAuthOnValue, advisory: ""},
		{name: "enabled with whitespace", inject: "  true \n", advisory: ""},
		{name: "enabled with advisory explicitly false", inject: ProxyInjectGHAuthOnValue, advisory: "false"},
		{name: "enabled with near-miss advisory value", inject: ProxyInjectGHAuthOnValue, advisory: "TRUE"},
		{name: "unrecognized value is not fatal", inject: "1", advisory: ""},
		{name: "unrecognized value with advisory mode is not fatal (injection is off)", inject: "TRUE", advisory: "true"},
		{name: "enabled with advisory mode", inject: ProxyInjectGHAuthOnValue, advisory: "true", fatal: true},
		{name: "enabled with padded advisory mode", inject: " true ", advisory: " true ", fatal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{ProxyInjectGHAuthEnv: tc.inject, ProxyAdvisoryOKEnv: tc.advisory}
			err := ValidateProxyInjectGHAuth(func(k string) string { return env[k] })
			if !tc.fatal {
				if err != nil {
					t.Fatalf("ValidateProxyInjectGHAuth(%q, advisory=%q) = %v, want nil", tc.inject, tc.advisory, err)
				}
				return
			}
			if !errors.Is(err, ErrProxyInjectGHAuthWithSelfAssertedIdentity) {
				t.Fatalf("ValidateProxyInjectGHAuth(%q, advisory=%q) = %v, want %v", tc.inject, tc.advisory, err, ErrProxyInjectGHAuthWithSelfAssertedIdentity)
			}
			// The error must name the fix: an operator reading a refusing pod's
			// log needs both variables and the opt-out value.
			msg := err.Error()
			for _, want := range []string{ProxyInjectGHAuthEnv, ProxyAdvisoryOKEnv, ProxyInjectGHAuthOffValue} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %q", msg, want)
				}
			}
		})
	}
}

// TestProxyInjectGHAuthWarnings: an unrecognized value is reported (so the
// operator learns the agent still holds its real token) but the reader keeps
// treating it as OFF - today's behavior. Recognized values report nothing.
func TestProxyInjectGHAuthWarnings(t *testing.T) {
	for _, v := range []string{"", " ", ProxyInjectGHAuthOnValue, " true ", ProxyInjectGHAuthOffValue} {
		env := map[string]string{ProxyInjectGHAuthEnv: v}
		if w := ProxyInjectGHAuthWarnings(func(k string) string { return env[k] }); w != nil {
			t.Errorf("recognized value %q produced warnings %v", v, w)
		}
	}
	for _, v := range []string{"1", "TRUE", "True", "yes", "on", "0", "ture", "False"} {
		env := map[string]string{ProxyInjectGHAuthEnv: v}
		w := ProxyInjectGHAuthWarnings(func(k string) string { return env[k] })
		if len(w) != 1 {
			t.Fatalf("unrecognized value %q produced %d warnings, want 1: %v", v, len(w), w)
		}
		for _, want := range []string{ProxyInjectGHAuthEnv, strconvQuote(v), "OFF", ProxyInjectGHAuthOnValue, ProxyInjectGHAuthOffValue} {
			if !strings.Contains(w[0], want) {
				t.Errorf("warning %q does not mention %q", w[0], want)
			}
		}
		// Non-fatal means the value keeps today's meaning: off.
		t.Setenv(ProxyInjectGHAuthEnv, v)
		if ProxyInjectGHAuth() {
			t.Errorf("unrecognized value %q enabled injection; it must stay off", v)
		}
	}
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
