package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGatewayUpsertRefreshesLiveInferenceRoutes(t *testing.T) {
	s, deps := apiServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer upstream.Close()

	defer setGatewaySecretsDirForTest(t.TempDir())()
	if err := deps.AgentMgr.SetBackendOverride("scanner", "litellm"); err != nil {
		t.Fatal(err)
	}
	if err := deps.AgentMgr.SetModelOverride("scanner", "m"); err != nil {
		t.Fatal(err)
	}

	refreshed := false
	deps.AgentMgr.SetInferenceCallbacks(func(name, backend, model string) {
		if name != "scanner" || backend != "litellm" || model != "m" {
			t.Fatalf("refresh callback = (%q,%q,%q), want scanner/litellm/m", name, backend, model)
		}
		refreshed = true
	}, func(string) {})

	rec := doPut(s, "/api/config/governor/gateways", map[string]interface{}{
		"name":     "litellm",
		"kind":     "litellm",
		"endpoint": upstream.URL,
		"api_key":  "sk-new-route-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert = %d: %s", rec.Code, rec.Body.String())
	}
	if !refreshed {
		t.Fatal("gateway key save did not refresh live litellm inference routes; the next kick would keep using the old proxy route key")
	}
}

func TestGatewayAndLiteLLMResponsesExposeKeySHA256(t *testing.T) {
	s, deps := apiServer(t)
	defer setGatewaySecretsDirForTest(t.TempDir())()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer upstream.Close()

	key := "sk-hash-visible"
	sum := sha256.Sum256([]byte(key))
	want := hex.EncodeToString(sum[:])
	path, err := s.storeGatewayAPIKey("litellm", key)
	if err != nil {
		t.Fatal(err)
	}

	gwResp := gatewaySectionResponse(config.GatewayConfig{Name: "litellm", Kind: config.GatewayKindLiteLLM, Endpoint: upstream.URL, APIKeyFile: path})
	if got := gwResp["keySHA256"]; got != want {
		t.Fatalf("gateway keySHA256 = %v, want %s", got, want)
	}
	if _, leaked := gwResp["api_key"]; leaked {
		t.Fatal("gateway response must never include the raw key")
	}
	if _, leaked := gwResp["keyHint"]; leaked {
		t.Fatal("gateway response must not include partial key material; use keySHA256 for correlation")
	}

	deps.Config.Governor.LiteLLM.Endpoint = upstream.URL
	deps.Config.Governor.LiteLLM.APIKeyFile = path
	llmResp := litellmSectionResponse(&deps.Config.Governor.LiteLLM)
	if got := llmResp["keySHA256"]; got != want {
		t.Fatalf("litellm keySHA256 = %v, want %s", got, want)
	}
	if _, leaked := llmResp["keyHint"]; leaked {
		t.Fatal("litellm response must not include partial key material; use keySHA256 for correlation")
	}

	s.UpdateInferenceEndpoint("litellm", []string{upstream.URL})
	backends := s.buildInferenceBackends()
	found := false
	for _, backend := range backends {
		if backend.ID == "litellm" {
			found = true
			if backend.KeySHA256 != want {
				t.Fatalf("status litellm keySHA256 = %q, want %s", backend.KeySHA256, want)
			}
		}
	}
	if !found {
		t.Fatal("status inferenceBackends did not include configured litellm backend")
	}
}
