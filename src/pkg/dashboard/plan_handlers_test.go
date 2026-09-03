package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agentparse"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planning"
)

// The plan MUTATION routes (approve/reject/child) are owner-only as of audit
// F16 — approval is what releases a draft epic's children into agent execution.
// The tests below exercise happy paths, not-found and input validation, so they
// call markOwnerRequest to supply credentials and keep their original
// assertions. Authorization itself is covered by f16_owner_gate_test.go.
//
// The read-only GET route (handlePlanTree) is deliberately NOT owner-gated, so
// its tests intentionally send no credentials — do not "fix" them by adding
// markOwnerRequest; that would hide a regression if the GET were ever gated.

// planServer builds a Server with a single "architect" bead store and an epic
// decomposed into children. It returns the server, store, and epic.
func planServer(t *testing.T) (*Server, *beads.Store, *beads.Bead) {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("plan epic", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	plan := "1. [T1] design [agent_suitable]\n2. [T2] review (depends: T1) [human_required]\n"
	if _, err := planning.DecomposeFromOutput(store, epic, plan, planning.Options{}); err != nil {
		t.Fatalf("decompose: %v", err)
	}

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.RegisterAPI(srv.deps)
	return srv, store, epic
}

func TestHandlePlanTree(t *testing.T) {
	srv, _, epic := planServer(t)

	req := httptest.NewRequest("GET", "/api/plan/"+epic.ID, nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool              `json:"ok"`
		Plan planning.PlanTree `json:"plan"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Plan.EpicID != epic.ID || len(resp.Plan.Children) != 2 {
		t.Fatalf("unexpected plan: %+v", resp.Plan)
	}
	if resp.Plan.PlanStatus != planning.PlanStatusDraft {
		t.Fatalf("want draft, got %q", resp.Plan.PlanStatus)
	}
}

func TestHandlePlanTree_NotFound(t *testing.T) {
	srv, _, _ := planServer(t)
	req := httptest.NewRequest("GET", "/api/plan/does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandlePlanApprove(t *testing.T) {
	srv, store, epic := planServer(t)

	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/approve", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("epic not approved: %q", got.Meta(planning.MetaPlanStatus))
	}

	// Double-approve → 400.
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/approve", nil)
	markOwnerRequest(req2)
	srv.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("double approve want 400, got %d", w2.Code)
	}
}

func TestHandlePlanApprove_NotFound(t *testing.T) {
	srv, _, _ := planServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/plan/missing/approve", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandlePlanReject(t *testing.T) {
	srv, store, epic := planServer(t)
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/reject", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := store.Get(epic.ID)
	if got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("want draft after reject, got %q", got.Meta(planning.MetaPlanStatus))
	}
}

func TestHandlePlanChild_Retag(t *testing.T) {
	srv, store, epic := planServer(t)
	children := store.List(beads.ListFilter{})
	var child *beads.Bead
	for _, b := range children {
		if b.Meta(planning.MetaParentEpic) == epic.ID {
			child = b
			break
		}
	}
	if child == nil {
		t.Fatal("no child found")
	}

	body := strings.NewReader(`{"action":"retag","execution":"human_required"}`)
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/child/"+child.ID, body)
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := store.Get(child.ID)
	if got.Meta(planning.MetaExecution) != agentparse.ExecutionHumanRequired {
		t.Fatalf("retag failed: %q", got.Meta(planning.MetaExecution))
	}
}

func TestHandlePlanChild_Remove(t *testing.T) {
	srv, store, epic := planServer(t)
	var child *beads.Bead
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(planning.MetaParentEpic) == epic.ID {
			child = b
			break
		}
	}
	body := strings.NewReader(`{"action":"remove"}`)
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/child/"+child.ID, body)
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got, _ := store.Get(child.ID)
	if got.Status != beads.StatusClosed {
		t.Fatalf("want closed, got %q", got.Status)
	}
}

func TestHandlePlanChild_BadAction(t *testing.T) {
	srv, store, epic := planServer(t)
	var child *beads.Bead
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(planning.MetaParentEpic) == epic.ID {
			child = b
			break
		}
	}
	body := strings.NewReader(`{"action":"bogus"}`)
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/child/"+child.ID, body)
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad action, got %d", w.Code)
	}
}

func TestHandlePlanReject_BadState(t *testing.T) {
	// Reject an undecomposed epic → planning.RejectPlan errors → 400.
	store, _ := beads.NewStore(t.TempDir())
	epic, _ := store.Create("undecomposed", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.RegisterAPI(srv.deps)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/reject", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandlePlanReject_NotFound(t *testing.T) {
	srv, _, _ := planServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/plan/missing/reject", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandlePlanChild_NotFound(t *testing.T) {
	srv, _, _ := planServer(t)
	body := strings.NewReader(`{"action":"remove"}`)
	req := httptest.NewRequest("POST", "/api/plan/missing/child/x", body)
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandlePlanChild_BadBody(t *testing.T) {
	srv, _, epic := planServer(t)
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/child/x", strings.NewReader("{not json"))
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad body, got %d", w.Code)
	}
}

func TestHandlePlanChild_RetagError(t *testing.T) {
	srv, store, epic := planServer(t)
	var child *beads.Bead
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Meta(planning.MetaParentEpic) == epic.ID {
			child = b
			break
		}
	}
	// Invalid execution value → RetagChild errors → 400.
	body := strings.NewReader(`{"action":"retag","execution":"bogus"}`)
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/child/"+child.ID, body)
	markOwnerRequest(req)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid execution, got %d", w.Code)
	}
}

func TestHandlePlanTree_NonEpicBead(t *testing.T) {
	// A task bead ID resolves to no epic store → 404.
	store, _ := beads.NewStore(t.TempDir())
	task, _ := store.Create("task", beads.TypeTask, beads.PriorityLow, "worker", "")
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.RegisterAPI(srv.deps)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/plan/"+task.ID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 for non-epic bead, got %d", w.Code)
	}
}

func TestHandlePlan_NoStores(t *testing.T) {
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Logger: slog.Default(), Ctx: context.Background()}
	srv.RegisterAPI(srv.deps)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/plan/x", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404 with no stores, got %d", w.Code)
	}
}

func TestBuildPlanning(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())

	// Draft epic with children.
	draft, _ := store.Create("draft", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, draft, "1. [T1] a [agent_suitable]\n", planning.Options{})

	// Approved epic with an open child → decomposing.
	appr, _ := store.Create("appr", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, appr, "1. [T1] b [agent_suitable]\n", planning.Options{AutoApprove: true})

	// A plain epic never decomposed → not counted.
	store.Create("plain", beads.TypeEpic, beads.PriorityHigh, "architect", "")

	fp := BuildPlanning(map[string]*beads.Store{"architect": store}, false, 6)
	if fp.ActivePlans != 2 {
		t.Errorf("ActivePlans: want 2, got %d", fp.ActivePlans)
	}
	if fp.AwaitingReview != 1 {
		t.Errorf("AwaitingReview: want 1, got %d", fp.AwaitingReview)
	}
	if fp.Decomposing != 1 {
		t.Errorf("Decomposing: want 1, got %d", fp.Decomposing)
	}
	if fp.Replans24h != 0 {
		t.Errorf("Replans24h: want 0, got %d", fp.Replans24h)
	}
}

func TestBuildPlanning_Replans24h(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	now := time.Now()

	// Approved epic replanned 2h ago → counted.
	recent, _ := store.Create("recent", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, recent, "1. [T1] a [agent_suitable]\n", planning.Options{AutoApprove: true})
	store.SetMetadata(recent.ID, planning.MetaLastReplanAt, now.Add(-2*time.Hour).Format(time.RFC3339))

	// Approved epic replanned 30h ago → outside the 24h window, not counted.
	old, _ := store.Create("old", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, old, "1. [T1] b [agent_suitable]\n", planning.Options{AutoApprove: true})
	store.SetMetadata(old.ID, planning.MetaLastReplanAt, now.Add(-30*time.Hour).Format(time.RFC3339))

	// Approved epic with a malformed timestamp → ignored, not counted.
	bad, _ := store.Create("bad", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, bad, "1. [T1] c [agent_suitable]\n", planning.Options{AutoApprove: true})
	store.SetMetadata(bad.ID, planning.MetaLastReplanAt, "not-a-timestamp")

	fp := buildPlanningAt(map[string]*beads.Store{"architect": store}, false, 6, now)
	if fp.Replans24h != 1 {
		t.Errorf("Replans24h: want 1 (only the 2h-ago replan), got %d", fp.Replans24h)
	}
}

func TestBuildPlanning_AvailableGate(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	// L4: planning not available (architect not scheduled) -> Available false.
	if fp := BuildPlanning(map[string]*beads.Store{"architect": store}, false, 4); fp.Available {
		t.Errorf("Available should be false at ACMM L4")
	}
	// L5 and L6: available.
	if fp := BuildPlanning(map[string]*beads.Store{"architect": store}, false, 5); !fp.Available {
		t.Errorf("Available should be true at ACMM L5")
	}
	if fp := BuildPlanning(map[string]*beads.Store{"architect": store}, false, 6); !fp.Available {
		t.Errorf("Available should be true at ACMM L6")
	}
}

func TestBuildPlanning_Empty(t *testing.T) {
	fp := BuildPlanning(map[string]*beads.Store{}, false, 6)
	if fp.ActivePlans != 0 || fp.AwaitingReview != 0 {
		t.Errorf("empty stores should yield zero planning metric, got %+v", fp)
	}
}
