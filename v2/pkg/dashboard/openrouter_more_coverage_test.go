package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func covK2ORServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// Point the gateway secrets dir at a temp dir so storeGatewayAPIKey (and thus
// upsertOpenRouterGateway / ApplyDeliveredGateway success paths) can write.
func covK2RedirectSecrets(t *testing.T) {
	t.Helper()
	orig := gatewaySecretsDir
	gatewaySecretsDir = t.TempDir()
	t.Cleanup(func() { gatewaySecretsDir = orig })
}

func TestCovK2_UpsertOpenRouterGateway(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)

	// New gateway: writes a key file + appends the gateway to config.
	if err := s.upsertOpenRouterGateway("sk-or-testkey", "openrouter/auto"); err != nil {
		t.Fatalf("upsertOpenRouterGateway (new): %v", err)
	}
	// The gateway now resolves and the key is readable via the secret file.
	if key := s.resolveOpenRouterKey(); key != "sk-or-testkey" {
		t.Fatalf("resolveOpenRouterKey = %q, want sk-or-testkey", key)
	}

	// Re-upsert (existing gateway) exercises the in-place replace branch.
	if err := s.upsertOpenRouterGateway("sk-or-testkey2", "anthropic/claude"); err != nil {
		t.Fatalf("upsertOpenRouterGateway (existing): %v", err)
	}
	if key := s.resolveOpenRouterKey(); key != "sk-or-testkey2" {
		t.Fatalf("resolveOpenRouterKey after re-upsert = %q", key)
	}
}

func TestCovK2_ApplyDeliveredGateway(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)

	// Unsupported name → error.
	if err := s.ApplyDeliveredGateway("notopenrouter", "openrouter", "https://x", "m", "k"); err == nil {
		t.Fatalf("expected error for unsupported delivered gateway name")
	}
	// Empty key → error.
	if err := s.ApplyDeliveredGateway(openRouterGatewayName, "openrouter", "https://x", "m", ""); err == nil {
		t.Fatalf("expected error for empty delivered key")
	}
	// Valid delivery → stores the gateway (empty default model falls back).
	if err := s.ApplyDeliveredGateway(openRouterGatewayName, "openrouter", "https://x", "", "sk-delivered"); err != nil {
		t.Fatalf("ApplyDeliveredGateway valid: %v", err)
	}
	if key := s.resolveOpenRouterKey(); key != "sk-delivered" {
		t.Fatalf("resolveOpenRouterKey after delivery = %q", key)
	}
}

func TestCovK2_OpenRouterCredit(t *testing.T) {
	covK2RedirectSecrets(t)
	s := covK2ORServer(t)

	// No gateway configured → connected:false.
	rec := doGet(s, "/api/openrouter/credit")
	if rec.Code != http.StatusOK {
		t.Fatalf("credit no-gateway: %d", rec.Code)
	}

	// With a gateway configured, FetchCredit hits openrouter.ai offline → the
	// "could not read credit" branch (connected:true + error). Still 200.
	if err := s.upsertOpenRouterGateway("sk-or-key", "openrouter/auto"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec = doGet(s, "/api/openrouter/credit")
	if rec.Code != http.StatusOK {
		t.Fatalf("credit with-gateway: %d", rec.Code)
	}
}

func TestCovK2_OpenRouterCallbackFlow(t *testing.T) {
	s := covK2ORServer(t)

	// Missing params → error redirect.
	rec := httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback missing params: expected 302, got %d", rec.Code)
	}

	// Unknown state → error redirect.
	rec = httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=abc&state=unknown", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback unknown state: expected 302, got %d", rec.Code)
	}

	// Create a real state, then hit the callback with it: the code exchange runs
	// (fails offline → error redirect), exercising the Consume + exchange path.
	state, err := s.openRouterState().Create("verifier123", "", "openrouter/auto")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	rec = httptest.NewRecorder()
	s.handleOpenRouterCallback(rec, httptest.NewRequest(http.MethodGet, "/openrouter/callback?code=thecode&state="+state, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("callback with valid state: expected 302, got %d", rec.Code)
	}
}

func TestCovK2_OpenRouterStartAndQR(t *testing.T) {
	s := covK2ORServer(t)

	// Start returns an authorize URL + state.
	rec := doGet(s, "/api/openrouter/connect/start?model=openrouter/auto")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}

	// QR with a bad (non-openrouter) data param → 400.
	rec = doGet(s, "/api/openrouter/qr?data=https://evil.example.com")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("qr bad data: expected 400, got %d", rec.Code)
	}
}
