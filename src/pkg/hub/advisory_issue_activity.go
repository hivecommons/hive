package hub

import (
	"os"
	"strings"
	"time"
)

const (
	advisoryIssueBucketFresh   = "fresh"
	advisoryIssueBucketAging   = "aging"
	advisoryIssueBucketStale   = "stale"
	advisoryIssueBucketUnknown = "unknown"

	advisoryIssueAgingAfterEnv = "HIVE_ADVISORY_ISSUE_AGING_AFTER"
	advisoryIssueStaleAfterEnv = "HIVE_ADVISORY_ISSUE_STALE_AFTER"

	// advisoryIssueAgingAfter is the first threshold where advisory/issue output
	// becomes suspect. A day leaves room for low-volume repos and quiet work
	// windows, while still making a silent Security Agent visible before trust is
	// lost.
	advisoryIssueAgingAfter = 24 * time.Hour
	// advisoryIssueStaleAfter is the operator-action threshold: after three days
	// without a digest post or actionable-issue count movement, the output stream
	// is stale enough to imply stuck/paused/broken agents rather than normal quiet.
	advisoryIssueStaleAfter = 72 * time.Hour
)

type AdvisoryIssueActivity struct {
	LastActivityAt string `json:"lastActivityAt,omitempty"`
	Bucket         string `json:"bucket"`
}

func advisoryIssueActivityFor(e RegistryEntry, now time.Time) AdvisoryIssueActivity {
	last, ok := advisoryIssueLastActivity(e)
	if e.AdvisoryError != "" && !appAwaitingDelivery(e) {
		out := AdvisoryIssueActivity{Bucket: advisoryIssueBucketStale}
		if ok {
			out.LastActivityAt = last.UTC().Format(time.RFC3339)
		}
		return out
	}
	if !ok {
		return AdvisoryIssueActivity{Bucket: advisoryIssueBucketUnknown}
	}
	agingAfter, staleAfter := advisoryIssueThresholds()
	bucket := advisoryIssueBucketFresh
	age := now.Sub(last)
	switch {
	case age > staleAfter:
		bucket = advisoryIssueBucketStale
	case age > agingAfter:
		bucket = advisoryIssueBucketAging
	}
	return AdvisoryIssueActivity{
		LastActivityAt: last.UTC().Format(time.RFC3339),
		Bucket:         bucket,
	}
}

func advisoryIssueThresholds() (agingAfter, staleAfter time.Duration) {
	agingAfter = durationEnvOrDefault(advisoryIssueAgingAfterEnv, advisoryIssueAgingAfter)
	staleAfter = durationEnvOrDefault(advisoryIssueStaleAfterEnv, advisoryIssueStaleAfter)
	if staleAfter <= agingAfter {
		return advisoryIssueAgingAfter, advisoryIssueStaleAfter
	}
	return agingAfter, staleAfter
}

func durationEnvOrDefault(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func advisoryIssueLastActivity(e RegistryEntry) (time.Time, bool) {
	var last time.Time
	if t, err := time.Parse(time.RFC3339, e.AdvisoryLastPostedAt); err == nil {
		last = t
	}
	if t, ok := lastIssueHistoryActivity(e.IssueHistory); ok && t.After(last) {
		last = t
	}
	if last.IsZero() {
		return time.Time{}, false
	}
	return last, true
}

func lastIssueHistoryActivity(points []SparkPoint) (time.Time, bool) {
	if len(points) == 0 {
		return time.Time{}, false
	}
	var last int64
	prev := points[0].V
	if prev > 0 {
		last = points[0].T
	}
	for _, p := range points[1:] {
		if p.V != prev {
			last = p.T
			prev = p.V
		}
	}
	if last <= 0 {
		return time.Time{}, false
	}
	return time.Unix(last, 0).UTC(), true
}
