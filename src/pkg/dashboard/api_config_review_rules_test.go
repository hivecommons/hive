package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19135: the owner-only review_feedback.rules.* editor.

const reviewRulesPath = "/api/config/review-feedback/rules"

func decodeReviewRules(t *testing.T, body []byte) reviewRulesResponse {
	t.Helper()
	var got reviewRulesResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func clearReviewRulesEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{config.ReviewRulesEnabledEnvVar, config.ReviewRulesTrustedEnvVar, config.ReviewRulesExtraPhrasesEnvVar, config.ReviewRulesDisabledSignalsEnvVar} {
		t.Setenv(v, "")
	}
}

func TestReviewRules_OwnerOnly(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, reviewRulesPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, reviewRulesPath, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.ReviewFeedback != nil {
		t.Fatal("a refused PUT changed the settings")
	}
}

func TestReviewRules_GetShowsDefaultsAndEnv(t *testing.T) {
	clearReviewRulesEnv(t)
	s := covApiServer(t)
	rec := doOwnerGet(s, reviewRulesPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeReviewRules(t, rec.Body.Bytes())
	if !got.Enabled.Effective || got.Enabled.Source != "default" || got.Enabled.Saved != nil {
		t.Fatalf("enabled = %+v", got.Enabled)
	}
	if strings.Join(got.TrustedAssociations.Effective, ",") != config.ReviewRulesDefaultTrusted || got.TrustedAssociations.Source != "default" {
		t.Fatalf("trusted = %+v", got.TrustedAssociations)
	}
	if got.ExtraPhrases.Effective == nil || len(got.ExtraPhrases.Effective) != 0 || got.ExtraPhrases.YAMLKey != "review_feedback.rules.extra_phrases" {
		t.Fatalf("phrases = %+v", got.ExtraPhrases)
	}
	if len(got.Signals) != len(config.ReviewRuleSignals()) || len(got.Associations) == 0 || got.MaxPhrases != config.ReviewRulesMaxPhrases {
		t.Fatalf("vocabulary = %+v", got)
	}

	t.Setenv(config.ReviewRulesTrustedEnvVar, "OWNER")
	t.Setenv(config.ReviewRulesEnabledEnvVar, "off")
	got = decodeReviewRules(t, doOwnerGet(s, reviewRulesPath).Body.Bytes())
	if got.TrustedAssociations.Source != "env" || strings.Join(got.TrustedAssociations.Effective, ",") != "OWNER" || got.Enabled.Effective || got.Enabled.Source != "env" {
		t.Fatalf("env: %+v %+v", got.TrustedAssociations, got.Enabled)
	}
}

func TestReviewRules_PutAppliesAndAudits(t *testing.T) {
	clearReviewRulesEnv(t)
	s := covApiServer(t)
	cfg := s.deps.Config
	rec := doPut(s, reviewRulesPath, map[string]any{
		"enabled":              false,
		"trusted_associations": []string{"owner", "MEMBER", "member", "CONTRIBUTOR"},
		"extra_phrases":        []string{"  worth   considering ", "Worth considering", "house rule"},
		"disabled_signals":     []string{"Never"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	set := cfg.ReviewRulesSettingsSnapshot()
	if set.Enabled == nil || *set.Enabled || strings.Join(set.TrustedAssociations, ",") != "OWNER,MEMBER,CONTRIBUTOR" ||
		strings.Join(set.ExtraPhrases, "|") != "worth considering|house rule" || strings.Join(set.DisabledSignals, ",") != "never" {
		t.Fatalf("saved = %+v", set)
	}
	got := decodeReviewRules(t, rec.Body.Bytes())
	if got.Enabled.Source != "settings" || got.Enabled.Effective || got.ExtraPhrases.Source != "settings" {
		t.Fatalf("response = %+v", got)
	}
	if s.audit != nil {
		found := false
		for _, entry := range s.audit.Recent(50) {
			if entry.Action == "config_review_feedback_rules" {
				found = true
			}
		}
		if !found {
			t.Error("missing config_review_feedback_rules audit")
		}
	}

	// An empty body clears every saved value: back to the defaults.
	if rec := doPut(s, reviewRulesPath, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("PUT clear: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.ReviewFeedback != nil {
		t.Fatalf("clear left %+v", cfg.ReviewFeedback)
	}
}

func TestReviewRules_PutValidation(t *testing.T) {
	clearReviewRulesEnv(t)
	s := covApiServer(t)
	many := make([]string, config.ReviewRulesMaxPhrases+1)
	for i := range many {
		many[i] = "phrase number " + strings.Repeat("x", i+1)
	}
	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown association", map[string]any{"trusted_associations": []string{"ADMIN"}}},
		{"only blanks", map[string]any{"trusted_associations": []string{" "}}},
		{"short phrase", map[string]any{"extra_phrases": []string{"ab"}}},
		{"long phrase", map[string]any{"extra_phrases": []string{strings.Repeat("a", config.ReviewRulesMaxPhraseRunes+1)}}},
		{"too many phrases", map[string]any{"extra_phrases": many}},
		{"unknown signal", map[string]any{"disabled_signals": []string{"sometimes"}}},
	}
	for _, tc := range cases {
		if rec := doPut(s, reviewRulesPath, tc.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if rec := doPutNoRole(s, reviewRulesPath, `{`); rec.Code != http.StatusForbidden {
		t.Errorf("malformed non-owner: %d", rec.Code)
	}
	if s.deps.Config.ReviewFeedback != nil {
		t.Fatalf("a refused PUT stored %+v", s.deps.Config.ReviewFeedback)
	}
}
