package config

import "testing"

// TestDebugConfig_EffectivePprofAddr pins the resolution rules for the
// `debug.pprof_addr` knob (hivecommons/hive#14681): unset → loopback default,
// "off" (any case, surrounding whitespace) → disabled, anything else verbatim.
func TestDebugConfig_EffectivePprofAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		addr string
		want string
	}{
		{name: "unset uses loopback default", addr: "", want: DefaultPprofAddr},
		{name: "whitespace only uses loopback default", addr: "  \t", want: DefaultPprofAddr},
		{name: "off disables", addr: PprofAddrOff, want: ""},
		{name: "off is case-insensitive", addr: "OFF", want: ""},
		{name: "off tolerates surrounding whitespace", addr: " off ", want: ""},
		{name: "explicit address is trimmed and kept", addr: " 127.0.0.1:7070 ", want: "127.0.0.1:7070"},
		{name: "ipv6 loopback kept verbatim", addr: "[::1]:6060", want: "[::1]:6060"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DebugConfig{PprofAddr: tc.addr}.EffectivePprofAddr()
			if got != tc.want {
				t.Fatalf("DebugConfig{PprofAddr:%q}.EffectivePprofAddr() = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestDebugConfig_DefaultIsLoopback guards the security property documented
// on DefaultPprofAddr: the default listener must never bind a non-loopback
// host, because pprof profiles leak source paths and heap contents.
func TestDebugConfig_DefaultIsLoopback(t *testing.T) {
	t.Parallel()
	if DefaultPprofAddr != "127.0.0.1:6060" {
		t.Fatalf("DefaultPprofAddr = %q, want loopback 127.0.0.1:6060", DefaultPprofAddr)
	}
	var cfg Config
	if got := cfg.Debug.EffectivePprofAddr(); got != DefaultPprofAddr {
		t.Fatalf("zero Config.Debug should resolve to DefaultPprofAddr, got %q", got)
	}
}
