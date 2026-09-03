package dashboard

// Operator approval desk API (RFC #4000).
//
//	GET  /api/approvals            — list pending operator-lane approvals
//	POST /api/approvals/resolve    — resolve ONE approval
//	POST /api/approvals/bulk       — resolve MANY, as N individual resolutions
//
// All three are owner-gated with requireOwnerRole, which additionally demands
// the server-verified marker header — the same bar the token-access, backup,
// and governor-security endpoints enforce. Approving a pending tool call is at
// least as privileged as those: a granted verdict lets an agent take an action
// the hive's ACMM level otherwise reserved for a human.
//
// The bulk path is deliberately NOT a parallel implementation. It calls
// Inbox.ResolveMany, which calls Inbox.Resolve once per item — the same
// function the single-item handler calls — and returns a per-item result list.
// This mirrors runBulkAction/applyBulkAction in pkg/hub/saas_bulk.go, and it is
// the shape RFC #4000 requires: "a bulk approve is N individual evaluations
// through the same decision function every agent request goes through".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/toolapprove"
)

// maxBulkApprovalsPerRequest caps how many approvals one bulk call may resolve.
// The canonical case from fleet-owner feedback is "bulk approve fifty green
// dependabot PRs", so this covers a select-all comfortably while keeping a
// buggy or malicious client from asking the hive to journal an unbounded number
// of resolutions in one request.
const maxBulkApprovalsPerRequest = 200

// maxApprovalRequestBodyBytes caps a request body. The largest legitimate body
// is a bulk call carrying maxBulkApprovalsPerRequest 32-char IDs plus a short
// rationale, so 64KiB is generous — the same ceiling saas_bulk.go applies.
const maxApprovalRequestBodyBytes = 64 * 1024

// registerApprovalRoutes wires the desk endpoints. Kept in its own file and its
// own register function so this feature's diff does not collide with the
// concurrently-edited route tables, matching registerBulkRoutes' rationale.
func (s *Server) registerApprovalRoutes() {
	s.mux.HandleFunc("GET /api/approvals", s.handleApprovalsList)
	s.mux.HandleFunc("POST /api/approvals/resolve", s.handleApprovalResolve)
	s.mux.HandleFunc("POST /api/approvals/bulk", s.handleApprovalBulk)
}

// approvalInbox returns the server's inbox, or nil when the desk is disabled.
// Every handler tolerates nil by reporting an empty, disabled desk rather than
// erroring: with the feature off the panel should render as "not enabled", not
// as a broken endpoint.
func (s *Server) approvalInbox() *toolapprove.Inbox {
	if s.deps == nil {
		return nil
	}
	return s.deps.ApprovalInbox
}

// approvalDesk returns the server's desk, or nil when disabled.
func (s *Server) approvalDesk() *toolapprove.Desk {
	if s.deps == nil {
		return nil
	}
	return s.deps.ApprovalDesk
}

// ApprovalRow is one pending item as the dashboard renders it.
type ApprovalRow struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Tool      string `json:"tool"`
	Agent     string `json:"agent"`
	Repo      string `json:"repo,omitempty"`
	Number    int    `json:"number,omitempty"`
	Title     string `json:"title,omitempty"`
	Rationale string `json:"rationale"`
	QueuedAt  string `json:"queued_at"`
	ACMMLevel int    `json:"acmm_level"`
	// Rule is the operator rule that WOULD resolve this item, re-evaluated live
	// against the current rule set rather than replayed from the enqueue-time
	// verdict. An operator editing rules sees the effect on the pending queue
	// immediately, which is the "tuning knobs embedded in the review loop"
	// behavior the fleet-owner feedback asked for.
	Rule string `json:"rule,omitempty"`
	// RuleAction is what that rule asks for.
	RuleAction string `json:"rule_action,omitempty"`
	// PolicyBug marks an item queued on a hive at ACMM L6+. Per requirement 4
	// of the throughput contract, an inbox accumulating at full autonomy is a
	// misconfiguration signal, not a normal wait — the UI renders these
	// distinctly rather than letting them sit politely.
	PolicyBug bool `json:"policy_bug,omitempty"`
}

// ApprovalsResponse is the GET /api/approvals body.
type ApprovalsResponse struct {
	// Enabled reports whether the desk is configured on this hive.
	Enabled bool `json:"enabled"`
	// Count is the pending total — the dashboard's badge value.
	Count int `json:"count"`
	// Items are the pending rows.
	Items []ApprovalRow `json:"items"`
	// RuleChips are the distinct rule names across pending items, for filters.
	RuleChips []string `json:"rule_chips"`
	// PolicyBugCount is how many pending items are queued at L6+.
	PolicyBugCount int `json:"policy_bug_count"`
	// ACMMLevel is the hive's current level, so the panel can explain why a
	// lane resolved the way it did.
	ACMMLevel int `json:"acmm_level"`
}

// handleApprovalsList returns the pending operator-lane approvals.
func (s *Server) handleApprovalsList(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	inbox := s.approvalInbox()
	if inbox == nil {
		jsonResponse(w, ApprovalsResponse{Enabled: false, Items: []ApprovalRow{}, RuleChips: []string{}})
		return
	}

	desk := s.approvalDesk()
	pending := inbox.List()
	rows := make([]ApprovalRow, 0, len(pending))
	policyBugs := 0

	for _, p := range pending {
		row := ApprovalRow{
			ID:        p.ID,
			Kind:      p.Request.Kind,
			Tool:      p.Verdict.Tool,
			Agent:     p.Verdict.Agent,
			Repo:      p.Request.Repo,
			Number:    p.Request.Number,
			Title:     p.Request.Title,
			Rationale: p.Verdict.Rationale,
			QueuedAt:  p.QueuedAt.UTC().Format("2006-01-02T15:04:05Z"),
			ACMMLevel: p.ACMMLevel,
		}
		// Re-evaluate which rule would resolve this item against the CURRENT
		// rule set, so the panel reflects live policy rather than a snapshot.
		if desk != nil {
			if m, ok := desk.WouldMatchRule(p.Request, p.ACMMLevel); ok {
				row.Rule = m.Name
				row.RuleAction = string(m.Action)
			}
		}
		if p.ACMMLevel >= 6 {
			row.PolicyBug = true
			policyBugs++
		}
		rows = append(rows, row)
	}

	chips := inbox.RuleChips()
	if chips == nil {
		chips = []string{}
	}

	jsonResponse(w, ApprovalsResponse{
		Enabled:        true,
		Count:          len(rows),
		Items:          rows,
		RuleChips:      chips,
		PolicyBugCount: policyBugs,
		ACMMLevel:      s.currentACMMLevel(),
	})
}

// ApprovalResolveRequest is the POST /api/approvals/resolve body.
type ApprovalResolveRequest struct {
	ID        string `json:"id"`
	Approved  bool   `json:"approved"`
	Rationale string `json:"rationale,omitempty"`
}

// handleApprovalResolve resolves ONE pending approval.
func (s *Server) handleApprovalResolve(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	inbox := s.approvalInbox()
	if inbox == nil {
		jsonError(w, "approval desk is not enabled on this hive", http.StatusNotFound)
		return
	}

	var req ApprovalResolveRequest
	if err := decodeApprovalBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	operator := approvalOperator(r)
	rec, err := inbox.Resolve(req.ID, req.Approved, operator, req.Rationale)
	if err != nil {
		// A replayed resolve is a 409, not a 500: the caller's intent was
		// already satisfied and re-executing is exactly what the journal
		// prevents. The original outcome is returned so the client can
		// reconcile its stale UI.
		if strings.Contains(err.Error(), toolapprove.ErrAlreadyResolved.Error()) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":       err.Error(),
				"id":          rec.ID,
				"approved":    rec.Approved,
				"executed":    rec.Executed,
				"resolved_at": rec.ResolvedAt,
			})
			return
		}
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	s.auditFromRequest(r, "approval-resolve",
		auditDetail("id", req.ID, "approved", fmt.Sprintf("%t", req.Approved)), "")

	jsonResponse(w, map[string]any{
		"ok":          true,
		"id":          rec.ID,
		"approved":    rec.Approved,
		"resolved_at": rec.ResolvedAt,
		"pending":     inbox.Count(),
	})
}

// ApprovalBulkRequest is the POST /api/approvals/bulk body.
type ApprovalBulkRequest struct {
	IDs       []string `json:"ids"`
	Approved  bool     `json:"approved"`
	Rationale string   `json:"rationale,omitempty"`
}

// handleApprovalBulk resolves MANY approvals as N individual resolutions.
//
// Partial failure is the normal case (an item may have been resolved by another
// operator between the list and the click), so the response is a per-item
// result list rather than a single boolean — the same contract BulkHiveResult
// establishes for bulk hive actions.
func (s *Server) handleApprovalBulk(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	inbox := s.approvalInbox()
	if inbox == nil {
		jsonError(w, "approval desk is not enabled on this hive", http.StatusNotFound)
		return
	}

	var req ApprovalBulkRequest
	if err := decodeApprovalBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		jsonError(w, "ids is required", http.StatusBadRequest)
		return
	}
	if len(req.IDs) > maxBulkApprovalsPerRequest {
		jsonError(w, fmt.Sprintf("too many ids: %d (max %d)", len(req.IDs), maxBulkApprovalsPerRequest),
			http.StatusBadRequest)
		return
	}

	operator := approvalOperator(r)
	// N individual resolutions through the SAME Resolve the single path uses.
	results := inbox.ResolveMany(req.IDs, req.Approved, operator, req.Rationale)

	okCount := 0
	for _, res := range results {
		if res.Ok {
			okCount++
		}
	}

	s.auditFromRequest(r, "approval-bulk",
		auditDetail("count", fmt.Sprintf("%d", len(req.IDs)),
			"resolved", fmt.Sprintf("%d", okCount),
			"approved", fmt.Sprintf("%t", req.Approved)), "")

	jsonResponse(w, map[string]any{
		"ok":       true,
		"results":  results,
		"resolved": okCount,
		"pending":  inbox.Count(),
	})
}

// decodeApprovalBody reads a bounded JSON body.
func decodeApprovalBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxApprovalRequestBodyBytes))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

// approvalOperator resolves who is resolving, for the journal and audit log.
func approvalOperator(r *http.Request) string {
	if u := r.Header.Get("X-Hive-User"); u != "" {
		return u
	}
	return "local"
}

// currentACMMLevel reads the hive's configured level for display, defaulting to
// the most restrictive reading when unset — the same fail-closed default
// toolapprove.ACMMLevelOf applies.
func (s *Server) currentACMMLevel() int {
	if s.deps == nil || s.deps.Config == nil || s.deps.Config.ACMMLevel == nil {
		return 0
	}
	return *s.deps.Config.ACMMLevel
}
