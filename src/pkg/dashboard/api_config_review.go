package dashboard

import (
	"net/http"
	"strings"
)

// handleReviewConfigGet returns the top-level review-swarm gate config
// (Config.Review) so the governor Features tab can prefill the Review Gate
// section. The struct is secret-free, so it is returned as-is.
func (s *Server) handleReviewConfigGet(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.deps.Config.Review)
}

// handleReviewConfigPut updates the review-swarm merge-gate settings from the
// governor Features dialog. Every field is a pointer so an absent key leaves
// the corresponding config untouched — the same "only what you send is
// changed" contract handleGovernorFeatures uses. saveConfig() persists a
// secret-free overlay that the entrypoint merges on restart.
//
// OWNER-ONLY: flipping require_approval on/off changes merge eligibility for
// every PR, so this follows the same requireOwnerRole gate as the other
// governor-config writers (audit F16/F22).
func (s *Server) handleReviewConfigPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		RequireApproval    *bool     `json:"require_approval"`
		FanOut             *bool     `json:"fan_out"`
		MaxParallelReviews *int      `json:"max_parallel_reviews"`
		ReviewerAgents     *[]string `json:"reviewer_agents"`
		FixerAgent         *string   `json:"fixer_agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if body.MaxParallelReviews != nil && *body.MaxParallelReviews < 0 {
		jsonError(w, "max_parallel_reviews must be >= 0", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	if body.RequireApproval != nil {
		cfg.Review.RequireApproval = *body.RequireApproval
	}
	if body.FanOut != nil {
		cfg.Review.FanOut = *body.FanOut
	}
	if body.MaxParallelReviews != nil {
		cfg.Review.MaxParallelReviews = *body.MaxParallelReviews
	}
	if body.ReviewerAgents != nil {
		agents := make([]string, 0, len(*body.ReviewerAgents))
		for _, a := range *body.ReviewerAgents {
			if a = strings.TrimSpace(a); a != "" {
				agents = append(agents, a)
			}
		}
		cfg.Review.ReviewerAgents = agents
	}
	if body.FixerAgent != nil {
		cfg.Review.FixerAgent = strings.TrimSpace(*body.FixerAgent)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after review update", "error", err)
	}
	s.auditFromRequest(r, "config_review", auditDetail("section", "review"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}
