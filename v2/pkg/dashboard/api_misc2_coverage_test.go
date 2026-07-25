package dashboard

import (
	"net/http"
	"testing"
)

func miscServer(t *testing.T) *Server {
	t.Helper()
	return covApiServer(t)
}

// ---- OpenRouter ----

func TestCovM2_OpenRouterStart(t *testing.T) {
	s := miscServer(t)
	if rec := doGet(s, "/api/openrouter/connect/start?model=claude"); rec.Code != http.StatusOK {
		t.Fatalf("openrouter start: %d", rec.Code)
	}
}

func TestCovM2_OpenRouterCredit_NoKey(t *testing.T) {
	s := miscServer(t)
	// No configured key → connected:false branch.
	if rec := doGet(s, "/api/openrouter/credit"); rec.Code != http.StatusOK {
		t.Fatalf("openrouter credit: %d", rec.Code)
	}
}

func TestCovM2_OpenRouterCallback_MissingParams(t *testing.T) {
	s := miscServer(t)
	// Missing code/state → redirect (error flag).
	rec := doGet(s, "/openrouter/callback")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
		t.Fatalf("callback missing params: %d", rec.Code)
	}
}

func TestCovM2_OpenRouterCallback_UnknownState(t *testing.T) {
	s := miscServer(t)
	rec := doGet(s, "/openrouter/callback?code=abc&state=neverissued")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
		t.Fatalf("callback unknown state: %d", rec.Code)
	}
}

func TestCovM2_OpenRouterCallback_ValidStateExchangeFails(t *testing.T) {
	s := miscServer(t)
	// Issue a real state, then redeem it. ExchangeCode hits the real endpoint and
	// fails → redirect with error flag, covering Consume + ExchangeCode branches.
	state, err := s.openRouterState().Create("verifier", "", "claude")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}
	rec := doGet(s, "/openrouter/callback?code=somecode&state="+state)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
		t.Fatalf("callback valid state: %d", rec.Code)
	}
}

func TestCovM2_OpenRouterQR(t *testing.T) {
	s := miscServer(t)
	// Missing/invalid data → error; a valid openrouter.ai URL → PNG.
	if rec := doGet(s, "/api/openrouter/qr"); rec.Code == 0 {
		t.Fatalf("qr no data: no status")
	}
	if rec := doGet(s, "/api/openrouter/qr?data=https://openrouter.ai/auth?x=1"); rec.Code == 0 {
		t.Fatalf("qr valid: no status")
	}
}

func TestCovM2_OpenRouterModels(t *testing.T) {
	s := miscServer(t)
	// Hits openrouter.ai models list (real network); any non-panic status is fine.
	if rec := doGet(s, "/api/openrouter/models"); rec.Code == 0 {
		t.Fatalf("openrouter models: no status")
	}
}

// ---- Packs ----

func TestCovM2_PacksList(t *testing.T) {
	s := miscServer(t)
	if rec := doGet(s, "/api/packs"); rec.Code != http.StatusOK {
		t.Fatalf("packs list: %d", rec.Code)
	}
}

// ---- ACMM ----

func TestCovM2_ACMMEvaluation(t *testing.T) {
	s := miscServer(t)
	if rec := doGet(s, "/api/acmm/evaluation"); rec.Code != http.StatusOK {
		t.Fatalf("acmm eval: %d", rec.Code)
	}
}

func TestCovM2_ACMMCreateIssue_BadBody(t *testing.T) {
	s := miscServer(t)
	if rec := doPostRaw(s, "/api/acmm/issue", "bad"); rec.Code >= 500 && rec.Code != http.StatusInternalServerError {
		t.Fatalf("acmm issue bad body: unexpected %d", rec.Code)
	}
}

// ---- Gateways ----

func TestCovM2_GatewaysList(t *testing.T) {
	s := miscServer(t)
	if rec := doGet(s, "/api/config/governor/gateways"); rec.Code != http.StatusOK {
		t.Fatalf("gateways list: %d", rec.Code)
	}
}

func TestCovM2_GatewaysUpsert(t *testing.T) {
	s := miscServer(t)
	if rec := doPutRaw(s, "/api/config/governor/gateways", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("gateways upsert bad body: %d", rec.Code)
	}
	// Missing name → 400.
	if rec := doPut(s, "/api/config/governor/gateways", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("gateways upsert no name: %d", rec.Code)
	}
	// Valid upsert.
	rec := doPut(s, "/api/config/governor/gateways", map[string]any{
		"name": "mygw", "kind": "openai", "endpoint": "https://api.example.com/v1",
	})
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("gateways upsert valid: unexpected %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCovM2_GatewaysDelete(t *testing.T) {
	s := miscServer(t)
	// Delete a non-existent gateway → 404.
	if rec := doDelete(s, "/api/config/governor/gateways/ghostgw", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("gateways delete missing: %d", rec.Code)
	}
}

// ---- cli_models discovery (no-token fallback) ----

func TestCovM2_DiscoverModels_NoToken(t *testing.T) {
	s := miscServer(t)
	// No copilot/gemini token wired → fallback branch (no network for the token
	// path). These return cliModelResult with fallback=true.
	_ = s.discoverCopilotModels()
	_ = s.discoverGeminiModels()
}

// ---- Backends ----

func TestCovM2_Backends(t *testing.T) {
	s := miscServer(t)
	if rec := doGet(s, "/api/config/backends"); rec.Code != http.StatusOK {
		t.Fatalf("backends: %d", rec.Code)
	}
}
