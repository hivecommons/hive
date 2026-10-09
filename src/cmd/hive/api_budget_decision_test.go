package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestDecideEvalBudgetWorkSkipsOptionalByModeAndCadence(t *testing.T) {
	if d := decideEvalBudgetWork(github.APIBudgetNormal, 3, 2); !d.SkipOptional || len(d.SkippedSteps) == 0 {
		t.Fatalf("normal off-cadence should skip optional: %+v", d)
	}
	if d := decideEvalBudgetWork(github.APIBudgetNormal, 3, 3); d.SkipOptional {
		t.Fatalf("normal on-cadence should run optional: %+v", d)
	}
	if d := decideEvalBudgetWork(github.APIBudgetConserve, 1, 1); !d.SkipOptional {
		t.Fatalf("conserve should skip optional: %+v", d)
	}
}

func TestAPIBudgetEffectiveEvalInterval(t *testing.T) {
	if got := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetNormal); got != 300*time.Second {
		t.Fatalf("normal interval = %v", got)
	}
	if got := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetConserve); got != 600*time.Second {
		t.Fatalf("conserve interval = %v", got)
	}
	if got := apiBudgetEffectiveEvalInterval(600, 1800, 2, github.APIBudgetCritical); got != 1800*time.Second {
		t.Fatalf("critical interval cap = %v", got)
	}
}

func TestAPIBudgetIntervalComposesWebhookHealthAndBudget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{"core": map[string]any{"limit": 5000, "remaining": 100, "reset": time.Now().Add(time.Hour).Unix()}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := github.NewClientForTest(srv.URL, "org", []string{"repo"}, slog.Default())
	c.SetAPIBudgetThresholds(800, 250)
	if _, err := c.RateLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = c.HandleWebhookInvalidation(context.Background(), "unknown", []byte(`{"repository":{"full_name":"org/repo"}}`))
	cfg := &config.Config{}
	cfg.Governor.EvalIntervalS = 300
	cfg.Governor.EvalIntervalWebhookS = 900
	cfg.Governor.EvalIntervalMaxS = 1800
	cfg.Governor.ConserveIntervalMultiplier = 2
	if got := apiBudgetIntervalForConfig(cfg, c); got != 1200*time.Second {
		t.Fatalf("interval = %v, want max(critical 1200s, webhook 900s)", got)
	}
	cfg.Governor.EvalIntervalWebhookS = 1500
	if got := apiBudgetIntervalForConfig(cfg, c); got != 1500*time.Second {
		t.Fatalf("interval = %v, want webhook max", got)
	}
}
