package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
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
		MaxPerspectives    *int      `json:"max_perspectives_per_pr"`
		ReviewerAgents     *[]string `json:"reviewer_agents"`
		FixerAgent         *string   `json:"fixer_agent"`
		AllAuthors         *bool     `json:"all_authors"`
		AcknowledgeNoFind  *bool     `json:"acknowledge_no_findings"`
		HumanDecisionLabel *string   `json:"human_decision_label"`
		// Recommendations arrives as a whole object rather than one pointer
		// per knob: its fields are only meaningful together (enabling it
		// without a repo list means "every watched repo"), and the Features
		// dialog always sends the full sub-object it rendered. Absent key
		// still leaves the block untouched, matching the contract above.
		Recommendations *config.RecommendationsConfig `json:"recommendations"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if body.MaxParallelReviews != nil && *body.MaxParallelReviews < 0 {
		jsonError(w, "max_parallel_reviews must be >= 0", http.StatusBadRequest)
		return
	}

	if body.MaxPerspectives != nil && *body.MaxPerspectives < 0 {
		jsonError(w, "max_perspectives_per_pr must be >= 0", http.StatusBadRequest)
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
	if body.MaxPerspectives != nil {
		cfg.Review.MaxPerspectivesPerPR = *body.MaxPerspectives
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
	if body.AllAuthors != nil {
		cfg.Review.AllAuthors = *body.AllAuthors
	}
	if body.AcknowledgeNoFind != nil {
		cfg.Review.AcknowledgeNoFindings = *body.AcknowledgeNoFind
	}
	// Deliberately not validated against the repos' label sets: the hive
	// governs many repos and a label present in one may be absent in another,
	// so rejecting the name here would block a setting that is correct
	// elsewhere. An unusable name degrades to "no label applied" at review
	// time, where the review marker still carries the signal.
	if body.HumanDecisionLabel != nil {
		cfg.Review.HumanDecisionLabel = strings.TrimSpace(*body.HumanDecisionLabel)
	}
	// Repo names are trimmed but NOT validated against the watched set: a
	// hive's repo list changes underneath a dialog that was opened minutes
	// ago, and rejecting the write would lose the operator's other edits.
	// An unwatched name simply never matches at render time.
	if body.Recommendations != nil {
		rec := *body.Recommendations
		repos := make([]string, 0, len(rec.Repos))
		for _, repo := range rec.Repos {
			if repo = strings.TrimSpace(repo); repo != "" {
				repos = append(repos, repo)
			}
		}
		rec.Repos = repos
		if rec.MinReadyToOpen < 0 {
			rec.MinReadyToOpen = 0
		}
		// Labels has no control in the Features dialog, so the browser always
		// omits it. Replacing the struct wholesale would silently erase a
		// label list configured in hive.yaml the first time an operator
		// toggled this on; carry the existing value across instead.
		if rec.Labels == nil {
			rec.Labels = cfg.Review.Recommendations.Labels
		}
		cfg.Review.Recommendations = rec
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after review update", "error", err)
	}
	s.auditFromRequest(r, "config_review", auditDetail("section", "review"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}
