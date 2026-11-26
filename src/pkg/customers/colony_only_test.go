package customers

import (
	"encoding/json"
	"errors"
	"math"
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

// Merge and unmerge in colony-only: the merged person's record is emptied,
// the identity moves, and both survive a restart exactly.
func TestColonyOnlyMergeAndUnmerge(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if _, err := b.store.Merge(fixAda, fixBea, "owner"); err != nil {
		t.Fatal(err)
	}
	b.colony.mu.Lock()
	owner, err := b.colony.db.FindByIdentity(cstore.KindGitHub, "bea-sample")
	stripped, _ := b.colony.db.Get(fixBea)
	b.colony.mu.Unlock()
	if err != nil || owner.ID != fixAda {
		t.Fatalf("after merge the login belongs to %q (%v), want %s", owner.ID, err, fixAda)
	}
	if stripped.Status != colonyStatusRemoved || len(stripped.Identities) != 0 || len(stripped.Attachments) != 0 {
		t.Errorf("merged-away record = %+v, want emptied", stripped)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	if len(again.store.People()) != 2 {
		t.Fatalf("people after restart = %d, want 2 (bea merged into ada)", len(again.store.People()))
	}
	if _, _, err := again.store.Unmerge(fixAda, fixBea); err != nil {
		t.Fatal(err)
	}
	again.colony.mu.Lock()
	owner, err = again.colony.db.FindByIdentity(cstore.KindGitHub, "bea-sample")
	again.colony.mu.Unlock()
	if err != nil || owner.ID != fixBea {
		t.Fatalf("after unmerge the login belongs to %q (%v), want %s", owner.ID, err, fixBea)
	}
}

// A colony write that fails is returned and changes nothing, in memory or
// on disk: colony-only never loses a change silently.
func TestColonyOnlyFailedWriteChangesNothing(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetNotes(fixBea, "lost?"); err == nil {
		t.Fatal("a write with colony.db closed succeeded")
	}
	if bea, _ := b.store.Get(fixBea); bea.Notes == "lost?" {
		t.Error("a failed write changed the store in memory")
	}
	if err := b.enrich.SetSource(fixBea, EnrichSourceGitdm, nil, nil, clk.now()); err == nil {
		t.Error("an enrichment write with colony.db closed succeeded")
	}
	if _, ok := b.enrich.Get(fixBea); ok {
		t.Error("a failed enrichment write is shown in memory")
	}
	if err := b.enrich.Delete(fixAda); err == nil {
		t.Error("an enrichment delete with colony.db closed succeeded")
	}
	if _, ok := b.enrich.Get(fixAda); !ok {
		t.Error("a failed enrichment delete dropped the entry in memory")
	}
	if n, err := b.enrich.DropSource(EnrichSourceGitdm); err == nil || n != 0 {
		t.Errorf("DropSource with colony.db closed = %d, %v; want an error and nothing dropped", n, err)
	}
	if pe, _ := b.enrich.Get(fixAda); len(pe.Affiliations) != 2 {
		t.Error("a failed DropSource dropped the entries in memory")
	}
	mustNotExist(t, filepath.Join(dir, FileName))
	mustNotExist(t, filepath.Join(dir, EnrichmentFileName))
}

// Enrichment removed in colony-only leaves colony.db, and an answer that
// does not round-trip is kept exactly in the fallback attachment.
func TestColonyOnlyEnrichmentRemoveAndFallback(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.enrich.Delete(fixAda); err != nil {
		t.Fatal(err)
	}
	ada := colonyPerson(t, b.colony, fixAda)
	if len(ada.Answers) != 0 {
		t.Errorf("answers after delete = %+v", ada.Answers)
	}
	for _, f := range ada.Facts {
		if isEnrichmentField(f.Field) {
			t.Errorf("enrichment fact after delete: %s", f.Field)
		}
	}

	// A confidence colony has no value for does not round-trip.
	odd := PersonEnrichment{Affiliations: []EnrichedAffiliation{{Org: "Odd Org", Source: EnrichSourceGitdm, Confidence: "certain"}},
		FetchedAt: map[string]time.Time{EnrichSourceGitdm: clk.now()}}
	b.colony.mu.Lock()
	err := b.colony.db.Batch(func(bt *cstore.Batch) error {
		return colonyWriteEnrichment(bt, fixBea, odd, true, clk.now())
	})
	bea, _ := b.colony.db.Get(fixBea)
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bea.Attachments[colonyAttachEnrichment]; !ok {
		t.Fatal("an enrichment entry that does not round-trip has no fallback attachment")
	}
	if got, ok, err := enrichmentFromColonyPerson(bea); err != nil || !ok || !sameJSON(got, odd) {
		t.Errorf("fallback read = %+v, %v, %v; want the entry exactly", got, ok, err)
	}
}

func TestColonyOnlyDecodeErrors(t *testing.T) {
	if _, _, err := personFromColony(cstore.Person{Attachments: map[string]json.RawMessage{colonyAttachPerson: json.RawMessage(`[]`)}}); err == nil {
		t.Error("a damaged hive.person attachment decoded")
	}
	newer, _ := json.Marshal(colonyPersonExt{Version: colonyOnlyVersion + 1})
	if _, _, err := personFromColony(cstore.Person{Attachments: map[string]json.RawMessage{colonyAttachPerson: newer}}); err == nil {
		t.Error("a hive.person attachment from a newer release decoded")
	}
	if _, ok, err := personFromColony(cstore.Person{ID: "p_x"}); ok || err != nil {
		t.Errorf("a record with no attachment: ok=%v err=%v", ok, err)
	}
	if _, _, err := enrichmentFromColonyPerson(cstore.Person{Attachments: map[string]json.RawMessage{colonyAttachEnrichment: json.RawMessage(`"x"`)}}); err == nil {
		t.Error("a damaged hive.enrichment attachment decoded")
	}
	if _, ok, _ := enrichmentFromColonyPerson(cstore.Person{}); ok {
		t.Error("a record with no answers holds enrichment")
	}
	// Entries colony would refuse are left out of the answers.
	pe := PersonEnrichment{
		Affiliations: []EnrichedAffiliation{{Org: " ", Source: EnrichSourceGitdm}, {Org: "x", Source: ""},
			{Org: "Back", From: "2024-01-01", Until: "2020-01-01", Source: EnrichSourceGitdm}},
		Activity: []EnrichedActivity{{Project: "", Source: EnrichSourceDevstats}, {Project: "p", Contributions: -1, Source: EnrichSourceDevstats},
			{Project: "p", Contributions: 1, URL: "ftp://x", Source: EnrichSourceDevstats}},
	}
	answers := colonyAnswers(pe)
	if len(answers) != 2 || len(answers[0].Affiliations) != 0 || len(answers[1].Activity) != 1 || answers[1].Activity[0].URL != "" {
		t.Errorf("answers = %+v", answers)
	}
}

func TestColonyExportAndRestoreEdges(t *testing.T) {
	dir := t.TempDir()
	if _, err := ExportColony(dir, "", false); err == nil {
		t.Error("export with no colony.db succeeded")
	}
	if ok, err := RestoreFromColony(dir, nil); ok || err != nil {
		t.Errorf("restore with no colony.db = %v, %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":1}`), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if ok, err := RestoreFromColony(dir, nil); ok || err != nil {
		t.Errorf("restore with people.json present = %v, %v", ok, err)
	}
}

// shadow writes the full answers too, and drops one when its source goes.
func TestColonyShadowWritesAndDropsAnswers(t *testing.T) {
	dir := fixtureDir(t)
	b := bootColony(t, dir, StoreShadow, newClock())
	ada := colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.Answer(colonyAnswerSource(EnrichSourceDevstats)); !ok {
		t.Fatal("the shadow import wrote no devstats answer")
	}
	if _, err := b.enrich.DropSource(EnrichSourceDevstats); err != nil {
		t.Fatal(err)
	}
	ada = colonyPerson(t, b.colony, fixAda)
	if _, ok := ada.Answer(colonyAnswerSource(EnrichSourceDevstats)); ok {
		t.Error("a dropped source's answer is still in colony.db")
	}
	if _, ok := ada.Answer(colonyAnswerSource(EnrichSourceGitdm)); !ok {
		t.Error("dropping one source removed another's answer")
	}
}

// A fresh hive in colony-only never creates people.json, and its first
// person survives a restart.
func TestColonyOnlyFreshHive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if len(b.store.People()) != 0 {
		t.Fatal("a fresh store holds people")
	}
	p, err := b.store.Add("Fixture Person", []Identity{{Kind: KindGitHub, Value: "fixture-fresh"}}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(dir, FileName))
	if par, err := b.store.ColonyParity(); err != nil || par.ColonyPeople != 1 || par.Mismatches != 0 {
		t.Errorf("colony-only parity = %+v, %v", par, err)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	if got, err := again.store.Get(p.ID); err != nil || got.Name != "Fixture Person" {
		t.Errorf("after restart = %+v, %v", got, err)
	}
	if err := again.colony.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEnrichment(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SetColony(again.colony); err == nil {
		t.Error("reading enrichment from a closed colony store succeeded")
	}
}

// A document written by a newer release is never read.
func TestColonyOnlyRefusesNewerDocument(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "customers")
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		t.Fatal(err)
	}
	db, err := cstore.Open(filepath.Join(dir, ColonyFileName), cstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(colonyDoc{Version: colonyOnlyVersion + 1})
	if err := db.SetStoreAttachment(colonyAttachDoc, doc); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, newClock().now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreColonyOnly, nil); err == nil {
		_ = c.Close()
		t.Fatal("colony-only read a document from a newer release")
	}
	if _, err := ExportColony(dir, t.TempDir(), false); err == nil {
		t.Error("export read a document from a newer release")
	}
}

// A stale people.json put back beside a migrated colony.db (older than the
// migration) is set aside, never taken as the truth: nobody is forgotten,
// and the migrated files keep everyone.
func TestColonyOnlyStrayJSONIsSetAside(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	// What an older binary in json mode would write: a store with only one
	// of the three people.
	partial := `{"version":1,"salt":"ffeeddccbbaa99887766554433221100","people":[{"id":"` + fixBea +
		`","name":"Bea Sample","status":"customer","origin":"accepted","satisfaction":{"level":"unknown"},"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}]}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(partial), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnrichmentFileName), []byte(`{"people":{}}`), storeFileMode); err != nil {
		t.Fatal(err)
	}
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{FileName, EnrichmentFileName} {
		if err := os.Chtimes(filepath.Join(dir, name), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	if again.colony.Mode() != StoreColonyOnly || len(again.store.People()) != 3 {
		t.Fatalf("mode %s with %d people, want colony-only with all 3", again.colony.Mode(), len(again.store.People()))
	}
	if _, err := again.store.Get(fixAda); err != nil {
		t.Fatalf("a person the stray file lacked was lost: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, FileName))
	mustNotExist(t, filepath.Join(dir, EnrichmentFileName))
	if got := readFile(t, filepath.Join(dir, FileName+colonyStraySuffix)); string(got) != partial {
		t.Error("the stray people.json was not kept as it was")
	}
	if ids := backupIDsIn(t, filepath.Join(dir, FileName+MigratedSuffix)); !slices.Contains(ids, fixAda) {
		t.Error("the migrated file lost a person")
	}
	if pe, ok := again.enrich.Get(fixAda); !ok || len(pe.Affiliations) != 2 {
		t.Errorf("enrichment after the stray file = %+v", pe)
	}
}

// A restore drops the colony-only document, so a later missing people.json
// is never filled from the old snapshot again.
func TestColonyOnlyRestoreIsOneShot(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := RestoreFromColony(dir, nil); err != nil || !ok {
		t.Fatalf("RestoreFromColony = %v, %v", ok, err)
	}
	if has, err := colonyHasDoc(dir, clk.now); err != nil || has {
		t.Fatalf("after a restore the document is still there: %v, %v", has, err)
	}
	if err := os.Remove(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if ok, err := RestoreFromColony(dir, nil); err != nil || ok {
		t.Errorf("a second restore = %v, %v; want nothing restored", ok, err)
	}
}

// A migration that does not verify leaves nothing a later boot could take
// for a migrated store, and the JSON is untouched.
func TestColonyOnlyClearsAfterAFailedVerify(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColony, clk)
	b.colony.mu.Lock()
	err := b.colony.db.SetStoreAttachment(colonyAttachDoc, json.RawMessage(`{"version":1}`))
	if err == nil {
		err = b.colony.db.SetAttachment(fixAda, colonyAttachPerson, json.RawMessage(`{"version":1}`))
	}
	if err == nil {
		err = b.colony.clearColonyOnlyLocked()
	}
	ada, _ := b.colony.db.Get(fixAda)
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(ada.Attachments) != 0 {
		t.Errorf("attachments left: %v", ada.Attachments)
	}
	if has, _ := func() (bool, error) {
		b.colony.mu.Lock()
		defer b.colony.mu.Unlock()
		_, ok, err := b.colony.db.StoreAttachment(colonyAttachDoc)
		return ok, err
	}(); has {
		t.Error("the colony-only document was left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("people.json touched: %v", err)
	}
}

func TestColonyIdentityProvenanceMapping(t *testing.T) {
	for in, want := range map[float64]float64{-1: 0, 2: 1, 0.42: 0.42} {
		got := colonyIdentity(Identity{Kind: KindGitHub, Value: "x", Confidence: in, Manual: true, By: "owner"}, cstore.KindGitHub, "x", cstore.LocalityShareable)
		if got.Score != want || !got.Manual || got.By != "owner" {
			t.Errorf("confidence %v mapped to %+v", in, got)
		}
	}
	nan := colonyIdentity(Identity{Confidence: math.NaN()}, cstore.KindGitHub, "x", cstore.LocalityShareable)
	if nan.Score != 0 {
		t.Errorf("NaN confidence mapped to score %v", nan.Score)
	}
	p := Person{ID: "p_b", Identities: []Identity{
		{Kind: KindGitHub, Value: "dup"}, {Kind: KindGitHub, Value: "DUP"}, {Kind: KindGitHub, Value: "gone"},
		{Kind: KindDiscord, Value: "chat"}, {Kind: KindGitHub, Value: "mine"},
	}}
	owners := map[string]string{
		colonyClassKey(cstore.KindGitHub, "dup"):  "p_a",
		colonyClassKey(cstore.KindGitHub, "gone"): "p_a",
		colonyClassKey(cstore.KindGitHub, "mine"): "p_b",
	}
	forgotten := func(_ IdentityKind, v string) bool { return v == "gone" }
	held := colonyHeldIdentities(p, owners, forgotten)
	if len(held) != 1 || held[0].Value != "dup" || held[0].Locality != cstore.LocalityLocalOnly {
		t.Errorf("held = %+v, want only the other owner's login, once, local_only", held)
	}
	if colonyHeldIdentities(p, nil, forgotten) != nil {
		t.Error("held identities without an owner map")
	}
}

func TestColonyOnlyClosedStoreHelpers(t *testing.T) {
	var none *Colony
	if none.colonyOnly() {
		t.Error("a nil Colony is colony-only")
	}
	c := &Colony{mode: StoreColonyOnly, dirty: map[string]bool{}}
	if err := c.dropColonyOnlyDocLocked(); err != nil {
		t.Error(err)
	}
	if err := c.clearColonyOnlyLocked(); err != nil {
		t.Error(err)
	}
	if err := c.saveColonyOnlyEnrichment(map[string]PersonEnrichment{"p_x": {}}, nil); err == nil {
		t.Error("an enrichment save with no store succeeded")
	}
	dir := t.TempDir()
	if has, err := colonyHasDoc(dir, time.Now); err != nil || has {
		t.Errorf("no colony.db: %v, %v", has, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ColonyFileName), []byte("not a bolt file"), storeFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := colonyHasDoc(dir, time.Now); err == nil {
		t.Error("a damaged colony.db reported no document")
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"version":1}`), storeFileMode); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreColonyOnly, nil); err == nil {
		_ = c.Close()
		t.Error("colony-only attached beside a damaged colony.db")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("people.json moved after a failed attach: %v", err)
	}
}

// An enrichment.json that cannot be read stops the boot before anything is
// migrated: nothing is renamed, and the wiring falls back.
func TestColonyOnlyUnreadableEnrichmentFailsClosed(t *testing.T) {
	dir := fixtureDir(t)
	ep := filepath.Join(dir, EnrichmentFileName)
	if err := os.Remove(ep); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ep, storeDirMode); err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	s, err := Open(dir, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := AttachColony(s, StoreColonyOnly, nil); err == nil {
		_ = c.Close()
		t.Fatal("colony-only attached with an unreadable enrichment.json")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Errorf("people.json renamed although the migration did not run: %v", err)
	}
}

// Enrichment for an id that is not a person stays in the migrated file and
// does not block the migration.
func TestColonyOnlyEnrichmentOrphanKeptInMigratedFile(t *testing.T) {
	dir := fixtureDir(t)
	ep := filepath.Join(dir, EnrichmentFileName)
	var f enrichmentFile
	if err := json.Unmarshal(readFile(t, ep), &f); err != nil {
		t.Fatal(err)
	}
	f.People["p_orphan00000"] = PersonEnrichment{Affiliations: []EnrichedAffiliation{{Org: "Orphan Org", Source: EnrichSourceGitdm}}}
	data, _ := json.Marshal(f)
	if err := os.WriteFile(ep, data, storeFileMode); err != nil {
		t.Fatal(err)
	}
	b := bootColony(t, dir, StoreColonyOnly, newClock())
	if b.colony.Mode() != StoreColonyOnly {
		t.Fatalf("mode = %s, want colony-only", b.colony.Mode())
	}
	if ids := backupIDsIn(t, filepath.Join(dir, EnrichmentFileName+MigratedSuffix)); !slices.Contains(ids, "p_orphan00000") {
		t.Error("the orphan entry is not kept in the migrated file")
	}
	if _, ok := b.enrich.Get("p_orphan00000"); ok {
		t.Error("an orphan entry was loaded as enrichment")
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportColony(dir, filepath.Join(dir, FileName+MigratedSuffix), true); err == nil {
		t.Error("export into a path that is a file succeeded")
	}
}

// A people.json written after the migration (an older binary that read
// colony-only as json) is newer than both copies' common point: neither is
// chosen, nothing is moved, and colony.db keeps everyone. A colony or shadow
// boot refuses too, since its orphan forget would erase what people.json
// lacks.
func TestColonyOnlyNewerJSONFailsClosed(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	partial := `{"version":1,"salt":"ffeeddccbbaa99887766554433221100","people":[]}`
	pp := filepath.Join(dir, FileName)
	if err := os.WriteFile(pp, []byte(partial), storeFileMode); err != nil {
		t.Fatal(err)
	}
	future := clk.t.Add(time.Hour)
	if err := os.Chtimes(pp, future, future); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []StoreMode{StoreColonyOnly, StoreColony, StoreShadow} {
		s, err := Open(dir, clk.now)
		if err != nil {
			t.Fatal(err)
		}
		if c, err := AttachColony(s, mode, nil); err == nil {
			_ = c.Close()
			t.Fatalf("%s chose between a newer people.json and a migrated colony.db", mode)
		}
	}
	if got := readFile(t, pp); string(got) != partial {
		t.Error("people.json was touched")
	}
	db, err := cstore.Open(filepath.Join(dir, ColonyFileName), cstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cc, err := readColonyOnly(db)
	if err != nil || len(cc.people) != 3 || !cc.hasDoc {
		t.Errorf("colony.db after the refusals: %d people, doc %v, %v", len(cc.people), cc.hasDoc, err)
	}
}

// A document left by an attempt that never finished (no migration time)
// does not make people.json stray: the next colony-only boot migrates from
// it.
func TestColonyOnlyLeftoverDocMigratesAgain(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColony, clk)
	b.colony.mu.Lock()
	err := b.colony.db.SetStoreAttachment(colonyAttachDoc, json.RawMessage(`{"version":1,"salt":"00"}`))
	b.colony.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.SetNotes(fixCy, "after the leftover"); err != nil {
		t.Fatal(err)
	}
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	if again.colony.Mode() != StoreColonyOnly {
		t.Fatalf("mode = %s, want colony-only", again.colony.Mode())
	}
	if cy, _ := again.store.Get(fixCy); cy.Notes != "after the leftover" {
		t.Errorf("the leftover document won over people.json: %+v", cy)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*"+colonyStraySuffix+"*")); len(m) != 0 {
		t.Errorf("files set aside: %v", m)
	}
	mustNotExist(t, filepath.Join(dir, FileName))
}

// A stop after people.json was renamed but before enrichment.json and the
// migration time: the next boot reads the verified store and finishes.
func TestColonyOnlyFinishesAnInterruptedRename(t *testing.T) {
	dir := fixtureDir(t)
	clk := newClock()
	b := bootColony(t, dir, StoreColonyOnly, clk)
	if err := b.colony.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, EnrichmentFileName+MigratedSuffix), filepath.Join(dir, EnrichmentFileName)); err != nil {
		t.Fatal(err)
	}
	if err := clearMigratedMarker(dir); err != nil {
		t.Fatal(err)
	}
	again := bootColony(t, dir, StoreColonyOnly, clk)
	mustNotExist(t, filepath.Join(dir, EnrichmentFileName))
	if _, err := os.Stat(filepath.Join(dir, EnrichmentFileName+MigratedSuffix)); err != nil {
		t.Errorf("enrichment.json not renamed .migrated: %v", err)
	}
	if again.colony.marker.MigratedAt.IsZero() {
		t.Error("the migration time was not recorded")
	}
	if pe, ok := again.enrich.Get(fixAda); !ok || len(pe.Affiliations) != 2 {
		t.Errorf("enrichment = %+v", pe)
	}
}
