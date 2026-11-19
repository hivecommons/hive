package customers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLookup is a profile source with canned answers per key.
type fakeLookup struct {
	mu        sync.Mutex
	source    string
	kind      IdentityKind
	answers   map[string]ForgeProfile
	err       error
	standDown string
	calls     []string
}

func (f *fakeLookup) Source() string     { return f.source }
func (f *fakeLookup) Kind() IdentityKind { return f.kind }
func (f *fakeLookup) StandDown() string  { return f.standDown }
func (f *fakeLookup) Lookup(_ context.Context, key string) (ForgeProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	if f.err != nil {
		return ForgeProfile{Requests: 1}, f.err
	}
	p, ok := f.answers[key]
	if !ok {
		return ForgeProfile{Requests: 1}, ErrProfileNotFound
	}
	if p.Requests == 0 {
		p.Requests = 1
	}
	return p, nil
}

type fillFixture struct {
	store *Store
	fill  *ProfileFill
	gh    *fakeLookup
	lin   *fakeLookup
	now   *time.Time
	on    *bool
	limit *int
}

func newFillFixture(t *testing.T) *fillFixture {
	t.Helper()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	st, err := Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	x := &fillFixture{store: st, now: &now}
	on, limit := true, 60
	x.on, x.limit = &on, &limit
	x.gh = &fakeLookup{source: ProfileSourceGitHub, kind: KindGitHub, answers: map[string]ForgeProfile{
		"ada": {Name: "Ada Lovelace", Company: "@acme", Location: "London, UK"},
	}}
	x.lin = &fakeLookup{source: ProfileSourceLinear, kind: KindEmail, answers: map[string]ForgeProfile{
		"grace@example.com": {Name: "Grace Hopper"},
		"ada@example.com":   {Name: "Ada (from Linear)"},
	}}
	x.fill = NewProfileFill(st)
	x.fill.SetClock(func() time.Time { return *x.now })
	x.fill.Enabled = func() bool { return *x.on }
	x.fill.HourlyLimit = func() int { return *x.limit }
	x.fill.Sources = func() []ProfileLookup { return []ProfileLookup{x.gh, x.lin} }
	return x
}

func (x *fillFixture) get(t *testing.T, id string) Person {
	t.Helper()
	p, err := x.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProfileFillFillsEmptyFieldsWithProvenance(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	res := x.fill.Run(context.Background(), "")
	if res.Skipped != "" || res.People != 1 || res.Filled != 1 || len(x.gh.calls) != 1 {
		t.Fatalf("result = %+v calls=%v", res, x.gh.calls)
	}
	p := x.get(t, ada.ID)
	if p.Profile.RealName.Value != "Ada Lovelace" || p.Profile.Organization.Value != "acme" || p.Profile.Location.Value != "London, UK" {
		t.Fatalf("profile = %+v", p.Profile)
	}
	for _, name := range profileFillFields {
		f := p.Profile.Field(name)
		if f.Source != SourceAuto || f.Rule != ProfileSourceGitHub || f.Login != "ada" || !f.At.Equal(*x.now) {
			t.Fatalf("%s provenance = %+v", name, f)
		}
	}
	if c := p.Profile.Country; c.Value != "GB" || c.Rule != CountryRuleLocation {
		t.Fatalf("country = %+v", c)
	}
	if fe := p.Profile.Fetched; fe == nil || fe.Outcome != FetchFilled || len(fe.Sources) != 1 || fe.Sources[0] != ProfileSourceGitHub {
		t.Fatalf("fetch record = %+v", fe)
	}
}

func TestProfileFillNeverOverwritesManualHubOrOtherValues(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	if _, err := x.store.SetProfileField(ada.ID, FieldOrganization, "Globex", "olivia"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.SetProfileField(ada.ID, FieldLocation, "", "olivia"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.SetProfileField(ada.ID, FieldCountry, "JP", "olivia"); err != nil {
		t.Fatal(err)
	}
	x.fill.Run(context.Background(), ada.ID)
	p := x.get(t, ada.ID)
	if p.Profile.Organization.Value != "Globex" || p.Profile.Organization.Source != SourceManual {
		t.Fatalf("manual organization overwritten: %+v", p.Profile.Organization)
	}
	if p.Profile.Location.Value != "" || p.Profile.Location.Source != SourceManual {
		t.Fatalf("a field a person cleared was refilled: %+v", p.Profile.Location)
	}
	if p.Profile.Country.Value != "JP" {
		t.Fatalf("manual country changed: %+v", p.Profile.Country)
	}
	if p.Profile.RealName.Value != "Ada Lovelace" {
		t.Fatalf("empty field not filled: %+v", p.Profile.RealName)
	}
	// A hub value is never overwritten either.
	grace := mustAdd(t, x.store, "grace", Identity{Kind: KindGitHub, Value: "ada2"}, Identity{Kind: KindEmail, Value: "grace@example.com"})
	err := x.store.change(func() error {
		p := x.store.people[grace.ID]
		p.Profile.RealName = ProfileField{Value: "Rear Admiral Hopper", Source: SourceHub, Rule: ProfileSourceHub}
		x.store.people[grace.ID] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	x.fill.Run(context.Background(), grace.ID)
	if got := x.get(t, grace.ID).Profile.RealName; got.Value != "Rear Admiral Hopper" || got.Source != SourceHub {
		t.Fatalf("hub value overwritten: %+v", got)
	}
}

func TestProfileFillRefreshUpdatesAndClearsOnlyItsOwnValues(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	x.fill.Run(context.Background(), "")
	// The person moves and removes their company from the public profile.
	x.gh.answers["ada"] = ForgeProfile{Name: "Ada Lovelace", Location: "Paris, France"}
	*x.now = x.now.Add(ProfileFillRefreshAge)
	x.fill.Run(context.Background(), "")
	p := x.get(t, ada.ID)
	if p.Profile.Location.Value != "Paris, France" || p.Profile.Country.Value != "FR" {
		t.Fatalf("refresh did not follow the move: %+v", p.Profile)
	}
	if p.Profile.Organization.Value != "" {
		t.Fatalf("a value the person removed was kept: %+v", p.Profile.Organization)
	}
	// A failed lookup changes nothing it did not read.
	x.gh.err = errors.New("unreachable")
	*x.now = x.now.Add(ProfileFillRefreshAge)
	res := x.fill.Run(context.Background(), "")
	p = x.get(t, ada.ID)
	if res.Failed != 1 || p.Profile.Location.Value != "Paris, France" || p.Profile.Fetched.Outcome != FetchFailed {
		t.Fatalf("failed lookup: res=%+v profile=%+v", res, p.Profile)
	}
}

func TestProfileFillFirstSourceWinsAndLinearUsesEmail(t *testing.T) {
	x := newFillFixture(t)
	both := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"}, Identity{Kind: KindEmail, Value: "ada@example.com"})
	onlyMail := mustAdd(t, x.store, "grace", Identity{Kind: KindEmail, Value: "grace@example.com"})
	x.fill.Run(context.Background(), "")
	if got := x.get(t, both.ID).Profile.RealName; got.Value != "Ada Lovelace" || got.Rule != ProfileSourceGitHub {
		t.Fatalf("forge must win over Linear: %+v", got)
	}
	got := x.get(t, onlyMail.ID).Profile.RealName
	if got.Value != "Grace Hopper" || got.Rule != ProfileSourceLinear || got.Login != "" {
		t.Fatalf("Linear name = %+v", got)
	}
	for _, k := range x.gh.calls {
		if k == "grace@example.com" || k == "ada@example.com" {
			t.Fatalf("an email was sent to the forge: %v", x.gh.calls)
		}
	}
}

func TestProfileFillBackfillsHiveUsersAndSkipsSuggestions(t *testing.T) {
	x := newFillFixture(t)
	hu, err := x.store.EnsureHiveUser("github:ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.store.change(func() error {
		x.store.sugg["github:stranger"] = Suggestion{ID: "sg_1", Kind: KindGitHub, Value: "stranger"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	x.gh.answers["stranger"] = ForgeProfile{Name: "Never Asked"}
	res := x.fill.Run(context.Background(), "")
	if res.Due != 1 || res.Filled != 1 {
		t.Fatalf("backfill = %+v", res)
	}
	if got := x.get(t, hu.ID).Profile.RealName; got.Value != "Ada Lovelace" || got.Login != "ada" {
		t.Fatalf("hive user not backfilled: %+v", got)
	}
	for _, k := range x.gh.calls {
		if k == "stranger" {
			t.Fatal("a suggestion was looked up")
		}
	}
	// Done once: the next pass has nobody due.
	x.gh.calls = nil
	if res := x.fill.Run(context.Background(), ""); res.Due != 0 || len(x.gh.calls) != 0 {
		t.Fatalf("second pass = %+v calls=%v", res, x.gh.calls)
	}
	// Refresh all makes everyone due again.
	if n, err := x.fill.RefreshAll(); err != nil || n != 1 {
		t.Fatalf("RefreshAll = %d, %v", n, err)
	}
	if res := x.fill.Run(context.Background(), ""); res.Due != 1 {
		t.Fatalf("after refresh all = %+v", res)
	}
}

func TestProfileFillGatesBudgetAndBackoff(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	*x.on = false
	if r := x.fill.Run(context.Background(), ""); r.Skipped != FillSkipDisabled || len(x.gh.calls) != 0 {
		t.Fatalf("disabled: %+v", r)
	}
	*x.on = true
	// A source whose quota is low is not asked, and the person stays due.
	x.gh.standDown = "core_low"
	if r := x.fill.Run(context.Background(), ""); r.StoodDown != 1 || len(x.gh.calls) != 0 {
		t.Fatalf("stand down: %+v", r)
	}
	if x.get(t, ada.ID).Profile.Fetched != nil {
		t.Fatal("a stood-down lookup was recorded")
	}
	x.gh.standDown = ""
	// The hourly budget is shared and all-or-nothing per lookup.
	*x.limit = 1
	if r := x.fill.Run(context.Background(), ""); r.Skipped != FillSkipBudget || !r.Truncated || len(x.gh.calls) != 0 {
		t.Fatalf("budget: %+v", r)
	}
	*x.limit = 60
	// A rate-limit refusal stops the pass and backs off, doubling.
	x.gh.err = ErrProfileRateLimited
	if r := x.fill.Run(context.Background(), ""); r.Skipped != FillSkipBackoff {
		t.Fatalf("rate limited: %+v", r)
	}
	first := x.fill.BackoffUntil().Sub(*x.now)
	if first != ProfileFillBackoffBase {
		t.Fatalf("first backoff = %v", first)
	}
	calls := len(x.gh.calls)
	if r := x.fill.Run(context.Background(), ""); r.Skipped != FillSkipBackoff || len(x.gh.calls) != calls {
		t.Fatalf("during backoff: %+v", r)
	}
	*x.now = x.fill.BackoffUntil()
	x.fill.Run(context.Background(), "")
	if second := x.fill.BackoffUntil().Sub(*x.now); second != 2*ProfileFillBackoffBase {
		t.Fatalf("second backoff = %v", second)
	}
	for i := 0; i < 10; i++ {
		*x.now = x.fill.BackoffUntil()
		x.fill.Run(context.Background(), "")
	}
	if capped := x.fill.BackoffUntil().Sub(*x.now); capped != ProfileFillBackoffMax {
		t.Fatalf("backoff not capped: %v", capped)
	}
	// An answer resets it.
	x.gh.err = nil
	*x.now = x.fill.BackoffUntil()
	if r := x.fill.Run(context.Background(), ""); r.Filled != 1 || !x.fill.BackoffUntil().IsZero() {
		t.Fatalf("after recovery: %+v backoff=%v", r, x.fill.BackoffUntil())
	}
	if x.get(t, ada.ID).Profile.Fetched == nil {
		t.Fatal("a rate-limited person was never recorded after recovery")
	}
}

func TestProfileFillPassCapAndOrder(t *testing.T) {
	x := newFillFixture(t)
	*x.limit = 1000
	for i := 0; i < ProfileFillMaxPerPass+5; i++ {
		mustAdd(t, x.store, "p", Identity{Kind: KindGitHub, Value: "user" + string(rune('a'+i))})
	}
	res := x.fill.Run(context.Background(), "")
	if res.People != ProfileFillMaxPerPass || !res.Truncated || res.NotFound != ProfileFillMaxPerPass {
		t.Fatalf("pass = %+v", res)
	}
	*x.now = x.now.Add(time.Minute)
	res = x.fill.Run(context.Background(), "")
	if res.Due != 5 || res.People != 5 || res.Truncated {
		t.Fatalf("second pass must take the never-fetched rest: %+v", res)
	}
}

func TestProfileFillOnlyAndUnknownPerson(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	x.fill.Run(context.Background(), "")
	// "Refresh from profiles" on one person ignores the refresh age.
	x.gh.answers["ada"] = ForgeProfile{Name: "Countess of Lovelace"}
	if r := x.fill.Run(context.Background(), ada.ID); r.People != 1 {
		t.Fatalf("refresh one = %+v", r)
	}
	if got := x.get(t, ada.ID).Profile.RealName.Value; got != "Countess of Lovelace" {
		t.Fatalf("refresh one did not update: %q", got)
	}
	if r := x.fill.Run(context.Background(), "p_missing"); r.Skipped != FillSkipNotFound {
		t.Fatalf("unknown person = %+v", r)
	}
}

func TestProfileLookupKey(t *testing.T) {
	p := Person{Identities: []Identity{
		{Kind: KindGitHub, Value: "claimed", Claimed: true},
		{Kind: KindGitHub, Value: "dependabot[bot]"},
		{Kind: KindHiveUser, Value: "github:Ada"},
		{Kind: KindEmail, Value: "ada@example.com"},
	}}
	if got := profileLookupKey(p, KindGitHub); got != "ada" {
		t.Fatalf("login key = %q", got)
	}
	if got := profileLookupKey(p, KindEmail); got != "ada@example.com" {
		t.Fatalf("email key = %q", got)
	}
	if got := profileLookupKey(Person{Identities: []Identity{{Kind: KindGitHub, Value: "ada_l.dev"}}}, KindGitHub); got != "ada_l.dev" {
		t.Fatalf("a GitLab-shaped username = %q", got)
	}
	if got := profileLookupKey(Person{Identities: []Identity{{Kind: KindGitHub, Value: "../etc"}}}, KindGitHub); got != "" {
		t.Fatalf("a path-shaped login = %q", got)
	}
	if got := profileLookupKey(Person{Identities: []Identity{{Kind: KindHiveUser, Value: "not a login!"}}}, KindGitHub); got != "" {
		t.Fatalf("non-login hive user = %q", got)
	}
}

// Precedence (#19319 labels hub values): by hand, then hub person-entered or
// consented, then the public forge profile, then hub derived or admin-set.
func TestProfileFillPrecedenceOverLabeledHubValues(t *testing.T) {
	x := newFillFixture(t)
	ada := mustAdd(t, x.store, "ada", Identity{Kind: KindGitHub, Value: "ada"})
	if err := x.store.change(func() error {
		p := x.store.people[ada.ID]
		p.Profile.RealName = ProfileField{Value: "Ada K.", Source: SourceHub, Rule: hubRulePersonEntered}
		p.Profile.Organization = ProfileField{Value: "example.com", Source: SourceHub, Rule: "hub_email_domain"}
		p.Profile.Country = ProfileField{Value: "US", Source: SourceHub, Rule: "hub_browser_language"}
		x.store.people[ada.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	x.fill.Run(context.Background(), "")
	p := x.get(t, ada.ID)
	if p.Profile.RealName.Value != "Ada K." || p.Profile.RealName.Source != SourceHub {
		t.Fatalf("a person-entered hub value was overwritten: %+v", p.Profile.RealName)
	}
	if p.Profile.Organization.Value != "acme" || p.Profile.Organization.Rule != ProfileSourceGitHub {
		t.Fatalf("a derived hub value did not yield to the forge: %+v", p.Profile.Organization)
	}
	if p.Profile.Country.Value != "GB" || p.Profile.Country.Source != SourceAuto {
		t.Fatalf("a derived hub country did not yield to the forge location: %+v", p.Profile.Country)
	}
	// A derived hub value with no forge value to replace it is kept.
	grace := mustAdd(t, x.store, "grace", Identity{Kind: KindGitHub, Value: "grace"})
	x.gh.answers["grace"] = ForgeProfile{Name: "Grace Hopper"}
	if err := x.store.change(func() error {
		p := x.store.people[grace.ID]
		p.Profile.Organization = ProfileField{Value: "Navy", Source: SourceHub, Rule: "hub_admin"}
		x.store.people[grace.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	x.fill.Run(context.Background(), grace.ID)
	if got := x.get(t, grace.ID).Profile.Organization; got.Value != "Navy" || got.Source != SourceHub {
		t.Fatalf("an admin hub value was dropped with nothing to replace it: %+v", got)
	}
	for _, f := range []ProfileField{
		{Source: SourceHub, Rule: ProfileSourceHub}, {Source: SourceHub, Rule: hubRulePersonEntered},
		{Source: SourceHub, Rule: hubRuleUnrecorded}, {Source: SourceHub}, {Source: SourceAuto, Rule: "hub_admin"},
	} {
		if hubYieldsToForge(f) {
			t.Errorf("hubYieldsToForge(%+v) = true", f)
		}
	}
}
