package customers

import "testing"

func TestRefForms(t *testing.T) {
	cases := map[string]string{
		IssueRef("org/repo", 12):   "gh:issue:org/repo#12",
		PRRef("org/repo", 7):       "gh:pr:org/repo#7",
		InboxRef("plaud", "abc"):   "inbox:plaud:abc",
		MeetingRef("m1"):           "meeting:m1",
		EventRef("e1"):             "event:e1",
		FactRef("project", "slug"): "fact:project/slug",
		BeadRef("work", "b-1"):     "bead:work:b-1",
		AuditRef("2026-11-01"):     "audit:2026-11-01",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("ref = %q, want %q", got, want)
		}
	}
	if RefKind("meeting:m1") != RefMeeting || RefKind("no-kind") != "" {
		t.Fatal("RefKind")
	}
	if validRef("no-kind") || validRef(":x") || validRef("gh:a\nb") || !validRef("gh:issue:o/r#1") {
		t.Fatal("validRef")
	}
}

type fakeResolver struct {
	gone    map[string]bool
	private map[string]string // ref -> the only user who may see it
}

func (f fakeResolver) Exists(ref string) bool { return !f.gone[ref] }

func (f fakeResolver) Visible(ref string, v Viewer) bool {
	owner, restricted := f.private[ref]
	return !restricted || owner == v.User
}

func TestResolversAnswerPerKind(t *testing.T) {
	r := Resolvers{
		RefMeeting: fakeResolver{gone: map[string]bool{"meeting:old": true}},
		RefInbox:   fakeResolver{private: map[string]string{"inbox:plaud:mine": "alice"}},
		RefFact:    nil,
	}
	if r.Exists("meeting:old") || !r.Exists("meeting:new") {
		t.Fatal("the meeting resolver must decide what exists")
	}
	// A personal recording is visible only to whoever can open it.
	if !r.Visible("inbox:plaud:mine", Viewer{User: "alice"}) {
		t.Fatal("the owner of a recording must see it")
	}
	if r.Visible("inbox:plaud:mine", Viewer{User: "bob", Full: true}) {
		t.Fatal("a full viewer must not see someone else's personal recording")
	}
	// A kind with nobody to ask is kept and shown.
	if !r.Exists("gh:issue:o/r#1") || !r.Visible("gh:issue:o/r#1", Viewer{}) {
		t.Fatal("an unresolved kind must default to kept and visible")
	}
	if !r.Exists("fact:project/x") || !r.Visible("fact:project/x", Viewer{}) {
		t.Fatal("a nil resolver must behave like a missing one")
	}
}
