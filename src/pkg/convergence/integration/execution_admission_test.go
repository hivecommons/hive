package integration

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func executionFixturePath(t *testing.T) string {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	directory := ".execution-fixture-" + hex.EncodeToString(nonce[:])
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	return filepath.Join(directory, "ledger.jsonl")
}

func executionAdmissionFixture(t *testing.T) (*Store, config.IntegrationExecutionConfig, ExecutionAdmissionInput) {
	t.Helper()
	store, err := OpenExecution(executionFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	minutes := 0.0
	cfg := config.IntegrationExecutionConfig{
		Mode: config.IntegrationEnforce, MaxActive: 2, MaxJobs: 10, MaxAttempts: 1,
		MaxRunnerMinutes: 100, ObservationAge: time.Minute, Window: time.Hour, Cooldown: time.Minute,
		Scopes: []config.IntegrationExecutionScope{{
			Repo: "o/r", Base: "v5", Authority: "hive", RepositoryID: 1, RequestActorID: 2, CandidateActorID: 3,
			ProducerAppID: 4, MaterializerID: 5, CandidateWorkflowID: 6, DefaultBranch: "v5",
			PromotionAppID: 7, PromotionInstallationID: 8,
			Policy: strings.Repeat("a", 64), WorkflowTree: strings.Repeat("b", 40), RequiredJobs: []string{"tests"},
			ExecutorID: strings.Repeat("c", 64), AuthorityRef: "refs/heads/hive-integration-authority/v5",
			AuthorityCommit: strings.Repeat("d", 40), QualificationID: "qualification",
			MaxMembers: 2, ExpectedJobs: 2, ExpectedMinutes: 10,
		}},
	}
	in := ExecutionAdmissionInput{
		Repo: "o/r", Base: "v5", BaseSHA: strings.Repeat("e", 40), Now: now, ExecutorID: cfg.Scopes[0].ExecutorID,
		AuthorityVerified: true, QualificationVerified: true, Cost: Cost{Complete: true, RunnerMinutes: &minutes},
		Members: []ExecutionMember{
			{PR: 1, Head: strings.Repeat("1", 40), WorkKey: "o/r#10", Eligible: true, ScopeKnown: true,
				DependenciesKnown: true, DependenciesSatisfied: true, ReadyAt: now.Add(-time.Hour), IntentGeneration: 1, PlanRevision: 1},
			{PR: 2, Head: strings.Repeat("2", 40), WorkKey: "o/r#20", Eligible: true, ScopeKnown: true,
				DependenciesKnown: true, DependenciesSatisfied: true, ReadyAt: now.Add(-time.Minute), Priority: 10000, IntentGeneration: 1, PlanRevision: 1},
		},
	}
	return store, cfg, in
}

func TestExecutionAdmissionFittingPrefixDurabilityAndNoRedispatch(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	cfg.MaxActive = 1
	decision, err := store.PlanExecution(cfg, in)
	if err != nil || !decision.Allowed || decision.Plan == nil {
		t.Fatalf("admission: %+v %v", decision, err)
	}
	if len(decision.Plan.Recipe.Members) != 1 || decision.Plan.Recipe.Members[0].PR != 1 {
		t.Fatalf("capacity should select a fitting aged prefix, not stall or accept unbounded priority: %+v", decision)
	}
	replayed, err := OpenExecution(store.path)
	if err != nil || len(replayed.ExecutionSnapshot().Plans) != 1 {
		t.Fatalf("durable plan: %v", err)
	}
	decision, err = replayed.PlanExecution(cfg, in)
	if err != nil || decision.Allowed || decision.Plan != nil {
		t.Fatalf("duplicate/reserved admission: %+v %v", decision, err)
	}
	legacy, err := Open(store.path)
	if err == nil || legacy != nil {
		t.Fatal("legacy proof store read execution partition")
	}
}

func TestExecutionAdmissionOffAndAdvisoryNeverReserveOrBlock(t *testing.T) {
	for _, mode := range []string{config.IntegrationOff, config.IntegrationAdvisory} {
		store, cfg, in := executionAdmissionFixture(t)
		cfg.Mode = mode
		decision, err := store.PlanExecution(cfg, in)
		if err != nil || !decision.Allowed || len(store.ExecutionSnapshot().Plans) != 0 {
			t.Fatalf("%s changed durable admission: %+v %v", mode, decision, err)
		}
		in.AuthorityVerified = false
		decision, err = store.PlanExecution(cfg, in)
		if err != nil || !decision.Allowed {
			t.Fatalf("%s blocked legacy: %+v %v", mode, decision, err)
		}
	}
}

func TestExecutionAdmissionRejectsMissingAuthorityEligibilityAndTelemetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ExecutionAdmissionInput)
	}{
		{"observer", func(in *ExecutionAdmissionInput) { in.ExecutorID = "another-installation" }},
		{"authority", func(in *ExecutionAdmissionInput) { in.AuthorityVerified = false }},
		{"qualification", func(in *ExecutionAdmissionInput) { in.QualificationVerified = false }},
		{"native", func(in *ExecutionAdmissionInput) { in.NativeSelected = true }},
		{"head", func(in *ExecutionAdmissionInput) { in.BaseSHA = "" }},
		{"clock", func(in *ExecutionAdmissionInput) { in.Now = time.Time{} }},
		{"cohort", func(in *ExecutionAdmissionInput) { in.Cost.Complete = false }},
		{"minutes", func(in *ExecutionAdmissionInput) { in.Cost.RunnerMinutes = nil }},
		{"eligibility", func(in *ExecutionAdmissionInput) { in.Members = nil }},
		{"scope", func(in *ExecutionAdmissionInput) {
			for i := range in.Members {
				in.Members[i].ScopeKnown = false
			}
		}},
		{"dependency", func(in *ExecutionAdmissionInput) {
			for i := range in.Members {
				in.Members[i].DependenciesSatisfied = false
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, cfg, in := executionAdmissionFixture(t)
			test.change(&in)
			decision, err := store.PlanExecution(cfg, in)
			if err != nil || decision.Allowed || !decision.WouldBlock || decision.Reason == "" {
				t.Fatalf("unsafe admission: %+v %v", decision, err)
			}
			if len(store.ExecutionSnapshot().Plans) != 0 {
				t.Fatal("blocked admission persisted a reservation")
			}
		})
	}
}

func TestExecutionAdmissionGlobalSlotsAndNativeLimits(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	cfg.MaxActive = 1
	second := cfg.Scopes[0]
	second.Repo = "o/other"
	cfg.Scopes = append(cfg.Scopes, second)
	if decision, err := store.PlanExecution(cfg, in); err != nil || decision.Plan == nil {
		t.Fatalf("first: %+v %v", decision, err)
	}

	in.Repo = second.Repo
	if decision, err := store.PlanExecution(cfg, in); err != nil || decision.Allowed {
		t.Fatalf("reused global slots: %+v %v", decision, err)
	}
	cfg.Scopes[1].Authority = "native"
	in.NativeSelected = true
	if decision, err := store.PlanExecution(cfg, in); err != nil || !decision.Allowed || decision.EffectiveWriter != "github-native" {
		t.Fatalf("native admission replaced queue authority: %+v %v", decision, err)
	}
	in.Cost.Complete = false
	if decision, _ := store.PlanExecution(cfg, in); decision.Allowed {
		t.Fatal("native unknown telemetry was treated as zero")
	}
}

func TestExecutionAdmissionInvalidUnenrolledAndNativeJobBudget(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	invalid := cfg
	invalid.Mode = "unexpected"
	if _, err := store.PlanExecution(invalid, in); err == nil {
		t.Fatal("invalid configuration authorized execution")
	}
	outside := in
	outside.Repo = "o/unlisted"
	if decision, err := store.PlanExecution(cfg, outside); err != nil || !decision.Allowed || decision.EffectiveWriter != "legacy" {
		t.Fatalf("unenrolled destination changed behavior: %+v %v", decision, err)
	}
	cfg.Scopes[0].Authority = "native"
	cfg.MaxJobs = 2
	in.Cost.Jobs, in.Cost.Running = 1, 1
	in.NativeSelected = true
	if decision, err := store.PlanExecution(cfg, in); err != nil || decision.Allowed {
		t.Fatalf("native Hive admission exceeded assigned-job bound: %+v %v", decision, err)
	}
}
