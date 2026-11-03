package effects

import (
	"context"
	"errors"
	"testing"

	cruxeffects "github.com/hivecommons/hive/crux/effects"
)

// TestAliasesAreTheCruxTypes pins the alias direction: a Boundary written
// against this package is the same type the Crux ring consumes, and the
// sentinel errors are the same values, so errors.Is works across the seam.
func TestAliasesAreTheCruxTypes(t *testing.T) {
	var b Boundary = NoopBoundary{}
	var cb cruxeffects.Boundary = b
	res, err := cruxeffects.Execute(context.Background(), cb, Claim{Repo: "o/r", Kind: KindIssueComment, Target: "1"},
		func(context.Context) (Result, error) { return Result{Provenance: "p"}, nil })
	if err != nil || res.Provenance != "p" {
		t.Fatalf("Execute through alias = %+v, %v", res, err)
	}
	if !errors.Is(NotApplied(errors.New("x")), cruxeffects.ErrNotApplied) {
		t.Fatal("NotApplied must wrap the Crux sentinel")
	}
	if ErrDenied != cruxeffects.ErrDenied || KindBranchPush != cruxeffects.KindBranchPush {
		t.Fatal("alias values differ from the Crux leaf")
	}
	var journaled JournaledBoundary = LoggingBoundary{}
	var cruxJournaled cruxeffects.JournaledBoundary = journaled
	if _, err := cruxJournaled.ExecuteJournaled(context.Background(), Claim{}, func(context.Context) (Result, error) {
		t.Fatal("missing journal invoked an effect through the alias")
		return Result{}, nil
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("journal-required alias lost the denial: %v", err)
	}
}
