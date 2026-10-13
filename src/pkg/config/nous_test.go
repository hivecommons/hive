package config

import (
	"testing"
	"time"
)

func TestNousGatePollInterval(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		yaml int
		want time.Duration
	}{
		{"default", "", 0, DefaultNousGatePollInterval},
		{"yaml", "", 45, 45 * time.Second},
		{"negative yaml falls back", "", -3, DefaultNousGatePollInterval},
		{"env duration wins", "2m", 45, 2 * time.Minute},
		{"env seconds wins", "7", 45, 7 * time.Second},
		{"env zero ignored", "0", 45, 45 * time.Second},
		{"env garbage ignored", "soon", 0, DefaultNousGatePollInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(NousGatePollIntervalEnv, tc.env)
			if got := (NousConfig{GatePollSeconds: tc.yaml}).GatePollInterval(); got != tc.want {
				t.Fatalf("GatePollInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}
