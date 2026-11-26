package customers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cstore "github.com/hivecommons/colony/store"
)

func TestColonyParseStoreModeAndNil(t *testing.T) {
	for raw, want := range map[string]StoreMode{"": StoreJSON, "json": StoreJSON, " Shadow ": StoreShadow,
		"COLONY": StoreColony, "colony-only": StoreJSON} {
		if got := ParseStoreMode(raw); got != want {
			t.Fatalf("ParseStoreMode(%q) = %q, want %q", raw, got, want)
		}
	}
	var c *Colony
	if c.Mode() != StoreJSON || c.Close() != nil {
		t.Fatal("a nil Colony is json mode and closes cleanly")
	}
	c.mirrorPeople(nil, nil, nil, nil, nil)
	c.mirrorEnrichment(nil, nil, nil)
	pe := PersonEnrichment{Affiliations: []EnrichedAffiliation{{Org: "Acme", Source: EnrichSourceGitdm}}}
	if got := c.enrichmentFromColony("p_x", pe); len(got.Affiliations) != 1 {
		t.Fatal("a nil Colony must not change enrichment")
	}
	if c, err := AttachColony(nil, StoreShadow, nil); c != nil || err != nil {
		t.Fatal("AttachColony(nil) must be a no-op")
	}
}

func TestColonyKindMapping(t *testing.T) {
	for _, k := range []IdentityKind{KindGitHub, KindEmail, KindGitEmail, KindSlack, KindHiveUser} {
		ck, ok := colonyKind(k)
		if !ok {
			t.Fatalf("%s has no colony kind", k)
		}
		back, ok := hiveKind(ck)
		if !ok || back != k {
			t.Fatalf("%s round-trips to %s", k, back)
		}
	}
	for _, k := range []IdentityKind{KindDiscord, KindTeams, KindMeet, KindZoom, KindName} {
		if _, ok := colonyKind(k); ok {
			t.Fatalf("%s must stay in hive", k)
		}
	}
	if _, ok := hiveKind(cstore.KindGHE); ok {
		t.Fatal("hive has no ghe kind")
	}
	if colonyClassKey(cstore.KindGitEmail, "A@X.org") != colonyClassKey(cstore.KindEmail, "a@x.org") {
		t.Fatal("both email kinds share one namespace")
	}
	if colonyClassKey(cstore.KindGitHub, "@Ada") != colonyClassKey(cstore.KindGitHub, "ada") {
		t.Fatal("logins compare case-insensitively without @")
	}
}

func TestColonyHubLabelRoundTrip(t *testing.T) {
	for _, rule := range []string{"", ProfileSourceHub, ProfileSourceHubPerson, ProfileSourceHubAdmin,
		ProfileSourceHubEmailDomain, ProfileSourceHubBrowserLanguage, ProfileSourceHubUnrecorded} {
		src := cstore.HubSourcePrefix + colonyHubLabel(rule)
		if got := hubRuleFromSource(src); got != rule {
			t.Fatalf("rule %q -> %q -> %q", rule, src, got)
		}
	}
	if colonyHubLabel("hub_unknown") != colonyHubUnlabeled || hubRuleFromSource("hub:nope") != "" {
		t.Fatal("unknown hub labels fail to unlabeled")
	}
}

func TestColonyProfileFieldFromFact(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		fact cstore.Fact
		want ProfileField
	}{
		{cstore.Fact{Value: "X", Rank: cstore.RankUser, By: "o", At: at}, ProfileField{Value: "X", Source: SourceManual, By: "o", At: at}},
		{cstore.Fact{Value: "Y", Rank: cstore.RankAdmin, At: at}, ProfileField{Value: "Y", Source: SourceManual, At: at}},
		{cstore.Fact{Value: "Z", Rank: cstore.RankHubShared, Source: "hub:person", At: at},
			ProfileField{Value: "Z", Source: SourceHub, Rule: ProfileSourceHubPerson, At: at}},
		{cstore.Fact{Value: "W", Rank: cstore.RankEnrichment, Source: colonySourceAuto, At: at}, ProfileField{Value: "W", Source: SourceAuto, At: at}},
		{cstore.Fact{Value: "G", Rank: cstore.RankEnrichment, Source: colonySourceGitdm, At: at},
			ProfileField{Value: "G", Source: SourceAuto, Rule: AffiliationAutoAcceptedGitdm, At: at}},
		{cstore.Fact{Value: "L", Rank: cstore.RankEnrichment, Source: ProfileSourceGitHub, At: at,
			DerivedFrom: []cstore.IdentityRef{{Kind: cstore.KindGitHub, Value: "ada"}}},
			ProfileField{Value: "L", Source: SourceAuto, Rule: ProfileSourceGitHub, Login: "ada", At: at}},
	}
	for _, c := range cases {
		if got := profileFieldFromFact(c.fact); got != c.want {
			t.Fatalf("profileFieldFromFact(%+v) = %+v, want %+v", c.fact, got, c.want)
		}
	}
}

func TestColonySmallHelpers(t *testing.T) {
	if got := truncateRunes("héllo", 2); got != "h" {
		t.Fatalf("truncateRunes cut inside a rune: %q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatal(got)
	}
	if colonyStatus("") != cstore.StatusActive || colonyStatus(StatusCustomer) != "customer" {
		t.Fatal("colonyStatus")
	}
	if !sameStringSet([]string{"a", "b"}, []string{"b", "a"}) || sameStringSet([]string{"a"}, []string{"b"}) ||
		sameStringSet([]string{"a"}, nil) {
		t.Fatal("sameStringSet")
	}
	if colonyActivityField("Open Telemetry") != "activity:open_telemetry" || colonyActivityField("") != "" ||
		colonyActivityField(strings.Repeat("x", colonyMaxFieldBytes)) != "" {
		t.Fatal("colonyActivityField")
	}
	if !parseEnrichedDate("bad").IsZero() || parseEnrichedDate("2020-01-02").IsZero() || !parseEnrichedDate("").IsZero() {
		t.Fatal("parseEnrichedDate")
	}
	if bytesHash(nil) != "" || bytesHash([]byte("x")) == "" {
		t.Fatal("bytesHash")
	}
	if len(parseEnrichment([]byte("{not json"))) != 0 || len(parseEnrichment(nil)) != 0 {
		t.Fatal("parseEnrichment of a bad file is empty")
	}
}

// An email-only gitdm match is local_only and records the emails it was
// matched by; a dismissed value is not mirrored.
func TestColonyEnrichmentFactsLocality(t *testing.T) {
	ids := []cstore.Identity{
		{Kind: cstore.KindGitHub, Value: "ada", Verified: true, Locality: cstore.LocalityShareable},
		{Kind: cstore.KindEmail, Value: "ada@example.com", Verified: true, Locality: cstore.LocalityLocalOnly},
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	pe := PersonEnrichment{Affiliations: []EnrichedAffiliation{{Org: "Acme", From: "2020-01-01", Source: EnrichSourceGitdm,
		Confidence: EnrichConfidenceMedium, MatchedBy: "email"}}}
	facts := colonyEnrichmentFacts(pe, ids, now)
	if len(facts) != 1 || facts[0].Confidence != "medium" || len(facts[0].DerivedFrom) != 1 || !facts[0].DerivedFrom[0].Kind.IsEmail() {
		t.Fatalf("email match facts = %+v", facts)
	}
	if got := colonyEnrichmentFacts(pe, ids, now, cstore.Dismissal{Field: cstore.FieldAffiliation, Value: "acme"}); len(got) != 0 {
		t.Fatalf("a dismissed value was mirrored: %+v", got)
	}
	if got := colonyEnrichmentFacts(pe, nil, now); len(got) != 1 || got[0].Locality != cstore.LocalityLocalOnly {
		t.Fatalf("a fact with no identity to derive from must be local_only: %+v", got)
	}
}

// Enrichment changes that drop a source or prune a person reach colony.
func TestColonyEnrichmentDropAndPrune(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if n, err := b.enrich.DropSource(EnrichSourceDevstats); err != nil || n != 1 {
		t.Fatalf("DropSource = %d, %v", n, err)
	}
	ada := colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.FactAt(cstore.FieldActivityPrefix+"kubernetes", cstore.RankEnrichment); ok {
		t.Fatal("dropping devstats left its activity in colony")
	}
	if n, err := b.enrich.Prune(func(string) bool { return false }); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	ada = colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.FactAt(cstore.FieldAffiliation, cstore.RankEnrichment); ok {
		t.Fatal("pruning left the affiliation in colony")
	}
	if err := b.enrich.Delete("p_unknown"); err != nil {
		t.Fatal(err)
	}
}

// Dismissing an affiliation in hive removes colony's suggestion of it.
func TestColonyDismissDropsSuggestion(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if _, err := b.store.DismissAffiliation(fixAda, "Hooli"); err != nil {
		t.Fatal(err)
	}
	ada := colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.FactAt(cstore.FieldAffiliation, cstore.RankEnrichment); ok {
		t.Fatal("a dismissed suggestion stayed in colony")
	}
	if par, _ := b.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity = %+v", par)
	}
}

// Colony mode reads devstats counts from colony: a changed count is read,
// and a project colony alone holds is listed.
func TestColonyModeActivityRead(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreColony, newClock())
	b.colony.mu.Lock()
	for field, v := range map[string]string{cstore.FieldActivityPrefix + "kubernetes": "50", cstore.FieldActivityPrefix + "envoy": "3"} {
		if _, err := b.colony.db.SetFact(fixAda, cstore.Fact{Field: field, Value: v, Rank: cstore.RankEnrichment,
			Source: colonySourceDevst, Suggested: true}); err != nil {
			b.colony.mu.Unlock()
			t.Fatal(err)
		}
	}
	b.colony.mu.Unlock()
	pe, _ := b.enrich.Get(fixAda)
	got := map[string]int{}
	for _, a := range pe.Activity {
		got[a.Project] = a.Contributions
	}
	if got["Kubernetes"] != 50 || got["envoy"] != 3 {
		t.Fatalf("activity = %+v", pe.Activity)
	}
	// A dirty person is read from the JSON.
	b.colony.mu.Lock()
	b.colony.dirty[fixAda] = true
	b.colony.mu.Unlock()
	if pe, _ := b.enrich.Get(fixAda); len(pe.Activity) != 1 || pe.Activity[0].Contributions != 42 {
		t.Fatalf("dirty read = %+v", pe.Activity)
	}
}

// The backups drop the pins and hints that name a forgotten person, and an
// unreadable enrichment backup is removed rather than kept.
func TestColonyForgetInBackupsRefs(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if err := os.WriteFile(filepath.Join(dir, EnrichmentFileName+PreColonySuffix), []byte("{bad"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if err := forgetInBackups(dir, fixAda, []string{strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, filepath.Join(dir, FileName+PreColonySuffix))
	if strings.Contains(data, `"kind": "distinct"`) || !strings.Contains(data, strings.Repeat("a", 64)) {
		t.Fatalf("backup after forget = %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, EnrichmentFileName+PreColonySuffix)); !os.IsNotExist(err) {
		t.Fatal("an unreadable enrichment backup was kept")
	}
	if ids := backupPersonIDs(dir); len(ids) != 2 {
		t.Fatalf("backup ids = %v", ids)
	}
	_ = b
}

// The person's own forge company replaces an auto-accepted gitdm value;
// AutoAcceptCandidate never offers over it.
func TestAutoAcceptAndForgeCompany(t *testing.T) {
	s := openStore(t)
	p := mustAdd(t, s, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
	now := s.now()
	affs := []EnrichedAffiliation{
		{Org: "Hooli", From: "2020-01-01", Source: EnrichSourceGitdm, Confidence: EnrichConfidenceHigh, MatchedBy: EnrichMatchedByLogin},
		{Org: "Mail Corp", Source: EnrichSourceGitdm, Confidence: EnrichConfidenceMedium, MatchedBy: "email"},
	}
	if c, ok := AutoAcceptCandidate(p, affs, now); !ok || c.Org != "Hooli" {
		t.Fatalf("candidate = %+v, %v", c, ok)
	}
	changed, err := s.AutoAcceptAffiliation(p.ID, affs)
	if err != nil || !changed {
		t.Fatalf("AutoAcceptAffiliation = %v, %v", changed, err)
	}
	if changed, _ := s.AutoAcceptAffiliation(p.ID, affs); changed {
		t.Fatal("a second auto-accept of the same value changed something")
	}
	got, _, err := s.ApplyProfileFill(p.ID, map[string]ProfileFillValue{
		FieldOrganization: {Value: "Initech", Source: ProfileSourceGitHub, Login: "ada"},
	}, ProfileFetch{At: now, Outcome: FetchUnchanged, Sources: []string{ProfileSourceGitHub}})
	if err != nil || got.Profile.Organization.Value != "Initech" || got.Profile.Organization.Rule != ProfileSourceGitHub {
		t.Fatalf("forge company did not replace the auto-accepted value: %+v, %v", got.Profile.Organization, err)
	}
	if _, ok := AutoAcceptCandidate(got, affs, now); ok {
		t.Fatal("auto-accept offered over the person's own forge company")
	}
	if _, ok := AutoAcceptCandidate(Person{}, nil, now); ok {
		t.Fatal("nothing to accept")
	}
	// Dismissing an auto-accepted value clears it.
	q := mustAdd(t, s, "Bea", Identity{Kind: KindGitHub, Value: "bea"})
	if _, err := s.AutoAcceptAffiliation(q.ID, affs); err != nil {
		t.Fatal(err)
	}
	q, err = s.DismissAffiliation(q.ID, "Hooli")
	if err != nil || q.Profile.Organization.Value != "" {
		t.Fatalf("dismiss = %+v, %v", q.Profile.Organization, err)
	}
	if changed, _ := s.AutoAcceptAffiliation(q.ID, affs); changed {
		t.Fatal("a dismissed value was accepted again")
	}
}
