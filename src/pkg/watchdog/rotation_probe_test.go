package watchdog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/rotation"
)

type scriptedProber struct {
	provider string
	headroom rotation.Headroom
}

func claudeCredentialsFile(t *testing.T, refreshToken string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"stale-token","refreshToken":%q,"expiresAt":%d}}`,
		refreshToken, time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type unauthorizedTransport struct{}

func (unauthorizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody, Header: make(http.Header)}, nil
}

func claudeProberForCredentials(path string) rotation.ClaudeProber {
	return rotation.ClaudeProber{
		ThresholdPct:    80,
		BaseURL:         "https://example.invalid",
		Client:          &http.Client{Transport: unauthorizedTransport{}},
		CredentialsPath: path,
	}
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

func TestRotationAuthProbeExpiredClaudeTokenWithRefreshIsNotFailed(t *testing.T) {
	p := RotationAuthProbe{Prober: claudeProberForCredentials(claudeCredentialsFile(t, "refresh-token"))}
	got, detail := p.ProbeAuth(context.Background())
	if got != AuthUnknown {
		t.Fatalf("ProbeAuth = %s (%s), want %s: Claude can refresh this credential on use", got, detail, AuthUnknown)
	}
	if detail == "" {
		t.Fatal("the inconclusive verdict must explain the refreshable expiry")
	}
}

func TestRotationAuthProbeExpiredClaudeTokenWithoutRefreshStillFails(t *testing.T) {
	p := RotationAuthProbe{Prober: claudeProberForCredentials(claudeCredentialsFile(t, ""))}
	got, detail := p.ProbeAuth(context.Background())
	if got != AuthFailed {
		t.Fatalf("ProbeAuth = %s (%s), want %s: this credential has no recovery grant", got, detail, AuthFailed)
	}
}
