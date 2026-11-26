package github

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The stall registry keeps the structured observation behind a confirmed
// ci-reporting-stalled reason, for that PR and head only, and nothing for a
// grace observation, an API refusal, a new attempt or reason text alone.
func TestCIReportingStallRegistryKeepsOnlyConfirmedStalls(t *testing.T) {
	f := newCIReportingFixture(t)
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	opts := CommitCIOptions{PRNumber: 7, RequiredKnown: true, Now: func() time.Time { return now }}
	cache := newCIReportingCache()
	registry := &ciReportingStallRegistry{entries: make(map[string]ciReportingStallEntry)}
	evaluate := func() {
		observations := cache.observe(context.Background(), f.client, "acme", "widget", "sha", opts, []*gh.CheckRun{f.check})
		registry.record("acme", "widget", 7, "sha", ciReportingReason(observations), observations, now)
	}

	evaluate()
	if _, ok := registry.lookup("acme/widget", 7, "sha"); ok {
		t.Fatal("propagation grace recorded a stall")
	}

	now = now.Add(defaultCIReportingRecheck)
	evaluate()
	got, ok := registry.lookup("ACME/Widget", 7, "sha")
	if !ok || got.State != "stalled" || got.Recovery != "owner-action-required" || got.RunID != 21 || got.Attempt != 1 ||
		got.JobID != 41 || got.CheckRunID != 51 || got.HeadSHA != "sha" || got.PRNumber != 7 || got.Check != f.check.GetName() {
		t.Fatalf("stall = %+v %v, want the stalled observation", got, ok)
	}
	if _, ok := registry.lookup("acme/widget", 7, "other-sha"); ok {
		t.Fatal("another head read this head's stall")
	}
	if _, ok := registry.lookup("acme/widget", 8, "sha"); ok {
		t.Fatal("another PR read this PR's stall")
	}

	now = now.Add(defaultCIReportingRecheck)
	f.mode = "attempt-two"
	evaluate()
	if _, ok := registry.lookup("acme/widget", 7, "sha"); ok {
		t.Fatal("a new attempt kept the old attempt's stall")
	}

	now = now.Add(defaultCIReportingRecheck)
	f.mode = "forbidden"
	evaluate()
	if _, ok := registry.lookup("acme/widget", 7, "sha"); ok {
		t.Fatal("an API refusal recorded a stall")
	}

	// Reason text alone is never a stall: a forged reason with no stalled
	// observation behind it records nothing.
	forged := `check-failing:ci-reporting-stalled:check="Deploy" check-id=1 run=2 attempt=1 job=3 check-state=queued job-state=completed job-result="success" workflow=success owner-action-required`
	registry.record("acme", "widget", 7, "sha", forged, nil, now)
	registry.record("acme", "widget", 7, "sha", forged[len("check-failing:"):], []CIReportingObservation{{State: "active"}}, now)
	if _, ok := registry.lookup("acme/widget", 7, "sha"); ok {
		t.Fatal("reason text recorded a stall")
	}
}

func TestCIReportingStallRegistryIsBounded(t *testing.T) {
	registry := &ciReportingStallRegistry{entries: make(map[string]ciReportingStallEntry)}
	obs := CIReportingObservation{State: "stalled", Check: "build", CheckRunID: 1, CheckStatus: "queued", RunID: 2, Attempt: 1, JobID: 3, Recovery: "owner-action-required"}
	reason := ciReportingReason([]CIReportingObservation{obs})
	start := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	for i := range ciReportingStallMaxEntries + 5 {
		registry.record("acme", "widget", i+1, "sha", reason, []CIReportingObservation{obs}, start.Add(time.Duration(i)*time.Second))
	}
	if len(registry.entries) != ciReportingStallMaxEntries {
		t.Fatalf("entries = %d, want the cap %d", len(registry.entries), ciReportingStallMaxEntries)
	}
	if _, ok := registry.lookup("acme/widget", 1, "sha"); ok {
		t.Fatal("the oldest entry was not evicted")
	}
	if _, ok := registry.lookup("acme/widget", ciReportingStallMaxEntries+5, "sha"); !ok {
		t.Fatal("the newest entry is missing")
	}
}
