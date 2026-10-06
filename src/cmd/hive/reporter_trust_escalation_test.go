package main

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestReporterTrustHeldRelayPRIsEscalatedWithGreenCI(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	actionable := &github.ActionableResult{}
	actionable.PRs.Held = []github.PullRequest{{Repo: "acme/widgets", Number: 42, Author: "relay-contributor", Labels: []string{"hold", "needs-human"}, CIStatus: "success", ReporterTrustReason: "reporter-trust hold — issue #581 filed by @stranger"}}
	got := runEscalationSweep(context.Background(), escalationTestConfig(), client, actionable, nil, nil, discardLogger())
	if !got["acme/widgets#42"] {
		t.Fatalf("held relay PR missing from escalation: %v", got)
	}
	if len(fake.paths) != 0 {
		t.Fatalf("green policy hold triggered CI recovery writes: %v", fake.paths)
	}
}
