package mutation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

func openTargetLedger(t *testing.T, maxWriters int) *Ledger {
	t.Helper()
	l, err := OpenLedgerScoped(filepath.Join(t.TempDir(), "claims.json"), maxWriters, TargetWriterScope)
	if err != nil {
		t.Fatalf("OpenLedgerScoped: %v", err)
	}
	return l
}

func TestTargetWriterScope(t *testing.T) {
	cases := []struct {
		claim Claim
		want  string
	}{
		{TaskClaim("acme/widgets", "acme/widgets#label_mutation/7"), "acme/widgets#7"},
		{TaskClaim("acme/widgets", "acme/widgets#review_submit/7"), "acme/widgets#7"},
		{TaskClaim("acme/widgets", "acme/widgets#7"), "acme/widgets#7"},
		{TaskClaim("acme/widgets", "acme/widgets#branch_push/feat/123"), "acme/widgets"},
		{TaskClaim("acme/widgets", "acme/widgets#issue_create/abc123"), "acme/widgets"},
		{TaskClaim("acme/widgets", "acme/widgets#pull_request_create/feature"), "acme/widgets"},
		{TaskClaim("acme/widgets", "acme/widgets!KEY"), "acme/widgets"},
		{RepoClaim("acme/widgets"), "acme/widgets"},
	}
	for _, tc := range cases {
		if got := TargetWriterScope(tc.claim); got != tc.want {
			t.Errorf("TargetWriterScope(%s) = %q, want %q", tc.claim.Key(), got, tc.want)
		}
	}
}

// #14645: two writers on different PRs in the same repo both acquire; the
// per-repo ledger (OpenLedger) still refuses the second.
func TestLedgerTargetScope_DifferentTargetsSameRepoBothAcquire(t *testing.T) {
	now := time.Unix(100, 0)
	a := TaskClaim("acme/widgets", "acme/widgets#review_submit/7")
	b := TaskClaim("acme/widgets", "acme/widgets#label_mutation/9")

	l := openTargetLedger(t, 0)
	if _, err := l.Acquire(a, "reviewer", ttl, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := l.Acquire(b, "labeler", ttl, now); err != nil {
		t.Fatalf("a writer on a different PR in the same repo must acquire: %v", err)
	}

	repo, err := OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	if _, err := repo.Acquire(a, "reviewer", ttl, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := repo.Acquire(b, "labeler", ttl, now); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("the per-repo ledger keeps its single repo slot: %v", err)
	}
}

func TestLedgerTargetScope_SameTargetSecondWriterRefused(t *testing.T) {
	now := time.Unix(100, 0)
	l := openTargetLedger(t, 0)
	if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#review_submit/7"), "reviewer", ttl, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#label_mutation/7"), "labeler", ttl, now)
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("second writer on the same repo#number must be refused: %v", err)
	}
	if !strings.Contains(err.Error(), "target acme/widgets#7 writers exhausted (max 1)") {
		t.Fatalf("error must name the exhausted target: %v", err)
	}
}

func TestLedgerTargetScope_UnnumberedFallsBackToRepo(t *testing.T) {
	now := time.Unix(100, 0)
	l := openTargetLedger(t, 0)
	if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#branch_push/feature"), "pusher", ttl, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#pull_request_create/feature"), "opener", ttl, now)
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "repo acme/widgets writers exhausted (max 1)") {
		t.Fatalf("unnumbered mutations share the repo slot: %v", err)
	}
	if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#label_mutation/7"), "labeler", ttl, now); err != nil {
		t.Fatalf("a numbered target does not queue behind the repo pool: %v", err)
	}
}

func TestLedgerTargetScope_MaxWritersHonoured(t *testing.T) {
	now := time.Unix(100, 0)
	l := openTargetLedger(t, 2)
	for _, kind := range []string{"review_submit", "label_mutation"} {
		if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#"+kind+"/7"), kind, ttl, now); err != nil {
			t.Fatalf("acquire %s within max 2: %v", kind, err)
		}
	}
	_, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#issue_comment/7"), "commenter", ttl, now)
	if !errors.Is(err, ErrNoCapacity) || !strings.Contains(err.Error(), "(max 2)") {
		t.Fatalf("third writer on the target must be refused at max 2: %v", err)
	}
}

// #14645 ask 3: label writes on two different issues are admitted at enforce
// while a review on a third PR in the same repo is in flight.
func TestBoundaryTargetScope_LabelsNotBlockedByInFlightReview(t *testing.T) {
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	ledger := openTargetLedger(t, 0)
	now := time.Unix(100, 0)
	b := &Boundary{Executor: Executor{Ledger: ledger, Journal: journal, Mode: "enforce", Now: func() time.Time { return now }}, Holder: "test"}

	review, err := ledger.Acquire(TaskClaim("acme/widgets", "acme/widgets#review_submit/3"), "reviewer", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"5", "6"} {
		called := false
		_, err := b.Execute(context.Background(), effects.Claim{Repo: "acme/widgets", Kind: effects.KindLabelMutation, Target: target},
			func(context.Context) (effects.Result, error) {
				called = true
				return effects.Result{Provenance: "labeled"}, nil
			})
		if err != nil || !called {
			t.Fatalf("label on #%s behind in-flight review on #3: called=%v err=%v", target, called, err)
		}
	}
	called := false
	_, err = b.Execute(context.Background(), effects.Claim{Repo: "acme/widgets", Kind: effects.KindLabelMutation, Target: "3"},
		func(context.Context) (effects.Result, error) {
			called = true
			return effects.Result{}, nil
		})
	if !errors.Is(err, effects.ErrDenied) || called {
		t.Fatalf("label on the reviewed PR itself must still wait: called=%v err=%v", called, err)
	}
	if _, err := ledger.Release(review.Claim.Key(), review.Epoch, now); err != nil {
		t.Fatal(err)
	}
}
