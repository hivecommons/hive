package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestDestroyedGroupAndPolicyCannotReviveAfterRestart(t *testing.T) {
	store := testStore(t)
	subject := testSubject()
	policy := config.IntegrationPolicy{Repo: subject.Repo, Base: subject.Base, Engine: subject.Engine, Workflow: subject.Workflow, Blob: strings.Repeat("a", 40), AppID: 42, Jobs: []string{"policy"}}
	subject.Policy = policy.Fingerprint()
	if err := store.SetPolicies([]config.IntegrationPolicy{policy}, epoch); err != nil {
		t.Fatal(err)
	}
	receipt := Evidence{Subject: subject, State: Passed, ObservedAt: epoch, Source: "github-actions-bound-v1", RunID: 1, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.InvalidateGroup(subject.Repo, subject.Authority, subject.TestedSHA, "before-create", "group destroyed", epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateGroup(subject.Repo, subject.Authority, subject.TestedSHA, "before-create", "group destroyed", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(Event{Version: 1, ID: "late-pass", At: epoch.Add(time.Hour), Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	if store.evidence[subject.ID()].State != Superseded {
		t.Fatal("destroy-before-create revived passing group")
	}
	receipt.Subject.Authority = "another-group"
	if err := store.Append(Event{Version: 1, ID: "other-pass", At: epoch, Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	if store.evidence[receipt.Subject.ID()].State != Passed {
		t.Fatal("independent group blocked")
	}
	if err := store.SetPolicies(nil, epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if store.evidence[receipt.Subject.ID()].State != Superseded {
		t.Fatal("removed policy remained passed")
	}
	receipt.Subject.Authority = "late-old-policy"
	receipt.ObservedAt = epoch.Add(time.Hour)
	if err := store.Append(Event{Version: 1, ID: "late-old-policy", At: receipt.ObservedAt, Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	if store.evidence[receipt.Subject.ID()].State != Superseded {
		t.Fatal("old policy revived after removal")
	}
	restarted, err := Open(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetPolicies(nil, epoch.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(restarted.Snapshot(epoch).Evidence) != 3 || restarted.evidence[receipt.Subject.ID()].State != Superseded {
		t.Fatal("policy lineage not replayed")
	}
}

func TestJobCandidateCorrelationIsOrderIndependentAndNotDoubleCounted(t *testing.T) {
	for _, jobFirst := range []bool{false, true} {
		store := testStore(t)
		subject := testSubject()
		job := Job{Repo: subject.Repo, Base: subject.Base, ID: 10, RunID: 20, Attempt: subject.Attempt, Head: subject.TestedSHA, RunnerID: 30,
			StartedAt: epoch, CompletedAt: epoch.Add(time.Minute), ObservedAt: epoch.Add(time.Minute), Status: "completed", Conclusion: "success"}
		evidence := Evidence{Subject: subject, State: Passed, ObservedAt: epoch, Source: "bound", RunID: 20, SuiteID: 2, CheckIDs: []int64{3}}
		jobEvent := Event{Version: 1, ID: "job", At: job.ObservedAt, Job: &job}
		proofEvent := Event{Version: 1, ID: "proof", At: epoch, Evidence: &evidence}
		events := []Event{proofEvent, jobEvent}
		if jobFirst {
			events = []Event{jobEvent, proofEvent}
		}
		for _, event := range events {
			if err := store.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		evidence.Subject.Policy = "another-independent-workflow-policy"
		if err := store.Append(Event{Version: 1, ID: "shared-proof", At: epoch, Evidence: &evidence}); err != nil {
			t.Fatal(err)
		}
		if len(store.jobs[job.Key()].Candidates) != 2 || len(store.jobs) != 1 {
			t.Fatalf("%+v", store.jobs)
		}
		if len(store.ForTestedHead("O/R", subject.TestedSHA)) != 2 || len(store.ForTestedHead("other/r", subject.TestedSHA)) != 0 {
			t.Fatal("head lookup lost repository identity")
		}
		snapshot := store.Snapshot(epoch)
		snapshot.Jobs[0].Candidates[0] = "mutated"
		if store.jobs[job.Key()].Candidates[0] == "mutated" {
			t.Fatal("snapshot aliased durable job")
		}
	}
}

func TestDispatchIntentReconcilesDAGWithoutOverwritingOwnerScope(t *testing.T) {
	store := testStore(t)
	intent := testIntent(1)
	intent.Plan, intent.PlanRevision, intent.ScopeKnown, intent.Provenance = "unresolved", 0, false, "dispatch-enumerator"
	if err := store.EnsureIntent(intent); err != nil {
		t.Fatal(err)
	}
	intent.Plan, intent.PlanRevision = "approved-epic", 2
	intent.ObservedAt = epoch.Add(time.Minute)
	if err := store.EnsureIntent(intent); err != nil {
		t.Fatal(err)
	}
	current, found := store.Intent(intent.Key)
	if !found || current.Generation != 2 || current.PlanRevision != 2 || !current.ObservedAt.Equal(epoch.Add(time.Second)) {
		t.Fatalf("%+v", current)
	}
	if err := store.EnsureIntent(intent); err != nil {
		t.Fatal(err)
	}
	current.Generation, current.ScopeKnown, current.Provenance = 3, true, "owner-approved-scope"
	putIntent(t, store, current)
	intent.PlanRevision = 4
	if err := store.EnsureIntent(intent); err != nil {
		t.Fatal(err)
	}
	current, _ = store.Intent(intent.Key)
	if current.Generation != 3 || !current.ScopeKnown {
		t.Fatal("automatic discovery overwrote owner intent")
	}
	if len(store.Intents()) != 1 {
		t.Fatal("canonical work duplicated")
	}
	if _, present := store.Reservation(intent.Key, epoch); present {
		t.Fatal("unknown reservation invented")
	}
}
