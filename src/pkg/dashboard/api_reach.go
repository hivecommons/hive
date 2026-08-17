package dashboard

import (
	"net/http"
	"time"

	"github.com/kubestellar/hive/pkg/reach"
)

// reachDashboardResponse is the /api/reach payload returned by the dashboard server.
type reachDashboardResponse struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Reports     []reach.PRReachReport `json:"reports"`
}

// handleReach serves GET /api/reach for the dashboard frontend (#3995, phase 2c of #3973).
// It returns recent PR reach telemetry with error rate deltas and attribution data.
func (s *Server) handleReach(w http.ResponseWriter, r *http.Request) {
	resp := reachDashboardResponse{
		GeneratedAt: time.Now().UTC(),
		Reports:     []reach.PRReachReport{},
	}
	if s != nil && s.deps != nil && s.deps.ReachReportsFunc != nil {
		reports := s.deps.ReachReportsFunc()
		if reports != nil {
			resp.Reports = reports
		}
	}
	jsonResponse(w, resp)
}
