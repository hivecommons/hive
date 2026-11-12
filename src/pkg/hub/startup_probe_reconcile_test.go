package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

type testPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func decodeStartupProbePatch(t *testing.T, patch string) []testPatchOp {
	t.Helper()
	var ops []testPatchOp
	if err := json.Unmarshal([]byte(patch), &ops); err != nil {
		t.Fatalf("patch is not valid JSON-Patch: %v\n%s", err, patch)
	}
	return ops
}

func TestStartupProbePatchAddsMissingProbe(t *testing.T) {
	live := []byte(`{"spec":{"template":{"spec":{
		"initContainers":[{"name":"copy-config","image":"ghcr.io/hivecommons/hive:v4-latest"}],
		"containers":[
			{"name":"sidecar","image":"busybox"},
			{"name":"hive","image":"ghcr.io/hivecommons/hive:v4-latest",
			 "livenessProbe":{"httpGet":{"path":"/api/livez","port":3002},"periodSeconds":30,"failureThreshold":3}}
		]}}}}`)
	patch, changes, err := startupProbePatchJSON(live)
	if err != nil {
		t.Fatal(err)
	}
	ops := decodeStartupProbePatch(t, patch)
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/spec/template/spec/containers/1/startupProbe" {
		t.Fatalf("unexpected patch: %s", patch)
	}
	var got templateStartupProbe
	if err := json.Unmarshal(ops[0].Value, &got); err != nil {
		t.Fatal(err)
	}
	if got != wantHostedStartupProbe() {
		t.Fatalf("patched probe %+v != template %+v", got, wantHostedStartupProbe())
	}
	if got.HTTPGet.Path != "/api/health" || got.HTTPGet.Port != dashboardPort || got.FailureThreshold != hostedStartupProbeFailureThreshold {
		t.Fatalf("probe does not match the template: %+v", got)
	}
	if len(changes) != 1 || changes[0] != "startupProbe" {
		t.Fatalf("unexpected changes: %v", changes)
	}
}

func TestStartupProbePatchReplacesDriftedProbe(t *testing.T) {
	live := []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"hive",
		"startupProbe":{"httpGet":{"path":"/api/health","port":3002},"initialDelaySeconds":10,"periodSeconds":5,"failureThreshold":30}}]}}}}`)
	patch, _, err := startupProbePatchJSON(live)
	if err != nil {
		t.Fatal(err)
	}
	if ops := decodeStartupProbePatch(t, patch); len(ops) != 1 || ops[0].Path != "/spec/template/spec/containers/0/startupProbe" {
		t.Fatalf("drifted probe must be replaced: %s", patch)
	}
}

func TestStartupProbePatchNoopWhenConverged(t *testing.T) {
	want := wantHostedStartupProbe()
	// API-server defaulted fields (scheme, timeoutSeconds, successThreshold)
	// must not make a converged Deployment look drifted.
	probe := map[string]any{
		"httpGet":             map[string]any{"path": want.HTTPGet.Path, "port": want.HTTPGet.Port, "scheme": "HTTP"},
		"initialDelaySeconds": want.InitialDelaySeconds,
		"periodSeconds":       want.PeriodSeconds,
		"failureThreshold":    want.FailureThreshold,
		"timeoutSeconds":      1,
		"successThreshold":    1,
	}
	dep := map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"initContainers": []any{map[string]any{"name": "init-permissions", "image": "ghcr.io/hivecommons/hive:v4-latest"}},
		"containers":     []any{map[string]any{"name": "hive", "startupProbe": probe}},
	}}}}
	live, _ := json.Marshal(dep)
	patch, changes, err := startupProbePatchJSON(live)
	if err != nil {
		t.Fatal(err)
	}
	if patch != "" || len(changes) != 0 {
		t.Fatalf("converged deployment must not be patched, got %s (%v)", patch, changes)
	}
}

func TestStartupProbePatchRewritesLegacyInitImageKeepingTag(t *testing.T) {
	want, _ := json.Marshal(wantHostedStartupProbe())
	live := []byte(`{"spec":{"template":{"spec":{
		"initContainers":[
			{"name":"copy-config","image":"ghcr.io/hivecommons/hive:v4-latest"},
			{"name":"init-permissions","image":"ghcr.io/kubestellar/hive:v2-latest"}
		],
		"containers":[{"name":"hive","startupProbe":` + string(want) + `}]}}}}`)
	patch, changes, err := startupProbePatchJSON(live)
	if err != nil {
		t.Fatal(err)
	}
	ops := decodeStartupProbePatch(t, patch)
	if len(ops) != 1 || ops[0].Op != "replace" || ops[0].Path != "/spec/template/spec/initContainers/1/image" {
		t.Fatalf("unexpected patch: %s", patch)
	}
	var img string
	if err := json.Unmarshal(ops[0].Value, &img); err != nil {
		t.Fatal(err)
	}
	if img != "ghcr.io/hivecommons/hive:v2-latest" {
		t.Fatalf("legacy init image must move to the template repo keeping its tag, got %q", img)
	}
	if len(changes) != 1 || !strings.Contains(changes[0], "init-permissions") {
		t.Fatalf("unexpected changes: %v", changes)
	}
}

func TestRewriteLegacyHiveImage(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/kubestellar/hive:v2-latest":  "ghcr.io/hivecommons/hive:v2-latest",
		"ghcr.io/kubestellar/hive":            "ghcr.io/hivecommons/hive",
		"ghcr.io/kubestellar/hive@sha256:abc": "ghcr.io/hivecommons/hive@sha256:abc",
	} {
		if got, ok := rewriteLegacyHiveImage(in); !ok || got != want {
			t.Errorf("%s: got %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"ghcr.io/hivecommons/hive:v4-latest", "ghcr.io/kubestellar/hive-other:v1", "busybox"} {
		if _, ok := rewriteLegacyHiveImage(in); ok {
			t.Errorf("%s must not be rewritten", in)
		}
	}
}
