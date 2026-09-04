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

	advisoryFreshnessAgingAfterEnv = "HIVE_ADVISORY_ISSUE_AGING_AFTER"
	advisoryFreshnessStaleAfterEnv = "HIVE_ADVISORY_ISSUE_STALE_AFTER"

	// advisoryFreshnessAgingAfter is the first threshold where the advisory
	// digest starts to look suspect. It is intentionally below the stale
	// threshold so the fleet row can show the same source of truth progressing
	// fresh → aging → stale.
	advisoryFreshnessAgingAfter = 45 * time.Minute
	// advisoryFreshnessStaleAfter is how long a hive's advisory digest may go
	// without a successful update before both the fleet chip and stale verdict
	// agree that the advisory digest is stale.
	advisoryFreshnessStaleAfter = 90 * time.Minute

	// Backward-compatible names for existing tests and internal callers. The
	// field is still named advisoryIssueActivity on the wire, but it now reports
	// advisory-digest freshness only.
	advisoryIssueAgingAfterEnv = advisoryFreshnessAgingAfterEnv
	advisoryIssueStaleAfterEnv = advisoryFreshnessStaleAfterEnv
	advisoryIssueAgingAfter    = advisoryFreshnessAgingAfter
	advisoryIssueStaleAfter    = advisoryFreshnessStaleAfter
)

type AdvisoryIssueActivity struct {
	LastActivityAt    string `json:"lastActivityAt,omitempty"`
	Bucket            string `json:"bucket"`
	AgingAfterSeconds int64  `json:"agingAfterSeconds,omitempty"`
	StaleAfterSeconds int64  `json:"staleAfterSeconds,omitempty"`
}

func advisoryIssueActivityFor(e RegistryEntry, now time.Time) AdvisoryIssueActivity {
	return advisoryFreshnessFor(e, now)
}

func advisoryFreshnessFor(e RegistryEntry, now time.Time) AdvisoryIssueActivity {
	agingAfter, staleAfter := advisoryIssueThresholds()
	unknown := AdvisoryIssueActivity{
		Bucket:            advisoryIssueBucketUnknown,
		AgingAfterSeconds: int64(agingAfter.Seconds()),
		StaleAfterSeconds: int64(staleAfter.Seconds()),
	}

	if e.AdvisoryError != "" && !appAwaitingDelivery(e) {
		out := AdvisoryIssueActivity{
			Bucket:            advisoryIssueBucketStale,
			AgingAfterSeconds: int64(agingAfter.Seconds()),
			StaleAfterSeconds: int64(staleAfter.Seconds()),
		}
		if last, ok := advisoryDigestLastActivity(e); ok {
			out.LastActivityAt = last.UTC().Format(time.RFC3339)
		}
		return out
	}

	if e.AdvisoryError != "" {
		return unknown
	}

	last, ok := advisoryDigestLastActivity(e)
	if !ok {
		return unknown
	}
	if !appCanWriteForAdvisory(e) || allAgentsQuietByDesign(e) {
		return unknown
	}

	bucket := advisoryIssueBucketFresh
	age := now.Sub(last)
	switch {
	case age > staleAfter:
		bucket = advisoryIssueBucketStale
	case age > agingAfter:
		bucket = advisoryIssueBucketAging
	}
	return AdvisoryIssueActivity{
		LastActivityAt:    last.UTC().Format(time.RFC3339),
		Bucket:            bucket,
		AgingAfterSeconds: int64(agingAfter.Seconds()),
		StaleAfterSeconds: int64(staleAfter.Seconds()),
	}
}

func advisoryIssueThresholds() (agingAfter, staleAfter time.Duration) {
	agingAfter = durationEnvOrDefault(advisoryIssueAgingAfterEnv, advisoryIssueAgingAfter)
	staleAfter = durationEnvOrDefault(advisoryIssueStaleAfterEnv, advisoryIssueStaleAfter)
	if staleAfter <= agingAfter {
		return advisoryFreshnessAgingAfter, advisoryFreshnessStaleAfter
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

func advisoryDigestLastActivity(e RegistryEntry) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, e.AdvisoryLastPostedAt); err == nil {
		return t, true
	}
	return time.Time{}, false
}
