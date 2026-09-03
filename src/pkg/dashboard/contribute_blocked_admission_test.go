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
