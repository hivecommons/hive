package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
)

// fakeIssueWriter is a non-GitHub forge.IssueWriter recording the two writes
// the escalation sweep performs.
type fakeIssueWriter struct {
	comments []string // "repo#number: body"
	labels   []string // "repo#number: label,label"
}

func (f *fakeIssueWriter) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	f.comments = append(f.comments, escalation.Key(repo, number)+": "+body)
	return nil
}

func (f *fakeIssueWriter) AddLabels(_ context.Context, repo string, number int, labels []string) error {
	f.labels = append(f.labels, escalation.Key(repo, number)+": "+strings.Join(labels, ","))
	return nil
}

var _ forge.IssueWriter = (*fakeIssueWriter)(nil)

// TestRunEscalationSweepWritesThroughANonGitHubForge is the thesis of
// kubestellar/hive#5259 in one test: the sweep's escalation actions land on
// whatever forge the hive is configured for, not on GitHub specifically.
//
// The sibling tests in escalation_sweep_test.go pin the same behavior against a
// fake GitHub API and still pass unchanged, which is the other half of the
// claim — a GitHub hive's path did not move. This one pins that a GitLab or
// Gitea hive, whose writer is a pkg/forge adapter rather than *github.Client,
// gets the same two writes.
func TestRunEscalationSweepWritesThroughANonGitHubForge(t *testing.T) {
	newTestEscalationStore(t)
	cfg := escalationTestConfig()
	cfg.Escalation.Threshold = 2
	w := &fakeIssueWriter{}
	ctx := context.Background()
	prKey := escalation.Key("acme/widgets", 7)

	// One red SHA is below the threshold: nothing is written to the forge.
	if got := runEscalationSweep(ctx, cfg, w,
		actionableWith(redPR("widgets", 7, "hive-agent", "sha-1")), nil, nil, discardLogger()); len(got) != 0 {
		t.Fatalf("escalated = %v, want empty below the threshold", got)
	}
	if len(w.comments) != 0 || len(w.labels) != 0 {
		t.Fatalf("below threshold the sweep must not write: comments=%v labels=%v", w.comments, w.labels)
	}

	// A second distinct red SHA crosses it: evidence comment, then the label.
	got := runEscalationSweep(ctx, cfg, w,
		actionableWith(redPR("widgets", 7, "hive-agent", "sha-2")), nil, nil, discardLogger())
	if !got[prKey] {
		t.Fatalf("escalated = %v, want %s marked escalated", got, prKey)
	}
	if len(w.comments) != 1 || !strings.HasPrefix(w.comments[0], prKey+": ") {
		t.Fatalf("comments = %v, want one evidence comment on %s", w.comments, prKey)
	}
	if want := prKey + ": " + escalation.NeedsHumanLabel; len(w.labels) != 1 || w.labels[0] != want {
		t.Fatalf("labels = %v, want [%q]", w.labels, want)
	}
}
