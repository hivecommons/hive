package customers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testHubKey = "0123456789abcdef0123456789abcdef"

// fakeHub answers from a table keyed by login, so a test states what the hub
// says about a person without computing hashes by hand.
type fakeHub struct {
	calls   int
	batches [][]string
	byLogin map[string]HubMatchEntry
	err     error
	// truncateAfter, when > 0, answers at most that many entries per call
	// and sets Truncated when more matched.
	truncateAfter int
}

func (f *fakeHub) Match(_ context.Context, hashes []string) (HubMatchReply, error) {
	f.calls++
	f.batches = append(f.batches, append([]string(nil), hashes...))
	if f.err != nil {
		return HubMatchReply{}, f.err
	}
	want := map[string]HubMatchEntry{}
	for login, e := range f.byLogin {
		e.MatchHash = HubMatchHash(testHubKey, login)
		want[e.MatchHash] = e
	}
	var out HubMatchReply
	for _, h := range hashes {
		if e, ok := want[h]; ok {
			if f.truncateAfter > 0 && len(out.Matches) == f.truncateAfter {
				out.Truncated = true
				break
			}
			out.Matches = append(out.Matches, e)
		}
	}
	return out, nil
}

type hubAudit struct{ person, action, fields string }

type hubFixture struct {
	store *Store
	hub   *fakeHub
	imp   *HubImport
	now   *time.Time
	on    *bool
	audit *[]hubAudit
}

func newHubFixture(t *testing.T) *hubFixture {
	t.Helper()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	on := true
	st, err := Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var audit []hubAudit
	hub := &fakeHub{byLogin: map[string]HubMatchEntry{}}
	imp := &HubImport{
		Store:   st,
		Matcher: hub,
		Enabled: func() bool { return on },
		Key:     func() string { return testHubKey },
		Audit: func(person, action string, fields []string) {
			audit = append(audit, hubAudit{person, action, strings.Join(fields, ",")})
		},
	}
	return &hubFixture{store: st, hub: hub, imp: imp, now: &now, on: &on, audit: &audit}
}

func (x *hubFixture) get(t *testing.T, id string) Person {
	t.Helper()
	p, err := x.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHubImportOverwritesAutoButNeverManual(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	if _, _, err := x.store.ApplyAutoProfile(ada.ID, map[string]string{
		FieldRealName: "ada from github", FieldOrganization: "github org", FieldLocation: "Berlin, Germany",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.SetProfileField(ada.ID, FieldOrganization, "Typed By Hand", "olivia"); err != nil {
		t.Fatal(err)
	}
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace", Organization: "Hub Org", Country: "GB"}

	res := x.imp.Run(context.Background(), "")
	if res.Requests != 1 || res.Imported != 1 || res.Skipped != "" {
		t.Fatalf("result = %+v", res)
	}
	p := x.get(t, ada.ID)
	if f := p.Profile.RealName; f.Value != "Ada Lovelace" || f.Source != SourceHub || f.Rule != ProfileSourceHub {
		t.Fatalf("hub must overwrite auto: %+v", f)
	}
	if f := p.Profile.Organization; f.Value != "Typed By Hand" || f.Source != SourceManual {
		t.Fatalf("manual must never be overwritten: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "GB" || f.Source != SourceHub {
		t.Fatalf("hub country must outrank the location-derived one: %+v", f)
	}
	if f := p.Profile.Location; f.Value != "Berlin, Germany" || f.Source != SourceAuto {
		t.Fatalf("location is not a hub field and must be untouched: %+v", f)
	}

	// The automatic source cannot take a hub field back.
	if _, _, err := x.store.ApplyAutoProfile(ada.ID, map[string]string{FieldRealName: "other", FieldLocation: "Paris, France"}); err != nil {
		t.Fatal(err)
	}
	p = x.get(t, ada.ID)
	if p.Profile.RealName.Value != "Ada Lovelace" || p.Profile.Country.Value != "GB" {
		t.Fatalf("auto overwrote hub: %+v", p.Profile)
	}
	// A hub organization is the person's own statement: no affiliation guess.
	q := x.get(t, ada.ID)
	q.Profile.Organization = ProfileField{Value: "Hub Org", Source: SourceHub}
	if SuggestAffiliation(q) != nil {
		t.Fatal("a hub organization must not be second-guessed")
	}
	// Audit: person id and field names only.
	if len(*x.audit) != 1 || (*x.audit)[0] != (hubAudit{ada.ID, HubAuditImport, "real_name,country"}) {
		t.Fatalf("audit = %+v", *x.audit)
	}
	for _, a := range *x.audit {
		if strings.Contains(a.fields, "Ada") || strings.Contains(a.fields, "GB") {
			t.Fatalf("audit carries a value: %+v", a)
		}
	}
}

func TestHubImportEditMakesFieldManual(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace"}
	x.imp.Run(context.Background(), "")
	if _, err := x.store.SetProfileField(ada.ID, FieldRealName, "Countess Ada", "olivia"); err != nil {
		t.Fatal(err)
	}
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada L."}
	x.imp.Run(context.Background(), "")
	if f := x.get(t, ada.ID).Profile.RealName; f.Value != "Countess Ada" || f.Source != SourceManual {
		t.Fatalf("an edited hub field must become manual and stay: %+v", f)
	}
	// A cleared reply does not remove a manual field either.
	x.hub.byLogin["ada"] = HubMatchEntry{Cleared: []string{FieldRealName}}
	x.imp.Run(context.Background(), "")
	if f := x.get(t, ada.ID).Profile.RealName; f.Value != "Countess Ada" {
		t.Fatalf("cleared removed a manual field: %+v", f)
	}
}

func TestHubImportNeverCreatesOrPromotes(t *testing.T) {
	x := newHubFixture(t)
	// A suggested person the hub would know about.
	if _, err := x.store.Observe([]Observation{{Kind: KindGitHub, Value: "stranger", Predicate: PredAuthored, Artefact: IssueRef("o/r", 1)}}); err != nil {
		t.Fatal(err)
	}
	// A claimed (unconfirmed) GitHub identity.
	mallory := mustAdd(t, x.store, "mallory")
	if _, err := x.store.changePerson(mallory.ID, func(p *Person) error {
		p.Identities = append(p.Identities, Identity{Kind: KindGitHub, Value: "claimed", Claimed: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	x.hub.byLogin["stranger"] = HubMatchEntry{RealName: "Stranger"}
	x.hub.byLogin["claimed"] = HubMatchEntry{RealName: "Claimed"}
	x.hub.byLogin["nobody"] = HubMatchEntry{RealName: "Nobody"}

	res := x.imp.Run(context.Background(), "")
	if x.hub.calls != 0 || res.Skipped != HubSkipNoPeople {
		t.Fatalf("a suggestion or claim was asked about: calls=%d res=%+v", x.hub.calls, res)
	}
	if n := len(x.store.People()); n != 1 {
		t.Fatalf("import created a person: %d people", n)
	}
	if len(x.store.Suggestions()) != 1 {
		t.Fatal("import changed the suggestions")
	}

	// An answer for a hash never asked about is ignored and creates no one.
	ada := mustAdd(t, x.store, "ada", adaLogin)
	before := x.get(t, ada.ID)
	res = x.imp.Run(context.Background(), "")
	if res.Imported != 0 || len(x.store.People()) != 2 {
		t.Fatalf("result = %+v people=%d", res, len(x.store.People()))
	}
	for _, b := range x.hub.batches {
		for _, h := range b {
			if h == HubMatchHash(testHubKey, "stranger") || h == HubMatchHash(testHubKey, "claimed") {
				t.Fatal("a suggested or claimed login was hashed")
			}
		}
	}
	after := x.get(t, ada.ID)
	if len(after.Identities) != len(before.Identities) || len(after.Tags) != len(before.Tags) || after.Notes != before.Notes || after.Satisfaction.Level != before.Satisfaction.Level {
		t.Fatal("import touched something other than the three fields")
	}
}

func TestHubImportSwitchOffMakesZeroRequests(t *testing.T) {
	x := newHubFixture(t)
	mustAdd(t, x.store, "ada", adaLogin)
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace"}
	*x.on = false
	if res := x.imp.Run(context.Background(), ""); res.Skipped != HubSkipDisabled || res.Requests != 0 {
		t.Fatalf("off result = %+v", res)
	}
	x.imp.Enabled = nil
	x.imp.Run(context.Background(), "")
	*x.on = true
	x.imp.Enabled = func() bool { return *x.on }
	x.imp.Key = func() string { return "" }
	if res := x.imp.Run(context.Background(), ""); res.Skipped != HubSkipNoKey {
		t.Fatalf("no key result = %+v", res)
	}
	if x.hub.calls != 0 {
		t.Fatalf("switch off or no key made %d requests", x.hub.calls)
	}
}

func TestHubImportClearedRevertsToAutomatic(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	if _, _, err := x.store.ApplyAutoProfile(ada.ID, map[string]string{FieldLocation: "Berlin, Germany"}); err != nil {
		t.Fatal(err)
	}
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace", Organization: "Hub Org", Country: "GB"}
	x.imp.Run(context.Background(), "")
	x.hub.byLogin["ada"] = HubMatchEntry{Organization: "Hub Org", Cleared: []string{FieldRealName, FieldCountry, "notes", "email"}}
	res := x.imp.Run(context.Background(), "")
	if res.Cleared != 1 {
		t.Fatalf("result = %+v", res)
	}
	p := x.get(t, ada.ID)
	if p.Profile.RealName != (ProfileField{}) {
		t.Fatalf("cleared real name not handed back: %+v", p.Profile.RealName)
	}
	if f := p.Profile.Country; f.Value != "DE" || f.Source != SourceAuto || f.Rule != CountryRuleLocation {
		t.Fatalf("cleared country must return to the automatic one: %+v", f)
	}
	if f := p.Profile.Organization; f.Value != "Hub Org" || f.Source != SourceHub {
		t.Fatalf("a field not named in cleared was removed: %+v", f)
	}
	last := (*x.audit)[len(*x.audit)-1]
	if last != (hubAudit{ada.ID, HubAuditClear, "real_name,country"}) {
		t.Fatalf("clear audit = %+v", last)
	}
}

func TestHubImportStalenessRevertsToAutomatic(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace", Organization: "Hub Org"}
	x.imp.Run(context.Background(), "")

	// Refreshed within the max age: kept, its time moved on.
	*x.now = x.now.Add(HubImportMaxAge - time.Hour)
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace"}
	x.imp.Run(context.Background(), "")
	if f := x.get(t, ada.ID).Profile.RealName; f.Source != SourceHub || !f.At.Equal(*x.now) {
		t.Fatalf("a re-sent value was not refreshed: %+v", f)
	}

	// The hub becomes unreachable. Organization was last refreshed at the
	// start and now ages out; real name was refreshed and stays.
	x.hub.err = errors.New("unreachable")
	*x.now = x.now.Add(2 * time.Hour)
	res := x.imp.Run(context.Background(), "")
	if res.Expired != 1 || res.Skipped != HubSkipFailed {
		t.Fatalf("result = %+v", res)
	}
	p := x.get(t, ada.ID)
	if p.Profile.Organization != (ProfileField{}) || p.Profile.RealName.Value != "Ada Lovelace" {
		t.Fatalf("profile = %+v", p.Profile)
	}
	// Turning the switch off does not keep a hub value forever.
	*x.on = false
	*x.now = x.now.Add(HubImportMaxAge + time.Minute)
	x.imp.Run(context.Background(), "")
	if p := x.get(t, ada.ID); p.Profile.RealName != (ProfileField{}) {
		t.Fatalf("a stale hub value outlived the switch: %+v", p.Profile.RealName)
	}
	last := (*x.audit)[len(*x.audit)-1]
	if last != (hubAudit{ada.ID, HubAuditClear, "real_name"}) {
		t.Fatalf("expiry audit = %+v", last)
	}
}

func TestHubImportForgottenPersonNotRebuilt(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	keep := mustAdd(t, x.store, "bob", Identity{Kind: KindGitHub, Value: "bob"})
	if _, err := x.store.Forget(ada.ID); err != nil {
		t.Fatal(err)
	}
	x.hub.byLogin["ada"] = HubMatchEntry{RealName: "Ada Lovelace"}
	x.hub.byLogin["bob"] = HubMatchEntry{RealName: "Bob"}
	x.imp.Run(context.Background(), "")
	if _, ok := x.store.Lookup(KindGitHub, "ada"); ok || len(x.store.People()) != 1 {
		t.Fatal("import rebuilt a forgotten person")
	}
	for _, h := range x.hub.batches[0] {
		if h == HubMatchHash(testHubKey, "ada") {
			t.Fatal("a forgotten login was sent to the hub")
		}
	}
	if !x.store.Forgotten(KindGitHub, "ada") {
		t.Fatal("the tombstone is gone")
	}
	// Applying an answer directly to the forgotten id fails too.
	if _, _, err := x.store.ApplyHubMatch(ada.ID, "ada", HubMatchEntry{RealName: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ApplyHubMatch on a forgotten person = %v", err)
	}
	if x.get(t, keep.ID).Profile.RealName.Value != "Bob" {
		t.Fatal("the remaining customer was not filled")
	}
}

func TestDecodeHubMatchReplyReadsOnlyThreeFieldsSanitised(t *testing.T) {
	long := strings.Repeat("é", MaxProfileFieldLen)
	body := `{"matches":[{"match_hash":"h1","rev":"r","real_name":"Ada\u0007 ‮Love​lace\n","organization":"` + long + `",` +
		`"country":"gb","notes":"secret","email":"a@example.com","slack_id":"U1","affiliation":"x","location":"Mars",` +
		`"cleared":["notes","real_name","email","real_name"]},{"match_hash":"h2","country":"Atlantis"}],"truncated":true,"extra":1}`
	reply, err := DecodeHubMatchReply(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Truncated || len(reply.Matches) != 2 {
		t.Fatalf("reply = %+v", reply)
	}
	e := reply.Matches[0]
	if e.RealName != "Ada Lovelace" {
		t.Fatalf("control/format characters not stripped: %q", e.RealName)
	}
	if len(e.Organization) > MaxProfileFieldLen || !strings.HasPrefix(e.Organization, "éé") || strings.ContainsRune(e.Organization, '�') {
		t.Fatalf("value not capped on a rune boundary: %d bytes", len(e.Organization))
	}
	if e.Country != "GB" || reply.Matches[1].Country != "" {
		t.Fatalf("country = %q / %q", e.Country, reply.Matches[1].Country)
	}
	if strings.Join(e.Cleared, ",") != "real_name" {
		t.Fatalf("cleared = %v", e.Cleared)
	}
	// Values stored through ApplyHubMatch are sanitised even when a caller
	// skips the decoder.
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	if _, _, err := x.store.ApplyHubMatch(ada.ID, "ada", HubMatchEntry{RealName: "A\x1b[31mda", Country: "zz"}); err != nil {
		t.Fatal(err)
	}
	p := x.get(t, ada.ID)
	if p.Profile.RealName.Value != "A[31mda" || p.Profile.Country.Value != "" {
		t.Fatalf("stored = %+v", p.Profile)
	}
}

func TestHubImportBatchesAndFollowsTruncation(t *testing.T) {
	x := newHubFixture(t)
	for i := 0; i < HubImportBatch+5; i++ {
		login := "user" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + strconv.Itoa(i)
		mustAdd(t, x.store, login, Identity{Kind: KindGitHub, Value: login})
		if i%50 == 0 {
			x.hub.byLogin[login] = HubMatchEntry{RealName: "Name " + strconv.Itoa(i)}
		}
	}
	x.hub.truncateAfter = 2
	res := x.imp.Run(context.Background(), "")
	if res.Imported != len(x.hub.byLogin) {
		t.Fatalf("result = %+v, want %d imported", res, len(x.hub.byLogin))
	}
	if res.Requests > HubImportMaxRequests {
		t.Fatalf("pass made %d requests", res.Requests)
	}
	for _, b := range x.hub.batches {
		if len(b) > HubImportBatch {
			t.Fatalf("batch of %d", len(b))
		}
	}
}

func TestHubImportOnlyAsksAboutOnePersonOnAccept(t *testing.T) {
	x := newHubFixture(t)
	ada := mustAdd(t, x.store, "ada", adaLogin)
	mustAdd(t, x.store, "bob", Identity{Kind: KindGitHub, Value: "bob"})
	x.imp.Run(context.Background(), ada.ID)
	if x.hub.calls != 1 || len(x.hub.batches[0]) != 1 || x.hub.batches[0][0] != HubMatchHash(testHubKey, "ada") {
		t.Fatalf("accept-time pull asked about %v", x.hub.batches)
	}
}
