package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// A PR the merge relay's contributor-lane gate refuses (a newcomer-tier
// author) must not be listed merge-eligible: hive-merge on it is refused
// every kick.
func TestWriteMergeEligible_ExcludesPRsTheRelayLaneRefuses(t *testing.T) {
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	dir := t.TempDir()
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
		setMergeEligibleLaneGate(nil)
	})
	setMergeEligibleLaneGate(func(_ context.Context, _ string, number int, _ string) (string, error) {
		if number == 15086 {
			return "contributor-lane policy refuses the merge relay for @Karibhou's PR: contributor-lane:claim-tier-untrusted (author tier \"newcomer\", require_tiers merger, trusted)", nil
		}
		return "", nil
	})

	green := func(number int, author string) github.PullRequest {
		return github.PullRequest{Repo: "hive", Number: number, Author: author, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "cafe"}
	}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		green(15086, "Karibhou"),
		green(15087, "trusted-human"),
	}}}
	verdicts := writeMergeEligible(actionable, github.HoldResult{}, "hivecommons", nil, false, nil, false, nil, nil, nil, discardLogger())

	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Eligible []struct {
			Number int `json:"number"`
		} `json:"merge_eligible"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Eligible) != 1 || out.Eligible[0].Number != 15087 {
		t.Fatalf("merge_eligible = %+v, want only #15087", out.Eligible)
	}
	if v := verdicts[github.MergeVerdictKey(actionable.PRs.Items[0])]; v.State == github.MergeVerdictEligible {
		t.Fatalf("refused PR verdict = %+v, want not eligible", v)
	}
}
