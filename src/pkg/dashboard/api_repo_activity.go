package dashboard

// The /api/repo-activity handler. The ActivityCollector it serves from moved
// to pkg/dashboard/collect (kubestellar/hive#5565 slice 2); the HTTP surface
// stays here with the rest of the dashboard's handlers.

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
)

type repoActivityResponse struct {
	Ready       bool                     `json:"ready"`
	Phase       string                   `json:"phase"`
	Snapshot    collect.ActivitySnapshot `json:"snapshot"`
	Limitations []string                 `json:"limitations"`
}

func (s *Server) handleRepoActivity(w http.ResponseWriter, r *http.Request) {
	var snap collect.ActivitySnapshot
	ready := false
	if s.deps != nil && s.deps.Activity != nil {
		snap, ready = s.deps.Activity.Snapshot()
	}
	jsonResponse(w, repoActivityResponse{
		Ready:    ready,
		Phase:    "phase_1_activity_only",
		Snapshot: snap,
		Limitations: []string{
			"Counts are recorded audit facts only; no token cost is attributed in this phase.",
			"Entries without repo= are reported as unattributed and are never spread across repos.",
			"window_hours is the freshness window used by the hub health verdict; per-repo counts are accumulated over count_window_hours. Divide by count_window_hours for a rate — window_hours would overstate it by 28x.",
			"Counts are bounded by audit-log retention: rotated and compressed backups are read, but only MaxBackups of them are kept, so a busy hive's effective lookback can be shorter than count_window_hours.",
		},
	})
}
