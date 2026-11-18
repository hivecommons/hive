package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// spellingClient is a hive configured with bare repo names in one org, the
// default config shape the claim ledger misread (projectbluefin/actions#748).
func spellingClient(t *testing.T, repos ...string) *Client {
	t.Helper()
	return NewClientForTest("http://127.0.0.1:1", "projectbluefin", repos, testLogger())
}

func spellingLedger(t *testing.T, c *Client) *ClaimLedger {
	t.Helper()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), testLogger())
	ledger.SetRepoCanonicalizer(c.canonicalRepo)
	return ledger
}

// The owner counts: a bare configured name means the hive's org, so another
// org's repo of the same name is never folded onto it.
func TestClientCanonicalRepoIsOwnerAware(t *testing.T) {
	c := spellingClient(t, "actions", "otherorg/tools")
	for in, want := range map[string]string{
		"actions":                "actions",
		"projectbluefin/actions": "actions",
		"ProjectBluefin/Actions": "actions",
		"otherorg/actions":       "otherorg/actions",
		"otherorg/tools":         "otherorg/tools",
		"OtherOrg/Tools":         "otherorg/tools",
		"tools":                  "tools",
		"projectbluefin/unknown": "projectbluefin/unknown",
	} {
		if got := c.canonicalRepo(in); got != want {
			t.Errorf("canonicalRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

// projectbluefin/actions#748: the verdict path records the hub task repo
// ("projectbluefin/actions") while the enumeration looks the issue up as
// "actions". The claim must be found, and the claimed-by-PR filter must
// bucket the issue, so the stage sweep stops writing 010 under it.
func TestVerdictClaimOwnerQualifiedFoundByConfiguredSpelling(t *testing.T) {
	c := spellingClient(t, "actions")
	ledger := spellingLedger(t, c)
	if err := ledger.Record(IssueClaim{
		Repo: "projectbluefin/actions", Issue: 748, PRNumber: 731, PRRepo: "projectbluefin/actions",
		PRAuthor: "kylerankin", PRState: PRStateOpen, ExternalAuthor: true,
		Source: ClaimSourceVerdict, SourceReporter: "mendezr",
	}); err != nil {
		t.Fatal(err)
	}
	if ledger.Len() != 1 {
		t.Fatalf("ledger holds %d claims, want 1", ledger.Len())
	}
	for _, repo := range []string{"actions", "projectbluefin/actions"} {
		claim, ok := ledger.Lookup(repo, 748)
		if !ok || claim.PRNumber != 731 || claim.Repo != "actions" {
			t.Fatalf("Lookup(%q, 748) = %+v, %v; want the #731 claim keyed by the configured spelling", repo, claim, ok)
		}
	}

	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "actions", Number: 748, Title: "[shared-ci] tracker"}}, Count: 1}}
	if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 1 || len(result.Issues.Items) != 0 {
		t.Fatalf("filter suppressed %d, kept %+v; want the issue bucketed claimed_by_pr", got, result.Issues.Items)
	}
}

// projectbluefin/utah#1236: a PR in another watched repo saying "Part of
// owner/repo#N" claims the issue under its owner-qualified name. It is found
// by the configured spelling; the same text naming another org's repo is not
// a claim on the hive's issue.
func TestCrossRepoReferenceClaimFoundByConfiguredSpelling(t *testing.T) {
	c := spellingClient(t, "utah", "utah-packages")
	now := time.Now()
	claimsFor := func(body string) []IssueClaim {
		pr := &gh.PullRequest{
			Number: gh.Ptr(1017), Title: gh.Ptr("Translate the BuildStream elements"), Body: gh.Ptr(body),
			State: gh.Ptr(PRStateOpen), User: &gh.User{Login: gh.Ptr("contributor")},
			HTMLURL: gh.Ptr("https://github.com/projectbluefin/utah-packages/pull/1017"),
		}
		claims := claimsFromPR(pr, "utah-packages", HiveIdentity{AppLogin: "hive[bot]"}, now)
		for i := range claims {
			claims[i].PRState = PRStateOpen
		}
		return claims
	}

	ledger := spellingLedger(t, c)
	ledger.Reconcile(claimsFor("Part of the BuildStream→RPM translation for projectbluefin/utah#1236"), true)
	if claim, ok := ledger.Lookup("utah", 1236); !ok || claim.PRNumber != 1017 || claim.Repo != "utah" {
		t.Fatalf("Lookup(utah, 1236) = %+v, %v; want the utah-packages#1017 reference claim", claim, ok)
	}

	foreign := spellingLedger(t, c)
	foreign.Reconcile(claimsFor("Part of otherorg/utah#1236"), true)
	if claim, ok := foreign.Lookup("utah", 1236); ok {
		t.Fatalf("another org's utah#1236 claimed the hive's issue: %+v", claim)
	}
}

// A ledger saved before the fix holds owner-qualified keys. Setting the
// canonicalizer re-keys them once, keeps the higher-ranked claim when both
// spellings hold one, moves the churn history too, and the next save writes
// the configured spelling.
func TestClaimLedgerRekeysOwnerQualifiedEntries(t *testing.T) {
	c := spellingClient(t, "actions")
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Now().UTC()
	file := ledgerFile{
		SavedAt: now,
		Claims: []IssueClaim{
			{Repo: "projectbluefin/actions", Issue: 748, PRNumber: 731, PRRepo: "projectbluefin/actions", PRState: PRStateOpen, ExternalAuthor: true, ObservedAt: now, FirstObservedAt: now},
			{Repo: "actions", Issue: 748, PRNumber: 900, PRRepo: "actions", PRState: PRStateOpen, ExternalAuthor: true, Reference: true, ObservedAt: now, FirstObservedAt: now},
			{Repo: "projectbluefin/actions", Issue: 12, PRNumber: 40, PRRepo: "actions", PRState: PRStateOpen, Closing: true, ObservedAt: now, FirstObservedAt: now},
		},
		History: []IssuePRRecord{
			{Repo: "projectbluefin/actions", Issue: 748, PRNumber: 731, PRRepo: "actions", State: PRStateOpen, ObservedAt: now},
		},
	}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Len() != 3 {
		t.Fatalf("loaded %d claims, want 3 (two spellings of #748 before re-keying)", ledger.Len())
	}

	ledger.SetRepoCanonicalizer(c.canonicalRepo)
	if ledger.Len() != 2 {
		t.Fatalf("after re-keying %d claims, want 2", ledger.Len())
	}
	if claim, ok := ledger.Lookup("actions", 748); !ok || claim.PRNumber != 731 {
		t.Fatalf("Lookup(actions, 748) = %+v, %v; want the stronger #731 claim to survive the merge", claim, ok)
	}
	if claim, ok := ledger.Lookup("actions", 12); !ok || claim.PRNumber != 40 {
		t.Fatalf("Lookup(actions, 12) = %+v, %v; want the re-keyed #40 claim", claim, ok)
	}
	if churn, ok := ledger.Churn("actions", 748); !ok || len(churn.Open) != 1 {
		t.Fatalf("Churn(actions, 748) = %+v, %v; want the re-keyed history record", churn, ok)
	}
	// Idempotent: setting it again (every guard pass does) changes nothing.
	ledger.SetRepoCanonicalizer(c.canonicalRepo)
	if ledger.Len() != 2 {
		t.Fatalf("second re-key left %d claims, want 2", ledger.Len())
	}

	if err := ledger.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range reloaded.Claims() {
		if claim.Repo != "actions" {
			t.Fatalf("saved claim keyed %q, want the configured spelling", claim.Repo)
		}
	}
}

// The flip itself: with an owner-qualified claim, the stage sweep (fed by the
// claimed-by-PR filter) wrote 010 while BackfillCoveredByLedger wrote 040.
// Keyed by one spelling, the filter buckets the issue covered, the sweep's
// 040 stands, and the backfill finds nothing to correct.
func TestOwnerQualifiedClaimFilterAndBackfillAgree(t *testing.T) {
	srv := newStageFactsServer(t, "help wanted", stagelabels.CoveredByPR.Label())
	c := testClient(t, srv.srv.URL)
	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	store := stagelabels.Shared()
	issue := c.IssueStageRef("r", 7)
	forget := func() { store.Forget(issue.Key()); store.Forget(c.PRStageRef("o/r", 8).Key()) }
	forget()
	t.Cleanup(forget)

	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "claims.json"), testLogger())
	ledger.SetRepoCanonicalizer(c.canonicalRepo)
	if err := ledger.Record(IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 8, PRRepo: "o/r", PRState: PRStateOpen, ExternalAuthor: true, Source: ClaimSourceVerdict}); err != nil {
		t.Fatal(err)
	}

	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "r", Number: 7}}, Count: 1}}
	if got := FilterClaimedIssues(result, ledger, nil, testLogger()); got != 1 {
		t.Fatalf("filter suppressed %d, want 1: the enumeration's bare spelling must find the claim", got)
	}
	// What the sweep writes for a claimed_by_pr issue.
	store.Observe(issue.Key(), stagelabels.CoveredByPR, "open PR covers it")
	for pass := 1; pass <= 2; pass++ {
		if res := c.BackfillCoveredByLedger(context.Background(), ledger, nil); res.Checked != 0 || res.Covered != 0 {
			t.Fatalf("backfill pass %d = %+v, want nothing to correct", pass, res)
		}
	}
	if srv.prReads != 0 {
		t.Fatalf("backfill read the PR %d times, want none (the two already agree)", srv.prReads)
	}
}
