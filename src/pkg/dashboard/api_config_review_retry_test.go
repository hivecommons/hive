package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19320: the owner-only review_feedback.retry.* editor.

const reviewRetryPath = "/api/config/review-feedback/retry"

func decodeReviewRetry(t *testing.T, body []byte) reviewRetryResponse {
	t.Helper()
	var got reviewRetryResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func clearReviewRetryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.ReviewRetryMaxEnvVar, "")
	t.Setenv(config.ReviewRetryBackoffEnvVar, "")
}

func TestReviewRetry_OwnerOnly(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, reviewRetryPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, reviewRetryPath, `{"max_retries":5}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.ReviewFeedback != nil {
		t.Fatal("a refused PUT changed the settings")
	}
}

func TestReviewRetry_GetShowsDefaultsAndEnv(t *testing.T) {
	clearReviewRetryEnv(t)
	s := covApiServer(t)
	rec := doOwnerGet(s, reviewRetryPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeReviewRetry(t, rec.Body.Bytes())
	if got.MaxRetries.Effective != config.ReviewRetryDefaultMax || got.MaxRetries.Source != "default" || got.MaxRetries.Saved != nil {
		t.Fatalf("max_retries = %+v", got.MaxRetries)
	}
	if got.Backoff.Effective != config.ReviewRetryDefaultBackoff.String() || got.Backoff.YAMLKey != "review_feedback.retry.backoff" {
		t.Fatalf("backoff = %+v", got.Backoff)
	}
	t.Setenv(config.ReviewRetryMaxEnvVar, "4")
	got = decodeReviewRetry(t, doOwnerGet(s, reviewRetryPath).Body.Bytes())
	if got.MaxRetries.Effective != 4 || got.MaxRetries.Source != "env" {
		t.Fatalf("env max_retries = %+v", got.MaxRetries)
	}
}

func TestReviewRetry_PutAppliesValidatesAndClears(t *testing.T) {
	clearReviewRetryEnv(t)
	s := covApiServer(t)
	cfg := s.deps.Config
	rec := doPut(s, reviewRetryPath, map[string]any{"max_retries": 0, "backoff": "90m"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	set := cfg.ReviewRetrySettingsSnapshot()
	if set.MaxRetries == nil || *set.MaxRetries != 0 || set.Backoff != "1h30m0s" {
		t.Fatalf("saved = %+v", set)
	}
	got := decodeReviewRetry(t, rec.Body.Bytes())
	if got.MaxRetries.Source != "settings" || got.MaxRetries.Effective != 0 || got.Backoff.Effective != "1h30m0s" {
		t.Fatalf("response = %+v", got)
	}
	for name, body := range map[string]map[string]any{
		"negative":      {"max_retries": -1},
		"too many":      {"max_retries": config.ReviewRetryMaxMax + 1},
		"not duration":  {"backoff": "soon"},
		"under minimum": {"backoff": "1m"},
	} {
		if rec := doPut(s, reviewRetryPath, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400", name, rec.Code)
		}
	}
	if got := cfg.ReviewRetrySettingsSnapshot(); got.MaxRetries == nil || *got.MaxRetries != 0 {
		t.Fatalf("a refused PUT changed the settings: %+v", got)
	}
	if rec := doPut(s, reviewRetryPath, map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("PUT clear: %d", rec.Code)
	}
	if cfg.ReviewFeedback != nil {
		t.Fatalf("clear left %+v", cfg.ReviewFeedback)
	}
}
