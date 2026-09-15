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

// TestIoscanConfig_CanariesEnabled covers the flipped default: canaries are ON
// now that CanaryRegistry.Scan is encoding-aware (#6701, #6720). A nil pointer
// (an omitted `canaries:` key, or a zero-valued IoscanConfig) means ON, an
// explicit true stays ON, and — the dangerous case a naive nil-means-true
// implementation swallows — an explicit false still opts OUT.
func TestIoscanConfig_CanariesEnabled(t *testing.T) {
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
			c := IoscanConfig{Canaries: tc.in}
			if got := c.CanariesEnabled(); got != tc.want {
				t.Fatalf("CanariesEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIoscanConfig_FailClosedAtLevel covers the level-conditional default: with
// no explicit fail_mode, the ACMM packs supply closed at L5/L6 (where agents can
// merge) and open at L1-L4. An explicit operator value overrides the level
// default in either direction, at every level.
func TestIoscanConfig_FailClosedAtLevel(t *testing.T) {
	open, closed := "open", "closed"
	cases := []struct {
		name  string
		mode  string
		level int
		want  bool
	}{
		{"default L1 open", "", 1, false},
		{"default L2 open", "", 2, false},
		{"default L3 open", "", 3, false},
		{"default L4 open", "", 4, false},
		{"default L5 closed", "", 5, true},
		{"default L6 closed", "", 6, true},
		{"default unknown level open", "", 0, false},

		{"override closed at L1", closed, 1, true},
		{"override closed at L4", closed, 4, true},
		{"override open at L5", open, 5, false},
		{"override open at L6", open, 6, false},
		{"override closed at L6 stays closed", closed, 6, true},
		{"override open at L1 stays open", open, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := IoscanConfig{FailMode: tc.mode}
			if got := c.FailClosedAtLevel(tc.level); got != tc.want {
				t.Fatalf("FailClosedAtLevel(%d) with mode %q = %v, want %v",
					tc.level, tc.mode, got, tc.want)
			}
		})
	}
}

func TestIoscanClassifierConfig_DefaultOff(t *testing.T) {
	var c IoscanConfig
	if c.Classifier.Enabled {
		t.Fatal("ioscan classifier must default off")
	}
}
