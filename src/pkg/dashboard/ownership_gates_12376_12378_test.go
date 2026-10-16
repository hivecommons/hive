package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#12376: the blocked label is split by what it waits on.
// hivecommons/hive#12378: a PR waiting on a gate label, a person's review
// decision or a hand merge is your turn.

var (
	blockedUnexplained12376 = want11565{OwnershipVerbReview, OwnershipReasonBlockedUnexplained}
	reviewDecision12378     = want11565{OwnershipVerbReview, OwnershipReasonReviewDecision}
	handMerge12378          = want11565{OwnershipVerbReview, OwnershipReasonHandMerge}
)

func withBlockers12376(in issueOwnershipInput, blockers ...string) issueOwnershipInput {
	in.openBlockers = blockers
	return in
}

func staleNonActionable12376(bucket, reason string, labels ...string) issueOwnershipInput {
	in := nonActionable11565(bucket, reason, labels...)
	stale := ownershipNow11565.AddDate(0, 0, -20)
	in.createdAt, in.updatedAt = stale, stale
	return in
}

func TestBlockedIssueOwnership12376(t *testing.T) {
	covered := staleIssue11592("", "blocked")
	covered.openLinkedPR = true
	cases := []struct {
		name      string
		in        issueOwnershipInput
		want      want11565
		elsewhere bool
	}{
		{"blocked on another item: waiting elsewhere", withBlockers12376(staleIssue11592("", "blocked"), "o/other#25"), notYours11565, true},
		{"fresh block on another item: waiting elsewhere", withBlockers12376(freshIssue11565("blocked"), "o/other#25"), notYours11565, true},
		{"blocked with no recorded blocker, stale: ask the person", staleIssue11592("", "blocked"), blockedUnexplained12376, false},
		{"label spelled in capitals", staleIssue11592("", "Blocked"), blockedUnexplained12376, false},
		{"blocked with no recorded blocker, fresh: unchanged", freshIssue11565("blocked"), notYours11565, false},
		{"blocked outside the queue, stale: links never read, unchanged", staleNonActionable12376(repoWorkBucketFiltered, "Exempt label", "blocked"), notYours11565, false},
		{"blocked outside the queue with an open link: waiting elsewhere", withBlockers12376(staleNonActionable12376(repoWorkBucketFiltered, "Exempt label", "blocked"), "o/other#25"), notYours11565, true},
		{"blocked and covered by an open PR: the PR closes it", covered, notYours11565, true},
		{"blocked and parked: the parking rule decides", staleIssue11592("", "blocked", "needs-human"), want11565{OwnershipVerbReview, OwnershipReasonParkedNeedsHuman}, false},
		{"blocked beats the safety net", staleIssue11592("quality", "blocked"), blockedUnexplained12376, false},
		{"no blocked label: unchanged", staleIssue11592(""), notYours11565, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideIssueOwnershipGated(tc.in, ownershipEnv11565(3))
			checkOwnership11565(t, got, tc.want)
			if got.WaitingElsewhere != tc.elsewhere {
				t.Fatalf("waiting elsewhere = %v, want %v (%+v)", got.WaitingElsewhere, tc.elsewhere, got)
			}
		})
	}
	got := decideIssueOwnershipGated(staleIssue11592("", "blocked"), ownershipEnv11565(3))
	if got.Reason != "Labelled blocked, nothing says on what — name the blocker or lift it" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestBlockedIssueThroughRepoDecision12376(t *testing.T) {
	stale := ownershipNow11565.AddDate(0, 0, -20)
	repos := []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{
			github.Issue{Repo: "o/r", Number: 1, Labels: []string{"blocked"}, CreatedAt: stale, UpdatedAt: stale,
				DependsOn: []github.IssueDependency{{Key: "o/r#9"}}},
			github.Issue{Repo: "o/r", Number: 2, Labels: []string{"blocked"}, CreatedAt: stale, UpdatedAt: stale,
				DependsOn: []github.IssueDependency{{Key: "o/r#9", Resolved: true}}},
		},
	}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(3))
	if got := repos[0].Ownership[ownershipKey(ownershipKindIssue, 1)]; got.YourTurn || !got.WaitingElsewhere {
		t.Fatalf("issue blocked by an open link = %+v, want waiting elsewhere", got)
	}
	got := repos[0].Ownership[ownershipKey(ownershipKindIssue, 2)]
	checkOwnership11565(t, got, blockedUnexplained12376)
	if got.InboxRank == 0 {
		t.Fatalf("blocked issue with only resolved links has no inbox rank: %+v", got)
	}
}

func withProtection12378(pr github.PullRequest, facts *github.ProtectionFacts) github.PullRequest {
	pr.Protection = facts
	return pr
}

func TestPRReviewDecisionIsYourTurn12378(t *testing.T) {
	required := &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}
	changes := &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested, ChangesRequestedBy: []string{"bob"}}
	draft := withProtection12378(personPR11565(), required)
	draft.Draft = true
	held := withProtection12378(hivePR11565("hold"), required)
	held.HoldSources = []string{github.HoldSourceLevelGate}
	cases := []struct {
		name   string
		pr     github.PullRequest
		held   bool
		want   want11565
		reason string
	}{
		{"person's PR, review required", withProtection12378(personPR11565(), required), false, reviewDecision12378, "an approving review is required by branch protection (0 given)"},
		{"hive PR, review required", withProtection12378(hivePR11565(), required), false, reviewDecision12378, "an approving review is required"},
		{"person's PR, changes requested", withProtection12378(personPR11565(), changes), false, reviewDecision12378, "changes requested by @bob"},
		{"approved", withProtection12378(personPR11565(), &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionApproved}), false, notYours11565, ""},
		{"draft keeps its decision", draft, false, notYours11565, ""},
		{"held PR keeps its hold-source decision", held, true, want11565{OwnershipVerbReview, OwnershipReasonLevelGateHold}, ""},
		{"needs-human keeps its rule", withProtection12378(personPR11565("needs-human"), required), false, want11565{OwnershipVerbReview, OwnershipReasonPersonPRNeedsHuman}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePROwnershipGated(tc.pr, nil, tc.held, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
			checkOwnership11565(t, got, tc.want)
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
		})
	}
}

// The merge classifier skips a PR whose review decision blocks it
// (HumanReviewBlocksMerge); ownership must then say your turn, and only then.
func TestPRReviewDecisionAgreesWithMergeClassifier12378(t *testing.T) {
	facts := []*github.ProtectionFacts{
		nil,
		{},
		{ReviewDecision: github.ReviewDecisionApproved},
		{ReviewDecision: github.ReviewDecisionReviewRequired, ApprovalsGiven: 1},
		{ReviewDecision: github.ReviewDecisionChangesRequested},
		{ReviewDecision: github.ReviewDecisionChangesRequested, ChangesRequestedBy: []string{"bob"}, LatestHumanReviewAddressed: true},
		{ReviewDecision: github.ReviewDecisionReviewRequired, RequiredChecksKnown: true, FailingRequiredChecks: []string{"build"}},
		{RequiredChecksKnown: true, MissingRequiredChecks: []string{"build"}},
	}
	for i, f := range facts {
		for _, pr := range []github.PullRequest{personPR11565(), hivePR11565()} {
			pr = withProtection12378(pr, f)
			_, skipped := pr.HumanReviewBlocksMerge()
			got := decidePROwnershipGated(pr, nil, false, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
			if yours := got.YourTurn && got.ReasonCode == OwnershipReasonReviewDecision; yours != skipped {
				t.Fatalf("facts #%d (%s): merge classifier skips = %v, ownership = %+v", i, prAgentRole(pr), skipped, got)
			}
		}
	}
}

// Every label that puts a PR in the Gated band makes it your turn, with the
// parking verb where the label names a bucket.
func TestPRGateLabelsAreYourTurn12378(t *testing.T) {
	cfg := config.DashboardIssueBandsConfig{WaitingLabels: append(append([]string{}, defaultIssueWaitingLabels...), "waiting-on-legal")}
	verbs := map[string]want11565{
		"needs-decision":   {OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision},
		"2-discussing":     {OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision},
		"needs-direction":  {OwnershipVerbSteer, OwnershipReasonParkedNeedsDirection},
		"needs-spec":       {OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec},
		"blocked":          {OwnershipVerbReview, OwnershipReasonPRGateLabel},
		"waiting-on-legal": {OwnershipVerbReview, OwnershipReasonPRGateLabel},
	}
	seen := map[string]bool{}
	for _, label := range prGateLabels(normalizeIssueBandsConfig(cfg)) {
		if label == ownershipLabelNeedsHuman || seen[label] {
			continue
		}
		seen[label] = true
		want, ok := verbs[label]
		if !ok {
			t.Fatalf("gate label %q has no expected decision; add it to the table", label)
		}
		for _, pr := range []github.PullRequest{personPR11565(label), hivePR11565(label)} {
			if band := prBand(pr, nil, false, cfg, ownershipNow11565, github.AutoMergeQueuedLabel, "").Band; band != "waiting" {
				t.Fatalf("%q: band = %q, want waiting", label, band)
			}
			checkOwnership11565(t, decidePROwnershipGated(pr, nil, false, cfg, ownershipEnv11565(5)), want)
		}
	}
	if len(seen) != len(verbs) {
		t.Fatalf("gate labels %v, want every label of %v", seen, verbs)
	}

	// A repository that renames a bucket's labels gets the same verb.
	env := ownershipEnv11565(5)
	env.parking = ownershipParkingFor(config.HardSuppressLabelsConfig{NeedsDirection: []string{"wants-direction"}})
	checkOwnership11565(t, decidePROwnershipGated(personPR11565("wants-direction"), nil, false, cfg, env), verbs["needs-direction"])

	// needs-human keeps its own rules, the adjudicator wait included.
	checkOwnership11565(t, decidePROwnershipGated(personPR11565("needs-human", "needs-decision"), nil, false, cfg, ownershipEnv11565(5)),
		want11565{OwnershipVerbReview, OwnershipReasonPersonPRNeedsHuman})
	waiting := decidePROwnershipGated(hivePR11565("needs-human", "needs-decision"), nil, false, cfg, ownershipEnv11565(5))
	if waiting.YourTurn || !waiting.WaitingElsewhere {
		t.Fatalf("hive needs-human PR within the adjudicator wait = %+v, want waiting elsewhere", waiting)
	}
	// A held PR keeps its hold-source decision.
	held := personPR11565("hold", "needs-decision")
	held.HoldSources = []string{github.HoldSourcePerson}
	if got := decidePROwnershipGated(held, nil, true, cfg, ownershipEnv11565(5)); got.YourTurn {
		t.Fatalf("person-held PR = %+v, want not your turn", got)
	}
}

func TestPRHandMergeIsYourTurn12378(t *testing.T) {
	eligible := &github.MergeVerdict{State: github.MergeVerdictEligible}
	outstanding := &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "awaiting review approval"}
	takes := func(trusted ...string) func(repo, author string) bool {
		return func(_, author string) bool {
			for _, login := range trusted {
				if author == login {
					return true
				}
			}
			return false
		}
	}
	draft := personPR11565()
	draft.Draft = true
	cases := []struct {
		name    string
		pr      github.PullRequest
		verdict *github.MergeVerdict
		lane    func(repo, author string) bool
		want    want11565
	}{
		{"eligible, no lane takes the author", personPR11565(), eligible, takes(), handMerge12378},
		{"eligible, a lane takes the author", personPR11565(), eligible, takes("alice"), notYours11565},
		{"Hive's own PR is the self-authored lane's", hivePR11565(), eligible, takes(), notYours11565},
		{"not eligible yet", personPR11565(), outstanding, takes(), notYours11565},
		{"no verdict", personPR11565(), nil, takes(), notYours11565},
		{"draft", draft, eligible, takes(), notYours11565},
		{"lane unknown", personPR11565(), eligible, nil, notYours11565},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := ownershipEnv11565(5)
			env.mergeLaneTakesAuthor = tc.lane
			checkOwnership11565(t, decidePROwnershipGated(tc.pr, tc.verdict, false, config.DashboardIssueBandsConfig{}, env), tc.want)
		})
	}

	// Through the snapshot: the verdict attached before the decision runs,
	// the PR counted once, in the inbox, and the lane asked under the PR's
	// owner/repo name (the sweeps' allow-lists accept that spelling).
	bare := personPR11565()
	bare.Repo = ""
	repos := []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: bare}}}}
	attachRepoMergeVerdicts(repos, map[string]github.MergeVerdict{github.MergeVerdictKey(bare): *eligible})
	env := ownershipEnv11565(5)
	var askedRepo string
	env.mergeLaneTakesAuthor = func(repo, author string) bool {
		askedRepo = repo
		return takes()(repo, author)
	}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	if askedRepo != "o/r" {
		t.Fatalf("lane asked for repo %q, want owner/repo %q", askedRepo, "o/r")
	}
	got := repos[0].Ownership[ownershipKey(ownershipKindPR, personPR11565().Number)]
	checkOwnership11565(t, got, handMerge12378)
	if got.InboxRank != 1 {
		t.Fatalf("inbox rank = %d, want 1", got.InboxRank)
	}
}
