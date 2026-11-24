package dashboard

import (
	"net/http"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// The reviewer-stated rules editor on the Settings Features tab
// (hivecommons/hive#19135). When a person reviewing one of the hive's own PRs
// states a rule or a source of truth, the review-feedback sweep records it in
// the review-rules knowledge channel (#19046). Which phrases count as a rule
// and whose rules are recorded at once are the owner's preferences:
//
//   - review_feedback.rules.enabled              (HIVE_REVIEW_FEEDBACK_RULES)
//   - review_feedback.rules.trusted_associations[] (HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS)
//   - review_feedback.rules.extra_phrases[]        (HIVE_REVIEW_FEEDBACK_RULE_EXTRA_PHRASES)
//   - review_feedback.rules.disabled_signals[]     (HIVE_REVIEW_FEEDBACK_RULE_DISABLED_SIGNALS)
//
// Both endpoints are OWNER-ONLY: the values change what every later agent run
// is told. A PUT is validated before anything changes, applies on the sweep's
// next pass without a restart, is audited, and is rolled back when the config
// file cannot be written. A saved value wins over its env var; clearing a
// field falls back to the env var, then to the default.

// reviewRulesField is one preference: the saved value (nil when unsaved), the
// effective value, where it comes from and the built-in default.
type reviewRulesField[T any] struct {
	Saved     *T     `json:"saved"`
	Effective T      `json:"effective"`
	Source    string `json:"source"` // "settings", "env" or "default"
	Default   T      `json:"default"`
	EnvVar    string `json:"env_var"`
	YAMLKey   string `json:"yaml_key"`
}

// reviewRulesResponse is the GET/PUT response body.
type reviewRulesResponse struct {
	Enabled             reviewRulesField[bool]     `json:"enabled"`
	TrustedAssociations reviewRulesField[[]string] `json:"trusted_associations"`
	ExtraPhrases        reviewRulesField[[]string] `json:"extra_phrases"`
	DisabledSignals     reviewRulesField[[]string] `json:"disabled_signals"`
	// Associations are the values trusted_associations may hold.
	Associations []string `json:"associations"`
	// Signals are the built-in signals, with examples.
	Signals []config.ReviewRuleSignal `json:"signals"`
	// MaxPhrases bounds extra_phrases.
	MaxPhrases int `json:"max_phrases"`
}

func envList(name string) ([]string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil, false
	}
	return config.ParseReviewRulesList(v), true
}

func listField(saved []string, envVar, yamlKey string, def []string) reviewRulesField[[]string] {
	f := reviewRulesField[[]string]{Default: def, EnvVar: envVar, YAMLKey: yamlKey, Effective: def, Source: "default"}
	if f.Default == nil {
		f.Default = []string{}
		f.Effective = []string{}
	}
	if env, ok := envList(envVar); ok {
		f.Effective, f.Source = env, "env"
	}
	if len(saved) > 0 {
		s := append([]string(nil), saved...)
		f.Saved, f.Effective, f.Source = &s, s, "settings"
	}
	return f
}

func reviewRulesResponseFor(cfg *config.Config) reviewRulesResponse {
	set := cfg.ReviewRulesSettingsSnapshot()
	enabled := reviewRulesField[bool]{Default: true, Effective: config.ReviewRulesEnvEnabled(), Source: "default",
		EnvVar: config.ReviewRulesEnabledEnvVar, YAMLKey: "review_feedback.rules.enabled"}
	if strings.TrimSpace(os.Getenv(config.ReviewRulesEnabledEnvVar)) != "" {
		enabled.Source = "env"
	}
	if set.Enabled != nil {
		v := *set.Enabled
		enabled.Saved, enabled.Effective, enabled.Source = &v, v, "settings"
	}
	return reviewRulesResponse{
		Enabled: enabled,
		TrustedAssociations: listField(set.TrustedAssociations, config.ReviewRulesTrustedEnvVar,
			"review_feedback.rules.trusted_associations", config.ParseReviewRulesList(config.ReviewRulesDefaultTrusted)),
		ExtraPhrases:    listField(set.ExtraPhrases, config.ReviewRulesExtraPhrasesEnvVar, "review_feedback.rules.extra_phrases", nil),
		DisabledSignals: listField(set.DisabledSignals, config.ReviewRulesDisabledSignalsEnvVar, "review_feedback.rules.disabled_signals", nil),
		Associations:    append([]string(nil), config.ReviewAssociations...),
		Signals:         config.ReviewRuleSignals(),
		MaxPhrases:      config.ReviewRulesMaxPhrases,
	}
}

// reviewRulesPutBody is the PUT body. A field that is absent or null clears
// the saved value (back to the env var, then the default).
type reviewRulesPutBody struct {
	Enabled             *bool     `json:"enabled"`
	TrustedAssociations *[]string `json:"trusted_associations"`
	ExtraPhrases        *[]string `json:"extra_phrases"`
	DisabledSignals     *[]string `json:"disabled_signals"`
}

// validateReviewRulesPut normalises the body into settings, or returns the
// first problem as a message for the owner.
func validateReviewRulesPut(body reviewRulesPutBody) (config.ReviewRulesSettings, string) {
	var out config.ReviewRulesSettings
	if body.Enabled != nil {
		v := *body.Enabled
		out.Enabled = &v
	}
	if body.TrustedAssociations != nil {
		seen := map[string]bool{}
		for _, a := range *body.TrustedAssociations {
			a = strings.ToUpper(strings.TrimSpace(a))
			if a == "" || seen[a] {
				continue
			}
			if !config.IsReviewAssociation(a) {
				return out, "unknown association " + a + "; use one of " + strings.Join(config.ReviewAssociations, ", ")
			}
			seen[a] = true
			out.TrustedAssociations = append(out.TrustedAssociations, a)
		}
		if len(*body.TrustedAssociations) > 0 && len(out.TrustedAssociations) == 0 {
			return out, "trusted_associations needs at least one association, or leave it empty for the default"
		}
	}
	if body.ExtraPhrases != nil {
		seen := map[string]bool{}
		for _, p := range *body.ExtraPhrases {
			p = strings.Join(strings.Fields(p), " ")
			if p == "" || seen[strings.ToLower(p)] {
				continue
			}
			if why := config.ValidateReviewRulePhrase(p); why != "" {
				return out, "phrase \"" + p + "\" " + why
			}
			seen[strings.ToLower(p)] = true
			out.ExtraPhrases = append(out.ExtraPhrases, p)
		}
		if len(out.ExtraPhrases) > config.ReviewRulesMaxPhrases {
			return out, "at most 20 extra phrases"
		}
	}
	if body.DisabledSignals != nil {
		seen := map[string]bool{}
		for _, n := range *body.DisabledSignals {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" || seen[n] {
				continue
			}
			if !config.KnownReviewRuleSignal(n) {
				return out, "unknown signal " + n
			}
			seen[n] = true
			out.DisabledSignals = append(out.DisabledSignals, n)
		}
	}
	return out, ""
}

// handleReviewRulesGet returns the reviewer-rule capture preferences.
func (s *Server) handleReviewRulesGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, reviewRulesResponseFor(s.deps.Config))
}

// handleReviewRulesPut replaces the saved preferences. Body:
// {"enabled": true, "trusted_associations": ["OWNER"], "extra_phrases": ["..."],
// "disabled_signals": ["never"]}; an absent or null field clears that saved
// value.
func (s *Server) handleReviewRulesPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body reviewRulesPutBody
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	next, problem := validateReviewRulesPut(body)
	if problem != "" {
		jsonError(w, problem, http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	prev := cfg.SetReviewRulesSettings(next)
	if err := s.saveConfig(); err != nil {
		cfg.SetReviewRulesSettings(prev)
		if s.logger != nil {
			s.logger.Error("failed to persist config after review-rules update", "error", err)
		}
		jsonError(w, "failed to save config; reviewer-rule settings unchanged", http.StatusInternalServerError)
		return
	}
	resp := reviewRulesResponseFor(cfg)
	s.auditFromRequest(r, "config_review_feedback_rules", auditDetail(
		"section", "features",
		"enabled", boolString(resp.Enabled.Effective),
		"trusted_associations", strings.Join(resp.TrustedAssociations.Effective, ","),
		"extra_phrases", strings.Join(resp.ExtraPhrases.Effective, " | "),
		"disabled_signals", strings.Join(resp.DisabledSignals.Effective, ","),
	), "")
	if s.logger != nil {
		s.logger.Info("reviewer-rule capture settings updated via runtime settings",
			"enabled", resp.Enabled.Effective, "trusted_associations", resp.TrustedAssociations.Effective,
			"extra_phrases", len(resp.ExtraPhrases.Effective), "disabled_signals", resp.DisabledSignals.Effective)
	}
	jsonResponse(w, resp)
}
