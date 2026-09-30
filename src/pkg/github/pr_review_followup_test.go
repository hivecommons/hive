package github

import (
	"testing"
	"time"
)

func reviewFollowUpRaw(reviewedAt time.Time) rawPRReviewFollowUp {
	var raw rawPRReviewFollowUp
	raw.LatestOpinionatedReviews.Nodes = append(raw.LatestOpinionatedReviews.Nodes, struct {
		State             string              `json:"state"`
		SubmittedAt       time.Time           `json:"submittedAt"`
		AuthorAssociation string              `json:"authorAssociation"`
		Author            *rawPRCommentAuthor `json:"author"`
	}{
		State:             "CHANGES_REQUESTED",
		SubmittedAt:       reviewedAt,
		AuthorAssociation: "OWNER",
		Author:            &rawPRCommentAuthor{Login: "Danathar", Typename: "User"},
	})
	return raw
}

func addFollowUpCommit(raw *rawPRReviewFollowUp, at time.Time, parents int) {
	node := struct {
		Commit struct {
			CommittedDate time.Time `json:"committedDate"`
			Parents       struct {
				TotalCount int `json:"totalCount"`
			} `json:"parents"`
		} `json:"commit"`
	}{}
	node.Commit.CommittedDate = at
	node.Commit.Parents.TotalCount = parents
	raw.Commits.Nodes = append(raw.Commits.Nodes, node)
}

// The #252 timeline is the whole point of the rule: a merge-from-base commit
// pushed by the held-PR CI/conflict repair path after the review keeps the
// branch mergeable and answers nothing.
func TestReviewAddressed_MergeCommitDoesNotAddress(t *testing.T) {
	reviewed := time.Date(2026, 9, 2, 14, 15, 0, 0, time.UTC)
	commits := []PRCommitSummary{
		{CommittedAt: reviewed.Add(-2 * time.Hour), Parents: 1},
		{CommittedAt: reviewed.Add(96 * time.Minute), Parents: 2},
	}
	if addressed, by := ReviewAddressed(reviewed, commits, nil); addressed {
		t.Fatalf("merge commit after the review must not address it (by=%q)", by)
	}
}

func TestReviewAddressed_NonMergeCommitAndReply(t *testing.T) {
	reviewed := time.Date(2026, 9, 2, 14, 15, 0, 0, time.UTC)

	commits := []PRCommitSummary{{CommittedAt: reviewed.Add(time.Hour), Parents: 1}}
	addressed, by := ReviewAddressed(reviewed, commits, nil)
	if !addressed || by != ReviewAddressedByCommit {
		t.Fatalf("non-merge commit after the review: got addressed=%v by=%q", addressed, by)
	}

	replies := []time.Time{reviewed.Add(30 * time.Minute)}
	addressed, by = ReviewAddressed(reviewed, []PRCommitSummary{{CommittedAt: reviewed.Add(9 * time.Hour), Parents: 2}}, replies)
	if !addressed || by != ReviewAddressedByReply {
		t.Fatalf("reply after the review: got addressed=%v by=%q", addressed, by)
	}

	// Everything before the review answers nothing.
	if addressed, _ := ReviewAddressed(reviewed,
		[]PRCommitSummary{{CommittedAt: reviewed.Add(-time.Minute), Parents: 1}},
		[]time.Time{reviewed.Add(-time.Hour)}); addressed {
		t.Fatal("activity older than the review must not address it")
	}
	if addressed, _ := ReviewAddressed(time.Time{}, commits, replies); addressed {
		t.Fatal("no review means nothing to address")
	}
}

func TestReviewFollowUpFrom_ClassifiesTheMotivatingCase(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	reviewed := time.Date(2026, 9, 2, 14, 15, 0, 0, time.UTC)

	raw := reviewFollowUpRaw(reviewed)
	addFollowUpCommit(&raw, reviewed.Add(96*time.Minute), 2) // merge-from-base

	got := c.reviewFollowUpFrom(raw)
	if got.Reviewer != "Danathar" || !got.SubmittedAt.Equal(reviewed) {
		t.Fatalf("latest human change request: got %+v", got)
	}
	if got.Addressed {
		t.Fatalf("merge-from-base commit must leave the review unaddressed: %+v", got)
	}
	if want := "CHANGES REQUESTED by @Danathar (unaddressed)"; got.Summary() != want {
		t.Errorf("Summary() = %q, want %q", got.Summary(), want)
	}

	// The owning agent's own reply (attribution trailer) addresses it.
	raw.Comments.Nodes = append(raw.Comments.Nodes,
		rawComment(1, "sec-check-user", "User", "COLLABORATOR",
			"pushed the fix\n\n"+AttributionTrailerPrefix+" agent=sec-check", reviewed.Add(2*time.Hour)))
	got = c.reviewFollowUpFrom(raw)
	if !got.Addressed || got.AddressedBy != ReviewAddressedByReply {
		t.Fatalf("hive reply must address the review: %+v", got)
	}
	if want := "CHANGES REQUESTED by @Danathar (addressed by reply)"; got.Summary() != want {
		t.Errorf("Summary() = %q, want %q", got.Summary(), want)
	}
}

// Only a trusted human's change request routes an agent back to its PR: a
// bot's, a stranger's, and an approval are all "nothing requested".
func TestReviewFollowUpFrom_OnlyTrustedHumanChangeRequests(t *testing.T) {
	c := humanCommentTestClient(t, "http://127.0.0.1:1")
	reviewed := time.Date(2026, 9, 2, 14, 15, 0, 0, time.UTC)

	cases := map[string]struct {
		state, assoc, login, typename string
	}{
		"approved":     {"APPROVED", "OWNER", "Danathar", "User"},
		"review bot":   {"CHANGES_REQUESTED", "COLLABORATOR", "Copilot", "User"},
		"hive itself":  {"CHANGES_REQUESTED", "COLLABORATOR", "hive[bot]", "Bot"},
		"stranger":     {"CHANGES_REQUESTED", "NONE", "stranger", "User"},
		"ghost author": {"CHANGES_REQUESTED", "OWNER", "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := reviewFollowUpRaw(reviewed)
			raw.LatestOpinionatedReviews.Nodes[0].State = tc.state
			raw.LatestOpinionatedReviews.Nodes[0].AuthorAssociation = tc.assoc
			if tc.login == "" {
				raw.LatestOpinionatedReviews.Nodes[0].Author = nil
			} else {
				raw.LatestOpinionatedReviews.Nodes[0].Author = &rawPRCommentAuthor{Login: tc.login, Typename: tc.typename}
			}
			if got := c.reviewFollowUpFrom(raw); !got.SubmittedAt.IsZero() {
				t.Fatalf("expected no routed change request, got %+v", got)
			}
		})
	}
}

func TestPRReviewFollowUpCandidate(t *testing.T) {
	changes := &ProtectionFacts{ReviewDecision: ReviewDecisionChangesRequested}
	hive := PullRequest{Repo: "o/r", Number: 252, HiveAttributed: true, Protection: changes}
	if !PRReviewFollowUpCandidate(hive) {
		t.Fatal("a hive-opened PR with a CHANGES_REQUESTED decision is a candidate")
	}

	human := hive
	human.HiveAttributed = false
	if PRReviewFollowUpCandidate(human) {
		t.Error("a human-opened PR is never a candidate")
	}
	draft := hive
	draft.Draft = true
	if PRReviewFollowUpCandidate(draft) {
		t.Error("a draft is never a candidate")
	}
	fork := hive
	fork.FromFork = true
	if PRReviewFollowUpCandidate(fork) {
		t.Error("a fork PR is never a candidate")
	}
	approved := hive
	approved.Protection = &ProtectionFacts{ReviewDecision: ReviewDecisionApproved}
	if PRReviewFollowUpCandidate(approved) {
		t.Error("an approved PR is never a candidate")
	}
	none := hive
	none.Protection = nil
	if PRReviewFollowUpCandidate(none) {
		t.Error("a PR with no review decision is never a candidate")
	}
}

func TestEnrichPRReviewFollowUp_SkipsNonCandidatesAndClearsStale(t *testing.T) {
	var c *Client
	prs := []PullRequest{{Repo: "o/r", Number: 1, ReviewFollowUp: &PRReviewFollowUp{Reviewer: "stale"}}}
	c.EnrichPRReviewFollowUp(t.Context(), prs, 5)
	if prs[0].ReviewFollowUp == nil || prs[0].ReviewFollowUp.Reviewer != "stale" {
		t.Fatal("a nil client must not touch the PRs")
	}

	// A real client with no candidates makes no call, and clears a stamp left
	// by an earlier pass (the review may have been withdrawn since).
	live := humanCommentTestClient(t, "http://127.0.0.1:1")
	live.EnrichPRReviewFollowUp(t.Context(), prs, 5)
	if prs[0].ReviewFollowUp != nil {
		t.Fatalf("stale follow-up state must be cleared, got %+v", prs[0].ReviewFollowUp)
	}
}
