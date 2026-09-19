package review

import (
	"strings"
	"testing"
	"time"
)

// Combined dispatch is the point of the feature: one kick, every outstanding
// perspective, one pending entry per perspective so the collector's view of
// "what has this head received" is unchanged.
func TestCombinedDispatchIsOneKickForEveryPerspective(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	prs := []PullRequest{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Author: "hive-bot[bot]"}}
	opts := DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 5,
		CombinedPerspectives: true, PostComments: true, AcknowledgeNoFindings: true,
		MaxPerspectivesPerPR: 1, // must NOT narrow a combined review
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
		AIAuthor:             "hive", Now: now,
	}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, opts)

	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d, want exactly 1", len(plan.ReviewKicks))
	}
	k := plan.ReviewKicks[0]
	if len(k.Perspectives) != len(DefaultPerspectives) {
		t.Fatalf("kick covers %v, want all %d", k.Perspectives, len(DefaultPerspectives))
	}
	if k.Perspective != DefaultPerspectives[0] {
		t.Fatalf("kick.Perspective = %q, want first perspective for legacy consumers", k.Perspective)
	}
	if len(plan.State.Pending) != len(DefaultPerspectives) {
		t.Fatalf("pending = %d, want one per perspective", len(plan.State.Pending))
	}
	for _, want := range []string{"ALL 5 perspectives", "JSON ARRAY", "exactly ONE comment", "security —", "docs-currency —", "Read widely; report narrowly"} {
		if !strings.Contains(k.Message, want) {
			t.Errorf("kick missing %q", want)
		}
	}
	if strings.Count(k.Message, "hive-review 7 --repo acme/hive --comment") != 1 {
		t.Errorf("combined kick should name the publish command once:\n%s", k.Message)
	}

	// Second cycle: nothing outstanding, nothing dispatched.
	again := PlanDispatch(prs, Artifact{}, plan.State, opts)
	if len(again.ReviewKicks) != 0 {
		t.Fatalf("re-dispatched a PR whose perspectives are all pending: %d kicks", len(again.ReviewKicks))
	}
}

// One combined kick costs one parallel slot, so a deep queue is still reviewed
// breadth-first — which was the whole reason for the per-PR cap the combined
// path bypasses.
func TestCombinedDispatchSpendsOneSlotPerPR(t *testing.T) {
	prs := []PullRequest{
		{Repo: "acme/hive", Number: 1, HeadSHA: "a", Author: "hive-bot[bot]"},
		{Repo: "acme/hive", Number: 2, HeadSHA: "b", Author: "hive-bot[bot]"},
		{Repo: "acme/hive", Number: 3, HeadSHA: "c", Author: "hive-bot[bot]"},
	}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 2, CombinedPerspectives: true,
		Agents: []AgentCapability{reviewer("r1")}, AIAuthor: "hive",
	})
	if len(plan.ReviewKicks) != 2 {
		t.Fatalf("kicks = %d, want 2 (slot budget)", len(plan.ReviewKicks))
	}
	if plan.ReviewKicks[0].Number != 1 || plan.ReviewKicks[1].Number != 2 {
		t.Fatalf("kicks did not go breadth-first: %+v", plan.ReviewKicks)
	}
}

// The configured set, not the built-in one, decides what a PR is reviewed for
// — in both dispatch modes.
func TestDispatchHonoursConfiguredPerspectives(t *testing.T) {
	set, err := NewPerspectiveSet([]string{"security", "api-compat"}, map[string]string{"api-compat": "public API breakage"})
	if err != nil {
		t.Fatal(err)
	}
	prs := []PullRequest{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Author: "hive-bot[bot]"}}
	base := DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 5, Perspectives: set,
		Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}, AIAuthor: "hive"}

	t.Run("combined", func(t *testing.T) {
		o := base
		o.CombinedPerspectives = true
		plan := PlanDispatch(prs, Artifact{}, DispatchState{}, o)
		if len(plan.ReviewKicks) != 1 || len(plan.ReviewKicks[0].Perspectives) != 2 {
			t.Fatalf("kicks = %+v", plan.ReviewKicks)
		}
		msg := plan.ReviewKicks[0].Message
		if !strings.Contains(msg, "api-compat — public API breakage") || strings.Contains(msg, "correctness —") {
			t.Fatalf("kick did not use the configured set:\n%s", msg)
		}
	})
	t.Run("per-perspective", func(t *testing.T) {
		plan := PlanDispatch(prs, Artifact{}, DispatchState{}, base)
		if len(plan.ReviewKicks) != 2 {
			t.Fatalf("kicks = %d, want 2", len(plan.ReviewKicks))
		}
		for _, k := range plan.ReviewKicks {
			if k.Perspective != PerspectiveSecurity && k.Perspective != "api-compat" {
				t.Fatalf("dispatched unconfigured perspective %q", k.Perspective)
			}
		}
	})
}

// The single-perspective kick must read the configured focus too, or a hive's
// style guidance would apply only when combined mode is on.
func TestPerspectivePromptUsesConfiguredFocus(t *testing.T) {
	set, _ := NewPerspectiveSet(nil, map[string]string{"style": "every exported symbol has a doc comment"})
	got := BuildPerspectivePromptWith(PerspectiveStyle, PullRequest{Repo: "o/r", Number: 1}, PromptOptions{Perspectives: set})
	if !strings.Contains(got, "Focus ONLY on every exported symbol has a doc comment.") {
		t.Fatalf("configured focus not used:\n%s", got)
	}
}

// Every perspective must get its own verdict; a clean one is still a verdict.
// A silently-omitted perspective reads downstream as never reviewed.
func TestCombinedPromptDemandsEveryVerdict(t *testing.T) {
	got := BuildCombinedPrompt(PullRequest{Repo: "o/r", Number: 1}, nil, PromptOptions{PostComments: true, AcknowledgeNoFindings: true})
	for _, want := range []string{
		"even the ones that found nothing",
		"A perspective you could not meaningfully assess is requires_human, not approve",
		"no findings from correctness, security, intent-alignment, style, docs-currency.",
		"_No findings from:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// A combined kick that never reaches the agent must release EVERY perspective
// it covered, not just the first. Before this, a busy reviewer turned one
// missed kick into three perspectives stuck "pending" forever — never
// re-dispatched, never judged — while the first was re-sent on its own.
func TestConfirmDeliveredReleasesEveryPerspectiveOfAnUndeliveredCombinedKick(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 7, HeadSHA: "abc"}
	kick := DispatchKick{Kind: "review", Agent: "reviewer", Repo: "o/r", Number: 7, HeadSHA: "abc",
		Perspective: PerspectiveSecurity, Perspectives: []Perspective{PerspectiveSecurity, PerspectiveStyle, PerspectiveDocsCurrency}}
	var state DispatchState
	for _, p := range kick.Perspectives {
		state.Pending = append(state.Pending, PendingReview{Repo: "o/r", Number: 7, HeadSHA: "abc", Perspective: p, Agent: "reviewer"})
	}

	got := ConfirmDelivered(state, []DispatchKick{kick}, nil)
	if len(got.Pending) != 0 {
		t.Fatalf("undelivered combined kick left pending entries: %+v", got.Pending)
	}
	if missing := pendingMissingPerspectives(got, pr, kick.Perspectives); len(missing) != 3 {
		t.Fatalf("next cycle should re-dispatch all three, got %v", missing)
	}

	// Delivered: pending stays, and Recent records each perspective so the
	// relay can bind a late verdict for any of them.
	got = ConfirmDelivered(state, []DispatchKick{kick}, []DispatchKick{kick})
	if len(got.Pending) != 3 {
		t.Fatalf("delivered kick lost pending entries: %+v", got.Pending)
	}
	seen := map[Perspective]bool{}
	for _, r := range got.Recent {
		seen[r.Perspective] = true
	}
	for _, p := range kick.Perspectives {
		if !seen[p] {
			t.Fatalf("recent lacks %s: %+v", p, got.Recent)
		}
	}
}
