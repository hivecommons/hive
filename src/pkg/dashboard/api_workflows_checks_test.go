package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/checksfile"
)

// PUT /api/workflows/bindings proposes the bound model's declared checks as
// .hive/workflows/checks.yml (hivecommons/hive#18355); an emit failure is a
// warning and never fails the binding, and unbinding emits nothing.
func TestWorkflowBindingEmitsChecks(t *testing.T) {
	s := workflowTestServer(t, true)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	type call struct {
		repo   string
		checks []workflowmodel.Check
	}
	var calls []call
	var emitErr error
	s.deps.WorkflowEmitChecks = func(_ context.Context, repo string, m *workflowmodel.Model) (checksfile.Result, error) {
		calls = append(calls, call{repo: repo, checks: m.Spec.Checks})
		if emitErr != nil {
			return checksfile.Result{}, emitErr
		}
		return checksfile.Result{Outcome: checksfile.OpenedPR, PR: 12}, nil
	}
	m := techdocsModel(t)
	m.Spec.Checks = []workflowmodel.Check{{ID: "check-build", Name: "build", Kind: workflowmodel.CheckCI, Required: true}}
	if rec := wfReq(s, http.MethodPost, "/api/workflows", m, true); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}

	bind := map[string]string{"repo": "repo1", "name": "cncf-techdocs", "version": "1"}
	rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", bind, true)
	var out struct {
		Changed  bool
		Checks   checksfile.Result
		Warnings []string
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Changed {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body)
	}
	if len(calls) != 1 || calls[0].repo != "repo1" || len(calls[0].checks) != 1 || calls[0].checks[0].ID != "check-build" {
		t.Fatalf("emit calls: %+v", calls)
	}
	if out.Checks.Outcome != checksfile.OpenedPR || out.Checks.PR != 12 || len(out.Warnings) != 0 {
		t.Fatalf("response: %+v", out)
	}

	emitErr = errors.New("no forge client")
	rec = wfReq(s, http.MethodPut, "/api/workflows/bindings", bind, true)
	out = struct {
		Changed  bool
		Checks   checksfile.Result
		Warnings []string
	}{}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Changed {
		t.Fatalf("rebind: %d %s", rec.Code, rec.Body)
	}
	if len(calls) != 2 || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], checksfile.Path) {
		t.Fatalf("rebind should retry the emit and warn: calls=%d %+v", len(calls), out)
	}

	if rec := wfReq(s, http.MethodPut, "/api/workflows/bindings", map[string]string{"repo": "repo1"}, true); rec.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body)
	}
	if len(calls) != 2 {
		t.Fatalf("unbind emitted: %d calls", len(calls))
	}
}
