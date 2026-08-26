package watchdog

import (
	"context"
	"errors"
	"testing"

	"github.com/kubestellar/hive/pkg/rotation"
)

type scriptedProber struct {
	provider string
	headroom rotation.Headroom
}

func (p scriptedProber) Provider() string { return p.provider }
func (p scriptedProber) Probe(ctx context.Context) rotation.Headroom {
	return p.headroom
}

func TestRotationAuthProbeVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		probeErr error
		want     AuthStatus
	}{
		{"clean answer proves the credential", nil, AuthOK},
		{"401 in the error is an auth failure", errors.New("claude probe failed: HTTP 401"), AuthFailed},
		{"login-expired chrome is an auth failure", errors.New("output: ● Login Expired · please run /login"), AuthFailed},
		{"unauthorized is an auth failure", errors.New("Unauthorized: token rejected"), AuthFailed},
		{"invalid api key is an auth failure", errors.New("Invalid API key provided"), AuthFailed},
		{"transport error is honestly unknown", errors.New("dial tcp: connection refused"), AuthUnknown},
		{"parse error is honestly unknown", errors.New("claude usage output did not match"), AuthUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := RotationAuthProbe{Prober: scriptedProber{
				provider: "anthropic",
				headroom: rotation.Headroom{Provider: "anthropic", Available: true, ProbeErr: tc.probeErr},
			}}
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
