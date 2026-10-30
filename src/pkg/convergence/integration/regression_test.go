package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestConcurrentWritersFenceCapacityAndReplay(t *testing.T) {
	first := testStore(t)
	second, err := Open(first.path)
	if err != nil {
		t.Fatal(err)
	}
	now := epoch.Add(time.Hour)
	for _, intent := range []Intent{testIntent(1), testIntent(2)} {
		putIntent(t, first, intent)
	}
	if err := first.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	cfg := configFor(config.IntegrationEnforce)
	cfg.MaxActive = 1
	results := make(chan bool, 2)
	var workers sync.WaitGroup
	for index, writer := range []*Store{first, second} {
		workers.Add(1)
		go func(index int, writer *Store) {
			defer workers.Done()
			input := Input{Key: fmt.Sprintf("o/r#%d", index+1), Repo: "o/r", Base: "v5", Now: now, Reserve: true, PlanApproved: true, PlanRevision: 1,
				Claim: "worker", ClaimGeneration: 1, ClaimExpiresAt: now.Add(time.Hour)}
			decision, err := writer.Decide(cfg, input)
			if err != nil {
				t.Error(err)
			}
			results <- err == nil && decision.Allowed
		}(index, writer)
	}
	workers.Wait()
	close(results)
	admitted := 0
	for allowed := range results {
		if allowed {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("%d admissions for one slot across two writers", admitted)
	}
	restarted, err := Open(first.path)
	if err != nil || len(restarted.Snapshot(now).Reservations) != 1 {
		t.Fatalf("restart lost fencing: %v", err)
	}
	if err := os.Truncate(first.path, 0); err != nil {
		t.Fatal(err)
	}
	if first.EnsureIntent(testIntent(3)) == nil {
		t.Fatal("stale writer accepted truncated journal")
	}
}

func TestReleaseFencesOldGenerationAndIntentDrift(t *testing.T) {
	store := testStore(t)
	now := epoch.Add(time.Hour)
	intent := testIntent(1)
	putIntent(t, store, intent)
	if err := store.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	input := Input{Key: intent.Key, Repo: intent.Repo, Base: intent.Base, Now: now, Reserve: true, Claim: "owner", ClaimGeneration: 2, ClaimExpiresAt: now.Add(time.Hour), PlanApproved: true, PlanRevision: 1}
	if decision, err := store.Decide(configFor(config.IntegrationEnforce), input); err != nil || !decision.Allowed {
		t.Fatalf("%+v %v", decision, err)
	}
	for _, release := range []Event{
		{Version: 1, ID: "old-generation", At: now, Release: intent.Key, ReleaseClaim: "owner", ReleaseGeneration: 1},
		{Version: 1, ID: "wrong-owner", At: now, Release: intent.Key, ReleaseClaim: "other", ReleaseGeneration: 2},
	} {
		if err := store.Append(release); err != nil {
			t.Fatal(err)
		}
		if len(store.Snapshot(now).Reservations) != 1 {
			t.Fatal("delayed release freed a replacement holder")
		}
	}
	intent.Generation++
	intent.Paths = []string{"new.go"}
	putIntent(t, store, intent)
	if len(store.Snapshot(now).Reservations) != 0 {
		t.Fatal("scope generation retained old reservation")
	}
	if err := store.EnsureIntent(testIntent(1)); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot(now).Intents[0].Generation != 2 {
		t.Fatal("dispatch overwrote owner intent")
	}
}

func TestRetryBudgetRenewalAndQualification(t *testing.T) {
	store := testStore(t)
	now := epoch.Add(time.Hour)
	intent := testIntent(1)
	intent.Head = strings.Repeat("a", 40)
	putIntent(t, store, intent)
	if err := store.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	cfg := configFor(config.IntegrationEnforce)
	cfg.MaxRunnerMinutes = 15
	cfg.AdmissionRunnerMinutes = 10
	input := Input{Key: intent.Key, Repo: intent.Repo, Base: intent.Base, Now: now, Reserve: true, Claim: "owner", ClaimGeneration: 1, ClaimExpiresAt: now.Add(time.Hour), PlanApproved: true, PlanRevision: 1}
	if d, err := store.Decide(cfg, input); err != nil || !d.Allowed {
		t.Fatalf("%+v %v", d, err)
	}
	other := testIntent(2)
	putIntent(t, store, other)
	second := input
	second.Key = other.Key
	if d, err := store.Decide(cfg, second); err != nil || d.Allowed || !strings.Contains(d.Reason, "allocations") {
		t.Fatalf("%+v %v", d, err)
	}
	job := Job{Repo: "o/r", Base: "v5", ID: 1, RunID: 2, Attempt: cfg.MaxAttempts, Head: intent.Head, Status: "completed", Conclusion: "failure", ObservedAt: now}
	if err := store.Append(Event{Version: 1, ID: "failed-rerun", At: now, Job: &job}); err != nil {
		t.Fatal(err)
	}
	probe := input
	probe.Reserve = false
	if d, err := store.Decide(cfg, probe); err != nil || d.Allowed || !strings.Contains(d.Reason, "attempts") {
		t.Fatalf("%+v %v", d, err)
	}
	input.Now = now.Add(time.Minute)
	input.RenewOnly = true
	if d, err := store.Decide(cfg, input); err != nil || !d.Allowed {
		t.Fatalf("heartbeat was new admission: %+v %v", d, err)
	}
	rejected := Evidence{Subject: testSubject(), State: Rejected, ObservedAt: now, Source: "bound-producer"}
	rejected.Subject.Attempt = cfg.MaxAttempts
	if err := store.Append(Event{Version: 1, ID: "candidate-red", At: now, Evidence: &rejected}); err != nil {
		t.Fatal(err)
	}
	intent.Generation++
	intent.Head = rejected.Subject.Members[1].Head
	putIntent(t, store, intent)
	probe.Head = intent.Head
	if d, err := store.Decide(cfg, probe); err != nil || d.Allowed || !strings.Contains(d.Reason, "candidate attempts") {
		t.Fatalf("%+v %v", d, err)
	}
	intent.ScopeKnown = false
	intent.Generation++
	putIntent(t, store, intent)
	for _, mode := range []string{config.IntegrationAdvisory, config.IntegrationEnforce} {
		cfg.Mode = mode
		d, err := store.Decide(cfg, probe)
		if err != nil || !d.WouldBlock || d.Allowed != (mode == config.IntegrationAdvisory) {
			t.Fatalf("%+v %v", d, err)
		}
	}
	probe.Key = "o/r#999"
	if d, err := store.Decide(cfg, probe); err != nil || d.Allowed {
		t.Fatalf("unknown scope admitted %+v %v", d, err)
	}
}

func TestInvalidationBeforeObservationAndMergedLifecycle(t *testing.T) {
	store := testStore(t)
	subject := testSubject()
	if err := store.InvalidateMember(subject.Repo, 1, "", "review withdrawn", "early", epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	passed := Evidence{Subject: subject, State: Passed, ObservedAt: epoch, Source: "bound-producer", RunID: 1, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.Append(Event{Version: 1, ID: "late-observation", At: epoch.Add(time.Hour), Evidence: &passed}); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(epoch).Evidence[0]; got.State != Superseded || got.InvalidatedBy != "member/early" {
		t.Fatalf("%+v", got)
	}
	if err := store.InvalidateMember(subject.Repo, 1, "", "review withdrawn", "early", epoch.Add(time.Hour)); err != nil {
		t.Fatal("delivery replay changed invalidation", err)
	}
	fresh := passed
	fresh.Subject.Generation++
	fresh.ObservedAt = epoch.Add(2 * time.Minute)
	if err := store.Append(Event{Version: 1, ID: "fresh-observation", At: fresh.ObservedAt, Evidence: &fresh}); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateGroup(subject.Repo, subject.Authority, strings.Repeat("f", 40), "other", "different group destroyed", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if store.evidence[fresh.Subject.ID()].State != Passed {
		t.Fatal("unrelated group destroyed valid prefix")
	}
	if err := store.InvalidateGroup(subject.Repo, subject.Authority, subject.TestedSHA, "destroyed", "group destroyed", epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if store.evidence[fresh.Subject.ID()].State != Superseded {
		t.Fatal("destroyed group proof retained")
	}
	receipt := IntegrationReceipt{Repo: "o/r", PR: 1, Head: subject.Members[0].Head, MergeSHA: subject.TestedSHA, MergedAt: epoch, Source: "signed-merge"}
	if err := store.Append(Event{Version: 1, ID: "merged", At: epoch, Integrated: &receipt}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Integrated("o/r", 1); !ok {
		t.Fatal("merge receipt missing")
	}
	if _, ok := store.Integrated("o/r", 2); ok {
		t.Fatal("closure inferred integration")
	}
	restarted, err := Open(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Snapshot(epoch).Integrated) != 1 {
		t.Fatal("merge receipt lost across restart")
	}
}

func TestStoreRejectsMalformedObservationsAndFilesystemFailures(t *testing.T) {
	store := testStore(t)
	for index, event := range []Event{
		{Version: 99, ID: "bad", At: epoch},
		{Version: 1, ID: "bad", At: epoch, Job: &Job{}},
		{Version: 1, ID: "bad", At: epoch, Coverage: &Coverage{}},
		{Version: 1, ID: "bad", At: epoch, Reservation: &Reservation{}},
		{Version: 1, ID: "bad", At: epoch, Integrated: &IntegrationReceipt{}},
		{Version: 1, ID: "bad", At: epoch, MemberChange: &MemberChange{}},
		{Version: 1, ID: "bad", At: epoch, Evidence: &Evidence{Subject: testSubject(), State: "invented"}},
		{Version: 1, ID: "bad", At: epoch, Evidence: &Evidence{State: Unknown}},
		{Version: 1, ID: "bad", At: epoch, Evidence: &Evidence{Subject: testSubject(), State: Passed, ObservedAt: epoch, Source: "unbound"}},
		{Version: 1, ID: "bad", At: epoch, Job: &Job{Repo: "o/r", ID: 1, RunID: 2, Attempt: 1, Head: strings.Repeat("a", 40), ObservedAt: epoch, RunnerID: 3, StartedAt: epoch, CompletedAt: epoch.Add(-time.Hour)}},
	} {
		event.ID = fmt.Sprintf("invalid-%d", index)
		if store.Append(event) == nil {
			t.Fatalf("invalid event %d accepted", index)
		}
	}
	i := testIntent(1)
	i.Prerequisites = []Prerequisite{{Key: "o/r#2", PlanNode: "2", Condition: "closed-is-deployed"}}
	if i.Validate() == nil {
		t.Fatal("ambiguous dependency condition accepted")
	}
	if err := os.WriteFile(store.path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store.path); err == nil {
		t.Fatal("invalid replay envelope accepted")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	unwritable, err := Open(filepath.Join(blocker, "journal"))
	if err == nil && unwritable.EnsureIntent(testIntent(1)) == nil {
		t.Fatal("file parent accepted as directory")
	}
	lockBlock := testStore(t)
	if err := os.Mkdir(lockBlock.path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	if lockBlock.Append(coverageEvent(epoch)) == nil {
		t.Fatal("unavailable lock ignored")
	}
}
