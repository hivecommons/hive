package github

// Regression coverage for kubestellar/hive#4929: the scanner re-implemented an
// issue its own open PR already covered, because the PR wrote "Refs #362"
// instead of a closing keyword and weak claims were waved straight through the
// agent-side filter.
//
// The reported shape, on projectbluefin/dakota: PR #1402 (Aug 24) said
// `Refs #362`; PR #1411 (Aug 26) said `Fixes #362`. Two independent
// implementations of one issue, two days apart, neither aware of the other.
// The scanner's hold-gated policy forbids `gh pr list`, so the second agent had
// no way to discover the first PR — and the same policy is what tells agents to
// write `Refs #N` for multi-phase work.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scannerActionable builds the actionable set the scanner's ${ISSUE_LIST} is
// rendered from, holding the one issue at the centre of the report.
//
// repo is passed rather than hardcoded because claim keys are compared on the
// CONFIG form of the repo name: FetchClaims stamps claims with the name from
// the client's repo list ("dakota"), and Issue.Repo carries the same form, so a
// test that mixed "dakota" and "projectbluefin/dakota" would look up a key that
// never matches and pass or fail for the wrong reason.
func scannerActionable(repo string) *ActionableResult {
	result := &ActionableResult{}
	result.Issues.Items = []Issue{
		{Repo: repo, Number: 362, Title: "persistent kargs", AgeMinutes: 5},
	}
	result.Issues.Count = 1
	return result
}

// TestScannerNotReofferedIssueUnderOpenRefsPR is the end-to-end reproduction:
// a live PR body carrying only "Refs #362" must keep #362 out of the actionable
// set the scanner is kicked with, two days after the PR was opened.
//
// It runs the whole path — FetchClaims parses the live PR, Reconcile anchors it,
// FilterClaimedIssues decides — rather than hand-building a claim, because the
// bug was an interaction between those stages and a hand-built claim would step
// over the tier that produced it.
func TestScannerNotReofferedIssueUnderOpenRefsPR(t *testing.T) {
	opened := time.Now().Add(-48 * time.Hour) // #1402, two days before #1411

	srv := prClaimServer(t, []map[string]any{
		pr(1402, "kubestellar-hive[bot]", "fix persistent kargs",
			"Refs #362 — phase 1 of the fix, per the multi-phase convention."),
	}, http.StatusOK)
	c := NewClientForTest(srv.URL, "projectbluefin", []string{"dakota"}, testLogger())

	claims, err := c.FetchClaims(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"})
	if err != nil {
		t.Fatalf("FetchClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("expected one claim from the Refs body, got %+v", claims)
	}
	if !claims[0].Reference {
		t.Fatalf("claim from a non-closing reference must be marked Reference: %+v", claims[0])
	}
	if claims[0].Issue != 362 {
		t.Fatalf("claimed issue = %d, want 362", claims[0].Issue)
	}
	// Backdate to when #1402 was actually opened; the ledger anchors the
	// deferral window on first observation, not on this scan.
	claims[0].ObservedAt = opened
	claims[0].FirstObservedAt = opened

	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	ledger.Reconcile(claims, true)

	result := scannerActionable("dakota")
	suppressed := FilterClaimedIssues(result, ledger, nil, testLogger())
	if suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 — issue #362 must not be re-offered while "+
			"an open PR references it (the scanner cannot see that PR)", suppressed)
	}
	if len(result.Issues.Items) != 0 {
		t.Fatalf("issue #362 still actionable: %+v", result.Issues.Items)
	}
}

// TestWeakClaimWindowDoesNotResetOnReobservation is the mutation this design
// most easily fails silently. Reconcile's authoritative path REPLACES the claim
// map every enumeration cycle; if the fresh scan's timestamp became the anchor,
// a PR re-observed each cycle would restart its own deadline forever and the
// bounded window would be a permanent freeze — the exact outcome the #3980
// analysis refused.
//
// The claim below is re-observed 200 times across the window's lifetime and
// must still release on schedule.
func TestWeakClaimWindowDoesNotResetOnReobservation(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	first := time.Now()
	clock := first
	ledger.SetClock(func() time.Time { return clock })

	observe := func(at time.Time) {
		ledger.Reconcile([]IssueClaim{{
			Repo: "projectbluefin/dakota", Issue: 362,
			PRNumber: 1402, PRRepo: "projectbluefin/dakota",
			PRURL:      "https://github.com/projectbluefin/dakota/pull/1402",
			PRAuthor:   "kubestellar-hive[bot]",
			ObservedAt: at,
			Reference:  true,
		}}, true)
	}
	observe(first)

	// Re-observe across the whole window, as a live enumeration cycle would.
	step := weakClaimDeferWindow / 200
	for i := 1; i <= 200; i++ {
		clock = first.Add(time.Duration(i) * step)
		observe(clock)
	}

	got, ok := ledger.Lookup("projectbluefin/dakota", 362)
	if !ok {
		t.Fatal("claim vanished across re-observation")
	}
	if !got.FirstObservedAt.Equal(first) {
		t.Fatalf("FirstObservedAt = %v, want the original %v — re-observation must not "+
			"re-anchor the window, or it never expires", got.FirstObservedAt, first)
	}
	if !got.ObservedAt.Equal(clock) {
		t.Fatalf("ObservedAt = %v, want the latest observation %v", got.ObservedAt, clock)
	}

	// One tick past the window it must release, despite 201 observations.
	clock = first.Add(weakClaimDeferWindow + time.Minute)
	result := scannerActionable("projectbluefin/dakota")
	if suppressed := FilterClaimedIssues(result, ledger, nil, testLogger()); suppressed != 0 {
		t.Fatalf("suppressed = %d past the window, want 0 — the window must be a bound, not a freeze", suppressed)
	}
}

// TestWeakClaimWindowRestartsForADifferentPR: the anchor is carried forward for
// the SAME pull request only. A different PR taking over the issue is new work,
// and deferring behind the previous PR's expired clock would hand the issue
// straight back out.
func TestWeakClaimWindowRestartsForADifferentPR(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	old := time.Now().Add(-weakClaimDeferWindow - time.Hour)

	base := IssueClaim{
		Repo: "projectbluefin/dakota", Issue: 362,
		PRRepo: "projectbluefin/dakota", PRAuthor: "kubestellar-hive[bot]",
		Reference: true,
	}
	first := base
	first.PRNumber = 1402
	first.ObservedAt, first.FirstObservedAt = old, old
	ledger.Reconcile([]IssueClaim{first}, true)

	// Expired: the issue is actionable again.
	if suppressed := FilterClaimedIssues(scannerActionable("projectbluefin/dakota"), ledger, nil, testLogger()); suppressed != 0 {
		t.Fatalf("expired claim suppressed %d, want 0", suppressed)
	}

	// A different PR now references the issue. Fresh work, fresh window.
	second := base
	second.PRNumber = 1500
	second.ObservedAt = time.Now()
	ledger.Reconcile([]IssueClaim{second}, true)

	got, _ := ledger.Lookup("projectbluefin/dakota", 362)
	if !got.FirstObservedAt.After(old) {
		t.Fatalf("a different PR must start a new window, got anchor %v (old was %v)",
			got.FirstObservedAt, old)
	}
	if suppressed := FilterClaimedIssues(scannerActionable("projectbluefin/dakota"), ledger, nil, testLogger()); suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 — the new PR's window has not elapsed", suppressed)
	}
}

// TestWeakClaimRedStaleReleasesBeforeWindow: the red+stale valve runs BEFORE the
// window, so an abandoned weak PR costs the issue nothing rather than three days.
func TestWeakClaimRedStaleReleasesBeforeWindow(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "projectbluefin/dakota", Issue: 362,
		PRNumber: 1402, PRRepo: "projectbluefin/dakota",
		PRAuthor: "kubestellar-hive[bot]", Reference: true,
		ObservedAt: now, FirstObservedAt: now,
	}}, true)

	// Control: healthy PR, well inside the window, so the issue is deferred.
	if suppressed := FilterClaimedIssues(scannerActionable("projectbluefin/dakota"), ledger, nil, testLogger()); suppressed != 1 {
		t.Fatalf("healthy weak claim suppressed %d, want 1", suppressed)
	}

	redStale := func(prRepo string, prNumber int) bool {
		return prRepo == "projectbluefin/dakota" && prNumber == 1402
	}
	result := scannerActionable("projectbluefin/dakota")
	if suppressed := FilterClaimedIssues(result, ledger, redStale, testLogger()); suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0 — a red+stale weak PR must defer nothing", suppressed)
	}
	if len(result.Issues.Items) != 1 {
		t.Fatalf("issue must stay actionable behind a dead PR, got %+v", result.Issues.Items)
	}
}

// TestLoadClaimLedgerAnchorsPreUpgradeClaims: a ledger written before
// FirstObservedAt existed carries a zero anchor. Loading must normalize it to
// ObservedAt, so an upgraded hive neither treats every stored claim as
// expired (re-offering the work the window exists to hold) nor as unanchored
// forever.
func TestLoadClaimLedgerAnchorsPreUpgradeClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	observed := time.Now().Add(-time.Hour)
	// Written in the pre-#4929 shape: no first_observed_at key at all.
	body := `{"saved_at":"` + time.Now().UTC().Format(time.RFC3339) + `","claims":[{` +
		`"repo":"projectbluefin/dakota","issue":362,"pr_number":1402,` +
		`"pr_repo":"projectbluefin/dakota","pr_url":"https://example.test/pull/1402",` +
		`"pr_author":"kubestellar-hive[bot]","reference":true,` +
		`"observed_at":"` + observed.UTC().Format(time.RFC3339Nano) + `"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// Guard the premise: the fixture really does lack the field.
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["claims"].([]any)[0].(map[string]any)["first_observed_at"]; present {
		t.Fatal("fixture is not a pre-upgrade ledger; it already carries first_observed_at")
	}

	ledger, err := LoadClaimLedger(path, testLogger())
	if err != nil {
		t.Fatalf("LoadClaimLedger: %v", err)
	}
	got, ok := ledger.Lookup("projectbluefin/dakota", 362)
	if !ok {
		t.Fatal("pre-upgrade claim did not survive the load")
	}
	if got.FirstObservedAt.IsZero() {
		t.Fatal("pre-upgrade claim loaded with a zero anchor; its window would never start")
	}
	if !got.FirstObservedAt.Equal(got.ObservedAt) {
		t.Fatalf("anchor = %v, want it normalized to ObservedAt %v", got.FirstObservedAt, got.ObservedAt)
	}
	// And it behaves: an hour old is well inside the window, so it defers.
	if suppressed := FilterClaimedIssues(scannerActionable("projectbluefin/dakota"), ledger, nil, testLogger()); suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 — a freshly-normalized claim defers", suppressed)
	}
}

// TestStrongClaimUnaffectedByWindow: a hive-authored closing claim suppresses
// for as long as it stands. The window applies only to the weak tiers; a PR
// that says it closes the issue must not start handing the issue back after
// three days just because the reviewer is slow.
func TestStrongClaimUnaffectedByWindow(t *testing.T) {
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), testLogger())
	old := time.Now().Add(-30 * 24 * time.Hour)
	ledger.Reconcile([]IssueClaim{{
		Repo: "projectbluefin/dakota", Issue: 362,
		PRNumber: 1411, PRRepo: "projectbluefin/dakota",
		PRAuthor:   "kubestellar-hive[bot]",
		ObservedAt: time.Now(), FirstObservedAt: old,
	}}, true)

	if suppressed := FilterClaimedIssues(scannerActionable("projectbluefin/dakota"), ledger, nil, testLogger()); suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1 — a closing claim never expires into a re-offer", suppressed)
	}
}
