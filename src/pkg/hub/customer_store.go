package hub

// The hub CRM fields on the colony store (hivecommons/hive#19856, phase 4 of
// src/docs/design/colony-customer-data-backend.md).
//
// HIVE_HUB_CUSTOMER_STORE picks the mode, read once at boot. Unset means
// colony (the default since #19972); a set but unknown value means json.
//
//   - json: the behaviour before the colony store, and the rollback. No
//     colony.db is created. If one exists from an earlier shadow or colony
//     boot, it is opened only so that deleting a user also erases them from
//     it.
//   - shadow: on boot, every SaaSUser JSON record is imported into
//     /data/saas/colony.db (first boot: one transaction, with the source
//     fingerprint recorded), then reconciled person by person and checked by
//     counts. Every later write goes to both stores; the JSON stays
//     authoritative for reads.
//   - colony (the default): as shadow, and reads of the CRM fields come from
//     colony. A field or person colony does not hold reads from the JSON
//     (dual-read), and the JSON is still written, so setting json again
//     loses nothing. A hub upgrading from the json default runs the import
//     on that boot; if it fails, the hub stays on json.
//   - colony-only (opt-in, never the default; customer_store_only.go): after
//     a verified boot, colony.db is the only copy of the CRM fields and the
//     consent records, and user records are written without the CRM fields.
//
// The JSON files are never rewritten by the migration. They are the backup
// and the rollback path. Before each shadow or colony boot opens colony.db,
// a copy of it is kept as colony.db.bak-<time> (newest few only).
//
// Failure never blocks the hub: a file that is unreadable, written by a newer
// colony release, or still locked by another process after the boot retry
// window (HIVE_HUB_CUSTOMER_STORE_LOCK_WAIT, #20103) fails closed to json mode
// and is left untouched.
// A failed mirror write is logged by count and never fails the JSON save.
// Nothing here logs a value.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// hubCustomerStoreLockWaitEnv overrides how long boot keeps retrying a
	// colony.db that another process holds (#20103). A Go duration; 0 turns
	// the retry off (one attempt, as before).
	hubCustomerStoreLockWaitEnv = "HIVE_HUB_CUSTOMER_STORE_LOCK_WAIT"
	// hubCustomerStoreLockWaitDefault covers a lock that is still held when
	// the new hub pod boots: the old pod's exit and, on the RWX volume, the
	// server-side release of its lock, which after an unclean node loss can
	// take a lock-lease recovery of about 90s. A boot can wait up to this plus
	// one more hubCustomerStoreLockTimeout attempt.
	hubCustomerStoreLockWaitDefault = 2 * time.Minute
	// hubCustomerStoreBackupMode keeps backups owner-only, like colony.db.
	hubCustomerStoreBackupMode os.FileMode = 0o600
	// hubCustomerStoreMaxOrphanPercent and hubCustomerStoreOrphanFloor bound
	// how many people one boot may erase because their JSON record is gone.
	// More than that looks like a missing or half-restored users directory,
	// not deletions, and erasure cannot be undone (it tombstones).
	hubCustomerStoreMaxOrphanPercent = 10
	hubCustomerStoreOrphanFloor      = 10
)

// Lock timing, variables so tests can shrink them.
var (
	// hubCustomerStoreLockTimeout bounds each attempt's wait for the bbolt
	// file lock.
	hubCustomerStoreLockTimeout = 5 * time.Second
	// hubCustomerStoreRetryBackoff is the first pause between attempts on a
	// locked file; it doubles up to hubCustomerStoreRetryBackoffMax.
	hubCustomerStoreRetryBackoff    = 1 * time.Second
	hubCustomerStoreRetryBackoffMax = 15 * time.Second
)

// hubCustomerStoreLockWait reads hubCustomerStoreLockWaitEnv. Unset, invalid
// or negative values use the default.
func hubCustomerStoreLockWait(logger *slog.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv(hubCustomerStoreLockWaitEnv))
	if raw == "" {
		return hubCustomerStoreLockWaitDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		logger.Warn("hub customer store: invalid lock wait; using default",
			"env", hubCustomerStoreLockWaitEnv, "default", hubCustomerStoreLockWaitDefault)
		return hubCustomerStoreLockWaitDefault
	}
	return d
}

// openHubCustomerStoreFile opens colony.db, retrying while another process
// holds its lock (#20103). During a hub pod handover the old pod keeps the
// lock until it exits, so one failed attempt must not put the new pod in json
// mode for its whole life. Only store.ErrLocked is retried, with doubling
// backoff, until wait has passed; every other error returns at once. Each
// retry is logged.
func openHubCustomerStoreFile(ctx context.Context, logger *slog.Logger, path, mode string, wait time.Duration) (*store.Store, error) {
	deadline := time.Now().Add(wait)
	backoff := hubCustomerStoreRetryBackoff
	for attempt := 1; ; attempt++ {
		st, err := store.Open(path, store.Options{LockTimeout: hubCustomerStoreLockTimeout})
		if err == nil || !errors.Is(err, store.ErrLocked) {
			if err == nil && attempt > 1 {
				logger.Info("hub customer store: lock acquired after retry", "requested", mode, "attempts", attempt)
			}
			return st, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		pause := min(backoff, remaining)
		logger.Warn("hub customer store: colony.db locked by another process; retrying",
			"requested", mode, "attempt", attempt, "retry_in", pause, "gives_up_in", remaining.Round(time.Second))
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, hubCustomerStoreRetryBackoffMax)
	}
}

// hubCustomerStoreDefault is the mode when HIVE_HUB_CUSTOMER_STORE is unset
// or blank (#19972). json stays a valid value and the rollback.
const hubCustomerStoreDefault = hubCustomerStoreColony

// parseHubCustomerStoreMode reads the mode. Unset is the colony default; an
// unknown value is json, so a mistyped rollback can never move the hub onto
// colony.
func parseHubCustomerStoreMode(raw string) (string, bool) {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "":
		return hubCustomerStoreDefault, true
	case hubCustomerStoreJSON, hubCustomerStoreShadow, hubCustomerStoreColony, hubCustomerStoreColonyOnly:
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
	// dirty holds the person ids whose last sync failed or was skipped.
	// A colony-mode read never overlays them, so a colony copy that fell
	// behind the JSON can never be written back over it. Guarded by mu.
	dirty map[string]bool
}

// markCRM records the outcome of a sync for person id. The caller holds mu.
func (h *hubCustomerStoreState) markCRM(id string, err error) {
	if h.dirty == nil {
		h.dirty = make(map[string]bool)
	}
	if err == nil {
		delete(h.dirty, id)
		return
	}
	h.dirty[id] = true
}

func (h *hubCustomerStoreState) isDirty(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dirty[id]
}

// crmErrClass names an error without its text, which can carry a file name
// and so a login: logs say what failed, never about whom.
func crmErrClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrLocked):
		return "locked"
	case errors.Is(err, store.ErrSchemaTooNew):
		return "schema_too_new"
	case errors.Is(err, store.ErrInvalid):
		return "invalid_value"
	case errors.Is(err, store.ErrTombstoned):
		return "tombstoned"
	case errors.Is(err, store.ErrIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, errCRMNotStored):
		return "not_stored"
	case errors.Is(err, errConsentsNewer):
		// /data/saas/consents changed after the colony-only migration:
		// move it aside (hub-deployment.md, colony-only).
		return "consents_newer_than_colony_only"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "io_or_other"
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
	OrphansHeld   int
	ColonyPeople  int
	ColonyFacts   map[store.Rank]int
	ColonyConsent int
	// ColonyHeld counts identities colony keeps as held (a second user
	// with the same linked login or email).
	ColonyHeld int
	Tombstones int
	Parity     bool
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
	if mode != hubCustomerStoreJSON {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			logger.Warn("hub customer store: mkdir failed; staying in json mode", "error", crmErrClass(err))
			return hubCustomerStoreJSON
		}
	}
	st, err := openHubCustomerStoreFile(ctx, logger, path, mode, hubCustomerStoreLockWait(logger))
	if err != nil {
		// ErrSchemaTooNew (an older binary on a newer file), a lock still
		// held after the retry window, or an unreadable file: fail closed to
		// json, never rewrite the file.
		logger.Warn("hub customer store: open failed; json mode", "requested", mode, "error", crmErrClass(err))
		return hubCustomerStoreJSON
	}
	// A colony-only migration that crashed part-way is undone first, so
	// nothing below reads a consents directory it had renamed away.
	if err := recoverHubColonyOnlyPending(st); err != nil {
		_ = st.Close()
		logger.Error("hub customer store: an interrupted colony-only migration could not be undone; json mode, nothing changed",
			"requested", mode, "error", crmErrClass(err))
		return hubCustomerStoreJSON
	}
	if mode != hubCustomerStoreColonyOnly {
		// Back from colony-only: the CRM fields and consent files are
		// written back from colony first (#19972).
		if restored, err := restoreHubFromColonyOnly(st, time.Now()); err != nil {
			// The data is only in colony: stay colony-only rather than
			// serve records without it. The next boot tries again.
			logger.Warn("hub customer store: restore from colony-only failed; staying colony-only", "requested", mode,
				"error", crmErrClass(err))
			mode = hubCustomerStoreColonyOnly
		} else if restored {
			logger.Info("hub customer store: restored json from colony-only", "mode", mode)
		}
	}
	migrated := false
	if mode == hubCustomerStoreColonyOnly {
		m, err := hubColonyOnlyMigrated(st)
		if err != nil {
			_ = st.Close()
			logger.Warn("hub customer store: colony-only check failed; json mode", "error", crmErrClass(err))
			return hubCustomerStoreJSON
		}
		migrated = m
		if migrated {
			// A consents directory beside a store colony-only took over is
			// never read as the truth: it is set aside, not deleted.
			// One newer than the migration refuses colony-only, with
			// nothing written: json mode reads that directory, and colony.db
			// and every stripped record stay as they are.
			if aside, err := setAsideReappearedConsents(st, time.Now()); err != nil {
				_ = st.Close()
				logger.Error("hub customer store: a consents directory beside the colony-only store was not set aside; json mode",
					"error", crmErrClass(err))
				return hubCustomerStoreJSON
			} else if aside {
				logger.Warn("hub customer store: a consents directory reappeared beside the colony-only store; set aside as consents.ignored, colony stays the truth")
			}
		}
	}
	if mode != hubCustomerStoreJSON && exists {
		// Backed up only once the lock is held, from a consistent read of
		// the open store, so a boot that fails closed never rotates a good
		// backup out.
		if err := backupHubCustomerStore(st, path, time.Now().UTC()); err != nil {
			_ = st.Close()
			logger.Warn("hub customer store: backup failed; json mode", "error", crmErrClass(err))
			return hubCustomerStoreJSON
		}
	}
	if mode != hubCustomerStoreJSON && !migrated {
		rep, err := syncHubCustomerStore(ctx, st)
		if err != nil {
			_ = st.Close()
			logger.Warn("hub customer store: boot sync failed; json mode", "requested", mode, "error", crmErrClass(err))
			return hubCustomerStoreJSON
		}
		logger.Info("hub customer store: boot sync", "mode", mode, "imported", rep.Imported,
			"json_users", rep.JSONUsers, "synced", rep.Synced, "skipped", rep.Skipped, "failed", rep.Failed,
			"forgotten", rep.Forgotten, "orphans_held", rep.OrphansHeld, "colony_people", rep.ColonyPeople,
			"facts_user", rep.ColonyFacts[store.RankUser], "facts_admin", rep.ColonyFacts[store.RankAdmin],
			"facts_enrichment", rep.ColonyFacts[store.RankEnrichment], "consents", rep.ColonyConsent,
			"held", rep.ColonyHeld, "tombstones", rep.Tombstones, "parity", rep.Parity)
		if !rep.Parity {
			logger.Warn("hub customer store: parity mismatch after boot sync; people that did not sync read from json",
				"json_users", rep.JSONUsers, "colony_people", rep.ColonyPeople, "skipped", rep.Skipped, "failed", rep.Failed)
		}
		if mode == hubCustomerStoreColonyOnly {
			// First colony-only boot: only a clean sync and a verified
			// read-back move the hub off the JSON.
			switch {
			case !rep.Parity:
				logger.Warn("hub customer store: colony-only needs a clean boot sync; running in colony mode")
				mode = hubCustomerStoreColony
			case rep.Skipped > 0:
				// A user colony does not hold would lose their consent
				// records with the rename.
				logger.Warn("hub customer store: colony-only needs every user in colony; running in colony mode",
					"skipped", rep.Skipped)
				mode = hubCustomerStoreColony
			default:
				if err := migrateHubColonyOnly(st, time.Now()); err != nil {
					logger.Warn("hub customer store: colony-only migration not verified; running in colony mode, json kept",
						"error", crmErrClass(err))
					mode = hubCustomerStoreColony
				}
			}
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
	hubCRM.mu.Lock()
	hubCRM.dirty = nil
	hubCRM.mu.Unlock()
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

// backupHubCustomerStore writes a full snapshot of the open store (colony's
// unfiltered Export, which also carries the tombstones and their salt) aside
// as colony.db.bak-<time>, keeping only the newest hubCustomerStoreBackupsKept.
// A snapshot identical to the newest backup is not written again, so
// repeated boots do not rotate distinct backups out. A backup holds a person
// forgotten after it was taken until it rotates out.
func backupHubCustomerStore(st *store.Store, path string, now time.Time) error {
	snap, err := st.Export()
	if err != nil {
		return err
	}
	snap.ExportedAt = time.Time{}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(path + hubCustomerStoreBackupPrefix + "*")
	if err != nil {
		return err
	}
	sort.Strings(matches)
	if n := len(matches); n > 0 {
		if prev, err := os.ReadFile(matches[n-1]); err == nil && sha256.Sum256(prev) == sha256.Sum256(data) {
			return nil
		}
	}
	dst := path + hubCustomerStoreBackupPrefix + now.Format(hubCustomerStoreBackupStamp)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".colony-backup-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(hubCustomerStoreBackupMode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
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
// the directory, including a missing one, is returned, so a caller never
// mistakes it for "no users".
func listHubUserFiles() ([]hubUserFile, error) {
	entries, err := os.ReadDir(saasUsersDir)
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
	// Only a file directly inside the users directory is ever read.
	path = filepath.Clean(path)
	if filepath.Dir(path) != filepath.Clean(saasUsersDir) || !strings.HasSuffix(path, ".json") {
		return nil, fs.ErrInvalid
	}
	data, err := os.ReadFile(path) // #nosec G703 -- confined above to a .json file directly inside saasUsersDir
	if err != nil {
		return nil, err
	}
	var u SaaSUser
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// hubUsersSource is the colony migration Source over /data/saas/users. It
// carries the listing the boot sync works from, so the import and the
// orphan pass see the same set of files.
type hubUsersSource struct {
	files []hubUserFile
}

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
func (src hubUsersSource) Records(ctx context.Context, fn func(store.Record) error) error {
	files := src.files
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
	// A missing or unreadable users directory fails the sync (and so the
	// boot falls back to json): it must never read as "every user deleted".
	files, err := listHubUserFiles()
	if errors.Is(err, fs.ErrNotExist) {
		// A hub with no users directory yet is only fine for an empty store.
		if stats, serr := st.Stats(); serr == nil && stats.People == 0 {
			files, err = nil, nil
		}
	}
	if err != nil {
		return rep, err
	}
	res, err := st.Migrate(ctx, hubCustomerStoreMigration, hubUsersSource{files: files}, store.MigrateOptions{})
	if err != nil {
		// The import is one transaction, so a failure leaves nothing half
		// written; the reconcile below still brings each person across, and
		// the next boot tries the import again.
		slog.Warn("hub customer store: import transaction failed; reconciling per person", "error", crmErrClass(err))
	} else {
		rep.Imported = !res.Skipped
	}

	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	onDisk := make(map[string]bool, len(files))
	synced := make(map[string]bool, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		onDisk[f.ID] = true
		u, err := readHubUserForSync(f.Path)
		if err != nil {
			hubCRM.markCRM(f.ID, err)
			rep.Failed++
			continue
		}
		id := crmPersonID(u)
		onDisk[id] = true
		if synced[id] {
			continue // a second file for the same person
		}
		err = syncPerson(st, u)
		hubCRM.markCRM(id, err)
		switch {
		case err == nil:
			synced[id] = true
		case errors.Is(err, errCRMSkipped):
			rep.Skipped++
		default:
			rep.Failed++
		}
	}
	rep.Synced = len(synced)
	rep.JSONUsers = len(onDisk)

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
	// A burst of orphans is held, not erased: it looks like a missing or
	// half-restored users directory, and erasure tombstones for good.
	var people int
	if stats, err := st.Stats(); err == nil {
		people = stats.People
	}
	limit := max(hubCustomerStoreOrphanFloor, people*hubCustomerStoreMaxOrphanPercent/100)
	if len(orphans) > limit {
		rep.OrphansHeld = len(orphans)
		slog.Warn("hub customer store: too many people without a json record; none erased this boot",
			"orphans", len(orphans), "limit", limit, "colony_people", people)
		orphans = nil
	}
	for _, id := range orphans {
		if _, err := st.Forget(id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return rep, err
		}
		delete(hubCRM.dirty, id)
		rep.Forgotten++
	}

	stats, err := st.Stats()
	if err != nil {
		return rep, err
	}
	rep.ColonyPeople, rep.ColonyFacts, rep.ColonyConsent, rep.Tombstones = stats.People, stats.Facts, stats.Consents, stats.Tombstones
	rep.ColonyHeld = stats.Held
	rep.Parity = rep.ColonyPeople == rep.Synced && rep.Failed == 0 && rep.OrphansHeld == 0
	return rep, nil
}

// errCRMSkipped marks a person colony refuses on purpose: a forgotten
// (tombstoned) identity, or a primary identity another person holds.
var errCRMSkipped = errors.New("hub customer store: person skipped")

// errCRMNotStored marks a fact colony declined without an error (a stale
// timestamp after a clock step), so the person is marked dirty rather than
// silently left behind the JSON.
var errCRMNotStored = errors.New("hub customer store: fact not stored")

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
		if err != nil {
			return false
		}
		same := func(have store.Identity) bool {
			return sameCRMIdentity(have, want) && have.Source == want.Source && have.Verified == want.Verified &&
				(have.Locality == want.Locality || have.Locality == store.LocalityLocalOnly)
		}
		if slices.ContainsFunc(p.Identities, same) {
			return true
		}
		return slices.ContainsFunc(p.Held, func(h store.HeldIdentity) bool { return same(h.Identity) })
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
		// One another person owns is kept here as a held identity
		// (colony v0.4.0) instead of being dropped.
		if _, err := st.Upsert(store.PersonSpec{ID: id, Identities: []store.Identity{ident}, HoldConflicts: true}); err != nil &&
			!errors.Is(err, store.ErrTombstoned) {
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
	for _, have := range p.Held {
		if !slices.ContainsFunc(wantIdent, func(w store.Identity) bool { return sameCRMIdentity(have.Identity, w) }) {
			if _, err := st.RemoveHeld(id, have.Ref()); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}

	want := crmFactsFromUser(u)
	for _, f := range want {
		if have, ok := p.FactAt(f.Field, f.Rank); ok && crmFactEqual(have, f) {
			continue
		}
		res, err := st.SetFact(id, f)
		if err != nil {
			return err
		}
		if !res.Stored {
			return errCRMNotStored
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
	return syncCRMConsents(st, id, records)
}

// syncCRMConsents makes colony's consent records for person id match the
// hub's, delivery bookkeeping included (colony v0.4.0). A consent the hub
// pruned is deleted from colony too.
func syncCRMConsents(st *store.Store, id string, records map[string]customerConsent) error {
	p, err := st.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	consents := crmConsentsFromRecords(records)
	for _, c := range consents {
		have, ok := p.Consent(c.Scope)
		if ok && crmConsentEqual(have, c) {
			continue
		}
		if ok {
			// SetConsent keeps stored delivery bookkeeping when the record
			// names none; the hub's record is the whole truth, so replace it.
			if _, err := st.DeleteConsent(id, c.Scope); err != nil {
				return err
			}
		}
		if err := st.SetConsent(id, c); err != nil {
			return err
		}
	}
	for _, have := range p.Consents {
		if slices.ContainsFunc(consents, func(c store.Consent) bool { return c.Scope == have.Scope }) {
			continue
		}
		if _, err := st.DeleteConsent(id, have.Scope); err != nil {
			return err
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
		a.GrantedAt.Equal(b.GrantedAt) && a.WithdrawnAt.Equal(b.WithdrawnAt) && a.DeliveredAt.Equal(b.DeliveredAt) &&
		(b.UpdatedAt.IsZero() || a.UpdatedAt.Equal(b.UpdatedAt)) && a.FirstDeliveredAt.Equal(b.FirstDeliveredAt) &&
		slices.Equal(a.DeliveredFields, b.DeliveredFields) && slices.Equal(a.LastDeliveredFields, b.LastDeliveredFields) &&
		a.DeliveredRev == b.DeliveredRev
}

// crmMirrorUser is the dual-write hook in saveSaaSUser (shadow and colony).
func crmMirrorUser(u *SaaSUser) {
	st, mode := hubCRM.active()
	if st == nil || mode == hubCustomerStoreJSON || u == nil {
		return
	}
	hubCRM.mu.Lock()
	defer hubCRM.mu.Unlock()
	// Mirror what is on disk now, read under mu, not the caller's copy: two
	// concurrent saves of one person then always converge on the JSON that
	// won the rename, whatever order their mirrors run in.
	id := crmPersonID(u)
	if path, err := saveSaaSUserPath(u); err == nil {
		if latest, err := readHubUserForSync(path); err == nil {
			u = latest
		}
	}
	err := syncPerson(st, u)
	hubCRM.markCRM(id, err)
	if err != nil && !errors.Is(err, errCRMSkipped) {
		slog.Warn("hub customer store: mirror write failed; json is unaffected", "error", crmErrClass(err))
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
	if u == nil {
		return
	}
	st, mode := hubCRM.active()
	if st != nil && mode == hubCustomerStoreColonyOnly {
		u.crmOverlaid = crmOnlyOverlay(st, u)
		return
	}
	u.crmOverlaid = true
	if st == nil || mode != hubCustomerStoreColony {
		return
	}
	id := crmPersonID(u)
	if hubCRM.isDirty(id) {
		return // colony may be behind the JSON for this person
	}
	p, err := st.Get(id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("hub customer store: read failed; using json", "error", crmErrClass(err))
		}
		return
	}
	crmOverlay(u, p)
}

// crmOverlayConsents is the colony-mode read hook for a person's consent
// records (#19857): the grant is read from colony as well as from the
// consent file, and colony can only narrow what the file grants, never
// widen it. Writes go to the file first and are then mirrored, so when the
// two differ colony is the copy that may be behind, and a mirror that did
// not run (a user file that could not be read at that moment) must never
// bring back a field the person turned off or undo a withdrawal. The
// hub-only delivery bookkeeping (first and last delivery, delivered fields,
// rev) always comes from the file in this mode, though colony holds a copy
// since v0.4.0.
// It is called by the read paths: the match index, the match answer and the
// person's own consent page. Write paths never call it.
//
// Only hive ids the consent file still lists are considered: the file
// decides which records exist (a consent the hub prunes is deleted from
// colony too, and an emptied record an older release left there is never
// read). A person whose last sync failed or was
// skipped is read from the file alone, like crmOverlayUser.
func crmOverlayConsents(u *SaaSUser, records map[string]customerConsent) {
	st, mode := hubCRM.active()
	if st == nil || mode != hubCustomerStoreColony || u == nil || len(records) == 0 {
		return
	}
	id := crmPersonID(u)
	if hubCRM.isDirty(id) {
		return
	}
	p, err := st.Get(id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("hub customer store: consent read failed; using json", "error", crmErrClass(err))
		}
		return
	}
	for hiveID, rec := range records {
		have, ok := p.Consent(hiveID)
		if !ok {
			continue // colony does not hold it: dual-read from the JSON
		}
		records[hiveID] = crmOverlayConsent(rec, have)
	}
}

// crmOverlayConsent narrows one consent record by the colony grant:
//   - a field is granted only when both the file and colony grant it;
//   - the record is withdrawn when either side is withdrawn (the file's time
//     wins when it has one);
//   - a disclosure version that is not the current one, on either side, is
//     kept, so a stale version on either side stops delivery.
//
// The granted time and the delivery bookkeeping stay from the file.
func crmOverlayConsent(rec customerConsent, have store.Consent) customerConsent {
	var colony customerConsentFields
	for _, f := range have.Fields {
		switch f {
		case store.FieldRealName:
			colony.RealName = true
		case store.FieldOrganization:
			colony.Organization = true
		case store.FieldCountry:
			colony.Country = true
		}
	}
	rec.Fields = customerConsentFields{
		RealName:     rec.Fields.RealName && colony.RealName,
		Organization: rec.Fields.Organization && colony.Organization,
		Country:      rec.Fields.Country && colony.Country,
	}
	if rec.WithdrawnAt == nil && !have.WithdrawnAt.IsZero() {
		w := have.WithdrawnAt
		rec.WithdrawnAt = &w
	}
	if rec.TextVersion == customerConsentTextVersion && have.TextVersion != customerConsentTextVersion {
		rec.TextVersion = have.TextVersion
	}
	return rec
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
	delete(hubCRM.dirty, crmPersonID(u))
	return nil
}
