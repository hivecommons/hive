package dashboard

import (
	"strings"
	"testing"
	"time"
)

// An issue an owner filed moments ago is withheld from the contributor lane
// (selectTask and the Ready queue share the admission), and offered once the
// hold clears (hivecommons/hive#18898).
func TestOwnerGraceWithholdsFreshOwnerIssueFromContributors(t *testing.T) {
	hub, server := covK2Hub(t)
	var holdReasons = map[int]string{1: "owner grace: filed by andy 2m ago; contributor claims wait 15m unless a person adds `help wanted`"}
	var seenCreated []time.Time
	server.deps.ContributorHold = func(repo string, number int, createdAt time.Time) string {
		seenCreated = append(seenCreated, createdAt)
		return holdReasons[number]
	}

	created := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	fresh := intgIssue(1, "owner filed", "andy", nil)
	fresh["created_at"] = created
	other := intgIssue(2, "someone else's", "stranger", nil)
	setStatusIssues(server, fresh, other)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 2 {
		t.Fatalf("owner-filed issue inside the grace must stay out of the ReadyQueue, got %+v", queue)
	}
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	task := hub.selectTask(conn)
	if task == nil || task.Type != "task_assign" || task.Number != 2 {
		t.Fatalf("selectTask must skip the grace-held issue, got %+v", task)
	}
	sawCreated := false
	for _, c := range seenCreated {
		if !c.IsZero() && c.Format(time.RFC3339) == created {
			sawCreated = true
		}
	}
	if !sawCreated {
		t.Fatalf("the hook must receive the issue's created_at; saw %v", seenCreated)
	}

	// The hold lapses (grace elapsed or a person added help wanted): offered.
	holdReasons = map[int]string{}
	queue = hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 2 {
		t.Fatalf("after the hold clears both issues are offered, got %+v", queue)
	}
}

func TestOwnerGraceWithheldRowCarriesTheReason(t *testing.T) {
	d := contributorAdmissionDecision{reason: contributorAdmissionReasonOwnerGrace, detail: "owner grace: filed by andy 2m ago"}
	item := withheldFromAdmissionDecision(withheldCandidate{repoFull: "o/r"}, d)
	if item.Reason != contributorAdmissionReasonOwnerGrace || !strings.HasPrefix(item.Detail, "owner grace: ") {
		t.Fatalf("withheld item = %+v, want owner_grace with the forge's sentence", item)
	}
}
