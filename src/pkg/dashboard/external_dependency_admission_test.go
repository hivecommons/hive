package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

func externalItemWithDependency(id, blocker string, resolved bool) map[string]any {
	item := externalItem("linear", id, "external work")
	item["depends_on"] = []any{map[string]any{"key": blocker, "resolved": resolved}}
	return item
}

func TestLinearDependencyAdmissionTracksSourceState(t *testing.T) {
	blocked := newQueueServer(t, statusWith("acme/repo",
		externalItemWithDependency("ENG-2", "acme/repo!ENG-1", false),
		externalItem("linear", "ENG-3", "unrelated"),
	))
	if got := wsKeysOf(blocked.contributeHub.ReadyQueue(readyQueueDefaultLimit)); len(got) != 1 || got[0] != "acme/repo!ENG-3" {
		t.Fatalf("open Linear blocker must withhold only its dependent, got %v", got)
	}
	assigned := blocked.contributeHub.selectTask(depTestConn())
	if assigned == nil || assigned.Type != "task_assign" || assigned.ExternalID != "ENG-3" {
		t.Fatalf("assignment must skip blocked ENG-2 and select unrelated ENG-3: %+v", assigned)
	}

	// Mutate the authoritative source snapshot in the same running server. The
	// next sweep must reverse the decision without a restart or queue surgery.
	blocked.statusMu.Lock()
	blocked.status = statusWith("acme/repo",
		externalItemWithDependency("ENG-2", "acme/repo!ENG-1", true),
	)
	blocked.statusMu.Unlock()
	if got := wsKeysOf(blocked.contributeHub.ReadyQueue(readyQueueDefaultLimit)); len(got) != 1 || got[0] != "acme/repo!ENG-2" {
		t.Fatalf("resolved Linear blocker must admit its dependent, got %v", got)
	}
}

func TestLinearDependencyAdmissionAlsoGatesInternalKicks(t *testing.T) {
	s := newQueueServer(t, statusWith("acme/repo"))
	issues := []ghpkg.Issue{
		{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-2", DependsOn: []ghpkg.IssueDependency{{Key: "acme/repo!ENG-1"}}},
		{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-3"},
	}
	admitted, withheld := s.ConvergenceKickProjection(issues)
	if len(admitted) != 1 || admitted[0].ExternalID != "ENG-3" {
		t.Fatalf("internal kick projection admitted %+v, want only ENG-3", admitted)
	}
	if len(withheld) != 1 || withheld[0].Issue.ExternalID != "ENG-2" || withheld[0].Decision.Reason != convergence.ReasonWaitingForDependency {
		t.Fatalf("internal kick projection withheld %+v, want blocked ENG-2", withheld)
	}
}

func TestExternalDependencyUsesSourceAwareConvergenceSubject(t *testing.T) {
	candidate := contributorAdmissionCandidate{
		ref:       worksource.Ref{SourceType: "linear", Repo: "acme/repo", ExternalID: "ENG-2"},
		dependsOn: []ghpkg.IssueDependency{{Key: "acme/repo!ENG-1"}},
	}
	observation := observeExternalDependencies(candidate)
	decision := convergence.Evaluate(observation)
	if decision.Admitted || observation.Subject.Key() != "acme/repo!ENG-2" {
		t.Fatalf("observation = %+v, decision = %+v; want blocked source-aware subject", observation, decision)
	}
	if len(decision.Blockers) != 1 || decision.Blockers[0] != "acme/repo!ENG-1" {
		t.Fatalf("blockers = %v, want canonical external key", decision.Blockers)
	}
}

func TestGitHubPlanningRefAndDependencyGateRemainUnchanged(t *testing.T) {
	issue := ghpkg.Issue{Repo: "acme/repo", Number: 42, SourceType: "github", ExternalID: "42"}
	if got := planning.IssueRef(issue); got != "gh-acme/repo#42" {
		t.Fatalf("planning.IssueRef changed to %q", got)
	}
	bead := &beads.Bead{ExternalRef: planning.IssueRef(issue)}
	keys := beadIdentityKeys(bead)
	if len(keys) != 1 || keys[0] != "acme/repo#42" {
		t.Fatalf("gh- ExternalRef no longer maps to the legacy identity: %v", keys)
	}

	store := depTestStore(t)
	seedDependentBead(t, store, planning.IssueRef(issue))
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(), contributorAdmissionCandidate{
		repoFull: "acme/repo", repoName: "repo", number: 42,
		ref: worksource.Ref{SourceType: "github", Repo: "acme/repo", ExternalID: "42", Number: 42},
	})
	if decision.admitted || decision.reason != contributorAdmissionReasonDependencyBlocked {
		t.Fatalf("GitHub bead dependency gate changed: %+v", decision)
	}
}
