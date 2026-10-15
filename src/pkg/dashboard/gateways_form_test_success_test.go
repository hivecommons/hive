package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The form-level Test on the opened litellm card used to answer a bare "OK —
// N models available", which could not confirm WHAT was tested, while the
// saved-card Test spelled out endpoint, default model, key and route
// (hivecommons/hive#9945, Mike's two screenshots). On success the form Test
// must now return the same diagnostic text the saved Test does, including the
// route line agents will use — and must still never leak the key.
func TestGatewayFormTestSuccessEchoesWhatWasTested(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"rits/zai-org/glm-5-3"},{"id":"rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"}]}`))
	}))
	defer upstream.Close()

	endpoint := "https://raw.githubusercontent.com"
	routeImportFetchesToServer(t, upstream)
	t.Setenv("HIVE_TEST_LITELLM_KEY", "sk-livekeyvalue")
	origProxyFunc := probeProxyFunc
	probeProxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	t.Cleanup(func() { probeProxyFunc = origProxyFunc })

	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:         "litellm",
		Kind:         config.GatewayKindLiteLLM,
		Endpoint:     endpoint,
		DefaultModel: "rits/zai-org/glm-5-3",
		APIKeyEnv:    "HIVE_TEST_LITELLM_KEY",
		KeyName:      "mspreitz-cb-1",
	}}

	discoverBody := `{"name":"litellm","kind":"litellm","endpoint":"` + endpoint + `","api_key":"sk-livekeyvalue","default_model":"rits/zai-org/glm-5-3","key_name":"mspreitz-cb-1"}`
	discoverReq := httptest.NewRequest(http.MethodPost, "/api/config/governor/gateways/discover", strings.NewReader(discoverBody))
	discoverReq.Header.Set("Content-Type", "application/json")
	markOwnerRequest(discoverReq)
	discoverW := httptest.NewRecorder()
	srv.handleGovernorGatewaysDiscover(discoverW, discoverReq)
	if discoverW.Code != http.StatusOK {
		t.Fatalf("discover code = %d, body %s", discoverW.Code, discoverW.Body.String())
	}
	var discoverResp struct {
		OK         bool     `json:"ok"`
		Models     []string `json:"models"`
		Diagnostic string   `json:"diagnostic"`
		Probe      struct {
			OK         bool   `json:"ok"`
			ModelCount int    `json:"model_count"`
			Diagnostic string `json:"diagnostic"`
			Route      string `json:"route"`
		} `json:"probe"`
	}
	if err := json.Unmarshal(discoverW.Body.Bytes(), &discoverResp); err != nil {
		t.Fatalf("decode discover: %v", err)
	}
	if !discoverResp.OK || len(discoverResp.Models) != 2 {
		t.Fatalf("discover response = %+v, want ok with 2 models", discoverResp)
	}
	if !discoverResp.Probe.OK || discoverResp.Probe.ModelCount != 2 {
		t.Fatalf("probe = %+v, want ok with model_count 2", discoverResp.Probe)
	}
	if discoverResp.Diagnostic != discoverResp.Probe.Diagnostic {
		t.Fatalf("top-level and probe diagnostics differ:\n%s\n%s", discoverResp.Diagnostic, discoverResp.Probe.Diagnostic)
	}

	testReq := httptest.NewRequest(http.MethodPost, "/api/config/governor/gateways/litellm/test", nil)
	testReq.SetPathValue("name", "litellm")
	testW := httptest.NewRecorder()
	srv.handleGovernorGatewaysTest(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("saved test code = %d, body %s", testW.Code, testW.Body.String())
	}
	var testResp struct {
		Probe struct {
			OK         bool   `json:"ok"`
			Diagnostic string `json:"diagnostic"`
			Route      string `json:"route"`
		} `json:"probe"`
	}
	if err := json.Unmarshal(testW.Body.Bytes(), &testResp); err != nil {
		t.Fatalf("decode saved test: %v", err)
	}
	if !testResp.Probe.OK {
		t.Fatalf("saved test = %+v, want ok", testResp.Probe)
	}
	if discoverResp.Diagnostic != testResp.Probe.Diagnostic {
		t.Fatalf("form and saved success diagnostics differ:\nform:  %s\nsaved: %s", discoverResp.Diagnostic, testResp.Probe.Diagnostic)
	}
	for _, want := range []string{
		"Endpoint: " + endpoint,
		"Default model: rits/zai-org/glm-5-3",
		"Using key: mspreitz-cb-1",
		"SHA256: ",
		"Connection OK: 2 models available",
		`Agents on backend "litellm" send inference to POST ` + endpoint + "/v1/chat/completions",
		"(model rits/zai-org/glm-5-3)",
		`route source: gateway "litellm"`,
	} {
		if !strings.Contains(discoverResp.Diagnostic, want) {
			t.Errorf("diagnostic %q missing %q", discoverResp.Diagnostic, want)
		}
	}
	if strings.Contains(discoverResp.Diagnostic, "not saved yet") {
		t.Errorf("form endpoint equals the saved route; no unsaved warning expected: %q", discoverResp.Diagnostic)
	}
	if strings.Contains(discoverW.Body.String(), "sk-livekeyvalue") {
		t.Errorf("response leaks API key: %s", discoverW.Body.String())
	}

	// Typing a DIFFERENT endpoint than the saved one must say agents do not
	// use it yet, so a passing form Test is not read as "agents are fixed".
	otherBody := strings.Replace(discoverBody, `"endpoint":"`+endpoint+`"`, `"endpoint":"https://raw.githubusercontent.com/other"`, 1)
	otherReq := httptest.NewRequest(http.MethodPost, "/api/config/governor/gateways/discover", strings.NewReader(otherBody))
	otherReq.Header.Set("Content-Type", "application/json")
	markOwnerRequest(otherReq)
	otherW := httptest.NewRecorder()
	srv.handleGovernorGatewaysDiscover(otherW, otherReq)
	var otherResp struct {
		OK         bool   `json:"ok"`
		Diagnostic string `json:"diagnostic"`
	}
	if err := json.Unmarshal(otherW.Body.Bytes(), &otherResp); err != nil {
		t.Fatalf("decode other discover: %v", err)
	}
	if !otherResp.OK || !strings.Contains(otherResp.Diagnostic, "not saved yet") {
		t.Fatalf("unsaved-endpoint form Test = %+v, want ok with an unsaved-route warning", otherResp)
	}
}

// Pins the frontend half: the form-level Test renders the server's probe
// diagnostic on success instead of synthesising a bare "OK — N models".
func TestGatewayFormTestStaticRendersServerDiagnostic(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"const probe = await probeGatewayFormModels();",
		"el.innerHTML = gatewayProbeHTML(probe && probe.ok ? probe : { ok: true, model_count: _gatewayFormModels.length });",
		"if (data.probe && data.probe.ok) probe = data.probe;",
		"return probe;",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
