package customers

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// goneSet is a resolver for every kind: an artefact exists until it is put
// in the set.
type goneSet map[string]bool

func (g goneSet) Exists(ref string) bool      { return !g[ref] }
func (g goneSet) Visible(string, Viewer) bool { return true }
func (g goneSet) resolvers(kinds ...string) Resolvers {
	r := Resolvers{}
	for _, k := range kinds {
		r[k] = g
	}
	return r
}

func newSweepService(t *testing.T, gone goneSet) *Service {
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
	return NewService(store, index, gone.resolvers(RefMeeting, RefEvent, RefInbox))
}

var sweepAt = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func sight(kind IdentityKind, value, pred, ref, source string) Sighting {
	return Sighting{Source: source, Observation: Observation{Kind: kind, Value: value, Predicate: pred, Artefact: ref, At: sweepAt}}
}

func sweepInput() SweepInput {
	return SweepInput{
		Users: []HiveUser{
			{Name: "github:Olivia", Maintainer: true, Rule: "owner"},
			{Name: "reed", Rule: "read"},
			{Name: "  "},
		},
		Links: []ProvenLink{
			{User: "olivia", Identity: Identity{Kind: KindGitHub, Value: "olivia", Source: "access_list"}},
			{User: "reed", Identity: Identity{Kind: KindEmail, Value: "Reed@Example.com", Source: "calendar_proof"}},
			{User: "nobody", Identity: Identity{Kind: KindEmail, Value: "ghost@example.com"}},
		},
		Sightings: []Sighting{
			sight(KindGitHub, "olivia", PredAuthored, PRRef("o/r", 10), "github_poll"),
			sight(KindGitHub, "olivia", PredReported, IssueRef("o/r", 1), "github_poll"),
			sight(KindEmail, "reed@example.com", PredInvited, EventRef("e1"), "calendar"),
			sight(KindEmail, "reed@example.com", PredSent, InboxRef("email", "m1"), "inbox_mail"),
			sight(KindHiveUser, "reed", PredAdded, InboxRef("upload", "u1"), "inbox_uploads"),
			sight(KindHiveUser, "not-a-user", PredAdded, InboxRef("upload", "u2"), "inbox_uploads"),
			sight(KindGitHub, "stranger", PredReported, IssueRef("o/r", 2), "github_poll"),
			sight(KindEmail, "guest@example.com", PredInvited, EventRef("e1"), "calendar"),
			sight(KindName, "Some Display Name", PredAttended, MeetingRef("m1"), "meetings"),
		},
	}
}

func TestSweepBuildsCustomersEdgesAndSuggestions(t *testing.T) {
	s := newSweepService(t, goneSet{})
	res, err := s.Sweep(sweepInput())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.Users != 2 || res.Linked != 2 || res.Edges != 5 || res.Suggestions != 2 {
		t.Fatalf("result = %+v, want 2 users, 2 links, 5 edges, 2 suggestions", res)
	}

	// Dashboard users are customers; "github:Olivia" and "olivia" are one.
	olivia, ok := s.Store().Lookup(KindHiveUser, "Olivia")
	if !ok || olivia.Origin != OriginHiveUser || !olivia.HasIdentity("github:olivia") {
		t.Fatalf("olivia = %+v, %v", olivia, ok)
	}
	if me, _ := s.Get(Viewer{User: "github:olivia", Full: true, Owner: true}, MeID); me.ID != olivia.ID {
		t.Fatal(`"github:olivia" and "olivia" must be the same "me"`)
	}
	reed, _ := s.Store().Lookup(KindHiveUser, "reed")
	if !reed.HasIdentity("email:reed@example.com") {
		t.Fatalf("a proven calendar email was not linked: %+v", reed.Identities)
	}
	if len(s.Store().People()) != 2 {
		t.Fatalf("people = %d: only dashboard users are customers", len(s.Store().People()))
	}

	// Customers get edges, each naming the identity that produced it.
	oc, _ := s.Index().Counts(olivia.ID)
	rc, _ := s.Index().Counts(reed.ID)
	if !reflect.DeepEqual(oc, map[string]int{PredAuthored: 1, PredReported: 1}) {
		t.Fatalf("olivia's edges = %v", oc)
	}
	if !reflect.DeepEqual(rc, map[string]int{PredInvited: 1, PredSent: 1, PredAdded: 1}) {
		t.Fatalf("reed's edges = %v", rc)
	}
	edges, _, _ := s.Index().ByPerson(reed.ID, PredSent, "", 0)
	if len(edges) != 1 || edges[0].Via != "email:reed@example.com" || edges[0].Source != "inbox_mail" || !edges[0].At.Equal(sweepAt) {
		t.Fatalf("edge = %+v", edges)
	}

	// Everyone else is a suggestion: a key, counters and references. No
	// name, no profile, no edge.
	sugg := map[string]Suggestion{}
	for _, sg := range s.Store().Suggestions() {
		sugg[string(sg.Kind)+":"+sg.Value] = sg
	}
	if len(sugg) != 2 || sugg["github:stranger"].Counts[PredReported] != 1 || sugg["email:guest@example.com"].Evidence[0] != EventRef("e1") {
		t.Fatalf("suggestions = %+v", sugg)
	}
	if s.Index().Len() != 5 {
		t.Fatalf("index holds %d edges; a suggested person must have none", s.Index().Len())
	}

	// Automatic tags from role and history.
	olivia, _ = s.Store().Get(olivia.ID)
	reed, _ = s.Store().Get(reed.ID)
	if !olivia.HasTag(TagMaintainer) || !olivia.HasTag(TagContributor) || olivia.HasTag(TagEndUser) {
		t.Fatalf("olivia's tags = %v", olivia.ActiveTags())
	}
	if got := reed.ActiveTags(); len(got) != 1 || got[0] != TagStakeholder {
		t.Fatalf("reed's tags = %v, want stakeholder only", got)
	}
	for _, tg := range olivia.Tags {
		if tg.Source != SourceAuto || tg.Rule == "" {
			t.Fatalf("automatic tag without provenance: %+v", tg)
		}
	}
}

func TestSweepIsIdempotentAndDoesNotRewriteAnUnchangedStore(t *testing.T) {
	s := newSweepService(t, goneSet{})
	if _, err := s.Sweep(sweepInput()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Store().Path())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(s.Store().Path())
	res, err := s.Sweep(sweepInput())
	if err != nil {
		t.Fatal(err)
	}
	if res.Linked != 0 || res.TagsChanged != 0 || res.Suggestions != 2 {
		t.Fatalf("second sweep = %+v, want nothing new", res)
	}
	after, _ := os.ReadFile(s.Store().Path())
	if string(before) != string(after) {
		t.Fatal("an identical sweep changed people.json")
	}
	if again, _ := os.Stat(s.Store().Path()); !again.ModTime().Equal(info.ModTime()) {
		t.Fatal("an identical sweep rewrote people.json")
	}
	if s.Index().Len() != 5 {
		t.Fatalf("an identical sweep duplicated edges: %d", s.Index().Len())
	}
}

func TestSweepNeverOverridesAPersonsDecisions(t *testing.T) {
	s := newSweepService(t, goneSet{})
	if _, err := s.Sweep(sweepInput()); err != nil {
		t.Fatal(err)
	}
	olivia, _ := s.Store().Lookup(KindHiveUser, "olivia")
	reed, _ := s.Store().Lookup(KindHiveUser, "reed")
	// A person turns a tag off, and unlinks an identity.
	if _, err := s.Store().SetTag(olivia.ID, TagContributor, false, "olivia"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Edit(Viewer{User: "olivia", Full: true, Owner: true}, reed.ID, Edit{Op: OpIdentityRemove, Key: "email:reed@example.com"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Index().Counts(reed.ID); n[PredSent] != 0 {
		t.Fatalf("removing an identity kept its edges: %v", n)
	}
	// Someone forgets the stranger after accepting them.
	var sid string
	for _, sg := range s.Store().Suggestions() {
		if sg.Value == "stranger" {
			sid = sg.ID
		}
	}
	stranger, err := s.Accept(Viewer{User: "olivia", Full: true, Owner: true}, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(Viewer{User: "olivia", Full: true, Owner: true}, stranger.ID); err != nil {
		t.Fatal(err)
	}

	res, err := s.Sweep(sweepInput())
	if err != nil {
		t.Fatal(err)
	}
	olivia, _ = s.Store().Get(olivia.ID)
	if olivia.HasTag(TagContributor) {
		t.Fatal("the sweep turned a manually cleared tag back on")
	}
	reed, _ = s.Store().Get(reed.ID)
	if reed.HasIdentity("email:reed@example.com") || res.Linked != 0 {
		t.Fatal("the sweep re-linked an identity a person removed")
	}
	// The forgotten stranger is not rediscovered: no profile, no
	// suggestion, no edge.
	if _, ok := s.Store().Lookup(KindGitHub, "stranger"); ok {
		t.Fatal("a forgotten person was rebuilt")
	}
	for _, sg := range s.Store().Suggestions() {
		if sg.Value == "stranger" {
			t.Fatal("a forgotten person came back as a suggestion")
		}
	}
	if back, _ := s.Index().ByArtefact(IssueRef("o/r", 2)); len(back) != 0 {
		t.Fatalf("a forgotten person has edges again: %+v", back)
	}
}

// TestSweepPrunesWhatRetentionDeleted: when an artefact is gone, the edges
// to it, the suggestions that rested on it and the evidence that cited it
// all go with it.
func TestSweepPrunesWhatRetentionDeleted(t *testing.T) {
	gone := goneSet{}
	s := newSweepService(t, gone)
	in := SweepInput{
		Users: []HiveUser{{Name: "olivia", Maintainer: true, Rule: "owner"}},
		Links: []ProvenLink{{User: "olivia", Identity: Identity{Kind: KindEmail, Value: "olivia@example.com"}}},
		Sightings: []Sighting{
			sight(KindEmail, "olivia@example.com", PredAdded, MeetingRef("m1"), "meetings"),
			sight(KindEmail, "olivia@example.com", PredOrganised, EventRef("e1"), "calendar"),
			sight(KindEmail, "guest@example.com", PredAdded, MeetingRef("m1"), "meetings"),
		},
	}
	if _, err := s.Sweep(in); err != nil {
		t.Fatal(err)
	}
	olivia, _ := s.Store().Lookup(KindHiveUser, "olivia")
	if _, err := s.Store().SuggestGoals(olivia.ID, []GoalSuggestion{{Text: "Wants meeting notes", Evidence: []string{MeetingRef("m1")}}}); err != nil {
		t.Fatal(err)
	}
	if s.Index().Len() != 2 || len(s.Store().Suggestions()) != 1 {
		t.Fatalf("setup: %d edges, %d suggestions", s.Index().Len(), len(s.Store().Suggestions()))
	}

	// The meeting's transcript is purged by its own retention. The next
	// sweep no longer sees it either.
	gone[MeetingRef("m1")] = true
	in.Sightings = in.Sightings[1:2]
	res, err := s.Sweep(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.PrunedEdges != 1 || res.ExpiredSuggestions != 1 || res.DroppedEvidence != 1 {
		t.Fatalf("result = %+v, want 1 edge pruned, 1 suggestion expired, 1 reference dropped", res)
	}
	if back, _ := s.Index().ByArtefact(MeetingRef("m1")); len(back) != 0 {
		t.Fatalf("edges to a purged meeting survive: %+v", back)
	}
	if len(s.Store().Suggestions()) != 0 {
		t.Fatal("a suggestion outlived its only evidence")
	}
	if got, _ := s.Store().Get(olivia.ID); len(got.Goals) != 0 {
		t.Fatalf("a goal suggestion outlived its evidence: %+v", got.Goals)
	}
	if raw, _ := os.ReadFile(s.Store().Path()); len(raw) == 0 || strings.Contains(string(raw), MeetingRef("m1")) {
		t.Fatal("people.json still points at the purged meeting")
	}
	if s.Index().Len() != 1 {
		t.Fatalf("the event edge must stay: %d edges", s.Index().Len())
	}
}

func TestSweepWithoutAnIndexStillKeepsPeople(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "customers"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(store, nil, nil)
	res, err := s.Sweep(sweepInput())
	if err != nil || res.Users != 2 || res.Edges != 0 || res.Suggestions != 2 {
		t.Fatalf("Sweep without an index = %+v, %v", res, err)
	}
	olivia, _ := store.Lookup(KindHiveUser, "olivia")
	if !olivia.HasTag(TagMaintainer) {
		t.Fatal("the role tag does not need the index")
	}
}

func TestAutoTagRules(t *testing.T) {
	cases := []struct {
		name       string
		counts     map[string]int
		maintainer string
		want       map[string]string
	}{
		{"nothing", nil, "", map[string]string{}},
		{"role", nil, "role:owner", map[string]string{TagMaintainer: "role:owner"}},
		{"authored a PR", map[string]int{PredAuthored: 2, PredReported: 1}, "", map[string]string{TagContributor: RuleAuthoredPR}},
		{"only reported", map[string]int{PredReported: 1, PredMentionedIn: 3}, "", map[string]string{TagEndUser: RuleReportedIssue}},
		{"meetings and mail only", map[string]int{PredInvited: 2, PredSent: 1}, "", map[string]string{TagStakeholder: RuleMeetingOrMail}},
		{"GitHub activity is not a stakeholder", map[string]int{PredInvited: 2, PredAssigned: 1}, "", map[string]string{}},
		{"a maintainer is not a stakeholder", map[string]int{PredInvited: 2}, "association:github_poll", map[string]string{TagMaintainer: "association:github_poll"}},
	}
	for _, tc := range cases {
		if got := autoTags(tc.counts, tc.maintainer); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: autoTags = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSweepMaintainerFromAssociation(t *testing.T) {
	s := newSweepService(t, goneSet{})
	ada, err := s.Add(Viewer{User: "olivia", Full: true}, "Ada", []Identity{{Kind: KindGitHub, Value: "ada"}})
	if err != nil {
		t.Fatal(err)
	}
	member := sight(KindGitHub, "ada", PredAuthored, PRRef("o/r", 5), "github_poll")
	member.Maintainer = true
	if _, err := s.Sweep(SweepInput{Sightings: []Sighting{member}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Store().Get(ada.ID)
	if !got.HasTag(TagMaintainer) || !got.HasTag(TagContributor) {
		t.Fatalf("tags = %v", got.ActiveTags())
	}
	// The association goes away (the PR left the enumeration): the
	// automatic maintainer tag follows; contributor rests on the edge.
	if _, err := s.Sweep(SweepInput{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Store().Get(ada.ID)
	if got.HasTag(TagMaintainer) || !got.HasTag(TagContributor) {
		t.Fatalf("tags after the association went away = %v", got.ActiveTags())
	}
}

func TestMentions(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"", nil},
		{"thanks @Alice and @bob-2, also @alice again.", []string{"Alice", "bob-2"}},
		{"mail me at carol@example.com", nil},
		{"cc @org/team and @dave", []string{"dave"}},
		{"run `npm i @scope/pkg` then ping @erin", []string{"erin"}},
		{"(@frank) @-bad- @", []string{"frank", "bad"}},
		{"see @a.b for details, thanks @gina.", []string{"gina"}},
	}
	for _, tc := range cases {
		if got := Mentions(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Mentions(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	long := ""
	for i := 0; i < mentionMax+10; i++ {
		long += " @user" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if got := Mentions(long); len(got) != mentionMax {
		t.Fatalf("Mentions bound = %d, want %d", len(got), mentionMax)
	}
	if got := Mentions("@" + strings.Repeat("a", maxLoginLen+1)); got != nil {
		t.Fatalf("an over-long login was accepted: %v", got)
	}
}

func TestIsBotLogin(t *testing.T) {
	for login, want := range map[string]bool{
		"dependabot[bot]": true, "Copilot[bot]": true, "release-bot": true, "ghost": true, "": true,
		"alice": false, "robotics-fan": false,
	} {
		if got := IsBotLogin(login); got != want {
			t.Errorf("IsBotLogin(%q) = %v, want %v", login, got, want)
		}
	}
}
