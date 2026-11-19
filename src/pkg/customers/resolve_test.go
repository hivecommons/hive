package customers

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizeIdentity(t *testing.T) {
	cases := []struct {
		kind IdentityKind
		in   string
		want string
	}{
		{KindGitHub, " @Octo-Cat ", "octo-cat"},
		{KindEmail, "Ada@Example.COM", "ada@example.com"},
		{KindGitEmail, "1+Ada@users.noreply.github.com", "1+ada@users.noreply.github.com"},
		{KindHiveUser, "Alice", "alice"},
		{KindDiscord, "1234", "1234"},
		{KindName, "  Ada Lovelace ", "Ada Lovelace"},
	}
	for _, tc := range cases {
		if got := NormalizeIdentity(tc.kind, tc.in); got != tc.want {
			t.Errorf("NormalizeIdentity(%s, %q) = %q, want %q", tc.kind, tc.in, got, tc.want)
		}
	}
	if IdentityKey(KindGitHub, "@Ada") != "github:ada" {
		t.Fatalf("IdentityKey = %q", IdentityKey(KindGitHub, "@Ada"))
	}
}

// TestIdentityResolutionTable is the resolution contract: which inputs
// link, which only raise a hint, and which do nothing at all.
func TestIdentityResolutionTable(t *testing.T) {
	type outcome int
	const (
		links outcome = iota
		hints
		nothing
	)
	cases := []struct {
		name  string
		setup func(s *Store, ada, bo Person)
		id    Identity
		want  outcome
	}{
		{name: "exact new key links", id: Identity{Kind: KindEmail, Value: "ada@example.com", Source: "calendar_proof"}, want: links},
		{name: "already held is a no-op", id: Identity{Kind: KindGitHub, Value: "ADA"}, want: nothing},
		{name: "key held by someone else only hints", id: Identity{Kind: KindGitHub, Value: "bo"}, want: hints},
		{name: "a display name never links", id: Identity{Kind: KindName, Value: "Ada Lovelace"}, want: nothing},
		{name: "a hive user is never linked automatically", id: Identity{Kind: KindHiveUser, Value: "ada"}, want: nothing},
		{
			name: "an unlink pin blocks the link",
			setup: func(s *Store, ada, _ Person) {
				if _, _, err := s.AddIdentity(ada.ID, Identity{Kind: KindEmail, Value: "ada@example.com"}, "owner"); err != nil {
					panic(err)
				}
				if _, _, err := s.RemoveIdentity(ada.ID, "email:ada@example.com", "owner"); err != nil {
					panic(err)
				}
			},
			id:   Identity{Kind: KindEmail, Value: "ada@example.com"},
			want: nothing,
		},
		{
			name: "a distinct pin silences the hint",
			setup: func(s *Store, ada, bo Person) {
				if err := s.AddHint(ada.ID, bo.ID, "test"); err != nil {
					panic(err)
				}
				h := s.Hints(ada.ID)
				if _, err := s.ResolveHint(h[0].ID, false, "owner"); err != nil {
					panic(err)
				}
			},
			id:   Identity{Kind: KindGitHub, Value: "bo"},
			want: nothing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			ada := mustAdd(t, s, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
			bo := mustAdd(t, s, "Bo", Identity{Kind: KindGitHub, Value: "bo"})
			if tc.setup != nil {
				tc.setup(s, ada, bo)
			}
			linked, err := s.AutoLink(ada.ID, tc.id)
			if err != nil {
				t.Fatalf("AutoLink: %v", err)
			}
			got, _ := s.Get(ada.ID)
			has := tc.id.Kind != KindName && got.HasIdentity(tc.id.Key())
			nHints := len(s.Hints(ada.ID))
			switch tc.want {
			case links:
				if !linked || !has || nHints != 0 {
					t.Fatalf("linked=%v has=%v hints=%d, want a link", linked, has, nHints)
				}
				for _, i := range got.Identities {
					if i.Key() == tc.id.Key() && (i.Manual || i.Source != tc.id.Source) {
						t.Fatalf("automatic link stored as %+v", i)
					}
				}
			case hints:
				if linked || has || nHints != 1 {
					t.Fatalf("linked=%v has=%v hints=%d, want only a hint", linked, has, nHints)
				}
				if owner, _ := s.Lookup(tc.id.Kind, tc.id.Value); owner.ID != bo.ID {
					t.Fatal("the identity moved off its owner")
				}
			case nothing:
				if linked || nHints != 0 {
					t.Fatalf("linked=%v hints=%d, want nothing", linked, nHints)
				}
			}
		})
	}
	s := openStore(t)
	if _, err := s.AutoLink("p_missing", Identity{Kind: KindGitHub, Value: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AutoLink missing = %v", err)
	}
	if _, err := s.AutoLink("p_missing", Identity{Kind: "fax", Value: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("AutoLink bad kind = %v", err)
	}
}

func TestAddIdentityClaimBecomesHint(t *testing.T) {
	s := openStore(t)
	ada := mustAdd(t, s, "Ada", Identity{Kind: KindEmail, Value: "ada@example.com"})
	bo := mustAdd(t, s, "Bo")

	got, hint, err := s.AddIdentity(bo.ID, Identity{Kind: KindEmail, Value: "ADA@example.com"}, "bo")
	if err != nil {
		t.Fatalf("AddIdentity: %v", err)
	}
	if hint == nil || hint.Person != bo.ID || hint.Other != ada.ID || hint.Identity == nil {
		t.Fatalf("hint = %+v", hint)
	}
	if got.HasIdentity("email:ada@example.com") {
		t.Fatal("a claimed identity must not be linked")
	}
	if owner, _ := s.Lookup(KindEmail, "ada@example.com"); owner.ID != ada.ID {
		t.Fatal("the claim took the identity from its owner")
	}
	// Claiming again does not pile up hints.
	if _, again, _ := s.AddIdentity(bo.ID, Identity{Kind: KindEmail, Value: "ada@example.com"}, "bo"); again == nil || again.ID != hint.ID {
		t.Fatalf("second claim hint = %+v, want the first", again)
	}
	if n := len(s.Hints(ada.ID)); n != 1 {
		t.Fatalf("hints on the owner = %d, want 1", n)
	}

	// A free identity links, as a manual one.
	got, hint, err = s.AddIdentity(bo.ID, Identity{Kind: KindGitHub, Value: "@Bo"}, "bo")
	if err != nil || hint != nil || !got.HasIdentity("github:bo") {
		t.Fatalf("AddIdentity free = %+v, %+v, %v", got, hint, err)
	}
	for _, i := range got.Identities {
		if !i.Manual || i.By != "bo" || i.Source != "manual" {
			t.Fatalf("manual identity stored as %+v", i)
		}
	}
	// Adding it twice is a no-op.
	twice, _, err := s.AddIdentity(bo.ID, Identity{Kind: KindGitHub, Value: "bo"}, "bo")
	if err != nil || len(twice.Identities) != 1 {
		t.Fatalf("duplicate add = %+v, %v", twice, err)
	}
	// Names may repeat across people: a name is not a key.
	if _, h, err := s.AddIdentity(bo.ID, Identity{Kind: KindName, Value: "Ada"}, "bo"); err != nil || h != nil {
		t.Fatalf("name add = %+v, %v", h, err)
	}
	if _, ok := s.Lookup(KindName, "Ada"); ok {
		t.Fatal("a name must never resolve to a person")
	}
	if _, _, err := s.AddIdentity("p_missing", Identity{Kind: KindGitHub, Value: "x"}, "bo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
	if _, _, err := s.RemoveIdentity(bo.ID, "github:nobody", "bo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing identity = %v", err)
	}
	if _, _, err := s.RemoveIdentity("p_missing", "github:bo", "bo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove on missing person = %v", err)
	}
}

func TestManualRelinkClearsUnlinkPin(t *testing.T) {
	s := openStore(t)
	ada := mustAdd(t, s, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
	if _, removed, err := s.RemoveIdentity(ada.ID, "github:ada", "owner"); err != nil || removed.Value != "ada" {
		t.Fatalf("RemoveIdentity = %+v, %v", removed, err)
	}
	if len(s.Pins()) != 1 {
		t.Fatalf("pins = %v, want one unlink pin", s.Pins())
	}
	if linked, _ := s.AutoLink(ada.ID, Identity{Kind: KindGitHub, Value: "ada"}); linked {
		t.Fatal("the pin must block the automatic link")
	}
	if _, _, err := s.AddIdentity(ada.ID, Identity{Kind: KindGitHub, Value: "ada"}, "owner"); err != nil {
		t.Fatal(err)
	}
	if len(s.Pins()) != 0 {
		t.Fatalf("a manual re-link must clear the pin it overrides: %v", s.Pins())
	}
}

func TestObserveSuggestsOnlyKeysCountersAndFewRefs(t *testing.T) {
	s := openStore(t)
	ada := mustAdd(t, s, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var obs []Observation
	obs = append(obs, Observation{Kind: KindGitHub, Value: "Ada", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1), At: at})
	for i := 1; i <= 8; i++ {
		obs = append(obs, Observation{Kind: KindGitHub, Value: "Stranger", Predicate: PredAuthored, Artefact: IssueRef("o/r", 100+i), At: at.Add(time.Duration(i) * time.Hour)})
	}
	obs = append(obs,
		Observation{Kind: KindGitHub, Value: "stranger", Predicate: PredAuthored, Artefact: IssueRef("o/r", 101), At: at},
		Observation{Kind: KindName, Value: "Some Name", Predicate: PredAttended, Artefact: MeetingRef("m1"), At: at},
		Observation{Kind: KindHiveUser, Value: "alice", Predicate: PredLoggedIn, Artefact: AuditRef("2026-10-01"), At: at},
		Observation{Kind: "fax", Value: "1", Predicate: PredSent, Artefact: InboxRef("fax", "1"), At: at},
		Observation{Kind: KindEmail, Value: "  ", Predicate: PredSent, Artefact: InboxRef("email", "1"), At: at},
	)
	owners, err := s.Observe(obs)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(owners) != 1 || owners[0] != ada.ID {
		t.Fatalf("owners = %v, want only observation 0 -> %s", owners, ada.ID)
	}
	sugg := s.Suggestions()
	if len(sugg) != 1 {
		t.Fatalf("suggestions = %+v, want exactly the stranger", sugg)
	}
	sg := sugg[0]
	if sg.Kind != KindGitHub || sg.Value != "stranger" {
		t.Fatalf("suggestion key = %s:%s", sg.Kind, sg.Value)
	}
	if len(sg.Evidence) != MaxSuggestionRef {
		t.Fatalf("evidence = %d refs, want the cap %d", len(sg.Evidence), MaxSuggestionRef)
	}
	if sg.Counts[PredAuthored] != 8 || sg.Total() != 8 {
		t.Fatalf("counts = %v, want 8 (the repeated artefact is one sighting)", sg.Counts)
	}
	if !sg.FirstSeen.Equal(at.Add(time.Hour)) || !sg.LastSeen.Equal(at.Add(8*time.Hour)) {
		t.Fatalf("seen window = %v .. %v", sg.FirstSeen, sg.LastSeen)
	}
	if len(s.People()) != 1 {
		t.Fatal("observing must never create a customer")
	}
}

func TestAcceptDismissAndExpire(t *testing.T) {
	s := openStore(t)
	_, err := s.Observe([]Observation{
		{Kind: KindGitHub, Value: "carol", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1)},
		{Kind: KindEmail, Value: "dan@example.com", Predicate: PredInvited, Artefact: EventRef("e1")},
		{Kind: KindEmail, Value: "erin@example.com", Predicate: PredAttended, Artefact: MeetingRef("m1")},
		{Kind: KindEmail, Value: "erin@example.com", Predicate: PredAttended, Artefact: MeetingRef("m2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	byValue := map[string]Suggestion{}
	for _, sg := range s.Suggestions() {
		byValue[sg.Value] = sg
	}
	if len(byValue) != 3 {
		t.Fatalf("suggestions = %v", byValue)
	}

	carol, err := s.Accept(byValue["carol"].ID, "owner")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if carol.Origin != OriginAccepted || carol.Status != StatusCustomer || !carol.HasIdentity("github:carol") {
		t.Fatalf("accepted person = %+v", carol)
	}
	if carol.Identities[0].By != "owner" || !carol.Identities[0].Manual {
		t.Fatalf("accept is a human decision and must say who: %+v", carol.Identities[0])
	}
	if _, err := s.Accept(byValue["carol"].ID, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepting twice = %v", err)
	}

	if err := s.Dismiss(byValue["dan@example.com"].ID); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if !s.Forgotten(KindEmail, "dan@example.com") {
		t.Fatal("a dismissed key must be tombstoned, not kept readable")
	}
	if _, err := s.Observe([]Observation{{Kind: KindEmail, Value: "dan@example.com", Predicate: PredInvited, Artefact: EventRef("e2")}}); err != nil {
		t.Fatal(err)
	}
	for _, sg := range s.Suggestions() {
		if sg.Value == "dan@example.com" {
			t.Fatal("a dismissed suggestion came back")
		}
	}
	if err := s.Dismiss("s_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Dismiss missing = %v", err)
	}

	// A suggestion expires with its evidence.
	gone := map[string]bool{MeetingRef("m1"): true}
	removed, err := s.ExpireSuggestions(func(ref string) bool { return !gone[ref] })
	if err != nil || removed != 0 {
		t.Fatalf("ExpireSuggestions = %d, %v", removed, err)
	}
	left := s.Suggestions()
	if len(left) != 1 || len(left[0].Evidence) != 1 || left[0].Evidence[0] != MeetingRef("m2") {
		t.Fatalf("after one purge: %+v", left)
	}
	gone[MeetingRef("m2")] = true
	removed, err = s.ExpireSuggestions(func(ref string) bool { return !gone[ref] })
	if err != nil || removed != 1 || len(s.Suggestions()) != 0 {
		t.Fatalf("ExpireSuggestions = %d, %v; left %v", removed, err, s.Suggestions())
	}
}

func TestMergeAndUnmergeRoundTrip(t *testing.T) {
	s := openStore(t)
	ada := mustAdd(t, s, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
	alt := mustAdd(t, s, "A. Lovelace", Identity{Kind: KindEmail, Value: "ada@example.com"}, Identity{Kind: KindGitHub, Value: "ada-alt"})
	if _, err := s.SetTag(ada.ID, TagMaintainer, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTag(alt.ID, TagStakeholder, true, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGoal(alt.ID, "get the importer fixed", "owner"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(ada.ID)
	altBefore, _ := s.Get(alt.ID)

	merged, err := s.Merge(ada.ID, alt.ID, "owner")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Identities) != 3 || !merged.HasTag(TagStakeholder) || !merged.HasTag(TagMaintainer) || len(merged.Goals) != 1 {
		t.Fatalf("merged = %+v", merged)
	}
	if _, err := s.Get(alt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the merged-in person still exists")
	}
	if owner, _ := s.Lookup(KindEmail, "ada@example.com"); owner.ID != ada.ID {
		t.Fatal("the merged identity does not resolve to the target")
	}
	if len(merged.MergedFrom) != 1 || merged.MergedFrom[0].By != "owner" {
		t.Fatalf("merge history = %+v", merged.MergedFrom)
	}

	dst, src, err := s.Unmerge(ada.ID, alt.ID)
	if err != nil {
		t.Fatalf("Unmerge: %v", err)
	}
	if len(dst.Identities) != len(before.Identities) || dst.HasTag(TagStakeholder) || len(dst.Goals) != 0 || len(dst.MergedFrom) != 0 {
		t.Fatalf("target after unmerge = %+v, want what it was: %+v", dst, before)
	}
	if src.ID != alt.ID || len(src.Identities) != len(altBefore.Identities) || !src.HasTag(TagStakeholder) || len(src.Goals) != 1 {
		t.Fatalf("restored = %+v, want %+v", src, altBefore)
	}
	if owner, _ := s.Lookup(KindEmail, "ada@example.com"); owner.ID != alt.ID {
		t.Fatal("the identity did not go back")
	}
	// The undo is a decision: the pair is not offered as the same again.
	if err := s.AddHint(ada.ID, alt.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if len(s.Hints(ada.ID)) != 0 {
		t.Fatal("an unmerged pair was hinted again")
	}

	if _, err := s.Merge(ada.ID, ada.ID, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self merge = %v", err)
	}
	if _, err := s.Merge(ada.ID, "p_missing", "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge missing src = %v", err)
	}
	if _, err := s.Merge("p_missing", ada.ID, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge missing dst = %v", err)
	}
	if _, _, err := s.Unmerge(ada.ID, alt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmerge twice = %v", err)
	}
	if _, _, err := s.Unmerge("p_missing", alt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmerge missing = %v", err)
	}
}

func TestTwoDashboardUsersNeverMerge(t *testing.T) {
	s := openStore(t)
	alice, _ := s.EnsureHiveUser("alice")
	bob, _ := s.EnsureHiveUser("bob")
	if _, err := s.Merge(alice.ID, bob.ID, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("merging two dashboard users = %v, want ErrConflict", err)
	}
	// A dashboard user folded into a hand-added profile keeps being "me".
	ext := mustAdd(t, s, "Alice external", Identity{Kind: KindGitHub, Value: "alice-gh"})
	merged, err := s.Merge(ext.ID, alice.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if merged.Origin != OriginHiveUser {
		t.Fatalf("origin = %s, want hive_user", merged.Origin)
	}
	me, err := s.EnsureHiveUser("alice")
	if err != nil || me.ID != ext.ID {
		t.Fatalf("me after merge = %s, %v; want %s", me.ID, err, ext.ID)
	}
	dst, src, err := s.Unmerge(ext.ID, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Origin != OriginManual || src.Identity(KindHiveUser) != "alice" {
		t.Fatalf("after unmerge dst=%+v src=%+v", dst, src)
	}
}

func TestSplitMovesIdentitiesAndPins(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada",
		Identity{Kind: KindGitHub, Value: "ada"},
		Identity{Kind: KindEmail, Value: "shared@example.com"})
	orig, fresh, err := s.Split(p.ID, []string{"email:shared@example.com"}, "", "owner")
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(orig.Identities) != 1 || !fresh.HasIdentity("email:shared@example.com") || fresh.Name != "shared@example.com" {
		t.Fatalf("split = %+v / %+v", orig, fresh)
	}
	if owner, _ := s.Lookup(KindEmail, "shared@example.com"); owner.ID != fresh.ID {
		t.Fatal("the split identity does not resolve to the new person")
	}
	// Pins keep the sweep from undoing it.
	if linked, _ := s.AutoLink(p.ID, Identity{Kind: KindEmail, Value: "shared@example.com"}); linked {
		t.Fatal("automatic resolution re-linked a split identity")
	}
	if len(s.Hints(p.ID)) != 0 {
		t.Fatal("a split pair must not be hinted as the same person")
	}

	if _, _, err := s.Split(p.ID, []string{"github:ada"}, "x", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("splitting off the last identity = %v", err)
	}
	if _, _, err := s.Split(p.ID, []string{"github:nobody"}, "x", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("splitting nothing = %v", err)
	}
	if _, _, err := s.Split("p_missing", []string{"github:ada"}, "x", "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("split missing = %v", err)
	}
	me, _ := s.EnsureHiveUser("alice")
	if _, _, err := s.AddIdentity(me.ID, Identity{Kind: KindGitHub, Value: "alice"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Split(me.ID, []string{"hive_user:alice"}, "x", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("splitting off a hive user = %v", err)
	}
}

func TestResolveHint(t *testing.T) {
	s := openStore(t)
	a := mustAdd(t, s, "A")
	b := mustAdd(t, s, "B")
	if err := s.AddHint(a.ID, b.ID, "same email local part"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddHint("p_missing", b.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hint on missing person = %v", err)
	}
	h := s.Hints(b.ID)
	if len(h) != 1 || h[0].Reason != "same email local part" {
		t.Fatalf("hints = %+v", h)
	}
	got, err := s.ResolveHint(h[0].ID, true, "owner")
	if err != nil || got.Other != b.ID {
		t.Fatalf("ResolveHint = %+v, %v", got, err)
	}
	if len(s.Hints("")) != 0 || len(s.Pins()) != 0 {
		t.Fatal("answering yes closes the hint and pins nothing")
	}
	if _, err := s.ResolveHint(h[0].ID, true, "owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolving twice = %v", err)
	}
}
