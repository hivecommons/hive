package ioscan

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// DefaultCLIClassifierBudgetPerEval is the per-pass classifier budget when
	// the classifier runs through a CLI backend. One CLI start-up per call is
	// far slower than an HTTP request, so the pass is bounded well below
	// DefaultClassifierBudgetPerEval.
	DefaultCLIClassifierBudgetPerEval = 8
	// DefaultCLIClassifierDailyQuota caps CLI classifier calls per UTC day.
	DefaultCLIClassifierDailyQuota = 500
)

// DailyQuotaExhaustedError is returned by DailyQuotaClassifier once the
// quota for the current UTC day is spent. Callers treat it like an
// exhausted per-pass budget: the text passes on unclassified.
type DailyQuotaExhaustedError struct {
	Day   string
	Quota int
}

func (e *DailyQuotaExhaustedError) Error() string {
	return fmt.Sprintf("ioscan classifier daily quota of %d calls reached for %s (UTC)", e.Quota, e.Day)
}

// DailyQuotaClassifier allows at most quota calls to next per UTC day and
// refuses the rest with *DailyQuotaExhaustedError. Wrap it in a
// CachedClassifier so cache hits do not spend the quota.
type DailyQuotaClassifier struct {
	next  Classifier
	quota int
	now   func() time.Time
	mu    sync.Mutex
	day   string
	used  int
}

// NewDailyQuotaClassifier wraps next with a per-UTC-day call quota. A quota
// of zero or less uses DefaultCLIClassifierDailyQuota; a nil now uses
// time.Now.
func NewDailyQuotaClassifier(next Classifier, quota int, now func() time.Time) *DailyQuotaClassifier {
	if quota <= 0 {
		quota = DefaultCLIClassifierDailyQuota
	}
	if now == nil {
		now = time.Now
	}
	return &DailyQuotaClassifier{next: next, quota: quota, now: now}
}

func (q *DailyQuotaClassifier) Score(ctx context.Context, text string) (InjectionScore, error) {
	q.mu.Lock()
	day := q.now().UTC().Format(time.DateOnly)
	if day != q.day {
		q.day = day
		q.used = 0
	}
	if q.used >= q.quota {
		q.mu.Unlock()
		return InjectionScore{}, &DailyQuotaExhaustedError{Day: day, Quota: q.quota}
	}
	q.used++
	q.mu.Unlock()
	return q.next.Score(ctx, text)
}
