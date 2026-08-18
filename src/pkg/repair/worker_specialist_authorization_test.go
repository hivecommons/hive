package repair

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/automation"
	"github.com/kubestellar/hive/pkg/visualhive"
)

func TestWorkerAuthorizationUsesExactConfiguredSpecialist(t *testing.T) {
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: strings.Repeat("a", 64), OwningAgentHint: "quality",
	}
	lifecycle := &fakeLifecycle{}
	worker := Worker{Config: Config{
		Agent: "scanner",
		Policy: automation.Policy{
			ACMMLevel: 4, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"},
		},
	}, Lifecycle: lifecycle}

	err := worker.authorize(finding, automation.ActionRepairModel, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "scanner") {
		t.Fatalf("scanner repair authorization = %v, want exact-role denial", err)
	}
	if len(lifecycle.decisions) != 1 || lifecycle.decisions[0] != "repair_model:false" {
		t.Fatalf("authorization audit = %v", lifecycle.decisions)
	}
}

func TestWorkerUsesControllerBoundAttemptStart(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 7, 16, 12, 30, 0, 123000000, time.UTC)
	provider := &fakeProvider{}
	worker := Worker{
		Config: Config{
			RepositoryDir: repository, WorktreeRoot: t.TempDir(), BaseBranch: "main", ExpectedRemoteURL: remote, Agent: "quality",
			Policy:             automation.Policy{ACMMLevel: 5, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"}, MaxRepairAttempts: 3},
			AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}},
			AttemptStartedAt: anchor, ModelTimeout: time.Minute, CommandTimeout: time.Minute,
		},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: strings.Repeat("d", 64), Fingerprint: "bound-start",
		Status: visualhive.StatusIssueOpen, Title: "repair", Body: "repair", IssueKind: "functional", Severity: "medium", OwningAgentHint: "quality",
		IssueNumber: 1, IssueURL: "https://example.test/issues/1",
	}
	if _, err := worker.Run(context.Background(), finding); err != nil {
		t.Fatal(err)
	}
	attempt, ok := state.Get(finding.RepositoryFingerprint)
	if !ok || !attempt.StartedAt.Equal(anchor) {
		t.Fatalf("persisted attempt start = %s, want %s", attempt.StartedAt, anchor)
	}
}

func TestWorkerAuthorizationDoesNotInferRoleWhenConfigured(t *testing.T) {
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryFingerprint: strings.Repeat("b", 64), OwningAgentHint: "scanner",
	}
	lifecycle := &fakeLifecycle{}
	worker := Worker{Config: Config{
		Agent: "ci-maintainer",
		Policy: automation.Policy{
			ACMMLevel: 4, Mode: automation.ModeRepairPR, AllowedRepositories: []string{"owner/repo"},
		},
	}, Lifecycle: lifecycle}

	if err := worker.authorize(finding, automation.ActionRepairModel, nil, 1); err != nil {
		t.Fatalf("ci-maintainer repair authorization = %v", err)
	}
	if len(lifecycle.decisions) != 1 || lifecycle.decisions[0] != "repair_model:true" {
		t.Fatalf("authorization audit = %v", lifecycle.decisions)
	}
}

func TestWorkerRejectsUnknownConfiguredSpecialist(t *testing.T) {
	state, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := Worker{Config: Config{
		RepositoryDir: t.TempDir(), WorktreeRoot: t.TempDir(), BaseBranch: "main", Agent: "generic-agent",
	}, Provider: &fakeProvider{}, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{}}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "1", RepositoryFingerprint: strings.Repeat("c", 64),
		IssueNumber: 1, IssueURL: "https://example.test/issues/1",
	}
	if err = worker.validate(finding); err == nil || !strings.Contains(err.Error(), "persistent specialist") {
		t.Fatalf("unknown specialist validation = %v", err)
	}
}
