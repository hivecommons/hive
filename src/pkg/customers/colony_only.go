package customers

// customers.store colony-only (#19972): colony.db is the only store.
//
// Everything people.json and enrichment.json hold is kept in colony.db, in
// colony's own slots where it has one and as hive's attachments where it
// does not:
//
//   - identities with their provenance (manual, by, the numeric confidence
//     as colony's score), and identities another person owns as colony held
//     identities; profile fields as facts at their rank; dismissed
//     affiliations; status;
//   - enrichment: the effective affiliation and activity counts as facts,
//     and every provider answer in full (each affiliation with its period,
//     each project with its name, period and link, when the source
//     answered) as colony answers;
//   - the person's "hive.person" attachment: hive's own record of them (tags,
//     goals, satisfaction, notes, merge records and so on, the identities in
//     hive's order with the kinds colony has no kind for). Every profile
//     field colony's facts give back exactly is left out of it and read from
//     colony; one that does not round-trip stays in it, so nothing is lost;
//   - a "hive.enrichment" attachment only when the answers do not give the
//     enrichment back exactly;
//   - the store-level "hive.people" attachment: the rest of people.json
//     (salt, suggestions, pins, hints, tombstones).
//
// The first colony-only boot brings colony.db in line from the JSON as the
// colony mode does, writes every person in full in one transaction, reads
// it all back and compares it with the JSON. Only when every person, every
// enrichment entry and the document agree are people.json and
// enrichment.json renamed to *.migrated: kept, never deleted. A mismatch
// leaves the hive in colony mode with the JSON as it was.
//
// After that every change is one colony transaction and nothing writes the
// JSON. A write that fails is returned to the caller and nothing changes.
// ExportColony writes people.json and enrichment.json back out, and a boot
// in any other mode that finds no people.json beside a colony-only
// colony.db restores them first, so going back to colony, shadow or json
// is only a mode change.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/colony/resolver"
	cstore "github.com/hivecommons/colony/store"
)

const (
	// MigratedSuffix names the JSON files a colony-only migration renamed.
	MigratedSuffix = ".migrated"
	// colonyAttachPerson, colonyAttachEnrichment and colonyAttachDoc are
	// hive's attachment namespaces in colony.db.
	colonyAttachPerson     = "hive.person"
	colonyAttachEnrichment = "hive.enrichment"
	colonyAttachDoc        = "hive.people"
	// colonyOnlyVersion versions the attachments' shape.
	colonyOnlyVersion = 1
	// colonyTypedDismissed marks the dismissed affiliations as read from
	// colony in colonyPersonExt.Typed.
	colonyTypedDismissed = "dismissed_affiliations"
	// colonyMigratedStamp suffixes a *.migrated name that is already taken.
	colonyMigratedStamp = "20060102T150405Z"
	// colonyExportLockTimeout bounds the wait for colony.db's lock in an
	// export: a running hive holds it.
	colonyExportLockTimeout = 2 * time.Second
)

// colonyPersonExt is the hive.person attachment.
type colonyPersonExt struct {
	Version int    `json:"version"`
	Person  Person `json:"person"`
	// Typed names the profile fields (and colonyTypedDismissed) that are
	// left out of Person because colony's facts give them back exactly.
	Typed []string `json:"typed,omitempty"`
}

// colonyDoc is the hive.people attachment: people.json without its people.
type colonyDoc struct {
	Version     int          `json:"version"`
	Salt        string       `json:"salt"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	Pins        []Pin        `json:"pins,omitempty"`
	Hints       []Hint       `json:"hints,omitempty"`
	Tombstones  []string     `json:"tombstones,omitempty"`
}

// ColonyExportReport counts what ExportColony wrote. It holds no values.
type ColonyExportReport struct {
	People     int    `json:"people"`
	Enrichment int    `json:"enrichment"`
	PeopleFile string `json:"people_file"`
	EnrichFile string `json:"enrichment_file"`
}

// colonyOnly reports whether c is in colony-only mode.
func (c *Colony) colonyOnly() bool { return c != nil && c.mode == StoreColonyOnly }

// sameJSON reports whether a and b encode to the same JSON.
func sameJSON(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// colonyAnswerSource is the colony answer source of a hive enrichment source.
func colonyAnswerSource(source string) string { return cstore.ColonySourcePrefix + source }

// colonyAnswers maps one person's enrichment to colony answers, one per
// source, in the order the sources first appear. An entry colony would
// refuse is left out; the hive.enrichment attachment then keeps it.
func colonyAnswers(pe PersonEnrichment) []cstore.Answer {
	var order []string
	bySource := map[string]*cstore.Answer{}
	get := func(src string) *cstore.Answer {
		if a, ok := bySource[src]; ok {
			return a
		}
		a := &cstore.Answer{Source: colonyAnswerSource(src), FetchedAt: pe.FetchedAt[src]}
		bySource[src] = a
		order = append(order, src)
		return a
	}
	for _, a := range pe.Affiliations {
		if a.Source == "" || strings.TrimSpace(a.Org) == "" {
			continue
		}
		ans := get(a.Source)
		aff := resolver.Affiliation{Org: a.Org, From: parseEnrichedDate(a.From), Until: parseEnrichedDate(a.Until),
			Source: a.Source, MatchedBy: a.MatchedBy}
		switch a.Confidence {
		case EnrichConfidenceHigh, EnrichConfidenceMedium, EnrichConfidenceLow:
			aff.Confidence = resolver.Confidence(a.Confidence)
		}
		if !aff.From.IsZero() && !aff.Until.IsZero() && aff.Until.Before(aff.From) {
			continue
		}
		ans.Affiliations = append(ans.Affiliations, aff)
	}
	for _, t := range pe.Activity {
		if t.Source == "" || strings.TrimSpace(t.Project) == "" || t.Contributions < 0 {
			continue
		}
		act := resolver.Activity{Project: t.Project, Contributions: t.Contributions, Period: t.Period, Source: t.Source}
		if strings.HasPrefix(t.URL, "https://") || strings.HasPrefix(t.URL, "http://") {
			act.URL = t.URL
		}
		get(t.Source).Activity = append(get(t.Source).Activity, act)
	}
	var rest []string
	for src := range pe.FetchedAt {
		if _, ok := bySource[src]; !ok && src != "" {
			rest = append(rest, src)
		}
	}
	sort.Strings(rest)
	for _, src := range rest {
		get(src)
	}
	out := make([]cstore.Answer, 0, len(order))
	for _, src := range order {
		out = append(out, *bySource[src])
	}
	return out
}

// enrichmentFromAnswers is the inverse of colonyAnswers.
func enrichmentFromAnswers(answers []cstore.Answer) PersonEnrichment {
	var pe PersonEnrichment
	for _, a := range answers {
		src, ok := strings.CutPrefix(a.Source, cstore.ColonySourcePrefix)
		if !ok || src == "" {
			continue
		}
		for _, aff := range a.Affiliations {
			e := EnrichedAffiliation{Org: aff.Org, Source: aff.Source, Confidence: string(aff.Confidence), MatchedBy: aff.MatchedBy}
			if !aff.From.IsZero() {
				e.From = aff.From.UTC().Format(enrichmentDateLayout)
			}
			if !aff.Until.IsZero() {
				e.Until = aff.Until.UTC().Format(enrichmentDateLayout)
			}
			pe.Affiliations = append(pe.Affiliations, e)
		}
		for _, act := range a.Activity {
			pe.Activity = append(pe.Activity, EnrichedActivity{Project: act.Project, Contributions: act.Contributions,
				Period: act.Period, URL: act.URL, Source: act.Source})
		}
		if !a.FetchedAt.IsZero() {
			if pe.FetchedAt == nil {
				pe.FetchedAt = map[string]time.Time{}
			}
			pe.FetchedAt[src] = a.FetchedAt.UTC()
		}
	}
	return pe
}

// enrichmentFromColony is one person's enrichment as colony-only holds it:
// the hive.enrichment attachment when there is one, else the answers. ok is
// false when colony holds none.
func enrichmentFromColonyPerson(cp cstore.Person) (PersonEnrichment, bool, error) {
	if raw, ok := cp.Attachments[colonyAttachEnrichment]; ok {
		var pe PersonEnrichment
		if err := json.Unmarshal(raw, &pe); err != nil {
			return PersonEnrichment{}, false, fmt.Errorf("customers: decoding %s: %w", colonyAttachEnrichment, err)
		}
		return pe, true, nil
	}
	if len(cp.Answers) == 0 {
		return PersonEnrichment{}, false, nil
	}
	return enrichmentFromAnswers(cp.Answers), true, nil
}

// colonyProfileView is the profile colony's facts give back: the four
// profile fields and the dismissed affiliations, as the colony mode reads
// them.
func colonyProfileView(cp cstore.Person) (map[string]ProfileField, []string) {
	view := cp.EffectiveView(cstore.ViewOptions{ExcludeSuggested: true})
	fields := make(map[string]ProfileField, len(colonyOwnedProfileFields))
	for _, name := range colonyOwnedProfileFields {
		if eff, ok := view[name]; ok {
			fields[name] = profileFieldFromFact(eff)
		} else {
			fields[name] = ProfileField{}
		}
	}
	var dismissed []string
	for _, d := range cp.Dismissed {
		if d.Field == cstore.FieldAffiliation {
			dismissed = append(dismissed, strings.ToLower(d.Value))
		}
	}
	if len(dismissed) > MaxDismissedAffiliations {
		dismissed = dismissed[len(dismissed)-MaxDismissedAffiliations:]
	}
	return fields, dismissed
}

// colonyPersonExtFor builds the hive.person attachment for p, given what
// colony now holds for p: each profile field colony gives back exactly is
// left out and marked typed.
func colonyPersonExtFor(p Person, cp cstore.Person) colonyPersonExt {
	ext := colonyPersonExt{Version: colonyOnlyVersion, Person: clonePerson(p)}
	fields, dismissed := colonyProfileView(cp)
	for _, name := range colonyOwnedProfileFields {
		if sameJSON(*p.Profile.Field(name), fields[name]) {
			*ext.Person.Profile.Field(name) = ProfileField{}
			ext.Typed = append(ext.Typed, name)
		}
	}
	if sameJSON(p.Profile.DismissedAffiliations, dismissed) {
		ext.Person.Profile.DismissedAffiliations = nil
		ext.Typed = append(ext.Typed, colonyTypedDismissed)
	}
	return ext
}

// personFromColony is one person as colony-only holds them. ok is false for
// a record with no hive.person attachment (a merged-away person's emptied
// record, or one written by another mode).
func personFromColony(cp cstore.Person) (Person, bool, error) {
	raw, ok := cp.Attachments[colonyAttachPerson]
	if !ok {
		return Person{}, false, nil
	}
	var ext colonyPersonExt
	if err := json.Unmarshal(raw, &ext); err != nil {
		return Person{}, false, fmt.Errorf("customers: decoding %s: %w", colonyAttachPerson, err)
	}
	if ext.Version > colonyOnlyVersion {
		return Person{}, false, fmt.Errorf("customers: %s version %d is newer than this release", colonyAttachPerson, ext.Version)
	}
	p := ext.Person
	fields, dismissed := colonyProfileView(cp)
	for _, name := range ext.Typed {
		if name == colonyTypedDismissed {
			p.Profile.DismissedAffiliations = dismissed
			continue
		}
		if f := p.Profile.Field(name); f != nil {
			*f = fields[name]
		}
	}
	return p, true, nil
}

// colonyRecordOf is cp as a Replace record, so a write that changes one part
// of a person keeps the rest exactly.
func colonyRecordOf(cp cstore.Person) cstore.Record {
	held := make([]cstore.Identity, 0, len(cp.Held))
	for _, h := range cp.Held {
		held = append(held, h.Identity)
	}
	return cstore.Record{
		Person:      cstore.PersonSpec{ID: cp.ID, Status: cp.Status, Identities: cp.Identities, Held: held},
		Facts:       cp.Facts,
		Consents:    cp.Consents,
		Dismissed:   cp.Dismissed,
		Answers:     cp.Answers,
		Attachments: withoutAttachments(cp.Attachments),
		CreatedAt:   cp.CreatedAt,
	}
}

// withoutAttachments copies attachments, leaving out the names given.
func withoutAttachments(in map[string]json.RawMessage, drop ...string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

// colonyWritePerson writes p's people part in full inside b, keeping the
// enrichment colony holds for them, and then its hive.person attachment.
func colonyWritePerson(b *cstore.Batch, p Person, forgotten func(IdentityKind, string) bool) (cstore.Person, error) {
	rec := colonyRecord(p, nil, forgotten)
	if cp, err := b.Get(p.ID); err == nil {
		for _, f := range cp.Facts {
			if !isProfileField(f.Field) {
				rec.Facts = append(rec.Facts, f)
			}
		}
		rec.Answers, rec.Consents = cp.Answers, cp.Consents
		rec.Attachments = withoutAttachments(cp.Attachments, colonyAttachPerson)
	} else if !errors.Is(err, cstore.ErrNotFound) {
		return cstore.Person{}, err
	}
	if _, err := b.Replace(rec); err != nil {
		return cstore.Person{}, err
	}
	got, err := b.Get(p.ID)
	if err != nil {
		return cstore.Person{}, err
	}
	raw, err := json.Marshal(colonyPersonExtFor(p, got))
	if err != nil {
		return cstore.Person{}, err
	}
	if err := b.SetAttachment(p.ID, colonyAttachPerson, raw); err != nil {
		return cstore.Person{}, err
	}
	return b.Get(p.ID)
}

// colonyWriteEnrichment writes one person's enrichment inside b: facts,
// answers and, when the answers do not give it back exactly, the
// hive.enrichment attachment. held false removes it. A person colony does
// not hold is skipped.
func colonyWriteEnrichment(b *cstore.Batch, id string, pe PersonEnrichment, held bool, now time.Time) error {
	cp, err := b.Get(id)
	if errors.Is(err, cstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	rec := colonyRecordOf(cp)
	rec.Facts = nil
	for _, f := range cp.Facts {
		if !isEnrichmentField(f.Field) {
			rec.Facts = append(rec.Facts, f)
		}
	}
	rec.Answers = nil
	delete(rec.Attachments, colonyAttachEnrichment)
	if held {
		rec.Facts = append(rec.Facts, colonyEnrichmentFacts(pe, cp.Identities, now, cp.Dismissed...)...)
		rec.Answers = colonyAnswers(pe)
	}
	if _, err := b.Replace(rec); err != nil {
		return err
	}
	if !held {
		return nil
	}
	got, err := b.Get(id)
	if err != nil {
		return err
	}
	if back, ok, err := enrichmentFromColonyPerson(got); err != nil || (ok && sameJSON(back, pe)) {
		return err
	}
	raw, err := json.Marshal(pe)
	if err != nil {
		return err
	}
	return b.SetAttachment(id, colonyAttachEnrichment, raw)
}

// colonyForgetInBatch is colonyForgetOne inside a batch.
func colonyForgetInBatch(b *cstore.Batch, id string) error {
	cp, err := b.Get(id)
	if errors.Is(err, cstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, i := range cp.Identities {
		if i.Kind == cstore.KindOIDC || !i.Verified {
			if err := b.RemoveIdentity(id, i.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
				return err
			}
		}
	}
	if _, err := b.Forget(id); err != nil && !errors.Is(err, cstore.ErrNotFound) {
		return err
	}
	return nil
}

// colonyStrip empties the record of a person merged into someone else,
// keeping the id with the status removed (an unmerge brings it back from the
// merge record).
func colonyStrip(b *cstore.Batch, id string) error {
	if _, err := b.Get(id); errors.Is(err, cstore.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := b.Replace(cstore.Record{Person: cstore.PersonSpec{ID: id, Status: colonyStatusRemoved}})
	return err
}

// docLocked is the hive.people document of s; s.mu is held.
func (s *Store) docLocked() ([]byte, error) {
	d := colonyDoc{Version: colonyOnlyVersion, Salt: s.salt, Pins: s.pins, Hints: s.hints}
	for _, sg := range s.sugg {
		d.Suggestions = append(d.Suggestions, sg)
	}
	sort.Slice(d.Suggestions, func(i, j int) bool { return d.Suggestions[i].ID < d.Suggestions[j].ID })
	for t := range s.tombs {
		d.Tombstones = append(d.Tombstones, t)
	}
	sort.Strings(d.Tombstones)
	return json.Marshal(d)
}

// saveColonyOnlyLocked is Store.change's save in colony-only mode: one
// colony transaction with every person that changed since sn, the people
// that left and the document. s.mu is held. On error nothing was written.
func (s *Store) saveColonyOnlyLocked(sn snapshot) error {
	c := s.colony
	doc, err := s.docLocked()
	if err != nil {
		return err
	}
	var changed []Person
	for id, p := range s.people {
		if old, ok := sn.people[id]; !ok || !reflect.DeepEqual(old, p) {
			changed = append(changed, p)
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].ID < changed[j].ID })
	var gone []string
	for id := range sn.people {
		if _, ok := s.people[id]; !ok {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	forgets := s.pendingForget
	docChanged := !bytes.Equal(doc, s.saved)
	if len(changed) == 0 && len(gone) == 0 && len(forgets) == 0 && !docChanged {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || c.dirty["*"] {
		return fmt.Errorf("customers: %s is not open; nothing was saved", ColonyFileName)
	}
	err = c.db.Batch(func(b *cstore.Batch) error {
		return colonyOnlyApply(b, changed, gone, forgets, s.forgottenLocked, docChanged, doc)
	})
	if err != nil {
		c.failures++
		return fmt.Errorf("customers: writing %s: %w", ColonyFileName, err)
	}
	s.saved = doc
	if len(forgets) > 0 {
		// The records are gone already; this scrubs the backups and the
		// freed pages, and is retried until it finishes.
		for id, hashes := range forgets {
			c.addPendingLocked(id, hashes)
		}
		c.retryPendingLocked()
		if err := c.writeMarkerLocked(); err != nil {
			c.logWarn("[customers] colony sync marker not written", "error", err)
		}
	}
	return nil
}

// colonyOnlyApply is the body of one colony-only save.
func colonyOnlyApply(b *cstore.Batch, changed []Person, gone []string, forgets map[string][]string,
	forgotten func(IdentityKind, string) bool, docChanged bool, doc []byte) error {
	for id := range forgets {
		if err := colonyForgetInBatch(b, id); err != nil {
			return err
		}
	}
	// The people who left go first, so an identity that moved to someone
	// in this change is free before it is written again.
	for _, id := range gone {
		if _, forgot := forgets[id]; forgot {
			continue
		}
		if err := colonyStrip(b, id); err != nil {
			return err
		}
	}
	var again []Person
	for _, p := range changed {
		got, err := colonyWritePerson(b, p, forgotten)
		if err != nil {
			return err
		}
		if len(got.Held) > 0 {
			again = append(again, p)
		}
	}
	// A person written before the one who released an identity holds it;
	// written again, they own it.
	for _, p := range again {
		if _, err := colonyWritePerson(b, p, forgotten); err != nil {
			return err
		}
	}
	if docChanged {
		return b.SetStoreAttachment(colonyAttachDoc, doc)
	}
	return nil
}

// saveColonyOnlyEnrichment is the enrichment store's save in colony-only
// mode: one colony transaction. On error nothing was written.
func (c *Colony) saveColonyOnlyEnrichment(changed map[string]PersonEnrichment, removed []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || c.dirty["*"] {
		return fmt.Errorf("customers: %s is not open; nothing was saved", ColonyFileName)
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := c.now()
	err := c.db.Batch(func(b *cstore.Batch) error {
		for _, id := range ids {
			if err := colonyWriteEnrichment(b, id, changed[id], true, now); err != nil {
				return err
			}
		}
		for _, id := range removed {
			if err := colonyWriteEnrichment(b, id, PersonEnrichment{}, false, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		c.failures++
		return fmt.Errorf("customers: writing enrichment to %s: %w", ColonyFileName, err)
	}
	return nil
}

// colonyOnlyContents is everything a colony-only colony.db holds for hive,
// read back: the people.json and enrichment.json it stands for.
type colonyOnlyContents struct {
	doc    colonyDoc
	hasDoc bool
	people []Person
	enrich map[string]PersonEnrichment
	// records counts colony records that are neither a hive person nor an
	// emptied merged-away record.
	records int
}

func readColonyOnly(db *cstore.Store) (colonyOnlyContents, error) {
	out := colonyOnlyContents{enrich: map[string]PersonEnrichment{}}
	raw, ok, err := db.StoreAttachment(colonyAttachDoc)
	if err != nil {
		return out, err
	}
	if ok {
		if err := json.Unmarshal(raw, &out.doc); err != nil {
			return out, fmt.Errorf("customers: decoding %s: %w", colonyAttachDoc, err)
		}
		if out.doc.Version > colonyOnlyVersion {
			return out, fmt.Errorf("customers: %s version %d is newer than this release", colonyAttachDoc, out.doc.Version)
		}
		out.hasDoc = true
	}
	err = db.List(func(cp cstore.Person) error {
		p, isHive, err := personFromColony(cp)
		if err != nil {
			return err
		}
		if !isHive {
			if cp.Status != colonyStatusRemoved {
				out.records++
			}
			return nil
		}
		out.people = append(out.people, p)
		pe, ok, err := enrichmentFromColonyPerson(cp)
		if err != nil {
			return err
		}
		if ok {
			out.enrich[p.ID] = pe
		}
		return nil
	})
	sort.Slice(out.people, func(i, j int) bool { return out.people[i].ID < out.people[j].ID })
	return out, err
}

// files renders the contents as people.json and enrichment.json bytes, in
// the shape Store.saveLocked and EnrichmentStore.saveLocked write.
func (cc colonyOnlyContents) files() ([]byte, []byte, error) {
	f := storeFile{Version: fileVersion, Salt: cc.doc.Salt, People: cc.people, Suggestions: cc.doc.Suggestions,
		Pins: cc.doc.Pins, Hints: cc.doc.Hints, Tombstones: cc.doc.Tombstones}
	people, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	enrich, err := json.MarshalIndent(enrichmentFile{People: cc.enrich}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return people, enrich, nil
}

// attachColonyOnly is AttachColony for colony-only.
func attachColonyOnly(s *Store, logger *slog.Logger) (*Colony, error) {
	dir := filepath.Dir(s.path)
	c := &Colony{mode: StoreColonyOnly, dir: dir, now: s.now, logger: logger, dirty: map[string]bool{}}
	if _, err := os.Stat(s.path); err == nil {
		migrated, err := colonyHasDoc(dir, s.now)
		if err != nil {
			return nil, err
		}
		if !migrated {
			return migrateColonyOnly(s, logger)
		}
		// colony.db was migrated already, so this people.json is not the
		// truth: an older binary that read colony-only as json started a
		// new one, or a stale copy was put back. Taking it as the truth
		// would forget everyone it lacks. It is set aside, never deleted,
		// and the store is read from colony.db.
		for _, name := range []string{FileName, EnrichmentFileName} {
			to, err := renameAside(filepath.Join(dir, name), colonyStraySuffix, s.now())
			if err != nil {
				return nil, err
			}
			if to != "" {
				c.logWarn("[customers] colony-only: a JSON file beside the migrated colony store was set aside, not read",
					"file", filepath.Base(to))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// A migration interrupted between its renames leaves enrichment.json
	// behind; it is renamed like the rest.
	if _, err := renameMigrated(filepath.Join(dir, EnrichmentFileName), s.now()); err != nil {
		return nil, err
	}
	db, err := cstore.Open(filepath.Join(dir, ColonyFileName), cstore.Options{Now: s.now})
	if err != nil {
		return nil, fmt.Errorf("customers: opening %s: %w", ColonyFileName, err)
	}
	c.db = db
	cc, err := readColonyOnly(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !cc.hasDoc && (len(cc.people) > 0 || cc.records > 0) {
		// A colony.db from another mode, and no people.json beside it:
		// nothing here says what hive held, so nothing is guessed.
		_ = db.Close()
		return nil, fmt.Errorf("customers: %s holds people but no %s document and %s is missing; restore %s or run colony export",
			ColonyFileName, colonyAttachDoc, FileName, FileName)
	}
	m, _ := c.readMarker()
	c.marker = m
	s.mu.Lock()
	s.loadColonyOnlyLocked(cc)
	s.colony = c
	s.mu.Unlock()
	c.mu.Lock()
	c.retryPendingLocked()
	c.mu.Unlock()
	c.logInfo("[customers] colony-only: people read from the colony store", "people", len(cc.people))
	return c, nil
}

// loadColonyOnlyLocked replaces s's contents with cc; s.mu is held.
func (s *Store) loadColonyOnlyLocked(cc colonyOnlyContents) {
	if cc.hasDoc && cc.doc.Salt != "" {
		s.salt = cc.doc.Salt
	}
	s.people = map[string]Person{}
	for _, p := range cc.people {
		s.people[p.ID] = p
	}
	s.sugg = map[string]Suggestion{}
	for _, sg := range cc.doc.Suggestions {
		if sg.ID != "" {
			s.sugg[IdentityKey(sg.Kind, sg.Value)] = sg
		}
	}
	s.pins, s.hints = cc.doc.Pins, cc.doc.Hints
	s.tombs = map[string]bool{}
	for _, t := range cc.doc.Tombstones {
		s.tombs[t] = true
	}
	s.reindexLocked()
	s.saved = nil
	if cc.hasDoc {
		s.saved, _ = s.docLocked()
	}
}

// colonyStraySuffix names a JSON file set aside beside a migrated store.
const colonyStraySuffix = ".stray"

// colonyHasDoc reports whether dir's colony.db holds the colony-only
// document, without keeping it open. A missing colony.db holds none.
func colonyHasDoc(dir string, now func() time.Time) (bool, error) {
	path := filepath.Join(dir, ColonyFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	db, err := cstore.Open(path, cstore.Options{Now: now})
	if err != nil {
		return false, fmt.Errorf("customers: opening %s: %w", ColonyFileName, err)
	}
	_, ok, err := db.StoreAttachment(colonyAttachDoc)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return ok, err
}

// migrateColonyOnly is the first colony-only boot: the colony-mode boot,
// then a full write, a read-back compare and the rename. When the compare
// fails the hive runs in colony mode with the JSON untouched.
func migrateColonyOnly(s *Store, logger *slog.Logger) (*Colony, error) {
	c, err := AttachColony(s, StoreColony, logger)
	if err != nil || c == nil {
		return c, err
	}
	enrichPath := filepath.Join(c.dir, EnrichmentFileName)
	_, enrichData, err := fileHash(enrichPath)
	if err != nil {
		c.logWarn("[customers] colony-only migration not run: enrichment.json unreadable; running in colony mode, the JSON kept",
			"error", err)
		return c, nil
	}
	enrich := parseEnrichment(enrichData)
	if err := c.migrate(s, enrich); err != nil {
		// Nothing the failed attempt wrote may later be taken for a
		// migrated store.
		c.mu.Lock()
		if cerr := c.clearColonyOnlyLocked(); cerr != nil {
			c.logWarn("[customers] colony-only leftovers not cleared; the next colony or shadow boot drops them", "error", cerr)
		}
		c.mu.Unlock()
		c.logWarn("[customers] colony-only migration not verified; running in colony mode, people.json and enrichment.json kept",
			"error", err)
		return c, nil
	}
	return c, nil
}

// migrate writes s and enrich into colony.db, verifies the read-back and
// renames the JSON files. On error colony.db may hold the extra attachments,
// which no other mode reads, and the JSON is as it was.
func (c *Colony) migrate(s *Store, enrich map[string]PersonEnrichment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	people := make([]Person, 0, len(s.people))
	for _, p := range s.people {
		people = append(people, clonePerson(p))
	}
	sort.Slice(people, func(i, j int) bool { return people[i].ID < people[j].ID })
	doc, err := s.docLocked()
	if err != nil {
		return err
	}
	want := colonyOnlyContents{hasDoc: true, people: people}
	if err := json.Unmarshal(doc, &want.doc); err != nil {
		return err
	}
	inPeople := map[string]bool{}
	for _, p := range people {
		inPeople[p.ID] = true
	}
	wantEnrich := map[string]PersonEnrichment{}
	orphans := 0
	for id, pe := range enrich {
		if inPeople[id] {
			wantEnrich[id] = pe
		} else {
			orphans++
		}
	}
	want.enrich = wantEnrich
	wantPeople, wantEnrichData, err := want.files()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return errors.New("customers: colony store closed")
	}
	now := c.now()
	err = c.db.Batch(func(b *cstore.Batch) error {
		for _, p := range people {
			if _, err := colonyWritePerson(b, p, s.forgottenLocked); err != nil {
				return fmt.Errorf("person: %w", err)
			}
		}
		for _, p := range people {
			pe, held := wantEnrich[p.ID]
			if err := colonyWriteEnrichment(b, p.ID, pe, held, now); err != nil {
				return fmt.Errorf("enrichment: %w", err)
			}
		}
		return b.SetStoreAttachment(colonyAttachDoc, doc)
	})
	if err != nil {
		return err
	}
	cc, err := readColonyOnly(c.db)
	if err != nil {
		return err
	}
	gotPeople, gotEnrich, err := cc.files()
	if err != nil {
		return err
	}
	if !bytes.Equal(gotPeople, wantPeople) {
		return fmt.Errorf("customers: read-back of %d people does not match %s", len(people), FileName)
	}
	if !bytes.Equal(gotEnrich, wantEnrichData) {
		return fmt.Errorf("customers: read-back of %d enrichment entries does not match %s", len(wantEnrich), EnrichmentFileName)
	}
	// enrichment.json goes first: a people.json left by a failure after it
	// means the next boot simply migrates again.
	var renamed []string
	enrichTo, err := renameMigrated(filepath.Join(c.dir, EnrichmentFileName), now)
	if err != nil {
		return err
	}
	peopleTo, err := renameMigrated(s.path, now)
	if err != nil {
		if enrichTo != "" {
			if rerr := os.Rename(enrichTo, filepath.Join(c.dir, EnrichmentFileName)); rerr != nil {
				c.logWarn("[customers] enrichment.json could not be put back after a failed rename", "file", filepath.Base(enrichTo))
			}
		}
		return err
	}
	for _, to := range []string{peopleTo, enrichTo} {
		if to != "" {
			renamed = append(renamed, filepath.Base(to))
		}
	}
	c.mode = StoreColonyOnly
	s.saved = doc
	c.marker.Migrated, c.marker.MigratedAt = renamed, now.UTC()
	if err := c.writeMarkerLocked(); err != nil {
		c.logWarn("[customers] colony sync marker not written", "error", err)
	}
	c.logInfo("[customers] colony-only: migrated; the JSON files were renamed and are no longer written",
		"people", len(people), "enrichment", len(wantEnrich), "enrichment_orphans_kept_in_migrated_file", orphans,
		"renamed", len(renamed))
	return nil
}

// renameMigrated renames path to path.migrated, or to
// path.migrated-<time> when that name is taken, and returns the new path.
// A missing path is not an error. Nothing is ever overwritten.
func renameMigrated(path string, now time.Time) (string, error) {
	return renameAside(path, MigratedSuffix, now)
}

// renameAside renames path to path+suffix, or path+suffix-<time> when that
// is taken. A missing path is not an error; nothing is ever overwritten.
func renameAside(path, suffix string, now time.Time) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	to := path + suffix
	if _, err := os.Stat(to); err == nil {
		to = path + suffix + "-" + now.UTC().Format(colonyMigratedStamp)
		if _, err := os.Stat(to); err == nil {
			return "", fmt.Errorf("customers: %s already exists", filepath.Base(to))
		}
	}
	if err := os.Rename(path, to); err != nil {
		return "", err
	}
	return to, nil
}

// dropColonyOnlyDocLocked removes the hive.people document a colony-only run
// left behind, in a mode where people.json is the truth: from then on it is
// stale, and must never be restored over a newer people.json.
func (c *Colony) dropColonyOnlyDocLocked() error {
	if c.db == nil {
		return nil
	}
	if _, ok, err := c.db.StoreAttachment(colonyAttachDoc); err != nil || !ok {
		return err
	}
	if err := c.db.SetStoreAttachment(colonyAttachDoc, nil); err != nil {
		return err
	}
	c.logInfo("[customers] colony store: dropped the colony-only document; people.json is the store again")
	return nil
}

// clearColonyOnlyLocked removes the colony-only document and every hive
// attachment: what a migration that did not verify left behind, so it can
// never be read as a migrated store. c.mu is held.
func (c *Colony) clearColonyOnlyLocked() error {
	if c.db == nil {
		return nil
	}
	var ids []string
	if err := c.db.List(func(cp cstore.Person) error {
		if len(cp.Attachments) > 0 {
			ids = append(ids, cp.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	return c.db.Batch(func(b *cstore.Batch) error {
		for _, id := range ids {
			for _, ns := range []string{colonyAttachPerson, colonyAttachEnrichment} {
				if err := b.SetAttachment(id, ns, nil); err != nil {
					return err
				}
			}
		}
		return b.SetStoreAttachment(colonyAttachDoc, nil)
	})
}

// loadColonyOnly fills the enrichment store from colony-only colony.db.
func (e *EnrichmentStore) loadColonyOnlyLocked(c *Colony) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return errors.New("customers: colony store closed")
	}
	cc, err := readColonyOnly(c.db)
	if err != nil {
		return err
	}
	e.people = cc.enrich
	data, err := json.MarshalIndent(enrichmentFile{People: e.people}, "", "  ")
	if err != nil {
		return err
	}
	e.saved = data
	return nil
}

// ExportColony writes people.json and enrichment.json, in the shape the json
// mode reads, from the colony-only colony.db in dir into outDir (dir when
// empty). It is the manual rollback (hive colony export --format
// hive-people-json). An existing file is never overwritten unless force is
// set. The hive must be stopped: a running hive holds colony.db's lock.
func ExportColony(dir, outDir string, force bool) (ColonyExportReport, error) {
	var rep ColonyExportReport
	if outDir == "" {
		outDir = dir
	}
	path := filepath.Join(dir, ColonyFileName)
	if _, err := os.Stat(path); err != nil {
		return rep, fmt.Errorf("customers: %s: %w", ColonyFileName, err)
	}
	colonyFileMu.Lock()
	defer colonyFileMu.Unlock()
	db, err := cstore.Open(path, cstore.Options{LockTimeout: colonyExportLockTimeout})
	if err != nil {
		return rep, fmt.Errorf("customers: opening %s (stop the hive first): %w", ColonyFileName, err)
	}
	defer func() { _ = db.Close() }()
	cc, err := readColonyOnly(db)
	if err != nil {
		return rep, err
	}
	if !cc.hasDoc {
		return rep, fmt.Errorf("customers: %s holds no %s document: %w", ColonyFileName, colonyAttachDoc, errNoColonyOnlyDoc)
	}
	people, enrich, err := cc.files()
	if err != nil {
		return rep, err
	}
	if err := os.MkdirAll(outDir, storeDirMode); err != nil {
		return rep, err
	}
	rep.PeopleFile = filepath.Join(outDir, FileName)
	rep.EnrichFile = filepath.Join(outDir, EnrichmentFileName)
	if !force {
		for _, f := range []string{rep.PeopleFile, rep.EnrichFile} {
			if _, err := os.Stat(f); err == nil {
				return rep, fmt.Errorf("customers: %s exists; move it aside or pass --force", filepath.Base(f))
			}
		}
	}
	if err := writeFileAtomic(rep.EnrichFile, ".enrichment-*", enrich); err != nil {
		return rep, err
	}
	if err := writeFileAtomic(rep.PeopleFile, ".people-*", people); err != nil {
		return rep, err
	}
	rep.People, rep.Enrichment = len(cc.people), len(cc.enrich)
	return rep, nil
}

// RestoreFromColony is the automatic rollback from colony-only: when dir has
// no people.json and its colony.db was written in colony-only mode, it
// writes people.json and enrichment.json back from colony.db, so a hive set
// back to colony, shadow or json finds its data. It does nothing otherwise
// and reports whether it restored. Call it before Open, in every mode but
// colony-only.
func RestoreFromColony(dir string, logger *slog.Logger) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(dir, ColonyFileName)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	rep, err := ExportColony(dir, dir, false)
	if errors.Is(err, errNoColonyOnlyDoc) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// people.json is the truth again: the document must never be restored
	// a second time over a newer people.json.
	if err := dropColonyOnlyDoc(dir); err != nil {
		return true, err
	}
	if logger != nil {
		logger.Info("[customers] restored people.json and enrichment.json from the colony-only store",
			"people", rep.People, "enrichment", rep.Enrichment)
	}
	return true, nil
}

// dropColonyOnlyDoc removes the colony-only document from dir's colony.db.
func dropColonyOnlyDoc(dir string) error {
	colonyFileMu.Lock()
	defer colonyFileMu.Unlock()
	db, err := cstore.Open(filepath.Join(dir, ColonyFileName), cstore.Options{LockTimeout: colonyExportLockTimeout})
	if err != nil {
		return err
	}
	err = db.SetStoreAttachment(colonyAttachDoc, nil)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return err
}

// errNoColonyOnlyDoc is wrapped by ExportColony when colony.db was not
// written in colony-only mode.
var errNoColonyOnlyDoc = errors.New("not a colony-only store")
