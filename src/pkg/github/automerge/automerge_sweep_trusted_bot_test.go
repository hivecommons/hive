package automerge

// The trusted-bot lane lets the self-authored sweep merge dependency-bot PRs
// through the same gates as the App's own. These tests pin the lane boundary:
// only logins in the live TrustedBotAuthors set qualify, the match is
// case-insensitive on the exact login, a nil resolver keeps the sweep App-only,
// and the lane string recorded on the mutation claim distinguishes the two.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func newTrustedBotEngine(t *testing.T, trusted func() map[string]bool) *Engine {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:0")
	client.SetAppBotLogin(testHiveAppBotLogin)
	return New(client, Options{TrustedBotAuthors: trusted})
}

func TestSweepLaneForAuthor(t *testing.T) {
	trusted := func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	c := newTrustedBotEngine(t, trusted)

	cases := []struct {
		author   string
		wantLane string
		wantOK   bool
	}{
		{testHiveAppBotLogin, "self-authored", true},
		{"dependabot[bot]", "trusted-bot", true},
		{"Dependabot[bot]", "trusted-bot", true},
		{"renovate[bot]", "", false},
		{"mallory", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		lane, ok := c.sweepLaneForAuthor(tc.author)
		if lane != tc.wantLane || ok != tc.wantOK {
			t.Errorf("sweepLaneForAuthor(%q) = (%q, %v), want (%q, %v)", tc.author, lane, ok, tc.wantLane, tc.wantOK)
		}
	}
}

func TestSweepLaneForAuthorNilResolverIsAppOnly(t *testing.T) {
	c := newTrustedBotEngine(t, nil)
	if _, ok := c.sweepLaneForAuthor("dependabot[bot]"); ok {
		t.Fatal("nil TrustedBotAuthors must not admit dependabot")
	}
	if lane, ok := c.sweepLaneForAuthor(testHiveAppBotLogin); !ok || lane != "self-authored" {
		t.Fatalf("App login must still qualify: got (%q, %v)", lane, ok)
	}
}

func TestPrefilterSelfAuthoredPRAdmitsTrustedBot(t *testing.T) {
	c := newTrustedBotEngine(t, func() map[string]bool { return map[string]bool{"dependabot[bot]": true} })
	pr := selfAuthoredListPR(func(pr *gh.PullRequest) { pr.User = &gh.User{Login: gh.Ptr("dependabot[bot]")} })
	if got := c.prefilterSelfAuthoredPR(pr); got != "" {
		t.Fatalf("trusted bot PR should pass prefilter, got %q", got)
	}
	// The other gates still apply to the bot lane.
	pr.Draft = gh.Ptr(true)
	if got := c.prefilterSelfAuthoredPR(pr); got != "draft" {
		t.Fatalf("draft trusted bot PR should be skipped as draft, got %q", got)
	}
	// Lane is live: an emptied set (operator reload) revokes admission.
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{} }
	pr.Draft = nil
	if got := c.prefilterSelfAuthoredPR(pr); got != "not-app-authored" {
		t.Fatalf("revoked trusted bot should be not-app-authored, got %q", got)
	}
}

func TestPrefilterTrustedAuthorPRDoesNotSpecialCaseTrustedBot(t *testing.T) {
	// The prefilter's contract is genuine skip reasons only; lane hand-off is
	// the sweep loop's decision, so a trusted bot here is judged by the role
	// gate like any other author.
	c := newTrustedBotEngine(t, func() map[string]bool { return map[string]bool{"dependabot[bot]": true} })
	c.trustedAuthorizer = func(string, string) TrustedAuthorDecision { return TrustedAuthorDecision{} }
	pr := selfAuthoredListPR(func(pr *gh.PullRequest) {
		pr.User = &gh.User{Login: gh.Ptr("dependabot[bot]")}
	})
	if got := c.prefilterTrustedAuthorPR("acme", "widget", pr, TrustedAuthorPolicy{RequireRole: "merger"}); got != "untrusted-author-role" {
		t.Fatalf("prefilterTrustedAuthorPR = %q, want untrusted-author-role", got)
	}
}

func TestTrustedAuthorSweepHandsTrustedBotToSelfAuthoredLane(t *testing.T) {
	fx := trustedSweepFixture{author: "dependabot[bot]"}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
	trustedAuthorChecks := 0
	c := newTrustedSweepEngine(api.URL, policy, Options{
		TrustedBotAuthors: func() map[string]bool { return map[string]bool{"dependabot[bot]": true} },
	})
	c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
		trustedAuthorChecks++
		return TrustedAuthorDecision{}
	}

	result, err := c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{SelfAuthoredLaneActive: true})
	if err != nil {
		t.Fatalf("SweepTrustedAuthorAutoMerges: %v", err)
	}
	if trustedAuthorChecks != 0 {
		t.Fatalf("trusted-author checks = %d, want 0 for trusted-bot lane", trustedAuthorChecks)
	}
	// Seen == Skipped + Candidates + len(Merged): the hand-off is a counted
	// skip (reason trusted-bot-lane), not a silently dropped PR.
	if result.Seen != 1 || result.Candidates != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want trusted bot counted as a skipped hand-off", result)
	}
}

func TestTrustedAuthorSweepEvaluatesTrustedBotWhenSelfAuthoredLaneInactive(t *testing.T) {
	// ACMM below the self-merge floor (or self_authored off): no other sweep
	// will ever look at the bot's PR, so the trusted-author tier must keep
	// judging it by role — and may merge it when the operator granted one.
	fx := trustedSweepFixture{author: "dependabot[bot]"}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
	trustedAuthorChecks := 0
	c := newTrustedSweepEngine(api.URL, policy, Options{
		TrustedBotAuthors: func() map[string]bool { return map[string]bool{"dependabot[bot]": true} },
	})
	c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
		trustedAuthorChecks++
		return TrustedAuthorDecision{}
	}

	result, err := c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepTrustedAuthorAutoMerges: %v", err)
	}
	if trustedAuthorChecks == 0 {
		t.Fatal("trusted-author role check must run when the self-authored lane is inactive")
	}
	if result.Seen != 1 || result.Candidates != 0 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want trusted bot skipped as untrusted-author-role", result)
	}
}

func TestTrustedAuthorSweepHandsAppAuthoredToSelfLaneWithOwnReason(t *testing.T) {
	// The self-authored lane owns the App's own PRs, so the trusted-author
	// tier must hand off under its own reason — not a contradictory
	// untrusted-author-role, and not the contributor lane's reason, which
	// read as if that lane had routed the PR (hivecommons/hive#14482) —
	// whether or not the self-authored lane is active.
	for _, active := range []bool{false, true} {
		fx := trustedSweepFixture{author: testHiveAppBotLogin}
		api := newTrustedSweepAPI(t, &fx)
		var logs bytes.Buffer
		policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
		tracker := NewStageTracker()
		c := newTrustedSweepEngine(api.URL, policy, Options{
			Logger:           slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
			StageTracker:     tracker,
		})

		result, err := c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{SelfAuthoredLaneActive: active})
		api.Close()
		if err != nil {
			t.Fatalf("SweepTrustedAuthorAutoMerges(active=%v): %v", active, err)
		}
		if result.Seen != 1 || result.Candidates != 0 || result.Skipped != 1 {
			t.Fatalf("active=%v result = %+v, want App PR counted as a skipped hand-off", active, result)
		}
		drained := tracker.drain()
		if len(drained) != 1 || len(drained[0]) != 1 {
			t.Fatalf("active=%v drained = %+v, want one observation for the App PR", active, drained)
		}
		if obs := drained[0][0]; obs.Lane != stageLaneTrusted || obs.Reason != trustedAuthorReasonAppAuthored {
			t.Fatalf("active=%v observation lane/reason = %s/%s, want %s/%s", active, obs.Lane, obs.Reason, stageLaneTrusted, trustedAuthorReasonAppAuthored)
		}
		if v := classifyStage(drained[0][0]); v.Rank != stageRankNone {
			t.Fatalf("active=%v hand-off classified as %+v, want no opinion", active, v)
		}
		if strings.Contains(logs.String(), "untrusted-author-role") || strings.Contains(logs.String(), contributorLaneReasonAppAuthored) || !strings.Contains(logs.String(), "reason="+trustedAuthorReasonAppAuthored+" tier=trusted-author") {
			t.Fatalf("active=%v logs = %q, want reason %s under tier=trusted-author and neither untrusted-author-role nor %s", active, logs.String(), trustedAuthorReasonAppAuthored, contributorLaneReasonAppAuthored)
		}
	}
}
