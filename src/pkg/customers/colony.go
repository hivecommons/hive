package customers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/colony/resolver"
	cstore "github.com/hivecommons/colony/store"
	bolt "go.etcd.io/bbolt"
)

// The colony store (#19855, design: src/docs/design/colony-customer-data-backend.md).
//
// Person records (identities, profile fields with their provenance,
// dismissed affiliations, status) and enrichment results move into the
// embedded hivecommons/colony store, /data/customers/colony.db, beside
// people.json. Tags, goals, satisfaction, suggestions, pins, hints, merge
// records and the edge index stay in hive, keyed by the same person ids.
//
// customers.store picks the mode (config.CustomersStoreMode). Unset, a spoke
// runs shadow until parity is confirmed on the project's own hive, then
// colony; the hub's default is already colony (#19972):
//
//   - json: people.json and enrichment.json only. colony.db is never
//     created; an existing one is opened only to forget a person. It is the
//     rollback, and what a hive falls back to when colony.db cannot be used.
//   - shadow: the first boot imports both JSON files into colony.db in one
//     transaction, records the source hash and keeps .pre-colony copies.
//     Every change is written to the JSON and then to colony.db. Reads come
//     from the JSON. A parity check logs counts only.
//   - colony: as shadow, but colony.db is authoritative for what it holds:
//     on boot its records replace the JSON copy, and enrichment is read from
//     it. The JSON is still written, so switching back to json loses
//     nothing. It becomes the spoke default once parity is confirmed on the
//     project's own hive (#19972); the hub default is already colony.
//   - colony-only (opt-in, never the default; colony_only.go): colony.db is
//     the only store. The first colony-only boot writes everything into it,
//     reads it all back and compares, and only then renames people.json and
//     enrichment.json to *.migrated. Nothing writes the JSON after that;
//     ExportColony writes it back out for a rollback.
//
// The JSON is always written first. A colony write that fails leaves the
// sync marker stale, so the next boot imports again from the JSON, which is
// never older than colony.db.

// StoreMode is the customers.store mode.
type StoreMode string

// Store modes; the strings match config.CustomersStore*.
const (
	StoreJSON   StoreMode = "json"
	StoreShadow StoreMode = "shadow"
	StoreColony StoreMode = "colony"
	// StoreColonyOnly stops the JSON writes (colony_only.go, #19972).
	StoreColonyOnly StoreMode = "colony-only"
)

const (
	// ColonyFileName is the colony store file inside the customers data dir.
	ColonyFileName = cstore.DefaultFileName
	// PreColonySuffix names the copies of people.json and enrichment.json
	// taken before the first import. Forget removes the person from them.
	PreColonySuffix = ".pre-colony"
	// colonySyncFileName records the hashes of the JSON files colony.db was
	// last brought in line with.
	colonySyncFileName = "colony-sync.json"
	// colonyMigrationName names the import in colony's meta bucket.
	colonyMigrationName = "hive-people-json"
	// colonyStatusRemoved marks a colony record whose person left the JSON
	// without being forgotten (merged into someone else): it keeps the id,
	// so an unmerge can bring it back, and holds no identity or fact.
	colonyStatusRemoved = "removed"
	// Fact sources for values that did not come from a provider or the hub.
	colonySourceManual = "hive:manual"
	colonySourceAuto   = "hive:auto"
	colonySourceGitdm  = cstore.ColonySourcePrefix + EnrichSourceGitdm
	colonySourceDevst  = cstore.ColonySourcePrefix + EnrichSourceDevstats
	// colonyHubUnlabeled is the hub label of a hub value saved before the
	// hub sent labels.
	colonyHubUnlabeled = "unlabeled"
	// colonyAcceptedByPolicy records the auto-accept policy as the acceptor.
	colonyAcceptedByPolicy = "auto_accept:gitdm_high"
	// Identity confidence when an identity comes back from colony, which
	// records Verified rather than a number: a verified identity is high, a
	// claim low.
	colonyConfidenceHigh = 0.9
	colonyConfidenceLow  = 0.3
	// colonyMaxFieldBytes is colony's limit on a fact field name.
	colonyMaxFieldBytes = 64
	// colonyMaxSourceBytes is colony's limit on a source label.
	colonyMaxSourceBytes = 128
	// colonyMaxByBytes is colony's limit on who linked an identity.
	colonyMaxByBytes = 256
	// colonyMaxScore is the top of colony's identity score range.
	colonyMaxScore = 1.0
	// colonyCompactTxBytes bounds one compaction transaction.
	colonyCompactTxBytes = 64 << 20
)

// colonyOwnedProfileFields are the fact fields the people mirror owns.
var colonyOwnedProfileFields = ProfileFields

// Colony mirrors the customers stores into colony.db. A nil *Colony is the
// json mode: every method is a no-op.
type Colony struct {
	mode   StoreMode
	dir    string
	now    func() time.Time
	logger *slog.Logger

	mu sync.Mutex
	db *cstore.Store
	// marker is what colony.db was last brought in line with.
	marker colonySyncMarker
	// dirty holds the person ids whose last colony write failed. While it
	// is non-empty the marker is not advanced, so the next boot imports
	// again; a dirty person's enrichment is read from the JSON.
	dirty map[string]bool
	// failures counts failed colony writes since boot.
	failures int
}

type colonySyncMarker struct {
	People     string    `json:"people_sha256"`
	Enrichment string    `json:"enrichment_sha256"`
	At         time.Time `json:"at"`
	// PendingForget holds the person ids whose forget has not reached
	// colony.db or the backups yet, with the tombstone hashes hive wrote
	// for them. Ids are random and hold no personal data. Every boot and
	// every later forget retries them.
	PendingForget map[string][]string `json:"pending_forget,omitempty"`
	// Migrated names the JSON files the colony-only migration renamed, and
	// MigratedAt when (#19972).
	Migrated   []string  `json:"migrated,omitempty"`
	MigratedAt time.Time `json:"migrated_at,omitzero"`
}

// colonyFileMu serialises every forget, compaction and offline open of a
// colony.db in this process, so two forgets never compact over each other.
var colonyFileMu sync.Mutex

// ColonyParity is the parity check: counts only, never values.
type ColonyParity struct {
	Mode           StoreMode `json:"mode"`
	JSONPeople     int       `json:"json_people"`
	ColonyPeople   int       `json:"colony_people"`
	JSONIdentities int       `json:"json_identities"`
	ColonyIdent    int       `json:"colony_identities"`
	// JSONHeld counts the identities colony gives to another person (one
	// owner per identity), which it keeps as held identities; ColonyHeld
	// counts what it holds.
	JSONHeld        int `json:"json_held"`
	ColonyHeld      int `json:"colony_held"`
	FactsUser       int `json:"facts_user"`
	FactsHubShared  int `json:"facts_hub_shared"`
	FactsEnrichment int `json:"facts_enrichment"`
	Mismatches      int `json:"mismatches"`
	Failures        int `json:"write_failures"`
}

// ParseStoreMode reads a mode; anything unknown is json.
func ParseStoreMode(raw string) StoreMode {
	switch m := StoreMode(strings.ToLower(strings.TrimSpace(raw))); m {
	case StoreShadow, StoreColony, StoreColonyOnly:
		return m
	}
	return StoreJSON
}

// Mode is the colony mode; json for a nil Colony.
func (c *Colony) Mode() StoreMode {
	if c == nil {
		return StoreJSON
	}
	return c.mode
}

// Close releases colony.db.
func (c *Colony) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}

func (c *Colony) logWarn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

func (c *Colony) logInfo(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

// AttachColony opens colony.db beside people.json for mode and brings it in
// line: the first boot imports, a boot after the JSON changed without it
// imports again, and in colony mode colony.db's records replace the JSON
// copy. It fails closed: any error leaves the store in json mode and is
// returned for the log, and colony.db is never rewritten by an older schema.
// The store must not be in use yet.
func AttachColony(s *Store, mode StoreMode, logger *slog.Logger) (*Colony, error) {
	if s == nil || mode == StoreJSON {
		return nil, nil
	}
	if mode == StoreColonyOnly {
		return attachColonyOnly(s, logger)
	}
	dir := filepath.Dir(s.path)
	c := &Colony{mode: mode, dir: dir, now: s.now, logger: logger, dirty: map[string]bool{}}
	db, err := cstore.Open(filepath.Join(dir, ColonyFileName), cstore.Options{Now: s.now})
	if err != nil {
		return nil, fmt.Errorf("customers: opening %s: %w", ColonyFileName, err)
	}
	c.db = db
	if err := c.boot(s); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := c.bootRead(s); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.mu.Lock()
	s.colony = c
	s.mu.Unlock()
	return c, nil
}

// fileHash is the hex SHA-256 of a file, "" when it does not exist.
func fileHash(path string) (string, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

func bytesHash(data []byte) string {
	if data == nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Colony) markerPath() string { return filepath.Join(c.dir, colonySyncFileName) }

func (c *Colony) readMarker() (colonySyncMarker, bool) {
	data, err := os.ReadFile(c.markerPath())
	if err != nil {
		return colonySyncMarker{}, false
	}
	var m colonySyncMarker
	if json.Unmarshal(data, &m) != nil {
		return colonySyncMarker{}, false
	}
	return m, true
}

func (c *Colony) writeMarkerLocked() error {
	c.marker.At = c.now().UTC()
	data, err := json.MarshalIndent(c.marker, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.markerPath(), ".colony-sync-*", data)
}

// boot runs the import or the catch-up, then the colony-mode read.
func (c *Colony) boot(s *Store) error {
	peopleHash, peopleData, err := fileHash(s.path)
	if err != nil {
		return err
	}
	enrichPath := filepath.Join(c.dir, EnrichmentFileName)
	enrichHash, enrichData, err := fileHash(enrichPath)
	if err != nil {
		return err
	}
	enrich := parseEnrichment(enrichData)
	s.mu.Lock()
	people := make([]Person, 0, len(s.people))
	for _, p := range s.people {
		people = append(people, clonePerson(p))
	}
	// The tombstone check is taken under s.mu now, so c.mu is never held
	// while s.mu is taken (Store.change takes s.mu, then c.mu).
	forgotten := s.forgottenSnapshotLocked()
	s.mu.Unlock()
	sort.Slice(people, func(i, j int) bool { return people[i].ID < people[j].ID })

	_, ran, err := c.db.Migration(colonyMigrationName)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, hadMarker := c.readMarker()
	c.marker = colonySyncMarker{PendingForget: prev.PendingForget, Migrated: prev.Migrated, MigratedAt: prev.MigratedAt}
	if !ran {
		if err := c.writeBackups(peopleData, enrichData); err != nil {
			return err
		}
		src := &colonyJSONSource{people: people, enrich: enrich, now: c.now(),
			fingerprint: peopleHash + ":" + enrichHash, forgotten: forgotten}
		res, err := c.db.Migrate(context.Background(), colonyMigrationName, src, cstore.MigrateOptions{})
		if err != nil {
			return fmt.Errorf("customers: importing into %s: %w", ColonyFileName, err)
		}
		c.logInfo("[customers] imported people.json into the colony store", "people", res.Record.Report.People,
			"identities", res.Record.Report.Identities, "facts", res.Record.Report.Facts,
			"skipped_identities", res.Record.Report.SkippedIdentities, "skipped_facts", res.Record.Report.SkippedFacts)
	} else if !hadMarker || prev.People != peopleHash || prev.Enrichment != enrichHash {
		// The JSON changed while colony.db was not written (json mode, or
		// a failed colony write): the JSON is newer, import it again.
		if err := c.resyncLocked(people, enrich, forgotten); err != nil {
			return err
		}
		c.logInfo("[customers] colony store caught up from people.json", "people", len(people))
	}
	// people.json is the truth in this mode: a document an earlier
	// colony-only run left behind is stale from here on.
	if err := c.dropColonyOnlyDocLocked(); err != nil {
		return err
	}
	// Forgets that did not finish (a failed colony write, or a json-mode
	// forget that could not open colony.db) run now, and anyone colony.db or
	// the backups still hold who is neither in people.json nor merged into
	// someone there is forgotten as well.
	if err := c.forgetOrphansLocked(keptPersonIDs(people)); err != nil {
		return err
	}
	c.retryPendingLocked()
	if c.db == nil {
		return fmt.Errorf("customers: %s did not reopen after compaction", ColonyFileName)
	}
	if err := c.verifyCountLocked(len(people)); err != nil {
		// No new marker is written, so every later boot catches up again
		// and fails closed until the counts agree.
		return err
	}
	c.marker.People, c.marker.Enrichment = peopleHash, enrichHash
	return c.writeMarkerLocked()
}

// verifyCountLocked checks that colony holds as many live people as the
// JSON.
func (c *Colony) verifyCountLocked(want int) error {
	n := 0
	err := c.db.List(func(cp cstore.Person) error {
		if cp.Status != colonyStatusRemoved {
			n++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("customers: %s holds %d people, people.json holds %d", ColonyFileName, n, want)
	}
	return nil
}

// keptPersonIDs is every id a record may still be held for: the people in
// the JSON and everyone merged into them, at any depth (an unmerge brings
// them back).
func keptPersonIDs(people []Person) map[string]bool {
	keep := map[string]bool{}
	var walk func(p Person)
	walk = func(p Person) {
		keep[p.ID] = true
		for _, m := range p.MergedFrom {
			walk(m.Person)
		}
	}
	for _, p := range people {
		walk(p)
	}
	return keep
}

// forgetOrphansLocked forgets every colony record, and removes from the
// backups every person, that keep does not name: they were forgotten while
// colony.db could not be written.
func (c *Colony) forgetOrphansLocked(keep map[string]bool) error {
	var orphans []string
	err := c.db.List(func(cp cstore.Person) error {
		if !keep[cp.ID] {
			orphans = append(orphans, cp.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range backupPersonIDs(c.dir) {
		if !keep[id] {
			orphans = append(orphans, id)
		}
	}
	for _, id := range orphans {
		if _, pending := c.marker.PendingForget[id]; !pending {
			c.addPendingLocked(id, nil)
		}
	}
	return nil
}

// backupPersonIDs lists the people the .pre-colony backups hold.
func backupPersonIDs(dir string) []string {
	var out []string
	if data, err := os.ReadFile(filepath.Join(dir, FileName+PreColonySuffix)); err == nil {
		var f storeFile
		if json.Unmarshal(data, &f) == nil {
			for _, p := range f.People {
				out = append(out, p.ID)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, EnrichmentFileName+PreColonySuffix)); err == nil {
		var f enrichmentFile
		if json.Unmarshal(data, &f) == nil {
			for id := range f.People {
				out = append(out, id)
			}
		}
	}
	return out
}

func (c *Colony) addPendingLocked(id string, tombs []string) {
	if c.marker.PendingForget == nil {
		c.marker.PendingForget = map[string][]string{}
	}
	c.marker.PendingForget[id] = append(c.marker.PendingForget[id], tombs...)
}

// retryPendingLocked runs every pending forget and drops those that
// finished.
func (c *Colony) retryPendingLocked() {
	if len(c.marker.PendingForget) == 0 {
		return
	}
	ids := make([]string, 0, len(c.marker.PendingForget))
	for id := range c.marker.PendingForget {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	erased := false
	for _, id := range ids {
		ok := c.db != nil
		if ok {
			if err := colonyForgetOne(c.db, id); err != nil {
				ok = false
				c.failures++
				c.logWarn("[customers] colony store forget failed; it is retried", "error", err)
			} else {
				erased = true
			}
		}
		if err := forgetInBackups(c.dir, id, c.marker.PendingForget[id]); err != nil {
			ok = false
			c.logWarn("[customers] forget in the pre-colony backups failed; it is retried", "error", err)
		}
		if ok {
			delete(c.marker.PendingForget, id)
		}
	}
	if erased && !c.compactLocked() {
		// The freed pages may still hold the bytes: compact again later.
		for _, id := range ids {
			if _, still := c.marker.PendingForget[id]; !still {
				c.addPendingLocked(id, nil)
			}
		}
	}
}

// colonyForgetOne erases one person from an open colony store. Colony
// tombstones every identity the record holds; hive does not tombstone a
// dashboard account or an unconfirmed claim (a dashboard user who signs in
// again gets a fresh profile), so those are detached first and only what
// hive tombstones is tombstoned in colony too.
func colonyForgetOne(db *cstore.Store, id string) error {
	cp, err := db.Get(id)
	if errors.Is(err, cstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, i := range cp.Identities {
		if i.Kind == cstore.KindOIDC || !i.Verified {
			if err := db.RemoveIdentity(id, i.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
				return err
			}
		}
	}
	if _, err := db.Forget(id); err != nil && !errors.Is(err, cstore.ErrNotFound) {
		return err
	}
	return nil
}

// bootRead is the colony-mode read at boot, after the import. When it
// changed people.json the marker records the new file, since both now agree.
func (c *Colony) bootRead(s *Store) error {
	if c.mode != StoreColony {
		return nil
	}
	if err := s.readFromColony(c); err != nil {
		return err
	}
	h, _, err := fileHash(s.path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if h == c.marker.People {
		return nil
	}
	c.marker.People = h
	return c.writeMarkerLocked()
}

func parseEnrichment(data []byte) map[string]PersonEnrichment {
	out := map[string]PersonEnrichment{}
	if len(data) == 0 {
		return out
	}
	var f enrichmentFile
	if json.Unmarshal(data, &f) != nil {
		return out
	}
	for id, pe := range f.People {
		if id != "" {
			out[id] = pe
		}
	}
	return out
}

func (c *Colony) backupPath(name string) string { return filepath.Join(c.dir, name+PreColonySuffix) }

// writeBackups keeps the JSON as it stood before the first import. An
// existing backup is never replaced, so a second import keeps the first.
func (c *Colony) writeBackups(peopleData, enrichData []byte) error {
	for name, data := range map[string][]byte{FileName: peopleData, EnrichmentFileName: enrichData} {
		if data == nil {
			continue
		}
		path := c.backupPath(name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := writeFileAtomic(path, ".pre-colony-*", data); err != nil {
			return err
		}
	}
	return nil
}

// colonyJSONSource is the import source: the people and enrichment the JSON
// holds, already mapped to colony records.
type colonyJSONSource struct {
	people      []Person
	enrich      map[string]PersonEnrichment
	now         time.Time
	fingerprint string
	forgotten   func(IdentityKind, string) bool
}

func (src *colonyJSONSource) Fingerprint() (string, error) { return src.fingerprint, nil }

func (src *colonyJSONSource) Records(ctx context.Context, fn func(cstore.Record) error) error {
	owners := colonyOwners(src.people)
	for _, p := range src.people {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec := colonyRecord(p, owners, src.forgotten)
		if pe, ok := src.enrich[p.ID]; ok {
			rec.Facts = append(rec.Facts, colonyEnrichmentFacts(pe, rec.Person.Identities, src.now, rec.Dismissed...)...)
			rec.Answers = colonyAnswers(pe)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// colonyKind maps a hive identity kind to colony's. A hive kind colony has
// no kind for (chat handles, meeting names, display names) stays in hive.
func colonyKind(k IdentityKind) (cstore.Kind, bool) {
	switch k {
	case KindGitHub:
		return cstore.KindGitHub, true
	case KindEmail:
		return cstore.KindEmail, true
	case KindGitEmail:
		return cstore.KindGitEmail, true
	case KindSlack:
		return cstore.KindSlack, true
	case KindHiveUser:
		// The dashboard account id: an OIDC subject or the consumer's
		// canonical account, always local_only in colony.
		return cstore.KindOIDC, true
	}
	return "", false
}

func hiveKind(k cstore.Kind) (IdentityKind, bool) {
	switch k {
	case cstore.KindGitHub:
		return KindGitHub, true
	case cstore.KindEmail:
		return KindEmail, true
	case cstore.KindGitEmail:
		return KindGitEmail, true
	case cstore.KindSlack:
		return KindSlack, true
	case cstore.KindOIDC:
		return KindHiveUser, true
	}
	return "", false
}

// colonyClassKey is the key colony matches an identity by: both email kinds
// share one namespace.
func colonyClassKey(k cstore.Kind, value string) string {
	cls := string(k)
	if k.IsEmail() {
		cls = string(cstore.KindEmail)
	}
	v := strings.TrimSpace(value)
	switch k {
	case cstore.KindGitHub, cstore.KindGHE:
		v = strings.ToLower(strings.TrimPrefix(v, "@"))
	case cstore.KindEmail, cstore.KindGitEmail:
		v = strings.ToLower(v)
	}
	return cls + "\x00" + v
}

// colonyOwners decides which person an identity belongs to in colony, where
// an identity has one owner and both email kinds are one namespace: a
// confirmed identity before a claim, then the lower person id.
func colonyOwners(people []Person) map[string]string {
	owners := map[string]string{}
	for _, claimed := range []bool{false, true} {
		for _, p := range people {
			for _, i := range p.Identities {
				k, ok := colonyKind(i.Kind)
				if !ok || i.Claimed != claimed {
					continue
				}
				key := colonyClassKey(k, NormalizeIdentity(i.Kind, i.Value))
				if _, taken := owners[key]; !taken {
					owners[key] = p.ID
				}
			}
		}
	}
	return owners
}

// colonyIdentities maps p's identities. Only identities owners gives to p
// are kept, and a forgotten one is dropped.
func colonyIdentities(p Person, owners map[string]string, forgotten func(IdentityKind, string) bool) []cstore.Identity {
	var out []cstore.Identity
	seen := map[string]bool{}
	for _, i := range p.Identities {
		k, ok := colonyKind(i.Kind)
		if !ok {
			continue
		}
		v := NormalizeIdentity(i.Kind, i.Value)
		key := colonyClassKey(k, v)
		if seen[key] || (owners != nil && owners[key] != p.ID) || (forgotten != nil && forgotten(i.Kind, v)) {
			continue
		}
		seen[key] = true
		loc := cstore.LocalityShareable
		if k.IsAlwaysLocal() {
			loc = cstore.LocalityLocalOnly
		}
		// colony forces every email on a domain that is not free-mail to
		// local_only whatever is asked.
		out = append(out, colonyIdentity(i, k, v, loc))
	}
	return out
}

// colonyIdentity maps one identity with its provenance: who linked it, by
// hand or not, and the numeric confidence as colony's score.
func colonyIdentity(i Identity, k cstore.Kind, v string, loc cstore.Locality) cstore.Identity {
	score := i.Confidence
	switch {
	case score < 0 || math.IsNaN(score):
		score = 0
	case score > colonyMaxScore:
		score = colonyMaxScore
	}
	return cstore.Identity{Kind: k, Value: v, Verified: !i.Claimed, Locality: loc,
		Source: truncateRunes(i.Source, colonyMaxSourceBytes), At: i.At,
		Manual: i.Manual, By: truncateRunes(i.By, colonyMaxByBytes), Score: score}
}

// colonyHeldIdentities maps the identities of p that owners gives to someone
// else: colony keeps them as held identities on p instead of dropping them.
func colonyHeldIdentities(p Person, owners map[string]string, forgotten func(IdentityKind, string) bool) []cstore.Identity {
	if owners == nil {
		return nil
	}
	var out []cstore.Identity
	seen := map[string]bool{}
	for _, i := range p.Identities {
		k, ok := colonyKind(i.Kind)
		if !ok {
			continue
		}
		v := NormalizeIdentity(i.Kind, i.Value)
		key := colonyClassKey(k, v)
		owner, owned := owners[key]
		if seen[key] || !owned || owner == p.ID || (forgotten != nil && forgotten(i.Kind, v)) {
			continue
		}
		seen[key] = true
		out = append(out, colonyIdentity(i, k, v, cstore.LocalityLocalOnly))
	}
	return out
}

// truncateRunes cuts s to at most n bytes on a rune boundary.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// colonyHubLabel is the colony source label of a hub Rule.
func colonyHubLabel(rule string) string {
	if rule == "" {
		return colonyHubUnlabeled
	}
	for label, r := range hubLabelRules {
		if r == rule {
			return label
		}
	}
	return colonyHubUnlabeled
}

func hubRuleFromSource(source string) string {
	label := strings.TrimPrefix(source, cstore.HubSourcePrefix)
	if label == colonyHubUnlabeled {
		return ""
	}
	if r, ok := hubLabelRules[label]; ok {
		return r
	}
	return ""
}

// githubRef is the confirmed GitHub login among ids, as a colony ref: the
// one named by prefer when the person holds it, else the first.
func githubRef(ids []cstore.Identity, prefer ...string) (cstore.IdentityRef, bool) {
	for _, want := range prefer {
		w := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(want), "@"))
		for _, i := range ids {
			if w != "" && i.Kind == cstore.KindGitHub && i.Verified && i.Value == w {
				return i.Ref(), true
			}
		}
	}
	for _, i := range ids {
		if i.Kind == cstore.KindGitHub && i.Verified {
			return i.Ref(), true
		}
	}
	return cstore.IdentityRef{}, false
}

func emailRefs(ids []cstore.Identity) []cstore.IdentityRef {
	var out []cstore.IdentityRef
	for _, i := range ids {
		if i.Kind.IsEmail() && i.Verified {
			out = append(out, i.Ref())
		}
	}
	return out
}

func fieldAt(f ProfileField, p Person) time.Time {
	if !f.At.IsZero() {
		return f.At
	}
	return p.UpdatedAt
}

// colonyProfileFacts maps p's profile fields: manual is rank user (an empty
// value is an explicit clear), hub is hub_shared with its label, auto is
// enrichment with its rule as the source. A value derived from the person's
// public GitHub profile records the login it came from; every other value
// is local_only.
func colonyProfileFacts(p Person, ids []cstore.Identity) []cstore.Fact {
	gh, hasGH := githubRef(ids)
	var out []cstore.Fact
	for _, name := range colonyOwnedProfileFields {
		f := *p.Profile.Field(name)
		at := fieldAt(f, p)
		switch f.Source {
		case SourceManual:
			out = append(out, cstore.Fact{Field: name, Value: f.Value, Rank: cstore.RankUser, Source: colonySourceManual,
				Locality: cstore.LocalityLocalOnly, At: at, By: f.By})
		case SourceHub:
			if f.Value == "" {
				continue
			}
			out = append(out, cstore.Fact{Field: name, Value: f.Value, Rank: cstore.RankHubShared,
				Source: cstore.HubSourcePrefix + colonyHubLabel(f.Rule), Locality: cstore.LocalityLocalOnly, At: at})
		case SourceAuto:
			if f.Value == "" {
				continue
			}
			fact := cstore.Fact{Field: name, Value: f.Value, Rank: cstore.RankEnrichment, Source: f.Rule,
				Locality: cstore.LocalityLocalOnly, At: at}
			if fact.Source == "" {
				fact.Source = colonySourceAuto
			}
			publicGitHub := f.Rule == ProfileSourceGitHub ||
				(name == FieldCountry && f.Rule == CountryRuleLocation && p.Profile.Location.Rule == ProfileSourceGitHub)
			if f.Rule == AffiliationAutoAcceptedGitdm {
				fact.Source = colonySourceGitdm
				fact.Confidence = resolver.ConfidenceHigh
				fact.AcceptedAt, fact.AcceptedBy = at, colonyAcceptedByPolicy
				publicGitHub = true
			}
			if publicGitHub && hasGH {
				ref := gh
				login := f.Login
				if name == FieldCountry {
					login = p.Profile.Location.Login
				}
				if r, ok := githubRef(ids, login); ok {
					ref = r
				}
				fact.Locality = cstore.LocalityShareable
				fact.DerivedFrom = []cstore.IdentityRef{ref}
			}
			out = append(out, fact)
		}
	}
	return out
}

// colonyDismissals maps the dismissed affiliations.
func colonyDismissals(p Person) []cstore.Dismissal {
	var out []cstore.Dismissal
	for _, d := range p.Profile.DismissedAffiliations {
		if d != "" {
			out = append(out, cstore.Dismissal{Field: cstore.FieldAffiliation, Value: d, At: p.UpdatedAt})
		}
	}
	return out
}

// colonyStatus is the colony status of a hive status; an empty one is
// colony's default, so the two compare equal.
func colonyStatus(st Status) string {
	if st == "" {
		return cstore.StatusActive
	}
	return string(st)
}

// colonyRecord is one person as colony holds it (enrichment facts aside).
func colonyRecord(p Person, owners map[string]string, forgotten func(IdentityKind, string) bool) cstore.Record {
	ids := colonyIdentities(p, owners, forgotten)
	return cstore.Record{
		Person: cstore.PersonSpec{ID: p.ID, Status: colonyStatus(p.Status), Identities: ids,
			Held: colonyHeldIdentities(p, owners, forgotten)},
		Facts:     colonyProfileFacts(p, ids),
		Dismissed: colonyDismissals(p),
		CreatedAt: p.CreatedAt,
	}
}

func parseEnrichedDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(enrichmentDateLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// colonyActivityField is the colony field of one project's activity, or ""
// when it does not fit colony's field limit.
func colonyActivityField(project string) string {
	key := strings.ReplaceAll(strings.ToLower(strings.Join(strings.Fields(project), " ")), " ", "_")
	f := cstore.FieldActivityPrefix + key
	if key == "" || len(f) > colonyMaxFieldBytes {
		return ""
	}
	return f
}

// colonyEnrichmentFacts maps one person's enrichment: the gitdm affiliation
// current at now becomes a suggested affiliation fact (colony holds one per
// rank), each devstats project a suggested activity count. A value records
// the identities it was matched by, so an email match stays local_only.
func colonyEnrichmentFacts(pe PersonEnrichment, ids []cstore.Identity, now time.Time, dismissed ...cstore.Dismissal) []cstore.Fact {
	var out []cstore.Fact
	isDismissed := func(v string) bool {
		for _, d := range dismissed {
			if d.Field == cstore.FieldAffiliation && strings.EqualFold(strings.TrimSpace(d.Value), strings.TrimSpace(v)) {
				return true
			}
		}
		return false
	}
	gh, hasGH := githubRef(ids)
	var gitdm []EnrichedAffiliation
	for _, a := range pe.Affiliations {
		if a.Source == EnrichSourceGitdm {
			gitdm = append(gitdm, a)
		}
	}
	if cur, ok := currentEnrichedAffiliation(gitdm, now); ok && !isDismissed(cur.Org) {
		f := cstore.Fact{Field: cstore.FieldAffiliation, Value: cur.Org, Rank: cstore.RankEnrichment, Source: colonySourceGitdm,
			Locality: cstore.LocalityShareable, From: parseEnrichedDate(cur.From), Until: parseEnrichedDate(cur.Until),
			At: pe.FetchedAt[EnrichSourceGitdm], Suggested: true}
		switch cur.Confidence {
		case EnrichConfidenceHigh, EnrichConfidenceMedium, EnrichConfidenceLow:
			f.Confidence = resolver.Confidence(cur.Confidence)
		}
		if cur.MatchedBy != "email" && hasGH {
			f.DerivedFrom = append(f.DerivedFrom, gh)
		}
		if cur.MatchedBy != EnrichMatchedByLogin {
			f.DerivedFrom = append(f.DerivedFrom, emailRefs(ids)...)
		}
		if len(f.DerivedFrom) == 0 {
			f.Locality = cstore.LocalityLocalOnly
		}
		out = append(out, f)
	}
	if hasGH {
		seen := map[string]bool{}
		for _, a := range pe.Activity {
			field := colonyActivityField(a.Project)
			if a.Source != EnrichSourceDevstats || field == "" || seen[field] {
				continue
			}
			seen[field] = true
			out = append(out, cstore.Fact{Field: field, Value: strconv.Itoa(a.Contributions), Rank: cstore.RankEnrichment,
				Source: colonySourceDevst, Locality: cstore.LocalityShareable, At: pe.FetchedAt[EnrichSourceDevstats],
				Suggested: true, DerivedFrom: []cstore.IdentityRef{gh}})
		}
	}
	return out
}

// isEnrichmentField reports whether the enrichment mirror owns a field.
func isEnrichmentField(field string) bool {
	return field == cstore.FieldAffiliation || strings.HasPrefix(field, cstore.FieldActivityPrefix)
}

func isProfileField(field string) bool {
	for _, f := range colonyOwnedProfileFields {
		if f == field {
			return true
		}
	}
	return false
}

// syncPeopleLocked brings colony.db in line with the given people (their
// current state) and the ids that left people.json. Identities and facts
// are removed first, so an identity that moved between two people in one
// change is free before it is added again. It returns the ids it failed.
func (c *Colony) syncPeopleLocked(changed []Person, gone []string, forgotten func(IdentityKind, string) bool) []string {
	if c.db == nil {
		return append(personIDs(changed), gone...)
	}
	var failed []string
	for _, id := range gone {
		if err := c.stripLocked(id); err != nil && !errors.Is(err, cstore.ErrNotFound) {
			failed = append(failed, id)
		}
	}
	desired := make(map[string]cstore.Record, len(changed))
	for _, p := range changed {
		rec := colonyRecord(p, nil, forgotten)
		desired[p.ID] = rec
		cp, err := c.db.Get(p.ID)
		if errors.Is(err, cstore.ErrNotFound) {
			continue
		}
		if err != nil {
			failed = append(failed, p.ID)
			continue
		}
		if err := c.pruneLocked(cp, rec, isProfileField, true); err != nil {
			failed = append(failed, p.ID)
		}
	}
	for _, p := range changed {
		rec := desired[p.ID]
		// An identity colony gives to someone else is kept on this person
		// as a held identity (colony v0.4.0), so nothing is dropped.
		kept := rec.Person.Identities[:0:0]
		for _, i := range rec.Person.Identities {
			owner, err := c.db.FindByIdentity(i.Kind, i.Value)
			if err == nil && owner.ID != p.ID {
				i.Locality = cstore.LocalityLocalOnly
				rec.Person.Held = append(rec.Person.Held, i)
				continue
			}
			kept = append(kept, i)
		}
		rec.Person.Identities = kept
		if err := c.importLocked(rec); err != nil {
			failed = append(failed, p.ID)
			continue
		}
		// A gitdm suggestion dismissed here leaves colony too.
		if err := c.dropDismissedLocked(p.ID, rec.Dismissed); err != nil {
			failed = append(failed, p.ID)
		}
	}
	return failed
}

// dropDismissedLocked deletes the enrichment affiliation of a person when
// its value was dismissed.
func (c *Colony) dropDismissedLocked(id string, dismissed []cstore.Dismissal) error {
	if len(dismissed) == 0 {
		return nil
	}
	cp, err := c.db.Get(id)
	if err != nil {
		return err
	}
	f, ok := cp.FactAt(cstore.FieldAffiliation, cstore.RankEnrichment)
	if !ok {
		return nil
	}
	for _, d := range dismissed {
		if strings.EqualFold(strings.TrimSpace(d.Value), strings.TrimSpace(f.Value)) {
			_, err := c.db.DeleteFact(id, cstore.FieldAffiliation, cstore.RankEnrichment)
			return err
		}
	}
	return nil
}

func personIDs(ps []Person) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// importLocked merges one record through colony's precedence rules.
func (c *Colony) importLocked(rec cstore.Record) error {
	var held []cstore.HeldIdentity
	for _, i := range rec.Person.Held {
		held = append(held, cstore.HeldIdentity{Identity: i})
	}
	p := cstore.Person{ID: rec.Person.ID, Status: rec.Person.Status, Identities: rec.Person.Identities,
		Facts: rec.Facts, Dismissed: rec.Dismissed, CreatedAt: rec.CreatedAt, Held: held, Answers: rec.Answers}
	_, err := c.db.Import(context.Background(), cstore.Snapshot{SchemaVersion: cstore.SchemaVersion, People: []cstore.Person{p}})
	return err
}

// pruneLocked removes from cp what the desired record no longer holds: the
// facts in the fields owns says this mirror owns, and with identities set
// the identities.
func (c *Colony) pruneLocked(cp cstore.Person, rec cstore.Record, owns func(string) bool, identities bool) error {
	if identities {
		want := map[string]bool{}
		for _, i := range rec.Person.Identities {
			want[colonyClassKey(i.Kind, i.Value)] = true
		}
		for _, i := range cp.Identities {
			if want[colonyClassKey(i.Kind, i.Value)] {
				continue
			}
			if err := c.db.RemoveIdentity(cp.ID, i.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
				return err
			}
		}
		wantHeld := map[string]bool{}
		for _, i := range rec.Person.Held {
			wantHeld[colonyClassKey(i.Kind, i.Value)] = true
		}
		for _, h := range cp.Held {
			if wantHeld[colonyClassKey(h.Kind, h.Value)] {
				continue
			}
			if _, err := c.db.RemoveHeld(cp.ID, h.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
				return err
			}
		}
	}
	wantFact := map[string]bool{}
	for _, f := range rec.Facts {
		wantFact[f.Field+"\x00"+string(f.Rank)] = true
	}
	for _, f := range cp.Facts {
		if !owns(f.Field) || wantFact[f.Field+"\x00"+string(f.Rank)] {
			continue
		}
		if _, err := c.db.DeleteFact(cp.ID, f.Field, f.Rank); err != nil {
			return err
		}
	}
	return nil
}

// stripLocked empties the colony record of a person who left people.json
// without being forgotten (merged into someone else), keeping only the id
// and the status removed, so an unmerge can bring them back.
func (c *Colony) stripLocked(id string) error {
	cp, err := c.db.Get(id)
	if err != nil {
		return err
	}
	for _, i := range cp.Identities {
		if err := c.db.RemoveIdentity(id, i.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
			return err
		}
	}
	for _, f := range cp.Facts {
		if _, err := c.db.DeleteFact(id, f.Field, f.Rank); err != nil {
			return err
		}
	}
	for _, h := range cp.Held {
		if _, err := c.db.RemoveHeld(id, h.Ref()); err != nil && !errors.Is(err, cstore.ErrNotFound) {
			return err
		}
	}
	for _, a := range cp.Answers {
		if err := c.db.DeleteAnswer(id, a.Source); err != nil && !errors.Is(err, cstore.ErrNoAnswer) {
			return err
		}
	}
	for ns := range cp.Attachments {
		if err := c.db.SetAttachment(id, ns, nil); err != nil {
			return err
		}
	}
	_, err = c.db.Upsert(cstore.PersonSpec{ID: id, Status: colonyStatusRemoved})
	return err
}

// syncEnrichmentLocked brings one person's enrichment facts in line. A
// person colony does not hold (pruned, forgotten) is skipped.
func (c *Colony) syncEnrichmentLocked(id string, pe PersonEnrichment, held bool) error {
	if c.db == nil {
		return errors.New("customers: colony store closed")
	}
	cp, err := c.db.Get(id)
	if errors.Is(err, cstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var facts []cstore.Fact
	if held {
		facts = colonyEnrichmentFacts(pe, cp.Identities, c.now(), cp.Dismissed...)
	}
	rec := cstore.Record{Person: cstore.PersonSpec{ID: id}, Facts: facts}
	if err := c.pruneLocked(cp, rec, isEnrichmentField, false); err != nil {
		return err
	}
	for _, f := range facts {
		if _, err := c.db.SetFact(id, f); err != nil {
			return err
		}
	}
	// The full answers (every affiliation with its period, every project
	// with its name, period and link, and when each source answered) go to
	// colony's answer slots, so enrichment.json is no longer the only copy.
	var answers []cstore.Answer
	if held {
		answers = colonyAnswers(pe)
	}
	want := map[string]bool{}
	for _, a := range answers {
		want[a.Source] = true
		if _, err := c.db.SetAnswer(id, a); err != nil {
			return err
		}
	}
	for _, a := range cp.Answers {
		if want[a.Source] {
			continue
		}
		if err := c.db.DeleteAnswer(id, a.Source); err != nil && !errors.Is(err, cstore.ErrNoAnswer) {
			return err
		}
	}
	return nil
}

// resyncLocked brings colony.db in line with the whole JSON: every person
// is written again, an enrichment entry is rewritten, and a colony record
// whose person left the JSON is forgotten when hive forgot their
// identities, or stripped when they were merged away.
func (c *Colony) resyncLocked(people []Person, enrich map[string]PersonEnrichment, forgotten func(IdentityKind, string) bool) error {
	in := make(map[string]bool, len(people))
	for _, p := range people {
		in[p.ID] = true
	}
	keep := keptPersonIDs(people)
	var gone []string
	err := c.db.List(func(cp cstore.Person) error {
		// Someone merged into a person in the JSON is stripped; anyone
		// else who left the JSON was forgotten, and forgetOrphansLocked
		// erases them after this catch-up.
		if !in[cp.ID] && keep[cp.ID] && cp.Status != colonyStatusRemoved {
			gone = append(gone, cp.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if failed := c.syncPeopleLocked(people, gone, forgotten); len(failed) > 0 {
		return fmt.Errorf("customers: %d people could not be written to %s", len(failed), ColonyFileName)
	}
	for _, p := range people {
		pe, held := enrich[p.ID]
		if err := c.syncEnrichmentLocked(p.ID, pe, held); err != nil {
			return err
		}
	}
	return nil
}

// mirrorPeople is called by Store.change after people.json was written.
func (c *Colony) mirrorPeople(changed []Person, gone []string, forgets map[string][]string, saved []byte,
	forgotten func(IdentityKind, string) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, hashes := range forgets {
		c.addPendingLocked(id, hashes)
	}
	if len(forgets) > 0 {
		c.retryPendingLocked()
		if err := c.writeMarkerLocked(); err != nil {
			c.logWarn("[customers] colony sync marker not written", "error", err)
		}
	}
	var plainGone []string
	for _, id := range gone {
		if _, f := forgets[id]; !f {
			plainGone = append(plainGone, id)
		}
	}
	failed := c.syncPeopleLocked(changed, plainGone, forgotten)
	c.settleLocked(failed, func(m *colonySyncMarker) { m.People = bytesHash(saved) })
}

// mirrorEnrichment is called by the enrichment store after enrichment.json
// was written, with the people it changed.
func (c *Colony) mirrorEnrichment(changed map[string]PersonEnrichment, removed []string, saved []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var failed []string
	for id, pe := range changed {
		if err := c.syncEnrichmentLocked(id, pe, true); err != nil {
			failed = append(failed, id)
		}
	}
	for _, id := range removed {
		if err := c.syncEnrichmentLocked(id, PersonEnrichment{}, false); err != nil {
			failed = append(failed, id)
		}
	}
	c.settleLocked(failed, func(m *colonySyncMarker) { m.Enrichment = bytesHash(saved) })
}

// settleLocked records a write's outcome: failures make the people dirty
// and freeze the marker; a clean write advances it.
func (c *Colony) settleLocked(failed []string, advance func(*colonySyncMarker)) {
	for _, id := range failed {
		c.dirty[id] = true
		c.failures++
	}
	if len(failed) > 0 {
		c.logWarn("[customers] colony store write failed; the next boot imports again from the JSON", "people", len(failed))
	}
	if len(c.dirty) > 0 {
		return
	}
	advance(&c.marker)
	if err := c.writeMarkerLocked(); err != nil {
		c.logWarn("[customers] colony sync marker not written", "error", err)
	}
}

// compactLocked rewrites colony.db so pages a forget freed no longer hold
// the person's bytes. A failure leaves the file as it was.
// It reports whether the compaction succeeded.
func (c *Colony) compactLocked() bool {
	if c.db == nil {
		return false
	}
	colonyFileMu.Lock()
	defer colonyFileMu.Unlock()
	path := c.db.Path()
	if err := c.db.Close(); err != nil {
		c.logWarn("[customers] colony store close before compaction failed", "error", err)
	}
	c.db = nil
	compacted := true
	if err := compactBolt(path); err != nil {
		compacted = false
		c.logWarn("[customers] colony store compaction failed", "error", err)
	}
	db, err := cstore.Open(path, cstore.Options{Now: c.now})
	if err != nil {
		c.failures++
		c.dirty["*"] = true
		c.logWarn("[customers] colony store did not reopen after compaction; writes stop until restart", "error", err)
		return false
	}
	c.db = db
	return compacted
}

// compactBolt copies the bbolt file at path into a fresh file and renames
// it over, dropping freed pages.
func compactBolt(path string) error {
	tmp := path + ".compact"
	_ = os.Remove(tmp)
	src, err := bolt.Open(path, storeFileMode, &bolt.Options{ReadOnly: true, Timeout: cstore.DefaultLockTimeout})
	if err != nil {
		return err
	}
	dst, err := bolt.Open(tmp, storeFileMode, &bolt.Options{Timeout: cstore.DefaultLockTimeout})
	if err != nil {
		_ = src.Close()
		return err
	}
	cerr := bolt.Compact(dst, src, colonyCompactTxBytes)
	_ = src.Close()
	if err := dst.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, path)
}

// forgetInBackups removes a person from people.json.pre-colony and
// enrichment.json.pre-colony, drops the pins and hints that name them, and
// adds their tombstones, so a restored backup does not bring them back.
//
// The *.migrated files a colony-only migration left (#19972) are backups
// too, and are scrubbed the same way.
func forgetInBackups(dir, id string, tombs []string) error {
	peopleFiles := []string{filepath.Join(dir, FileName+PreColonySuffix)}
	enrichFiles := []string{filepath.Join(dir, EnrichmentFileName+PreColonySuffix)}
	for _, pat := range []struct {
		name string
		into *[]string
	}{{FileName, &peopleFiles}, {EnrichmentFileName, &enrichFiles}} {
		matches, err := filepath.Glob(filepath.Join(dir, pat.name+MigratedSuffix+"*"))
		if err != nil {
			return err
		}
		*pat.into = append(*pat.into, matches...)
	}
	for _, pp := range peopleFiles {
		if err := forgetInPeopleBackup(pp, id, tombs); err != nil {
			return err
		}
	}
	for _, ep := range enrichFiles {
		if err := forgetInEnrichmentBackup(ep, id); err != nil {
			return err
		}
	}
	return nil
}

// forgetInPeopleBackup removes a person from one people.json backup.
func forgetInPeopleBackup(pp, id string, tombs []string) error {
	if data, err := os.ReadFile(pp); err == nil {
		var f storeFile
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("customers: decoding %s: %w", filepath.Base(pp), err)
		}
		people := f.People[:0:0]
		for _, p := range f.People {
			if p.ID != id {
				people = append(people, p)
			}
		}
		pins := f.Pins[:0:0]
		for _, p := range f.Pins {
			if p.Person != id && p.Other != id {
				pins = append(pins, p)
			}
		}
		hints := f.Hints[:0:0]
		for _, h := range f.Hints {
			if h.Person != id && h.Other != id {
				hints = append(hints, h)
			}
		}
		f.People, f.Pins, f.Hints = people, pins, hints
		have := map[string]bool{}
		for _, t := range f.Tombstones {
			have[t] = true
		}
		for _, t := range tombs {
			if !have[t] {
				f.Tombstones = append(f.Tombstones, t)
			}
		}
		sort.Strings(f.Tombstones)
		out, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(pp, ".pre-colony-*", out); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// forgetInEnrichmentBackup removes a person from one enrichment.json backup.
func forgetInEnrichmentBackup(ep, id string) error {
	if data, err := os.ReadFile(ep); err == nil {
		var f enrichmentFile
		if json.Unmarshal(data, &f) != nil {
			// An unreadable enrichment backup holds nothing anyone can use;
			// remove it rather than keep a person in it.
			return os.Remove(ep)
		}
		if _, ok := f.People[id]; ok {
			delete(f.People, id)
			out, err := json.MarshalIndent(f, "", "  ")
			if err != nil {
				return err
			}
			if err := writeFileAtomic(ep, ".pre-colony-*", out); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ForgetOffline is Forget's colony half in json mode: colony.db and the
// .pre-colony backups may remain from an earlier shadow or colony run, and a
// forgotten person must leave them too. forgets maps each person id to the
// tombstones hive wrote for them. It opens colony.db only if it exists and
// never creates it. What it cannot finish (colony.db damaged, locked or
// written by a newer release) is recorded in colony-sync.json and retried by
// the next forget and by the next shadow or colony boot; the error is
// returned for the log only, since people.json has already forgotten them.
func ForgetOffline(dir string, forgets map[string][]string, now func() time.Time) error {
	colonyFileMu.Lock()
	defer colonyFileMu.Unlock()
	c := &Colony{dir: dir, now: now, dirty: map[string]bool{}}
	m, _ := c.readMarker()
	c.marker = m
	for id, tombs := range forgets {
		c.addPendingLocked(id, tombs)
	}
	if len(c.marker.PendingForget) == 0 {
		return nil
	}
	path := filepath.Join(dir, ColonyFileName)
	var openErr error
	var db *cstore.Store
	if _, err := os.Stat(path); err == nil {
		db, openErr = cstore.Open(path, cstore.Options{Now: now})
	} else if !errors.Is(err, os.ErrNotExist) {
		openErr = err
	}
	var failed error
	erased := false
	for id, tombs := range c.marker.PendingForget {
		ok := openErr == nil
		if db != nil {
			if err := colonyForgetOne(db, id); err != nil {
				ok, failed = false, err
			} else {
				erased = true
			}
		}
		if err := forgetInBackups(dir, id, tombs); err != nil {
			ok, failed = false, err
		}
		if ok {
			delete(c.marker.PendingForget, id)
		}
	}
	if db != nil {
		if err := db.Close(); err != nil && failed == nil {
			failed = err
		}
		if erased {
			if err := compactBolt(path); err != nil && failed == nil {
				failed = err
			}
		}
	}
	if openErr != nil {
		failed = fmt.Errorf("customers: opening %s to forget: %w", ColonyFileName, openErr)
	}
	if _, err := os.Stat(c.markerPath()); err == nil || len(c.marker.PendingForget) > 0 {
		if err := c.writeMarkerLocked(); err != nil && failed == nil {
			failed = err
		}
	}
	return failed
}

// readFromColony is the colony-mode read: for each person, what colony.db
// holds replaces the JSON copy of the identities colony can hold, the
// profile fields, the dismissed affiliations and the status. Metadata only
// hive keeps (who linked an identity, the login a value was read for) is
// kept when the value itself agrees. The result is written back to the
// JSON, so both files agree again.
func (s *Store) readFromColony(c *Colony) error {
	c.mu.Lock()
	byID := map[string]cstore.Person{}
	err := c.db.List(func(cp cstore.Person) error {
		byID[cp.ID] = cp
		return nil
	})
	c.mu.Unlock()
	if err != nil {
		return err
	}
	missing := 0
	err = s.change(func() error {
		for id, p := range s.people {
			cp, ok := byID[id]
			if !ok || cp.Status == colonyStatusRemoved {
				missing++
				continue
			}
			s.people[id] = s.overlayColonyLocked(p, cp)
		}
		return nil
	})
	if missing > 0 {
		c.logWarn("[customers] people.json holds people the colony store does not; kept from the JSON", "people", missing)
	}
	return err
}

// overlayColonyLocked applies cp to p; s.mu is held.
func (s *Store) overlayColonyLocked(p Person, cp cstore.Person) Person {
	inColony := map[string]cstore.Identity{}
	for _, i := range cp.Identities {
		inColony[colonyClassKey(i.Kind, i.Value)] = i
	}
	kept := p.Identities[:0:0]
	have := map[string]bool{}
	for _, i := range p.Identities {
		k, mapped := colonyKind(i.Kind)
		if !mapped {
			kept = append(kept, i)
			continue
		}
		key := colonyClassKey(k, NormalizeIdentity(i.Kind, i.Value))
		ci, ok := inColony[key]
		if ok {
			// colony is authoritative for whether an identity is confirmed.
			have[key] = true
			i.Claimed = !ci.Verified
		}
		// An identity colony does not hold is kept: colony may refuse one
		// (one owner per identity, tombstones written on forget), and the
		// JSON must stay a lossless rollback copy.
		kept = append(kept, i)
	}
	for key, ci := range inColony {
		if have[key] {
			continue
		}
		k, ok := hiveKind(ci.Kind)
		if !ok {
			continue
		}
		ident := Identity{Kind: k, Value: ci.Value, Source: ci.Source, Claimed: !ci.Verified, At: ci.At,
			Confidence: colonyConfidenceHigh}
		if !ci.Verified {
			ident.Confidence = colonyConfidenceLow
		}
		if owner, taken := s.owner[ident.Key()]; taken && owner != p.ID {
			continue
		}
		kept = append(kept, ident)
	}
	p.Identities = kept

	view := cp.EffectiveView(cstore.ViewOptions{ExcludeSuggested: true})
	for _, name := range colonyOwnedProfileFields {
		f := p.Profile.Field(name)
		eff, ok := view[name]
		next := ProfileField{}
		if ok {
			next = profileFieldFromFact(eff)
		}
		if f.Value == next.Value && f.Source == next.Source {
			continue
		}
		*f = next
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
	if !sameStringSet(dismissed, p.Profile.DismissedAffiliations) {
		p.Profile.DismissedAffiliations = dismissed
	}
	switch st := Status(cp.Status); st {
	case StatusCustomer, StatusSuggested:
		p.Status = st
	}
	return p
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// profileFieldFromFact maps an effective colony fact back to a profile
// field: user is manual, hub_shared is hub with its label, enrichment is
// auto with the source as its rule.
func profileFieldFromFact(f cstore.Fact) ProfileField {
	switch f.Rank {
	case cstore.RankUser, cstore.RankAdmin:
		return ProfileField{Value: f.Value, Source: SourceManual, At: f.At, By: f.By}
	case cstore.RankHubShared:
		return ProfileField{Value: f.Value, Source: SourceHub, Rule: hubRuleFromSource(f.Source), At: f.At}
	}
	out := ProfileField{Value: f.Value, Source: SourceAuto, Rule: f.Source, At: f.At}
	switch f.Source {
	case colonySourceAuto:
		out.Rule = ""
	case colonySourceGitdm:
		out.Rule = AffiliationAutoAcceptedGitdm
	}
	if f.Source == ProfileSourceGitHub {
		for _, d := range f.DerivedFrom {
			if d.Kind == cstore.KindGitHub {
				out.Login = d.Value
			}
		}
	}
	return out
}

// enrichmentFromColony is the colony-mode enrichment read for one person:
// the gitdm affiliation and the devstats counts come from colony.db, the
// display detail only hive keeps (project name, period, link, history, when
// each source answered) from enrichment.json.
func (c *Colony) enrichmentFromColony(id string, pe PersonEnrichment) PersonEnrichment {
	if c == nil || c.mode != StoreColony {
		return pe
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil || c.dirty[id] || c.dirty["*"] {
		return pe
	}
	cp, err := c.db.Get(id)
	if err != nil {
		return pe
	}
	var gitdm, other []EnrichedAffiliation
	for _, a := range pe.Affiliations {
		if a.Source == EnrichSourceGitdm {
			gitdm = append(gitdm, a)
		} else {
			other = append(other, a)
		}
	}
	fact, ok := cp.FactAt(cstore.FieldAffiliation, cstore.RankEnrichment)
	cur, curOK := currentEnrichedAffiliation(gitdm, c.now())
	switch {
	case ok && fact.Source == colonySourceGitdm && fact.Value != "":
		if !curOK || cur.Org != fact.Value {
			a := EnrichedAffiliation{Org: fact.Value, Source: EnrichSourceGitdm, Confidence: string(fact.Confidence)}
			if !fact.From.IsZero() {
				a.From = fact.From.UTC().Format(enrichmentDateLayout)
			}
			if !fact.Until.IsZero() {
				a.Until = fact.Until.UTC().Format(enrichmentDateLayout)
			}
			a.MatchedBy = EnrichMatchedByLogin
			for _, d := range fact.DerivedFrom {
				if d.Kind.IsEmail() {
					a.MatchedBy = "email"
				}
			}
			gitdm = []EnrichedAffiliation{a}
		}
	case curOK:
		// colony holds no current gitdm value (dismissed there, or none).
		gitdm = nil
	}
	out := clonePersonEnrichment(pe)
	out.Affiliations = append(other, gitdm...)

	counts := map[string]string{}
	for _, f := range cp.Facts {
		if f.Rank == cstore.RankEnrichment && f.Source == colonySourceDevst && strings.HasPrefix(f.Field, cstore.FieldActivityPrefix) {
			counts[f.Field] = f.Value
		}
	}
	var acts []EnrichedActivity
	seen := map[string]bool{}
	for _, a := range pe.Activity {
		if a.Source != EnrichSourceDevstats {
			acts = append(acts, a)
			continue
		}
		field := colonyActivityField(a.Project)
		v, ok := counts[field]
		if !ok {
			continue
		}
		seen[field] = true
		if n, err := strconv.Atoi(v); err == nil {
			a.Contributions = n
		}
		acts = append(acts, a)
	}
	var extra []string
	for field := range counts {
		if !seen[field] {
			extra = append(extra, field)
		}
	}
	sort.Strings(extra)
	for _, field := range extra {
		n, err := strconv.Atoi(counts[field])
		if err != nil {
			continue
		}
		acts = append(acts, EnrichedActivity{Project: strings.TrimPrefix(field, cstore.FieldActivityPrefix),
			Contributions: n, Source: EnrichSourceDevstats})
	}
	out.Activity = acts
	return out
}

// ColonyParity compares people.json with colony.db and returns counts only.
func (s *Store) ColonyParity() (ColonyParity, error) {
	s.mu.Lock()
	c := s.colony
	people := make([]Person, 0, len(s.people))
	for _, p := range s.people {
		people = append(people, clonePerson(p))
	}
	forgotten := s.forgottenLocked
	owners := colonyOwners(people)
	recs := make(map[string]cstore.Record, len(people))
	for _, p := range people {
		recs[p.ID] = colonyRecord(p, owners, forgotten)
	}
	s.mu.Unlock()
	if c == nil {
		return ColonyParity{Mode: StoreJSON, JSONPeople: len(people)}, nil
	}
	out := ColonyParity{Mode: c.mode, JSONPeople: len(people)}
	for _, r := range recs {
		out.JSONIdentities += len(r.Person.Identities)
		out.JSONHeld += len(r.Person.Held)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out.Failures = c.failures
	if c.db == nil {
		return out, errors.New("customers: colony store closed")
	}
	seen := map[string]bool{}
	err := c.db.List(func(cp cstore.Person) error {
		if cp.Status == colonyStatusRemoved {
			return nil
		}
		out.ColonyPeople++
		out.ColonyIdent += len(cp.Identities)
		out.ColonyHeld += len(cp.Held)
		for _, f := range cp.Facts {
			switch f.Rank {
			case cstore.RankUser:
				out.FactsUser++
			case cstore.RankHubShared:
				out.FactsHubShared++
			case cstore.RankEnrichment:
				out.FactsEnrichment++
			}
		}
		rec, ok := recs[cp.ID]
		seen[cp.ID] = true
		if !ok || !colonyMatches(cp, rec) {
			out.Mismatches++
		}
		return nil
	})
	for id := range recs {
		if !seen[id] {
			out.Mismatches++
		}
	}
	return out, err
}

// colonyMatches reports whether colony's record agrees with the JSON's on
// identities (with their provenance), held identities, profile facts and
// status.
func colonyMatches(cp cstore.Person, rec cstore.Record) bool {
	if cp.Status != rec.Person.Status {
		return false
	}
	ids := map[string]cstore.Identity{}
	for _, i := range cp.Identities {
		ids[colonyClassKey(i.Kind, i.Value)] = i
	}
	if len(ids) != len(rec.Person.Identities) {
		return false
	}
	for _, i := range rec.Person.Identities {
		have, ok := ids[colonyClassKey(i.Kind, i.Value)]
		if !ok || have.Verified != i.Verified || have.Manual != i.Manual || have.By != i.By || have.Score != i.Score {
			return false
		}
	}
	held := map[string]bool{}
	for _, h := range cp.Held {
		held[colonyClassKey(h.Kind, h.Value)] = true
	}
	if len(held) != len(rec.Person.Held) {
		return false
	}
	for _, i := range rec.Person.Held {
		if !held[colonyClassKey(i.Kind, i.Value)] {
			return false
		}
	}
	have := map[string]string{}
	for _, f := range cp.Facts {
		if isProfileField(f.Field) {
			have[f.Field+"\x00"+string(f.Rank)] = f.Value
		}
	}
	if len(have) != len(rec.Facts) {
		return false
	}
	for _, f := range rec.Facts {
		if v, ok := have[f.Field+"\x00"+string(f.Rank)]; !ok || v != f.Value {
			return false
		}
	}
	return true
}
