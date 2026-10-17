package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A claim by a human principal replaces a hand-applied backend-named label
// with the login; an already-correct label is left alone (no write).
func TestApplyClaimedByLabel_ReplacesBackendNameWithLogin(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	fake.mu.Lock()
	fake.truth[12528] = []string{"enhancement", "hive/claimed-by-copilot"}
	fake.truth[12532] = []string{"hive/claimed-by-clubanderson"}
	fake.mu.Unlock()

	human := claims.Claim{Repo: "acme/widgets", Issue: 12528, Holder: "clubanderson", Kind: claims.KindHuman, Session: "copilot"}
	removed, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 12528, human)
	if err != nil {
		t.Fatal(err)
	}
	if !added || strings.Join(removed, ",") != "hive/claimed-by-copilot" {
		t.Fatalf("removed %v added %v, want copilot removed and login added", removed, added)
	}
	fake.mu.Lock()
	if got := strings.Join(fake.added[12528], ","); got != "hive/claimed-by-clubanderson" {
		t.Fatalf("added %q, want hive/claimed-by-clubanderson", got)
	}
	fake.mu.Unlock()

	human.Issue = 12532
	removed, added, err = c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 12532, human)
	if err != nil {
		t.Fatal(err)
	}
	if added || len(removed) != 0 {
		t.Fatalf("correct label must be a no-op; removed %v added %v", removed, added)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.added[12532])+len(fake.removed[12532]) != 0 {
		t.Fatalf("unexpected writes on an already-correct issue: +%v -%v", fake.added[12532], fake.removed[12532])
	}
}

// A hive-agent claim uses the agent name; a claim whose holder is a backend
// name never produces a backend-suffixed label.
func TestApplyClaimedByLabel_AgentAndBackendHolder(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	fake.mu.Lock()
	fake.truth[1] = []string{"hive/claimed-by-ann"}
	fake.truth[2] = []string{"hive/claimed-by-claude", "hive/claimed-by-ann"}
	fake.mu.Unlock()

	if _, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 1, claims.Claim{Holder: "scanner", Kind: claims.KindAgent}); err != nil || !added {
		t.Fatalf("agent claim: added=%v err=%v", added, err)
	}
	// A contributor claim with a backend holder has no principal: it only
	// strips the malformed label and must not disturb ann's.
	removed, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 2, claims.Claim{Holder: "codex", Kind: claims.KindContributor})
	if err != nil || added || strings.Join(removed, ",") != "hive/claimed-by-claude" {
		t.Fatalf("backend holder: removed %v added %v err %v", removed, added, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[1], ","); got != "hive/claimed-by-scanner" {
		t.Fatalf("agent label = %q", got)
	}
	if len(fake.removed[1]) != 0 {
		t.Fatalf("another party's live label must be kept, removed %v", fake.removed[1])
	}
	for _, l := range append(fake.added[1], fake.added[2]...) {
		if claims.ClaimedByLabelIsBackend(l) {
			t.Fatalf("backend name leaked into a label: %q", l)
		}
	}
	if len(fake.added[2]) != 0 {
		t.Fatalf("no label expected for a backend-named holder, added %v", fake.added[2])
	}
}

// A contributor claim on an issue carrying a human's live label adds its own
// and keeps the human's (one label per party).
func TestApplyClaimedByLabel_KeepsCoPartyLabel(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	fake.mu.Lock()
	fake.truth[4] = []string{"hive/claimed-by-clubanderson"}
	fake.mu.Unlock()

	removed, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 4, claims.Claim{Holder: "Karibhou", Kind: claims.KindContributor})
	if err != nil || !added || len(removed) != 0 {
		t.Fatalf("removed %v added %v err %v, want only an add", removed, added, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.added[4], ","); got != "hive/claimed-by-Karibhou" {
		t.Fatalf("added %q", got)
	}
	if len(fake.removed[4]) != 0 {
		t.Fatalf("co-party label removed: %v", fake.removed[4])
	}
}

// Release drops only the releasing holder's label.
func TestRemoveClaimedByLabel(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	if err := c.RemoveClaimedByLabel(context.Background(), "acme/widgets", 3, claims.Claim{Holder: "Karibhou", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveClaimedByLabel(context.Background(), "acme/widgets", 3, claims.Claim{Holder: "copilot", Kind: claims.KindHuman}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.removed[3], ","); got != "hive/claimed-by-Karibhou" {
		t.Fatalf("removed %q, want only the holder's label", got)
	}
}

// The issue reconciler heals a backend-named claimed-by label: swapped for
// the live holder's login when the issue is claimed, dropped otherwise, and
// a principal-named label is never touched.
func TestSyncIssueStageLabels_HealsBackendClaimedBy(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	result := &ActionableResult{
		Issues: IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 1, Labels: []string{"hive/claimed-by-pi", stagelabels.Actionable.Label()}}}),
		WorkDetailsByRepo: map[string]RepoWorkDetails{"acme/widgets": {Issues: []RepoWorkIssue{
			{Repo: "acme/widgets", Number: 2, Bucket: workBucketClaimed, ClaimKind: "human", ClaimedBy: "clubanderson",
				Labels: []string{"hive/claimed-by-copilot", stagelabels.Claimed.Label()}},
			{Repo: "acme/widgets", Number: 3, Bucket: workBucketClaimed, ClaimKind: "contributor", ClaimedBy: "Karibhou",
				Labels: []string{"hive/claimed-by-clubanderson", stagelabels.Claimed.Label()}},
		}}},
	}
	res := syncSeeded(fake, c, result, store, stageNow())
	if res.ClaimedByHealed != 2 {
		t.Fatalf("ClaimedByHealed = %d, want 2", res.ClaimedByHealed)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Join(fake.removed[1], ",") != "hive/claimed-by-pi" || len(fake.added[1]) != 0 {
		t.Fatalf("issue 1: -%v +%v, want only the backend label dropped", fake.removed[1], fake.added[1])
	}
	if strings.Join(fake.removed[2], ",") != "hive/claimed-by-copilot" || strings.Join(fake.added[2], ",") != "hive/claimed-by-clubanderson" {
		t.Fatalf("issue 2: -%v +%v, want copilot → clubanderson", fake.removed[2], fake.added[2])
	}
	if len(fake.removed[3])+len(fake.added[3]) != 0 {
		t.Fatalf("issue 3: a principal-named label must not be touched: -%v +%v", fake.removed[3], fake.added[3])
	}
}
