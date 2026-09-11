package client

import "testing"

// TestBaseURLReportsResolvedBase pins the accessor the attach path depends on:
// BaseURL() must return exactly the base the client resolved at construction —
// trailing slash trimmed, env override honoured — so the loopback fast-path
// decision and the "which hive is this?" banner cannot disagree with the URL
// the client actually dials.
func TestBaseURLReportsResolvedBase(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://spoke-7.example:3001/")
	c := New()
	if got, want := c.BaseURL(), "http://spoke-7.example:3001"; got != want {
		t.Errorf("BaseURL() = %q, want %q (env override, slash trimmed)", got, want)
	}
}

// TestBaseURLDefaultsWhenEnvUnset pins the default: with no HIVE_DASHBOARD_URL
// the accessor reports DefaultBaseURL, matching what requests will dial.
func TestBaseURLDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv(BaseURLEnv, "")
	if got := New().BaseURL(); got != DefaultBaseURL {
		t.Errorf("BaseURL() with no env = %q, want %q", got, DefaultBaseURL)
	}
}
