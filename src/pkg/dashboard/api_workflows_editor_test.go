package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

func TestWorkflowYAMLRoundTrip(t *testing.T) {
	s := workflowTestServer(t, false)
	m := techdocsModel(t)
	rec := wfReq(s, http.MethodPost, "/api/workflows/yaml", map[string]any{"model": m}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("to yaml: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.YAML, "apiVersion: ") || !strings.Contains(out.YAML, "\nspec:\n") || strings.Contains(out.YAML, "{") {
		t.Fatalf("expected block yaml, got:\n%s", out.YAML)
	}
	rec = wfReq(s, http.MethodPost, "/api/workflows/yaml", map[string]any{"yaml": out.YAML}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("from yaml: %d %s", rec.Code, rec.Body)
	}
	var back struct {
		Model *workflowmodel.Model `json:"model"`
		Error string               `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Error != "" || back.Model == nil {
		t.Fatalf("round trip: %s", rec.Body)
	}
	a, _ := json.Marshal(m)
	b, _ := json.Marshal(back.Model)
	if string(a) != string(b) {
		t.Fatalf("model changed across yaml round trip:\n%s\n%s", a, b)
	}
}

func TestWorkflowYAMLErrorsInline(t *testing.T) {
	s := workflowTestServer(t, false)
	for _, text := range []string{"spec: [unclosed", "metadata:\n  name: x\nbogus: 1\n"} {
		rec := wfReq(s, http.MethodPost, "/api/workflows/yaml", map[string]any{"yaml": text}, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: %d %s", text, rec.Code, rec.Body)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if e, _ := out["error"].(string); e == "" {
			t.Fatalf("%q: expected inline error, got %s", text, rec.Body)
		}
	}
	rec := wfReq(s, http.MethodPost, "/api/workflows/yaml", map[string]any{}, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: %d", rec.Code)
	}
}

func TestWorkflowSimulateUnsavedModel(t *testing.T) {
	s := workflowTestServer(t, false)
	rec := wfReq(s, http.MethodPost, "/api/workflows/simulate", map[string]any{
		"model": hivedefault.Generate(), "kind": "pr",
		"facts": map[string]any{
			"labels": []string{"role/trusted"}, "open": true,
			"author": map[string]any{"login": "andy", "kind": "human", "role": "trusted"},
			"pr":     map[string]any{"checks": "green", "review": "approved", "dco": "yes"},
		},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out SimulateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Decision.Stage != 80 || out.Decision.State != "awaiting-merge-admission" || len(out.Trace) != 1 {
		t.Fatalf("decision %+v trace %+v", out.Decision, out.Trace)
	}

	// Missing facts and missing kind are 400s; a non-compiling model is a
	// 422 that carries the diagnostics.
	rec = wfReq(s, http.MethodPost, "/api/workflows/simulate", map[string]any{"model": hivedefault.Generate(), "kind": "pr"}, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no facts: %d", rec.Code)
	}
	rec = wfReq(s, http.MethodPost, "/api/workflows/simulate", map[string]any{"model": hivedefault.Generate(), "facts": map[string]any{}}, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no kind: %d", rec.Code)
	}
	broken := techdocsModel(t)
	broken.Spec.Transitions = nil
	rec = wfReq(s, http.MethodPost, "/api/workflows/simulate", map[string]any{"model": broken, "kind": "issue", "facts": map[string]any{"open": true}}, false)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"diagnostics"`) {
		t.Fatalf("broken model: %d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowSaveAssignsNextVersionWhenEmpty(t *testing.T) {
	s := workflowTestServer(t, true)
	m := techdocsModel(t)
	m.Metadata.Name = "editor-draft"
	m.Spec.Meta.Version = ""
	rec := wfReq(s, http.MethodPost, "/api/workflows", m, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first save: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"version":"1"`) {
		t.Fatalf("expected version 1: %s", rec.Body)
	}
	rec = wfReq(s, http.MethodPost, "/api/workflows", m, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"version":"2"`) {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}
}
