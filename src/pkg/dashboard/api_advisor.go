package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

// Advisor records over REST (hivecommons/hive#9722): the listing the agent
// page, hivectl, the TUI and (in a later phase) the admin MCP read from. It
// sits behind the dashboard's existing authentication and role checks — the
// guard invariant from #7563: no new way in, no new authority. Records carry
// transcript-derived advisor text, so the floor matches /api/audit's
// read-write requirement rather than the read floor.

// advisorRecordsDefaultLimit bounds one listing response, same posture as the
// audit listing's cap.
const advisorRecordsDefaultLimit = 200

// SetAdvisorRecords wires the advisor record store the listing serves from.
// A nil store leaves the endpoint answering with empty listings.
func (s *Server) SetAdvisorRecords(store *advisor.Store) {
	s.advisorRecords = store
}

// SetAdvisorStatusResolver wires the live "is the advisor active for this
// agent, and if not why" predicate (config.AdvisorActiveForAgent), so the
// listing can report an advisor configured on an unsupported backend as not
// active instead of silently showing no records.
func (s *Server) SetAdvisorStatusResolver(fn func(agent string) (bool, string)) {
	s.advisorStatus = fn
}

// advisorRecordsResponse is the GET /api/advisor/records payload.
type advisorRecordsResponse struct {
	Records []advisor.Record `json:"records"`
	// Active and ActiveReason are present only when the listing is filtered
	// to one agent: whether the advisor currently reviews it, and when it
	// does not, the operator-readable reason (not enabled, or an unsupported
	// backend — refused, never silently skipped).
	Active       *bool  `json:"active,omitempty"`
	ActiveReason string `json:"active_reason,omitempty"`
}

// handleAdvisorRecords serves GET /api/advisor/records?agent=&since=&limit=.
// Requires read-write or higher — the same floor as the audit listing, since
// advisor text quotes agent transcripts.
func (s *Server) handleAdvisorRecords(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		jsonError(w, "insufficient access", http.StatusForbidden)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	var since time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(w, "since must be RFC3339", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	limit := advisorRecordsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			jsonError(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if parsed < limit {
			limit = parsed
		}
	}
	resp := advisorRecordsResponse{Records: []advisor.Record{}}
	if s.advisorRecords != nil {
		records := s.advisorRecords.List(agent, since, limit)
		// Records are scrubbed at write time; redact again at the boundary so
		// a rotated-in canary or token pattern can never ride a stale record
		// out (defense in depth, same as the pane/summary paths).
		for i := range records {
			records[i].Text = redactTokens(records[i].Text)
		}
		resp.Records = records
	}
	if agent != "" && s.advisorStatus != nil {
		active, reason := s.advisorStatus(agent)
		resp.Active = &active
		resp.ActiveReason = reason
	}
	jsonResponse(w, resp)
}
