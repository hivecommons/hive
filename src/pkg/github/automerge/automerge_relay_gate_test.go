package automerge

// Tests for the merge-request relay's lane gate (RelayMergeRefusal).
//
// Reproduction of hivecommons/docs#401 (2026-10-20): a PR from an
// untrusted-tier contributor, CI green, review approved, never soaked, was
// merged by the hive within minutes because a scanner's merge request went
// through the relay, which checked none of the contributor-lane gates. The
// first test below is that request; it must be refused with a reason naming
// the policy.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func relayGateEngine(t *testing.T, fx *contributorLaneFixture, policy ContributorLanePolicy, opts Options) (*Engine, *contributorLaneClock) {
	t.Helper()
	api := newContributorLaneAPI(t, fx)
	t.Cleanup(api.Close)
	clock := &contributorLaneClock{at: time.Date(2026, 10, 20, 15, 27, 0, 0, time.UTC)}
	return newContributorLaneEngine(api.URL, policy, clock, opts), clock
}

// TestRelayMergeRefusalUntrustedTierScannerRequest is docs#401: green CI,
// scanner request, author's only claim says tier=untrusted. The relay must
// refuse, the reason must name the tier policy, and nothing may be merged.
func TestRelayMergeRefusalUntrustedTierScannerRequest(t *testing.T) {
	fx := validContributorFixture()
	fx.labels = []string{"stage/060-reviewed", "contributor-lane:claim-tier-untrusted"}
	fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=untrusted verified=true")
	policy := defaultContributorLanePolicy()
	policy.Soak = 30 * time.Minute
	engine, clock := relayGateEngine(t, &fx, policy, Options{ContributorSoak: soakedTracker(clock0())})
	_ = clock

	refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if refusal == "" {
		t.Fatal("an untrusted-tier contributor PR must be refused by the relay gate")
	}
	for _, want := range []string{RelayMergeRefusalPrefix, "@" + contributorLaneTestAuthor, contributorLaneReasonClaimTier, `tier "untrusted"`, "require_tiers merger, trusted"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("refusal %q does not name %q", refusal, want)
		}
	}
	if fx.merges != 0 {
		t.Fatalf("merges = %d, want 0: the gate is read-only", fx.merges)
	}
}

func clock0() *contributorLaneClock {
	return &contributorLaneClock{at: time.Date(2026, 10, 20, 15, 27, 0, 0, time.UTC)}
}

func TestRelayMergeRefusalSoakAndHeadAge(t *testing.T) {
	t.Run("soak remaining is named", func(t *testing.T) {
		fx := validContributorFixture()
		policy := defaultContributorLanePolicy()
		policy.Soak = 30 * time.Minute
		// A fresh tracker: the head is first seen green now, so the whole
		// soak remains.
		engine, _ := relayGateEngine(t, &fx, policy, Options{ContributorSoak: NewContributorSoakTracker()})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		for _, want := range []string{contributorLaneReasonSoaking, "30m0s remaining", "soak is 30m0s"} {
			if !strings.Contains(refusal, want) {
				t.Fatalf("refusal %q does not name %q", refusal, want)
			}
		}
		if fx.merges != 0 {
			t.Fatalf("merges = %d, want 0", fx.merges)
		}
	})
	t.Run("fresh head under min_head_age is named", func(t *testing.T) {
		fx := validContributorFixture()
		clock := clock0()
		fx.headCommittedAt = clock.at.Add(-time.Minute).Format(time.RFC3339)
		policy := defaultContributorLanePolicy()
		engine, _ := relayGateEngine(t, &fx, policy, Options{ContributorSoak: soakedTracker(clock), HeadFirstSeen: NewHeadFirstSeenTracker(), MinHeadAge: 3 * time.Minute})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(refusal, "pending: head pushed ") || !strings.Contains(refusal, "min_head_age") {
			t.Fatalf("refusal %q should name the min_head_age gate", refusal)
		}
	})
}

func TestRelayMergeRefusalAllowsWhatTheLaneWouldMerge(t *testing.T) {
	fx := validContributorFixture()
	policy := defaultContributorLanePolicy()
	clock := clock0()
	engine, _ := relayGateEngine(t, &fx, policy, Options{ContributorSoak: soakedTracker(clock)})
	refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
	if err != nil || refusal != "" {
		t.Fatalf("refusal = %q, err = %v; a soaked, approved, verified-tier PR must pass", refusal, err)
	}
	if fx.merges != 0 {
		t.Fatalf("merges = %d, want 0: the relay performs the merge, not the gate", fx.merges)
	}
	if fx.updates != 0 {
		t.Fatalf("updates = %d, want 0", fx.updates)
	}
}

func TestRelayMergeRefusalHiveAuthoredAndTrustedAuthorsPassThrough(t *testing.T) {
	t.Run("App-authored keeps today's relay behaviour", func(t *testing.T) {
		fx := validContributorFixture()
		fx.author = testHiveAppBotLogin
		policy := defaultContributorLanePolicy()
		policy.Enabled = false // even with the lane off
		engine, _ := relayGateEngine(t, &fx, policy, Options{})
		if refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7"); err != nil || refusal != "" {
			t.Fatalf("refusal = %q, err = %v; want pass-through", refusal, err)
		}
	})
	t.Run("trusted bot author passes through", func(t *testing.T) {
		fx := validContributorFixture()
		fx.author = "dependabot[bot]"
		policy := defaultContributorLanePolicy()
		engine, _ := relayGateEngine(t, &fx, policy, Options{TrustedBotAuthors: func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }})
		if refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7"); err != nil || refusal != "" {
			t.Fatalf("refusal = %q, err = %v; want pass-through", refusal, err)
		}
	})
	t.Run("trusted-author role passes through", func(t *testing.T) {
		fx := validContributorFixture()
		fx.author = "operator"
		policy := defaultContributorLanePolicy()
		policy.Enabled = false
		engine, _ := relayGateEngine(t, &fx, policy, Options{
			TrustedAuthorPolicy: func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"} },
			TrustedAuthorizer: func(login, requireRole string) TrustedAuthorDecision {
				return TrustedAuthorDecision{Allowed: login == "operator" && requireRole == "merger", Role: "owner"}
			},
		})
		if refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7"); err != nil || refusal != "" {
			t.Fatalf("refusal = %q, err = %v; want pass-through", refusal, err)
		}
	})
}

func TestRelayMergeRefusalPolicyScope(t *testing.T) {
	t.Run("lane disabled refuses contributors", func(t *testing.T) {
		fx := validContributorFixture()
		policy := defaultContributorLanePolicy()
		policy.Enabled = false
		engine, _ := relayGateEngine(t, &fx, policy, Options{})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil || !strings.Contains(refusal, relayLaneReasonDisabled) || !strings.Contains(refusal, "contributor_lane.enabled") {
			t.Fatalf("refusal = %q, err = %v", refusal, err)
		}
	})
	t.Run("repo outside lane scope refuses", func(t *testing.T) {
		fx := validContributorFixture()
		policy := defaultContributorLanePolicy()
		policy.Repos = map[string]bool{"other": true}
		engine, _ := relayGateEngine(t, &fx, policy, Options{})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil || !strings.Contains(refusal, relayLaneReasonRepo) {
			t.Fatalf("refusal = %q, err = %v", refusal, err)
		}
	})
	t.Run("control labels refuse", func(t *testing.T) {
		for _, tc := range []struct{ label, want string }{
			{"hive-sentinel", contributorLaneReasonSentinelBlocked},
			{"hold", "label:hold"},
			{"do-not-merge/hold", "label:do-not-merge/hold"},
		} {
			fx := validContributorFixture()
			fx.labels = []string{tc.label}
			policy := defaultContributorLanePolicy()
			engine, clock := relayGateEngine(t, &fx, policy, Options{})
			_ = clock
			refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
			if err != nil || !strings.Contains(refusal, tc.want) {
				t.Fatalf("%s: refusal = %q, err = %v", tc.label, refusal, err)
			}
		}
	})
	t.Run("decision requested by the reviewer refuses", func(t *testing.T) {
		fx := validContributorFixture()
		policy := defaultContributorLanePolicy()
		clock := clock0()
		engine, _ := relayGateEngine(t, &fx, policy, Options{
			ContributorSoak: soakedTracker(clock),
			ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
				return ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "which API?"}
			},
		})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil || refusal == "" || fx.merges != 0 {
			t.Fatalf("refusal = %q, err = %v, merges = %d", refusal, err, fx.merges)
		}
	})
	t.Run("behind head is reported, never updated by the gate", func(t *testing.T) {
		fx := validContributorFixture()
		fx.mergeableState = "behind"
		policy := defaultContributorLanePolicy()
		engine, _ := relayGateEngine(t, &fx, policy, Options{})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err != nil || !strings.Contains(refusal, "behind") {
			t.Fatalf("refusal = %q, err = %v", refusal, err)
		}
		if fx.updates != 0 {
			t.Fatalf("updates = %d, want 0: the relay gate must not push", fx.updates)
		}
	})
	t.Run("expected head mismatch refuses", func(t *testing.T) {
		fx := validContributorFixture()
		policy := defaultContributorLanePolicy()
		clock := clock0()
		engine, _ := relayGateEngine(t, &fx, policy, Options{ContributorSoak: soakedTracker(clock)})
		refusal, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha-old")
		if err != nil || !strings.Contains(refusal, relayLaneReasonHead) {
			t.Fatalf("refusal = %q, err = %v", refusal, err)
		}
	})
}

func TestRelayMergeRefusalFailsClosed(t *testing.T) {
	t.Run("nil engine", func(t *testing.T) {
		var engine *Engine
		if _, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7"); err == nil {
			t.Fatal("nil engine must error")
		}
	})
	t.Run("PR fetch error", func(t *testing.T) {
		fx := validContributorFixture()
		fx.getStatus = http.StatusInternalServerError
		engine, _ := relayGateEngine(t, &fx, defaultContributorLanePolicy(), Options{})
		if _, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7"); err == nil || !strings.Contains(err.Error(), "fail closed") {
			t.Fatalf("err = %v, want fail-closed fetch error", err)
		}
	})
	t.Run("lane evidence error", func(t *testing.T) {
		fx := validContributorFixture()
		fx.filesStatus = http.StatusInternalServerError
		engine, _ := relayGateEngine(t, &fx, defaultContributorLanePolicy(), Options{})
		_, err := engine.RelayMergeRefusal(context.Background(), "widget", 7, "sha7")
		if err == nil || !strings.Contains(err.Error(), contributorLaneReasonFilesEvidence) {
			t.Fatalf("err = %v, want lane evidence error", err)
		}
	})
}

func TestRelayMergeRefusalText(t *testing.T) {
	policy := ContributorLanePolicy{Soak: 30 * time.Minute, RequireTiers: map[string]bool{"trusted": true, "merger": true}}
	got := relayMergeRefusal("Karibhou", "untrusted", policy, contributorLaneReasonClaimTier, 0)
	want := RelayMergeRefusalPrefix + ` for @Karibhou's PR: contributor-lane:claim-tier-untrusted (author tier "untrusted", require_tiers merger, trusted)`
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := relayMergeRefusal("", "", ContributorLanePolicy{}, contributorLaneReasonNoClaim, 0); !strings.Contains(got, `tier "none", require_tiers (none)`) {
		t.Fatalf("got %q", got)
	}
}
