package customers

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	cstore "github.com/hivecommons/colony/store"
)

// Fixtures are synthetic (testdata/colony): invented names, example domains.

// normalizedPeople re-encodes a people.json so two files that hold the same
// data compare equal whatever their formatting.
func normalizedPeople(t *testing.T, data []byte) string {
	t.Helper()
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding people file: %v", err)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func normalizedEnrichment(t *testing.T, data []byte) string {
	t.Helper()
	var f enrichmentFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding enrichment file: %v", err)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (or stat failed: %v); colony-only must not write it", filepath.Base(path), err)
	}
}

// The first colony-only boot writes everything into colony.db, verifies the
// read-back and only then renames the JSON files, which it keeps.
func TestColonyOnlyMigratesVerifiesAndRenames(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if b.colony.Mode() != StoreColonyOnly {
		t.Fatalf("mode = %s, want colony-only", b.colony.Mode())
	}
	mustNotExist(t, filepath.Join(dir, FileName))
	mustNotExist(t, filepath.Join(dir, EnrichmentFileName))
	migrated := readFile(t, filepath.Join(dir, FileName+MigratedSuffix))
	migratedEnrich := readFile(t, filepath.Join(dir, EnrichmentFileName+MigratedSuffix))

	// What the store serves is exactly what the renamed file holds.
	b.colony.mu.Lock()
	cc, err := readColonyOnly(b.colony.db)
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	people, enrich, err := cc.files()
	if err != nil {
		t.Fatal(err)
	}
	if normalizedPeople(t, people) != normalizedPeople(t, migrated) {
		t.Fatal("colony.db does not give back people.json")
	}
	if normalizedEnrichment(t, enrich) != normalizedEnrichment(t, migratedEnrich) {
		t.Fatal("colony.db does not give back enrichment.json")
	}

	// The data sits in colony's typed slots, not only in hive's attachment.
	ada := colonyPerson(t, b.colony, fixAda)
	var gh cstore.Identity
	for _, i := range ada.Identities {
		if i.Kind == cstore.KindGitHub {
			gh = i
		}
	}
	if !gh.Manual || gh.By != "owner" || gh.Score != 1 {
		t.Errorf("identity provenance not in colony: %+v", gh)
	}
	ans, ok := ada.Answer(colonyAnswerSource(EnrichSourceGitdm))
	if !ok || len(ans.Affiliations) != 2 || ans.FetchedAt.IsZero() {
		t.Errorf("gitdm answer = %+v, want both affiliations with the fetch time", ans)
	}
	if dev, ok := ada.Answer(colonyAnswerSource(EnrichSourceDevstats)); !ok || dev.Activity[0].URL == "" || dev.Activity[0].Period == "" {
		t.Errorf("devstats answer = %+v, want project link and period", dev)
	}
	if _, ok := ada.Attachments[colonyAttachEnrichment]; ok {
		t.Error("enrichment round-trips through the answers; no fallback attachment expected")
	}
	var ext colonyPersonExt
	if err := json.Unmarshal(ada.Attachments[colonyAttachPerson], &ext); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ext.Typed, FieldOrganization) || ext.Person.Profile.Organization.Value != "" {
		t.Errorf("a profile field colony gives back exactly is duplicated in the attachment: typed=%v", ext.Typed)
	}

	// The enrichment store reads from colony.db.
	if pe, ok := b.enrich.Get(fixAda); !ok || len(pe.Affiliations) != 2 || len(pe.Activity) != 1 {
		t.Errorf("enrichment in colony-only = %+v", pe)
	}
}

// After the migration no change writes the JSON, and a restart reads
// everything back from colony.db.
func TestColonyOnlyWritesNoJSONAndSurvivesRestart(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := b.store.SetNotes(fixBea, "fixture note"); err != nil {
		t.Fatal(err)
	}
	at := clk.now()
	if err := b.enrich.SetSource(fixBea, EnrichSourceGitdm, []EnrichedAffiliation{{Org: "Fixture Org", From: "2024-01-01", Confidence: "high"}}, nil, at); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(dir, FileName))
	mustNotExist(t, filepath.Join(dir, EnrichmentFileName))
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}

	again := bootColony(t, dir, StoreColonyOnly, clk)
	bea, err := again.store.Get(fixBea)
	if err != nil || bea.Notes != "fixture note" {
		t.Fatalf("after restart bea = %+v, %v", bea, err)
	}
	if pe, ok := again.enrich.Get(fixBea); !ok || len(pe.Affiliations) != 1 || pe.Affiliations[0].Org != "Fixture Org" {
		t.Errorf("enrichment after restart = %+v", pe)
	}
	if len(again.store.People()) != 3 {
		t.Errorf("people after restart = %d, want 3", len(again.store.People()))
	}
	mustNotExist(t, filepath.Join(dir, FileName))
}

// colony export writes the JSON back, refuses to overwrite, and a boot in
// colony mode with no people.json restores it automatically.
func TestColonyOnlyExportAndRollback(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := b.store.SetNotes(fixBea, "fixture note"); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportColony(dir, t.TempDir(), false); err == nil {
		t.Fatal("export while the hive holds colony.db must fail")
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	rep, err := ExportColony(dir, out, false)
	if err != nil {
		t.Fatalf("ExportColony: %v", err)
	}
	if rep.People != 3 || rep.Enrichment != 1 {
		t.Errorf("report = %+v", rep)
	}
	if _, err := ExportColony(dir, out, false); err == nil {
		t.Error("a second export overwrote people.json without --force")
	}
	if _, err := ExportColony(dir, out, true); err != nil {
		t.Errorf("export with force: %v", err)
	}
	exported, err := Open(out, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if bea, _ := exported.Get(fixBea); bea.Notes != "fixture note" {
		t.Errorf("exported bea = %+v", bea)
	}

	// Rollback to colony: just the mode change.
	restored, err := RestoreFromColony(dir, nil)
	if err != nil || !restored {
		t.Fatalf("RestoreFromColony = %v, %v", restored, err)
	}
	if again, err := RestoreFromColony(dir, nil); err != nil || again {
		t.Errorf("a second restore = %v, %v; want nothing to do", again, err)
	}
	c := bootColony(t, dir, StoreColony, clk)
	if bea, _ := c.store.Get(fixBea); bea.Notes != "fixture note" {
		t.Errorf("after rollback to colony bea = %+v", bea)
	}
	c.colony.mu.Lock()
	_, hasDoc, err := c.colony.db.StoreAttachment(colonyAttachDoc)
	c.colony.mu.Unlock()
	if err != nil || hasDoc {
		t.Errorf("the colony-only document is still there after the rollback (%v): a later restore could go back in time", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+MigratedSuffix)); err != nil {
		t.Errorf("the migrated file must be kept: %v", err)
	}
}

// Rollback straight to json: the restored people.json is read.
func TestColonyOnlyRollbackToJSON(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := b.store.SetNotes(fixCy, "fixture note"); err != nil {
		t.Fatal(err)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := RestoreFromColony(dir, nil); err != nil || !ok {
		t.Fatalf("RestoreFromColony = %v, %v", ok, err)
	}
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if cy, _ := s.Get(fixCy); cy.Notes != "fixture note" {
		t.Errorf("json mode after rollback: cy = %+v", cy)
	}
}

// A forget in colony-only erases the person from colony.db and from the
// renamed JSON files.
func TestColonyOnlyForgetScrubsMigratedFiles(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := b.store.Forget(fixAda); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	_, err := b.colony.db.Get(fixAda)
	b.colony.mu.Unlock()
	if !errors.Is(err, cstore.ErrNotFound) {
		t.Errorf("forgotten person still in colony.db: %v", err)
	}
	for _, name := range []string{FileName + MigratedSuffix, EnrichmentFileName + MigratedSuffix} {
		if ids := backupIDsIn(t, filepath.Join(dir, name)); slices.Contains(ids, fixAda) {
			t.Errorf("%s still holds the forgotten person", name)
		}
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := again.store.Get(fixAda); !errors.Is(err, ErrNotFound) {
		t.Errorf("forgotten person came back after restart: %v", err)
	}
}

func backupIDsIn(t *testing.T, path string) []string {
	t.Helper()
	data := readFile(t, path)
	var ids []string
	var pf storeFile
	if json.Unmarshal(data, &pf) == nil {
		for _, p := range pf.People {
			ids = append(ids, p.ID)
		}
	}
	var ef enrichmentFile
	if json.Unmarshal(data, &ef) == nil {
		for id := range ef.People {
			ids = append(ids, id)
		}
	}
	return ids
}

// A colony.db written by another mode, with no people.json beside it, is
// never guessed at in colony-only mode.
func TestColonyOnlyRefusesForeignStoreWithoutJSON(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreShadow, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, FileName), filepath.Join(dir, "aside.json")); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreColonyOnly, nil); err == nil {
		_ = c.Close()
		t.Fatal("colony-only attached to a shadow colony.db with no people.json")
	}
	if restored, err := RestoreFromColony(dir, nil); err != nil || restored {
		t.Errorf("RestoreFromColony on a shadow store = %v, %v; want nothing to do", restored, err)
	}
}

// A second identity holder is kept as a colony held identity in shadow
// mode, and parity counts it.
func TestColonyShadowHoldsConflictingIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatal(err)
	}
	at := "2026-01-02T03:04:05Z"
	people := `{"version":1,"salt":"00112233445566778899aabbccddeeff","people":[
 {"id":"p_00000000000a","name":"One","status":"customer","origin":"manual","satisfaction":{"level":"unknown"},
  "identities":[{"kind":"github","value":"fixture-dup","source":"manual","confidence":1,"manual":true,"at":"` + at + `","by":"owner"}],
  "created_at":"` + at + `","updated_at":"` + at + `"},
 {"id":"p_00000000000b","name":"Two","status":"customer","origin":"manual","satisfaction":{"level":"unknown"},
  "identities":[{"kind":"github","value":"fixture-dup","source":"self","confidence":0.3,"claimed":true,"at":"` + at + `","by":"two"}],
  "created_at":"` + at + `","updated_at":"` + at + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(people), storeFileMode); err != nil {
		t.Fatal(err)
	}
	b := bootColony(t, dir, StoreShadow, newClock())
	two := colonyPerson(t, b.colony, "p_00000000000b")
	if len(two.Held) != 1 || two.Held[0].Value != "fixture-dup" || two.Held[0].By != "two" {
		t.Fatalf("second claimant = %+v, want the login held with its provenance", two)
	}
	par, err := b.store.ColonyParity()
	if err != nil {
		t.Fatal(err)
	}
	if par.JSONHeld != 1 || par.ColonyHeld != 1 || par.Mismatches != 0 {
		t.Errorf("parity = %+v, want one held identity on both sides and no mismatch", par)
	}
}

func TestColonyAnswersRoundTrip(t *testing.T) {
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	pe := PersonEnrichment{
		Affiliations: []EnrichedAffiliation{
			{Org: "Acme", From: "2019-01-01", Until: "2022-01-01", Source: EnrichSourceGitdm, Confidence: "high", MatchedBy: "github_login"},
			{Org: "Hooli", From: "2022-01-01", Source: EnrichSourceGitdm, Confidence: "medium"},
		},
		Activity:  []EnrichedActivity{{Project: "Fixture", Contributions: 3, Period: "all time", URL: "https://devstats.example/x", Source: EnrichSourceDevstats}},
		FetchedAt: map[string]time.Time{EnrichSourceGitdm: at, EnrichSourceDevstats: at, EnrichSourceCrowdDev: at},
	}
	answers := colonyAnswers(pe)
	if len(answers) != 3 {
		t.Fatalf("answers = %d, want one per source (a source with no entries keeps its fetch time)", len(answers))
	}
	if !sameJSON(enrichmentFromAnswers(answers), pe) {
		t.Errorf("round trip = %+v, want %+v", enrichmentFromAnswers(answers), pe)
	}
}

func TestRenameMigratedNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(path, []byte("{}"), storeFileMode); err != nil {
			t.Fatal(err)
		}
		if _, err := renameMigrated(path, now); err != nil {
			t.Fatalf("rename %d: %v", i, err)
		}
	}
	if err := os.WriteFile(path, []byte("{}"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := renameMigrated(path, now); err == nil {
		t.Error("a third rename at the same second overwrote a migrated file")
	}
	if to, err := renameMigrated(filepath.Join(dir, "missing.json"), now); err != nil || to != "" {
		t.Errorf("missing file: %q, %v", to, err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, FileName+MigratedSuffix+"*"))
	if len(matches) != 2 {
		t.Errorf("migrated files = %v, want two kept", matches)
	}
}
