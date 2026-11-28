package hub

// The colony-only guard (#20116). A hub image older than colony-only opens
// a colony-only colony.db as an ordinary store: it reads the stripped user
// records as empty and its boot sync writes that emptiness into colony. Those
// images cannot be changed, but every one of them refuses a file whose
// schema_version is newer than its colony release (store.ErrSchemaTooNew)
// and falls back to json mode without writing the file. So while the store
// is colony-only, the hub records a schema_version no colony release writes,
// and keeps the real one beside it. This hub puts the real version back
// before it opens the file and raises it again once the store is open, so
// the file is guarded for as long as it is colony-only, including after a
// crash. A store that is not colony-only (json, shadow or colony mode) is
// never guarded.

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"
	"unsafe"

	"github.com/hivecommons/colony/store"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// hubColonyOnlyGuardSchema is the schema_version a guarded file records. It
// is far above any colony release, so every released and future colony
// rejects it with ErrSchemaTooNew.
const hubColonyOnlyGuardSchema = 1_000_000

var (
	// colony's meta bucket and its schema_version key (colony store.go).
	hubColonyMetaBucket    = []byte("meta")
	hubColonySchemaVersion = []byte("schema_version")
	// hubColonyOnlyGuardKey holds the real schema_version while the guard
	// is set. Colony reads only its own meta keys, so it ignores this one.
	hubColonyOnlyGuardKey = []byte("hub:colony-only-guard")
)

// setHubColonyOnlyGuardTx sets or clears the guard in one transaction.
func setHubColonyOnlyGuardTx(tx *bolt.Tx, on bool) error {
	meta := tx.Bucket(hubColonyMetaBucket)
	if meta == nil {
		return nil
	}
	held := meta.Get(hubColonyOnlyGuardKey)
	if on {
		if held != nil {
			return nil
		}
		cur := meta.Get(hubColonySchemaVersion)
		v, err := strconv.Atoi(string(cur))
		if err != nil || v < 1 || v > store.SchemaVersion {
			return fmt.Errorf("hub customer store: unexpected schema_version %q; colony-only guard not set", cur)
		}
		if err := meta.Put(hubColonyOnlyGuardKey, []byte(strconv.Itoa(v))); err != nil {
			return err
		}
		return meta.Put(hubColonySchemaVersion, []byte(strconv.Itoa(hubColonyOnlyGuardSchema)))
	}
	if held == nil {
		return nil
	}
	if string(meta.Get(hubColonySchemaVersion)) == strconv.Itoa(hubColonyOnlyGuardSchema) {
		if err := meta.Put(hubColonySchemaVersion, append([]byte(nil), held...)); err != nil {
			return err
		}
	}
	return meta.Delete(hubColonyOnlyGuardKey)
}

// hubColonyOnlyGuarded reports whether the guard is set.
func hubColonyOnlyGuarded(db *bolt.DB) (bool, error) {
	var on bool
	err := db.View(func(tx *bolt.Tx) error {
		if meta := tx.Bucket(hubColonyMetaBucket); meta != nil {
			on = meta.Get(hubColonyOnlyGuardKey) != nil
		}
		return nil
	})
	return on, err
}

// releaseHubColonyOnlyGuard puts the real schema_version back in the file
// at path, so store.Open accepts it. It runs before every open; a file
// without the guard is not written. A held lock reports store.ErrLocked, so
// the caller's retry treats it like a locked store.Open.
func releaseHubColonyOnlyGuard(path string, timeout time.Duration) error {
	if _, err := os.Stat(path); err != nil {
		// Nothing to release; store.Open reports anything else.
		return nil
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return fmt.Errorf("%w: %s", store.ErrLocked, path)
		}
		return fmt.Errorf("hub customer store: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	on, err := hubColonyOnlyGuarded(db)
	if err != nil || !on {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error { return setHubColonyOnlyGuardTx(tx, false) })
}

// setHubColonyOnlyGuard sets or clears the guard on an open store. Colony
// reads schema_version only when it opens the file, so the open store is
// unaffected.
func setHubColonyOnlyGuard(st *store.Store, on bool) error {
	db, err := hubStoreBolt(st)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error { return setHubColonyOnlyGuardTx(tx, on) })
}

// syncHubColonyOnlyGuard sets the guard when the store is colony-only (the
// colony-only document is present) and clears it otherwise.
func syncHubColonyOnlyGuard(st *store.Store) error {
	migrated, err := hubColonyOnlyMigrated(st)
	if err != nil {
		return err
	}
	return setHubColonyOnlyGuard(st, migrated)
}

// hubStoreBolt returns the bbolt handle of an open colony store. Colony has
// no API for its schema_version, and the open store holds the file lock, so
// the guard is written through the store's own handle. A colony release
// that changes this field fails TestHubColonyOnlyGuardReachesStoreHandle.
func hubStoreBolt(st *store.Store) (*bolt.DB, error) {
	if st == nil {
		return nil, errors.New("hub customer store: no open store")
	}
	f := reflect.ValueOf(st).Elem().FieldByName("db")
	if !f.IsValid() || f.Type() != reflect.TypeFor[*bolt.DB]() {
		return nil, errors.New("hub customer store: colony store layout changed; colony-only guard not set")
	}
	db := *(**bolt.DB)(unsafe.Pointer(f.UnsafeAddr())) // #nosec G103 -- reads colony's own *bolt.DB field, checked by name and type above
	if db == nil {
		return nil, errors.New("hub customer store: colony store has no file handle")
	}
	return db, nil
}
