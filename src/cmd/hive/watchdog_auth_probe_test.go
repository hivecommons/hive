package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/crux/watchdog"
)

// These two tests used to live in pkg/watchdog against rotation.ClaudeProber
// directly; they moved here with the usageProber adapter, since the watchdog
// no longer imports pkg/rotation (hivecommons/hive#13536).

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

func claudeAuthProbeForCredentials(path string) watchdog.RotationAuthProbe {
	return watchdog.RotationAuthProbe{Prober: usageProber{rotation.ClaudeProber{
		ThresholdPct:    80,
		BaseURL:         "https://example.invalid",
		Client:          &http.Client{Transport: unauthorizedTransport{}},
		CredentialsPath: path,
	}}}
}

func TestRotationAuthProbeExpiredClaudeTokenWithRefreshIsNotFailed(t *testing.T) {
	p := claudeAuthProbeForCredentials(claudeCredentialsFile(t, "refresh-token"))
	if p.Provider() != "anthropic" {
		t.Fatalf("provider = %q", p.Provider())
	}
	got, detail := p.ProbeAuth(context.Background())
	if got != watchdog.AuthUnknown {
		t.Fatalf("ProbeAuth = %s (%s), want %s: Claude can refresh this credential on use", got, detail, watchdog.AuthUnknown)
	}
	if detail == "" {
		t.Fatal("the inconclusive verdict must explain the refreshable expiry")
	}
}

func TestRotationAuthProbeExpiredClaudeTokenWithoutRefreshStillFails(t *testing.T) {
	p := claudeAuthProbeForCredentials(claudeCredentialsFile(t, ""))
	got, detail := p.ProbeAuth(context.Background())
	if got != watchdog.AuthFailed {
		t.Fatalf("ProbeAuth = %s (%s), want %s: this credential has no recovery grant", got, detail, watchdog.AuthFailed)
	}
}
