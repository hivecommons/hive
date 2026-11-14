package refderive

import (
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/marketplace/curated"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// curatedTechdocs returns the embedded cncf-techdocs-assessment model at one
// spec.meta.version, as a hive that imported it from the marketplace holds it.
func curatedTechdocs(t *testing.T, version string) *workflowmodel.Model {
	t.Helper()
	entries, errs := curated.Load()
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, e := range entries {
		if e.Model.Metadata.Name == "cncf-techdocs-assessment" && e.Model.Spec.Meta.Version == version {
			return e.Model
		}
	}
	t.Fatalf("cncf-techdocs-assessment@%s not embedded", version)
	return nil
}

// TestDeriveWithEmbeddedCuratedModels pins hivecommons/hive#18139 step 4
// against the real curated files: a hive bound to curated-2026.11.1 derives
// nothing (the version declares no intake fields and is never edited in
// place), binding curated-2026.11.2 derives the same repos the hand-written
// intake in derive_test.go does.
func TestDeriveWithEmbeddedCuratedModels(t *testing.T) {
	opts := Options{Authorized: []string{"cncf/techdocs"}}

	old := curatedTechdocs(t, "curated-2026.11.1")
	if old.Spec.Intake.DerivesReferenceRepos() {
		t.Fatalf("curated-2026.11.1 must declare no intake fields: %+v", old.Spec.Intake)
	}
	if _, err := Derive(accepted395(t), old.Spec.Intake, opts); !errors.Is(err, ErrNoIntakeFields) {
		t.Fatalf("curated-2026.11.1: err %v, want ErrNoIntakeFields", err)
	}

	cur := curatedTechdocs(t, "curated-2026.11.2")
	if cur.Spec.Intake.ReferenceRepoFields == nil || cur.Spec.Intake.OutOfScopeFields == nil {
		t.Fatalf("curated-2026.11.2 intake: %+v", cur.Spec.Intake)
	}
	fromModel, err := Derive(accepted395(t), cur.Spec.Intake, opts)
	if err != nil {
		t.Fatalf("curated-2026.11.2: %v", err)
	}
	fromTest, err := Derive(accepted395(t), techdocsIntake, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromModel.Repos) == 0 || len(fromModel.Repos) != len(fromTest.Repos) {
		t.Fatalf("curated-2026.11.2 derived %+v, hand-written intake derived %+v", fromModel.Repos, fromTest.Repos)
	}
	for i := range fromModel.Repos {
		if fromModel.Repos[i].Repo != fromTest.Repos[i].Repo || fromModel.Repos[i].DerivedFrom != "cncf/techdocs#395" {
			t.Fatalf("repo %d: model %+v, test %+v", i, fromModel.Repos[i], fromTest.Repos[i])
		}
	}
	if len(fromModel.Skipped) != 0 {
		t.Fatalf("nothing in #395 should be skipped: %+v", fromModel.Skipped)
	}
}
