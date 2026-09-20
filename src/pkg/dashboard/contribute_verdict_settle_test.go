package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#7871 suite: the hub turns a no_work_needed reason's
// citation into a verified ledger claim — and never into an unverified one.

type settleFixture struct {
	mu       sync.Mutex
	checked  []string
	recorded []ghpkg.IssueClaim
}

func (f *settleFixture) record(c ghpkg.IssueClaim) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, c)
	return nil
}

func (f *settleFixture) verifier(answers map[string]ghpkg.SettleVerification, errs map[string]error) ghpkg.SettleVerifier {
	return func(_ context.Context, _ string, _ int, ref ghpkg.SettlingRef, _ time.Time) (ghpkg.SettleVerification, error) {
		f.mu.Lock()
		f.checked = append(f.checked, ref.String())
		f.mu.Unlock()
		if err, ok := errs[ref.String()]; ok {
			return ghpkg.SettleVerification{Reason: "boom"}, err
		}
		return answers[ref.String()], nil
	}
}

func TestVerdictSettle7871_RecordsFirstVerifiedReferenceOnly(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	mergedAt := time.Now().Add(-time.Hour)
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/actions#532": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 532, PRRepo: "o/actions", PRURL: "https://github.com/o/actions/pull/532",
			PRAuthor: "bob", MergedPR: true, MergedAt: mergedAt,
		}},
		"o/actions#999": {Settled: true, Claim: ghpkg.IssueClaim{PRNumber: 999}},
	}, nil)

	hub.settleIssueFromVerdict("o/actions", 533,
		"all four workflow entries #533 asks for are already merged into upstream/main (via #532); see also #999",
		time.Now(), "ct-1")

	if len(fx.recorded) != 1 {
		t.Fatalf("expected exactly one recorded claim, got %+v", fx.recorded)
	}
	c := fx.recorded[0]
	if c.Repo != "o/actions" || c.Issue != 533 || c.PRNumber != 532 || !c.MergedPR ||
		c.Source != ghpkg.ClaimSourceVerdict || c.SourceReporter != "ct-1" {
		t.Fatalf("unexpected claim: %+v", c)
	}
	if len(fx.checked) != 1 || fx.checked[0] != "o/actions#532" {
		t.Fatalf("the task's own issue must be skipped and the first hit must stop the fan-out; checked=%v", fx.checked)
	}
}

func TestVerdictSettle7871_UnverifiedReferencesRecordNothing(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/r#123": {Settled: false, Reason: "github returned no pull request"},
	}, map[string]error{
		"o/r@deadbee": errors.New("500"),
	})

	hub.settleIssueFromVerdict("o/r", 7, "already fixed by #123 and deadbee", time.Now(), "ct-1")

	if len(fx.recorded) != 0 {
		t.Fatalf("a fabricated or unverifiable citation must never become a claim: %+v", fx.recorded)
	}
	if len(fx.checked) != 2 {
		t.Fatalf("every candidate must be checked before giving up; checked=%v", fx.checked)
	}
}

func TestVerdictSettle7871_NoLedgerNoReasonNoRefsAreNoOps(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/r#1": {Settled: true, Claim: ghpkg.IssueClaim{PRNumber: 1, MergedPR: true}},
	}, nil)

	// No RecordIssueClaim wired: nothing is even looked up.
	s.deps.RecordIssueClaim = nil
	hub.settleIssueFromVerdict("o/r", 7, "done in #1", time.Now(), "ct-1")
	if len(fx.checked) != 0 {
		t.Fatalf("without a ledger the hub must not spend API calls; checked=%v", fx.checked)
	}

	s.deps.RecordIssueClaim = fx.record
	hub.settleIssueFromVerdict("o/r", 7, "", time.Now(), "ct-1")
	hub.settleIssueFromVerdict("o/r", 7, "maintainer decision pending, nothing cited", time.Now(), "ct-1")
	hub.settleIssueFromVerdict("", 7, "done in #1", time.Now(), "ct-1")
	if len(fx.checked) != 0 || len(fx.recorded) != 0 {
		t.Fatalf("empty reason / no refs / no repo must be no-ops; checked=%v recorded=%v", fx.checked, fx.recorded)
	}
}
