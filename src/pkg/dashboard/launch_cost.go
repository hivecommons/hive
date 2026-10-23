package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/tokens"
)

// handleLaunchCost serves GET /api/launch-cost (#12128): token spend placed
// against the issue / PR / head SHA each kick was launched for, from the
// cached LaunchCostCollector snapshot. Optional ?ref=owner/repo#N narrows
// by_ref and by_launch to one work item.
//
// Like handleRepoCost, a not-ready collector reports ready=false with empty
// lists rather than a fabricated $0.00.
func (s *Server) handleLaunchCost(w http.ResponseWriter, r *http.Request) {
	q, ok := parseHistoryQuery(w, r)
	if !ok {
		return
	}
	var resp collect.LaunchCostResponse
	switch {
	case s.deps != nil && s.deps.LaunchCost != nil:
		snap, ready := s.deps.LaunchCost.Snapshot()
		if !ready {
			resp = collect.ComputeLaunchCost(nil, nil, time.Time{})
			resp.CollectedAt = time.Time{}
			launchCostResponse(w, r, resp, q)
			return
		}
		resp = snap
	default:
		// No collector wired (minimal test Server): compute inline so the
		// endpoint degrades to a correct answer rather than 503ing.
		var summary *tokens.AggregateSummary
		var launches []tokens.LaunchRecord
		if s.deps != nil && s.deps.Tokens != nil {
			summary = s.deps.Tokens.Summary()
			launches = s.deps.Tokens.Launches()
		}
		resp = collect.ComputeLaunchCost(summary, launches, time.Now())
	}
	if ref := strings.TrimSpace(r.URL.Query().Get("ref")); ref != "" {
		resp = filterLaunchCostByRef(resp, ref)
	}
	launchCostResponse(w, r, resp, q)
}

// filterLaunchCostByRef keeps only the ref entry and launches naming ref.
// Totals and the mandatory buckets are left intact: they describe the whole
// window, and narrowing them would break the Σ == total invariant.
func filterLaunchCostByRef(resp collect.LaunchCostResponse, ref string) collect.LaunchCostResponse {
	byRef := make([]collect.RefCostEntry, 0, 1)
	for _, e := range resp.ByRef {
		if strings.EqualFold(e.Ref, ref) {
			byRef = append(byRef, e)
		}
	}
	byLaunch := make([]collect.LaunchCostEntry, 0, len(resp.ByLaunch))
	for _, l := range resp.ByLaunch {
		for _, r := range l.Refs {
			if strings.EqualFold(r.Ref, ref) {
				byLaunch = append(byLaunch, l)
				break
			}
		}
	}
	resp.ByRef = byRef
	resp.ByLaunch = byLaunch
	return resp
}
