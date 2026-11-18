package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/reviewrules"
)

// hivecommons/hive#19138: the owner-only review_feedback.rules.* editor.

const reviewFeedbackRulesPath = "/api/config/review-feedback/rules"

func decodeReviewFeedbackRules(t *testing.T, body []byte) reviewFeedbackRulesResponse {
	t.Helper()
	var got reviewFeedbackRulesResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func clearRuleEnv(t *testing.T) {
	t.Helper()
	t.Setenv(reviewrules.DisableEnvVar, "")
	t.Setenv(reviewrules.TrustedAssociationsEnvVar, "")
}

func TestReviewFeedbackRules_OwnerOnly(t *testing.T) {
	clearRuleEnv(t)
	s := covApiServer(t)
	if rec := doGetNoRole(s, reviewFeedbackRulesPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: want 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, reviewFeedbackRulesPath, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: want 403, got %d", rec.Code)
	}
	if r := s.deps.Config.ReviewFeedbackRules(); r.Enabled != nil {
		t.Fatalf("a refused PUT changed the config: %+v", r)
	}
}

func TestReviewFeedbackRules_GetDefaults(t *testing.T) {
	clearRuleEnv(t)
	s := covApiServer(t)
	rec := doOwnerGet(s, reviewFeedbackRulesPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeReviewFeedbackRules(t, rec.Body.Bytes())
	if !got.Enabled || !got.EffectiveEnabled || got.EnabledFromEnv || got.TrustedFromEnv {
		t.Fatalf("defaults = %+v", got)
	}
	if strings.Join(got.EffectiveTrustedAssociations, ",") != "OWNER,MEMBER,COLLABORATOR" {
		t.Fatalf("effective trusted = %v", got.EffectiveTrustedAssociations)
	}
	if len(got.Signals) != len(reviewrules.SignalNames()) || got.ExtraPhrases == nil || got.DisabledSignals == nil || got.TrustedAssociations == nil {
		t.Fatalf("vocabulary or empty lists missing: %+v", got)
	}
	for i, n := range reviewrules.SignalNames() {
		if got.Signals[i].Name != n {
			t.Errorf("signal %d = %q, want %q", i, got.Signals[i].Name, n)
		}
	}
}

func TestReviewFeedbackRules_PutAppliesLiveAndAudits(t *testing.T) {
	clearRuleEnv(t)
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	rec := doPut(s, reviewFeedbackRulesPath, map[string]any{
		"enabled":              false,
		"trusted_associations": []string{"owner"},
		"extra_phrases":        []string{"house rule"},
		"disabled_signals":     []string{"knowledge-base"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	live := cfg.ReviewFeedbackRules()
	if live.Enabled == nil || *live.Enabled || live.TrustedAssociations[0] != "OWNER" || live.ExtraPhrases[0] != "house rule" || live.DisabledSignals[0] != "knowledge-base" {
		t.Fatalf("live config = %+v", live)
	}
	got := decodeReviewFeedbackRules(t, rec.Body.Bytes())
	if got.EffectiveEnabled || strings.Join(got.EffectiveTrustedAssociations, ",") != "OWNER" {
		t.Fatalf("response = %+v", got)
	}
	found := false
	if s.audit != nil {
		for _, e := range s.audit.Recent(50) {
			found = found || e.Action == "config_review_feedback_rules"
		}
		if !found {
			t.Error("missing config_review_feedback_rules audit")
		}
	}

	// Absent fields are left alone; turning capture back on clears the key.
	rec = doPut(s, reviewFeedbackRulesPath, map[string]any{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT enabled: %d %s", rec.Code, rec.Body.String())
	}
	live = cfg.ReviewFeedbackRules()
	if live.Enabled != nil || live.ExtraPhrases[0] != "house rule" {
		t.Fatalf("partial PUT changed other fields or left enabled set: %+v", live)
	}
	// An empty list restores the built-in behaviour.
	if rec = doPut(s, reviewFeedbackRulesPath, map[string]any{"trusted_associations": []string{}, "extra_phrases": []string{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT clear: %d %s", rec.Code, rec.Body.String())
	}
	if live = cfg.ReviewFeedbackRules(); len(live.TrustedAssociations) != 0 || len(live.ExtraPhrases) != 0 {
		t.Fatalf("clear left %+v", live)
	}
}

func TestReviewFeedbackRules_PutValidation(t *testing.T) {
	clearRuleEnv(t)
	s := covApiServer(t)
	for name, body := range map[string]map[string]any{
		"unknown association": {"trusted_associations": []string{"STRANGER"}},
		"unknown signal":      {"disabled_signals": []string{"vibes"}},
		"short phrase":        {"extra_phrases": []string{"abc"}},
		"valid next to bad":   {"extra_phrases": []string{"house rule"}, "disabled_signals": []string{"vibes"}},
	} {
		if rec := doPut(s, reviewFeedbackRulesPath, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if r := s.deps.Config.ReviewFeedbackRules(); len(r.ExtraPhrases) != 0 || len(r.DisabledSignals) != 0 {
		t.Fatalf("a refused PUT stored %+v", r)
	}
}

func TestReviewFeedbackRules_EnvStillWinsAndIsReported(t *testing.T) {
	t.Setenv(reviewrules.DisableEnvVar, "off")
	t.Setenv(reviewrules.TrustedAssociationsEnvVar, "MEMBER")
	s := covApiServer(t)
	got := decodeReviewFeedbackRules(t, doOwnerGet(s, reviewFeedbackRulesPath).Body.Bytes())
	if got.EffectiveEnabled || !got.EnabledFromEnv || !got.TrustedFromEnv || strings.Join(got.EffectiveTrustedAssociations, ",") != "MEMBER" {
		t.Fatalf("env not reported: %+v", got)
	}
	// The yaml value is still editable and reported as stored.
	rec := doPut(s, reviewFeedbackRulesPath, map[string]any{"trusted_associations": []string{"OWNER"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	got = decodeReviewFeedbackRules(t, rec.Body.Bytes())
	if strings.Join(got.TrustedAssociations, ",") != "OWNER" || strings.Join(got.EffectiveTrustedAssociations, ",") != "MEMBER" {
		t.Fatalf("stored vs effective = %+v", got)
	}
}
