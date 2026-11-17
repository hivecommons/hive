package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/claims"
)

// POST /api/workflows/dispatch is the owner's "Send to <profile>": owner
// only, it kicks the prepared agent, records the hand-off only once the
// kick landed, audits it, and maps the prepare errors to status codes.
func TestWorkflowDispatchEndpoint(t *testing.T) {
	s := workflowTestServer(t, true)
	rt := cruxtest.NewRecorder()
	rt.Add("writer", "writer", crux.StateIdle)
	s.deps.AgentMgr = rt
	recorded := 0
	var gotBy string
	s.deps.WorkflowDispatchPrepare = func(_ context.Context, repo string, number int, by string, _ *claims.Ledger) (WorkflowDispatchPlan, error) {
		gotBy = by
		switch number {
		case 1:
			return WorkflowDispatchPlan{Ref: repo + "#1", Model: "m@1", State: "accepted", Profile: "techdocs-drafter", Agent: "writer", Message: "draft it",
				Record: func(time.Time) { recorded++ }}, nil
		case 2:
			return WorkflowDispatchPlan{}, ErrWorkflowDispatchNotOwned
		case 3:
			return WorkflowDispatchPlan{}, fmt.Errorf("wrapped: %w", ErrWorkflowDispatchHeld)
		case 4:
			return WorkflowDispatchPlan{}, ErrWorkflowDispatchNoAgent
		}
		return WorkflowDispatchPlan{}, errors.New("github down")
	}

	body := map[string]any{"repo": "cncf/techdocs", "number": 1}
	if rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", body, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d %s", rec.Code, rec.Body)
	}
	if recorded != 0 || len(rt.Calls()) != 0 {
		t.Fatal("a non-owner request reached the dispatcher")
	}
	for _, bad := range []any{"{", map[string]any{"repo": "cncf/techdocs"}, map[string]any{"number": 1}} {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", bad, true); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad body %v: %d", bad, rec.Code)
		}
	}

	rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "sent" || resp["agent"] != "writer" || resp["profile"] != "techdocs-drafter" || resp["ref"] != "cncf/techdocs#1" {
		t.Fatalf("response %v", resp)
	}
	if recorded != 1 || gotBy != "andy" {
		t.Fatalf("recorded %d by %q", recorded, gotBy)
	}
	kicked := false
	for _, c := range rt.Calls() {
		if c.Method == "SendKick" && len(c.Args) == 2 && c.Args[0] == "writer" && c.Args[1] == "draft it" {
			kicked = true
		}
	}
	if !kicked {
		t.Fatalf("no kick to writer: %v", rt.Calls())
	}

	for number, want := range map[int]int{2: http.StatusNotFound, 3: http.StatusConflict, 4: http.StatusConflict, 5: http.StatusBadGateway} {
		if rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", map[string]any{"repo": "cncf/techdocs", "number": number}, true); rec.Code != want {
			t.Errorf("number %d: %d want %d (%s)", number, rec.Code, want, rec.Body)
		}
	}

	// A busy agent is a 409 and records nothing: the hand-off stays open.
	rt.KickErr = crux.ErrAgentBusy
	if rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", body, true); rec.Code != http.StatusConflict {
		t.Fatalf("busy agent: %d", rec.Code)
	}
	if recorded != 1 {
		t.Fatalf("a refused kick was recorded (%d)", recorded)
	}

	s.deps.WorkflowDispatchPrepare = nil
	if rec := wfReq(s, http.MethodPost, "/api/workflows/dispatch", body, true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: %d", rec.Code)
	}
}
