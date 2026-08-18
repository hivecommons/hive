package config

import "testing"

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
