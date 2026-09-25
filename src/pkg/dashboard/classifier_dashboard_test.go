package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
)

func TestSmartClassifierSettingsUIElements(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("static", "index.html"))
	if err != nil {
		t.Fatalf("read dashboard SPA: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		"Smart classifier (Jev)",
		"id=\"smart-classifier-section\"",
		"id=\"smart-classifier-openrouter-status\"",
		"id=\"smart-classifier-connect-openrouter\"",
		"id=\"smart-classifier-backend\"",
		"id=\"smart-classifier-mode\"",
		"id=\"smart-classifier-min-confidence\"",
		"id=\"smart-classifier-decision-lane\"",
		"id=\"smart-classifier-decision-tier\"",
		"id=\"smart-classifier-decision-triage\"",
		"id=\"smart-classifier-stats\"",
		"data-action=\"openOpenRouterFund\"",
		"loadSmartClassifierStats()",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard SPA missing %q", want)
		}
	}
	if strings.Contains(html, "id=\"smart-classifier-section\" style=") {
		t.Fatal("smart classifier section must not add inline styles")
	}
}

func TestClassifierStatsReadinessNoKey(t *testing.T) {
	t.Setenv(defaultJevDashboardAPIKeyEnv, "")
	s, _ := apiServer(t)

	rec := doGet(s, "/api/classifier/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["openrouter_connected"] != false {
		t.Fatalf("openrouter_connected = %v, want false", body["openrouter_connected"])
	}
	if body["key_source"] != "none" {
		t.Fatalf("key_source = %v, want none", body["key_source"])
	}
}

func TestClassifierStatsReadinessEnvKey(t *testing.T) {
	t.Setenv(defaultJevDashboardAPIKeyEnv, "jev-test-key")
	s, _ := apiServer(t)

	rec := doGet(s, "/api/classifier/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["key_source"] != "env" {
		t.Fatalf("key_source = %v, want env", body["key_source"])
	}
	if body["jev_api_key_env_present"] != true {
		t.Fatalf("jev_api_key_env_present = %v, want true", body["jev_api_key_env_present"])
	}
}

func TestClassifierStatsReadinessOpenRouterKey(t *testing.T) {
	t.Setenv(defaultJevDashboardAPIKeyEnv, "")
	t.Setenv("OR_CLASSIFIER_TEST_KEY", "sk-or-test")
	s, deps := apiServer(t)
	deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:      "openrouter",
		Kind:      config.GatewayKindOpenRouter,
		Endpoint:  "https://openrouter.ai/api/v1",
		APIKeyEnv: "OR_CLASSIFIER_TEST_KEY",
	}}

	rec := doGet(s, "/api/classifier/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["openrouter_connected"] != true {
		t.Fatalf("openrouter_connected = %v, want true", body["openrouter_connected"])
	}
	if body["key_source"] != "openrouter" {
		t.Fatalf("key_source = %v, want openrouter", body["key_source"])
	}
}

func TestGovernorClassifierSave(t *testing.T) {
	t.Cleanup(classify.ResetForTest)
	t.Setenv(defaultJevDashboardAPIKeyEnv, "jev-test-key")
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/governor/classifier", map[string]any{
		"backend":       "jev",
		"mode":          "enforce",
		"minConfidence": 0.91,
		"decisions":     []string{"lane", "tier"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if deps.Config.Classifier.Backend != "jev" || deps.Config.Classifier.Mode != "enforce" {
		t.Fatalf("classifier backend/mode = %q/%q", deps.Config.Classifier.Backend, deps.Config.Classifier.Mode)
	}
	if deps.Config.Classifier.Jev.APIKeyEnv != defaultJevDashboardAPIKeyEnv {
		t.Fatalf("api_key_env = %q, want %q", deps.Config.Classifier.Jev.APIKeyEnv, defaultJevDashboardAPIKeyEnv)
	}
	if deps.Config.Classifier.Jev.MinConfidence != 0.91 {
		t.Fatalf("min confidence = %v", deps.Config.Classifier.Jev.MinConfidence)
	}
	if got := strings.Join(deps.Config.Classifier.Jev.Decisions, ","); got != "lane,tier" {
		t.Fatalf("decisions = %q", got)
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["backend"] != "jev" {
		t.Fatalf("response backend = %v", body["backend"])
	}
}
