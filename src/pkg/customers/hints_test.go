package customers

import (
	"errors"
	"testing"
)

func TestLocalPart(t *testing.T) {
	for in, want := range map[string]string{
		"Ada.Lovelace+news@Example.com": "ada.lovelace", "bob@x.io": "bob", "no-at-sign": "", "": "",
	} {
		if got := localPart(in); got != want {
			t.Errorf("localPart(%q) = %q, want %q", in, got, want)
		}
	}
	for s, want := range map[string]bool{"octocat": true, "bo": false, "info": false, "admin": false, "noreply": false} {
		if hintableLocal(s) != want {
			t.Errorf("hintableLocal(%q) = %v", s, !want)
		}
	}
}

func seedSuggestion(t *testing.T, s *Store, kind IdentityKind, value string) Suggestion {
	t.Helper()
	if _, err := s.Observe([]Observation{{Kind: kind, Value: value, Predicate: PredSent, Artefact: InboxRef("email", value)}}); err != nil {
		t.Fatal(err)
	}
	sg, ok := s.SuggestionByID(s.Suggestions()[0].ID)
	for _, c := range s.Suggestions() {
		if c.Value == NormalizeIdentity(kind, value) {
			sg, ok = c, true
		}
	}
	if !ok {
		t.Fatal("no suggestion")
	}
	return sg
}

func TestDeriveHintsComparesKeysNeverNames(t *testing.T) {
	s := openStore(t)
	octo := mustAdd(t, s, "Some Display Name", Identity{Kind: KindGitHub, Value: "octocat"})
	mustAdd(t, s, "Short", Identity{Kind: KindGitHub, Value: "bo"})
	byMail := mustAdd(t, s, "Mail Person", Identity{Kind: KindEmail, Value: "dana.reyes@example.com"})
	mail := seedSuggestion(t, s, KindEmail, "Octocat+news@Example.org")
	seedSuggestion(t, s, KindEmail, "bo@example.org")           // too short to compare
	seedSuggestion(t, s, KindEmail, "info@octocat-corp.com")    // role address
	seedSuggestion(t, s, KindEmail, "someone.else@example.org") // matches no login
	login := seedSuggestion(t, s, KindGitHub, "Dana.Reyes")
	// A name never raises a hint, even an exact match for a customer's name.
	if _, err := s.Observe([]Observation{{Kind: KindName, Value: "Some Display Name", Predicate: PredAttended, Artefact: MeetingRef("m1")}}); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeriveHints()
	if err != nil || n != 2 {
		t.Fatalf("DeriveHints = %d, %v; want 2", n, err)
	}
	hints := s.Hints("")
	got := map[string]string{}
	for _, h := range hints {
		got[h.Person] = h.Other
		if h.Reason == "" || h.Identity != nil {
			t.Fatalf("hint = %+v", h)
		}
	}
	if got[octo.ID] != mail.ID || got[byMail.ID] != login.ID || len(got) != 2 {
		t.Fatalf("hints = %v, want octocat<->mail and dana<->login", got)
	}
	// Raised once: running again adds nothing.
	if n, _ := s.DeriveHints(); n != 0 || len(s.Hints("")) != 2 {
		t.Fatalf("second DeriveHints = %d, %d open", n, len(s.Hints("")))
	}
	// A hint links nothing.
	if _, ok := s.Lookup(KindEmail, "octocat+news@example.org"); ok {
		t.Fatal("a hint linked an identity")
	}
}

func TestAnswerHintYesAbsorbsTheSuggestion(t *testing.T) {
	s := newService(t)
	st := s.Store()
	octo := mustAdd(t, st, "Octo", Identity{Kind: KindGitHub, Value: "octocat"})
	mail := seedSuggestion(t, st, KindEmail, "octocat@example.org")
	if _, err := st.DeriveHints(); err != nil {
		t.Fatal(err)
	}
	h := st.Hints(octo.ID)[0]

	view, err := s.Get(ownerV, octo.ID)
	if err != nil || len(view.Hints) != 1 || view.Hints[0].OtherKind != "email" || view.Hints[0].OtherValue != "octocat@example.org" || view.Hints[0].ID != h.ID {
		t.Fatalf("hint view = %+v, %v", view.Hints, err)
	}
	if _, err := s.AnswerHint(readV, h.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only AnswerHint = %v", err)
	}
	if _, err := s.AnswerHint(ownerV, "h_missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing hint = %v", err)
	}
	got, err := s.AnswerHint(rwV, h.ID, true)
	if err != nil || !got.HasIdentity("email:octocat@example.org") {
		t.Fatalf("AnswerHint yes = %+v, %v", got, err)
	}
	for _, i := range got.Identities {
		if i.Kind == KindEmail && (!i.Manual || i.By != "rita" || i.Source != "same_person_hint") {
			t.Fatalf("absorbed identity = %+v", i)
		}
	}
	if _, ok := st.SuggestionByID(mail.ID); ok || len(st.Hints("")) != 0 {
		t.Fatal("the suggestion or its hint survived the answer")
	}
	// Now it is a key: later sightings are the customer's activity.
	owners, _ := st.Observe([]Observation{{Kind: KindEmail, Value: "octocat@example.org", Predicate: PredSent, Artefact: InboxRef("email", "m9")}})
	if owners[0] != octo.ID || len(st.Suggestions()) != 0 {
		t.Fatalf("owners = %v, suggestions %d", owners, len(st.Suggestions()))
	}
}

func TestAnswerHintNoPinsThePair(t *testing.T) {
	s := newService(t)
	st := s.Store()
	octo := mustAdd(t, st, "Octo", Identity{Kind: KindGitHub, Value: "octocat"})
	seedSuggestion(t, st, KindEmail, "octocat@example.org")
	if _, err := st.DeriveHints(); err != nil {
		t.Fatal(err)
	}
	h := st.Hints(octo.ID)[0]
	got, err := s.AnswerHint(ownerV, h.ID, false)
	if err != nil || got.ID != octo.ID || got.HasIdentity("email:octocat@example.org") {
		t.Fatalf("AnswerHint no = %+v, %v", got, err)
	}
	if n, _ := st.DeriveHints(); n != 0 || len(st.Hints("")) != 0 {
		t.Fatalf("a rejected pair was asked again: %d", n)
	}
	if len(st.Suggestions()) != 1 {
		t.Fatal("answering no must leave the suggestion for someone to accept or dismiss")
	}
}

func TestAbsorbSuggestionRefusals(t *testing.T) {
	s := openStore(t)
	a := mustAdd(t, s, "A", Identity{Kind: KindGitHub, Value: "alphaone"})
	sg := seedSuggestion(t, s, KindEmail, "alphaone@example.org")
	if _, err := s.AbsorbSuggestion("p_missing", sg.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
	if _, err := s.AbsorbSuggestion(a.ID, "s_missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing suggestion = %v", err)
	}
	// Someone else takes the address by hand first: absorbing is refused.
	other := mustAdd(t, s, "B")
	if _, err := s.Accept(sg.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AbsorbSuggestion(other.ID, sg.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a suggestion that was accepted is gone: %v", err)
	}
	// Accepting closed the hints that named the suggestion.
	if _, err := s.Accept(seedSuggestion(t, s, KindEmail, "alphaone@elsewhere.org").ID, "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerHintBetweenCustomersAsksForAMerge(t *testing.T) {
	s := newService(t)
	st := s.Store()
	a := mustAdd(t, st, "A")
	b := mustAdd(t, st, "B")
	if err := st.AddHint(a.ID, b.ID, "same"); err != nil {
		t.Fatal(err)
	}
	h := st.Hints(a.ID)[0]
	if _, err := s.AnswerHint(ownerV, h.ID, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a customer-to-customer yes = %v, want ErrInvalid", err)
	}
	view, _ := s.Get(ownerV, a.ID)
	if view.Hints[0].OtherKind != "customer" || view.Hints[0].OtherValue != "B" {
		t.Fatalf("hint view = %+v", view.Hints)
	}
}
