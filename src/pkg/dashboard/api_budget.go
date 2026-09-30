package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
)

func (s *Server) handleBudgetIgnoreGet(w http.ResponseWriter, r *http.Request) {
	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"ignored": budget.IgnoreAll,
		"agents":  budget.IgnoredAgents,
		// by_agent lets the Budget config tab show each agent's tracked
		// token burn next to its exemption toggle.
		"by_agent": budget.ByAgent,
	})
}

func (s *Server) handleBudgetIgnoreSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// The dashboard checkbox sends {"ignored": bool} (global bypass);
	// {"ignored": [names]} sets the per-agent exemption list.
	var body struct {
		Ignored json.RawMessage `json:"ignored"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	var ignoreAll bool
	if err := json.Unmarshal(body.Ignored, &ignoreAll); err == nil {
		s.deps.Governor.SetBudgetIgnoreAll(ignoreAll)
	} else {
		var agents []string
		if err := json.Unmarshal(body.Ignored, &agents); err != nil {
			jsonError(w, "ignored must be a bool or a list of agent names", http.StatusBadRequest)
			return
		}
		s.deps.Governor.SetBudgetIgnored(agents)
	}

	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"status":  "updated",
		"ignored": budget.IgnoreAll,
		"agents":  budget.IgnoredAgents,
	})
}

// handleBudgetHistory serves the per-budget-window report (#4298): one row per
// CLOSED window, newest first, plus the window still open so an operator sees
// the whole picture in one response rather than joining two endpoints.
//
// `windows` is ALWAYS an array, never null — a hive that has not yet seen a
// roll returns `[]`, which a client renders as "no history yet" rather than
// crashing on a nil. That is the compatibility requirement #4298 names: new
// code must not break an environment that was never keeping this history.
func (s *Server) handleBudgetHistory(w http.ResponseWriter, r *http.Request) {
	windows := s.BudgetWindowHistory()
	if windows == nil {
		windows = []collect.BudgetWindowEntry{}
	}

	resp := map[string]any{"windows": windows}

	// The open window, straight from the live status, so the report reads
	// continuously from "now" back through the closed rows.
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status != nil {
		b := status.Budget
		current := map[string]any{
			"limit":     b.WeeklyBudget,
			"used":      b.Used,
			"pctUsed":   b.PctUsed,
			"exhausted": b.Exhausted,
		}
		if b.WindowStartsAt != "" {
			current["windowStart"] = b.WindowStartsAt
		}
		if b.WindowEndsAt != "" {
			current["windowEnd"] = b.WindowEndsAt
		}
		resp["current"] = current
	}

	jsonResponse(w, resp)
}
