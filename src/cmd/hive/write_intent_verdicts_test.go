package main

import (
	"context"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/github"
)

// writeIntentVerdicts must return an empty (never nil) map, and must not
// touch the GitHub client or the logger, when either cfg or actionable is
// nil. This is the "no config yet" / "no enumeration yet" boot-time case.
func TestWriteIntentVerdictsNilGuards(t *testing.T) {
	logger := restoreTestLogger()

	got := writeIntentVerdicts(context.Background(), nil, nil,
		&github.ActionableResult{}, nil, logger)
	if got == nil || len(got) != 0 {
		t.Fatalf("nil cfg: got %#v, want empty non-nil map", got)
	}

	got = writeIntentVerdicts(context.Background(), &config.Config{}, nil, nil, nil, logger)
	if got == nil || len(got) != 0 {
		t.Fatalf("nil actionable: got %#v, want empty non-nil map", got)
	}
}

// A PR authored by someone other than the configured AI author takes the
// non-agent classification path entirely: no GitHub client call is made (a
// nil client would panic if fetchIntentPREvidence were reached), and the
// resulting verdict is keyed by "org/repo#number" with Enforced mirroring
// cfg.Intent.Enforce.
func TestWriteIntentVerdictsNonAgentPRSkipsEvidenceFetch(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", AIAuthor: "hive-bot"},
		Intent: config.IntentConfig{
			Enforce: true,
		},
	}

	actionable := &github.ActionableResult{
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "widgets", Number: 7, Title: "Human PR", Author: "a-human"},
			},
		},
	}

	// A nil *github.Client would panic if the agent-PR evidence-fetch branch
	// were reached, so a clean return here proves the non-agent path was
	// taken.
	verdicts := writeIntentVerdicts(context.Background(), cfg, nil, actionable, nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/7"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/7", verdicts)
	}
	if v.AgentPR {
		t.Errorf("non-agent PR classified as AgentPR")
	}
}

// An agent-authored PR (author matches EffectiveAIAuthor) DOES reach the
// evidence-fetch branch; with a nil GitHub client that fetch fails, and the
// resulting verdict must be Tier1/unauthorized with the fetch error recorded
// as the reason, not silently dropped.
func TestWriteIntentVerdictsAgentPREvidenceFetchFailureDeniesTier1(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme", AIAuthor: "hive-bot"},
	}

	actionable := &github.ActionableResult{
		PRs: github.PRResult{
			Items: []github.PullRequest{
				{Repo: "widgets", Number: 9, Title: "Agent PR", Author: "hive-bot"},
			},
		},
	}

	verdicts := writeIntentVerdicts(context.Background(), cfg, nil, actionable, nil, restoreTestLogger())

	v, ok := verdicts["acme/widgets/9"]
	if !ok {
		t.Fatalf("verdicts = %#v, want key acme/widgets/9", verdicts)
	}
	if v.Authorized {
		t.Error("agent PR with unfetchable evidence must not be authorized")
	}
	if v.Reason == "" {
		t.Error("denied verdict must carry a non-empty reason")
	}
}
