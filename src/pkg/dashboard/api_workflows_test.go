package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

func workflowTestServer(t *testing.T, withStore bool) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	if withStore {
		deps.WorkflowStore = wfstore.Open(t.TempDir())
	}
	s.RegisterAPI(deps)
	return s
}

func techdocsModel(t *testing.T) *workflowmodel.Model {
	t.Helper()
	b, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func wfReq(s *Server, method, path string, body any, owner bool) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		if raw, ok := body.(string); ok {
			buf.WriteString(raw)
		} else {
			_ = json.NewEncoder(&buf).Encode(body)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "andy")
	if owner {
		req.Header.Set("X-Hive-Role", "owner")
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestWorkflowAPIDisabledWithoutStore(t *testing.T) {
	s := workflowTestServer(t, false)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/workflows"},
		{http.MethodGet, "/api/workflows/x/1"},
		{http.MethodPost, "/api/workflows"},
		{http.MethodDelete, "/api/workflows/x/1"},
	} {
		if rec := wfReq(s, c.method, c.path, nil, true); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// compile needs no store
	rec := wfReq(s, http.MethodPost, "/api/workflows/compile", techdocsModel(t), false)
	var out struct{ Compiled bool }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Compiled {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// binding a model requires the store
	rec = wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1", "name": "x", "version": "1"}, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWorkflowAPIOwnerGate(t *testing.T) {
	s := workflowTestServer(t, true)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/workflows"},
		{http.MethodPut, "/api/workflows/x/1"},
		{http.MethodDelete, "/api/workflows/x/1"},
		{http.MethodPut, "/api/workflows/bindings"},
	} {
		if rec := wfReq(s, c.method, c.path, techdocsModel(t), false); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}

func TestWorkflowAPILifecycle(t *testing.T) {
	s := workflowTestServer(t, true)
	m := techdocsModel(t)

	rec := wfReq(s, http.MethodGet, "/api/workflows", nil, false)
	var list struct {
		Workflows []wfstore.Entry
		Bindings  []json.RawMessage
		Enabled   bool
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Workflows) != 1 || list.Bindings == nil || list.Enabled {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	if rec := wfReq(s, http.MethodPost, "/api/workflows", "{", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m, true); rec.Code != http.StatusCreated {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/cncf-techdocs/9", m, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("path mismatch: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/cncf-techdocs/1", m, true); rec.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", hivedefault.Generate(), true); rec.Code != http.StatusForbidden {
		t.Fatalf("builtin save: %d", rec.Code)
	}

	rec = wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/1", nil, false)
	var got struct {
		Entry   wfstore.Entry
		Profile *workflowmodel.Profile
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Profile == nil || got.Entry.SavedBy != "andy" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/2", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/Bad%20Name/2", nil, false); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", rec.Code)
	}

	// drafts save but cannot be bound
	draft := techdocsModel(t)
	draft.Spec.Meta.Version = "draft"
	draft.Spec.Gates[0].ReleasedBy = ""
	if rec := wfReq(s, http.MethodPost, "/api/workflows", draft, true); rec.Code != http.StatusCreated {
		t.Fatalf("draft save: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1", "name": "cncf-techdocs", "version": "draft"}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bind draft: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", "{", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bind bad json: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "", "name": "cncf-techdocs", "version": "1"}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bind empty repo: %d", rec.Code)
	}

	rec = wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1", "name": "cncf-techdocs", "version": "1"}, true)
	var bind struct {
		Changed  bool
		Bindings []struct{ Repo, Name, Version string }
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &bind) != nil || !bind.Changed || len(bind.Bindings) != 1 {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	if b, ok := s.deps.Config.WorkflowBindingFor("myorg/repo1"); !ok || b.Version != "1" {
		t.Fatalf("%+v %v", b, ok)
	}
	rec = wfReq(s, http.MethodGet, "/api/workflows/bindings", nil, false)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"cncf-techdocs"`)) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// bound models cannot be deleted
	if rec := wfReq(s, http.MethodDelete, "/api/workflows/cncf-techdocs/1", nil, true); rec.Code != http.StatusConflict {
		t.Fatalf("delete bound: %d %s", rec.Code, rec.Body)
	}
	rec = wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1"}, true)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &bind) != nil || !bind.Changed || len(bind.Bindings) != 0 {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body)
	}
	rec = wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1"}, true)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &bind) != nil || bind.Changed {
		t.Fatalf("unbind again: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodDelete, "/api/workflows/cncf-techdocs/1", nil, true); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodDelete, "/api/workflows/cncf-techdocs/1", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodDelete, "/api/workflows/hive-default/1", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("delete builtin: %d", rec.Code)
	}

	// compile reports diagnostics for drafts and invalid shapes
	rec = wfReq(s, http.MethodPost, "/api/workflows/compile", draft, false)
	var comp struct {
		Compiled    bool
		Diagnostics []workflowmodel.Diagnostic
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &comp) != nil || comp.Compiled || len(comp.Diagnostics) == 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/compile", "{", false); rec.Code != http.StatusBadRequest {
		t.Fatalf("compile bad json: %d", rec.Code)
	}
}

func TestWorkflowBindingsWithoutConfig(t *testing.T) {
	s := workflowTestServer(t, true)
	s.deps.Config = nil
	if rec := wfReq(s, http.MethodGet, "/api/workflows/bindings", nil, false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "r"}, true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", rec.Code)
	}
	rec := wfReq(s, http.MethodGet, "/api/workflows", nil, false)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"bindings":[]`)) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestStageTrackerWorkflowsBlock(t *testing.T) {
	qualify := func(r string) string { return "myorg/" + r }
	if stageTrackerWorkflows(nil, nil, qualify) != nil {
		t.Fatal("nil config produced a block")
	}
	s := workflowTestServer(t, true)
	if stageTrackerWorkflows(s.deps.Config, s.workflowProfileLookup(), qualify) != nil {
		t.Fatal("unbound hive produced a block")
	}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", techdocsModel(t), true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	if _, err := s.deps.Config.SetWorkflowBinding("repo1", "cncf-techdocs", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deps.Config.SetWorkflowBinding("repo2", "ghost", "1"); err != nil {
		t.Fatal(err)
	}
	blk := stageTrackerWorkflows(s.deps.Config, s.workflowProfileLookup(), qualify)
	if blk == nil || blk.Default.Name != hivedefault.Name || len(blk.Default.Columns) != 7 {
		t.Fatalf("%+v", blk)
	}
	td := blk.Repos["myorg/repo1"]
	if td.Name != "cncf-techdocs" || td.Error != "" || len(td.Columns) != 5 {
		t.Fatalf("%+v", td)
	}
	var nums int
	for _, c := range td.Columns {
		nums += len(c.Numbers)
	}
	if nums == 0 {
		t.Fatalf("no stage numbers anchored: %+v", td.Columns)
	}
	if g := blk.Repos["myorg/repo2"]; g.Error == "" || g.Name != "ghost" {
		t.Fatalf("%+v", g)
	}
	noStore := stageTrackerWorkflows(s.deps.Config, nil, qualify)
	if noStore.Repos["myorg/repo1"].Error == "" {
		t.Fatalf("%+v", noStore.Repos)
	}
	// the tracker response carries it
	resp := s.buildStageTracker(&StatusPayload{HiveID: "h"}, nil, time.Now())
	if resp.Workflows == nil || len(resp.Workflows.Repos) != 2 {
		t.Fatalf("%+v", resp.Workflows)
	}
}
