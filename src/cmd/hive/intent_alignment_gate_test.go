package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/intent"
)

func TestWriteMergeEligibleExcludesMisalignedWhenIntentEnforced(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "kubestellar/hive", Number: 1, Title: "ok", Author: "agent", CIStatus: "success", Mergeable: github.MergeableYes},
		{Repo: "kubestellar/hive", Number: 2, Title: "drift", Author: "agent", CIStatus: "success", Mergeable: github.MergeableYes},
	}}}
	verdicts := map[string]intent.Verdict{
		"kubestellar/hive/1": {AgentPR: true, Authorized: true},
		"kubestellar/hive/2": {
			AgentPR:    true,
			Authorized: true,
			Alignment:  &intent.AlignmentVerdict{Status: intent.AlignmentStatusMisaligned, Rationale: "docs intent touched proxy"},
		},
	}
	writeMergeEligible(actionable, github.HoldResult{}, "kubestellar", nil, true, verdicts, false, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	raw, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		MergeEligible []struct {
			Number int `json:"number"`
		} `json:"merge_eligible"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.MergeEligible) != 1 || payload.MergeEligible[0].Number != 1 {
		t.Fatalf("merge_eligible = %+v, want only PR #1", payload.MergeEligible)
	}
}
