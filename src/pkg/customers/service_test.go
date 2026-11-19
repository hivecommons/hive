package customers

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var (
	ownerV = Viewer{User: "olivia", Full: true, Owner: true}
	rwV    = Viewer{User: "rita", Full: true}
	readV  = Viewer{User: "reed"}
)

func newService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "customers"), newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenIndex(filepath.Join(dir, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return NewService(store, index, nil)
}

func TestEveryRoleSeesTheirOwnMe(t *testing.T) {
	s := newService(t)
	ids := map[string]bool{}
	for _, v := range []Viewer{ownerV, rwV, readV} {
		me, err := s.Get(v, MeID)
		if err != nil {
			t.Fatalf("%s: Get me: %v", v.User, err)
		}
		if !me.Me || me.Identity(KindHiveUser) != v.User || me.Origin != OriginHiveUser {
			t.Fatalf("%s: me = %+v", v.User, me)
		}
		ids[me.ID] = true
		// The real id works as well as the alias.
		again, err := s.Get(v, me.ID)
		if err != nil || !again.Me {
			t.Fatalf("%s: Get by own id = %+v, %v", v.User, again, err)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("three users share profiles: %v", ids)
	}
	if _, err := s.Get(Viewer{}, MeID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer with no name = %v, want ErrForbidden", err)
	}
	if _, err := s.List(Viewer{User: "  "}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("List with no name = %v, want ErrForbidden", err)
	}
}

// TestReadOnlyReachesOnlyItself is the access contract: without the full
// view a viewer cannot see or change anyone else through any method, and an
// id they may not see answers exactly as one that does not exist.
func TestReadOnlyReachesOnlyItself(t *testing.T) {
	s := newService(t)
	other, err := s.Add(ownerV, "Ada", []Identity{{Kind: KindEmail, Value: "ada@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	ownerMe, _ := s.Get(ownerV, MeID)
	if _, err := s.Store().Observe([]Observation{{Kind: KindGitHub, Value: "stranger", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1)}}); err != nil {
		t.Fatal(err)
	}
	sid := s.Store().Suggestions()[0].ID

	list, err := s.List(readV)
	if err != nil {
		t.Fatal(err)
	}
	if list.Full || list.Owner || len(list.People) != 0 || len(list.Suggestions) != 0 || !list.Me.Me {
		t.Fatalf("read-only list leaks: %+v", list)
	}

	_, missing := s.Get(readV, "p_doesnotexist")
	for _, id := range []string{other.ID, ownerMe.ID} {
		if _, err := s.Get(readV, id); !errors.Is(err, ErrNotFound) || err.Error() != missing.Error() {
			t.Fatalf("Get(%s) as read-only = %v, want the same answer as a missing id (%v)", id, err, missing)
		}
		if s.CanSee(readV, id) {
			t.Fatalf("CanSee(%s) as read-only", id)
		}
		for _, op := range []string{OpName, OpNotes, OpIdentityAdd, OpIdentityRemove, OpTag, OpGoalAdd, OpGoalEdit, OpGoalConfirm, OpGoalReject, OpSatisfactionSet, OpSatisfactionConfirm, OpSatisfactionReject} {
			_, err := s.Edit(readV, id, Edit{Op: op, Name: "x", Notes: "x", Kind: "github", Value: "x", Key: "email:ada@example.com", Tag: "x", State: TagOn, Text: "x", Level: "neutral"})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Edit(%s, %s) as read-only = %v, want ErrNotFound", id, op, err)
			}
		}
		if _, err := s.Forget(readV, id); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Forget(%s) as read-only = %v", id, err)
		}
	}
	if _, err := s.Add(readV, "Sneaky", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Add as read-only = %v", err)
	}
	if _, err := s.Accept(readV, sid); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Accept as read-only = %v", err)
	}
	if err := s.Dismiss(readV, sid); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Dismiss as read-only = %v", err)
	}
	if _, err := s.Forget(readV, MeID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a read-only user forgetting themselves = %v", err)
	}
	// Nothing changed for the people the read-only user tried to reach.
	after, _ := s.Get(ownerV, other.ID)
	if after.Name != "Ada" || len(after.Identities) != 1 || len(after.Tags) != 0 || len(after.Goals) != 0 {
		t.Fatalf("a refused edit changed the target: %+v", after)
	}
	if len(s.Store().Suggestions()) != 1 {
		t.Fatal("a refused accept or dismiss changed the suggestions")
	}
}

func TestReadOnlyEditsItsOwnNameIdentitiesTagsAndGoals(t *testing.T) {
	s := newService(t)
	steps := []Edit{
		{Op: OpName, Name: "Reed R."},
		{Op: OpTag, Tag: TagEndUser, State: TagOn},
		{Op: OpGoalAdd, Text: "Get exports working"},
		{Op: OpIdentityAdd, Kind: "name", Value: "Reed Richards"},
	}
	var me PersonView
	var err error
	for _, e := range steps {
		if me, err = s.Edit(readV, MeID, e); err != nil {
			t.Fatalf("self edit %s: %v", e.Op, err)
		}
	}
	if me.Name != "Reed R." || !me.HasTag(TagEndUser) || len(me.Goals) != 1 || me.Goals[0].By != "reed" {
		t.Fatalf("me after self edits = %+v", me)
	}
	gid := me.Goals[0].ID
	if me, err = s.Edit(readV, MeID, Edit{Op: OpGoalEdit, GoalID: gid, Text: "Get CSV exports working"}); err != nil || me.Goals[0].Text != "Get CSV exports working" {
		t.Fatalf("goal edit = %+v, %v", me.Goals, err)
	}
	if _, err := s.Store().SuggestGoals(me.ID, []GoalSuggestion{{Text: "Wants an API", Evidence: []string{IssueRef("o/r", 3)}}}); err != nil {
		t.Fatal(err)
	}
	me, _ = s.Get(readV, MeID)
	var suggested string
	for _, g := range me.Goals {
		if g.State == StateSuggested {
			suggested = g.ID
		}
	}
	if suggested == "" {
		t.Fatal("a read-only user must see goal suggestions about themselves")
	}
	if me, err = s.Edit(readV, MeID, Edit{Op: OpGoalConfirm, GoalID: suggested}); err != nil || len(me.ConfirmedGoals()) != 2 {
		t.Fatalf("goal confirm = %+v, %v", me.Goals, err)
	}
	if me, err = s.Edit(readV, MeID, Edit{Op: OpGoalReject, GoalID: gid}); err != nil || len(me.Goals) != 1 {
		t.Fatalf("goal reject = %+v, %v", me.Goals, err)
	}
	if me, err = s.Edit(readV, MeID, Edit{Op: OpTag, Tag: TagEndUser, State: TagAuto}); err != nil || me.HasTag(TagEndUser) {
		t.Fatalf("tag reset = %+v, %v", me.Tags, err)
	}

	// Not theirs to change: notes and the satisfaction assessment.
	for _, e := range []Edit{
		{Op: OpNotes, Notes: "I am great"},
		{Op: OpSatisfactionSet, Level: "satisfied"},
		{Op: OpSatisfactionConfirm},
		{Op: OpSatisfactionReject},
	} {
		if _, err := s.Edit(readV, MeID, e); !errors.Is(err, ErrForbidden) {
			t.Fatalf("self %s as read-only = %v, want ErrForbidden", e.Op, err)
		}
	}
	for _, e := range []Edit{{Op: "merge"}, {Op: OpTag, Tag: "x", State: "maybe"}} {
		if _, err := s.Edit(ownerV, MeID, e); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad edit %+v = %v", e, err)
		}
	}
}

func TestReadOnlyViewHidesMaintainerMaterial(t *testing.T) {
	s := newService(t)
	me, _ := s.Get(readV, MeID)
	if _, err := s.Edit(ownerV, me.ID, Edit{Op: OpNotes, Notes: "churn risk, call them"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Edit(ownerV, me.ID, Edit{Op: OpSatisfactionSet, Level: string(LevelAtRisk)}); err != nil {
		t.Fatal(err)
	}
	other, _ := s.Add(ownerV, "Reed alt", []Identity{{Kind: KindGitHub, Value: "reed-gh"}})
	if err := s.Store().AddHint(me.ID, other.ID, "same name"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store().Merge(me.ID, other.ID, "olivia"); err != nil {
		t.Fatal(err)
	}
	third, _ := s.Add(ownerV, "Third", nil)
	if err := s.Store().AddHint(me.ID, third.ID, "same name"); err != nil {
		t.Fatal(err)
	}

	mine, err := s.Get(readV, MeID)
	if err != nil {
		t.Fatal(err)
	}
	if mine.Notes != "" || mine.Satisfaction.Level != "" || len(mine.MergedFrom) != 0 || len(mine.Hints) != 0 || mine.Counts != nil {
		t.Fatalf("read-only view carries maintainer material: %+v", mine)
	}
	full, err := s.Get(ownerV, me.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Notes == "" || full.Satisfaction.Level != LevelAtRisk || len(full.MergedFrom) != 1 || len(full.Hints) != 1 {
		t.Fatalf("full view is missing material: %+v", full)
	}
	if full.Me {
		t.Fatal("another person's profile is not the owner's me")
	}
}

// TestReadOnlyIdentityIsAClaimNotAKey: what a read-only user says about
// themselves is shown on their profile and does nothing else, and the
// answer is the same whether or not someone else holds the identity.
func TestReadOnlyIdentityIsAClaimNotAKey(t *testing.T) {
	s := newService(t)
	ada, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindEmail, Value: "ada@example.com"}})

	taken, err := s.Edit(readV, MeID, Edit{Op: OpIdentityAdd, Kind: "email", Value: "ADA@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	free, err := s.Edit(readV, MeID, Edit{Op: OpIdentityAdd, Kind: "email", Value: "free@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if taken.Hinted || free.Hinted {
		t.Fatal("the answer must not say whether someone else holds the identity")
	}
	claims := 0
	for _, i := range free.Identities {
		if i.Kind == KindEmail {
			claims++
			if !i.Claimed {
				t.Fatalf("a read-only user's identity must be an unconfirmed claim: %+v", i)
			}
		}
	}
	if claims != 2 {
		t.Fatalf("both claims must show on the profile alike, got %d", claims)
	}
	if owner, _ := s.Store().Lookup(KindEmail, "ada@example.com"); owner.ID != ada.ID {
		t.Fatal("a claim took an identity from its owner")
	}
	if _, ok := s.Store().Lookup(KindEmail, "free@example.com"); ok {
		t.Fatal("a claim must not resolve to anyone")
	}
	// A claim attracts no activity: the address is still unowned.
	owners, _ := s.Store().Observe([]Observation{{Kind: KindEmail, Value: "free@example.com", Predicate: PredSent, Artefact: InboxRef("email", "m1")}})
	if len(owners) != 0 {
		t.Fatalf("a claim attracted edges: %v", owners)
	}
	if len(s.Store().Hints("")) != 0 {
		t.Fatal("a claim must not raise a hint by itself")
	}

	// Someone with the full view confirms the free one: now it is a key.
	me := free
	confirmed, err := s.Edit(rwV, me.ID, Edit{Op: OpIdentityAdd, Kind: "email", Value: "free@example.com"})
	if err != nil || confirmed.Hinted || !confirmed.HasIdentity("email:free@example.com") {
		t.Fatalf("confirm = %+v, %v", confirmed, err)
	}
	if owner, ok := s.Store().Lookup(KindEmail, "free@example.com"); !ok || owner.ID != me.ID {
		t.Fatal("a confirmed claim must resolve")
	}
	// Confirming the taken one links nothing and asks the question.
	conflict, err := s.Edit(rwV, me.ID, Edit{Op: OpIdentityAdd, Kind: "email", Value: "ada@example.com"})
	if err != nil || !conflict.Hinted || conflict.HasIdentity("email:ada@example.com") {
		t.Fatalf("confirming a taken claim = %+v, %v", conflict, err)
	}
	// The claimant can withdraw a claim; that pins nothing.
	if _, err := s.Edit(readV, MeID, Edit{Op: OpIdentityRemove, Key: "email:ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(s.Store().Pins()) != 0 {
		t.Fatalf("withdrawing a claim pinned something: %v", s.Store().Pins())
	}
	// A hive user identity can never be claimed.
	if _, err := s.Edit(readV, MeID, Edit{Op: OpIdentityAdd, Kind: "hive_user", Value: "olivia"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("claiming a hive user = %v", err)
	}
	// Forgetting the claimant does not tombstone what they only claimed.
	if _, err := s.Edit(readV, MeID, Edit{Op: OpIdentityAdd, Kind: "github", Value: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ownerV, me.ID); err != nil {
		t.Fatal(err)
	}
	if s.Store().Forgotten(KindGitHub, "someone-else") {
		t.Fatal("forgetting a claimant tombstoned an identity they only claimed")
	}
	if !s.Store().Forgotten(KindEmail, "free@example.com") {
		t.Fatal("a confirmed identity must be tombstoned on forget")
	}
}

func TestAutoLinkUpgradesAClaim(t *testing.T) {
	s := newService(t)
	me, err := s.Edit(readV, MeID, Edit{Op: OpIdentityAdd, Kind: "github", Value: "reed"})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := s.Store().AutoLink(me.ID, Identity{Kind: KindGitHub, Value: "reed", Source: "device_flow"})
	if err != nil || !linked {
		t.Fatalf("a proven link must upgrade a claim: %v, %v", linked, err)
	}
	got, _ := s.Store().Get(me.ID)
	n := 0
	for _, i := range got.Identities {
		if i.Kind == KindGitHub {
			n++
			if i.Claimed || i.Source != "device_flow" {
				t.Fatalf("upgraded identity = %+v", i)
			}
		}
	}
	if n != 1 {
		t.Fatalf("the claim was duplicated: %d github identities", n)
	}
}

func TestFullViewersManageOwnerForgets(t *testing.T) {
	s := newService(t)
	ada, err := s.Add(rwV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}, {Kind: KindEmail, Value: "ada@example.com"}})
	if err != nil {
		t.Fatalf("read-write Add: %v", err)
	}
	if _, err := s.Index().Put([]Edge{
		edge(ada.ID, PredAuthored, IssueRef("o/r", 1), "github:ada", 1),
		edge(ada.ID, PredSent, InboxRef("email", "m1"), "email:ada@example.com", 2),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(rwV, ada.ID)
	if err != nil || got.Counts[PredAuthored] != 1 || got.Counts[PredSent] != 1 {
		t.Fatalf("Get with counts = %+v, %v", got.Counts, err)
	}
	// Removing an identity drops exactly the edges it produced.
	got, err = s.Edit(rwV, ada.ID, Edit{Op: OpIdentityRemove, Key: "email:ada@example.com"})
	if err != nil || got.Counts[PredSent] != 0 || got.Counts[PredAuthored] != 1 {
		t.Fatalf("after identity remove = %+v, %v", got.Counts, err)
	}
	if _, err := s.Edit(rwV, ada.ID, Edit{Op: OpSatisfactionSet, Level: "at_risk"}); err != nil {
		t.Fatalf("read-write satisfaction: %v", err)
	}
	if _, err := s.Edit(rwV, ada.ID, Edit{Op: OpNotes, Notes: "met at the summit"}); err != nil {
		t.Fatalf("read-write notes: %v", err)
	}

	list, err := s.List(rwV)
	if err != nil || !list.Full || list.Owner || len(list.People) != 1 || list.People[0].ID != ada.ID {
		t.Fatalf("read-write list = %+v, %v", list, err)
	}
	if ol, _ := s.List(ownerV); !ol.Owner || len(ol.People) != 2 {
		t.Fatalf("owner list = %+v", ol)
	}

	// Forget is the owner's alone.
	if _, err := s.Forget(rwV, ada.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-write Forget = %v, want ErrForbidden", err)
	}
	if _, err := s.Forget(Viewer{User: "x", Owner: true}, ada.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Forget without the full view = %v", err)
	}
	n, err := s.Forget(ownerV, ada.ID)
	if err != nil || n != 1 {
		t.Fatalf("owner Forget = %d, %v; want 1 edge removed", n, err)
	}
	if _, err := s.Get(ownerV, ada.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after forget = %v", err)
	}
	if s.Index().Len() != 0 {
		t.Fatalf("edges survive a forget: %d", s.Index().Len())
	}
	if !s.Store().Forgotten(KindGitHub, "ada") {
		t.Fatal("forget left no tombstone")
	}
	if _, err := s.Forget(ownerV, "p_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget missing = %v", err)
	}
}

func TestAcceptAndDismissNeedTheFullView(t *testing.T) {
	s := newService(t)
	if _, err := s.Store().Observe([]Observation{
		{Kind: KindGitHub, Value: "carol", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1)},
		{Kind: KindGitHub, Value: "dan", Predicate: PredAuthored, Artefact: IssueRef("o/r", 2)},
	}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, sg := range s.Store().Suggestions() {
		ids[sg.Value] = sg.ID
		if strings.Contains(sg.ID, sg.Value) {
			t.Fatalf("suggestion id %q is derived from the identity", sg.ID)
		}
	}
	carol, err := s.Accept(rwV, ids["carol"])
	if err != nil || carol.Origin != OriginAccepted || carol.Identities[0].By != "rita" {
		t.Fatalf("Accept = %+v, %v", carol, err)
	}
	if err := s.Dismiss(rwV, ids["dan"]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(rwV, ids["dan"]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("accepting a dismissed suggestion = %v", err)
	}
	if len(s.Store().Suggestions()) != 0 {
		t.Fatalf("suggestions left: %v", s.Store().Suggestions())
	}
}

func TestServiceWithoutIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "customers"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(store, nil, Resolvers{})
	if s.Index() != nil || s.Resolvers() == nil {
		t.Fatal("accessors")
	}
	ada, err := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Edit(ownerV, ada.ID, Edit{Op: OpIdentityRemove, Key: "github:ada"}); err != nil || got.Counts != nil {
		t.Fatalf("identity remove without an index = %+v, %v", got, err)
	}
	if n, err := s.Forget(ownerV, ada.ID); err != nil || n != 0 {
		t.Fatalf("Forget without an index = %d, %v", n, err)
	}
}

// privateTo is a resolver that lets only one user see each listed artefact.
type privateTo map[string]string

func (p privateTo) Exists(string) bool { return true }

func (p privateTo) Visible(ref string, v Viewer) bool {
	owner, restricted := p[ref]
	return !restricted || owner == v.User
}

func TestEdgesAndArtefactPeopleFollowTheViewer(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "customers"), newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenIndex(filepath.Join(dir, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	private := MeetingRef("owners-only")
	s := NewService(store, index, Resolvers{RefMeeting: privateTo{private: "olivia"}})

	reed, _ := s.Get(readV, MeID)
	ada, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	shared := IssueRef("o/r", 1)
	if _, err := index.Put([]Edge{
		edge(reed.ID, PredReported, shared, "hive_user:reed", 1),
		edge(reed.ID, PredAttended, private, "hive_user:reed", 2),
		edge(ada.ID, PredAssigned, shared, "github:ada", 3),
		edge(ada.ID, PredAttended, private, "github:ada", 4),
	}); err != nil {
		t.Fatal(err)
	}

	// The owner sees every edge of anyone.
	edges, next, err := s.Edges(ownerV, ada.ID, "", "", 10)
	if err != nil || next != "" || len(edges) != 2 {
		t.Fatalf("owner Edges(ada) = %d, %q, %v", len(edges), next, err)
	}
	// Read-write sees Ada, but not the artefact only the owner may open.
	edges, _, err = s.Edges(rwV, ada.ID, "", "", 10)
	if err != nil || len(edges) != 1 || edges[0].Artefact != shared {
		t.Fatalf("read-write Edges(ada) = %+v, %v", edges, err)
	}
	// Read-only: own edges only, and still not the private artefact.
	edges, _, err = s.Edges(readV, MeID, "", "", 10)
	if err != nil || len(edges) != 1 || edges[0].Artefact != shared {
		t.Fatalf("read-only Edges(me) = %+v, %v", edges, err)
	}
	if _, _, err := s.Edges(readV, ada.ID, "", "", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read-only Edges(ada) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Edges(Viewer{}, MeID, "", "", 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nameless Edges = %v", err)
	}

	// Walking back from an artefact never reaches a person the viewer may
	// not see: the read-only user finds only themselves on a shared issue.
	people, err := s.ArtefactPeople(ownerV, shared, 10)
	if err != nil || len(people) != 2 {
		t.Fatalf("owner ArtefactPeople = %+v, %v", people, err)
	}
	people, err = s.ArtefactPeople(readV, shared, 10)
	if err != nil || len(people) != 1 || people[0].Person.ID != reed.ID || !people[0].Person.Me || people[0].Predicate != PredReported {
		t.Fatalf("read-only ArtefactPeople = %+v, %v", people, err)
	}
	if people, err := s.ArtefactPeople(ownerV, shared, 1); err != nil || len(people) != 1 {
		t.Fatalf("limit = %+v, %v", people, err)
	}
	// An artefact the viewer may not see is not there for them.
	if _, err := s.ArtefactPeople(rwV, private, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ArtefactPeople on a private artefact = %v", err)
	}
	if s.ArtefactVisible(rwV, private) || !s.ArtefactVisible(ownerV, private) {
		t.Fatal("ArtefactVisible must follow the artefact's own source")
	}
	for _, bad := range []string{"", "no-kind", "gh:bad\nref"} {
		if s.ArtefactVisible(ownerV, bad) {
			t.Errorf("ArtefactVisible(%q) = true", bad)
		}
	}
	if s.ArtefactVisible(Viewer{Full: true}, shared) {
		t.Fatal("a nameless viewer sees no artefact")
	}

	// No index: nothing to read, and no error.
	bare := NewService(store, nil, nil)
	if e, n, err := bare.Edges(ownerV, ada.ID, "", "", 10); err != nil || e != nil || n != "" {
		t.Fatalf("Edges without an index = %v, %q, %v", e, n, err)
	}
	if p, err := bare.ArtefactPeople(ownerV, shared, 10); err != nil || p != nil {
		t.Fatalf("ArtefactPeople without an index = %v, %v", p, err)
	}
}

func edgeCounts(t *testing.T, s *Service, id string) map[string]int {
	t.Helper()
	c, err := s.Index().Counts(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestMergeUnmergeSplitMoveEdgesExactly: an edge follows the identity that
// produced it, through a merge, an undo and a split.
func TestMergeUnmergeSplitMoveEdgesExactly(t *testing.T) {
	s := newService(t)
	a, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	b, _ := s.Add(ownerV, "A. Lovelace", []Identity{{Kind: KindEmail, Value: "ada@example.com"}, {Kind: KindGitHub, Value: "ada-alt"}})
	if _, err := s.Index().Put([]Edge{
		edge(a.ID, PredAuthored, PRRef("o/r", 1), "github:ada", 1),
		edge(b.ID, PredSent, InboxRef("email", "m1"), "email:ada@example.com", 2),
		edge(b.ID, PredReported, IssueRef("o/r", 2), "github:ada-alt", 3),
	}); err != nil {
		t.Fatal(err)
	}

	merged, err := s.Merge(rwV, a.ID, b.ID)
	if err != nil || len(merged.Identities) != 3 || len(merged.MergedFrom) != 1 {
		t.Fatalf("Merge = %+v, %v", merged, err)
	}
	if c := edgeCounts(t, s, a.ID); c[PredAuthored] != 1 || c[PredSent] != 1 || c[PredReported] != 1 {
		t.Fatalf("after merge the target's edges = %v", c)
	}
	if c := edgeCounts(t, s, b.ID); len(c) != 0 {
		t.Fatalf("the merged-in person still has edges: %v", c)
	}

	// Undo: the merged identities' edges go back, the target keeps its own.
	back, err := s.Unmerge(rwV, a.ID, b.ID)
	if err != nil || len(back.Identities) != 1 || len(back.MergedFrom) != 0 {
		t.Fatalf("Unmerge = %+v, %v", back, err)
	}
	if c := edgeCounts(t, s, a.ID); len(c) != 1 || c[PredAuthored] != 1 {
		t.Fatalf("after unmerge the target's edges = %v", c)
	}
	if c := edgeCounts(t, s, b.ID); c[PredSent] != 1 || c[PredReported] != 1 {
		t.Fatalf("after unmerge the restored person's edges = %v", c)
	}

	// Split one identity off: exactly its edges move.
	fresh, err := s.Split(ownerV, b.ID, []string{"email:ada@example.com"}, "Mail only")
	if err != nil || fresh.Name != "Mail only" || !fresh.HasIdentity("email:ada@example.com") {
		t.Fatalf("Split = %+v, %v", fresh, err)
	}
	if c := edgeCounts(t, s, fresh.ID); len(c) != 1 || c[PredSent] != 1 {
		t.Fatalf("the new person's edges = %v", c)
	}
	if c := edgeCounts(t, s, b.ID); len(c) != 1 || c[PredReported] != 1 {
		t.Fatalf("the original's edges = %v", c)
	}

	// Refusals.
	if _, err := s.Merge(rwV, a.ID, a.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self merge = %v", err)
	}
	if _, err := s.Merge(rwV, a.ID, "p_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge a missing person = %v", err)
	}
	if _, err := s.Unmerge(rwV, a.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmerge twice = %v", err)
	}
	if _, err := s.Split(ownerV, b.ID, []string{"github:ada-alt"}, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("splitting off the last identity = %v", err)
	}
}

func TestMergeSplitExportRoles(t *testing.T) {
	s := newService(t)
	a, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	b, _ := s.Add(ownerV, "Bo", []Identity{{Kind: KindGitHub, Value: "bo"}, {Kind: KindEmail, Value: "bo@example.com"}})
	me, _ := s.Get(readV, MeID)
	// A read-only viewer can do none of it, on anyone, themselves included.
	for _, id := range []string{a.ID, b.ID, MeID, me.ID} {
		if _, err := s.Merge(readV, id, a.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("read-only Merge(%s) = %v", id, err)
		}
		if _, err := s.Unmerge(readV, id, a.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("read-only Unmerge(%s) = %v", id, err)
		}
		if _, err := s.Split(readV, id, []string{"github:bo"}, "x"); !errors.Is(err, ErrForbidden) {
			t.Errorf("read-only Split(%s) = %v", id, err)
		}
		if _, err := s.Export(readV, id); !errors.Is(err, ErrForbidden) {
			t.Errorf("read-only Export(%s) = %v", id, err)
		}
	}
	if _, err := s.Export(rwV, a.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-write Export = %v, want ErrForbidden (the owner exports)", err)
	}
	// Two dashboard users are never merged.
	alice, _ := s.Get(ownerV, MeID)
	if _, err := s.Merge(ownerV, alice.ID, me.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("merging two dashboard users = %v", err)
	}
	if _, err := s.Split(rwV, b.ID, []string{"email:bo@example.com"}, "Bo mail"); err != nil {
		t.Fatalf("read-write Split = %v", err)
	}
}

func TestExport(t *testing.T) {
	s := newService(t)
	a, _ := s.Add(ownerV, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	for _, e := range []Edit{{Op: OpNotes, Notes: "met at the summit"}, {Op: OpSatisfactionSet, Level: "at_risk"}} {
		if _, err := s.Edit(ownerV, a.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	var edges []Edge
	for i := 0; i < MaxEdgePage+10; i++ {
		edges = append(edges, edge(a.ID, PredAuthored, PRRef("o/r", i+1), "github:ada", i))
	}
	if _, err := s.Index().Put(edges); err != nil {
		t.Fatal(err)
	}
	out, err := s.Export(ownerV, a.ID)
	if err != nil || out.Person.ID != a.ID || out.Person.Notes != "met at the summit" || out.Person.Satisfaction.Level != LevelAtRisk {
		t.Fatalf("Export = %+v, %v", out.Person, err)
	}
	if len(out.Edges) != MaxEdgePage+10 || out.Truncated || out.Edges[0].Artefact == "" {
		t.Fatalf("export edges = %d truncated=%v", len(out.Edges), out.Truncated)
	}
	if _, err := s.Export(ownerV, "p_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("export of a missing person = %v", err)
	}
	bare := NewService(s.Store(), nil, nil)
	if out, err := bare.Export(ownerV, a.ID); err != nil || out.Edges == nil || len(out.Edges) != 0 {
		t.Fatalf("export without an index = %+v, %v", out, err)
	}
}
