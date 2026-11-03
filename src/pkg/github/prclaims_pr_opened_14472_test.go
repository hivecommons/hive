package github

import (
	"testing"
	"time"
)

// hivecommons/hive#14472: a PR the watcher opens claims its issues in the
// ledger immediately, in the repo spelling the scan uses, so the contribute
// admission gate sees it before the next eval-cycle scan.
func TestClaimsFromPROpened(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC)
	identity := HiveIdentity{AIAuthor: "hive-bot", AppLogin: "hive-app[bot]"}
	d := PROpenedDetail{
		Agent:  "sec-check",
		Repo:   "hivecommons/hive",
		Number: 14441,
		URL:    "https://github.com/hivecommons/hive/pull/14441",
		Title:  "fix(crux): attribute inference translator callers by socket UID",
		Body:   "Fixes #14439\n\nRefs #14400",
		Author: "hive-app[bot]",
		Head:   "fix/14439-socket-uid",
	}
	claims := ClaimsFromPROpened(d, []string{"hive", "docs"}, identity, now)
	if len(claims) != 2 {
		t.Fatalf("claims = %+v, want closing #14439 and reference #14400", claims)
	}
	strong := claims[0]
	if strong.Repo != "hive" || strong.Issue != 14439 || strong.PRNumber != 14441 || strong.PRRepo != "hive" ||
		strong.PRURL != d.URL || strong.PRAuthor != d.Author || strong.PRState != PRStateOpen ||
		!strong.Closing || strong.Reference || strong.ExternalAuthor || !strong.ObservedAt.Equal(now) {
		t.Fatalf("closing claim: %+v", strong)
	}
	if weak := claims[1]; weak.Issue != 14400 || !weak.Reference || weak.Closing {
		t.Fatalf("reference claim: %+v", weak)
	}

	// A repo configured as owner/repo keeps that spelling; an unknown repo is
	// passed through; an external author is flagged.
	d.Author = "someone-else"
	if got := ClaimsFromPROpened(d, []string{"hivecommons/hive"}, identity, now); len(got) != 2 || got[0].Repo != "hivecommons/hive" || !got[0].ExternalAuthor {
		t.Fatalf("owner/repo config: %+v", got)
	}
	if got := ClaimsFromPROpened(d, nil, identity, now); len(got) != 2 || got[0].Repo != "hivecommons/hive" {
		t.Fatalf("unknown repo: %+v", got)
	}
	if got := ClaimsFromPROpened(PROpenedDetail{Repo: "hive", Number: 0, Body: "Fixes #1"}, nil, identity, now); got != nil {
		t.Fatalf("PR 0 claimed: %+v", got)
	}
	if got := ClaimsFromPROpened(PROpenedDetail{Repo: "hive", Number: 5, Body: "no issue here"}, nil, identity, now); len(got) != 0 {
		t.Fatalf("issue-less PR claimed: %+v", got)
	}
}

func TestRecordOpenedPRClaimsBlocksLookup(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 0, 34, 0, time.UTC)
	identity := HiveIdentity{AIAuthor: "hive-bot"}
	c := &Client{org: "hivecommons", repos: []string{"hive"}, logger: testLogger()}
	ledger := NewClaimLedger("", testLogger())
	if _, ok := ledger.Lookup("hive", 14439); ok {
		t.Fatal("ledger claimed #14439 before any PR opened")
	}
	d := PROpenedDetail{Agent: "sec-check", Repo: "hivecommons/hive", Number: 14441, URL: "u", Body: "Fixes #14439", Author: "hive-bot", Head: "fix/14439"}
	if got := c.RecordOpenedPRClaims(ledger, d, identity, now); len(got) != 1 {
		t.Fatalf("recorded %+v", got)
	}
	claim, ok := ledger.Lookup("hive", 14439)
	if !ok || claim.PRNumber != 14441 || claim.PRState != PRStateOpen || !claim.Closing {
		t.Fatalf("lookup after open: %+v ok=%v", claim, ok)
	}
	// Recording the same PR again keeps the first observation; the issue
	// stays claimed either way.
	c.RecordOpenedPRClaims(ledger, d, identity, now.Add(time.Minute))
	if claim, ok = ledger.Lookup("hive", 14439); !ok || claim.PRNumber != 14441 || !claim.FirstObservedAt.Equal(now) {
		t.Fatalf("re-record: %+v ok=%v", claim, ok)
	}
	if got := (*Client)(nil).RecordOpenedPRClaims(ledger, d, identity, now); got != nil {
		t.Fatalf("nil client recorded %+v", got)
	}
	if got := c.RecordOpenedPRClaims(nil, d, identity, now); got != nil {
		t.Fatalf("nil ledger recorded %+v", got)
	}
}
