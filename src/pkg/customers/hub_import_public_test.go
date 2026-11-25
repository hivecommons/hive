package customers

import (
	"context"
	"strings"
	"testing"
)

// Labeled hub values (#19306): precedence manual > hub entered by the person
// > public forge profile > hub derived/admin, and manual is never overwritten.
func TestHubImportLabeledPrecedence(t *testing.T) {
	x := newHubFixture(t)
	forge := mustAdd(t, x.store, "forge", Identity{Kind: KindGitHub, Value: "forge-person"})
	empty := mustAdd(t, x.store, "empty", Identity{Kind: KindGitHub, Value: "empty-person"})
	manual := mustAdd(t, x.store, "manual", Identity{Kind: KindGitHub, Value: "manual-person"})
	if _, _, err := x.store.ApplyAutoProfile(forge.ID, map[string]string{
		FieldOrganization: "Forge Org", FieldLocation: "Berlin, Germany",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.SetProfileField(manual.ID, FieldOrganization, "Typed By Hand", "olivia"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.SetProfileField(manual.ID, FieldCountry, "", "olivia"); err != nil {
		t.Fatal(err)
	}
	derived := map[string]string{FieldOrganization: ProfileSourceHubEmailDomain, FieldCountry: ProfileSourceHubAdmin}
	for _, login := range []string{"forge-person", "empty-person", "manual-person"} {
		x.hub.byLogin[login] = HubMatchEntry{Organization: "Derived Org", Country: "GB", Rules: derived}
	}
	x.imp.Run(context.Background(), "")

	// A forge value outranks a hub derived/admin value.
	p := x.get(t, forge.ID)
	if f := p.Profile.Organization; f.Value != "Forge Org" || f.Source != SourceAuto {
		t.Fatalf("forge org must outrank a hub-derived one: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "DE" || f.Source != SourceAuto {
		t.Fatalf("forge-location country must outrank a hub admin one: %+v", f)
	}
	// An empty field takes the derived value, labeled.
	p = x.get(t, empty.ID)
	if f := p.Profile.Organization; f.Value != "Derived Org" || f.Source != SourceHub || f.Rule != ProfileSourceHubEmailDomain {
		t.Fatalf("empty org must take the labeled hub value: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "GB" || f.Rule != ProfileSourceHubAdmin || !HubValueYieldsToForge(f) {
		t.Fatalf("empty country must take the admin hub value and yield to forge: %+v", f)
	}
	// Manual values and manual clears are never overwritten.
	p = x.get(t, manual.ID)
	if f := p.Profile.Organization; f.Value != "Typed By Hand" || f.Source != SourceManual {
		t.Fatalf("manual org overwritten: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "" || f.Source != SourceManual {
		t.Fatalf("manual clear overwritten: %+v", f)
	}

	// A value the person entered on the hub outranks the forge value, but
	// still never a manual one.
	person := map[string]string{FieldOrganization: ProfileSourceHubPerson, FieldCountry: ProfileSourceHubPerson}
	for _, login := range []string{"forge-person", "manual-person"} {
		x.hub.byLogin[login] = HubMatchEntry{Organization: "Self Org", Country: "FR", Rules: person}
	}
	x.imp.Run(context.Background(), "")
	p = x.get(t, forge.ID)
	if f := p.Profile.Organization; f.Value != "Self Org" || f.Rule != ProfileSourceHubPerson || HubValueYieldsToForge(f) {
		t.Fatalf("hub person-entered org must outrank forge: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "FR" || f.Source != SourceHub {
		t.Fatalf("hub person-entered country must outrank forge location: %+v", f)
	}
	p = x.get(t, manual.ID)
	if p.Profile.Organization.Value != "Typed By Hand" || p.Profile.Country.Source != SourceManual {
		t.Fatalf("manual overwritten by a hub person value: %+v", p.Profile)
	}

	// The hub is authoritative for its own values: a later admin value
	// replaces the earlier person-entered hub value it no longer holds.
	x.hub.byLogin["forge-person"] = HubMatchEntry{Organization: "Admin Org", Rules: map[string]string{FieldOrganization: ProfileSourceHubAdmin}}
	x.imp.Run(context.Background(), "")
	if f := x.get(t, forge.ID).Profile.Organization; f.Value != "Admin Org" || f.Rule != ProfileSourceHubAdmin {
		t.Fatalf("hub value must follow the hub: %+v", f)
	}
}

// Decoding keeps only known labels on public fields and drops a real name
// that is not labeled as consented.
func TestDecodeHubMatchReplyLabels(t *testing.T) {
	body := `{"matches":[
		{"match_hash":"a","real_name":"Ada","organization":"Org","country":"GB",
		 "sources":{"real_name":"admin","organization":"email_domain","country":"mystery","email":"person"}},
		{"match_hash":"b","real_name":"Bea","sources":{"real_name":"consent"}},
		{"match_hash":"c","real_name":"Cy","organization":"Org"}]}`
	reply, err := DecodeHubMatchReply(strings.NewReader(body))
	if err != nil || len(reply.Matches) != 3 {
		t.Fatalf("decode: %v %+v", err, reply)
	}
	a, b, c := reply.Matches[0], reply.Matches[1], reply.Matches[2]
	if a.RealName != "" {
		t.Fatalf("a real name labeled non-consent must be dropped: %+v", a)
	}
	if a.rule(FieldOrganization) != ProfileSourceHubEmailDomain || len(a.Rules) != 1 {
		t.Fatalf("rules = %+v, want only organization labeled", a.Rules)
	}
	if b.RealName != "Bea" || b.rule(FieldRealName) != ProfileSourceHub {
		t.Fatalf("consented real name = %+v", b)
	}
	if c.RealName != "Cy" || c.rule(FieldOrganization) != ProfileSourceHub || c.Rules != nil {
		t.Fatalf("an unlabeled answer must read as consented, as before: %+v", c)
	}
	for rule, want := range map[string]string{
		ProfileSourceHubEmailDomain: "derived from an organization email domain",
		ProfileSourceHubAdmin:       "set by a hub admin",
		ProfileSourceHub:            "shared with consent",
	} {
		if got := HubLabel(rule); !strings.HasPrefix(got, "from hub: ") || !strings.Contains(got, want) {
			t.Fatalf("HubLabel(%q) = %q", rule, got)
		}
	}
}

// The hub's affiliation labels (sent only to a project hive) decode to their
// own rules, carry their own provenance text, and yield to a forge value:
// none of them is a value the person entered on the hub.
func TestDecodeHubMatchReplyAffiliationLabels(t *testing.T) {
	for label, want := range map[string]struct{ rule, text string }{
		"profile_company":   {ProfileSourceHubProfileCompany, "public profile company"},
		"org_membership":    {ProfileSourceHubOrgMembership, "public organization membership"},
		"identity_provider": {ProfileSourceHubIdentityProvider, "sign-in identity provider"},
		"ghe_account":       {ProfileSourceHubGHEAccount, "GitHub Enterprise account"},
	} {
		body := `{"matches":[{"match_hash":"a","organization":"Org","sources":{"organization":"` + label + `"}}]}`
		reply, err := DecodeHubMatchReply(strings.NewReader(body))
		if err != nil || len(reply.Matches) != 1 {
			t.Fatalf("%s: decode %v %+v", label, err, reply)
		}
		rule := reply.Matches[0].rule(FieldOrganization)
		if rule != want.rule {
			t.Fatalf("%s: rule %q, want %q", label, rule, want.rule)
		}
		if got := HubLabel(rule); got != "from hub: "+want.text {
			t.Fatalf("%s: HubLabel = %q", label, got)
		}
		if HubRuleEnteredByPerson(rule) || !HubValueYieldsToForge(ProfileField{Value: "Org", Source: SourceHub, Rule: rule}) {
			t.Fatalf("%s: an affiliation value must yield to a forge value", label)
		}
	}
}

// A forge profile fill that runs after the hub import replaces a hub derived
// or admin-set value, but never one the person entered or a manual one (#19088).
func TestApplyAutoProfileHubYield(t *testing.T) {
	x := newHubFixture(t)
	a := mustAdd(t, x.store, "a", Identity{Kind: KindGitHub, Value: "yield-a"})
	b := mustAdd(t, x.store, "b", Identity{Kind: KindGitHub, Value: "yield-b"})
	x.hub.byLogin["yield-a"] = HubMatchEntry{Organization: "Derived Org", Country: "GB",
		Rules: map[string]string{FieldOrganization: ProfileSourceHubEmailDomain, FieldCountry: ProfileSourceHubAdmin}}
	x.hub.byLogin["yield-b"] = HubMatchEntry{Organization: "Person Org", Country: "GB",
		Rules: map[string]string{FieldOrganization: ProfileSourceHubPerson, FieldCountry: ProfileSourceHubPerson}}
	x.imp.Run(context.Background(), "")
	vals := map[string]string{FieldOrganization: "Forge Org", FieldLocation: "Berlin, Germany"}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := x.store.ApplyAutoProfile(id, vals); err != nil {
			t.Fatal(err)
		}
	}
	p := x.get(t, a.ID)
	if f := p.Profile.Organization; f.Value != "Forge Org" || f.Source != SourceAuto {
		t.Fatalf("forge org must replace a hub-derived one: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "DE" || f.Source != SourceAuto {
		t.Fatalf("forge country must replace a hub admin one: %+v", f)
	}
	p = x.get(t, b.ID)
	if f := p.Profile.Organization; f.Value != "Person Org" || f.Source != SourceHub {
		t.Fatalf("person-entered hub org must stand: %+v", f)
	}
	if f := p.Profile.Country; f.Value != "GB" || f.Source != SourceHub {
		t.Fatalf("person-entered hub country must stand: %+v", f)
	}
}
