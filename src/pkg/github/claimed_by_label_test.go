package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A claim by a human principal replaces a hand-applied backend-named label
// and its own retired per-login label with the generic hive/claimed-by-human
// (hivecommons/hive: no login in a label); an already-correct label is left
// alone (no write).
func TestApplyClaimedByLabel_GenericHumanReplacesBackendAndLogin(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	fake.mu.Lock()
	fake.truth[12528] = []string{"enhancement", "hive/claimed-by-copilot", "hive/claimed-by-clubanderson"}
	fake.truth[12532] = []string{claims.ClaimedByHumanLabel}
	fake.mu.Unlock()

	human := claims.Claim{Repo: "acme/widgets", Issue: 12528, Holder: "clubanderson", Kind: claims.KindHuman, Session: "copilot"}
	removed, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 12528, human)
	if err != nil {
		t.Fatal(err)
	}
	if !added || strings.Join(removed, ",") != "hive/claimed-by-copilot,hive/claimed-by-clubanderson" {
		t.Fatalf("removed %v added %v, want copilot and per-login removed and the generic label added", removed, added)
	}
	fake.mu.Lock()
	if got := strings.Join(fake.added[12528], ","); got != "hive/claimed-by-human" {
		t.Fatalf("added %q, want hive/claimed-by-human", got)
	}
	for _, l := range fake.added[12528] {
		if strings.Contains(strings.ToLower(l), "clubanderson") {
			t.Fatalf("login leaked into label %q", l)
		}
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

// A contributor claim on an issue carrying ANOTHER person's retired
// per-login label adds the generic label and keeps the other party's (one
// label per party; only the claim's own legacy spelling is swapped).
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
	if got := strings.Join(fake.added[4], ","); got != claims.ClaimedByHumanLabel {
		t.Fatalf("added %q", got)
	}
	if len(fake.removed[4]) != 0 {
		t.Fatalf("co-party label removed: %v", fake.removed[4])
	}
}

// A new agent claim heals a missed human-label release, even after posting
// its own newer marker. Absence, expiry, and unrelated releases are not
// sufficient evidence to remove a co-party's label (#14239).
func TestApplyClaimedByLabel_ReleasedHuman(t *testing.T) {
	const human = "<!-- hive:claim who=ann kind=contributor at=2026-10-25T12:00:00Z until=2026-10-25T12:30:00Z -->"
	const release = "<!-- hive:release who=ann kind=contributor reason=lease_revoked at=2026-10-25T12:01:00Z -->"
	const agent = "<!-- hive:claim who=scanner kind=agent at=2026-10-25T12:02:00Z until=2026-10-25T12:32:00Z -->"
	for _, tc := range []struct {
		name   string
		bodies []string
		remove bool
	}{
		{"matching release then agent", []string{human, release, agent}, true},
		{"matching release without agent comment", []string{human, release}, true},
		{"legacy release", []string{human, "<!-- hive:release who=ann -->", agent}, true},
		{"no evidence", nil, false},
		{"release alone", []string{release, agent}, false},
		{"unreleased even if expired", []string{human, agent}, false},
		{"different holder", []string{human, "<!-- hive:release who=bob at=2026-10-25T12:01:00Z -->", agent}, false},
		{"older release", []string{"<!-- hive:release who=ann at=2026-10-25T11:59:00Z -->", human, agent}, false},
		{"legacy release before claim", []string{"<!-- hive:release who=ann -->", human, agent}, false},
		{"renewed human", []string{human, release, "<!-- hive:claim who=ann kind=human at=2026-10-25T12:01:30Z until=2026-10-25T12:31:30Z -->", agent}, false},
		{"another human", []string{human, release, "<!-- hive:claim who=bob kind=human at=2026-10-25T12:01:30Z until=2026-10-25T12:31:30Z -->", agent}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newIssueStageServer(t)
			c := issueStageTestClient(t, fake.srv, true)
			fake.truth[7] = []string{"bug", claims.ClaimedByHumanLabel, "hive/claimed-by-reviewer"}
			fake.commentBodies[7] = tc.bodies
			removed, added, err := c.ApplyClaimedByLabel(context.Background(), "acme/widgets", 7, claims.Claim{Holder: "scanner", Kind: claims.KindAgent})
			if err != nil || !added {
				t.Fatalf("added=%v err=%v", added, err)
			}
			wantRemoved := ""
			if tc.remove {
				wantRemoved = claims.ClaimedByHumanLabel
			}
			if got := strings.Join(removed, ","); got != wantRemoved {
				t.Fatalf("removed=%q, want %q", got, wantRemoved)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if got := strings.Join(fake.removed[7], ","); got != wantRemoved {
				t.Fatalf("label writes removed=%q, want %q", got, wantRemoved)
			}
			if got := strings.Join(fake.added[7], ","); got != "hive/claimed-by-scanner" {
				t.Fatalf("added=%q", got)
			}
		})
	}
}

// Release drops only the releasing holder's labels: the generic one plus
// the holder's retired per-login spelling; a backend-named "holder" has no
// label to drop and an agent claim drops only its own.
func TestRemoveClaimedByLabel(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	if err := c.RemoveClaimedByLabel(context.Background(), "acme/widgets", 3, claims.Claim{Holder: "Karibhou", Kind: claims.KindContributor}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveClaimedByLabel(context.Background(), "acme/widgets", 3, claims.Claim{Holder: "copilot", Kind: claims.KindHuman}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveClaimedByLabel(context.Background(), "acme/widgets", 5, claims.Claim{Holder: "scanner", Kind: claims.KindAgent}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if got := strings.Join(fake.removed[3], ","); got != "hive/claimed-by-human,hive/claimed-by-Karibhou" {
		t.Fatalf("removed %q, want the generic label and the holder's legacy label", got)
	}
	if got := strings.Join(fake.removed[5], ","); got != "hive/claimed-by-scanner" {
		t.Fatalf("agent removed %q, want only its own label", got)
	}
}

// The issue reconciler heals a backend-named claimed-by label: swapped for
// the live holder's label (generic human) when the issue is claimed, dropped
// otherwise. The live human holder's retired per-login label is swapped the
// same way; another party's label is never touched.
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
			{Repo: "acme/widgets", Number: 4, Bucket: workBucketClaimed, ClaimKind: "human", ClaimedBy: "clubanderson",
				Labels: []string{"hive/claimed-by-clubanderson", stagelabels.Claimed.Label()}},
			{Repo: "acme/widgets", Number: 5, Bucket: workBucketClaimed, ClaimKind: "agent", ClaimedBy: "scanner", ClaimPRStarted: true,
				Labels: []string{"hive/claimed-by-scanner", stagelabels.PRInProgress.Label()}},
		}}},
	}
	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.ClaimedByHealed != 3 {
		t.Fatalf("ClaimedByHealed = %d, want 3", res.ClaimedByHealed)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Join(fake.removed[1], ",") != "hive/claimed-by-pi" || len(fake.added[1]) != 0 {
		t.Fatalf("issue 1: -%v +%v, want only the backend label dropped", fake.removed[1], fake.added[1])
	}
	if strings.Join(fake.removed[2], ",") != "hive/claimed-by-copilot" || strings.Join(fake.added[2], ",") != "hive/claimed-by-human" {
		t.Fatalf("issue 2: -%v +%v, want copilot → human", fake.removed[2], fake.added[2])
	}
	if len(fake.removed[3])+len(fake.added[3]) != 0 {
		t.Fatalf("issue 3: another party's label must not be touched: -%v +%v", fake.removed[3], fake.added[3])
	}
	if strings.Join(fake.removed[4], ",") != "hive/claimed-by-clubanderson" || strings.Join(fake.added[4], ",") != "hive/claimed-by-human" {
		t.Fatalf("issue 4: -%v +%v, want the live holder's per-login label → human", fake.removed[4], fake.added[4])
	}
	if len(fake.removed[5])+len(fake.added[5]) != 0 {
		t.Fatalf("issue 5: an agent's own label must not be touched: -%v +%v", fake.removed[5], fake.added[5])
	}
}
