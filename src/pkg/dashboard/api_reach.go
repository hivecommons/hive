package dashboard

import (
	"net/http"
	"time"

	"github.com/kubestellar/hive/pkg/reach"
)

// handleReach serves PR reach telemetry on the dashboard (#3973, phase 2c).
func (s *Server) handleReach(w http.ResponseWriter, r *http.Request) {
	resp := reachResponse{
		GeneratedAt:     time.Now().UTC(),
		HivesReporting:  0,
		DeployedCommits: []string{},
		Reports:         []reach.PRReachReport{},
	}
	jsonResponse(w, resp)
}

type reachResponse struct {
	GeneratedAt     time.Time             `json:"generated_at"`
	HivesReporting  int                   `json:"hives_reporting"`
	DeployedCommits []string              `json:"deployed_commits"`
	Reports         []reach.PRReachReport `json:"reports"`
}
