package dashboard

// Approval desk API: authorization, listing, single and bulk resolve.
//
// The authorization tests matter most. Approving a pending tool call lets an
// agent take an action the hive's ACMM level otherwise reserved for a human, so
// these endpoints must be owner-gated exactly like token-access and backup.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/toolapprove"
)

// approvalServer wires a dashboard server with an enabled desk over a temp
// inbox, and seeds n pending operator-lane approvals.
func approvalServer(t *testing.T, n int) (*Server, *toolapprove.Inbox) {
	t.Helper()
	s, deps := apiServer(t)

	inbox := toolapprove.NewInbox(filepath.Join(t.TempDir(), "inbox.json"))
	eng, err := toolapprove.CompileRules([]toolapprove.Rule{{
		Name:   "needs-human",
		Expr:   `request.kind == "self-merge"`,
		Action: toolapprove.RuleActionOperatorApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	deps.ApprovalDesk = toolapprove.NewDesk(eng, nil)
	deps.ApprovalInbox = inbox

	for i := 1; i <= n; i++ {
		req := toolapprove.Request{
			Kind:   toolapprove.KindSelfMerge,
			Repo:   "hivecommons/hive",
			Number: i,
			Author: "hive-app[bot]",
			Tool:   toolapprove.ToolRequest{Tool: "hive-merge"},
		}
		v := toolapprove.Verdict{
			Decision:  toolapprove.DecisionOperatorApprove,
			ACMMLevel: 4,
			Tool:      "hive-merge",
			Rule:      "needs-human",
		}
		if _, err := inbox.Enqueue(req, v); err != nil {
			t.Fatalf("seed enqueue: %v", err)
		}
	}
	return s, inbox
}

// TestApprovalsRequireOwnerRole is the behavioural authorization guard on all
// three endpoints.
func TestApprovalsRequireOwnerRole(t *testing.T) {
	s, _ := approvalServer(t, 2)

	if rec := doGet(s, "/api/approvals"); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner GET /api/approvals = %d, want 403 — a read-only user "+
			"must not enumerate pending approvals (they quote tool arguments)", rec.Code)
	}

	for _, path := range []string{"/api/approvals/resolve", "/api/approvals/bulk"} {
		rec := doNonOwnerPost(s, path, map[string]any{"id": "x", "ids": []string{"x"}, "approved": true})
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-owner POST %s = %d, want 403 — a non-owner must not be able "+
				"to grant an agent an action the ACMM level reserved for a human", path, rec.Code)
		}
	}
}

// TestApprovalsReadWriteRoleRejected pins that a contributor (read-write) role
// is not sufficient — only owner.
func TestApprovalsReadWriteRoleRejected(t *testing.T) {
	s, _ := approvalServer(t, 1)

	req := httptest.NewRequest(http.MethodGet, "/api/approvals", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("read-write GET /api/approvals = %d, want 403", rec.Code)
	}
}

// TestApprovalsOwnerMarkerRequired pins that claiming the owner role without
// the SERVER-VERIFIED marker is refused — a spoofed header must not suffice.
func TestApprovalsOwnerMarkerRequired(t *testing.T) {
	s, _ := approvalServer(t, 1)

	req := httptest.NewRequest(http.MethodGet, "/api/approvals", nil)
	req.Header.Set("X-Hive-Role", "owner")
	// Deliberately NO ownerRoleVerifiedHeader — this is the spoof case.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("owner role WITHOUT the server-verified marker = %d, want 403 — "+
			"a client-supplied role header must never be sufficient", rec.Code)
	}
}

// TestApprovalsListReturnsPending is the positive control: with a real owner,
// the endpoint returns the seeded rows and the rule that would resolve them.
func TestApprovalsListReturnsPending(t *testing.T) {
	s, _ := approvalServer(t, 3)

	rec := doOwnerGet(s, "/api/approvals")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET /api/approvals = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp ApprovalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled {
		t.Error("response reports the desk disabled although an inbox is wired")
	}
	if resp.Count != 3 || len(resp.Items) != 3 {
		t.Fatalf("count=%d items=%d, want 3 and 3", resp.Count, len(resp.Items))
	}
	for _, item := range resp.Items {
		if item.Rule != "needs-human" {
			t.Errorf("row %s does not show which rule would resolve it: %q", item.ID, item.Rule)
		}
		if item.Kind != toolapprove.KindSelfMerge {
			t.Errorf("row %s lost its kind: %q", item.ID, item.Kind)
		}
	}
	if len(resp.RuleChips) != 1 || resp.RuleChips[0] != "needs-human" {
		t.Errorf("RuleChips = %v, want [needs-human]", resp.RuleChips)
	}
}

// TestApprovalsDisabledDeskReportsCleanly pins that a hive without the feature
// gets an honest "not enabled" 200 rather than a 404 that looks broken.
func TestApprovalsDisabledDeskReportsCleanly(t *testing.T) {
	s, _ := apiServer(t) // no ApprovalDesk / ApprovalInbox

	rec := doOwnerGet(s, "/api/approvals")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET with desk disabled = %d, want 200", rec.Code)
	}
	var resp ApprovalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Error("desk reports enabled with no inbox wired")
	}
	if resp.Items == nil {
		t.Error("Items is null rather than an empty array — the UI would need a null guard")
	}
}

// TestApprovalResolveSingle exercises the single-item resolve path end to end.
func TestApprovalResolveSingle(t *testing.T) {
	s, inbox := approvalServer(t, 2)
	id := inbox.List()[0].ID

	rec := doOwnerPost(s, "/api/approvals/resolve", map[string]any{"id": id, "approved": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := inbox.Count(); got != 1 {
		t.Errorf("after resolving 1 of 2, %d pending, want 1", got)
	}

	// Re-resolving the same ID must be a 409, not a second execution.
	rec = doOwnerPost(s, "/api/approvals/resolve", map[string]any{"id": id, "approved": true})
	if rec.Code != http.StatusConflict {
		t.Errorf("re-resolve = %d, want 409 — a replayed grant must not double-execute", rec.Code)
	}
}

// TestApprovalBulkResolvesEachIndividually pins that the bulk endpoint returns
// a per-item result list and actually resolves each item.
func TestApprovalBulkResolvesEachIndividually(t *testing.T) {
	s, inbox := approvalServer(t, 5)

	var ids []string
	for _, p := range inbox.List() {
		ids = append(ids, p.ID)
	}

	rec := doOwnerPost(s, "/api/approvals/bulk", map[string]any{"ids": ids, "approved": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results  []toolapprove.BulkResolveResult `json:"results"`
		Resolved int                             `json:"resolved"`
		Pending  int                             `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 5 {
		t.Fatalf("bulk returned %d per-item results for 5 ids", len(resp.Results))
	}
	if resp.Resolved != 5 || resp.Pending != 0 {
		t.Errorf("resolved=%d pending=%d, want 5 and 0", resp.Resolved, resp.Pending)
	}
	if inbox.Count() != 0 {
		t.Errorf("%d items still pending after a bulk approve of all", inbox.Count())
	}
}

// TestApprovalBulkRejectsOversizedBatch pins the blast-radius cap.
func TestApprovalBulkRejectsOversizedBatch(t *testing.T) {
	s, _ := approvalServer(t, 1)

	ids := make([]string, maxBulkApprovalsPerRequest+1)
	for i := range ids {
		ids[i] = "id"
	}
	rec := doOwnerPost(s, "/api/approvals/bulk", map[string]any{"ids": ids, "approved": true})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized bulk = %d, want 400", rec.Code)
	}
}

// TestApprovalsSourceGate is the source-level invariant, mirroring the pattern
// used for token-access: every handler in this file must carry the owner gate,
// so a sync merge or refactor cannot silently drop it.
func TestApprovalsSourceGate(t *testing.T) {
	raw := f16ReadSource(t, "api_approvals.go")

	for _, handler := range []string{"handleApprovalsList", "handleApprovalResolve", "handleApprovalBulk"} {
		body := f16HandlerBody(t, raw, handler)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s has no requireOwnerRole gate — a non-owner could resolve pending "+
				"approvals and grant agents actions the ACMM level reserved for a human. "+
				"Restore the gate; do not remove this test.", handler)
		}
	}

	// Count floor: three handlers, three gates. A refactor that collapses the
	// handlers must not reduce the number of gates.
	got := len(regexp.MustCompile(`requireOwnerRole\(w, r\)`).FindAllString(raw, -1))
	if got < 3 {
		t.Errorf("api_approvals.go has %d requireOwnerRole gates, want at least 3", got)
	}
}

// doNonOwnerPost posts WITHOUT the owner headers.
func doNonOwnerPost(s *Server, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}
