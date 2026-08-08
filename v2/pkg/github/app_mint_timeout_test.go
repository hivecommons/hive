package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests lock in the #2439 root-cause fix: a GitHub App installation-token
// mint against an unreachable / firewalled GHE endpoint (github.ibm.com with no
// installation) must NOT hang. It used to run on a bare gh.NewClient(nil) (no
// per-client Timeout) and was sometimes reached with an undeadlined process
// context — so a stuck TCP/TLS connect blocked the caller for minutes. That
// froze the first heartbeat attempt and readiness, crash-looping the pod and
// wedging the hub at "Upgrading". Every mint is now bounded by tokenMintTimeout
// end to end and returns a soft error the caller treats as "App not usable".

// blockingMintServer returns a handler that sleeps past tokenMintTimeout before
// (never) replying, simulating an endpoint that accepts the connection but never
// answers — the exact hang the fix must bound.
func blockingMintServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep past the mint timeout so the client gives up first, but bail the
		// moment the request context is cancelled (client hangup / test teardown)
		// so server Close() — which waits for in-flight handlers — returns
		// promptly and the test stays fast.
		select {
		case <-r.Context().Done():
		case <-time.After(tokenMintTimeout + 2*time.Second):
		}
	}))
}

// TestMintInstallationToken_HungEndpointReturnsBoundedError proves a hung mint
// endpoint yields an error within a small multiple of tokenMintTimeout even
// when the caller passes context.Background() (no deadline of its own).
func TestMintInstallationToken_HungEndpointReturnsBoundedError(t *testing.T) {
	key, _ := generateTestKey(t)
	server := blockingMintServer(t)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
	}

	start := time.Now()
	// Deliberately an undeadlined context: the mint's own bound must apply.
	_, _, err := auth.mintInstallationToken(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("mintInstallationToken returned nil error against a hung endpoint; expected a bounded timeout error")
	}
	// Allow generous slack over tokenMintTimeout for CI scheduling, but it MUST
	// be far under the server's 3x sleep — proving the client, not the server,
	// ended the call.
	if elapsed > tokenMintTimeout*2 {
		t.Fatalf("mintInstallationToken took %v, want < %v (the mint did not honor its bound)", elapsed, tokenMintTimeout*2)
	}
}

// TestToken_HungEndpointNoCacheReturnsBoundedError proves the public Token()
// path (used by appTransport.RoundTrip → EnumerateActionable, the heartbeat/
// eval loops) also stays bounded: with no cached token and a hung endpoint it
// errors quickly instead of hanging the loop.
func TestToken_HungEndpointNoCacheReturnsBoundedError(t *testing.T) {
	key, _ := generateTestKey(t)
	server := blockingMintServer(t)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		// No cached token: Token() must attempt a live mint, which hangs.
	}

	start := time.Now()
	_, err := auth.Token(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Token() returned nil error against a hung endpoint with no cache; expected a bounded error")
	}
	if elapsed > tokenMintTimeout*2 {
		t.Fatalf("Token() took %v, want < %v (mint did not honor its bound)", elapsed, tokenMintTimeout*2)
	}
}

// TestVerifyInstallation_HungEndpointReturnsBoundedError proves the
// installation-probe path (VerifyInstallation, used by the self-heal / re-check
// flows) is bounded too.
func TestVerifyInstallation_HungEndpointReturnsBoundedError(t *testing.T) {
	key, _ := generateTestKey(t)
	server := blockingMintServer(t)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
	}

	start := time.Now()
	_, err := auth.VerifyInstallation(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("VerifyInstallation returned nil error against a hung endpoint; expected a bounded error")
	}
	if elapsed > tokenMintTimeout*2 {
		t.Fatalf("VerifyInstallation took %v, want < %v (probe did not honor its bound)", elapsed, tokenMintTimeout*2)
	}
}

// TestScopedToken_HungEndpointReturnsBoundedError proves the per-agent scoped
// token mint (WriteAgentToken → ScopedToken, on the agent-launch path) is
// bounded, so a GHE stall cannot wedge agent startup either.
func TestScopedToken_HungEndpointReturnsBoundedError(t *testing.T) {
	key, _ := generateTestKey(t)
	server := blockingMintServer(t)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
	}

	start := time.Now()
	_, err := auth.ScopedToken(context.Background(), "contributor")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ScopedToken returned nil error against a hung endpoint; expected a bounded error")
	}
	if elapsed > tokenMintTimeout*2 {
		t.Fatalf("ScopedToken took %v, want < %v (mint did not honor its bound)", elapsed, tokenMintTimeout*2)
	}
}
