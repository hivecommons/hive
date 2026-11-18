package dashboard

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// The review_feedback.rules.* settings (hivecommons/hive#19138): the
// preferences for capturing rules reviewers state on the hive's PRs into its
// knowledge (src/docs/review-feedback-rules.md), edited on the Settings
// Features tab, Review section.
//
// Both endpoints are OWNER-ONLY: the trusted associations decide whose words
// are pinned into every later kick without a person's approval. A PUT is
// validated before anything changes, applies without a restart (the sweep
// reads hive.yaml on each pass), is audited, and is rolled back when the config
// file cannot be written. The HIVE_REVIEW_FEEDBACK_RULES and
// HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS variables still win when set;
// the response says so, and the card shows it.

// reviewFeedbackSignal describes one built-in signal the card can switch off.
type reviewFeedbackSignal struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// reviewFeedbackSignals is the card's vocabulary, in classifier table order.
var reviewFeedbackSignals = []reviewFeedbackSignal{
	{reviewrules.SignalSourceOfTruth, `"source of truth"`},
	{reviewrules.SignalAuthoritative, `"authoritative"`},
	{reviewrules.SignalAlways, `"should always", "must always"`},
	{reviewrules.SignalNever, `"should never", "must never"`},
	{reviewrules.SignalGoingForward, `"going forward", "from now on", "in future", "next time", "for future assessments"`},
	{reviewrules.SignalCaptureRequest, `"capture this", "document this", "record it"`},
	{reviewrules.SignalCriteria, `"assessment criteria", "assessment knowledge"`},
	{reviewrules.SignalKnowledgeBase, `"knowledge base"`},
}

// reviewFeedbackRulesResponse is the GET/PUT response body.
type reviewFeedbackRulesResponse struct {
	// Enabled, TrustedAssociations, ExtraPhrases and DisabledSignals are the
	// values hive.yaml holds (the editable ones). Enabled is true when unset.
	Enabled             bool     `json:"enabled"`
	TrustedAssociations []string `json:"trusted_associations"`
	ExtraPhrases        []string `json:"extra_phrases"`
	DisabledSignals     []string `json:"disabled_signals"`
	// EffectiveEnabled and EffectiveTrustedAssociations are what the sweep
	// uses after the environment variables are applied.
	EffectiveEnabled             bool     `json:"effective_enabled"`
	EffectiveTrustedAssociations []string `json:"effective_trusted_associations"`
	// EnabledFromEnv and TrustedFromEnv say an environment variable decides the
	// value, so editing the key has no effect until the variable is removed.
	EnabledFromEnv bool `json:"enabled_from_env"`
	TrustedFromEnv bool `json:"trusted_from_env"`
	// Defaults and vocabulary for the editor.
	DefaultTrustedAssociations []string               `json:"default_trusted_associations"`
	Associations               []string               `json:"associations"`
	Signals                    []reviewFeedbackSignal `json:"signals"`
}

func reviewFeedbackRulesResponseFor(cfg *config.Config) reviewFeedbackRulesResponse {
	r := cfg.ReviewFeedbackRules()
	res := reviewrules.Resolve(reviewrules.Settings{
		Enabled:             r.Enabled,
		TrustedAssociations: r.TrustedAssociations,
		ExtraPhrases:        r.ExtraPhrases,
		DisabledSignals:     r.DisabledSignals,
	})
	effTrusted := make([]string, 0, len(res.Policy.Trusted))
	for _, a := range config.ReviewFeedbackAssociations {
		if res.Policy.Trusted[a] {
			effTrusted = append(effTrusted, a)
		}
	}
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return reviewFeedbackRulesResponse{
		Enabled:                      r.Enabled == nil || *r.Enabled,
		TrustedAssociations:          nonNil(r.TrustedAssociations),
		ExtraPhrases:                 nonNil(r.ExtraPhrases),
		DisabledSignals:              nonNil(r.DisabledSignals),
		EffectiveEnabled:             !res.Policy.Off,
		EffectiveTrustedAssociations: effTrusted,
		EnabledFromEnv:               res.EnabledFromEnv,
		TrustedFromEnv:               res.TrustedFromEnv,
		DefaultTrustedAssociations:   strings.Split(reviewrules.DefaultTrustedAssociations, ","),
		Associations:                 config.ReviewFeedbackAssociations,
		Signals:                      reviewFeedbackSignals,
	}
}

// registerReviewFeedbackRulesRoutes mounts the editor's two owner-only routes.
func (s *Server) registerReviewFeedbackRulesRoutes() {
	s.mux.HandleFunc("GET /api/config/review-feedback/rules", s.handleReviewFeedbackRulesGet)
	s.mux.HandleFunc("PUT /api/config/review-feedback/rules", s.handleReviewFeedbackRulesPut)
}

// handleReviewFeedbackRulesGet returns the review_feedback.rules settings.
func (s *Server) handleReviewFeedbackRulesGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, reviewFeedbackRulesResponseFor(s.deps.Config))
}

// handleReviewFeedbackRulesPut updates review_feedback.rules. Every field is
// optional; an absent one is left as it is. Sending an empty list clears it,
// which restores the default (trusted associations) or the built-in behaviour
// (phrases, signals).
func (s *Server) handleReviewFeedbackRulesPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled             *bool     `json:"enabled"`
		TrustedAssociations *[]string `json:"trusted_associations"`
		ExtraPhrases        *[]string `json:"extra_phrases"`
		DisabledSignals     *[]string `json:"disabled_signals"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config

	// --- validate before mutating anything ---
	previous := cfg.ReviewFeedbackRules()
	next := previous
	if body.Enabled != nil {
		if *body.Enabled {
			next.Enabled = nil // on is the default; keep the file free of it
		} else {
			v := false
			next.Enabled = &v
		}
	}
	if body.TrustedAssociations != nil {
		next.TrustedAssociations = *body.TrustedAssociations
	}
	if body.ExtraPhrases != nil {
		next.ExtraPhrases = *body.ExtraPhrases
	}
	if body.DisabledSignals != nil {
		next.DisabledSignals = *body.DisabledSignals
	}

	// --- apply ---
	if err := cfg.SetReviewFeedbackRules(next); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveConfig(); err != nil {
		// Memory must not diverge from the file: put the old values back.
		_ = cfg.SetReviewFeedbackRules(previous)
		if s.logger != nil {
			s.logger.Error("failed to persist config after review_feedback.rules update", "error", err)
		}
		jsonError(w, "failed to save config; review_feedback.rules unchanged", http.StatusInternalServerError)
		return
	}
	cur := cfg.ReviewFeedbackRules()
	s.auditFromRequest(r, "config_review_feedback_rules", auditDetail(
		"section", "review_feedback",
		"enabled", strconv.FormatBool(cur.Enabled == nil || *cur.Enabled),
		"trusted_associations", strings.Join(cur.TrustedAssociations, ","),
		"extra_phrases", strconv.Itoa(len(cur.ExtraPhrases)),
		"disabled_signals", strings.Join(cur.DisabledSignals, ","),
	), "")
	if s.logger != nil {
		s.logger.Info("review_feedback.rules updated via runtime settings",
			"enabled", cur.Enabled == nil || *cur.Enabled, "trusted_associations", cur.TrustedAssociations,
			"extra_phrases", len(cur.ExtraPhrases), "disabled_signals", cur.DisabledSignals)
	}
	jsonResponse(w, reviewFeedbackRulesResponseFor(cfg))
}
