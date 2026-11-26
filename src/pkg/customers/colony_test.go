package customers

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cstore "github.com/hivecommons/colony/store"
)

// The fixture under testdata/colony is synthetic: invented names, example
// domains and one free-mail address.
const (
	fixAda = "p_000000000001"
	fixBea = "p_000000000002"
	fixCy  = "p_000000000003"
	// fixAdaEmail is on a domain that is not free-mail, so colony holds it
	// local_only.
	fixAdaEmail = "ada@example.com"
)

// fixtureDir copies the fixture JSON files into a fresh customers dir.
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "customers")
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileName, EnrichmentFileName} {
		data, err := os.ReadFile(filepath.Join("testdata", "colony", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, storeFileMode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type colonyBoot struct {
	store  *Store
	colony *Colony
	enrich *EnrichmentStore
}

// bootColony opens the stores in dir as a hive boot does for mode.
func bootColony(t *testing.T, dir string, mode StoreMode, clk *clock) colonyBoot {
	t.Helper()
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, err := AttachColony(s, mode, nil)
	if err != nil {
		t.Fatalf("AttachColony(%s): %v", mode, err)
	}
	e, err := OpenEnrichment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetColony(c); err != nil {
		t.Fatalf("SetColony: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return colonyBoot{store: s, colony: c, enrich: e}
}

func colonyPerson(t *testing.T, c *Colony, id string) cstore.Person {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.db.Get(id)
	if err != nil {
		t.Fatalf("colony Get(%s): %v", id, err)
	}
	return p
}

func factAt(t *testing.T, p cstore.Person, field string, rank cstore.Rank) cstore.Fact {
	t.Helper()
	f, ok := p.FactAt(field, rank)
	if !ok {
		t.Fatalf("%s holds no %s fact at rank %s: %+v", p.ID, field, rank, p.Facts)
	}
	return f
}

// The first shadow boot imports the fixture: same person ids, identities
// colony can hold, provenance mapped to ranks, enrichment as suggestions,
// dismissed affiliations, and a parity check that agrees.
func TestColonyImportFromFixture(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())

	for _, name := range []string{FileName, EnrichmentFileName} {
		want, _ := os.ReadFile(filepath.Join("testdata", "colony", name))
		got, err := os.ReadFile(filepath.Join(dir, name+PreColonySuffix))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s backup = %v, want the file as it was before the import", name, err)
		}
	}
	b.colony.mu.Lock()
	rec, ran, err := b.colony.db.Migration(colonyMigrationName)
	b.colony.mu.Unlock()
	if err != nil || !ran || rec.Report.People != 3 || rec.Fingerprint == "" {
		t.Fatalf("migration record = %+v, ran %v, err %v", rec, ran, err)
	}

	ada := colonyPerson(t, b.colony, fixAda)
	if len(ada.Identities) != 2 {
		t.Fatalf("ada identities = %+v, want github and email (discord stays in hive)", ada.Identities)
	}
	for _, i := range ada.Identities {
		switch i.Kind {
		case cstore.KindEmail:
			if i.Locality != cstore.LocalityLocalOnly {
				t.Fatalf("a private-domain email must be local_only: %+v", i)
			}
		case cstore.KindGitHub:
			if i.Locality != cstore.LocalityShareable || !i.Verified {
				t.Fatalf("github identity = %+v", i)
			}
		}
	}
	if org := factAt(t, ada, FieldOrganization, cstore.RankUser); org.Value != "Initech" || org.By != "owner" {
		t.Fatalf("manual organization = %+v, want rank user", org)
	}
	rn := factAt(t, ada, FieldRealName, cstore.RankEnrichment)
	if rn.Source != ProfileSourceGitHub || rn.Locality != cstore.LocalityShareable || len(rn.DerivedFrom) != 1 {
		t.Fatalf("forge real name = %+v, want enrichment from the public GitHub profile", rn)
	}
	if c := factAt(t, ada, FieldCountry, cstore.RankEnrichment); c.Value != "PT" || c.Source != CountryRuleLocation {
		t.Fatalf("derived country = %+v", c)
	}
	aff := factAt(t, ada, cstore.FieldAffiliation, cstore.RankEnrichment)
	if aff.Value != "Hooli" || !aff.Suggested || aff.Source != colonySourceGitdm || aff.Confidence != "high" {
		t.Fatalf("gitdm affiliation = %+v, want the current one as a suggestion", aff)
	}
	if act := factAt(t, ada, cstore.FieldActivityPrefix+"kubernetes", cstore.RankEnrichment); act.Value != "42" || act.Source != colonySourceDevst {
		t.Fatalf("devstats activity = %+v", act)
	}
	if len(ada.Dismissed) != 1 || ada.Dismissed[0].Value != "umbrella" {
		t.Fatalf("dismissed = %+v", ada.Dismissed)
	}

	bea := colonyPerson(t, b.colony, fixBea)
	if org := factAt(t, bea, FieldOrganization, cstore.RankHubShared); org.Source != "hub:email_domain" || org.Value != "Globex" {
		t.Fatalf("hub organization = %+v, want hub_shared with its label", org)
	}
	if c := factAt(t, bea, FieldCountry, cstore.RankHubShared); c.Source != "hub:person" {
		t.Fatalf("hub country = %+v", c)
	}
	for _, i := range bea.Identities {
		if i.Kind == cstore.KindGitHub && !i.Verified {
			t.Fatal("a confirmed identity must be verified")
		}
	}

	cy := colonyPerson(t, b.colony, fixCy)
	for _, i := range cy.Identities {
		if i.Kind == cstore.KindEmail && i.Verified {
			t.Fatalf("a claimed identity must not be verified: %+v", i)
		}
		if i.Kind == cstore.KindOIDC && i.Locality != cstore.LocalityLocalOnly {
			t.Fatalf("a dashboard account id must be local_only: %+v", i)
		}
	}

	par, err := b.store.ColonyParity()
	if err != nil {
		t.Fatal(err)
	}
	if par.JSONPeople != 3 || par.ColonyPeople != 3 || par.Mismatches != 0 || par.JSONIdentities != par.ColonyIdent {
		t.Fatalf("parity = %+v", par)
	}
	// Hive-only data is untouched.
	p, err := b.store.Get(fixAda)
	if err != nil || !p.HasTag(TagContributor) || p.Identity(KindDiscord) == "" {
		t.Fatalf("hive record = %+v, %v", p, err)
	}
}

// Booting again in shadow mode is a no-op: the import is not run twice, the
// backups keep the first copy, and nothing is duplicated.
func TestColonyImportIsIdempotent(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	b.colony.mu.Lock()
	first, _, _ := b.colony.db.Migration(colonyMigrationName)
	st1, _ := b.colony.db.Stats()
	b.colony.mu.Unlock()
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(filepath.Join(dir, FileName+PreColonySuffix))

	again := bootColony(t, dir, StoreShadow, clk)
	again.colony.mu.Lock()
	second, _, _ := again.colony.db.Migration(colonyMigrationName)
	st2, _ := again.colony.db.Stats()
	again.colony.mu.Unlock()
	if !second.At.Equal(first.At) || second.Fingerprint != first.Fingerprint {
		t.Fatalf("the import ran again: %+v then %+v", first, second)
	}
	if st1.People != st2.People || st1.Identities != st2.Identities || st1.Facts[cstore.RankEnrichment] != st2.Facts[cstore.RankEnrichment] {
		t.Fatalf("stats changed across an idle boot: %+v then %+v", st1, st2)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, FileName+PreColonySuffix)); !bytes.Equal(now, backup) {
		t.Fatal("the backup was replaced")
	}
	if par, _ := again.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity after a second boot = %+v", par)
	}
}

// Writes in shadow mode reach both stores; precedence holds in colony: an
// enrichment value never displaces a value a person set, and hub_shared
// outranks enrichment.
func TestColonyShadowWritesAndPrecedence(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if _, err := b.store.SetProfileField(fixAda, FieldOrganization, "Vandelay", "owner"); err != nil {
		t.Fatal(err)
	}
	ada := colonyPerson(t, b.colony, fixAda)
	if eff, _ := ada.Effective(FieldOrganization); eff.Value != "Vandelay" || eff.Rank != cstore.RankUser {
		t.Fatalf("effective organization = %+v", eff)
	}
	// A provider value for the same field cannot overwrite it.
	b.colony.mu.Lock()
	res, err := b.colony.db.SetFact(fixAda, cstore.Fact{Field: FieldOrganization, Value: "Acme", Rank: cstore.RankEnrichment,
		Source: colonySourceGitdm, Suggested: true})
	b.colony.mu.Unlock()
	if err != nil || res.Effective {
		t.Fatalf("an enrichment value became effective over a user value: %+v, %v", res, err)
	}
	bea := colonyPerson(t, b.colony, fixBea)
	b.colony.mu.Lock()
	res, err = b.colony.db.SetFact(fixBea, cstore.Fact{Field: FieldOrganization, Value: "Acme", Rank: cstore.RankEnrichment, Source: colonySourceGitdm})
	b.colony.mu.Unlock()
	if err != nil || res.Effective {
		t.Fatalf("an enrichment value became effective over a hub_shared value: %+v, %v (%+v)", res, err, bea.Facts)
	}
	b.colony.mu.Lock()
	_, err = b.colony.db.DeleteFact(fixBea, FieldOrganization, cstore.RankEnrichment)
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Resetting the field hands it back: the user fact goes, nothing is
	// left at a rank the hive no longer holds.
	if _, err := b.store.ResetProfileField(fixAda, FieldOrganization); err != nil {
		t.Fatal(err)
	}
	ada = colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.FactAt(FieldOrganization, cstore.RankUser); ok {
		t.Fatal("the reset left the user fact in colony")
	}
	// A new identity reaches colony; removing it removes it there.
	p, err := b.store.Get(fixBea)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.Add("Dee New", []Identity{{Kind: KindGitHub, Value: "dee-new"}}, "owner"); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	dee, err := b.colony.db.FindByIdentity(cstore.KindGitHub, "dee-new")
	b.colony.mu.Unlock()
	if err != nil || dee.ID == "" || dee.ID == p.ID {
		t.Fatalf("a new customer did not reach colony: %+v, %v", dee, err)
	}
	if par, _ := b.store.ColonyParity(); par.Mismatches != 0 || par.JSONPeople != 4 || par.ColonyPeople != 4 {
		t.Fatalf("parity after writes = %+v", par)
	}
}

// Forget erases the person from colony.db (compacted, so the bytes are
// gone from the file), tombstones their identities there, and removes them
// from the .pre-colony backups.
func TestColonyForgetErasesEverywhere(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if _, err := b.store.Forget(fixAda); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	_, err := b.colony.db.Get(fixAda)
	tomb, terr := b.colony.db.IsTombstoned(cstore.IdentityRef{Kind: cstore.KindGitHub, Value: "ada-example"})
	b.colony.mu.Unlock()
	if !errors.Is(err, cstore.ErrNotFound) || terr != nil || !tomb {
		t.Fatalf("colony still holds the person (%v) or did not tombstone them (%v, %v)", err, tomb, terr)
	}
	for _, name := range []string{ColonyFileName, FileName + PreColonySuffix, EnrichmentFileName + PreColonySuffix, FileName} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{fixAdaEmail, "ada-example", fixAda} {
			if bytes.Contains(data, []byte(needle)) {
				t.Fatalf("%s still holds %q after forget", name, needle)
			}
		}
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, FileName+PreColonySuffix)), fixBea) {
		t.Fatal("forget removed someone else from the backup")
	}
	// Re-adding the forgotten person's login does not bring them back in
	// colony.
	b.colony.mu.Lock()
	_, err = b.colony.db.Upsert(cstore.PersonSpec{Identities: []cstore.Identity{{Kind: cstore.KindGitHub, Value: "ada-example"}}})
	b.colony.mu.Unlock()
	if !errors.Is(err, cstore.ErrTombstoned) {
		t.Fatalf("a forgotten identity was re-added: %v", err)
	}
}

// In json mode an earlier run's colony.db is not written, except that a
// forget still reaches it and the backups.
func TestColonyForgetInJSONMode(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreJSON, nil); c != nil || err != nil {
		t.Fatalf("json mode attached a colony store: %v, %v", c, err)
	}
	if _, err := s.Forget(fixBea); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ColonyFileName, FileName + PreColonySuffix} {
		if strings.Contains(mustRead(t, filepath.Join(dir, name)), "bea.sample@gmail.com") {
			t.Fatalf("%s still holds the forgotten person after a json-mode forget", name)
		}
	}
}

// A json-mode boot never creates colony.db.
func TestColonyJSONModeCreatesNothing(t *testing.T) {
	dir := fixtureDir(t)
	s, err := Open(dir, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreJSON, nil); c != nil || err != nil {
		t.Fatalf("json mode: %v, %v", c, err)
	}
	if _, err := s.SetName(fixAda, "Ada E."); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ColonyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("json mode created colony.db: %v", err)
	}
}

// Rollback: colony mode keeps writing the JSON, so switching to json loses
// nothing; a change made in json mode is caught up when colony mode comes
// back, because the JSON is newer than colony.db.
func TestColonyRollbackToJSONAndBack(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColony, clk)
	if _, err := b.store.SetProfileField(fixAda, FieldOrganization, "Vandelay", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Get(fixAda)
	if err != nil || p.Profile.Organization.Value != "Vandelay" || p.Profile.Organization.Source != SourceManual {
		t.Fatalf("json mode after colony lost the change: %+v, %v", p.Profile.Organization, err)
	}
	if _, err := s.SetProfileField(fixAda, FieldOrganization, "Kramerica", "owner"); err != nil {
		t.Fatal(err)
	}

	back := bootColony(t, dir, StoreColony, clk)
	p, err = back.store.Get(fixAda)
	if err != nil || p.Profile.Organization.Value != "Kramerica" {
		t.Fatalf("colony mode lost the json-mode change: %+v, %v", p.Profile.Organization, err)
	}
	ada := colonyPerson(t, back.colony, fixAda)
	if eff, _ := ada.Effective(FieldOrganization); eff.Value != "Kramerica" {
		t.Fatalf("colony was not caught up: %+v", eff)
	}
	if par, _ := back.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity after the round trip = %+v", par)
	}
}

// colony mode reads colony.db: a value colony holds that the JSON does not
// wins at boot and is written back to the JSON. shadow mode keeps reading
// the JSON and reports the difference in the parity counts.
func TestColonyModeReadsColony(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	b.colony.mu.Lock()
	_, err := b.colony.db.SetFact(fixBea, cstore.Fact{Field: FieldRealName, Value: "Beatrix Sample", Rank: cstore.RankUser,
		Source: colonySourceManual, By: "owner", At: clk.now()})
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if par, _ := b.store.ColonyParity(); par.Mismatches != 1 {
		t.Fatalf("shadow parity = %+v, want the one difference counted", par)
	}
	if p, _ := b.store.Get(fixBea); p.Profile.RealName.Value != "" {
		t.Fatalf("shadow mode read colony: %+v", p.Profile.RealName)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}

	c := bootColony(t, dir, StoreColony, clk)
	p, err := c.store.Get(fixBea)
	if err != nil || p.Profile.RealName.Value != "Beatrix Sample" || p.Profile.RealName.Source != SourceManual {
		t.Fatalf("colony mode did not read colony: %+v, %v", p.Profile.RealName, err)
	}
	// Labels survive the read: the hub values keep their rules.
	if p.Profile.Organization.Source != SourceHub || p.Profile.Organization.Rule != ProfileSourceHubEmailDomain {
		t.Fatalf("hub organization after the read = %+v", p.Profile.Organization)
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, FileName)), "Beatrix Sample") {
		t.Fatal("the colony read was not written back to people.json")
	}
	if par, _ := c.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity after the colony read = %+v", par)
	}
}

// colony mode reads enrichment from colony.db; json and shadow read the
// JSON.
func TestColonyModeEnrichmentRead(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreColony, newClock())
	pe, ok := b.enrich.Get(fixAda)
	if cur, found := currentEnrichedAffiliation(pe.Affiliations, b.store.now()); !ok || !found || cur.Org != "Hooli" {
		t.Fatalf("enrichment read = %+v", pe)
	}
	// A gitdm answer written now reaches colony and is read back.
	if err := b.enrich.SetSource(fixAda, EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Massive Dynamic", From: "2025-01-01",
		Confidence: EnrichConfidenceHigh, MatchedBy: EnrichMatchedByLogin}}, nil, b.store.now()); err != nil {
		t.Fatal(err)
	}
	if f := factAt(t, colonyPerson(t, b.colony, fixAda), cstore.FieldAffiliation, cstore.RankEnrichment); f.Value != "Massive Dynamic" {
		t.Fatalf("colony affiliation = %+v", f)
	}
	// Removed from colony, it is no longer read.
	b.colony.mu.Lock()
	_, err := b.colony.db.DeleteFact(fixAda, cstore.FieldAffiliation, cstore.RankEnrichment)
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	pe, _ = b.enrich.Get(fixAda)
	if _, found := currentEnrichedAffiliation(pe.Affiliations, b.store.now()); found {
		t.Fatalf("colony mode read an affiliation colony does not hold: %+v", pe.Affiliations)
	}
	if len(pe.Activity) != 1 || pe.Activity[0].Contributions != 42 || pe.Activity[0].Project != "Kubernetes" {
		t.Fatalf("activity = %+v", pe.Activity)
	}
}

// local_only data never reaches a hub-bound path: the hub import sends only
// hashes of confirmed GitHub logins, and colony's Shareable export (the
// only export a hub-bound path may use) holds no email and no fact derived
// from one.
func TestColonyLocalOnlyNeverHubBound(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	hub := &fakeHub{byLogin: map[string]HubMatchEntry{}}
	imp := &HubImport{Store: b.store, Matcher: hub, Enabled: func() bool { return true }, Key: func() string { return testHubKey }}
	imp.Run(context.Background(), "")
	allowed := map[string]bool{HubMatchHash(testHubKey, "ada-example"): true, HubMatchHash(testHubKey, "bea-sample"): true}
	for _, batch := range hub.batches {
		for _, h := range batch {
			if !allowed[h] {
				t.Fatalf("the hub request carried %q, which is not a confirmed login hash", h)
			}
			if h == HubMatchHash(testHubKey, fixAdaEmail) {
				t.Fatal("an email reached the hub request")
			}
		}
	}

	b.colony.mu.Lock()
	snap, err := b.colony.db.Export(cstore.Shareable())
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tombstones) != 0 || snap.TombstoneSalt != "" {
		t.Fatal("a shareable export carried tombstones")
	}
	for _, p := range snap.People {
		for _, i := range p.Identities {
			if i.Kind.IsEmail() || i.Locality != cstore.LocalityShareable {
				t.Fatalf("shareable export holds %s identity %+v", p.ID, i)
			}
		}
		for _, f := range p.Facts {
			if f.Locality == cstore.LocalityLocalOnly && f.Value != "" {
				t.Fatalf("shareable export holds a local_only fact %+v", f)
			}
			for _, d := range f.DerivedFrom {
				if d.Kind.IsEmail() {
					t.Fatalf("shareable export holds a fact derived from an email: %+v", f)
				}
			}
			if f.Rank == cstore.RankUser && f.Value != "" {
				t.Fatalf("a value a hive user typed left the hive: %+v", f)
			}
		}
	}
}

// A damaged colony.db fails closed: AttachColony refuses it, the file is
// left as it was, and the people store keeps working on the JSON.
func TestColonyDamagedFileFailsClosed(t *testing.T) {
	dir := fixtureDir(t)
	garbage := []byte("not a bbolt file")
	if err := os.WriteFile(filepath.Join(dir, ColonyFileName), garbage, storeFileMode); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := AttachColony(s, StoreColony, nil)
	if err == nil || c != nil {
		_ = c.Close()
		t.Fatal("a damaged colony.db was accepted")
	}
	if s.Colony() != nil {
		t.Fatal("the store kept a colony mirror after a failed attach")
	}
	if got := mustRead(t, filepath.Join(dir, ColonyFileName)); got != string(garbage) {
		t.Fatal("a failed attach rewrote colony.db")
	}
	if _, err := s.SetName(fixAda, "Ada E."); err != nil {
		t.Fatalf("json fallback: %v", err)
	}
}

// Merging moves identities between people in one change; colony keeps one
// owner per identity, and unmerging brings the merged person back.
func TestColonyMergeAndUnmerge(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if _, err := b.store.Merge(fixAda, fixBea, "owner"); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	owner, err := b.colony.db.FindByIdentity(cstore.KindGitHub, "bea-sample")
	b.colony.mu.Unlock()
	if err != nil || owner.ID != fixAda {
		t.Fatalf("after merge the login belongs to %q (%v), want %s", owner.ID, err, fixAda)
	}
	if par, _ := b.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity after merge = %+v", par)
	}
	if _, _, err := b.store.Unmerge(fixAda, fixBea); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	owner, err = b.colony.db.FindByIdentity(cstore.KindGitHub, "bea-sample")
	b.colony.mu.Unlock()
	if err != nil || owner.ID != fixBea {
		t.Fatalf("after unmerge the login belongs to %q (%v), want %s", owner.ID, err, fixBea)
	}
	if par, _ := b.store.ColonyParity(); par.Mismatches != 0 || par.ColonyPeople != 3 {
		t.Fatalf("parity after unmerge = %+v", par)
	}
}

// The enrichment marker keeps the next boot honest: a failed colony write
// leaves it stale, so the next boot imports again.
func TestColonyFailedWriteForcesCatchUp(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	b.colony.mu.Lock()
	b.colony.dirty["x"] = true
	b.colony.mu.Unlock()
	if _, err := b.store.SetProfileField(fixAda, FieldLocation, "Porto", "owner"); err != nil {
		t.Fatal(err)
	}
	m, _ := b.colony.readMarker()
	h, _, _ := fileHash(filepath.Join(dir, FileName))
	if m.People == h {
		t.Fatal("the marker advanced while a colony write was outstanding")
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreShadow, clk)
	m, _ = again.colony.readMarker()
	if m.People != h {
		t.Fatal("the next boot did not catch up and record the JSON it matched")
	}
	if par, _ := again.store.ColonyParity(); par.Mismatches != 0 {
		t.Fatalf("parity after catch-up = %+v", par)
	}
}

// Forgetting a merge target also erases the person merged into them, from
// colony.db and from the backups.
func TestColonyForgetMergeTargetErasesMerged(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	if _, err := b.store.Merge(fixAda, fixBea, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.Forget(fixAda); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	_, err := b.colony.db.Get(fixBea)
	b.colony.mu.Unlock()
	if !errors.Is(err, cstore.ErrNotFound) {
		t.Fatalf("the merged person's record survived the forget: %v", err)
	}
	for _, name := range []string{ColonyFileName, FileName + PreColonySuffix, FileName} {
		if strings.Contains(mustRead(t, filepath.Join(dir, name)), fixBea) {
			t.Fatalf("%s still names the merged person", name)
		}
	}
}

// A json-mode forget that cannot reach colony.db does not fail: it is
// recorded, and the next shadow boot erases the person.
func TestColonyJSONModeForgetIsRetried(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, ColonyFileName)
	aside := db + ".aside"
	if err := os.Rename(db, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("damaged"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(fixBea); err != nil {
		t.Fatalf("a json-mode forget failed on an unreadable colony.db: %v", err)
	}
	c := &Colony{dir: dir}
	m, ok := c.readMarker()
	if _, pending := m.PendingForget[fixBea]; !ok || !pending {
		t.Fatalf("the forget was not recorded for retry: %+v", m)
	}
	if strings.Contains(mustRead(t, filepath.Join(dir, FileName+PreColonySuffix)), fixBea) {
		t.Fatal("the backup still holds the forgotten person")
	}
	if err := os.Rename(aside, db); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreShadow, clk)
	again.colony.mu.Lock()
	_, err = again.colony.db.Get(fixBea)
	again.colony.mu.Unlock()
	if !errors.Is(err, cstore.ErrNotFound) {
		t.Fatalf("the next boot did not erase the person: %v", err)
	}
	if strings.Contains(mustRead(t, db), "bea.sample@gmail.com") {
		t.Fatal("colony.db still holds the forgotten person's bytes")
	}
	if m, _ := again.colony.readMarker(); len(m.PendingForget) != 0 {
		t.Fatalf("pending forgets left after the retry: %+v", m.PendingForget)
	}
}

// Colony tombstones only what hive tombstones: a dashboard user forgotten
// and signing in again gets a fresh profile in both stores, and colony mode
// keeps it.
func TestColonyForgottenHiveUserCanReturn(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	if _, err := b.store.Forget(fixCy); err != nil {
		t.Fatal(err)
	}
	fresh, err := b.store.EnsureHiveUser("cy")
	if err != nil || fresh.ID == fixCy {
		t.Fatalf("EnsureHiveUser = %+v, %v", fresh, err)
	}
	b.colony.mu.Lock()
	owner, err := b.colony.db.FindByIdentity(cstore.KindOIDC, "cy")
	b.colony.mu.Unlock()
	if err != nil || owner.ID != fresh.ID {
		t.Fatalf("the returning dashboard user is not in colony: %+v, %v", owner, err)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	c := bootColony(t, dir, StoreColony, clk)
	if p, ok := c.store.Lookup(KindHiveUser, "cy"); !ok || p.ID != fresh.ID {
		t.Fatalf("colony mode lost the dashboard user's profile: %+v, %v", p, ok)
	}
}

// A hive upgraded from json that sets colony explicitly boots straight into
// colony mode (#19972), with no shadow boot in between: the first boot imports both
// files, keeps the .pre-colony copies as they stood, reads the same records
// (hive-only data included), and a second boot imports nothing again.
func TestColonyUpgradeFromJSONExplicitColony(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColony, clk)
	for _, name := range []string{FileName, EnrichmentFileName} {
		want, _ := os.ReadFile(filepath.Join("testdata", "colony", name))
		got, err := os.ReadFile(filepath.Join(dir, name+PreColonySuffix))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s backup = %v, want the file as it was before the import", name, err)
		}
	}
	par, err := b.store.ColonyParity()
	if err != nil || par.Mode != StoreColony || par.JSONPeople != 3 || par.ColonyPeople != 3 || par.Mismatches != 0 || par.Failures != 0 {
		t.Fatalf("parity = %+v, %v", par, err)
	}
	p, err := b.store.Get(fixAda)
	if err != nil || !p.HasTag(TagContributor) || p.Identity(KindDiscord) == "" || p.Profile.Organization.Value != "Initech" {
		t.Fatalf("record after the upgrade boot = %+v, %v", p, err)
	}
	b.colony.mu.Lock()
	first, _, _ := b.colony.db.Migration(colonyMigrationName)
	b.colony.mu.Unlock()
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColony, clk)
	again.colony.mu.Lock()
	second, _, _ := again.colony.db.Migration(colonyMigrationName)
	again.colony.mu.Unlock()
	if !second.At.Equal(first.At) || second.Fingerprint != first.Fingerprint {
		t.Fatalf("the import ran again: %+v then %+v", first, second)
	}
	if par, err := again.store.ColonyParity(); err != nil || par.ColonyPeople != 3 || par.Mismatches != 0 {
		t.Fatalf("parity after the second boot = %+v, %v", par, err)
	}
}
