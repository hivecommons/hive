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

var epoch = time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testIntent(n int) Intent {
	return Intent{Version: SchemaVersion, Key: fmt.Sprintf("o/r#%d", n), Repo: "o/r", Base: "v5", Generation: 1,
		Plan: "plan", PlanNode: fmt.Sprint(n), PlanRevision: 1, ScopeKnown: true, Confidence: "declared", Provenance: "approved-plan", ObservedAt: epoch.Add(time.Duration(n) * time.Second)}
}

func testSubject() Subject {
	return Subject{Version: SchemaVersion, Repo: "o/r", Base: "v5", Engine: "native", Authority: "group/ref",
		Members: []Member{{PR: 1, Head: strings.Repeat("a", 40), Entry: "entry1", Position: 1}, {PR: 2, Head: strings.Repeat("b", 40), Entry: "entry2", Position: 2}},
		BaseSHA: strings.Repeat("c", 40), TestedSHA: strings.Repeat("d", 40), TestedTree: strings.Repeat("e", 40),
		Policy: "policy@1", Producer: "app/42", Workflow: ".github/workflows/test.yml@abc", Attempt: 1, Generation: 1}
}

func putIntent(t *testing.T, s *Store, i Intent) {
	t.Helper()
	if err := s.Append(Event{Version: SchemaVersion, ID: fmt.Sprintf("intent/%s/%d", i.Key, i.Generation), At: i.ObservedAt, Intent: &i}); err != nil {
		t.Fatal(err)
	}
}

func coverageEvent(now time.Time) Event {
	c := Coverage{Repo: "o/r", Base: "v5", From: now.Add(-2 * time.Hour), Through: now, Complete: true, Source: "github-actions"}
	return Event{Version: SchemaVersion, ID: "coverage", At: now, Coverage: &c}
}

func configFor(mode string) config.IntegrationOrchestrationConfig {
	return config.IntegrationOrchestrationConfig{Mode: mode, Scopes: []config.IntegrationScope{{Repo: "o/r", Base: "v5"}}}.Effective()
}

func TestExactSubjectIdentity(t *testing.T) {
	s := testSubject()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*Subject){
		func(s *Subject) { s.Members = []Member{s.Members[1]}; s.Members[0].Position = 1 },
		func(s *Subject) {
			s.Members = []Member{s.Members[1], s.Members[0]}
			s.Members[0].Position, s.Members[1].Position = 1, 2
		},
		func(s *Subject) { s.BaseSHA = strings.Repeat("f", 40) },
		func(s *Subject) { s.TestedTree = strings.Repeat("f", 40) },
		func(s *Subject) { s.Policy = "policy@2" },
		func(s *Subject) { s.Producer = "app/99" },
		func(s *Subject) { s.Workflow = "different" },
		func(s *Subject) { s.Attempt++ },
		func(s *Subject) { s.Generation++ },
	} {
		next := s
		next.Members = append([]Member(nil), s.Members...)
		modify(&next)
		if next.ID() == s.ID() {
			t.Fatal("candidate identity transferred across different tested subject")
		}
	}
	for _, modify := range []func(*Subject){
		func(s *Subject) { s.Version++ }, func(s *Subject) { s.Engine = "invented" },
		func(s *Subject) { s.TestedSHA = "short" }, func(s *Subject) { s.Policy = "" },
		func(s *Subject) { s.Members[1].PR = s.Members[0].PR }, func(s *Subject) { s.Members[1].Position = 9 },
	} {
		next := testSubject()
		modify(&next)
		if next.Validate() == nil {
			t.Fatal("invalid exact subject accepted")
		}
	}
}

func TestJournalReplayInvalidationAndOutOfOrder(t *testing.T) {
	s := testStore(t)
	subject := testSubject()
	evidence := Evidence{Subject: subject, State: Passed, ObservedAt: epoch, Source: "github", RunID: 1, SuiteID: 2, CheckIDs: []int64{3}}
	e := Event{Version: SchemaVersion, ID: "pass", At: epoch, Evidence: &evidence}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateMember("o/r", 1, strings.Repeat("f", 40), "head changed", "delivery", epoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	evidence.ObservedAt = epoch.Add(time.Hour)
	e.ID = "late-pass"
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	snap := reopened.Snapshot(epoch)
	if len(snap.Evidence) != 1 || snap.Evidence[0].State != Superseded || snap.Evidence[0].InvalidatedBy == "" {
		t.Fatalf("replayed evidence=%+v", snap.Evidence)
	}
	// Removing predecessor does not produce a receipt for its successor.
	successor := subject
	successor.Members = []Member{subject.Members[1]}
	successor.Members[0].Position = 1
	if _, ok := reopened.evidence[successor.ID()]; ok {
		t.Fatal("group proof reused after predecessor removal")
	}
	e.ID = "pass"
	if err := s.Append(e); err == nil {
		t.Fatal("reused delivery ID changed its content")
	}
}

func TestIntentDAGGenerationAndRecovery(t *testing.T) {
	s := testStore(t)
	a, b := testIntent(1), testIntent(2)
	a.Prerequisites = []Prerequisite{{Key: b.Key, PlanNode: b.PlanNode}}
	putIntent(t, s, a)
	putIntent(t, s, b)
	b.Generation = 2
	b.Prerequisites = []Prerequisite{{Key: a.Key, PlanNode: a.PlanNode}}
	if err := s.Append(Event{Version: SchemaVersion, ID: "cycle", At: epoch, Intent: &b}); err == nil {
		t.Fatal("cycle accepted")
	}
	b.Prerequisites = nil
	putIntent(t, s, b)
	older := testIntent(2)
	if err := s.Append(Event{Version: SchemaVersion, ID: "old-intent", At: epoch, Intent: &older}); err != nil {
		t.Fatal(err)
	}
	if s.intents[b.Key].Generation != 2 {
		t.Fatal("intent generation regressed")
	}
	for _, i := range []Intent{{}, func() Intent { x := a; x.Prerequisites = []Prerequisite{{Key: a.Key, PlanNode: a.PlanNode}}; return x }()} {
		if i.Validate() == nil {
			t.Fatal("invalid intent accepted")
		}
	}
	invalid := Event{Version: SchemaVersion, ID: "empty", At: epoch}
	if s.Append(invalid) == nil {
		t.Fatal("empty event accepted")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), "corrupt"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(filepath.Dir(s.path), "corrupt")); err == nil {
		t.Fatal("corrupt ledger reset to empty")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(s.path), "partial"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(filepath.Dir(s.path), "partial")); err == nil {
		t.Fatal("incomplete tail accepted")
	}
}

func TestOffAdvisoryAndValidatedDependencies(t *testing.T) {
	s := testStore(t)
	i := testIntent(2)
	i.Prerequisites = []Prerequisite{{Key: "o/r#1", PlanNode: "1"}}
	putIntent(t, s, i)
	in := Input{Key: i.Key, Repo: i.Repo, Base: i.Base, Now: epoch.Add(time.Hour)}
	for _, mode := range []string{config.IntegrationOff, config.IntegrationAdvisory, config.IntegrationEnforce} {
		d, err := s.Decide(configFor(mode), in)
		if err != nil || d.Allowed != (mode != config.IntegrationEnforce) {
			t.Fatalf("mode=%s decision=%+v err=%v", mode, d, err)
		}
	}
	if err := s.Append(coverageEvent(in.Now)); err != nil {
		t.Fatal(err)
	}
	in.PlanApproved, in.PlanRevision = true, 1
	in.Dependencies = map[string]Dependency{"o/r#1": {Known: true, Satisfied: false}}
	d, err := s.Decide(configFor(config.IntegrationEnforce), in)
	if err != nil || d.Allowed || !strings.Contains(d.Reason, "prerequisite") {
		t.Fatalf("%+v %v", d, err)
	}
	in.Dependencies["o/r#1"] = Dependency{Known: true, Satisfied: true}
	d, err = s.Decide(configFor(config.IntegrationEnforce), in)
	if err != nil || !d.Allowed {
		t.Fatalf("%+v %v", d, err)
	}
	in.Base = "other"
	d, err = s.Decide(configFor(config.IntegrationEnforce), in)
	if err != nil || !d.Allowed {
		t.Fatal("out of scope blocked")
	}
}

func TestReservationsRaceRenewExpiryRollbackAndHints(t *testing.T) {
	s := testStore(t)
	now := epoch.Add(time.Hour)
	if err := s.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	a, b, c := testIntent(1), testIntent(2), testIntent(3)
	a.Paths, b.Paths = []string{"same.go"}, []string{"same.go"}
	a.Contracts, c.Contracts = []string{"wire/v1"}, []string{"wire/v1"}
	c.Paths = []string{"different.go"}
	for _, i := range []Intent{a, b, c} {
		putIntent(t, s, i)
	}
	cfg := configFor(config.IntegrationEnforce)
	input := Input{Key: a.Key, Repo: "o/r", Base: "v5", Now: now, Reserve: true, Claim: "owner", ClaimGeneration: 1, ClaimExpiresAt: now.Add(time.Hour), PlanApproved: true, PlanRevision: 1}
	d, err := s.Decide(cfg, input)
	if err != nil || !d.Allowed {
		t.Fatalf("%+v %v", d, err)
	}
	for _, i := range []Intent{b, c} {
		in := input
		in.Key = i.Key
		in.Reserve = false
		d, err := s.Decide(cfg, in)
		if err != nil || !d.Allowed || len(d.Hints) != 1 {
			t.Fatalf("overlap became lock: %+v %v", d, err)
		}
	}
	var wg sync.WaitGroup
	allowed := make(chan bool, 2)
	for _, owner := range []string{"new1", "new2"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			in := input
			in.Claim = owner
			d, err := s.Decide(cfg, in)
			allowed <- err == nil && d.Allowed
		}(owner)
	}
	wg.Wait()
	close(allowed)
	for ok := range allowed {
		if ok {
			t.Fatal("competing claim replaced active reservation")
		}
	}
	input.Now = now.Add(time.Minute)
	if _, err := s.Decide(cfg, input); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path)
	if err != nil || len(reopened.Snapshot(input.Now).Reservations) != 1 {
		t.Fatal("live reservation not replayed", err)
	}
	if len(reopened.Snapshot(now.Add(time.Hour)).Reservations) != 0 {
		t.Fatal("reservation never expired")
	}
	if err := s.Append(Event{Version: SchemaVersion, ID: "rollback", At: now, Release: "*"}); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot(now).Reservations) != 0 || len(s.Snapshot(now).Intents) != 3 {
		t.Fatal("rollback dropped evidence or retained reservations")
	}
	order := s.Order([]string{c.Key, a.Key, b.Key, "o/r#99"})
	if strings.Join(order, ",") != a.Key+","+b.Key+","+c.Key+",o/r#99" {
		t.Fatal(order)
	}
}

func TestActualJobAccountingClipsAndDeduplicates(t *testing.T) {
	s := testStore(t)
	now := epoch.Add(time.Hour)
	if err := s.Append(coverageEvent(now)); err != nil {
		t.Fatal(err)
	}
	jobs := []Job{
		{Repo: "o/r", Base: "v5", ID: 1, RunID: 10, Attempt: 1, Head: strings.Repeat("a", 40), RunnerID: 9, Status: "completed", Conclusion: "cancelled",
			StartedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-30 * time.Minute), ObservedAt: now, Candidates: []string{"a", "b"}},
		{Repo: "o/r", Base: "v5", ID: 2, RunID: 11, Attempt: 1, Head: strings.Repeat("a", 40), Status: "queued", ObservedAt: now},
		{Repo: "o/r", Base: "v5", ID: 3, RunID: 12, Attempt: 1, Head: strings.Repeat("a", 40), Status: "completed", Conclusion: "skipped", ObservedAt: now},
	}
	for n, j := range jobs {
		if err := s.Append(Event{Version: SchemaVersion, ID: fmt.Sprint(n), At: now, Job: &j}); err != nil {
			t.Fatal(err)
		}
	}
	cost := s.Cost("o/r", "v5", now.Add(-time.Hour), now)
	if !cost.Complete || cost.RunnerMinutes == nil || *cost.RunnerMinutes != 30 || cost.Jobs != 1 {
		t.Fatalf("%+v", cost)
	}
	// The same physical job delivered twice is still one cost.
	j := jobs[0]
	if err := s.Append(Event{Version: SchemaVersion, ID: "duplicate-job", At: now, Job: &j}); err != nil {
		t.Fatal(err)
	}
	j.Attempt = 2
	j.StartedAt = now.Add(-10 * time.Minute)
	j.CompletedAt = now.Add(-5 * time.Minute)
	if err := s.Append(Event{Version: SchemaVersion, ID: "attempt2", At: now, Job: &j}); err != nil {
		t.Fatal(err)
	}
	cost = s.Cost("o/r", "v5", now.Add(-time.Hour), now)
	if cost.RunnerMinutes == nil || *cost.RunnerMinutes != 35 || cost.Jobs != 2 {
		t.Fatalf("%+v", cost)
	}
	j.ID = 4
	j.Status = "in_progress"
	j.CompletedAt = time.Time{}
	j.ObservedAt = now.Add(-time.Minute)
	if err := s.Append(Event{Version: SchemaVersion, ID: "unfinished", At: now, Job: &j}); err != nil {
		t.Fatal(err)
	}
	if s.Cost("o/r", "v5", now.Add(-time.Hour), now).Complete {
		t.Fatal("unfinished observation counted unknown future as zero")
	}
	if s.Cost("other", "v5", now.Add(-time.Hour), now).RunnerMinutes != nil {
		t.Fatal("missing cohort reported zero")
	}
}
