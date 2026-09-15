package config

import "testing"

// TestACMMLevelOrZero pins the nil-safety contract of Config.ACMMLevelOrZero.
// The helper feeds every ioscan FailClosedAtLevel gate (dashboard
// api_governor_security.go, status_builder.go, scheduler ioscan_enforce.go and
// the cmd/hive canary wiring), so "unset means 0" must hold for BOTH a nil
// receiver and a nil ACMMLevel pointer: returning anything else — or panicking —
// would silently change whether ioscan fails closed on a hive that never set
// acmm_level.
func TestACMMLevelOrZero(t *testing.T) {
	t.Run("nil receiver returns 0", func(t *testing.T) {
		var c *Config
		if got := c.ACMMLevelOrZero(); got != 0 {
			t.Fatalf("nil receiver: got %d, want 0", got)
		}
	})

	t.Run("unset level returns 0", func(t *testing.T) {
		c := &Config{}
		if got := c.ACMMLevelOrZero(); got != 0 {
			t.Fatalf("unset ACMMLevel: got %d, want 0", got)
		}
	})

	t.Run("set level is returned verbatim", func(t *testing.T) {
		for _, lvl := range []int{0, 4, 5, 6} {
			lvl := lvl
			c := &Config{ACMMLevel: &lvl}
			if got := c.ACMMLevelOrZero(); got != lvl {
				t.Fatalf("ACMMLevel=%d: got %d", lvl, got)
			}
		}
	})
}
