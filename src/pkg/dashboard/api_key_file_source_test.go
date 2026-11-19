package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hive#18533: every save path that takes an api_key_file rejects one outside
// the allowed secret dirs with a message that lists the dirs and says to mount
// the Secret there.
func assertKeyFileRejection(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body: %s)", code, body)
	}
	for _, want := range []string{"outside the allowed secret dirs", "mount the Kubernetes Secret"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rejection %q does not contain %q", body, want)
		}
	}
	for _, root := range config.SecretFileRoots() {
		if !strings.Contains(body, root) {
			t.Fatalf("rejection %q does not list allowed dir %q", body, root)
		}
	}
}

func TestHandleGovernorLiteLLM_RejectsOutsideKeyFile(t *testing.T) {
	srv := newFullServer(t)
	body := `{"endpoint": "https://litellm.example.com", "apiKeyFile": "/secrets-cb/litellm_api_key"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/litellm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLiteLLM(w, req)
	assertKeyFileRejection(t, w.Code, w.Body.String())
	if srv.deps.Config.Governor.LiteLLM.APIKeyFile != "" {
		t.Fatal("rejected path was stored")
	}
}

func TestGatewaysUpsert_RejectsOutsideKeyFile(t *testing.T) {
	srv := newFullServer(t)
	body := `{"name":"corp","kind":"litellm","endpoint":"https://openrouter.ai/api/v1","api_key_file":"/secrets-cb/gw_key"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/gateways", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorGatewaysUpsert(w, req)
	assertKeyFileRejection(t, w.Code, w.Body.String())
	if len(srv.deps.Config.Governor.Gateways) != 0 {
		t.Fatal("config mutated on a rejected path")
	}
}

func TestInferenceAuth_RejectsOutsideKeyFileWithRemedy(t *testing.T) {
	s := covApiServer(t)
	rec := doPut(s, "/api/config/governor/inference-auth", map[string]any{
		"vllm": map[string]any{"api_key_file": "/secrets-cb/vllm_key"},
	})
	assertKeyFileRejection(t, rec.Code, rec.Body.String())
}

// TestLiteLLMSectionResponse_EffectiveKeySource: the governor config API shows
// the source actually in use and flags the ignored configured path. Never the
// key itself.
func TestLiteLLMSectionResponse_EffectiveKeySource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetSecretFileRootsForTest(root))
	t.Setenv(config.DefaultLiteLLMAPIKeyEnv, "sk-effective-env-key")

	lc := &config.LiteLLMConfig{APIKeyFile: "/secrets-cb/litellm_api_key"}
	resp := litellmSectionResponse(lc)
	if resp["keySource"] != "env:"+config.DefaultLiteLLMAPIKeyEnv {
		t.Fatalf("keySource = %v", resp["keySource"])
	}
	desc, _ := resp["keySourceDescription"].(string)
	if desc != "key from env "+config.DefaultLiteLLMAPIKeyEnv+" (configured path /secrets-cb/litellm_api_key not allowed)" {
		t.Fatalf("keySourceDescription = %q", desc)
	}
	if resp["apiKeyFileAllowed"] != false {
		t.Fatalf("apiKeyFileAllowed = %v, want false", resp["apiKeyFileAllowed"])
	}
	dirs, _ := resp["allowedKeyFileDirs"].([]string)
	if len(dirs) != 1 || dirs[0] != root {
		t.Fatalf("allowedKeyFileDirs = %v", resp["allowedKeyFileDirs"])
	}
	for k, v := range resp {
		if s, ok := v.(string); ok && strings.Contains(s, "sk-effective-env-key") {
			t.Fatalf("field %q leaked the key", k)
		}
	}

	good := filepath.Join(root, "litellm_api_key")
	if err := os.WriteFile(good, []byte("sk-file-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = litellmSectionResponse(&config.LiteLLMConfig{APIKeyFile: good})
	if resp["keySource"] != "file:"+good || resp["apiKeyFileAllowed"] != true || resp["keySourceDescription"] != "key from "+good {
		t.Fatalf("allowed file response = %v", resp)
	}
}

func TestGatewaySectionResponse_EffectiveKeySource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetSecretFileRootsForTest(root))
	good := filepath.Join(root, "gw_key")
	if err := os.WriteFile(good, []byte("sk-gw-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp := gatewaySectionResponse(config.GatewayConfig{Name: "ok", APIKeyFile: good})
	if resp["keySource"] != "file:"+good || resp["apiKeyFileAllowed"] != true {
		t.Fatalf("allowed gateway response = %v", resp)
	}

	resp = gatewaySectionResponse(config.GatewayConfig{Name: "corp", APIKeyFile: "/secrets-cb/gw_key"})
	if resp["keySource"] != "" || resp["apiKeyFileAllowed"] != false {
		t.Fatalf("outside gateway response = %v", resp)
	}
	if resp["keySourceDescription"] != "no key found (configured path /secrets-cb/gw_key not allowed)" {
		t.Fatalf("keySourceDescription = %v", resp["keySourceDescription"])
	}

	resp = gatewaySectionResponse(config.GatewayConfig{Name: "envonly"})
	if resp["apiKeyFileAllowed"] != true {
		t.Fatal("a gateway with no api_key_file is not a problem")
	}
}
