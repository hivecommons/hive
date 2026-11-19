package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

func TestWorkflowVersionRoutesNeedStore(t *testing.T) {
	s := workflowTestServer(t, false)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/workflows/x/versions"},
		{http.MethodGet, "/api/workflows/x/diff?from=1&to=2"},
		{http.MethodPost, "/api/workflows/x/1/confirm"},
		{http.MethodGet, "/api/workflows/x/1/labels"},
	} {
		if rec := wfReq(s, c.method, c.path, nil, true); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	s = workflowTestServer(t, true)
	if rec := wfReq(s, http.MethodPost, "/api/workflows/x/1/confirm", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("confirm owner gate: %d", rec.Code)
	}
}

func TestWorkflowVersionsAndDiff(t *testing.T) {
	s := workflowTestServer(t, true)
	m := techdocsModel(t)
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	m2 := techdocsModel(t)
	m2.Spec.Meta.Version = "2"
	m2.Spec.States[0].Label = "Requested!"
	m2.Spec.States = append(m2.Spec.States, workflowmodel.State{ID: "extra", Label: "Extra", Owner: workflowmodel.OwnerHuman})
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m2, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}

	rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/versions", nil, false)
	var vs struct{ Versions []wfstore.Entry }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &vs) != nil || len(vs.Versions) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/ghost/versions", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", rec.Code)
	}
	rec = wfReq(s, http.MethodGet, "/api/workflows/hive-default/versions", nil, false)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &vs) != nil || len(vs.Versions) != 1 || !vs.Versions[0].ReadOnly {
		t.Fatalf("builtin: %d %s", rec.Code, rec.Body)
	}

	for _, bad := range []string{"", "?from=1", "?to=2"} {
		if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff"+bad, nil, false); rec.Code != http.StatusBadRequest {
			t.Fatalf("diff %q: %d", bad, rec.Code)
		}
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff?from=1&to=9", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("diff missing to: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff?from=9&to=1", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("diff missing from: %d", rec.Code)
	}
	rec = wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff?from=1&to=2", nil, false)
	var d struct {
		From, To wfstore.Entry
		Diff     workflowmodel.ModelDiff
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if d.From.Version != "1" || d.To.Version != "2" || d.Diff.Empty || len(d.Diff.States.Added) != 1 || len(d.Diff.States.Changed) != 1 || d.Diff.Summary != "+1 state, 2 changed" {
		t.Fatalf("diff %+v", d.Diff)
	}
	// same version both sides is empty; cross-model diff against hive-default works
	rec = wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff?from=1&to=1", nil, false)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil || !d.Diff.Empty {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/diff?from=1&toName=hive-default&to="+hivedefault.Version, nil, false)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil || d.To.Name != hivedefault.Name || len(d.Diff.States.Added) == 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowConfirm(t *testing.T) {
	s := workflowTestServer(t, true)
	m := techdocsModel(t)
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceIngestedUnreviewed
	m.Spec.States[0].Lifecycle.Confidence = workflowmodel.ConfidenceInferred
	m.Spec.Events[0].Confidence = workflowmodel.ConfidenceInferred
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/hive-default/1/confirm", nil, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "read-only") {
		t.Fatalf("builtin: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/cncf-techdocs/9/confirm", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/Bad%20Name/1/confirm", nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", rec.Code)
	}

	rec := wfReq(s, http.MethodPost, "/api/workflows/cncf-techdocs/1/confirm", nil, true)
	var out struct {
		Entry     wfstore.Entry
		Confirmed int
		From      string
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if out.Entry.Version != "2" || out.Entry.Provenance != workflowmodel.ProvenanceLocal || out.Confirmed != 2 || out.From != "cncf-techdocs@1" || !out.Entry.Compiled || out.Entry.SavedBy != "andy" {
		t.Fatalf("%+v", out)
	}
	for _, dg := range out.Entry.Diagnostics {
		if dg.Code == workflowmodel.CodeInferred {
			t.Fatalf("confirmed version still warns inferred: %+v", out.Entry.Diagnostics)
		}
	}
	saved, _, err := s.deps.WorkflowStore.Get("cncf-techdocs", "2")
	if err != nil || workflowmodel.Inferred(saved) != 0 || saved.Metadata.Annotations[workflowmodel.AnnotationConfirmedBy] != "andy" {
		t.Fatalf("%v %+v", err, saved.Metadata)
	}
	// the original is untouched
	orig, _, err := s.deps.WorkflowStore.Get("cncf-techdocs", "1")
	if err != nil || workflowmodel.Inferred(orig) != 2 {
		t.Fatalf("%v inferred=%d", err, workflowmodel.Inferred(orig))
	}
	// confirming again allocates the next version
	rec = wfReq(s, http.MethodPost, "/api/workflows/cncf-techdocs/2/confirm", nil, true)
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Entry.Version != "3" || out.Confirmed != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !auditHasAction(t, s, "workflow_confirm") {
		t.Fatal("no workflow_confirm audit entry")
	}
}

func auditHasAction(t *testing.T, s *Server, action string) bool {
	t.Helper()
	rec := wfReq(s, http.MethodGet, "/api/audit?limit=50", nil, true)
	if rec.Code != 200 {
		t.Fatalf("audit: %d %s", rec.Code, rec.Body)
	}
	return strings.Contains(rec.Body.String(), `"`+action+`"`)
}

func TestWorkflowLabelsPreview(t *testing.T) {
	s := workflowTestServer(t, true)
	m := techdocsModel(t)
	m.Spec.Labels = workflowmodel.LabelSet{Prefix: "docs", Entries: []workflowmodel.LabelEntry{
		{Name: "docs/010-requested", State: "requested"},
		{Name: "docs/020-analysing", Color: "123456", Description: strings.Repeat("x", 200), State: "analysing"},
	}}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/1/labels", nil, false)
	var out struct {
		Name, Version, Prefix string
		Labels                []workflowmodel.LabelDef
		Commands              []struct {
			State string
			Verbs []string
		}
		Stages []struct {
			State string
			Label string
		}
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if out.Name != "cncf-techdocs" || out.Prefix != "docs" || len(out.Labels) != 2 {
		t.Fatalf("%+v", out)
	}
	if out.Labels[0].Color != stagelabels.StageColor || out.Labels[0].Description != m.Spec.States[0].Label || out.Labels[1].Color != "123456" || len(out.Labels[1].Description) >= 200 {
		t.Fatalf("labels %+v", out.Labels)
	}
	verbs := map[string][]string{}
	for _, c := range out.Commands {
		verbs[c.State] = c.Verbs
	}
	if len(verbs["analysing"]) != 1 || verbs["analysing"][0] != "hand back" || len(verbs["requested"]) != 0 {
		t.Fatalf("commands %+v", out.Commands)
	}
	if len(out.Stages) == 0 || out.Stages[0].Label == "" {
		t.Fatalf("stages %+v", out.Stages)
	}

	// hive-default previews the built-in family
	rec = wfReq(s, http.MethodGet, "/api/workflows/hive-default/"+hivedefault.Version+"/labels", nil, false)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Labels) == 0 || out.Labels[0].Color != stagelabels.StageColor {
		t.Fatalf("builtin: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/9/labels", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	// a draft that does not compile answers 400 with its diagnostics
	draft := techdocsModel(t)
	draft.Spec.Meta.Version = "draft"
	draft.Spec.Gates[0].ReleasedBy = ""
	if rec := wfReq(s, http.MethodPost, "/api/workflows", draft, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec = wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/draft/labels", nil, false)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"diagnostics"`) {
		t.Fatalf("draft: %d %s", rec.Code, rec.Body)
	}
}
