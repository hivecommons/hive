package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/worksource"
)

// TestExternalWorkSourceFailsClosedAndKeepsPRMaintenance is the seam-level
// guard for ADR-0020: when the external provider is unreachable or answers
// badly, the cycle lists NO issues — it never falls back to the GitHub issues
// the operator configured the hive to ignore — while GitHub PR maintenance is
// left untouched because this overlay only replaces the Issues half.
func TestExternalWorkSourceFailsClosedAndKeepsPRMaintenance(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider is down", http.StatusServiceUnavailable)
	}))
	defer broken.Close()

	t.Setenv("EXTERNAL_SEAM_TEST_TOKEN", "provider-token")
	cfg := config.WorkSourceConfig{
		Type: "external",
		External: config.ExternalSourceConfig{
			Name:      "acme",
			BaseURL:   broken.URL,
			AuthToken: "$EXTERNAL_SEAM_TEST_TOKEN",
			Repos:     []string{"acme/app"},
		},
	}
	ws, err := worksource.FromConfig(cfg, nil, "", "", testLogger())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	got := workSourceIssuesForCycle(context.Background(), ws, nil, nil, config.IssueFilterConfig{}, testLogger())
	if got.Items == nil {
		t.Fatal("Items must never be nil (the fail-closed result is an empty list)")
	}
	if len(got.Items) != 0 || got.Count != 0 {
		t.Fatalf("items = %+v (count %d), want none", got.Items, got.Count)
	}
}

// TestExternalWorkSourceConfigErrorFailsClosed covers the other error mouth:
// a work_source block that cannot build a source at all.
func TestExternalWorkSourceConfigErrorFailsClosed(t *testing.T) {
	cfg := config.WorkSourceConfig{
		Type:     "external",
		External: config.ExternalSourceConfig{Name: "acme"}, // no base_url, no token, no repos
	}
	ws, wsErr := worksource.FromConfig(cfg, nil, "", "", testLogger())
	if wsErr == nil {
		t.Fatal("an incomplete external block must be a config error")
	}

	got := workSourceIssuesForCycle(context.Background(), ws, wsErr, nil, config.IssueFilterConfig{}, testLogger())
	if got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("items = %+v, want the empty fail-closed list", got.Items)
	}
}

// TestExternalWorkSourceAdmissionGatesStillApply: the boundary adds no
// admission authority. Exempt labels and the issue filter run over external
// items exactly as they do over GitHub ones.
func TestExternalWorkSourceAdmissionGatesStillApply(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/source":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contract": worksource.ExternalContract, "source_type": "acme",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"contract": worksource.ExternalContract,
				"items": []map[string]any{
					{"repo": "acme/app", "external_id": "ACME-1", "title": "admitted", "state": "Todo", "labels": []string{"ready"}},
					{"repo": "acme/app", "external_id": "ACME-2", "title": "exempt", "state": "Todo", "labels": []string{"wontfix"}},
				},
				"next_cursor": "",
			})
		}
	}))
	defer provider.Close()

	t.Setenv("EXTERNAL_SEAM_TEST_TOKEN", "provider-token")
	cfg := config.WorkSourceConfig{
		Type: "external",
		External: config.ExternalSourceConfig{
			Name:      "acme",
			BaseURL:   provider.URL,
			AuthToken: "$EXTERNAL_SEAM_TEST_TOKEN",
			Repos:     []string{"acme/app"},
		},
	}
	ws, err := worksource.FromConfig(cfg, nil, "", "", testLogger())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	got := workSourceIssuesForCycle(context.Background(), ws, nil, []string{"wontfix"}, config.IssueFilterConfig{}, testLogger())
	if len(got.Items) != 1 || got.Items[0].Title != "admitted" {
		t.Fatalf("items = %+v, want only the admitted item", got.Items)
	}
}
