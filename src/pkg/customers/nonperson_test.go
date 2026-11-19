package customers

import (
	"errors"
	"testing"
)

func TestIsNonPersonAddress(t *testing.T) {
	own := []string{"hive@hosted-x.hive.example.dev"}
	for addr, want := range map[string]bool{
		"b43dc28a888d316aa1fe4a47bf3038cd1bca7bbb2b5cdbf5e7ecb9cc10672a95@group.calendar.google.com": true,
		"room-1@resource.calendar.google.com":                                                        true,
		"hivecommons-dev@googlegroups.com":                                                           true,
		"Hive@Hosted-X.hive.example.dev":                                                             true,
		"noreply@example.com":                                                                        true,
		"no-reply+x@example.com":                                                                     true,
		"do-not-reply@example.com":                                                                   true,
		"mailer-daemon@example.com":                                                                  true,
		"bounces-123@example.com":                                                                    true,
		"notifications@example.com":                                                                  true,
		"ada@example.com":                                                                            false,
		"hive@elsewhere.example.com":                                                                 false,
		"nic1@ibm.com":                                                                               false,
		"ada@notgooglegroups.com":                                                                    false,
		"not an address":                                                                             false,
		"":                                                                                           false,
		"someone@calendar.google.com.example.org":                                                    false,
	} {
		if got := IsNonPersonAddress(addr, own...); got != want {
			t.Errorf("IsNonPersonAddress(%q) = %v, want %v", addr, got, want)
		}
	}
	if !LooksLikeGitHubLogin("danathar") || LooksLikeGitHubLogin("a@b.c") || LooksLikeGitHubLogin("ibmid:x") {
		t.Error("LooksLikeGitHubLogin")
	}
}

// TestSweepNeverSuggestsNonPeopleAndExpiresStoredOnes: the deny rule keeps
// group, list, service and own addresses out of Suggested, and a suggestion
// stored before the rule expires through the sweep rather than being
// deleted blindly. A person who owns such an address keeps their edges.
func TestSweepNeverSuggestsNonPeopleAndExpiresStoredOnes(t *testing.T) {
	svc := newSweepService(t, goneSet{})
	const group = "abc@group.calendar.google.com"
	const own = "hive@hosted-x.hive.example.dev"
	// Stored by an older sweep, before the rule.
	if _, err := svc.Store().Observe([]Observation{
		{Kind: KindEmail, Value: group, Predicate: PredInvited, Artefact: EventRef("e1"), At: sweepAt},
		{Kind: KindEmail, Value: own, Predicate: PredInvited, Artefact: EventRef("e1"), At: sweepAt},
		{Kind: KindEmail, Value: "ada@example.com", Predicate: PredInvited, Artefact: EventRef("e1"), At: sweepAt},
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(svc.Store().Suggestions()); n != 3 {
		t.Fatalf("setup: %d suggestions", n)
	}
	in := SweepInput{OwnAddresses: []string{own}, Sightings: []Sighting{
		sight(KindEmail, group, PredInvited, EventRef("e1"), "calendar"),
		sight(KindEmail, own, PredInvited, EventRef("e1"), "calendar"),
		sight(KindEmail, "list@googlegroups.com", PredInvited, EventRef("e1"), "calendar"),
		sight(KindEmail, "ada@example.com", PredInvited, EventRef("e1"), "calendar"),
	}}
	res, err := svc.Sweep(in)
	if err != nil {
		t.Fatal(err)
	}
	got := svc.Store().Suggestions()
	if len(got) != 1 || got[0].Value != "ada@example.com" {
		t.Fatalf("suggestions = %+v, want only ada", got)
	}
	if res.ExpiredSuggestions != 2 {
		t.Fatalf("expired = %d, want the 2 stored non-people", res.ExpiredSuggestions)
	}
	// Nothing was tombstoned: allowing the address again brings it back.
	in.OwnAddresses = nil
	in.Sightings = []Sighting{sight(KindEmail, own, PredInvited, EventRef("e1"), "calendar")}
	if _, err := svc.Sweep(in); err != nil {
		t.Fatal(err)
	}
	if _, ok := suggestionFor(svc, KindEmail, own); !ok {
		t.Fatal("an expired suggestion must not be tombstoned")
	}
}

func suggestionFor(svc *Service, kind IdentityKind, value string) (Suggestion, bool) {
	for _, sg := range svc.Store().Suggestions() {
		if sg.Kind == kind && sg.Value == NormalizeIdentity(kind, value) {
			return sg, true
		}
	}
	return Suggestion{}, false
}

// TestLinkedIdentityIsNeverSuggested: an identity a hive user owns is that
// person's, and a suggestion that predates the link is removed.
func TestLinkedIdentityIsNeverSuggested(t *testing.T) {
	svc := newSweepService(t, goneSet{})
	if _, err := svc.Store().Observe([]Observation{
		{Kind: KindGitHub, Value: "danathar", Predicate: PredAuthored, Artefact: PRRef("o/r", 1), At: sweepAt},
		{Kind: KindEmail, Value: "me@example.com", Predicate: PredInvited, Artefact: EventRef("e1"), At: sweepAt},
	}); err != nil {
		t.Fatal(err)
	}
	in := SweepInput{
		Users: []HiveUser{{Name: "danathar"}, {Name: "olivia", Maintainer: true, Rule: "owner"}},
		Links: []ProvenLink{
			{User: "danathar", Identity: Identity{Kind: KindGitHub, Value: "danathar", Source: "access_list"}},
			{User: "olivia", Identity: Identity{Kind: KindEmail, Value: "Me@Example.com", Source: "calendar_proof"}},
		},
		Sightings: []Sighting{
			sight(KindGitHub, "Danathar", PredAuthored, PRRef("o/r", 1), "github_poll"),
			sight(KindEmail, "me@example.com", PredInvited, EventRef("e1"), "calendar"),
		},
	}
	res, err := svc.Sweep(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 2 || len(svc.Store().Suggestions()) != 0 {
		t.Fatalf("linked=%d suggestions=%+v", res.Linked, svc.Store().Suggestions())
	}
	// A suggestion left behind for an owned key (for example one stored
	// before the identity was added by hand) expires on the next sweep.
	st := svc.Store()
	st.mu.Lock()
	st.sugg["github:danathar"] = Suggestion{ID: "s_stale", Kind: KindGitHub, Value: "danathar", Evidence: []string{PRRef("o/r", 1)}}
	st.mu.Unlock()
	if _, err := svc.Sweep(in); err != nil {
		t.Fatal(err)
	}
	if len(st.Suggestions()) != 0 {
		t.Fatalf("an owned identity is still suggested: %+v", st.Suggestions())
	}
}

func attachSetup(t *testing.T) (*Service, Suggestion) {
	t.Helper()
	svc := newSweepService(t, goneSet{})
	if _, err := svc.Sweep(SweepInput{
		Users: []HiveUser{{Name: "olivia", Maintainer: true, Rule: "owner"}},
		Sightings: []Sighting{
			sight(KindGitHub, "ada-gh", PredAuthored, PRRef("o/r", 5), "github_poll"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	sg, ok := suggestionFor(svc, KindGitHub, "ada-gh")
	if !ok {
		t.Fatal("setup: no suggestion")
	}
	return svc, sg
}

func TestAcceptIntoAttachesToAnExistingCustomer(t *testing.T) {
	svc, sg := attachSetup(t)
	ada, err := svc.Add(ownerV, "Ada", []Identity{{Kind: KindEmail, Value: "ada@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	before := len(svc.Store().People())
	got, err := svc.AcceptInto(ownerV, sg.ID, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != ada.ID || got.Identity(KindGitHub) != "ada-gh" {
		t.Fatalf("result = %+v", got.Person)
	}
	if len(svc.Store().People()) != before {
		t.Fatalf("people %d -> %d: attaching must never create a duplicate", before, len(svc.Store().People()))
	}
	if _, ok := suggestionFor(svc, KindGitHub, "ada-gh"); ok {
		t.Fatal("the suggestion is still in Suggested")
	}
	// The identity is a manual decision of Ada's and is not suggested again,
	// and the next sweep writes the activity onto her.
	var ident Identity
	for _, i := range got.Identities {
		if i.Kind == KindGitHub {
			ident = i
		}
	}
	if !ident.Manual {
		t.Fatalf("identity = %+v, want manual", ident)
	}
	if _, err := svc.Sweep(SweepInput{Sightings: []Sighting{sight(KindGitHub, "ada-gh", PredAuthored, PRRef("o/r", 5), "github_poll")}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := suggestionFor(svc, KindGitHub, "ada-gh"); ok {
		t.Fatal("a pinned identity was suggested again")
	}
	if c, _ := svc.index.Counts(ada.ID); c[PredAuthored] != 1 {
		t.Fatalf("counts = %v, the activity did not move to Ada", c)
	}
	// Undo with the merge's own undo.
	if len(got.MergedFrom) != 1 {
		t.Fatalf("merge history = %+v", got.MergedFrom)
	}
	back, err := svc.Unmerge(ownerV, ada.ID, got.MergedFrom[0].Person.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Identity(KindGitHub) != "" || len(back.MergedFrom) != 0 {
		t.Fatalf("after undo Ada = %+v", back.Person)
	}
	if p, ok := svc.Store().Lookup(KindGitHub, "ada-gh"); !ok || p.ID == ada.ID {
		t.Fatal("after undo the identity must belong to the restored person")
	}
}

func TestAcceptIntoMeAndRoles(t *testing.T) {
	svc, sg := attachSetup(t)
	// A read-only viewer cannot see suggestions, so cannot attach one.
	if _, err := svc.AcceptInto(readV, sg.ID, MeID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only AcceptInto = %v, want ErrForbidden", err)
	}
	if _, ok := suggestionFor(svc, KindGitHub, "ada-gh"); !ok {
		t.Fatal("a refused attach consumed the suggestion")
	}
	// A read-write user may attach to their own Me.
	got, err := svc.AcceptInto(rwV, sg.ID, MeID)
	if err != nil || !got.Me || got.Identity(KindGitHub) != "ada-gh" {
		t.Fatalf("read-write AcceptInto me = %+v, %v", got, err)
	}
	// Unknown targets and suggestions fail without creating anyone.
	before := len(svc.Store().People())
	if _, err := svc.AcceptInto(ownerV, "s_missing", MeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing suggestion = %v", err)
	}
	if _, err := svc.AcceptInto(ownerV, sg.ID, "p_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target = %v", err)
	}
	if len(svc.Store().People()) != before {
		t.Fatal("a failed attach created a person")
	}
}
