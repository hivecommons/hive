package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// modelSelectHelpersJS extracts the agent config dialog's Model select
// helpers (hivecommons/hive#9945) plus a minimal environment for them, so a
// node harness can render the options exactly as the browser does.
func modelSelectHelpersJS(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, "const MODEL_PRECEDENCE_TITLE = ")
	if start < 0 {
		t.Fatal("MODEL_PRECEDENCE_TITLE not found in index.html")
	}
	rest := html[start:]
	title := rest[:strings.Index(rest, "\n")]
	return title + "\n" +
		extractJSFunction(t, html, "fullModelLabel") + "\n    }\n" +
		extractJSFunction(t, html, "inferenceBackendDefaultModel") + "\n    }\n" +
		extractJSFunction(t, html, "modelDisallowedByGateway") + "\n    }\n" +
		extractJSFunction(t, html, "modelDefaultOptionLabel") + "\n    }\n" +
		extractJSFunction(t, html, "modelSelectOptionsHtml") + "\n    }\n" +
		extractJSFunction(t, html, "_modelIdMatches") + "\n    }\n" +
		`var _inferenceModelCache = (typeof _inferenceModelCache !== 'undefined') ? _inferenceModelCache : {};
		var _configuredGateways = (typeof _configuredGateways !== 'undefined') ? _configuredGateways : [];
		var INFERENCE_BACKENDS = ['vllm', 'llm-d', 'litellm', 'watsonx'];
		function includesNameCI(list, name) { return list.some(x => String(x).toLowerCase() === String(name).toLowerCase()); }
		function isInferenceBackendName(b) { return INFERENCE_BACKENDS.includes(b) || includesNameCI(_configuredGateways, b); }
		function canonicalInferenceBackendName(b) { return b; }
`
}

// The FMA quality agent's dialog offered "(from launch command)",
// "NVIDIA-Nemotron-…", "glm-5-3" and "✓ aws/claude-sonnet-4-6 (current)".
// The operator could not tell that the first was "no per-agent model", that
// "glm-5-3" was the gateway's rits/zai-org/glm-5-3, or that the current
// model was the reason every call 403'd (hivecommons/hive#9945). Pin: option
// VALUES stay the full gateway ids, labels show the full id, the default
// option names the resolved gateway default, and a current model outside the
// entitled set is flagged.
func TestAgentModelPickerShowsFullIdsAndResolvedDefault(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`${modelSelectOptionsHtml(_agentBackend, g.model || '', _modelOpts)}`,
		`<select id="cfg-model" title="${esc(MODEL_PRECEDENCE_TITLE)}"`,
		"setTimeout(() => ensureModelOptionsForBackend(_agentBackend), 0);",
		"sel.innerHTML = modelSelectOptionsHtml(backend, current, opts);",
		"defaultModel: data.default_model || '',",
		"entitledModels: Array.isArray(data.entitled_models) ? data.entitled_models : null,",
		"label: m.split('/').pop() + labelSuffix, suffix: labelSuffix }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, `>(from launch command)<`) {
		t.Error("the opaque \"(from launch command)\" option label must be gone")
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertions above still ran")
	}
	script := extractJSFunction(t, html, "esc") + "\n    }\n" +
		extractJSOneLiner(t, html, "_normalizeModel") + "\n" +
		extractJSOneLiner(t, html, "_modelsEqual") + "\n" +
		modelSelectHelpersJS(t, html) +
		`const glm = 'rits/zai-org/glm-5-3', nemo = 'rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4';
		const opts = [nemo, glm].map(m => ({ value: m, label: m.split('/').pop() + ' (your key)', suffix: ' (your key)' }));
		_inferenceModelCache.litellm = { models: opts, entitled: true, entitledModels: [glm, nemo], defaultModel: glm };
		const out = {
			stale: modelSelectOptionsHtml('litellm', 'aws/claude-sonnet-4-6', opts),
			none: modelSelectOptionsHtml('litellm', '', opts),
			explicit: modelSelectOptionsHtml('litellm', glm, opts),
			unknownEntitlement: (function(){ _inferenceModelCache.litellm = { models: opts, entitled: false, defaultModel: '' }; return modelSelectOptionsHtml('litellm', 'aws/claude-sonnet-4-6', opts); })(),
			cli: modelSelectOptionsHtml('copilot', '', [{ value: 'claude-sonnet-4-6', label: 'claude-sonnet-4-6' }]),
		};
		console.log(JSON.stringify(out));`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}

	stale := got["stale"]
	for _, want := range []string{
		`<option value="">(backend default — rits/zai-org/glm-5-3)</option>`,
		`<option value="rits/zai-org/glm-5-3">rits/zai-org/glm-5-3 (your key)</option>`,
		`<option value="rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4">rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4 (your key)</option>`,
		`<option value="aws/claude-sonnet-4-6" selected>aws/claude-sonnet-4-6 (current — not allowed by gateway)</option>`,
	} {
		if !strings.Contains(stale, want) {
			t.Errorf("stale-model options missing %q:\n%s", want, stale)
		}
	}
	if strings.Contains(stale, `>glm-5-3 (`) {
		t.Errorf("label must show the full gateway id, not the stripped tail:\n%s", stale)
	}
	if !strings.Contains(got["none"], `<option value="">(backend default — rits/zai-org/glm-5-3)</option>`) || strings.Contains(got["none"], "(current") {
		t.Errorf("no per-agent model: want only the default option, no (current):\n%s", got["none"])
	}
	if !strings.Contains(got["explicit"], `<option value="rits/zai-org/glm-5-3" selected>rits/zai-org/glm-5-3 (your key)</option>`) || strings.Contains(got["explicit"], "(current") {
		t.Errorf("explicit entitled model must preselect its full-id option:\n%s", got["explicit"])
	}
	if !strings.Contains(got["unknownEntitlement"], `aws/claude-sonnet-4-6 (current)</option>`) || strings.Contains(got["unknownEntitlement"], "not allowed") {
		t.Errorf("without an entitlement signal the current model must not be called disallowed:\n%s", got["unknownEntitlement"])
	}
	if !strings.Contains(got["unknownEntitlement"], `(backend default — the gateway`) {
		t.Errorf("inference backend without a known default must still explain the default option:\n%s", got["unknownEntitlement"])
	}
	if !strings.Contains(got["cli"], `<option value="">(backend default — chosen by the copilot CLI)</option>`) || !strings.Contains(got["cli"], `<option value="claude-sonnet-4-6">claude-sonnet-4-6</option>`) {
		t.Errorf("CLI backend default option / static label wrong:\n%s", got["cli"])
	}
}

// /api/inference/models/{backend} must carry the resolved default model and
// the full entitled set the dialog needs (hivecommons/hive#9945).
func TestInferenceModelsAPIReportsDefaultModelAndEntitledSet(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"rits/zai-org/glm-5-3"},{"id":"aws/claude-sonnet-4-6"},{"id":"rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"}]}`))
	}))
	defer upstream.Close()

	srv := newFullServer(t)
	srv.deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name: "litellm", Kind: config.GatewayKindLiteLLM, Endpoint: upstream.URL, DefaultModel: "rits/zai-org/glm-5-3",
	}}
	srv.SetInferenceEndpoints(map[string][]string{"litellm": {upstream.URL}})
	orig := getEntitledModelsFn()
	SetEntitledModelsProvider(func(ep string) ([]string, string, bool) {
		return []string{"rits/zai-org/glm-5-3", "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"}, "403", true
	})
	t.Cleanup(func() { SetEntitledModelsProvider(orig) })

	req := httptest.NewRequest(http.MethodGet, "/api/inference/models/litellm", nil)
	req.SetPathValue("backend", "litellm")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Models         []string `json:"models"`
		Entitled       bool     `json:"entitled"`
		EntitledModels []string `json:"entitled_models"`
		DefaultModel   string   `json:"default_model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DefaultModel != "rits/zai-org/glm-5-3" {
		t.Errorf("default_model = %q, want the litellm gateway default", resp.DefaultModel)
	}
	if !resp.Entitled || len(resp.EntitledModels) != 2 || len(resp.Models) != 2 {
		t.Errorf("entitlement not reported: %+v", resp)
	}
	for _, m := range resp.Models {
		if m == "aws/claude-sonnet-4-6" {
			t.Errorf("non-entitled model offered: %v", resp.Models)
		}
	}
}
