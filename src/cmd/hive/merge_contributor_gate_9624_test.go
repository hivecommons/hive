package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// A review-swarm verdict is advisory AI output and must never be what lets
// the hive merge a person's PR (hivecommons/hive#9624). These tests pin the
// contributor rule in the one merge-eligibility classifier, and pin that the
// hive's own PRs and trusted-bot PRs are classified exactly as before.

const (
	testHiveAIAuthor     = "hive-ai"
	testTrustedBot       = "dependabot[bot]"
	testContributor      = "outside-dev"
	testMaintainer       = "maintainer-1"
	testContributorHead  = "sha-head"
	testContributorRepo  = "org/repo"
	testContributorPRNum = 42
)

// swarmApproved is a review artifact carrying an aggregate approve for
// testContributorRepo#testContributorPRNum at testContributorHead.
func swarmApproved() review.Artifact {
	return review.Artifact{GeneratedAt: time.Now(), Items: []review.Aggregate{{
		Repo: testContributorRepo, Number: testContributorPRNum, HeadSHA: testContributorHead,
		Verdict: review.VerdictApprove, MergeEligible: true,
	}}}
}

func swarmGates(authors *mergeAuthorPolicy) mergeGates {
	return mergeGates{
		requireReviewApproval: true,
		reviewArtifact:        swarmApproved(),
		reviewLoaded:          true,
		authors:               authors,
	}
}

func testAuthorPolicy(optIn bool) *mergeAuthorPolicy {
	return &mergeAuthorPolicy{
		identity:            github.HiveIdentity{AIAuthor: testHiveAIAuthor, AppLogin: "hive-app[bot]"},
		trustedBots:         map[string]bool{testTrustedBot: true},
		allowContributorPRs: optIn,
	}
}

// greenPR is green, clean, not held, at testContributorHead.
func greenPR(author string) github.PullRequest {
	return github.PullRequest{
		Repo: testContributorRepo, Number: testContributorPRNum, Author: author,
		HeadSHA: testContributorHead, CIStatus: "success",
		Mergeable: github.MergeableYes, MergeableState: "clean",
	}
}

func withMaintainerApproval(pr github.PullRequest, sha string) github.PullRequest {
	pr.Protection = &github.ProtectionFacts{
		ApprovalsGiven:      1,
		MaintainerApprovals: []github.MaintainerApproval{{Login: testMaintainer, CommitSHA: sha}},
	}
	return pr
}

// The issue's first acceptance case: green, not held, swarm-approved, and
// still not merge-eligible, because the swarm verdict is not a maintainer.
func TestContributorGate_SwarmApprovalAloneIsNotEligible(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		bucket, verdict, _ := classifyMergeEligibility(greenPR(testContributor), false, testContributorRepo, swarmGates(testAuthorPolicy(optIn)))
		if bucket != mergeBucketSkip {
			t.Fatalf("optIn=%v: bucket = %v, want skip (verdict %+v)", optIn, bucket, verdict)
		}
		if verdict.State == github.MergeVerdictEligible {
			t.Errorf("optIn=%v: verdict is eligible (green) for a swarm-approved contributor PR", optIn)
		}
		if !strings.Contains(verdict.Reason, github.ContributorMergeReason) {
			t.Errorf("optIn=%v: reason %q does not say it needs a maintainer's review", optIn, verdict.Reason)
		}
	}
	// Positive control: the same gates without the author gate make it
	// eligible, so the skip above is the new rule and not a broken fixture.
	if bucket, _, _ := classifyMergeEligibility(greenPR(testContributor), false, testContributorRepo, swarmGates(nil)); bucket != mergeBucketEligible {
		t.Fatalf("positive control: bucket = %v, want eligible with no author gate", bucket)
	}
}

// Default config: the hive never merges contributor PRs, even ones a
// maintainer approved. The reason names the opt-in.
func TestContributorGate_OptInOffNeverEligible(t *testing.T) {
	pr := withMaintainerApproval(greenPR(testContributor), testContributorHead)
	bucket, verdict, _ := classifyMergeEligibility(pr, false, testContributorRepo, swarmGates(testAuthorPolicy(false)))
	if bucket != mergeBucketSkip {
		t.Fatalf("bucket = %v, want skip with auto_merge.contributor_prs off", bucket)
	}
	if want := github.ContributorMergeRefusalReason(false); verdict.Reason != want {
		t.Errorf("reason = %q, want %q", verdict.Reason, want)
	}
	if !strings.Contains(verdict.Reason, "auto_merge.contributor_prs") {
		t.Errorf("reason %q does not name the opt-in", verdict.Reason)
	}
}

// The second half of the first acceptance case: with the opt-in, the same PR
// becomes eligible once a maintainer approved THIS head, and only then.
func TestContributorGate_MaintainerApprovalAtHeadIsEligible(t *testing.T) {
	approved := withMaintainerApproval(greenPR(testContributor), testContributorHead)
	bucket, verdict, _ := classifyMergeEligibility(approved, false, testContributorRepo, swarmGates(testAuthorPolicy(true)))
	if bucket != mergeBucketEligible || verdict.State != github.MergeVerdictEligible {
		t.Fatalf("bucket=%v verdict=%+v, want eligible with the opt-in and a maintainer approval at head", bucket, verdict)
	}
	if !strings.Contains(verdict.Reason, testMaintainer) {
		t.Errorf("reason %q does not name the approving maintainer", verdict.Reason)
	}

	// The maintainer's approval stands in for the swarm: no swarm verdict at
	// all is fine, a swarm verdict is not required.
	noSwarm := mergeGates{requireReviewApproval: true, reviewLoaded: true, authors: testAuthorPolicy(true)}
	if bucket, verdict, _ := classifyMergeEligibility(approved, false, testContributorRepo, noSwarm); bucket != mergeBucketEligible {
		t.Errorf("without a swarm verdict: bucket=%v verdict=%+v, want eligible on the maintainer's approval", bucket, verdict)
	}

	// An approval of an older head does not vouch for commits pushed since.
	stale := withMaintainerApproval(greenPR(testContributor), "sha-older")
	bucket, verdict, _ = classifyMergeEligibility(stale, false, testContributorRepo, swarmGates(testAuthorPolicy(true)))
	if bucket != mergeBucketSkip {
		t.Fatalf("stale approval: bucket = %v, want skip", bucket)
	}
	if want := github.ContributorMergeRefusalReason(true); verdict.Reason != want {
		t.Errorf("stale approval: reason = %q, want %q", verdict.Reason, want)
	}

	// Facts never fetched: fail closed.
	if bucket, _, _ := classifyMergeEligibility(greenPR(testContributor), false, testContributorRepo, swarmGates(testAuthorPolicy(true))); bucket != mergeBucketSkip {
		t.Errorf("no review facts: bucket = %v, want skip", bucket)
	}
}

// An unknown author and a forged hive trailer are both contributor PRs, and
// a bot the operator did not list is too.
func TestContributorGate_FailsClosedOnAuthorship(t *testing.T) {
	cases := map[string]github.PullRequest{
		"empty author": greenPR(""),
		"forged hive attribution trailer": func() github.PullRequest {
			pr := greenPR(testContributor)
			pr.HiveAttributed = true
			pr.HiveAgent = "scanner"
			return pr
		}(),
		"untrusted bot": greenPR("some-app[bot]"),
	}
	for name, pr := range cases {
		bucket, verdict, _ := classifyMergeEligibility(pr, false, testContributorRepo, swarmGates(testAuthorPolicy(false)))
		if bucket != mergeBucketSkip || !strings.Contains(verdict.Reason, github.ContributorMergeReason) {
			t.Errorf("%s: bucket=%v reason=%q, want a contributor refusal", name, bucket, verdict.Reason)
		}
	}
}

// Regression: hive-authored and trusted-bot PRs are classified exactly as
// they were before the gate existed, in every branch the gate could touch.
func TestContributorGate_HiveAndTrustedBotPRsUnchanged(t *testing.T) {
	appPR := greenPR("hive-app[bot]")
	appPR.AppAuthored = true
	authors := map[string]github.PullRequest{
		"app authored":                appPR,
		"app login via identity":      greenPR("Hive-App[bot]"),
		"app/ spelling":               greenPR("app/hive-app"),
		"project.ai_author any case":  greenPR("HIVE-AI"),
		"trusted bot":                 greenPR(testTrustedBot),
		"trusted bot, different case": greenPR("Dependabot[bot]"),
	}
	variants := map[string]func(github.PullRequest) (github.PullRequest, bool, mergeGates){
		"swarm approved": func(pr github.PullRequest) (github.PullRequest, bool, mergeGates) {
			return pr, false, swarmGates(nil)
		},
		"awaiting swarm approval": func(pr github.PullRequest) (github.PullRequest, bool, mergeGates) {
			return pr, false, mergeGates{requireReviewApproval: true, reviewLoaded: true}
		},
		"no review requirement": func(pr github.PullRequest) (github.PullRequest, bool, mergeGates) {
			return pr, false, mergeGates{}
		},
		"red": func(pr github.PullRequest) (github.PullRequest, bool, mergeGates) {
			pr.CIStatus, pr.FailingChecks = "failure", []string{"build"}
			return pr, false, mergeGates{}
		},
		"held": func(pr github.PullRequest) (github.PullRequest, bool, mergeGates) {
			return pr, true, swarmGates(nil)
		},
	}
	for an, base := range authors {
		for vn, variant := range variants {
			pr, held, before := variant(base)
			wantBucket, wantVerdict, _ := classifyMergeEligibility(pr, held, testContributorRepo, before)
			for _, optIn := range []bool{false, true} {
				after := before
				after.authors = testAuthorPolicy(optIn)
				gotBucket, gotVerdict, _ := classifyMergeEligibility(pr, held, testContributorRepo, after)
				if gotBucket != wantBucket || gotVerdict != wantVerdict {
					t.Errorf("%s / %s (optIn=%v): got %v %+v, want %v %+v (unchanged)",
						an, vn, optIn, gotBucket, gotVerdict, wantBucket, wantVerdict)
				}
			}
		}
	}
}

// Red routing is unchanged for contributor PRs: a red one still lands in the
// failing bucket, held or not, exactly as with no gate. It carries no
// authoring agent, so it routes to nobody new.
func TestContributorGate_RedRoutingUnchanged(t *testing.T) {
	red := greenPR(testContributor)
	red.CIStatus, red.FailingChecks = "failure", []string{"build"}
	for _, held := range []bool{false, true} {
		wantBucket, wantVerdict, _ := classifyMergeEligibility(red, held, testContributorRepo, swarmGates(nil))
		gotBucket, gotVerdict, _ := classifyMergeEligibility(red, held, testContributorRepo, swarmGates(testAuthorPolicy(false)))
		if gotBucket != mergeBucketFailing || gotBucket != wantBucket || gotVerdict != wantVerdict {
			t.Errorf("held=%v: got %v %+v, want %v %+v (failing, unchanged)", held, gotBucket, gotVerdict, wantBucket, wantVerdict)
		}
	}
}

// End to end through writeMergeEligible with a real config: the gate is
// installed from cfg, a swarm-approved contributor PR is kept out of
// merge-eligible.json, and the merge relay's F4 binding therefore refuses it.
func TestWriteMergeEligible_ContributorPRKeptOutOfEligibleList(t *testing.T) {
	origPath := review.ReviewVerdictsPath
	t.Cleanup(func() { review.ReviewVerdictsPath = origPath })
	review.ReviewVerdictsPath = filepath.Join(t.TempDir(), "review-verdicts.json")

	contributor := greenPR(testContributor)
	hivePR := greenPR(testHiveAIAuthor)
	hivePR.Number = testContributorPRNum + 1
	artifact := swarmApproved()
	artifact.Items = append(artifact.Items, review.Aggregate{
		Repo: testContributorRepo, Number: hivePR.Number, HeadSHA: testContributorHead,
		Verdict: review.VerdictApprove, MergeEligible: true,
	})
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(review.ReviewVerdictsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Project.AIAuthor = testHiveAIAuthor
	eligible, _ := runWriteMergeEligible(t, []github.PullRequest{contributor, hivePR},
		mergeEligibleInputs{org: "org", requireReview: true, cfg: cfg})
	if len(eligible) != 1 || eligible[0].Number != hivePR.Number {
		t.Fatalf("eligible = %+v, want only the hive-authored PR #%d", eligible, hivePR.Number)
	}

	// The relay's target binding reads the same file.
	authz := bindMergeAuthz(func(string, int, string) error { return nil })
	if err := authz("scanner", 0, testContributorRepo, contributor.Number, testContributorHead); err == nil {
		t.Error("merge relay authorized a swarm-approved contributor PR")
	}
	if err := authz("scanner", 0, testContributorRepo, hivePR.Number, testContributorHead); err != nil {
		t.Errorf("merge relay refused the eligible hive PR: %v", err)
	}

	// Opting in is not enough on its own: no maintainer approval, no merge.
	cfg.AutoMerge.ContributorPRs = true
	eligible, _ = runWriteMergeEligible(t, []github.PullRequest{contributor},
		mergeEligibleInputs{org: "org", requireReview: true, cfg: cfg})
	if len(eligible) != 0 {
		t.Fatalf("opt-in without approval: eligible = %+v, want none", eligible)
	}
	eligible, _ = runWriteMergeEligible(t, []github.PullRequest{withMaintainerApproval(contributor, testContributorHead)},
		mergeEligibleInputs{org: "org", requireReview: true, cfg: cfg})
	if len(eligible) != 1 || eligible[0].Number != contributor.Number {
		t.Fatalf("opt-in with approval at head: eligible = %+v, want PR #%d", eligible, contributor.Number)
	}
}

func TestContributorMergePolicyFor(t *testing.T) {
	if got := contributorMergePolicyFor(nil); got.AllowContributorPRs || len(got.TrustedBots) != 0 {
		t.Errorf("nil cfg policy = %+v, want the most restrictive", got)
	}
	cfg := &config.Config{}
	got := contributorMergePolicyFor(cfg)
	if got.AllowContributorPRs {
		t.Error("contributor merges must default off")
	}
	if !got.TrustedBots[testTrustedBot] {
		t.Errorf("trusted bots = %v, want the auto_merge default set", got.TrustedBots)
	}
	cfg.AutoMerge.ContributorPRs = true
	if !contributorMergePolicyFor(cfg).AllowContributorPRs {
		t.Error("auto_merge.contributor_prs: true not carried to the relay policy")
	}
	if newMergeAuthorPolicy(nil) != nil {
		t.Error("nil cfg must leave the classifier gate uninstalled")
	}
}
