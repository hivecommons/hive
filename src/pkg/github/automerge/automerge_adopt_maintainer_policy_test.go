package automerge

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// governor.labels.stage.adoption.maintainer_policy (hivecommons/hive#14524):
// hive's own hosted spoke left PR #14524 — a maintainer's clanker PR, nudged
// twice, its hive gone 26h — at 072 every cycle with "author maintains the
// repository; not adopting". Owner decision: clanker PRs are adoptable on
// disconnect regardless of the owner's repo role (default adopt); ask and
// never remain opt-outs.

const maintainerLabels072 = "stage/072-returned-to-contributor"

func maintainerPolicyWith(policy string) StageLabelPolicy {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.AdoptionMaintainerPolicy = policy
	return p
}

func TestMaintainerPolicyDefaultsToAdopt(t *testing.T) {
	for in, want := range map[string]string{
		"": config.AdoptionMaintainerPolicyAdopt, "adopt": config.AdoptionMaintainerPolicyAdopt,
		" Ask ": config.AdoptionMaintainerPolicyAsk, "NEVER": config.AdoptionMaintainerPolicyNever,
		"bogus": config.AdoptionMaintainerPolicyAdopt,
	} {
		if got := (StageLabelPolicy{AdoptionMaintainerPolicy: in}).maintainerPolicy(); got != want {
			t.Errorf("maintainerPolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

// adopt (the default): the author's role is never read and a maintainer's
// clanker PR is adopted on the same clock as any contributor's.
func TestMaintainerPolicyAdoptTakesMaintainerPR(t *testing.T) {
	for _, policy := range []string{"", config.AdoptionMaintainerPolicyAdopt} {
		t.Run("policy="+policy, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(policy))
			defer done()
			h.adopt.permissions = map[string]string{"mallory": "admin"}
			h.adopt.allowMaintainerEdits(7, true)
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			h.clock.advance(2 * time.Hour)
			h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker"), "check-failing:unit")
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if !rec.AdoptedInPlace() || rec.AdoptReason != AdoptReasonOffline {
				t.Fatalf("maintainer's clanker PR not adopted under policy %q: %+v", policy, rec)
			}
			if h.adopt.permissionReads != 0 {
				t.Fatalf("permission read %d times; adopt must not consult the role", h.adopt.permissionReads)
			}
			if rec.AdoptionBlocked("aaa") != "" {
				t.Fatalf("adopted record still marked blocked: %+v", rec)
			}
		})
	}
}

// adopt never loosens the human-author rule: a PR without author/clanker
// is still not adopted.
func TestMaintainerPolicyAdoptKeepsHumanRule(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(config.AdoptionMaintainerPolicyAdopt))
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.adopt.allowMaintainerEdits(7, true)
	// No relay trailer and no registered presence: a person's own PR.
	h.sink.presence = &ContributorPresence{}
	human := func(labels ...string) *gh.PullRequest {
		pr := adoptPR(7, "aaa", labels...)
		pr.Body = gh.Ptr("Fixes the widget.\n")
		return pr
	}
	h.observe(human(), "check-failing:unit")
	h.clock.advance(3 * time.Hour)
	h.observe(human("stage/071-needs-author", "role/contributor", "author/human"), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "" || rec.NudgeCount != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("human-authored PR adopted: %+v", rec)
	}
}

// adopt also keeps an existing maintainer adoption: the release-on-role
// paths only fire under never.
func TestMaintainerPolicyAdoptKeepsLiveAdoption(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(config.AdoptionMaintainerPolicyAdopt))
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker"), "check-failing:unit")
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() {
		t.Fatalf("maintainer's adoption released under adopt: %+v", rec)
	}
}

// ask: a maintainer's due adoption is held and recorded as blocked
// (author-is-maintainer) for the tracker; the owner's Adopt — the
// adoption/supersede request label — lifts the hold on the next pass.
func TestMaintainerPolicyAskHoldsThenOwnerAdopts(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(config.AdoptionMaintainerPolicyAsk))
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "maintain"}
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker"), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedHead != "" || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("ask adopted on its own: %+v", rec)
	}
	if got := rec.AdoptionBlocked("aaa"); got != stagelabels.AdoptBlockedAuthorMaintainer {
		t.Fatalf("blocked reason = %q, want %q (%+v)", got, stagelabels.AdoptBlockedAuthorMaintainer, rec)
	}
	if rec.AdoptBlockedAt.IsZero() || rec.AdoptionBlocked("bbb") != "" {
		t.Fatalf("blocked stamp: %+v", rec)
	}
	// A non-maintainer under ask is adopted as usual.
	h.adopt.permissions = map[string]string{"mallory": "write"}
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker"), "check-failing:unit")
	if rec, _ = h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() {
		t.Fatalf("write-level author not adopted under ask: %+v", rec)
	}
}

func TestMaintainerPolicyAskOwnerRequestLiftsHold(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(config.AdoptionMaintainerPolicyAsk))
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker"), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptionBlocked("aaa") == "" {
		t.Fatalf("setup: not held: %+v", rec)
	}
	// The owner clicks Adopt: the dashboard sets adoption/supersede.
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedHead != "aaa" {
		t.Fatalf("owner's Adopt did not adopt: %+v", rec)
	}
	if rec.AdoptionBlocked("aaa") != "" {
		t.Fatalf("blocked mark survived the adoption: %+v", rec)
	}
	// And the adoption is kept on later passes (only never releases).
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	if rec, _ = h.e.stageStoreOrShared().Get(key); rec.AdoptedHead != "aaa" {
		t.Fatalf("owner-requested adoption released under ask: %+v", rec)
	}
}

// never: the pre-#14524 rule, held and released; the tracker sees no
// owner action (no blocked mark is written, the policy is not ask).
func TestMaintainerPolicyNeverHoldsWithoutAsking(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, maintainerPolicyWith(config.AdoptionMaintainerPolicyNever))
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", maintainerLabels072, "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedHead != "" {
		t.Fatalf("never adopted a maintainer (even on request): %+v", rec)
	}
	if got := rec.AdoptionBlocked("aaa"); got != stagelabels.AdoptBlockedAuthorMaintainer {
		t.Fatalf("never should still record why for the row: %q (%+v)", got, rec)
	}
}
