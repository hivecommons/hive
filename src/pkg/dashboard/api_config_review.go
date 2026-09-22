package dashboard

import (
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
)

// handleReviewConfigGet returns the top-level review-swarm gate config
// (Config.Review) so the governor Features tab can prefill the Review Gate
// section. The struct is secret-free, so it is returned as-is.
//
// The built-in perspective list and focus text ride along as read-only
// metadata. The dialog shows an operator what each perspective looks for by
// default so an override is never a one-way door — without it, nothing in the
// UI would remember what the default said once it had been edited away from.
func (s *Server) handleReviewConfigGet(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, reviewSectionResponse(s.deps.Config))
}

type reviewSection struct {
	config.ReviewConfig
	BuiltinPerspectives []map[string]string `json:"builtin_perspectives"`
}

// reviewSectionResponse is the review block as the dashboard sees it: the
// config plus the built-in perspective list and default focus text. Served
// from both the governor bundle and the standalone GET so the Features tab
// sees the same shape whichever it loads from.
func reviewSectionResponse(cfg *config.Config) reviewSection {
	builtin := make([]map[string]string, 0, len(review.DefaultPerspectives))
	for _, p := range review.DefaultPerspectives {
		builtin = append(builtin, map[string]string{"name": string(p), "focus": review.DefaultFocus(p)})
	}
	var rc config.ReviewConfig
	if cfg != nil {
		rc = cfg.Review
	}
	return reviewSection{rc, builtin}
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
		ConfidenceScore    *bool     `json:"confidence_score"`
		// Perspectives and PerspectivePrompts travel together: a hive-defined
		// perspective is only valid once its prompt exists, so validating one
		// without the other would reject a correct pair sent in two requests.
		// The dialog always sends both when it sends either.
		Perspectives         *[]string          `json:"perspectives"`
		PerspectivePrompts   *map[string]string `json:"perspective_prompts"`
		CombinedPerspectives *bool              `json:"combined_perspectives"`
		// ReviseRepos / ReviseVerdictsBefore drive the reviewer's revisit
		// lane (#7706). Both were config-file-only until now, which left the
		// pilot switch reachable only by a hand edit that the periodic saver
		// then overwrote. An empty cutoff clears it; a non-empty one must be
		// RFC 3339 so a typo is refused here rather than silently ignored by
		// parseReviseCutoff at dispatch time.
		ReviseRepos          *[]string `json:"revise_repos"`
		ReviseVerdictsBefore *string   `json:"revise_verdicts_before"`
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
	// Validated against the real resolver rather than a copy of its rules, so
	// what the API accepts is exactly what the hive will load. Rejected here
	// with the resolver's own message, which names the offending perspective.
	if body.Perspectives != nil || body.PerspectivePrompts != nil {
		names := cfg.Review.Perspectives
		if body.Perspectives != nil {
			names = *body.Perspectives
		}
		prompts := cfg.Review.PerspectivePrompts
		if body.PerspectivePrompts != nil {
			prompts = *body.PerspectivePrompts
		}
		if _, err := review.NewPerspectiveSet(names, prompts); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Perspectives != nil {
			cleaned := make([]string, 0, len(names))
			for _, n := range names {
				if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
					cleaned = append(cleaned, n)
				}
			}
			cfg.Review.Perspectives = cleaned
		}
		if body.PerspectivePrompts != nil {
			cleaned := make(map[string]string, len(prompts))
			for k, v := range prompts {
				k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
				if k != "" && v != "" {
					cleaned[k] = v
				}
			}
			if len(cleaned) == 0 {
				cleaned = nil
			}
			cfg.Review.PerspectivePrompts = cleaned
		}
	}
	if body.CombinedPerspectives != nil {
		cfg.Review.CombinedPerspectives = *body.CombinedPerspectives
	}
	if body.ReviseVerdictsBefore != nil {
		cutoff := strings.TrimSpace(*body.ReviseVerdictsBefore)
		if cutoff != "" {
			if _, err := time.Parse(time.RFC3339, cutoff); err != nil {
				jsonError(w, "revise_verdicts_before must be RFC 3339 (e.g. 2026-09-19T14:00:00Z) or empty", http.StatusBadRequest)
				return
			}
		}
		cfg.Review.ReviseVerdictsBefore = cutoff
	}
	if body.ReviseRepos != nil {
		repos := make([]string, 0, len(*body.ReviseRepos))
		for _, repo := range *body.ReviseRepos {
			if repo = strings.TrimSpace(repo); repo != "" {
				repos = append(repos, repo)
			}
		}
		cfg.Review.ReviseRepos = repos
	}
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
	if body.ConfidenceScore != nil {
		cfg.Review.ConfidenceScore = *body.ConfidenceScore
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
	// The GitHub client caches the revise allowlist and the perspective set at
	// boot; without this the file changes but the running relay keeps the old
	// values until the next restart, and the operator sees "updated" for a
	// setting that is not in effect.
	if s.deps.ReviewConfigApplied != nil {
		s.deps.ReviewConfigApplied(cfg.Review)
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}
