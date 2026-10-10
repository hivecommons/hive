package main

import (
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

func TestWebhookAwareEvalIntervalComposesWithBudgetStretch(t *testing.T) {
	normal := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetNormal)
	conserve := apiBudgetEffectiveEvalInterval(600, 1800, 2, github.APIBudgetConserve)
	critical := apiBudgetEffectiveEvalInterval(300, 1800, 2, github.APIBudgetCritical)
	for _, tt := range []struct {
		name    string
		budget  time.Duration
		webhook int
		max     int
		healthy bool
		want    time.Duration
	}{
		{"stale webhooks keep the budget interval", normal, 900, 1800, false, 300 * time.Second},
		{"healthy webhooks raise the base interval", normal, 900, 1800, true, 900 * time.Second},
		{"budget stretch above the webhook interval wins", conserve, 900, 1800, true, 1200 * time.Second},
		{"webhook interval above the budget stretch wins", critical, 1500, 1800, true, 1500 * time.Second},
		{"eval_interval_max_s caps the webhook interval", normal, 900, 600, true, 600 * time.Second},
		{"webhook interval below the base is a no-op", normal, 120, 1800, true, 300 * time.Second},
		{"zero webhook interval disables the raise", normal, 0, 1800, true, 300 * time.Second},
	} {
		if got := webhookAwareEvalInterval(tt.budget, tt.webhook, tt.max, tt.healthy); got != tt.want {
			t.Errorf("%s: interval = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestEvalIntervalForConfigWithoutWebhooksMatchesBudget(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.EvalIntervalS = 300
	cfg.Governor.EvalIntervalMaxS = 1800
	cfg.Governor.EvalIntervalWebhookS = 900
	if got, want := evalIntervalForConfig(cfg, nil, nil), apiBudgetIntervalForConfig(cfg, nil); got != want {
		t.Fatalf("no webhook events: interval = %v, want budget interval %v", got, want)
	}
	if got := evalIntervalForConfig(nil, nil, nil); got != 300*time.Second {
		t.Fatalf("nil config interval = %v, want 300s", got)
	}
}
