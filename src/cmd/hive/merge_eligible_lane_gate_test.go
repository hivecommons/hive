package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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

// A PR that passes every merge gate but whose merge relay is refused for a
// reason that needs a maintainer (trust or admission) is merged by no Hive
// lane. In a repo whose auto-merge is on its verdict is a hand merge naming
// the refusal, so the dashboard asks a person; before, only the scheduler's
// needs_maintainer_merge list carried it. The lane switched off is a hand
// merge the same way, without a needs_maintainer_merge row. A temporary
// refusal, an auto-merge-off repo and a PR with no refusal keep today's
// verdicts, and the buckets and needs_maintainer_merge are the same either
// way.
func TestWriteMergeEligible_TrustRefusalIsHandMergeWhenAutoMergeOn(t *testing.T) {
	trust := automerge.RelayMergeRefusalPrefix + " for contributor's PR: " + prstage.ContributorLaneReasonNoClaim + " (details)"
	soak := automerge.RelayMergeRefusalPrefix + ": " + prstage.ContributorLaneReasonSoaking + " (10m remaining)"
	disabled := automerge.RelayMergeRefusalPrefix + " for contributor's PR: " + automerge.RelayLaneReasonDisabled + " (details)"
	pr := func(n int) github.PullRequest {
		return github.PullRequest{Repo: "demo", Number: n, Author: "contributor", Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", HeadSHA: "abc"}
	}
	trusted, soaking, clean, laneOff := pr(1), pr(2), pr(3), pr(4)

	run := func(t *testing.T, level int) (map[string]github.MergeVerdict, []int, []int) {
		t.Helper()
		dir := t.TempDir()
		origMerge, origFail := mergeEligiblePath, ciFailingPath
		mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
		ciFailingPath = filepath.Join(dir, "ci-failing.json")
		prevGate := mergeEligibleLaneGate
		t.Cleanup(func() {
			mergeEligiblePath = origMerge
			ciFailingPath = origFail
			setMergeEligibleLaneGate(prevGate)
		})
		setMergeEligibleLaneGate(func(_ context.Context, _ string, number int, _ string) (string, error) {
			switch number {
			case trusted.Number:
				return trust, nil
			case soaking.Number:
				return soak, nil
			case laneOff.Number:
				return disabled, nil
			}
			return "", nil
		})
		cfg := &config.Config{ACMMLevel: &level, Project: config.ProjectConfig{Org: "org"}}
		actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{trusted, soaking, clean, laneOff}}}
		verdicts := writeMergeEligible(actionable, github.HoldResult{}, "org", nil, false, nil, false, nil, nil, cfg, discardLogger())
		data, err := os.ReadFile(mergeEligiblePath)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Eligible []struct {
				Number int `json:"number"`
			} `json:"merge_eligible"`
			NeedsMaintainer []struct {
				Number int    `json:"number"`
				Reason string `json:"reason"`
			} `json:"needs_maintainer_merge"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		var eligible, maintainer []int
		for _, row := range out.Eligible {
			eligible = append(eligible, row.Number)
		}
		for _, row := range out.NeedsMaintainer {
			maintainer = append(maintainer, row.Number)
			if row.Reason != trust {
				t.Errorf("needs_maintainer_merge reason = %q, want the bare refusal %q", row.Reason, trust)
			}
		}
		return verdicts, eligible, maintainer
	}
	checkBuckets := func(t *testing.T, eligible, maintainer []int) {
		t.Helper()
		if len(eligible) != 1 || eligible[0] != clean.Number {
			t.Errorf("merge_eligible = %v, want only #%d", eligible, clean.Number)
		}
		if len(maintainer) != 1 || maintainer[0] != trusted.Number {
			t.Errorf("needs_maintainer_merge = %v, want only #%d", maintainer, trusted.Number)
		}
	}

	t.Run("auto-merge on", func(t *testing.T) {
		verdicts, eligible, maintainer := run(t, config.SelfMergeMinACMMLevel)
		checkBuckets(t, eligible, maintainer)
		want := github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: trust + "; " + relayRefusalHandMergeReason, HandMerge: true}
		if got := verdicts[github.MergeVerdictKey(trusted)]; got != want {
			t.Errorf("trust refusal verdict = %+v, want %+v", got, want)
		}
		// The lane switched off is a hand merge too, though not a
		// needs_maintainer_merge row (checkBuckets).
		if got, want := verdicts[github.MergeVerdictKey(laneOff)], (github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: disabled + "; " + relayRefusalHandMergeReason, HandMerge: true}); got != want {
			t.Errorf("lane disabled verdict = %+v, want %+v", got, want)
		}
		if got, want := verdicts[github.MergeVerdictKey(soaking)], (github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: soak}); got != want {
			t.Errorf("soak refusal verdict = %+v, want unchanged %+v", got, want)
		}
		if got := verdicts[github.MergeVerdictKey(clean)]; got.State != github.MergeVerdictEligible || got.HandMerge {
			t.Errorf("no refusal verdict = %+v, want eligible and not a hand merge", got)
		}
	})

	t.Run("auto-merge off", func(t *testing.T) {
		verdicts, eligible, maintainer := run(t, config.SelfMergeMinACMMLevel-1)
		checkBuckets(t, eligible, maintainer)
		for _, tc := range []struct {
			pr   github.PullRequest
			want string
		}{
			{trusted, trust + "; " + repoAutoMergeOffReason},
			{soaking, soak + "; " + repoAutoMergeOffReason},
			{laneOff, disabled + "; " + repoAutoMergeOffReason},
			{clean, repoAutoMergeOffReason},
		} {
			got := verdicts[github.MergeVerdictKey(tc.pr)]
			want := github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: tc.want, HandMerge: true}
			if got != want {
				t.Errorf("#%d verdict = %+v, want unchanged %+v", tc.pr.Number, got, want)
			}
			if strings.Contains(got.Reason, relayRefusalHandMergeReason) {
				t.Errorf("#%d reason %q carries the auto-merge-on tail in an off repo", tc.pr.Number, got.Reason)
			}
		}
	})
}

func TestRelayRefusalVerdict(t *testing.T) {
	refusal := func(reason, detail string) string {
		return automerge.RelayMergeRefusalPrefix + " for contributor's PR: " + reason + " (" + detail + ")"
	}
	trust := refusal(prstage.ContributorLaneReasonAuthorAssociation, "details")
	disabled := refusal(automerge.RelayLaneReasonDisabled, "auto_merge.contributor_lane.enabled is false; a person must merge contributor PRs")
	repoNotAllowed := refusal(automerge.RelayLaneReasonRepoNotAllowed, "this repo is outside auto_merge.contributor_lane.repos")
	soak := refusal(prstage.ContributorLaneReasonSoaking, "10m remaining")
	noVerdict := refusal(prstage.ContributorLaneReasonNoVerdict, "details")
	headAge := refusal("pending: head pushed 1m ago", "auto_merge.min_head_age")
	workflowPath := refusal(prstage.ContributorLaneReasonWorkflowPath, "author tier \"trusted\", require_tiers merger, trusted")
	testsRemoved := refusal(prstage.ContributorLaneReasonTestsRemoved, "details")
	outstanding := func(reason string) github.MergeVerdict {
		return github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: reason}
	}
	hand := func(reason string) github.MergeVerdict {
		return github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: reason, HandMerge: true}
	}
	for _, tc := range []struct {
		name      string
		refusal   string
		off       bool
		want      github.MergeVerdict
		wantFinal github.MergeVerdict
	}{
		{"trust refusal, auto-merge on", trust, false, hand(trust + "; " + relayRefusalHandMergeReason), hand(trust + "; " + relayRefusalHandMergeReason)},
		{"lane disabled, auto-merge on", disabled, false, hand(disabled + "; " + relayRefusalHandMergeReason), hand(disabled + "; " + relayRefusalHandMergeReason)},
		{"repo not allowed, auto-merge on", repoNotAllowed, false, hand(repoNotAllowed + "; " + relayRefusalHandMergeReason), hand(repoNotAllowed + "; " + relayRefusalHandMergeReason)},
		{"workflow-path fence, auto-merge on", workflowPath, false, hand(workflowPath + "; " + relayRefusalHandMergeReason), hand(workflowPath + "; " + relayRefusalHandMergeReason)},
		{"tests-removed fence, auto-merge on", testsRemoved, false, hand(testsRemoved + "; " + relayRefusalHandMergeReason), hand(testsRemoved + "; " + relayRefusalHandMergeReason)},
		{"soak refusal, auto-merge on", soak, false, outstanding(soak), outstanding(soak)},
		{"no reviewer verdict, auto-merge on", noVerdict, false, outstanding(noVerdict), outstanding(noVerdict)},
		{"head too new, auto-merge on", headAge, false, outstanding(headAge), outstanding(headAge)},
		// Auto-merge off: relayRefusalVerdict leaves the verdict alone and
		// handMergeVerdict adds the switch, exactly as before.
		{"trust refusal, auto-merge off", trust, true, outstanding(trust), hand(trust + "; " + repoAutoMergeOffReason)},
		{"lane disabled, auto-merge off", disabled, true, outstanding(disabled), hand(disabled + "; " + repoAutoMergeOffReason)},
		{"repo not allowed, auto-merge off", repoNotAllowed, true, outstanding(repoNotAllowed), hand(repoNotAllowed + "; " + repoAutoMergeOffReason)},
		{"workflow-path fence, auto-merge off", workflowPath, true, outstanding(workflowPath), hand(workflowPath + "; " + repoAutoMergeOffReason)},
		{"soak refusal, auto-merge off", soak, true, outstanding(soak), hand(soak + "; " + repoAutoMergeOffReason)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := relayRefusalVerdict(tc.refusal, tc.off)
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			// What the classifier writes after handMergeVerdict: at most one
			// hand-merge tail, whichever applies.
			final := handMergeVerdict(got, tc.off)
			if final != tc.wantFinal {
				t.Errorf("after handMergeVerdict got %+v, want %+v", final, tc.wantFinal)
			}
			tails := strings.Count(final.Reason, relayRefusalHandMergeReason) + strings.Count(final.Reason, repoAutoMergeOffReason)
			if final.HandMerge && tails != 1 {
				t.Errorf("final reason %q carries %d hand-merge tails, want exactly one", final.Reason, tails)
			}
		})
	}
}

// needs_maintainer_merge stays trust-only: the lane switched off or scoped
// to other repos, or a workflow-file or tests-removed fence, is a hand merge
// on the dashboard, but not a maintainer-merge row for the scheduler.
func TestRelayRefusalNoLaneMerges(t *testing.T) {
	for _, tc := range []struct {
		reason         string
		noLane, trusts bool
	}{
		{prstage.ContributorLaneReasonNoClaim, true, true},
		{prstage.ContributorLaneReasonNoLinkedIssue, true, true},
		{prstage.ContributorLaneReasonAuthorAssociation, true, true},
		{automerge.RelayLaneReasonDisabled, true, false},
		{automerge.RelayLaneReasonRepoNotAllowed, true, false},
		{prstage.ContributorLaneReasonWorkflowPath, true, false},
		{prstage.ContributorLaneReasonTestsRemoved, true, false},
		{prstage.ContributorLaneReasonSoaking, false, false},
		{prstage.ContributorLaneReasonNoVerdict, false, false},
		{"pending: head pushed 1m ago", false, false},
	} {
		refusal := automerge.RelayMergeRefusalPrefix + ": " + tc.reason + " (details)"
		if got := relayRefusalNoLaneMerges(refusal); got != tc.noLane {
			t.Errorf("relayRefusalNoLaneMerges(%s) = %v, want %v", tc.reason, got, tc.noLane)
		}
		if got := relayRefusalNeedsMaintainer(refusal); got != tc.trusts {
			t.Errorf("relayRefusalNeedsMaintainer(%s) = %v, want %v", tc.reason, got, tc.trusts)
		}
	}
	if relayRefusalNoLaneMerges(automerge.RelayLaneReasonDisabled) {
		t.Error("a bare reason without the relay prefix is not a relay refusal")
	}
}
