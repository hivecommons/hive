package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestBlockedIssueNeverAppearsInContributorQueue(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeLabelsMode = config.FilterModeAllow
	server.deps.Config.Hub.ContributeDenyLabels = []string{"3-clanker-queue"}

	blocked := intgIssue(1, "lab-dependent work", "bob", nil)
	blocked["labels"] = []any{"3-clanker-queue", "blocked"}
	ready := intgIssue(2, "independent work", "bob", nil)
	ready["labels"] = []any{"3-clanker-queue"}
	setStatusIssues(server, blocked, ready)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 2 {
		t.Fatalf("blocked issue must stay out of ReadyQueue, got %+v", queue)
	}

	conn := &ContributorConnection{
		profile: &ContributorProfile{
			GitHubUsername: "alice",
			ContributorID:  "c-alice",
			TrustTier:      "contributor",
		},
		lastPong: time.Now(),
	}
	task := hub.selectTask(conn)
	if task == nil || task.Type != "task_assign" || task.Number != 2 {
		t.Fatalf("blocked issue must stay out of selectTask, got %+v", task)
	}

	admitted, withheld := server.ConvergenceKickProjection([]ghpkg.Issue{
		{Repo: "myorg/repo1", Number: 1, Labels: []string{"Blocked"}},
		{Repo: "myorg/repo1", Number: 2, Labels: []string{"3-clanker-queue"}},
	})
	if len(admitted) != 1 || admitted[0].Number != 2 {
		t.Fatalf("blocked issue must stay out of internal kicks, got %+v", admitted)
	}
	if len(withheld) != 1 || withheld[0].Issue.Number != 1 ||
		withheld[0].Decision.Reason != contributorAdmissionReasonWorkflowBlocked {
		t.Fatalf("blocked kick finding = %+v, want workflow_blocked for #1", withheld)
	}
}

func TestBlockedIssueAdmissionIsCaseInsensitive(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeLabelsMode = config.FilterModeAllow
	server.deps.Config.Hub.ContributeDenyLabels = []string{"3-clanker-queue"}

	blocked := intgIssue(1, "lab-dependent work", "bob", nil)
	blocked["labels"] = []any{"3-clanker-queue", "Blocked"}
	ready := intgIssue(2, "independent work", "bob", nil)
	ready["labels"] = []any{"3-clanker-queue"}
	setStatusIssues(server, blocked, ready)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 2 {
		t.Fatalf("blocked issue must stay out of ReadyQueue, got %+v", queue)
	}

	task := hub.selectTask(&ContributorConnection{
		profile: &ContributorProfile{
			GitHubUsername: "alice",
			ContributorID:  "c-alice",
			TrustTier:      "contributor",
		},
		lastPong: time.Now(),
	})
	if task == nil || task.Type != "task_assign" || task.Number != 2 {
		t.Fatalf("blocked issue must stay out of selectTask, got %+v", task)
	}
}

func TestContributeSkipLabelWithholdsAndExplainsMatchingLabel(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeLabelsMode = config.FilterModeAllow
	server.deps.Config.Hub.ContributeDenyLabels = []string{"3-clanker-queue"}
	server.deps.Config.Hub.ContributeSkipLabels = []string{"wayfinder:*"}

	skipped := intgIssue(1, "decision brief", "bob", nil)
	skipped["labels"] = []any{"3-clanker-queue", "Wayfinder:Grilling"}
	ready := intgIssue(2, "independent work", "bob", nil)
	ready["labels"] = []any{"3-clanker-queue", "bug"}
	setStatusIssues(server, skipped, ready)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 2 {
		t.Fatalf("skip-label issue must stay out of ReadyQueue, got %+v", queue)
	}

	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(), contributorAdmissionCandidate{
		repoFull: "myorg/repo1",
		repoName: "repo1",
		number:   1,
		labels:   []string{"Wayfinder:Grilling"},
	})
	if decision.admitted || decision.reason != contributorAdmissionReasonLabelSkipped || decision.skippedLabel != "Wayfinder:Grilling" {
		t.Fatalf("skip-label decision = %+v, want label_skipped with matched label", decision)
	}
	decision = hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(), contributorAdmissionCandidate{
		repoFull: "myorg/repo1",
		repoName: "repo1",
		number:   2,
		labels:   []string{"bug"},
	})
	if !decision.admitted {
		t.Fatalf("non-matching label should admit, got %+v", decision)
	}

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	item, ok := findWithheld(snap.withheld, "myorg/repo1#1")
	if !ok {
		t.Fatalf("withheld skip-label item missing: %+v", snap.withheld)
	}
	if item.Reason != contributorAdmissionReasonLabelSkipped || item.SkippedLabel != "Wayfinder:Grilling" {
		t.Fatalf("withheld item = %+v, want label_skipped with matched label", item)
	}
}

func TestConvergenceKickProjectionSkipsConfiguredLabels(t *testing.T) {
	_, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeSkipLabels = []string{"wayfinder:*"}

	admitted, withheld := server.ConvergenceKickProjection([]ghpkg.Issue{
		{Repo: "myorg/repo1", Number: 1, Labels: []string{"wayfinder:map"}},
		{Repo: "myorg/repo1", Number: 2, Labels: []string{"bug"}},
	})
	if len(admitted) != 1 || admitted[0].Number != 2 {
		t.Fatalf("skip-label issue must stay out of internal kicks, got %+v", admitted)
	}
	if len(withheld) != 1 || withheld[0].Issue.Number != 1 ||
		withheld[0].Decision.Reason != contributorAdmissionReasonLabelSkipped ||
		len(withheld[0].Decision.Blockers) != 1 || withheld[0].Decision.Blockers[0] != "wayfinder:map" {
		t.Fatalf("skip-label kick finding = %+v, want label_skipped for #1", withheld)
	}
}

func TestNeedsDecisionLabelNeverAppearsInContributorQueue(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeLabelsMode = config.FilterModeAllow
	server.deps.Config.Hub.ContributeDenyLabels = []string{"3-clanker-queue"}

	decision := intgIssue(1, "awaiting maintainer decision", "bob", nil)
	decision["labels"] = []any{"3-clanker-queue", "needs-decision"}
	ready := intgIssue(2, "independent work", "bob", nil)
	ready["labels"] = []any{"3-clanker-queue"}
	setStatusIssues(server, decision, ready)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 2 {
		t.Fatalf("needs-decision issue must stay out of ReadyQueue, got %+v", queue)
	}

	task := hub.selectTask(&ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	})
	if task == nil || task.Type != "task_assign" || task.Number != 2 {
		t.Fatalf("needs-decision issue must stay out of selectTask, got %+v", task)
	}
}
