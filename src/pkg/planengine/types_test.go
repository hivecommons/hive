package planengine

import (
	"errors"
	"testing"
)

func TestDocumentStatusValues(t *testing.T) {
	cases := map[DocumentStatus]string{
		DocumentDraft:      "draft",
		DocumentFinal:      "final",
		DocumentStale:      "stale",
		DocumentSuperseded: "superseded",
		DocumentArchived:   "archived",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("DocumentStatus %q, want %q", got, want)
		}
	}
}

func TestArtifactStatusFinalAndJoinKey(t *testing.T) {
	st := ArtifactStatus{Name: "demo", DocumentStatus: DocumentFinal}
	if !st.Final() {
		t.Fatal("final document should report Final")
	}
	if st.JoinKey() != "demo" {
		t.Fatalf("JoinKey without artifact_id = %q, want name", st.JoinKey())
	}
	st.ArtifactID = "  art-1 "
	st.DocumentStatus = DocumentDraft
	if st.Final() {
		t.Fatal("draft document should not report Final")
	}
	if st.JoinKey() != "art-1" {
		t.Fatalf("JoinKey = %q, want trimmed artifact_id", st.JoinKey())
	}
}

func TestErrorMessagesAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		err  error
		want string
	}{
		{&NotFoundError{Kind: "spec", Name: "demo", Message: "gone"}, `spektacular spec "demo" not found: gone`},
		{&VerbError{Kind: "plan", Name: "demo", Code: "bad", Message: "nope"}, `spektacular plan "demo": nope (bad)`},
		{&WorkDirError{RunKey: "o/r#1", Stage: "spec", Repo: "o/r"}, "spektacular: no repo workdir resolved run=o/r#1 stage=spec repo=o/r"},
		{&WorkDirError{}, "spektacular: no repo workdir resolved"},
		{&PlanImportError{RunKey: "o/r#1", Artifact: "demo", Err: cause}, `spektacular: plan "demo" for run "o/r#1" could not be imported: boom`},
		{&ContractError{Kind: "spec", Name: "demo", Reason: "bad json", Err: cause}, `spektacular spec "demo": contract violation: bad json: boom`},
		{&ContractError{Kind: "spec", Name: "demo", Reason: "bad json"}, `spektacular spec "demo": contract violation: bad json`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("Error() = %q, want %q", got, tc.want)
		}
	}
	if !errors.Is(&PlanImportError{Err: cause}, cause) || !errors.Is(&ContractError{Err: cause}, cause) {
		t.Fatal("PlanImportError and ContractError should unwrap to their cause")
	}
}
