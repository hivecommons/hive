package github

// #9586 phase 2: in sidecar mode the hive process must hold NO agent token.
// WriteAgentToken records only the agent's tier (which the proxy signs into
// each request) and never mints; the real token exists only inside the
// credential sidecar.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/credsidecar"
)

func resetProxyTierRegistry(t *testing.T) {
	t.Helper()
	agentProxyTiersMu.Lock()
	agentProxyTiers = make(map[string]string)
	agentProxyTiersMu.Unlock()
	t.Cleanup(func() {
		agentProxyTiersMu.Lock()
		agentProxyTiers = make(map[string]string)
		agentProxyTiersMu.Unlock()
	})
}

// countingAppAuth is newFakeAppAuth with a hit counter on the mint endpoint.
func countingAppAuth(t *testing.T, token string) (*AppAuth, *atomic.Int32, *bytes.Buffer) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      token,
			"expires_at": time.Now().Add(testTokenTTL).Format(time.RFC3339),
		})
	}))
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, testRSABits)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	var logBuf bytes.Buffer
	return &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		apiURL:         server.URL,
	}, &hits, &logBuf
}

func TestWriteAgentToken_SidecarModeHoldsNoToken(t *testing.T) {
	const realToken = "ghs_would-be-real-token"
	t.Setenv(config.ProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOnValue)
	t.Setenv(credsidecar.URLEnv, credsidecar.DefaultURL)
	resetProxyTokenRegistry(t)
	resetProxyTierRegistry(t)
	auth, hits, logs := countingAppAuth(t, realToken)
	dir := useTempCacheDir(t)

	agents := map[string]string{"scanner": "contributor", "guide": "advisor", "merger-bot": "trusted"}
	for name, tier := range agents {
		if err := auth.WriteAgentToken(context.Background(), name, tier, 2001); err != nil {
			t.Fatalf("WriteAgentToken(%s): %v", name, err)
		}
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("the hive process minted %d token(s) in sidecar mode; only the sidecar may mint", n)
	}
	agentProxyTokensMu.RLock()
	registrySize := len(agentProxyTokens)
	agentProxyTokensMu.RUnlock()
	if registrySize != 0 {
		t.Fatalf("in-process token registry holds %d entries in sidecar mode, want 0", registrySize)
	}
	for name, tier := range agents {
		if tok, ok := AgentProxyToken(name); ok || tok != "" {
			t.Fatalf("AgentProxyToken(%s) = (%q, %v), want nothing in sidecar mode", name, tok, ok)
		}
		if got, ok := AgentProxyTier(name); !ok || got != tier {
			t.Fatalf("AgentProxyTier(%s) = (%q, %v), want %q", name, got, ok, tier)
		}
		data, err := os.ReadFile(AgentTokenCachePath(name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != AgentDummyToken(name) {
			t.Fatalf("cache for %s = %q, want the placeholder", name, data)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, _ := os.ReadFile(dir + "/" + e.Name())
		if strings.Contains(string(data), realToken) {
			t.Fatalf("%s holds the real token", e.Name())
		}
	}
	if !strings.Contains(logs.String(), "cred_sidecar=true") {
		t.Fatalf("delivery log does not say sidecar mode:\n%s", logs.String())
	}
}

// Without the sidecar URL, injection keeps its phase-1 behavior: the tier
// registry stays empty and the in-process token registry is fed.
func TestWriteAgentToken_InjectionWithoutSidecarUnchanged(t *testing.T) {
	const realToken = "ghs_real"
	t.Setenv(config.ProxyInjectGHAuthEnv, config.ProxyInjectGHAuthOnValue)
	t.Setenv(credsidecar.URLEnv, "")
	resetProxyTokenRegistry(t)
	resetProxyTierRegistry(t)
	auth, hits, _ := countingAppAuth(t, realToken)
	useTempCacheDir(t)

	if err := auth.WriteAgentToken(context.Background(), "scanner", "contributor", 2001); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("mint hits = %d, want 1", hits.Load())
	}
	if tok, ok := AgentProxyToken("scanner"); !ok || tok != realToken {
		t.Fatalf("in-process registry = (%q, %v)", tok, ok)
	}
	if _, ok := AgentProxyTier("scanner"); ok {
		t.Fatal("tier registry fed without sidecar mode")
	}
}

func TestCredSidecarEnabled_ReadsSharedVariable(t *testing.T) {
	t.Setenv(credsidecar.URLEnv, "")
	if credSidecarEnabled() {
		t.Fatal("enabled with the URL unset")
	}
	t.Setenv(credsidecar.URLEnv, credsidecar.DefaultURL)
	if !credSidecarEnabled() {
		t.Fatal("disabled with the URL set")
	}
	storeAgentProxyTier("x", "")
	if _, ok := AgentProxyTier("x"); ok {
		t.Fatal("an empty tier reported as present")
	}
}
