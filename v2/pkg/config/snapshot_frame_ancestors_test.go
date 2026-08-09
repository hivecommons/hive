package config

import "testing"

func TestValidateSnapshotFrameAncestors(t *testing.T) {
	got, err := ValidateSnapshotFrameAncestors([]string{
		" https://docs.projectbluefin.io ",
		"https://docs.projectbluefin.io",
		"https://example.com:8443",
		"",
	})
	if err != nil {
		t.Fatalf("ValidateSnapshotFrameAncestors returned error: %v", err)
	}
	want := []string{"https://docs.projectbluefin.io", "https://example.com:8443"}
	if len(got) != len(want) {
		t.Fatalf("normalized origins = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalized origins = %#v, want %#v", got, want)
		}
	}
}

func TestValidateSnapshotFrameAncestorsRejectsUnsafeOrigins(t *testing.T) {
	tests := []string{
		"http://docs.projectbluefin.io",
		"https://docs.projectbluefin.io/factory",
		"https://*.example.com",
		"not a url",
		"https://user:pass@example.com",
		"https://x'-alert(1)-'",
		"https://bad_host.example.com",
		"https://example.com:99999",
	}
	for _, origin := range tests {
		t.Run(origin, func(t *testing.T) {
			if _, err := ValidateSnapshotFrameAncestors([]string{origin}); err == nil {
				t.Fatalf("expected %q to be rejected", origin)
			}
		})
	}
}

func TestDashboardConfigSnapshotFrameAncestorsCSP(t *testing.T) {
	if got := (DashboardConfig{}).SnapshotFrameAncestorsCSP(); got != "'none'" {
		t.Fatalf("empty allowlist CSP = %q, want 'none'", got)
	}
	cfg := DashboardConfig{SnapshotFrameAncestors: []string{"https://docs.projectbluefin.io"}}
	if got := cfg.SnapshotFrameAncestorsCSP(); got != "https://docs.projectbluefin.io" {
		t.Fatalf("allowlist CSP = %q", got)
	}
}
