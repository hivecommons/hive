package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// writeTempKeyFile stores a gateway key VALUE in a temp file and returns its
// path, mirroring how the server records keys (api_key_file, never inline).
func writeTempKeyFile(t *testing.T, key string) string {
	t.Helper()
	dir := t.TempDir()
	// N8: api_key_file reads are confined to the managed secrets dirs, so this
	// temp dir has to be registered as an allowed root for the duration of the
	// test — otherwise ResolveAPIKey correctly refuses to read it.
	t.Cleanup(config.AllowSecretFileRoot(dir))
	path := filepath.Join(dir, "gateway_api_key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	return path
}

// The onboarding flow these tests pin: an operator once set an agent's backend
// to watsonx with no watsonx gateway configured, and the agent silently failed
// to launch for a day ("unknown backend: watsonx"). Selecting a gateway-backed
// method must now route the operator to Model Gateways to supply the key, and
// once the key is saved model discovery must populate the dropdown.

// The agent-config dialog's method picker must go through the SHARED guard, not
// a litellm-only check — that hardcoded 'litellm' comparison is exactly what let
// a watsonx selection through unchallenged.
func TestCliPinChangeUsesSharedGatewayGuard(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (!(await ensureGatewayMethodConfigured(sel.value))) {") {
		t.Error("onCliPinChange must gate every method through ensureGatewayMethodConfigured")
	}
	// The old litellm-only guards must be gone from all three call sites.
	if strings.Contains(html, "sel.value === 'litellm' && !(await ensureLiteLLMConfigured())") {
		t.Error("onCliPinChange still has the litellm-only guard")
	}
	if strings.Contains(html, "generalData.cliPinValue === 'litellm' && !(await ensureLiteLLMConfigured())") {
		t.Error("agent-create save still has the litellm-only guard")
	}
	if strings.Contains(html, "dirty.general.cliPinValue === 'litellm' && !(await ensureLiteLLMConfigured())") {
		t.Error("agent-config save still has the litellm-only guard")
	}
}

// The guard mirrors the litellm path rather than reimplementing it: litellm
// keeps its dedicated endpoint+key prompt, and every other gateway method lands
// on the Model Gateways tab with the add-form preset to that kind.
func TestGatewayGuardMirrorsLiteLLMAndOpensModelGateways(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"delegates litellm to the existing prompt", "if (method === 'litellm') return ensureLiteLLMConfigured();"},
		{"only guards gateway-backed methods", "if (!GATEWAY_METHODS.includes(method)) return true;"},
		{"presets the add form to this kind", "_pendingGatewayPresetKind = method;"},
		{"opens the Model Gateways tab", "openConfigDialog('governor', null, 'Model Gateways');"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("ensureGatewayMethodConfigured is missing %q", tc.snippet)
			}
		})
	}
	// watsonx must be one of the guarded methods (the required case).
	if !strings.Contains(html, "const GATEWAY_METHODS = ['openrouter', 'litellm', 'vllm', 'llm-d', 'watsonx'];") {
		t.Error("GATEWAY_METHODS must cover watsonx alongside the other gateway kinds")
	}
}

// A gateway that exists but carries no API key must NOT satisfy the guard for
// key-requiring kinds: a keyless watsonx gateway mints no IAM token and a
// keyless openrouter gateway cannot authenticate, so both fail exactly like a
// missing gateway. Self-hosted vllm/llm-d are commonly unauthenticated, so
// presence alone satisfies them. A configured+keyed gateway never interrupts.
func TestGatewayGuardRequiresKeyNotJustPresence(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (gw && (gw.hasKey || !GATEWAY_KEY_REQUIRED_METHODS.includes(method))) return true;") {
		t.Error("the guard must require hasKey for key-requiring kinds and accept keyless self-hosted gateways")
	}
	if !strings.Contains(html, "const GATEWAY_KEY_REQUIRED_METHODS = ['openrouter', 'watsonx'];") {
		t.Error("openrouter and watsonx are the key-requiring gateway methods")
	}
	// A live (non-fallback) discovery WITH MODELS proves the method works even
	// without a named gateway (env-configured endpoints) — do not interrupt
	// then. The length check matters: a 404 from an unconfigured backend used
	// to be cached as an empty non-fallback "discovery" that silently skipped
	// the guard (the Model Gateways redirect never appeared).
	if !strings.Contains(html, "if (cached && !cached.fallback && cached.models && cached.models.length) return true;") {
		t.Error("a live non-fallback discovery must short-circuit the guard only when it returned models")
	}
}

// The method-switch redirect (big agent card → maybeOpenMethodConfig) must
// fire when the method's gateway is missing OR keyless, and a non-OK
// /api/inference/models response (404 for an unconfigured backend) must never
// be cached as a successful discovery — that combination silently suppressed
// the Model Gateways dialog when an operator picked watsonx/litellm on a hive
// with no gateway configured.
func TestMethodSwitchRedirectFiresWithoutConfiguredGateway(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"non-OK discovery is not cached", "if (!resp.ok) {"},
		{"switch redirect requires a keyed gateway for key-requiring kinds", "if (!gw || (!gw.hasKey && GATEWAY_KEY_REQUIRED_METHODS.includes(backend))) {"},
		{"cache short-circuit needs actual models", "if (cached && !cached.fallback && cached.models && cached.models.length) return;"},
		{"keyless gateway names the reason", "'The ' + backend + ' gateway has no API key'"},
		{"a rejected gateway-method switch routes to the form", "dismissToast(toast, `No ${backend} gateway configured — opening Settings → Model Gateways`, 'info');"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("method-switch redirect path is missing %q", tc.snippet)
			}
		})
	}
}

// After the key is saved, discovery must re-run and repaint the agent dropdowns
// with no manual refresh — the second half of the operator's requirement.
func TestGatewaySaveTriggersModelDiscoveryAndPopulation(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"save closes the loop", "refreshMethodModelsAfterGatewaySave(payload.kind, payload.name);"},
		{"forces a fresh discovery past the TTL cache", "models = await fetchInferenceModels(method, true);"},
		{"repaints the agent model dropdown", "await updateModelDropdown(a.name, method);"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("the post-save discovery path is missing %q", tc.snippet)
			}
		})
	}
}

// A failed probe must say why, with the server's own error text, instead of
// rendering an empty dropdown that looks like "this gateway has no models".
func TestProbeFailureSurfacesErrorNotSilentEmptyList(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"echoes the server error", "showGatewayFormProbeError(data.error || 'Model discovery failed — check the endpoint and API key.');"},
		{"explains a connected-but-empty list", "showGatewayFormProbeError('Connected, but the gateway returned no models."},
		{"reports network failures", "showGatewayFormProbeError('Network error while discovering models: ' + e.message);"},
		{"flags a fallback list after save", "model discovery did not return a live list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("probe error reporting is missing %q", tc.snippet)
			}
		})
	}
}

// Model discovery for an agent backend must use the gateway's OWN auth scheme.
// A watsonx gateway needs an IAM-minted bearer plus X-IBM-Project-ID; sending
// the raw IBM Cloud key as a bearer 401s, and the dropdown then silently served
// unrelated static aliases.
func TestInferenceModelsUsesWatsonxIAMBearer(t *testing.T) {
	var gotAuth, gotProject string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(watsonx.ProjectIDHeader)
		if gotAuth != "Bearer IAM-JWT" || gotProject == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{{"id": "ibm/granite-4-h-small"}},
		})
	}))
	defer gateway.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "IAM-JWT", "expires_in": 3600})
	}))
	defer iam.Close()
	origMinter := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(iam.URL+"/identity/token", nil)
	t.Cleanup(func() { watsonx.DefaultMinter = origMinter })

	srv := newFullServer(t)
	keyFile := writeTempKeyFile(t, "ibm-cloud-key-1234567890")
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:       "watsonx",
		Kind:       config.GatewayKindWatsonx,
		Endpoint:   gateway.URL,
		APIKeyFile: keyFile,
		ProjectID:  "proj-42",
	}}
	srv.registerGatewayEndpoints()

	models := srv.fetchInferenceModelsForBackend("watsonx", []string{gateway.URL})
	if len(models) != 1 || models[0] != "ibm/granite-4-h-small" {
		t.Fatalf("models = %v, want the granite id discovered via the IAM bearer", models)
	}
	if gotAuth != "Bearer IAM-JWT" {
		t.Errorf("Authorization = %q, want the minted IAM bearer (not the raw key)", gotAuth)
	}
	if gotProject != "proj-42" {
		t.Errorf("%s = %q, want the configured project id", watsonx.ProjectIDHeader, gotProject)
	}
}

// Non-watsonx gateways keep the legacy raw-key discovery path untouched.
func TestInferenceModelsNonWatsonxUsesRawKeyPath(t *testing.T) {
	var gotAuth string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{{"id": "gpt-4o"}},
		})
	}))
	defer gateway.Close()

	srv := newFullServer(t)
	keyFile := writeTempKeyFile(t, "sk-raw-litellm-key")
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:     "litellm",
		Kind:     config.GatewayKindLiteLLM,
		Endpoint: gateway.URL,
	}}
	// Non-watsonx discovery keeps resolving its key through the legacy
	// inferenceAPIKey path (governor.litellm), not the gateway entry — pinning
	// that here is what proves the raw-key path is untouched by this change.
	srv.deps.Config.Governor.LiteLLM = config.LiteLLMConfig{
		Endpoint:   gateway.URL,
		APIKeyFile: keyFile,
	}
	srv.registerGatewayEndpoints()

	models := srv.fetchInferenceModelsForBackend("litellm", []string{gateway.URL})
	if len(models) != 1 || models[0] != "gpt-4o" {
		t.Fatalf("models = %v, want the discovered litellm model", models)
	}
	if gotAuth != "Bearer sk-raw-litellm-key" {
		t.Errorf("Authorization = %q, want the raw key unchanged for non-watsonx", gotAuth)
	}
}

// gatewayKindNeedsProbeAuth is the single predicate deciding which kinds need
// the minted-bearer path, so a new kind is one edit rather than a new branch in
// every discovery caller.
func TestGatewayKindNeedsProbeAuth(t *testing.T) {
	if !gatewayKindNeedsProbeAuth(config.GatewayKindWatsonx) {
		t.Error("watsonx must use the gatewayProbeAuth path")
	}
	for _, kind := range []string{
		config.GatewayKindLiteLLM,
		config.GatewayKindVLLM,
		config.GatewayKindLLMD,
		config.GatewayKindOpenRouter,
		config.GatewayKindCustom,
	} {
		if gatewayKindNeedsProbeAuth(kind) {
			t.Errorf("kind %q must keep the raw-key discovery path", kind)
		}
	}
}

// A backend resolves to its gateway by NAME or by KIND, since agent backends
// name a gateway directly and the built-in methods share name and kind.
func TestResolveGatewayForBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:     "corp-watsonx",
		Kind:     config.GatewayKindWatsonx,
		Endpoint: "https://us-south.ml.cloud.ibm.com/ml/gateway",
	}}
	if gw := srv.resolveGatewayForBackend("corp-watsonx"); gw == nil || gw.Kind != config.GatewayKindWatsonx {
		t.Errorf("lookup by name failed: %+v", gw)
	}
	if gw := srv.resolveGatewayForBackend("watsonx"); gw == nil || gw.Name != "corp-watsonx" {
		t.Errorf("lookup by kind failed: %+v", gw)
	}
	if gw := srv.resolveGatewayForBackend("nope"); gw != nil {
		t.Errorf("unknown backend should resolve to no gateway, got %+v", gw)
	}
}

// A watsonx gateway whose key does not mint must yield NO models rather than a
// misleading list — the caller then marks the dropdown as a fallback.
func TestInferenceModelsWatsonxMintFailureReturnsNoModels(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessage":"Provided API key could not be found"}`))
	}))
	defer iam.Close()
	origMinter := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(iam.URL+"/identity/token", nil)
	t.Cleanup(func() { watsonx.DefaultMinter = origMinter })

	srv := newFullServer(t)
	keyFile := writeTempKeyFile(t, "bad-key")
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:       "watsonx",
		Kind:       config.GatewayKindWatsonx,
		Endpoint:   "https://us-south.ml.cloud.ibm.com/ml/gateway",
		APIKeyFile: keyFile,
		ProjectID:  "proj-42",
	}}
	if models := srv.fetchInferenceModelsForBackend("watsonx", []string{"https://us-south.ml.cloud.ibm.com/ml/gateway"}); len(models) != 0 {
		t.Fatalf("models = %v, want none when the IAM mint fails", models)
	}
}
