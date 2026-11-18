package stagelabels

import (
	"testing"
)

func TestStoreNoteHeadApproval(t *testing.T) {
	s := NewStore("", nil)
	key := Key("acme/widget", 7)
	s.NoteHeadApproval(key, "h1", "octocat")
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteHeadApproval created a record")
	}
	s.Observe(key, NeedsDecision, "fork-guard:Canary")
	s.NoteHeadApproval(key, "h1", "@octocat")
	s.NoteHeadApproval(key, "", "someone")
	s.NoteHeadApproval(key, "h1", "")
	rec, _ := s.Get(key)
	if rec.HeadApprover("h1") != "octocat" || rec.HeadApprover("H1") != "octocat" || rec.HeadApprover("h2") != "" || rec.HeadApprover("") != "" {
		t.Fatalf("record = %q@%q", rec.ApprovedBy, rec.ApprovedHead)
	}
}
