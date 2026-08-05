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
