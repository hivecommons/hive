package dashboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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

// hivecommons/hive#7890: an OPEN PR is an external claim only when someone
// other than the reporter authored it. The reporter citing their own open PR
// must record nothing (and the fan-out continues to the next ref); a merged PR
// is a fact whoever merged it, and a different author's open PR still counts.
func TestVerdictSettle7890_SelfAuthoredOpenPRIsNotAnExternalClaim(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/actions#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "o/actions", PRURL: "https://github.com/o/actions/pull/41",
			PRAuthor: "Alice-Dev", Reference: true, ExternalAuthor: true,
		}},
		"o/actions#42": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 42, PRRepo: "o/actions", PRURL: "https://github.com/o/actions/pull/42",
			PRAuthor: "bob", Reference: true, ExternalAuthor: true,
		}},
	}, nil)

	// Case-insensitive: GitHub logins are, and the profile may differ in case.
	hub.settleIssueFromVerdict("o/actions", 7, "already covered by my open #41; bob's #42 also touches it", time.Now(), "alice-dev")

	if len(fx.recorded) != 1 || fx.recorded[0].PRNumber != 42 || fx.recorded[0].PRAuthor != "bob" {
		t.Fatalf("the reporter's own open PR must be skipped and the next ref tried; recorded=%+v", fx.recorded)
	}
	if len(fx.checked) != 2 {
		t.Fatalf("both refs should have been verified; checked=%v", fx.checked)
	}
}

func TestVerdictSettle7890_SelfAuthoredMergedPRStillSettles(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/actions#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "o/actions", PRAuthor: "alice-dev", MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
		}},
	}, nil)

	hub.settleIssueFromVerdict("o/actions", 7, "already landed in #41", time.Now(), "alice-dev")

	if len(fx.recorded) != 1 || !fx.recorded[0].MergedPR {
		t.Fatalf("a merged PR is a fact regardless of author; recorded=%+v", fx.recorded)
	}
}

func TestVerdictSettle7890_OnlySelfAuthoredOpenPRRecordsNothing(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/actions#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "o/actions", PRAuthor: "alice-dev", Reference: true, ExternalAuthor: true,
		}},
	}, nil)

	hub.settleIssueFromVerdict("o/actions", 7, "my #41 has this", time.Now(), "alice-dev")

	if len(fx.recorded) != 0 {
		t.Fatalf("a self-authored open PR alone must record nothing; recorded=%+v", fx.recorded)
	}
}

func TestAlreadyDoneVerdict8477_ParseStructuredAndRegex(t *testing.T) {
	refs, ok := alreadyDoneVerdictRefs("o/r", 7, "", verdictReasonKindAlreadyDone, &VerdictEvidence{PR: 41})
	if !ok || len(refs) != 1 || refs[0].Number != 41 {
		t.Fatalf("structured already_done refs = %+v ok=%v, want PR 41", refs, ok)
	}
	refs, ok = alreadyDoneVerdictRefs("o/r", 7, "merged PR #42 already resolves this issue", "", nil)
	if !ok || len(refs) != 1 || refs[0].Number != 42 {
		t.Fatalf("regex merged PR refs = %+v ok=%v, want PR 42", refs, ok)
	}
	if refs, ok := alreadyDoneVerdictRefs("o/r", 7, "maintainer decision pending; see #42", "", nil); ok || len(refs) != 0 {
		t.Fatalf("non already-done reason should not classify: refs=%+v ok=%v", refs, ok)
	}
}

func TestAlreadyDoneVerdict8477_VerifiedClosePath(t *testing.T) {
	hub, s := covK2Hub(t)
	level := config.SelfMergeMinACMMLevel
	s.deps.Config.ACMMLevel = &level
	enabled := true
	s.deps.Config.Hub.ContributeCloseAlreadyDone = &enabled
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	mergedAt := time.Now().Add(-time.Hour)
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/r#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "o/r", PRURL: "https://github.com/o/r/pull/41",
			PRAuthor: "dev", MergedPR: true, MergedAt: mergedAt,
		}},
	}, nil)
	var closedRepo, closedReporter string
	var closedNumber int
	hub.alreadyDoneMarker = func(_ context.Context, repo string, number int, claim ghpkg.IssueClaim, reporter string, closeIssue bool) error {
		if !closeIssue {
			t.Fatal("verified close path must request close")
		}
		closedRepo, closedNumber, closedReporter = repo, number, reporter
		if claim.PRNumber != 41 {
			t.Fatalf("claim PR = %d, want 41", claim.PRNumber)
		}
		return nil
	}

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "merged PR #41 already resolves this", "", nil, time.Now(), "danathar")

	if got != verdictDispositionAlreadyDoneClosed {
		t.Fatalf("disposition = %q, want closed", got)
	}
	if closedRepo != "o/r" || closedNumber != 7 || closedReporter != "danathar" {
		t.Fatalf("close path not called with issue/reporter: %s#%d reporter=%q", closedRepo, closedNumber, closedReporter)
	}
	if len(fx.recorded) != 1 || fx.recorded[0].Source != ghpkg.ClaimSourceVerdict {
		t.Fatalf("verified already-done must still record claim: %+v", fx.recorded)
	}
}

func TestAlreadyDoneVerdict8477_UnverifiedSuppressesLonger(t *testing.T) {
	hub, s := covK2Hub(t)
	s.deps.RecordIssueClaim = (&settleFixture{}).record
	s.deps.Config.Hub.ContributeAlreadyDoneHoldDays = 30
	hub.settleVerifier = func(_ context.Context, _ string, _ int, ref ghpkg.SettlingRef, _ time.Time) (ghpkg.SettleVerification, error) {
		return ghpkg.SettleVerification{Reason: "not merged"}, nil
	}

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "already fixed by merged PR #41", "", nil, time.Now(), "ct")

	if got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("disposition = %q, want unverified", got)
	}
	hub.completedMu.Lock()
	rec, ok := hub.noWorkVerdicts["o/r#7"]
	hub.completedMu.Unlock()
	if !ok {
		t.Fatal("unverified already-done verdict must be recorded in no-work ledger")
	}
	if !rec.AlreadyDoneUnverified || rec.ReasonKind != verdictReasonKindAlreadyDone {
		t.Fatalf("record not marked already-done-unverified: %+v", rec)
	}
	if gotHours, wantHours := rec.SuppressHours, float64(30*24); gotHours != wantHours {
		t.Fatalf("suppress hours = %v, want %v", gotHours, wantHours)
	}
}

func TestAlreadyDoneVerdict8477_ACMMGatePreventsCloseButRecords(t *testing.T) {
	hub, s := covK2Hub(t)
	level := config.SelfMergeMinACMMLevel - 1
	s.deps.Config.ACMMLevel = &level
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/r#41": {Settled: true, Claim: ghpkg.IssueClaim{PRNumber: 41, PRRepo: "o/r", MergedPR: true}},
	}, nil)
	var labeled bool
	hub.alreadyDoneMarker = func(_ context.Context, _ string, _ int, _ ghpkg.IssueClaim, _ string, closeIssue bool) error {
		if closeIssue {
			t.Fatal("ACMM below close floor must not close")
		}
		labeled = true
		return nil
	}

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "merged PR #41 already resolves this", "", nil, time.Now(), "ct")

	if got != verdictDispositionAlreadyDoneLabeled {
		t.Fatalf("disposition = %q, want labeled", got)
	}
	if !labeled {
		t.Fatal("verified already-done must still label/comment when close is not allowed")
	}
	if len(fx.recorded) != 1 {
		t.Fatalf("verified claim should still be recorded: %+v", fx.recorded)
	}
}

func TestAlreadyDoneVerdict8477_DefaultDoesNotClose(t *testing.T) {
	hub, s := covK2Hub(t)
	level := config.SelfMergeMinACMMLevel
	s.deps.Config.ACMMLevel = &level
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	hub.settleVerifier = fx.verifier(map[string]ghpkg.SettleVerification{
		"o/r#41": {Settled: true, Claim: ghpkg.IssueClaim{PRNumber: 41, PRRepo: "o/r", MergedPR: true}},
	}, nil)
	var labeled bool
	hub.alreadyDoneMarker = func(_ context.Context, _ string, _ int, _ ghpkg.IssueClaim, _ string, closeIssue bool) error {
		if closeIssue {
			t.Fatal("default already-done action should label/comment, not close")
		}
		labeled = true
		return nil
	}

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "merged PR #41 already resolves this", "", nil, time.Now(), "ct")

	if got != verdictDispositionAlreadyDoneLabeled || !labeled {
		t.Fatalf("disposition=%q labeled=%v, want labeled default path", got, labeled)
	}
}
