package hub

// The hub CRM fields on the colony store (hivecommons/hive#19856, phase 4 of
// src/docs/design/colony-customer-data-backend.md).
//
// HIVE_HUB_CUSTOMER_STORE picks the mode, read once at boot:
//
//   - json (the default): today's behaviour. No colony.db is created. If one
//     exists from an earlier shadow or colony boot, it is opened only so that
//     deleting a user also erases them from it.
//   - shadow: on boot, every SaaSUser JSON record is imported into
//     /data/saas/colony.db (first boot: one transaction, with the source
//     fingerprint recorded), then reconciled person by person and checked by
//     counts. Every later write goes to both stores; the JSON stays
//     authoritative for reads.
//   - colony: as shadow, and reads of the CRM fields come from colony. A
//     field or person colony does not hold reads from the JSON (dual-read),
//     and the JSON is still written, so setting json again loses nothing.
//
// The JSON files are never rewritten by the migration. They are the backup
// and the rollback path. Before each shadow or colony boot opens colony.db,
// a copy of it is kept as colony.db.bak-<time> (newest few only).
//
// Failure never blocks the hub: a file that is locked, unreadable or written
// by a newer colony release fails closed to json mode and is left untouched.
// A failed mirror write is logged by count and never fails the JSON save.
// Nothing here logs a value.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/colony/store"
)

const (
	// hubCustomerStoreEnv selects the mode.
	hubCustomerStoreEnv = "HIVE_HUB_CUSTOMER_STORE"

	hubCustomerStoreJSON   = "json"
	hubCustomerStoreShadow = "shadow"
	hubCustomerStoreColony = "colony"

	// hubCustomerStoreMigration names the one-time import in colony's meta
	// bucket.
	hubCustomerStoreMigration = "hub-saas-users-v1"
	// hubCustomerStoreBackupPrefix is appended to colony.db for boot backups.
	hubCustomerStoreBackupPrefix = ".bak-"
	// hubCustomerStoreBackupsKept bounds the boot backups kept on the PVC.
	hubCustomerStoreBackupsKept = 3
	// hubCustomerStoreBackupStamp is the backup suffix layout (UTC). Fixed
	// width, so the names sort in time order.
	hubCustomerStoreBackupStamp = "20060102T150405.000000000Z"
	// hubCustomerStoreLockTimeout bounds the wait for the bbolt file lock,
	// so a stuck lock costs a boot seconds, never availability.
	hubCustomerStoreLockTimeout = 5 * time.Second
	// hubCustomerStoreBackupMode keeps backups owner-only, like colony.db.
	hubCustomerStoreBackupMode os.FileMode = 0o600
)

// parseHubCustomerStoreMode reads the mode. An unknown value is json, so a
// typo can only keep today's behaviour.
func parseHubCustomerStoreMode(raw string) (string, bool) {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "", hubCustomerStoreJSON:
		return hubCustomerStoreJSON, true
	case hubCustomerStoreShadow, hubCustomerStoreColony:
		return m, true
	}
	return hubCustomerStoreJSON, false
}

// hubCustomerStorePath is /data/saas/colony.db: next to the users directory
// whose records it holds.
func hubCustomerStorePath() string {
	return filepath.Join(filepath.Dir(saasUsersDir), store.DefaultFileName)
}

// hubCustomerStoreState is the process-wide handle. The hub is one writer
// process on its PVC, which bbolt's file lock also enforces.
type hubCustomerStoreState struct {
	// mu serialises mirror writes, so each person's read-compare-write in
	// syncPerson is atomic with respect to other writers in this process.
	mu sync.Mutex
	// rw guards the fields below.
	rw   sync.RWMutex
	st   *store.Store
	mode string
	// usersDir is the saasUsersDir the store was opened for. Hooks do
	// nothing for any other directory.
	usersDir string
}

var hubCRM hubCustomerStoreState

// active returns the open store and the effective mode, or nil.
func (h *hubCustomerStoreState) active() (*store.Store, string) {
	h.rw.RLock()
	defer h.rw.RUnlock()
	if h.st == nil || h.usersDir != saasUsersDir {
		return nil, hubCustomerStoreJSON
	}
	return h.st, h.mode
}

// hubCustomerStoreSyncReport is the count-only result of a boot sync.
type hubCustomerStoreSyncReport struct {
	Imported      bool
	JSONUsers     int
	Synced        int
	Skipped       int
	Failed        int
	Forgotten     int
	ColonyPeople  int
	ColonyFacts   map[store.Rank]int
	ColonyConsent int
	Tombstones    int
	Parity        bool
}

// openHubCustomerStore opens the store for mode and, in shadow or colony
// mode, runs the boot sync. It returns the effective mode: json whenever the
// store cannot be used.
func openHubCustomerStore(ctx context.Context, logger *slog.Logger, mode string) string {
	if logger == nil {
		logger = slog.Default()
	}
	closeHubCustomerStore()
	path := hubCustomerStorePath()
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if mode == hubCustomerStoreJSON && !exists {
		return hubCustomerStoreJSON
	}
	if mode != hubCustomerStoreJSON && exists {
		if err := backupHubCustomerStore(path, time.Now().UTC()); err != nil {
			logger.Warn("hub customer store: backup failed; staying in json mode", "error", err)
			return hubCustomerStoreJSON
		}
	}
	if mode != hubCustomerStoreJSON {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			logger.Warn("hub customer store: mkdir failed; staying in json mode", "error", err)
			return hubCustomerStoreJSON
		}
	}
	st, err := store.Open(path, store.Options{LockTimeout: hubCustomerStoreLockTimeout})
	if err != nil {
		// ErrSchemaTooNew (an older binary on a newer file), ErrLocked or an
		// unreadable file: fail closed to json, never rewrite the file.
		logger.Warn("hub customer store: open failed; json mode", "requested", mode, "error", err)
		return hubCustomerStoreJSON
	}
	if mode != hubCustomerStoreJSON {
		rep, err := syncHubCustomerStore(ctx, st)
		if err != nil {
			_ = st.Close()
			logger.Warn("hub customer store: boot sync failed; json mode", "requested", mode, "error", err)
			return hubCustomerStoreJSON
		}
		logger.Info("hub customer store: boot sync", "mode", mode, "imported", rep.Imported,
			"json_users", rep.JSONUsers, "synced", rep.Synced, "skipped", rep.Skipped, "failed", rep.Failed,
			"forgotten", rep.Forgotten, "colony_people", rep.ColonyPeople,
			"facts_user", rep.ColonyFacts[store.RankUser], "facts_admin", rep.ColonyFacts[store.RankAdmin],
			"facts_enrichment", rep.ColonyFacts[store.RankEnrichment], "consents", rep.ColonyConsent,
			"tombstones", rep.Tombstones, "parity", rep.Parity)
		if !rep.Parity {
			logger.Warn("hub customer store: parity mismatch after boot sync; reads fall back to json per person",
				"json_users", rep.JSONUsers, "colony_people", rep.ColonyPeople, "skipped", rep.Skipped, "failed", rep.Failed)
		}
	}
	hubCRM.rw.Lock()
	hubCRM.st, hubCRM.mode, hubCRM.usersDir = st, mode, saasUsersDir
	hubCRM.rw.Unlock()
	return mode
}

// closeHubCustomerStore closes the store, if open.
func closeHubCustomerStore() {
	hubCRM.rw.Lock()
	st := hubCRM.st
	hubCRM.st, hubCRM.mode, hubCRM.usersDir = nil, hubCustomerStoreJSON, ""
	hubCRM.rw.Unlock()
	if st != nil {
		_ = st.Close()
	}
}

// startHubCustomerStore is the boot entry point (StartBackgroundPollers).
// It runs synchronously, before the pollers, so the import sees no
// concurrent writer from this process.
func (s *HubServer) startHubCustomerStore(ctx context.Context) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	raw := os.Getenv(hubCustomerStoreEnv)
	mode, ok := parseHubCustomerStoreMode(raw)
	if !ok {
		logger.Warn("hub customer store: unknown mode; using json", "env", hubCustomerStoreEnv)
	}
	if got := openHubCustomerStore(ctx, logger, mode); got != mode {
		logger.Warn("hub customer store: running in json mode", "requested", mode)
	}
}

// backupHubCustomerStore copies colony.db aside before a boot touches it and
// keeps only the newest hubCustomerStoreBackupsKept copies. A backup can
// hold a person forgotten after it was taken until it rotates out.
func backupHubCustomerStore(path string, now time.Time) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	dst := path + hubCustomerStoreBackupPrefix + now.Format(hubCustomerStoreBackupStamp)
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hubCustomerStoreBackupMode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return pruneHubCustomerStoreBackups(path)
}

func pruneHubCustomerStoreBackups(path string) error {
	matches, err := filepath.Glob(path + hubCustomerStoreBackupPrefix + "*")
	if err != nil {
		return err
	}
	// The stamp sorts lexically in time order.
	sort.Strings(matches)
	for len(matches) > hubCustomerStoreBackupsKept {
		if err := os.Remove(matches[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		matches = matches[1:]
	}
	return nil
}

// hubUserFile is one JSON user record on disk.
type hubUserFile struct {
	// ID is the canonical person id derived from the file name.
	ID   string
	Key  string
	Path string
}

// listHubUserFiles lists the user records by file name. An error reading
// the directory is returned, so a caller never mistakes it for "no users".
func listHubUserFiles() ([]hubUserFile, error) {
	entries, err := os.ReadDir(saasUsersDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []hubUserFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		key := stem
		if id, ok := decodeUserFilename(stem); ok {
			key = id
		}
		out = append(out, hubUserFile{ID: canonicalizeLegacy(key), Key: key, Path: filepath.Join(saasUsersDir, e.Name())})
	}
	return out, nil
}

// readHubUserForSync reads one record straight from disk, without the
// colony-mode overlay: the sync must mirror the JSON, and must not open a
// store read inside the import's write transaction.
func readHubUserForSync(path string) (*SaaSUser, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var u SaaSUser
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// hubUsersSource is the colony migration Source over /data/saas/users.
type hubUsersSource struct{}

// Fingerprint is the SHA-256 over every user and consent file, by name and
// content, in name order.
func (hubUsersSource) Fingerprint() (string, error) {
	h := sha256.New()
	for _, dir := range []string{saasUsersDir, customerConsentDir()} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00", filepath.Base(dir), e.Name(), len(data))
			h.Write(data)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Records yields one colony Record per user. An identity another user
// already holds is dropped from the later user, so one shared email cannot
// fail the whole import transaction.
func (hubUsersSource) Records(ctx context.Context, fn func(store.Record) error) error {
	files, err := listHubUserFiles()
	if err != nil {
		return err
	}
	seenPerson := make(map[string]bool)
	seenIdentity := make(map[string]bool)
	identityKey := func(i store.Identity) string {
		kind := i.Kind
		if kind.IsEmail() {
			kind = store.KindEmail
		}
		return string(kind) + "\x00" + strings.ToLower(strings.TrimSpace(i.Value))
	}
	// Two passes: every primary identity is claimed first, so a later
	// user's login held by an earlier user as a linked login or email never
	// drops the later user.
	type loaded struct {
		u       *SaaSUser
		id      string
		primary store.Identity
	}
	var users []loaded
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		u, err := readHubUserForSync(f.Path)
		if err != nil {
			continue // counted by the reconcile pass
		}
		id := crmPersonID(u)
		primary, ok := crmPrimaryIdentity(u)
		if id == "" || !ok || seenPerson[id] || seenIdentity[identityKey(primary)] {
			continue
		}
		seenPerson[id] = true
		seenIdentity[identityKey(primary)] = true
		users = append(users, loaded{u: u, id: id, primary: primary})
	}
	for _, l := range users {
		if err := ctx.Err(); err != nil {
			return err
		}
		u, id := l.u, l.id
		idents := []store.Identity{l.primary}
		for _, ident := range crmSecondaryIdentities(u) {
			if k := identityKey(ident); !seenIdentity[k] {
				seenIdentity[k] = true
				idents = append(idents, ident)
			}
		}
		rec := store.Record{
			Person: store.PersonSpec{ID: id, Identities: idents},
			Facts:  crmFactsFromUser(u),
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(u.CreatedAt)); err == nil {
			rec.CreatedAt = t.UTC()
		}
		if path, err := customerConsentPath(u); err == nil {
			if records, err := readCustomerConsents(path); err == nil {
				rec.Consents = crmConsentsFromRecords(records)
			}
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// syncHubCustomerStore is the boot sync: the one-time import, then a
// person-by-person reconcile that makes colony match the JSON, then the
// erasure of people whose JSON record is gone, then a count check. Running
// it again changes nothing.
func syncHubCustomerStore(ctx context.Context, st *store.Store) (hubCustomerStoreSyncReport, error) {
	rep := hubCustomerStoreSyncReport{}
	files, err := listHubUserFiles()
	if err != nil {
		return rep, err
	}
	rep.JSONUsers = len(files)
	res, err := st.Migrate(ctx, hubCustomerStoreMigration, hubUsersSource{}, store.MigrateOptions{})
	if err != nil {
		// The import is one transaction, so a failure leaves nothing half
		// written; the reconcile below still brings each person across, and
		// the next boot tries the import again.
		slog.Warn("hub customer store: import transaction failed; reconciling per person", "error", err)
	} else {
		rep.Imported = !res.Skipped
	}

	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	onDisk := make(map[string]bool, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		onDisk[f.ID] = true
		u, err := readHubUserForSync(f.Path)
		if err != nil {
			rep.Failed++
			continue
		}
		onDisk[crmPersonID(u)] = true
		switch err := syncPerson(st, u); {
		case err == nil:
			rep.Synced++
		case errors.Is(err, errCRMSkipped):
			rep.Skipped++
		default:
			rep.Failed++
		}
	}

	// Erase people whose JSON record is gone (deleted while colony.db was
	// not open). Keyed by file name, so a record that exists but does not
	// parse is never treated as deleted.
	var orphans []string
	if err := st.List(func(p store.Person) error {
		if !onDisk[p.ID] {
			orphans = append(orphans, p.ID)
		}
		return nil
	}); err != nil {
		return rep, err
	}
	for _, id := range orphans {
		if _, err := st.Forget(id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return rep, err
		}
		rep.Forgotten++
	}

	stats, err := st.Stats()
	if err != nil {
		return rep, err
	}
	rep.ColonyPeople, rep.ColonyFacts, rep.ColonyConsent, rep.Tombstones = stats.People, stats.Facts, stats.Consents, stats.Tombstones
	rep.Parity = rep.ColonyPeople == rep.Synced && rep.Failed == 0
	return rep, nil
}

// errCRMSkipped marks a person colony refuses on purpose: a forgotten
// (tombstoned) identity, or a primary identity another person holds.
var errCRMSkipped = errors.New("hub customer store: person skipped")

// syncPerson makes colony hold exactly what the JSON record u holds: its
// identities, one fact per field at the JSON source's rank, and its consent
// records. The caller holds hubCRM.mu. Only changed values are written, so a
// second run is a no-op.
func syncPerson(st *store.Store, u *SaaSUser) error {
	id := crmPersonID(u)
	primary, ok := crmPrimaryIdentity(u)
	if id == "" || !ok {
		return errCRMSkipped
	}
	p, err := st.Get(id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	holds := func(want store.Identity) bool {
		return err == nil && slices.ContainsFunc(p.Identities, func(have store.Identity) bool {
			return sameCRMIdentity(have, want) && have.Source == want.Source && have.Verified == want.Verified &&
				(have.Locality == want.Locality || have.Locality == store.LocalityLocalOnly)
		})
	}
	changed := false
	if !holds(primary) {
		if err := upsertCRMPrimary(st, id, primary); err != nil {
			if errors.Is(err, store.ErrTombstoned) || errors.Is(err, store.ErrIdentityConflict) {
				return errCRMSkipped
			}
			return err
		}
		changed = true
	}
	secondary := crmSecondaryIdentities(u)
	for _, ident := range secondary {
		if holds(ident) {
			continue
		}
		if _, err := st.UpsertIdentity(id, ident); err != nil &&
			!errors.Is(err, store.ErrTombstoned) && !errors.Is(err, store.ErrIdentityConflict) {
			return err
		}
		changed = true
	}
	if changed {
		if p, err = st.Get(id); err != nil {
			return err
		}
	}
	// Identities the record no longer carries (a changed email, an unlinked
	// login) are detached.
	wantIdent := append([]store.Identity{primary}, secondary...)
	for _, have := range p.Identities {
		if !slices.ContainsFunc(wantIdent, func(w store.Identity) bool { return sameCRMIdentity(have, w) }) {
			if err := st.RemoveIdentity(id, have.Ref()); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}

	want := crmFactsFromUser(u)
	for _, f := range want {
		if have, ok := p.FactAt(f.Field, f.Rank); ok && crmFactEqual(have, f) {
			continue
		}
		if _, err := st.SetFact(id, f); err != nil {
			return err
		}
	}
	for _, have := range p.Facts {
		if !slices.ContainsFunc(want, func(w store.Fact) bool { return w.Field == have.Field && w.Rank == have.Rank }) {
			if _, err := st.DeleteFact(id, have.Field, have.Rank); err != nil {
				return err
			}
		}
	}

	path, err := customerConsentPath(u)
	if err != nil {
		return nil
	}
	records, err := readCustomerConsents(path)
	if err != nil {
		return err
	}
	consents := crmConsentsFromRecords(records)
	for _, c := range consents {
		if have, ok := p.Consent(c.Scope); ok && crmConsentEqual(have, c) {
			continue
		}
		if err := st.SetConsent(id, c); err != nil {
			return err
		}
	}
	// A consent the hub pruned has no record left; colony has no delete,
	// so it is withdrawn there and never reads as active.
	for _, have := range p.Consents {
		if !slices.ContainsFunc(consents, func(c store.Consent) bool { return c.Scope == have.Scope }) && have.Active() {
			if err := st.WithdrawConsent(id, have.Scope); err != nil && !errors.Is(err, store.ErrNoConsent) {
				return err
			}
		}
	}
	return nil
}

// upsertCRMPrimary creates the person with its sign-in identity. A sign-in
// identity outranks a secondary one: when another person holds it only as a
// linked login or an email, it is detached from that person first.
func upsertCRMPrimary(st *store.Store, id string, primary store.Identity) error {
	_, err := st.Upsert(store.PersonSpec{ID: id, Identities: []store.Identity{primary}})
	if !errors.Is(err, store.ErrIdentityConflict) {
		return err
	}
	owner, ferr := st.FindByIdentity(primary.Kind, primary.Value)
	if ferr != nil {
		return err
	}
	for _, have := range owner.Identities {
		if sameCRMIdentity(have, primary) && have.Source != crmIdentitySourceLogin {
			if rerr := st.RemoveIdentity(owner.ID, have.Ref()); rerr != nil {
				return rerr
			}
			_, err = st.Upsert(store.PersonSpec{ID: id, Identities: []store.Identity{primary}})
			return err
		}
	}
	return err
}

func sameCRMIdentity(a, b store.Identity) bool {
	ka, kb := a.Kind, b.Kind
	if ka.IsEmail() {
		ka = store.KindEmail
	}
	if kb.IsEmail() {
		kb = store.KindEmail
	}
	return ka == kb && strings.EqualFold(strings.TrimSpace(a.Value), strings.TrimSpace(b.Value))
}

func crmConsentEqual(a, b store.Consent) bool {
	return a.Scope == b.Scope && slices.Equal(a.Fields, b.Fields) && a.TextVersion == b.TextVersion &&
		a.GrantedAt.Equal(b.GrantedAt) && a.WithdrawnAt.Equal(b.WithdrawnAt) && a.DeliveredAt.Equal(b.DeliveredAt)
}

// crmMirrorUser is the dual-write hook in saveSaaSUser (shadow and colony).
func crmMirrorUser(u *SaaSUser) {
	st, mode := hubCRM.active()
	if st == nil || mode == hubCustomerStoreJSON || u == nil {
		return
	}
	hubCRM.mu.Lock()
	err := syncPerson(st, u)
	hubCRM.mu.Unlock()
	if err != nil && !errors.Is(err, errCRMSkipped) {
		slog.Warn("hub customer store: mirror write failed; json is unaffected", "error", err)
	}
}

// crmMirrorConsentFile is the dual-write hook for a consent file write.
func crmMirrorConsentFile(path string) {
	st, mode := hubCRM.active()
	if st == nil || mode == hubCustomerStoreJSON {
		return
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	key := stem
	if id, ok := decodeUserFilename(stem); ok {
		key = id
	}
	userPath := ""
	for _, p := range saaSUserFilePaths(key) {
		if _, err := os.Stat(p); err == nil {
			userPath = p
			break
		}
	}
	if userPath == "" {
		return
	}
	u, err := readHubUserForSync(userPath)
	if err != nil {
		return
	}
	if p, err := customerConsentPath(u); err != nil || p != path {
		return
	}
	crmMirrorUser(u)
}

// crmOverlayUser is the colony-mode read hook in loadSaaSUser.
func crmOverlayUser(u *SaaSUser) {
	st, mode := hubCRM.active()
	if st == nil || mode != hubCustomerStoreColony || u == nil {
		return
	}
	p, err := st.Get(crmPersonID(u))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("hub customer store: read failed; using json", "error", err)
		}
		return
	}
	crmOverlay(u, p)
}

// crmForgetUser erases a deleted user from colony in every mode, before the
// JSON record is removed, so a failure leaves the user in place to retry.
func crmForgetUser(u *SaaSUser) error {
	st, _ := hubCRM.active()
	if st == nil || u == nil {
		return nil
	}
	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	if _, err := st.Forget(crmPersonID(u)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}
