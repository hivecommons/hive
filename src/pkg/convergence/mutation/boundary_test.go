package mutation

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

func testBoundary(t *testing.T, mode string) *Boundary {
	t.Helper()
	ledger, err := OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Boundary{Executor: Executor{Ledger: ledger, Journal: journal, Mode: mode, Now: func() time.Time { return time.Unix(100, 0) }}, Holder: "test"}
}

func TestBoundaryOffPassthrough(t *testing.T) {
	b := testBoundary(t, "off")
	called := false
	res, err := b.Execute(context.Background(), effects.Claim{}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{Provenance: "ok"}, nil
	})
	if err != nil || !called || res.Provenance != "ok" {
		t.Fatalf("off boundary = (%+v, %v), called=%v", res, err, called)
	}
}

func TestBoundaryShadowRecordsAndDoesNotBlockOverlap(t *testing.T) {
	b := testBoundary(t, "shadow")
	held, err := b.Executor.Ledger.Acquire(TaskClaim("acme/widget", "acme/widget#issue_comment/7"), "other", time.Hour, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Executor.Ledger.Release(held.Claim.Key(), held.Epoch, time.Unix(101, 0))
	called := false
	_, err = b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueComment, Target: "7"}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{Provenance: "comment"}, nil
	})
	if err != nil || !called {
		t.Fatalf("shadow should not block overlap: called=%v err=%v", called, err)
	}
}

func TestBoundaryEnforceDeniesOverlap(t *testing.T) {
	b := testBoundary(t, "enforce")
	held, err := b.Executor.Ledger.Acquire(TaskClaim("acme/widget", "acme/widget#issue_comment/7"), "other", time.Hour, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Executor.Ledger.Release(held.Claim.Key(), held.Epoch, time.Unix(101, 0))
	called := false
	_, err = b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueComment, Target: "7"}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{}, nil
	})
	if !errors.Is(err, effects.ErrDenied) || called {
		t.Fatalf("enforce err=%v called=%v, want denied without effect", err, called)
	}
}

func TestBoundaryEnforceJournalsSuccessAndReleases(t *testing.T) {
	b := testBoundary(t, "enforce")
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestCreate, Target: "feature", Actor: "alice"}
	res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{Provenance: "https://github.com/acme/widget/pull/1"}, nil
	})
	if err != nil || res.Provenance == "" {
		t.Fatalf("execute = (%+v, %v)", res, err)
	}
	entry, ok := b.Executor.Ledger.Get(TaskClaim("acme/widget", "acme/widget#pull_request_create/feature").Key())
	if !ok || entry.State != StateReleased {
		t.Fatalf("entry = %+v ok=%v, want released", entry, ok)
	}
	id := Effect{
		OutcomeKey:        "acme/widget@mutation",
		DesiredGeneration: 1,
		Transition:        "external." + effects.KindPullRequestCreate,
		Subject:           "acme/widget#pull_request_create/feature",
		ClaimKey:          TaskClaim("acme/widget", "acme/widget#pull_request_create/feature").Key(),
		Kind:              effects.KindPullRequestCreate,
		Inputs:            map[string]string{"repo": "acme/widget", "kind": effects.KindPullRequestCreate, "target": "feature"},
	}.LogicalID()
	op, ok := b.Executor.Journal.Get(id)
	if !ok || op.Status != StatusApplied || op.Result != res.Provenance {
		t.Fatalf("op = %+v ok=%v, want applied", op, ok)
	}
}

func TestBoundaryEffectErrorRecordsUnknown(t *testing.T) {
	b := testBoundary(t, "enforce")
	boom := errors.New("boom")
	_, err := b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "title"}, func(context.Context) (effects.Result, error) {
		return effects.Result{}, boom
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("execute err = %v, want boom", err)
	}
	id := Effect{
		OutcomeKey:        "acme/widget@mutation",
		DesiredGeneration: 1,
		Transition:        "external." + effects.KindIssueCreate,
		Subject:           "acme/widget#issue_create/title",
		ClaimKey:          TaskClaim("acme/widget", "acme/widget#issue_create/title").Key(),
		Kind:              effects.KindIssueCreate,
		Inputs:            map[string]string{"repo": "acme/widget", "kind": effects.KindIssueCreate, "target": "title"},
	}.LogicalID()
	op, ok := b.Executor.Journal.Get(id)
	if !ok || op.Status != StatusUnknown {
		t.Fatalf("op = %+v ok=%v, want unknown", op, ok)
	}
}

func TestBoundaryRejectsInvalidClaim(t *testing.T) {
	b := testBoundary(t, "enforce")
	_, err := b.Execute(context.Background(), effects.Claim{Repo: "bad", Kind: effects.KindIssueCreate, Target: "x"}, func(context.Context) (effects.Result, error) {
		t.Fatal("effect must not run")
		return effects.Result{}, nil
	})
	if !errors.Is(err, effects.ErrDenied) {
		t.Fatalf("err = %v, want denied", err)
	}
}

func TestBoundaryEnabledRequiresStores(t *testing.T) {
	b := &Boundary{Executor: Executor{Mode: "enforce"}}
	called := false
	_, err := b.Execute(context.Background(), effects.Claim{Repo: "acme/widget", Kind: effects.KindIssueCreate, Target: "x"}, func(context.Context) (effects.Result, error) {
		called = true
		return effects.Result{}, nil
	})
	if err == nil || called {
		t.Fatalf("err=%v called=%v, want store error before effect", err, called)
	}
}

func TestBoundaryNilFallsThrough(t *testing.T) {
	var b *Boundary
	res, err := b.Execute(context.Background(), effects.Claim{}, func(context.Context) (effects.Result, error) {
		return effects.Result{Provenance: "ok"}, nil
	})
	if err != nil || res.Provenance != "ok" {
		t.Fatalf("nil boundary = (%+v, %v)", res, err)
	}
}
