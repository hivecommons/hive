package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
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
	called := make(map[int]bool)
	refusal := automerge.RelayMergeRefusalPrefix + " for @contributor's PR: " + prstage.ContributorLaneReasonNoClaim + ` (author tier "none", require_tiers merger, trusted)`
	setMergeEligibleLaneGate(func(_ context.Context, repo string, number int, sha string) (string, error) {
		called[number] = true
		if repo != "hivecommons/hive" || sha != "cafe" {
			t.Errorf("gate target = %s#%d@%s, want hivecommons/hive at cafe", repo, number, sha)
		}
		if number == 15086 || number == 15088 || number == 15089 {
			return refusal, nil
		}
		if number == 15091 {
			return automerge.RelayMergeRefusalPrefix + ": " + prstage.ContributorLaneReasonSoaking + " (10m remaining)", nil
		}
		return "", nil
	})

	green := func(number int, author string) github.PullRequest {
		return github.PullRequest{Repo: "hive", Number: number, Author: author, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "cafe"}
	}
	attributed := green(15088, "contributor")
	attributed.HiveAttributed = true
	agentAttributed := green(15089, "contributor")
	agentAttributed.HiveAgent = "scanner"
	app := green(15090, "hive[bot]")
	app.AppAuthored = true
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		green(15086, "Karibhou"),
		green(15087, "trusted-human"),
		attributed,
		agentAttributed,
		app,
		green(15091, "soaking-contributor"),
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
		NeedsMaintainer []struct {
			Number  int    `json:"number"`
			Repo    string `json:"repo"`
			HeadSHA string `json:"head_sha"`
			Reason  string `json:"reason"`
		} `json:"needs_maintainer_merge"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Eligible) != 2 || out.Eligible[0].Number != 15087 || out.Eligible[1].Number != 15090 {
		t.Fatalf("merge_eligible = %+v, want trusted author #15087 and App #15090", out.Eligible)
	}
	if len(out.NeedsMaintainer) != 3 {
		t.Fatalf("needs_maintainer_merge = %+v, want the three trust-refused PRs", out.NeedsMaintainer)
	}
	for i, number := range []int{15086, 15088, 15089} {
		row := out.NeedsMaintainer[i]
		if row.Number != number || row.Repo != "hivecommons/hive" || row.HeadSHA != "cafe" || row.Reason != refusal {
			t.Errorf("maintainer row = %+v, want #%d with pinned head and relay refusal", row, number)
		}
	}
	for _, pr := range actionable.PRs.Items {
		if !called[pr.Number] {
			t.Errorf("PR #%d bypassed the authoritative relay gate", pr.Number)
		}
		v := verdicts[github.MergeVerdictKey(pr)]
		if pr.Number != 15087 && pr.Number != 15090 && (v.State == github.MergeVerdictEligible || !strings.Contains(v.Reason, automerge.RelayMergeRefusalPrefix)) {
			t.Errorf("refused PR #%d verdict = %+v, want refusal", pr.Number, v)
		}
	}
}

func TestRelayLaneRefusalKeepsUnavailableEvidenceForRelayRecheck(t *testing.T) {
	t.Cleanup(func() { setMergeEligibleLaneGate(nil) })
	setMergeEligibleLaneGate(func(context.Context, string, int, string) (string, error) {
		return "", errors.New("evidence unavailable")
	})
	if got := relayLaneRefusal(github.PullRequest{Number: 1}, "hivecommons/hive", discardLogger()); got != "" {
		t.Fatalf("unreadable gate refusal = %q, want existing relay-recheck behavior", got)
	}
}

func TestRelayRefusalNeedsMaintainerOnlyForTrustAdmission(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   bool
	}{
		{prstage.ContributorLaneReasonNoClaim, true},
		{prstage.ContributorLaneReasonClaimTier, true},
		{prstage.ContributorLaneReasonAuthorAssociation, true},
		{prstage.ContributorLaneReasonSoaking, false},
		{prstage.ContributorLaneReasonNoVerdict, false},
		{"pending: head pushed 1m ago", false},
	} {
		refusal := automerge.RelayMergeRefusalPrefix + ": " + tc.reason + " (details)"
		if got := relayRefusalNeedsMaintainer(refusal); got != tc.want {
			t.Errorf("maintainer classification for %s = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
