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
// page, hivectl, the TUI and the admin MCP advisor_records tool (#9725) read
// from, plus the per-agent advisor spend aggregate beside it. It
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

// advisorWindowMaxHours bounds the hours= lookback, the same 30-day ceiling
// /api/trends applies to its hours= parameter.
const advisorWindowMaxHours = 720

// advisorSpendRanges mirrors the Cost section's timeframe selector
// (COST_RANGES in static/index.html): each preset is a window anchored at now,
// so advisor spend is reported over exactly the ranges the existing spend
// reporting offers. A custom range is expressed with since/until.
var advisorSpendRanges = map[string]time.Duration{
	"hourly":  24 * time.Hour,
	"daily":   30 * 24 * time.Hour,
	"weekly":  12 * 7 * 24 * time.Hour,
	"monthly": 12 * 30 * 24 * time.Hour,
}

// advisorWindow parses the shared time-window parameters of the advisor
// endpoints: since and until (RFC3339, either may be omitted), or hours (a
// lookback from now, 1..720) as an alternative to since. The returned message
// is non-empty when the parameters are invalid.
func advisorWindow(r *http.Request, now time.Time) (since, until time.Time, msg string) {
	q := r.URL.Query()
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return since, until, "since must be RFC3339"
		}
		since = parsed
	}
	if raw := strings.TrimSpace(q.Get("until")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return since, until, "until must be RFC3339"
		}
		until = parsed
	}
	if raw := strings.TrimSpace(q.Get("hours")); raw != "" {
		if !since.IsZero() {
			return since, until, "use either since or hours, not both"
		}
		hours, err := strconv.Atoi(raw)
		if err != nil || hours <= 0 || hours > advisorWindowMaxHours {
			return since, until, "hours must be an integer between 1 and 720"
		}
		since = now.Add(-time.Duration(hours) * time.Hour)
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return since, until, "until must not be before since"
	}
	return since, until, ""
}

// handleAdvisorRecords serves
// GET /api/advisor/records?agent=&since=&until=&hours=&limit=.
// Requires read-write or higher — the same floor as the audit listing, since
// advisor text quotes agent transcripts. The admin MCP advisor_records tool
// reads this listing, so the two always return the same records.
func (s *Server) handleAdvisorRecords(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		jsonError(w, "insufficient access", http.StatusForbidden)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	since, until, msg := advisorWindow(r, time.Now().UTC())
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
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
		records := s.advisorRecords.ListWindow(agent, since, until, limit)
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

// advisorSpendResponse is the GET /api/advisor/spend payload: per-agent
// advisor spend over one window, reported beside (never folded into) the
// agent's own spend.
type advisorSpendResponse struct {
	// Range is the preset the window came from, or "custom" for since/until.
	Range string `json:"range"`
	// Since and Until bound the window (RFC3339); empty means open.
	Since        string               `json:"since,omitempty"`
	Until        string               `json:"until,omitempty"`
	TotalCostUSD float64              `json:"total_cost_usd"`
	Agents       []advisor.AgentSpend `json:"agents"`
}

// handleAdvisorSpend serves
// GET /api/advisor/spend?range=hourly|daily|weekly|monthly or
// ?since=&until=&hours=, optionally filtered by agent. The presets are the
// Cost section's timeframes. Same access as GET /api/cost: aggregate figures
// only, no advisor text.
func (s *Server) handleAdvisorSpend(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	since, until, msg := advisorWindow(r, now)
	if msg != "" {
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	resp := advisorSpendResponse{Range: "custom", Agents: []advisor.AgentSpend{}}
	if raw := strings.TrimSpace(r.URL.Query().Get("range")); raw != "" {
		span, ok := advisorSpendRanges[raw]
		if !ok {
			jsonError(w, "range must be one of hourly, daily, weekly, monthly", http.StatusBadRequest)
			return
		}
		if !since.IsZero() || !until.IsZero() {
			jsonError(w, "use either range or since/until/hours, not both", http.StatusBadRequest)
			return
		}
		resp.Range = raw
		since, until = now.Add(-span), now
	} else if since.IsZero() && until.IsZero() {
		resp.Range = "all"
	}
	if !since.IsZero() {
		resp.Since = since.Format(time.RFC3339)
	}
	if !until.IsZero() {
		resp.Until = until.Format(time.RFC3339)
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if s.advisorRecords != nil {
		for _, sp := range s.advisorRecords.SpendByAgent(since, until) {
			if agent != "" && sp.Agent != agent {
				continue
			}
			resp.Agents = append(resp.Agents, sp)
			resp.TotalCostUSD += sp.CostUSD
		}
	}
	jsonResponse(w, resp)
}
