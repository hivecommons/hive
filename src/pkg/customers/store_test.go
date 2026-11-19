package customers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clock is a test clock that moves one second per reading, so every stored
// time is distinct and ordered.
type clock struct{ t time.Time }

func (c *clock) now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "customers"), newClock().now)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustAdd(t *testing.T, s *Store, name string, ids ...Identity) Person {
	t.Helper()
	p, err := s.Add(name, ids, "owner")
	if err != nil {
		t.Fatalf("Add(%q): %v", name, err)
	}
	return p
}

func TestOpenCreatesPrivateStoreAndReloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := s.Add("Ada", []Identity{{Kind: KindGitHub, Value: "@Ada"}}, "owner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(p.ID, "p_") || len(p.ID) != len("p_")+12 {
		t.Fatalf("id %q is not p_ plus 12 hex", p.ID)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("people.json mode = %v, want 0600", info.Mode().Perm())
	}
	dinfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dinfo.Mode().Perm())
	}

	again, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := again.Lookup(KindGitHub, "ADA")
	if !ok || got.ID != p.ID {
		t.Fatalf("Lookup after reload = %+v, %v; want %s", got, ok, p.ID)
	}
	if got.Status != StatusCustomer || got.Origin != OriginManual {
		t.Fatalf("status/origin = %s/%s", got.Status, got.Origin)
	}
	if got.Satisfaction.Level != LevelUnknown {
		t.Fatalf("satisfaction starts %q, want unknown", got.Satisfaction.Level)
	}
}

func TestOpenRefusesDamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, nil); err == nil {
		t.Fatal("a damaged people.json must be an error, never silently replaced")
	}
}

func TestReturnedPeopleAreCopies(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada", Identity{Kind: KindEmail, Value: "ada@example.com"})
	p.Identities[0].Value = "changed@example.com"
	p.Name = "Changed"
	got, err := s.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ada" || got.Identities[0].Value != "ada@example.com" {
		t.Fatalf("store was changed through a returned value: %+v", got)
	}
}

func TestEnsureHiveUserIsIdempotentAndTheOnlyWayIn(t *testing.T) {
	s := openStore(t)
	me, err := s.EnsureHiveUser("Alice")
	if err != nil {
		t.Fatalf("EnsureHiveUser: %v", err)
	}
	again, err := s.EnsureHiveUser("alice")
	if err != nil {
		t.Fatalf("EnsureHiveUser again: %v", err)
	}
	if me.ID != again.ID {
		t.Fatalf("two profiles for one user: %s, %s", me.ID, again.ID)
	}
	if me.Origin != OriginHiveUser || me.Identity(KindHiveUser) != "alice" {
		t.Fatalf("me = %+v", me)
	}
	if len(s.People()) != 1 {
		t.Fatalf("people = %d, want 1", len(s.People()))
	}

	// Nobody can claim to be a dashboard user by hand: that identity is
	// what "me" resolves on.
	if _, err := s.Add("Mallory", []Identity{{Kind: KindHiveUser, Value: "alice"}}, "mallory"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add with hive_user = %v, want ErrInvalid", err)
	}
	other := mustAdd(t, s, "Mallory")
	if _, _, err := s.AddIdentity(other.ID, Identity{Kind: KindHiveUser, Value: "alice"}, "mallory"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("AddIdentity hive_user = %v, want ErrInvalid", err)
	}
	if _, _, err := s.RemoveIdentity(me.ID, IdentityKey(KindHiveUser, "alice"), "mallory"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RemoveIdentity hive_user = %v, want ErrInvalid", err)
	}
	if _, err := s.EnsureHiveUser("  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty user = %v, want ErrInvalid", err)
	}
}

func TestAddValidates(t *testing.T) {
	s := openStore(t)
	mustAdd(t, s, "Ada", Identity{Kind: KindEmail, Value: "ada@example.com"})
	cases := []struct {
		name string
		who  string
		ids  []Identity
		want error
	}{
		{"empty name", " ", nil, ErrInvalid},
		{"long name", strings.Repeat("x", MaxNameLen+1), nil, ErrInvalid},
		{"control character", "A\x07da", nil, ErrInvalid},
		{"unknown kind", "Bo", []Identity{{Kind: "fax", Value: "1"}}, ErrInvalid},
		{"empty identity", "Bo", []Identity{{Kind: KindGitHub, Value: " @ "}}, ErrInvalid},
		{"not an email", "Bo", []Identity{{Kind: KindEmail, Value: "bo"}}, ErrInvalid},
		{"taken identity", "Bo", []Identity{{Kind: KindEmail, Value: "ADA@example.com"}}, ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Add(tc.who, tc.ids, "owner"); !errors.Is(err, tc.want) {
				t.Fatalf("Add = %v, want %v", err, tc.want)
			}
		})
	}
	if n := len(s.People()); n != 1 {
		t.Fatalf("a refused Add left %d people, want 1", n)
	}
	many := make([]Identity, MaxIdentities+1)
	if _, err := s.Add("Bo", many, "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many identities = %v, want ErrInvalid", err)
	}
}

func TestSetNameAndNotes(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada")
	got, err := s.SetName(p.ID, "  Ada L.  ")
	if err != nil || got.Name != "Ada L." {
		t.Fatalf("SetName = %+v, %v", got, err)
	}
	if _, err := s.SetName(p.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name = %v", err)
	}
	if _, err := s.SetName("p_missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing person = %v", err)
	}
	got, err = s.SetNotes(p.ID, "line one\nline two")
	if err != nil || got.Notes != "line one\nline two" {
		t.Fatalf("SetNotes = %+v, %v", got, err)
	}
	if _, err := s.SetNotes(p.ID, strings.Repeat("n", MaxNotesLen+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long notes = %v", err)
	}
	if !got.UpdatedAt.After(p.UpdatedAt) {
		t.Fatal("UpdatedAt did not move")
	}
}

func TestForgetLeavesTombstoneThatBlocksRediscovery(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada",
		Identity{Kind: KindGitHub, Value: "ada"},
		Identity{Kind: KindEmail, Value: "ada@example.com"},
		Identity{Kind: KindName, Value: "Ada Lovelace"})
	other := mustAdd(t, s, "Bo")
	if err := s.AddHint(other.ID, p.ID, "test"); err != nil {
		t.Fatal(err)
	}

	gone, err := s.Forget(p.ID)
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if gone.ID != p.ID || len(gone.Identities) != 3 {
		t.Fatalf("Forget returned %+v", gone)
	}
	if _, err := s.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after forget = %v", err)
	}
	if _, ok := s.Lookup(KindGitHub, "ada"); ok {
		t.Fatal("identity still resolves after forget")
	}
	if len(s.Hints("")) != 0 {
		t.Fatal("a hint about the forgotten person survived")
	}
	if !s.Forgotten(KindGitHub, "Ada") || !s.Forgotten(KindEmail, "ada@example.com") {
		t.Fatal("forgotten identities are not tombstoned")
	}
	if s.Forgotten(KindName, "Ada Lovelace") {
		t.Fatal("a name is never a key, so it is never tombstoned")
	}
	if s.Tombstones() != 2 {
		t.Fatalf("tombstones = %d, want 2", s.Tombstones())
	}

	// The tombstone holds no readable identity.
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ada@example.com", "Lovelace", `"ada"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("people.json still holds %q after forget", leak)
		}
	}

	// Discovery does not rebuild them: no suggestion, no owner.
	owners, err := s.Observe([]Observation{
		{Kind: KindGitHub, Value: "ada", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1)},
		{Kind: KindEmail, Value: "Ada@Example.com", Predicate: PredSent, Artefact: InboxRef("email", "m1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 0 || len(s.Suggestions()) != 0 {
		t.Fatalf("a forgotten person was rediscovered: owners=%v suggestions=%v", owners, s.Suggestions())
	}
	// Nor does an automatic link.
	if linked, err := s.AutoLink(other.ID, Identity{Kind: KindGitHub, Value: "ada"}); err != nil || linked {
		t.Fatalf("AutoLink of a forgotten identity = %v, %v", linked, err)
	}
	// A person deliberately adding them back is a new human decision.
	back := mustAdd(t, s, "Ada again", Identity{Kind: KindGitHub, Value: "ada"})
	if s.Forgotten(KindGitHub, "ada") {
		t.Fatal("a manual add must clear the tombstone it overrides")
	}
	if got, ok := s.Lookup(KindGitHub, "ada"); !ok || got.ID != back.ID {
		t.Fatal("manually re-added person does not resolve")
	}
	if _, err := s.Forget("p_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget missing = %v", err)
	}
}

func TestForgetDashboardUserStartsFresh(t *testing.T) {
	s := openStore(t)
	me, err := s.EnsureHiveUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddGoal(me.ID, "ship the thing", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(me.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.EnsureHiveUser("alice")
	if err != nil {
		t.Fatalf("a dashboard user must be able to sign in again: %v", err)
	}
	if fresh.ID == me.ID || len(fresh.Goals) != 0 {
		t.Fatalf("forgotten profile came back: %+v", fresh)
	}
}

func TestFailedSaveLeavesStoreUnchanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	s, err := Open(dir, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	p := mustAdd(t, s, "Ada")
	// Point the store at a path whose directory does not exist, so the
	// next save fails.
	s.path = filepath.Join(dir, "missing", FileName)
	if _, err := s.SetName(p.ID, "Changed"); err == nil {
		t.Fatal("save into a missing directory must fail")
	}
	got, err := s.Get(p.ID)
	if err != nil || got.Name != "Ada" {
		t.Fatalf("a failed save changed memory: %+v, %v", got, err)
	}
	if _, err := s.Add("Bo", nil, "owner"); err == nil {
		t.Fatal("Add must fail too")
	}
	if len(s.People()) != 1 {
		t.Fatalf("a failed Add left %d people", len(s.People()))
	}
}

func TestHelpers(t *testing.T) {
	h := []Change{}
	for i := 0; i < MaxHistory+5; i++ {
		h = appendHistory(h, Change{State: "s"})
	}
	if len(h) != MaxHistory {
		t.Fatalf("history = %d, want %d", len(h), MaxHistory)
	}
	refs := capEvidence([]string{"a:1", " ", "a:1", "b:2", "c:3", "d:4", "e:5", "f:6"})
	if len(refs) != MaxEvidence || refs[0] != "a:1" || refs[1] != "b:2" {
		t.Fatalf("capEvidence = %v", refs)
	}
	var list []string
	for i := 0; i < maxRejected+3; i++ {
		list = remember(list, string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if len(list) != maxRejected {
		t.Fatalf("remember kept %d, want %d", len(list), maxRejected)
	}
	if got := remember([]string{"x"}, "x"); len(got) != 1 {
		t.Fatalf("remember duplicated: %v", got)
	}
	p := Person{Tags: []Tag{{Name: "a", State: TagOn}, {Name: "b", State: TagOff}},
		Goals: []Goal{{ID: "g1", State: StateConfirmed}, {ID: "g2", State: StateSuggested}}}
	if !p.HasTag("a") || p.HasTag("b") || p.HasTag("c") {
		t.Fatal("HasTag")
	}
	if got := p.ActiveTags(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("ActiveTags = %v", got)
	}
	if got := p.ConfirmedGoals(); len(got) != 1 || got[0].ID != "g1" {
		t.Fatalf("ConfirmedGoals = %v", got)
	}
	if ValidLevel("delighted") || !ValidLevel(LevelAtRisk) {
		t.Fatal("ValidLevel")
	}
	if (Suggestion{Counts: map[string]int{"a": 2, "b": 3}}).Total() != 5 {
		t.Fatal("Total")
	}
	if StatusSuggested == StatusCustomer {
		t.Fatal("statuses must differ")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
