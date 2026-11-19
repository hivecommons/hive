package dashboard

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
)

// The stranded-feedback retry editor on the Settings Features tab
// (hivecommons/hive#19320). When the review-feedback sweep routes a PR's
// comments to an agent and the run ends with no push and no reply on the PR,
// the sweep routes them again after a backoff that doubles each time, up to a
// cap, then asks a person:
//
//   - review_feedback.retry.max_retries (HIVE_REVIEW_FEEDBACK_RETRY_MAX; default 2)
//   - review_feedback.retry.backoff     (HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF; default 1h)
//
// Both endpoints are OWNER-ONLY. A PUT is validated before anything changes,
// applies on the sweep's next pass, is audited, and is rolled back when the
// config file cannot be written. An absent or null field clears the saved
// value (back to the env var, then the default).

// reviewRetryResponse is the GET/PUT response body.
type reviewRetryResponse struct {
	MaxRetries reviewRulesField[int]    `json:"max_retries"`
	Backoff    reviewRulesField[string] `json:"backoff"`
	// MaxMaxRetries bounds max_retries; MinBackoff bounds backoff.
	MaxMaxRetries int    `json:"max_max_retries"`
	MinBackoff    string `json:"min_backoff"`
}

func reviewRetryResponseFor(cfg *config.Config) reviewRetryResponse {
	set := cfg.ReviewRetrySettingsSnapshot()
	env := reviewfeedback.RetryPolicyFromEnv()
	maxR := reviewRulesField[int]{Default: reviewfeedback.DefaultRetryMax, Effective: env.MaxRetries, Source: "default",
		EnvVar: reviewfeedback.RetryMaxEnvVar, YAMLKey: "review_feedback.retry.max_retries"}
	if strings.TrimSpace(os.Getenv(reviewfeedback.RetryMaxEnvVar)) != "" {
		maxR.Source = "env"
	}
	if set.MaxRetries != nil {
		v := *set.MaxRetries
		maxR.Saved, maxR.Effective, maxR.Source = &v, v, "settings"
	}
	back := reviewRulesField[string]{Default: reviewfeedback.DefaultRetryBackoff.String(), Effective: env.Backoff.String(), Source: "default",
		EnvVar: reviewfeedback.RetryBackoffEnvVar, YAMLKey: "review_feedback.retry.backoff"}
	if strings.TrimSpace(os.Getenv(reviewfeedback.RetryBackoffEnvVar)) != "" {
		back.Source = "env"
	}
	if set.Backoff != "" {
		v := set.Backoff
		back.Saved, back.Effective, back.Source = &v, v, "settings"
	}
	return reviewRetryResponse{MaxRetries: maxR, Backoff: back,
		MaxMaxRetries: reviewfeedback.MaxRetryMax, MinBackoff: reviewfeedback.MinRetryBackoff.String()}
}

// reviewRetryPutBody is the PUT body.
type reviewRetryPutBody struct {
	MaxRetries *int    `json:"max_retries"`
	Backoff    *string `json:"backoff"`
}

// validateReviewRetryPut normalises the body into settings, or returns the
// first problem as a message for the owner.
func validateReviewRetryPut(body reviewRetryPutBody) (config.ReviewRetrySettings, string) {
	var out config.ReviewRetrySettings
	if body.MaxRetries != nil {
		n := *body.MaxRetries
		if n < 0 || n > reviewfeedback.MaxRetryMax {
			return out, "max_retries must be between 0 and " + strconv.Itoa(reviewfeedback.MaxRetryMax)
		}
		out.MaxRetries = &n
	}
	if body.Backoff != nil {
		if b := strings.TrimSpace(*body.Backoff); b != "" {
			d, err := time.ParseDuration(b)
			if err != nil {
				return out, "backoff must be a duration such as 45m or 2h"
			}
			if d < reviewfeedback.MinRetryBackoff {
				return out, "backoff must be at least " + reviewfeedback.MinRetryBackoff.String()
			}
			out.Backoff = d.String()
		}
	}
	return out, ""
}

// handleReviewRetryGet returns the stranded-feedback retry preferences.
func (s *Server) handleReviewRetryGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, reviewRetryResponseFor(s.deps.Config))
}

// handleReviewRetryPut replaces the saved preferences. Body:
// {"max_retries": 2, "backoff": "1h"}; an absent or null field clears that
// saved value.
func (s *Server) handleReviewRetryPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body reviewRetryPutBody
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	next, problem := validateReviewRetryPut(body)
	if problem != "" {
		jsonError(w, problem, http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	prev := cfg.SetReviewRetrySettings(next)
	if err := s.saveConfig(); err != nil {
		cfg.SetReviewRetrySettings(prev)
		if s.logger != nil {
			s.logger.Error("failed to persist config after review-feedback retry update", "error", err)
		}
		jsonError(w, "failed to save config; stranded-feedback retry settings unchanged", http.StatusInternalServerError)
		return
	}
	resp := reviewRetryResponseFor(cfg)
	s.auditFromRequest(r, "config_review_feedback_retry", auditDetail(
		"section", "features",
		"max_retries", strconv.Itoa(resp.MaxRetries.Effective),
		"backoff", resp.Backoff.Effective,
	), "")
	if s.logger != nil {
		s.logger.Info("stranded review-feedback retry settings updated via runtime settings",
			"max_retries", resp.MaxRetries.Effective, "backoff", resp.Backoff.Effective)
	}
	jsonResponse(w, resp)
}
