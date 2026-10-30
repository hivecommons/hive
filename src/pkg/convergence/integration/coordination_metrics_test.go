package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestObservedScopeDriftAndFalsePositiveRemainAdvisoryAndDurable(t *testing.T) {
	store := testStore(t)
	intent := testIntent(1)
	intent.Head = strings.Repeat("a", 40)
	intent.Paths = []string{"src/api"}
	intent.Generated = []string{"dashboard.js"}
	intent.Relations = []RelationAssessment{{Key: "o/r#2", Kind: "semantic", Disposition: "false-positive", Provenance: "owner-reviewed-independent-contracts"}}
	putIntent(t, store, intent)
	if err := store.ObserveFiles("elsewhere/r", intent.Head, []string{"unrelated"}, epoch); err != nil {
		t.Fatal(err)
	}
	if len(store.Snapshot(epoch).Drifts) != 0 {
		t.Fatal("cross-repository file observation attached")
	}
	files := []string{"src/api/handler.go", "dashboard.js", "outside.go"}
	if err := store.ObserveFiles("o/r", intent.Head, files, epoch); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveFiles("o/r", intent.Head, files, epoch.Add(time.Hour)); err != nil {
		t.Fatal("probe replay conflict", err)
	}
	snapshot := store.Snapshot(epoch)
	if len(snapshot.Drifts) != 1 || snapshot.Drifts[0].HeadVerified || len(snapshot.Drifts[0].OutsideAnticipated) != 1 ||
		snapshot.Drifts[0].OutsideAnticipated[0] != "outside.go" {
		t.Fatalf("%+v", snapshot.Drifts)
	}
	snapshot.Drifts[0].Actual[0] = "mutated"
	if store.Snapshot(epoch).Drifts[0].Actual[0] == "mutated" {
		t.Fatal("drift snapshot aliases ledger")
	}
	if !strings.Contains(relationDisposition(intent, "o/r#2", "semantic"), "false-positive") || relationDisposition(intent, "o/r#2", "file") != "" {
		t.Fatal("relation assessment lost kind or disposition")
	}
	restarted, err := Open(store.path)
	if err != nil || len(restarted.Snapshot(epoch).Drifts) != 1 {
		t.Fatalf("restart=%v", err)
	}
	bad := intent
	bad.Relations = []RelationAssessment{{Key: "o/r#2", Kind: "semantic", Disposition: "permanent-lock", Provenance: "text"}}
	if bad.Validate() == nil {
		t.Fatal("semantic feedback became a hard control")
	}
}

func TestScopedWindowCostPerMergedPRIncludesSharedAndCanceledExecution(t *testing.T) {
	store := testStore(t)
	now := epoch.Add(time.Hour)
	job := Job{Repo: "o/r", Base: "v5", ID: 1, RunID: 20, Attempt: 1, Head: testSubject().TestedSHA, RunnerID: 30,
		StartedAt: now.Add(-30 * time.Minute), CompletedAt: now.Add(-10 * time.Minute), ObservedAt: now, Status: "completed", Conclusion: "cancelled"}
	if err := store.Append(Event{Version: 1, ID: "physical-job", At: now, Job: &job}); err != nil {
		t.Fatal(err)
	}
	subject := testSubject()
	evidence := Evidence{Subject: subject, State: Rejected, ObservedAt: now, Source: "github-actions-bound-v1", RunID: 20, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.Append(Event{Version: 1, ID: "candidate", At: now, Evidence: &evidence}); err != nil {
		t.Fatal(err)
	}
	evidence.Subject.Policy = "second-policy"
	if err := store.Append(Event{Version: 1, ID: "shared-candidate", At: now, Evidence: &evidence}); err != nil {
		t.Fatal(err)
	}
	unknown := job
	unknown.ID, unknown.RunID, unknown.StartedAt, unknown.CompletedAt = 2, 99, now.Add(-10*time.Minute), now
	if err := store.Append(Event{Version: 1, ID: "unclassified-execution", At: now, Job: &unknown}); err != nil {
		t.Fatal(err)
	}
	merged := IntegrationReceipt{Repo: "o/r", Base: "v5", PR: 1, Head: subject.Members[0].Head, MergeSHA: subject.TestedSHA, MergedAt: now.Add(-5 * time.Minute), Source: "github-merged-pr-rest-v1"}
	if err := store.Append(Event{Version: 1, ID: "merged", At: now, Integrated: &merged}); err != nil {
		t.Fatal(err)
	}
	coverage := coverageEvent(now)
	coverage.Coverage.MergedComplete = true
	if err := store.Append(coverage); err != nil {
		t.Fatal(err)
	}
	report := store.CostReport("O/R", "v5", now.Add(-time.Hour), now)
	if report.MergedPRs == nil || *report.MergedPRs != 1 || report.RunnerMinutesPerMergedPR == nil || *report.RunnerMinutesPerMergedPR != 30 ||
		report.AttributedIntegrationMinutes == nil || *report.AttributedIntegrationMinutes != 20 || report.UnattributedExecutionJobs != 1 {
		t.Fatalf("%+v", report)
	}
	coverage.ID = "incomplete-denominator"
	coverage.Coverage.MergedComplete = false
	if err := store.Append(coverage); err != nil {
		t.Fatal(err)
	}
	report = store.CostReport("o/r", "v5", now.Add(-time.Hour), now)
	if report.MergedPRs != nil || report.RunnerMinutesPerMergedPR != nil || report.ObservedMergedPRs != 1 {
		t.Fatal("unknown denominator became zero or a claimed complete ratio")
	}
}

func TestConfirmedCombinedFailureShrinksOnlyBoundedHiveAdmissions(t *testing.T) {
	store := testStore(t)
	now := epoch.Add(time.Hour)
	cfg := configFor(config.IntegrationEnforce)
	cfg.MaxActive = 3
	if err := store.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{1, 2, 3} {
		putIntent(t, store, testIntent(number))
	}
	input := Input{Repo: "o/r", Base: "v5", Now: now, PlanApproved: true, PlanRevision: 1, Claim: "owner", ClaimGeneration: 1, ClaimExpiresAt: now.Add(time.Hour), Reserve: true}
	for _, key := range []string{"o/r#1", "o/r#2"} {
		input.Key = key
		if decision, err := store.Decide(cfg, input); err != nil || !decision.Allowed {
			t.Fatalf("%+v %v", decision, err)
		}
	}
	rejected := Evidence{Subject: testSubject(), State: Rejected, Source: "github-actions-bound-v1", ObservedAt: now, RunID: 20, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.Append(Event{Version: 1, ID: "combined-red", At: now, Evidence: &rejected}); err != nil {
		t.Fatal(err)
	}
	input.Key, input.Reserve = "o/r#3", false
	decision, err := store.Decide(cfg, input)
	if err != nil || decision.Allowed || decision.ActiveLimit != 2 || decision.FailureBackoff != 1 {
		t.Fatalf("%+v %v", decision, err)
	}
	if len(store.Snapshot(now).Reservations) != 2 {
		t.Fatal("failure canceled existing valid work")
	}
	cfg.Mode = config.IntegrationAdvisory
	if decision, err := store.Decide(cfg, input); err != nil || !decision.Allowed || !decision.WouldBlock {
		t.Fatalf("%+v %v", decision, err)
	}
}
