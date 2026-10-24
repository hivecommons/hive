package watchdog

import (
	"context"
	"testing"
)

type scriptedProber struct {
	provider string
	probeErr string
}

func (p scriptedProber) Provider() string                  { return p.provider }
func (p scriptedProber) ProbeError(context.Context) string { return p.probeErr }

func TestRotationAuthProbeVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		probeErr string
		want     AuthStatus
	}{
		{"clean answer proves the credential", "", AuthOK},
		{"401 in the error is an auth failure", "claude probe failed: HTTP 401", AuthFailed},
		{"login-expired chrome is an auth failure", "output: ● Login Expired · please run /login", AuthFailed},
		{"unauthorized is an auth failure", "Unauthorized: token rejected", AuthFailed},
		{"invalid api key is an auth failure", "Invalid API key provided", AuthFailed},
		{"transport error is honestly unknown", "dial tcp: connection refused", AuthUnknown},
		{"parse error is honestly unknown", "claude usage output did not match", AuthUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := RotationAuthProbe{Prober: scriptedProber{provider: "anthropic", probeErr: tc.probeErr}}
			if p.Provider() != "anthropic" {
				t.Fatal("provider must pass through")
			}
			got, detail := p.ProbeAuth(context.Background())
			if got != tc.want {
				t.Fatalf("ProbeAuth = %s (%s), want %s", got, detail, tc.want)
			}
			if detail == "" {
				t.Fatal("every verdict must carry its evidence")
			}
		})
	}
}
