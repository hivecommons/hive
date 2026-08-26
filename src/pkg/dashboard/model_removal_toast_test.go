package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests cover #4438: an operator opened the dashboard to a wall of pink
// "Model removed from litellm: xxx" toasts naming models the gateway still
// served — including the one the hive's own agents were configured to use.
//
// #4426 already taught the server to flag a TOTAL discovery failure
// (models_fallback), so the UI stops diffing against the static aliases. Two
// sibling paths reached the same false-removal storm without ever raising that
// flag, and one display choice made the resulting toast impossible to diagnose.

// modelsEndpoint serves an OpenAI-style /v1/models body listing ids.
func modelsEndpoint(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadEndpoint serves 403 on everything — a live gateway whose key just lost
// /v1/models access, which is exactly how the storm in #4438 began.
func deadEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchModelsFromEndpointsDetailed_ReportsIncompleteSweep pins the new
// signal at its source: a sweep that lost an endpoint says so, instead of
// silently passing off the survivors as the whole set.
func TestFetchModelsFromEndpointsDetailed_ReportsIncompleteSweep(t *testing.T) {
	live := modelsEndpoint(t, "openai/gpt-5.1", "anthropic/claude-sonnet-4-6")
	dead := deadEndpoint(t)

	models, complete := fetchModelsFromEndpointsDetailed([]string{live.URL, dead.URL}, "")
	if complete {
		t.Error("complete = true with one endpoint answering 403 — a partial sweep must report itself incomplete")
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want the 2 ids the reachable endpoint served", models)
	}
}

// TestFetchModelsFromEndpointsDetailed_CompleteSweep is the control: when
// every endpoint answers, the census is authoritative.
func TestFetchModelsFromEndpointsDetailed_CompleteSweep(t *testing.T) {
	a := modelsEndpoint(t, "model-a")
	b := modelsEndpoint(t, "model-b")

	models, complete := fetchModelsFromEndpointsDetailed([]string{a.URL, b.URL}, "")
	if !complete {
		t.Error("complete = false though every endpoint answered")
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want both endpoints' ids", models)
	}
}

// TestQueryInferenceModelsDetailed_PartialSweepIsNotAuthoritative is the core
// #4438 regression. Before the fix a partial sweep returned fallback=false,
// so the dashboard diffed the survivors against the full previous list and
// announced every model behind the unreachable endpoint as removed.
func TestQueryInferenceModelsDetailed_PartialSweepIsNotAuthoritative(t *testing.T) {
	live := modelsEndpoint(t, "vertex/claude-sonnet-4-6")
	dead := deadEndpoint(t)

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {live.URL, dead.URL}}

	models, fallback := srv.queryInferenceModelsDetailed("vllm")
	if !fallback {
		t.Error("fallback = false for a partial sweep — the UI would diff it and toast false removals (#4438)")
	}
	if len(models) != 1 || models[0] != "vertex/claude-sonnet-4-6" {
		t.Errorf("models = %v, want the reachable endpoint's list still shown (a short dropdown beats an empty one)", models)
	}
}

// TestQueryInferenceModelsDetailed_FullSweepIsAuthoritative is the control:
// a genuine removal must still be reported, or the fix would trade a false
// alarm for silence.
func TestQueryInferenceModelsDetailed_FullSweepIsAuthoritative(t *testing.T) {
	live := modelsEndpoint(t, "vertex/claude-sonnet-4-6")

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {live.URL}}

	models, fallback := srv.queryInferenceModelsDetailed("vllm")
	if fallback {
		t.Error("fallback = true though the only endpoint answered — real model changes would go unreported")
	}
	if len(models) != 1 {
		t.Errorf("models = %v, want 1", models)
	}
}

// TestQueryInferenceModelsDetailed_EnvAfterFailedProbeIsNotAuthoritative
// covers the second unflagged path. HIVE_*_MODELS is consulted only AFTER
// discovery comes up empty, so on a hive that sets it, a 403 swapped the live
// list for the env list and called the swap authoritative — the same storm,
// just with a different substitute list.
func TestQueryInferenceModelsDetailed_EnvAfterFailedProbeIsNotAuthoritative(t *testing.T) {
	dead := deadEndpoint(t)
	t.Setenv("HIVE_VLLM_MODELS", "model-a,model-b")

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {dead.URL}}

	models, fallback := srv.queryInferenceModelsDetailed("vllm")
	if !fallback {
		t.Error("fallback = false for the env list standing in for a failed probe (#4438)")
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want the env override still used to fill the dropdown", models)
	}
}

// TestQueryInferenceModelsDetailed_EnvWithoutEndpointIsAuthoritative keeps the
// one case where the env list is the CONFIGURED source rather than a
// consolation prize: nothing was registered, so no probe ever failed.
func TestQueryInferenceModelsDetailed_EnvWithoutEndpointIsAuthoritative(t *testing.T) {
	t.Setenv("HIVE_VLLM_MODELS", "model-a,model-b")

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{}

	models, fallback := srv.queryInferenceModelsDetailed("vllm")
	if fallback {
		t.Error("fallback = true for an operator-configured env list with no endpoint to probe")
	}
	if len(models) != 2 {
		t.Errorf("models = %v, want 2", models)
	}
}

// TestHandleInferenceModels_PartialSweepIsFlaggedPartial covers the REST
// discovery the dashboard's model AUTO-HEAL reads. A partial sweep here does
// not merely toast: an "absent" model gets the agent's selection rewritten and
// its session relaunched. The response now says the list is a floor, so
// auto-heal sits the sample out — while still offering what was discovered,
// labelled normally rather than as unverified static aliases.
func TestHandleInferenceModels_PartialSweepIsFlaggedPartial(t *testing.T) {
	live := modelsEndpoint(t, "vertex/claude-sonnet-4-6")
	dead := deadEndpoint(t)

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {live.URL, dead.URL}}

	req := httptest.NewRequest("GET", "/api/inference/models/vllm", nil)
	req.SetPathValue("backend", "vllm")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp struct {
		Models   []string `json:"models"`
		Fallback bool     `json:"fallback"`
		Partial  bool     `json:"partial"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !resp.Partial {
		t.Error("partial = false for a sweep that lost an endpoint — auto-heal would switch agents off models it never saw (#4438)")
	}
	if resp.Fallback {
		t.Error("fallback = true — these ids were really discovered; marking them unverified static aliases misleads the dropdown")
	}
	if len(resp.Models) != 1 || resp.Models[0] != "vertex/claude-sonnet-4-6" {
		t.Errorf("models = %v, want the reachable endpoint's list", resp.Models)
	}
}

// TestHandleInferenceModels_FullSweepIsNotPartial is the control: a complete
// sweep stays authoritative, so genuine removals still heal.
func TestHandleInferenceModels_FullSweepIsNotPartial(t *testing.T) {
	live := modelsEndpoint(t, "vertex/claude-sonnet-4-6")

	srv := newFullServer(t)
	srv.inferenceEndpoints = map[string][]string{"vllm": {live.URL}}

	req := httptest.NewRequest("GET", "/api/inference/models/vllm", nil)
	req.SetPathValue("backend", "vllm")
	w := httptest.NewRecorder()
	srv.handleInferenceModels(w, req)

	var resp struct {
		Fallback bool `json:"fallback"`
		Partial  bool `json:"partial"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Partial || resp.Fallback {
		t.Errorf("partial = %v, fallback = %v — a complete sweep must stay authoritative", resp.Partial, resp.Fallback)
	}
}

// TestAutoHealSitsOutPartialDiscovery pins the frontend half: the reconcile
// call must treat a partial list as non-authoritative, exactly as it already
// treats a static fallback.
func TestAutoHealSitsOutPartialDiscovery(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "reconcileModelsAfterDiscovery(backend, models, !!data.fallback || !!data.partial);") {
		t.Error("index.html lets auto-heal act on a partial discovery — it would rewrite an agent's model and relaunch its session over an endpoint that merely timed out (#4438)")
	}
}

// TestModelChangeToastsNameTheFullModelID pins the display half of #4438. The
// reporter could not find the toasted name anywhere in his gateway's model
// list, because the toast printed only the segment after the last "/": the
// gateway serves "<provider>/claude-sonnet-4-6" and the toast rendered bare
// "claude-sonnet-4-6" — indistinguishable from the id his agents were set to.
func TestModelChangeToastsNameTheFullModelID(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"showToast(`New model available on ${b.id}: ${m}`, 'success');",
		"showToast(`Model removed from ${b.id}: ${m}`, 'error', false, TOAST_MODEL_CHANGE_MS);",
		"showToast(`⚠ ${a.displayName || a.name} uses removed model ${m} — update its model selection`,",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — model-change toasts must name the full provider-prefixed id (#4438)", snippet)
		}
	}
	if strings.Contains(html, "Model removed from ${b.id}: ${m.split('/').pop()}") {
		t.Error("the removal toast still strips the provider prefix — the operator cannot tell which id actually went away (#4438)")
	}
}

// TestEmptyModelListNeverDiffs pins the frontend's last-ditch guard: an empty
// live list the server did not flag would otherwise declare every known model
// removed at once, the worst-case form of this storm.
func TestEmptyModelListNeverDiffs(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "if (prevIds.length > 0 && currentIds.length === 0) continue;") {
		t.Error("index.html lost the empty-model-list diff guard — an unflagged discovery hole would toast every model as removed (#4438)")
	}
}
