package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

const ingestCIYAML = "name: CI\non: [pull_request, push]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps: [{run: make test}]\n"

func TestWorkflowIngestFromFilesCompilesAndSaves(t *testing.T) {
	s := workflowTestServer(t, true)
	body := map[string]any{"files": map[string]string{".github/workflows/ci.yml": ingestCIYAML}, "name": "pasted", "save": true}
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest", body, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var resp struct {
		Compiled  bool                       `json:"compiled"`
		Pipelines int                        `json:"pipelines"`
		Entry     wfstore.Entry              `json:"entry"`
		Model     workflowmodel.Model        `json:"model"`
		Diags     []workflowmodel.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Compiled || resp.Pipelines != 1 || resp.Entry.Key() != "pasted@1" || !resp.Entry.Compiled {
		t.Fatalf("%+v", resp)
	}
	if resp.Model.Spec.Meta.Provenance != workflowmodel.ProvenanceIngestedUnreviewed {
		t.Fatalf("provenance %q", resp.Model.Spec.Meta.Provenance)
	}
	// A second ingest of the same name lands at the next version.
	rec = wfReq(s, http.MethodPost, "/api/workflows/ingest", body, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Entry.Key() != "pasted@2" {
		t.Fatalf("second version %s", resp.Entry.Key())
	}
	// Dry run (no save) needs no store and returns the model.
	s2 := workflowTestServer(t, false)
	rec = wfReq(s2, http.MethodPost, "/api/workflows/ingest", map[string]any{"files": map[string]string{".github/workflows/ci.yml": ingestCIYAML}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowIngestFromRepoUsesForgeFetcher(t *testing.T) {
	s := workflowTestServer(t, true)
	var asked string
	s.deps.WorkflowRepoFiles = func(_ context.Context, repo string) (map[string][]byte, string, error) {
		asked = repo
		if repo == "acme/missing" {
			return nil, "", errors.New("404")
		}
		return map[string][]byte{".github/workflows/ci.yml": []byte(ingestCIYAML)}, "main", nil
	}
	rec := wfReq(s, http.MethodPost, "/api/workflows/ingest", map[string]any{"repo": "acme/widgets"}, true)
	if rec.Code != http.StatusOK || asked != "acme/widgets" {
		t.Fatalf("%d %s asked=%q", rec.Code, rec.Body, asked)
	}
	var resp struct {
		Ref   string              `json:"ref"`
		Model workflowmodel.Model `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ref != "main" || resp.Model.Metadata.Name != "acme-widgets" || resp.Model.Spec.Scope.Repository != "acme/widgets" {
		t.Fatalf("%+v", resp)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest", map[string]any{"repo": "acme/missing"}, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("fetch failure %d", rec.Code)
	}
}

func TestWorkflowIngestGuards(t *testing.T) {
	s := workflowTestServer(t, true)
	cases := []struct {
		body  map[string]any
		owner bool
		want  int
	}{
		{map[string]any{"repo": "acme/widgets"}, false, http.StatusForbidden},
		{map[string]any{}, true, http.StatusBadRequest},
		{map[string]any{"repo": "acme/widgets", "files": map[string]string{"a.yml": "x"}}, true, http.StatusBadRequest},
		{map[string]any{"repo": "nojslash"}, true, http.StatusBadRequest},
		{map[string]any{"repo": "acme/widgets"}, true, http.StatusServiceUnavailable}, // no fetcher
	}
	for i, c := range cases {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/ingest", c.body, c.owner); rec.Code != c.want {
			t.Fatalf("case %d: %d %s", i, rec.Code, rec.Body)
		}
	}
}
