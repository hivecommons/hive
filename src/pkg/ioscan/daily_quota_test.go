package ioscan

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDailyQuotaClassifier_StopsForRestOfUTCDayThenResets(t *testing.T) {
	now := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
	next := &countingClassifier{}
	q := NewDailyQuotaClassifier(next, 3, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if _, err := q.Score(context.Background(), "text"); err != nil {
			t.Fatalf("call %d within quota: %v", i, err)
		}
	}
	_, err := q.Score(context.Background(), "text")
	var qe *DailyQuotaExhaustedError
	if !errors.As(err, &qe) {
		t.Fatalf("over quota err = %v, want *DailyQuotaExhaustedError", err)
	}
	if qe.Day != "2026-10-24" || qe.Quota != 3 {
		t.Fatalf("quota error = %+v", qe)
	}
	if next.calls != 3 {
		t.Fatalf("next called %d times, want 3", next.calls)
	}

	// Still the same UTC day an hour before midnight in another zone.
	now = time.Date(2026, 10, 24, 19, 59, 0, 0, time.FixedZone("EDT", -4*3600))
	if _, err := q.Score(context.Background(), "text"); !errors.As(err, &qe) {
		t.Fatalf("same UTC day should stay exhausted, got %v", err)
	}

	now = time.Date(2026, 10, 25, 0, 0, 1, 0, time.UTC)
	if _, err := q.Score(context.Background(), "text"); err != nil {
		t.Fatalf("new UTC day should reset the quota: %v", err)
	}
	if next.calls != 4 {
		t.Fatalf("next called %d times after reset, want 4", next.calls)
	}
}

func TestDailyQuotaClassifier_Defaults(t *testing.T) {
	q := NewDailyQuotaClassifier(&countingClassifier{}, 0, nil)
	if q.quota != DefaultCLIClassifierDailyQuota || q.now == nil {
		t.Fatalf("defaults not applied: quota=%d now=%v", q.quota, q.now != nil)
	}
}

func TestCachedDailyQuotaClassifier_CacheHitsDoNotSpendQuota(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	next := &countingClassifier{}
	c := NewCachedClassifier(NewDailyQuotaClassifier(next, 1, func() time.Time { return now }), DefaultClassifierCacheEntries)
	for i := 0; i < 5; i++ {
		if _, err := c.Score(context.Background(), "same text"); err != nil {
			t.Fatalf("cached call %d: %v", i, err)
		}
	}
	if next.calls != 1 {
		t.Fatalf("next called %d times, want 1", next.calls)
	}
	if _, err := c.Score(context.Background(), "other text"); err == nil {
		t.Fatal("uncached text over quota should fail")
	}
}

func TestNewCLIClassifier_RefusesBackendsWithoutToollessMode(t *testing.T) {
	for _, backend := range []string{"codex", "gemini", "unknown"} {
		if _, err := NewCLIClassifier(backend, "", 0, nil); err == nil {
			t.Fatalf("NewCLIClassifier(%q) succeeded, want refusal", backend)
		}
	}
	for _, backend := range []string{"claude", "copilot"} {
		if _, err := NewCLIClassifier(backend, "", 0, nil); err != nil {
			t.Fatalf("NewCLIClassifier(%s): %v", backend, err)
		}
	}
	// omp has no default model in its isolated profile: a model is required.
	if _, err := NewCLIClassifier("omp", "", 0, nil); !errors.Is(err, ErrCLIModelRequired) {
		t.Fatalf("NewCLIClassifier(omp, \"\") = %v, want ErrCLIModelRequired", err)
	}
	if _, err := NewCLIClassifier("omp", "anthropic/claude-sonnet-5", 0, nil); err != nil {
		t.Fatalf("NewCLIClassifier(omp, model): %v", err)
	}
}
