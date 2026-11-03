package main

import (
	"testing"
	"time"
)

// A hive running without the dashboard reports no coverage, which the
// governor reads as "kick as usual" — the feed must never panic on nil.
func TestContributorRoleCoverage_NilDashboardIsNoCoverage(t *testing.T) {
	if got := contributorRoleCoverage(nil, time.Now()); got != nil {
		t.Fatalf("contributorRoleCoverage(nil) = %+v, want nil", got)
	}
}
