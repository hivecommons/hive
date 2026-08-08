package config

import "testing"

// TestIoscanConfig_IsEnabled covers the F11 tri-state default-on contract:
// a nil Enabled pointer (an omitted `enabled:` key, or a zero-valued
// IoscanConfig) means scanning is ON, while an explicit false opts out.
func TestIoscanConfig_IsEnabled(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil defaults on", nil, true},
		{"explicit true", &on, true},
		{"explicit false opts out", &off, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{Enabled: tc.in}
			if got := c.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIoscanConfig_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"empty defaults open", "", false},
		{"open", "open", false},
		{"closed", "closed", true},
		{"closed case insensitive", "CLOSED", true},
		{"unknown stays open", "strict", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{FailMode: tc.mode}
			if got := c.FailClosed(); got != tc.want {
				t.Fatalf("FailClosed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIoscanConfig_CanariesDefaultOff(t *testing.T) {
	var c IoscanConfig
	if c.Canaries {
		t.Fatal("canaries must default off")
	}
}
