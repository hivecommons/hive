package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

// hivecommons/hive#13579: a 🔒 marker this hive wrote is believed only while
// its own ledger still holds the claim. The relay's disconnect release ended
// the claim in the ledger; the forge marker's until= was still an hour out,
// and the governor read the forge first — so hive agents stayed withheld.
func TestIssueClaimFor_OwnHiveMarkerReleasedInLedgerIsNotLive(t *testing.T) {
	marker := fmt.Sprintf("<!-- hive:claim who=repires kind=contributor id=c-repires hive=hosted tier=contributor verified=true at=%s until=%s -->",
		claimNow.Add(-10*time.Minute).UTC().Format(time.RFC3339), claimNow.Add(50*time.Minute).UTC().Format(time.RFC3339))
	server, _ := claimServer(t, []string{marker})
	c := newTestClient(t, server, "acme", []string{"widgets"})
	enableClaims(c, issueclaim.DefaultTTL)

	held := true
	c.SetIssueClaimLedger(func(repo string, number int) (issueclaim.Claim, bool) {
		if !held {
			return issueclaim.Claim{}, false
		}
		return issueclaim.Claim{Identity: "repires", Kind: "contributor", ExpiresAt: claimNow.Add(50 * time.Minute), Source: issueclaim.SourceLease}, true
	})

	// Without the hive guard installed the marker is believed as before.
	issue := claimIssue()
	claim, found, err := c.issueClaimFor(context.Background(), "acme", "widgets", &issue, issueclaim.DefaultTTL, claimNow)
	if err != nil || !found || claim.Identity != "repires" {
		t.Fatalf("baseline: claim=%+v found=%v err=%v", claim, found, err)
	}

	hive := "hosted"
	c.SetIssueClaimLedgerHive(func() string { return hive })
	// Ledger still holds it: the marker stands.
	claim, found, err = c.issueClaimFor(context.Background(), "acme", "widgets", &issue, issueclaim.DefaultTTL, claimNow)
	if err != nil || !found || claim.Source != issueclaim.SourceMarker {
		t.Fatalf("held in ledger: claim=%+v found=%v err=%v", claim, found, err)
	}
	// Ledger released it: this hive's own marker no longer counts.
	held = false
	claim, found, err = c.issueClaimFor(context.Background(), "acme", "widgets", &issue, issueclaim.DefaultTTL, claimNow)
	if err != nil || found {
		t.Fatalf("released in ledger: marker still read as live: claim=%+v found=%v err=%v", claim, found, err)
	}
	// Another hive's marker is never second-guessed by this ledger.
	hive = "other-hive"
	claim, found, err = c.issueClaimFor(context.Background(), "acme", "widgets", &issue, issueclaim.DefaultTTL, claimNow)
	if err != nil || !found || claim.Identity != "repires" {
		t.Fatalf("other hive's marker: claim=%+v found=%v err=%v", claim, found, err)
	}
	// No ledger yet (boot): guard off.
	hive = ""
	claim, found, err = c.issueClaimFor(context.Background(), "acme", "widgets", &issue, issueclaim.DefaultTTL, claimNow)
	if err != nil || !found {
		t.Fatalf("empty hive: claim=%+v found=%v err=%v", claim, found, err)
	}
}

func guardIssue(number int, kind string, prStarted bool, expires time.Time) Issue {
	return Issue{Repo: "hivecommons/hive", Number: number, Title: "t", ClaimedBy: "repires", ClaimExpiresAt: &expires,
		ClaimSource: issueclaim.SourceMarker, ClaimKind: kind, ClaimPRStarted: prStarted}
}

// A nil guard is the pre-#13579 filter: every live claim withholds.
func TestContributorClaimGuard_NilWithholdsEverything(t *testing.T) {
	now := claimNow
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{
		guardIssue(1, ClaimKindContributor, false, now.Add(time.Hour)),
		guardIssue(2, "agent", false, now.Add(time.Hour)),
	})}
	var g *ContributorClaimGuard
	if n := g.FilterLiveIssueClaims(result, now, testLogger()); n != 2 {
		t.Fatalf("withheld %d, want 2", n)
	}
	if _, ok := g.Overridden("hivecommons/hive", 1); ok {
		t.Fatal("nil guard overrides nothing")
	}
}

// An issue the contribute hub has put in contributor cool-off stays
// actionable for hive agents despite the live contributor claim, carries the
// cool-off clause in its work detail, and is reported Overridden so the
// scheduler's inflight lookup does not re-withhold it.
func TestContributorClaimGuard_CoolOffMakesClaimAdvisory(t *testing.T) {
	now := claimNow
	coolOff := map[int]string{13579: "contributor cool-off: 3 drops in 2h0m0s"}
	g := NewContributorClaimGuard(4*time.Hour, func(repo string, number int) (string, bool) {
		r, ok := coolOff[number]
		return r, ok
	})
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{
		guardIssue(13579, ClaimKindContributor, false, now.Add(time.Hour)),
		guardIssue(2, ClaimKindContributor, false, now.Add(time.Hour)),
		guardIssue(3, "agent", false, now.Add(time.Hour)),
	})}
	if n := g.FilterLiveIssueClaims(result, now, testLogger()); n != 2 {
		t.Fatalf("withheld %d, want 2 (#2 contributor-claimed, #3 agent-claimed)", n)
	}
	if len(result.Issues.Items) != 1 || result.Issues.Items[0].Number != 13579 {
		t.Fatalf("kept %+v, want #13579 only", result.Issues.Items)
	}
	if adv := result.Issues.Items[0].ClaimAdvisory; !strings.Contains(adv, "contributor cool-off: 3 drops") {
		t.Fatalf("ClaimAdvisory = %q", adv)
	}
	if reason, ok := g.Overridden("hivecommons/hive", 13579); !ok || !strings.Contains(reason, "cool-off") {
		t.Fatalf("Overridden = %q, %v", reason, ok)
	}
	if _, ok := g.Overridden("hivecommons/hive", 2); ok {
		t.Fatal("#2 was withheld, not overridden")
	}
	var advisory, withheld int
	for _, d := range result.WorkDetailsByRepo["hivecommons/hive"].Issues {
		switch {
		case d.Number == 13579 && d.Bucket == "actionable" && strings.Contains(d.Reason, "contributor cool-off: 3 drops in 2h0m0s"):
			advisory++
		case d.Bucket == "claimed" && strings.HasPrefix(d.Reason, "Live issue claim by repires"):
			withheld++
		}
	}
	if advisory != 1 || withheld != 2 {
		t.Fatalf("work details: advisory=%d withheld=%d: %+v", advisory, withheld, result.WorkDetailsByRepo["hivecommons/hive"].Issues)
	}

	// Cool-off is also surfaced on an issue that is withheld by a hive
	// agent's claim, and on an unclaimed issue, so operators see it.
	coolOff[3] = "contributor cool-off: 4 drops in 2h0m0s"
	coolOff[4] = "contributor cool-off: 3 drops in 2h0m0s"
	result = &ActionableResult{Issues: IssueResultFromItems([]Issue{
		guardIssue(3, "agent", false, now.Add(time.Hour)),
		{Repo: "hivecommons/hive", Number: 4},
	})}
	if n := g.FilterLiveIssueClaims(result, now, testLogger()); n != 1 {
		t.Fatalf("withheld %d, want 1", n)
	}
	var sawAgent, sawFree bool
	for _, d := range result.WorkDetailsByRepo["hivecommons/hive"].Issues {
		if d.Number == 3 && d.Bucket == "claimed" && strings.Contains(d.Reason, "; contributor cool-off: 4 drops") {
			sawAgent = true
		}
		if d.Number == 4 && d.Bucket == "actionable" && strings.Contains(d.Reason, "contributor cool-off: 3 drops") {
			sawFree = true
		}
	}
	if !sawAgent || !sawFree {
		t.Fatalf("cool-off clause missing: agent=%v free=%v: %+v", sawAgent, sawFree, result.WorkDetailsByRepo["hivecommons/hive"].Issues)
	}
}

// Starvation: contributor claims that keep hive agents off an issue for the
// configured window with no PR become advisory. The clock is injected; the
// first-withheld time resets when a pass sees the issue unclaimed.
func TestContributorClaimGuard_StarvationMakesClaimAdvisory(t *testing.T) {
	start := claimNow
	g := NewContributorClaimGuard(4*time.Hour, nil)
	pass := func(now time.Time, issues ...Issue) *ActionableResult {
		result := &ActionableResult{Issues: IssueResultFromItems(issues)}
		g.FilterLiveIssueClaims(result, now, testLogger())
		return result
	}
	// Each flap renews the claim an hour out; the issue is withheld on
	// every pass for 3h59m.
	for _, at := range []time.Duration{0, time.Hour, 2 * time.Hour, 3*time.Hour + 59*time.Minute} {
		r := pass(start.Add(at), guardIssue(13579, ClaimKindContributor, false, start.Add(at+time.Hour)))
		if len(r.Issues.Items) != 0 {
			t.Fatalf("at +%s the issue should still be withheld", at)
		}
	}
	if first, ok := g.WithheldSince("hivecommons/hive", 13579); !ok || !first.Equal(start) {
		t.Fatalf("WithheldSince = %v, %v; want %v", first, ok, start)
	}
	r := pass(start.Add(4*time.Hour), guardIssue(13579, ClaimKindContributor, false, start.Add(5*time.Hour)))
	if len(r.Issues.Items) != 1 {
		t.Fatal("at +4h the contributor claim must become advisory")
	}
	if adv := r.Issues.Items[0].ClaimAdvisory; !strings.Contains(adv, "withheld this issue from hive agents for 4h0m0s with no PR") {
		t.Fatalf("ClaimAdvisory = %q", adv)
	}
	if _, ok := g.Overridden("hivecommons/hive", 13579); !ok {
		t.Fatal("starved issue must be reported Overridden")
	}
	details := r.WorkDetailsByRepo["hivecommons/hive"].Issues
	if len(details) != 1 || details[0].Bucket != "actionable" || !strings.Contains(details[0].Reason, "advisory") {
		t.Fatalf("work detail = %+v", details)
	}

	// A claim WITH PR evidence is never starved out: somebody is shipping.
	r = pass(start.Add(9*time.Hour), guardIssue(13579, ClaimKindContributor, true, start.Add(10*time.Hour)))
	if len(r.Issues.Items) != 0 {
		t.Fatal("a contributor claim with pr_started must still withhold")
	}
	// Agent claims are never advisory however long they run.
	for _, at := range []time.Duration{0, 5 * time.Hour, 10 * time.Hour} {
		if r := pass(start.Add(at), guardIssue(99, "agent", false, start.Add(at+time.Hour))); len(r.Issues.Items) != 0 {
			t.Fatalf("agent claim at +%s became advisory", at)
		}
	}
	// A pass that sees the issue free resets the clock.
	pass(start.Add(11*time.Hour), Issue{Repo: "hivecommons/hive", Number: 13579})
	if _, ok := g.WithheldSince("hivecommons/hive", 13579); ok {
		t.Fatal("first-withheld must be dropped once the issue is seen unclaimed")
	}
	if _, ok := g.Overridden("hivecommons/hive", 13579); ok {
		t.Fatal("override must not outlive the pass that granted it")
	}
	r = pass(start.Add(12*time.Hour), guardIssue(13579, ClaimKindContributor, false, start.Add(13*time.Hour)))
	if len(r.Issues.Items) != 0 {
		t.Fatal("a fresh contributor claim starts a fresh starvation clock")
	}

	// Starvation disabled: never advisory.
	off := NewContributorClaimGuard(0, nil)
	result := &ActionableResult{Issues: IssueResultFromItems([]Issue{guardIssue(1, ClaimKindContributor, false, start.Add(time.Hour))})}
	off.FilterLiveIssueClaims(result, start, nil)
	result = &ActionableResult{Issues: IssueResultFromItems([]Issue{guardIssue(1, ClaimKindContributor, false, start.Add(100*time.Hour))})}
	if off.FilterLiveIssueClaims(result, start.Add(99*time.Hour), nil) != 1 {
		t.Fatal("starvation off must keep withholding")
	}
}
