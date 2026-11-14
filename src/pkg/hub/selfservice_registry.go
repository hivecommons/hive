package hub

// Self-service (byo-) hive registry: random, hashed, rotatable heartbeat keys
// (#18060, step 2 of the RFC; step 1 is heartbeat_selfservice_fence.go).
//
// A hive in the reserved SelfServiceHiveIDPrefix namespace runs on its owner's
// infrastructure. Its heartbeat bearer is NOT derived from the hub master
// (derivePerHiveKey): the owner must never hold HIVE_HUB_SECRET, a
// master-derived key cannot be rotated without a new hive ID (#1537), and a
// master rotation would break hives the operator cannot redeploy. Instead the
// hub mints a random key per hive, stores only its SHA-256 hash, and shows the
// raw key to the owner exactly once (the registration API, a later child).
//
// Rotation keeps the previous hash live for selfServiceKeyRotationOverlap so a
// redeploy with the new key does not show the hive offline — the same idea as
// maxLiveGenerations for the master.
//
// Audit F2: each bearer identifies exactly one hive. verify() looks a key up BY
// hive ID and compares against that record only, so a self-service key can
// never authenticate any other ID, and verifyHeartbeatBearerGeneration never
// tries the master-derived path for a byo- ID.
//
// Key material never leaves this file except as the one-time return value of
// register and rotate. Nothing here logs a raw key or a hash.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// selfServiceStorePath is the DEFAULT on-disk self-service registry. A var so
// tests can redirect it, like registryPath.
var selfServiceStorePath = "/data/hub-selfservice.json"

const (
	// selfServiceKeyBytes is the entropy of a minted heartbeat key.
	selfServiceKeyBytes = 32
	// selfServiceKeyRotationOverlap is how long the previous key keeps
	// authenticating after a rotation, so the owner can redeploy the spoke
	// with the new key without the hive going offline.
	selfServiceKeyRotationOverlap = 10 * time.Minute
	// selfServiceHeartbeatStampInterval bounds how often an accepted heartbeat
	// rewrites the store: the stamp exists for the expiry sweep (24h
	// granularity), so persisting it on every beat would be pure churn.
	selfServiceHeartbeatStampInterval = time.Minute
	// selfServiceBearerGeneration is the generation ID reported by
	// verifyHeartbeatBearerGeneration for a key verified against this store.
	// It is not a master generation; it only has to differ from the master
	// generation IDs so the auth-rollout view can tell the lanes apart.
	selfServiceBearerGeneration = -1
	selfServiceStoreFileMode    = 0o600
	selfServiceStoreDirMode     = 0o755
)

var (
	errSelfServiceNotByoID   = errors.New("self-service hive ID must start with " + SelfServiceHiveIDPrefix)
	errSelfServiceExists     = errors.New("self-service hive already registered")
	errSelfServiceNotFound   = errors.New("self-service hive not found")
	errSelfServiceNilStore   = errors.New("self-service store not configured")
	errSelfServiceEmptyOwner = errors.New("self-service hive owner required")
	// errSelfServiceCapReached is returned by registerCapped when the owner
	// already holds the maximum number of hives (#18166).
	errSelfServiceCapReached = errors.New("self-service per-owner cap reached")
)

// SelfServiceHive is one owner-registered hive. Only hashes of its keys are
// ever stored; the raw key exists in memory for the duration of the register
// or rotate call that minted it.
type SelfServiceHive struct {
	ID    string `json:"id"`
	Owner string `json:"owner"` // canonical hub login of the registering user
	Org   string `json:"org,omitempty"`
	// Repos the owner proved admin permission on at OwnershipCheckedAt.
	Repos        []string `json:"repos,omitempty"`
	DashboardURL string   `json:"dashboard_url,omitempty"`
	// KeyHash is the SHA-256 (hex) of the current heartbeat key.
	KeyHash string `json:"key_hash"`
	// PrevKeyHash is the hash of the key before the last rotation; it keeps
	// authenticating until PrevKeyExpiresAt.
	PrevKeyHash      string    `json:"prev_key_hash,omitempty"`
	PrevKeyExpiresAt time.Time `json:"prev_key_expires_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	// OwnershipCheckedAt is when the hub last confirmed the owner's admin
	// permission on Repos. Zero when never checked.
	OwnershipCheckedAt time.Time `json:"ownership_checked_at,omitempty"`
	// LastAcceptedHeartbeatAt is the last heartbeat that passed the bearer
	// check. Zero until the first one; the expiry sweep (selfservice_expiry.go)
	// counts the window from here, or from CreatedAt while it is zero.
	LastAcceptedHeartbeatAt time.Time `json:"last_accepted_heartbeat_at,omitempty"`
}

// selfServiceStore holds every SelfServiceHive and persists them as one JSON
// file via temp-file + rename, like saveRegistryNow. All methods are safe on a
// nil receiver (bare &HubServer{} test literals leave the field nil): reads
// answer "nothing registered" and writes return errSelfServiceNilStore.
type selfServiceStore struct {
	mu    sync.RWMutex
	path  string
	hives map[string]*SelfServiceHive
	// stampPersisted is, per hive, the LastAcceptedHeartbeatAt value last
	// written to disk; noteAcceptedHeartbeat debounces against it, not against
	// the in-memory stamp, so a steady stream of beats still flushes once per
	// selfServiceHeartbeatStampInterval.
	stampPersisted map[string]time.Time
	// now is injectable for rotation-overlap tests; nil means time.Now.
	now func() time.Time
}

func newSelfServiceStore(path string) *selfServiceStore {
	return &selfServiceStore{path: path, hives: make(map[string]*SelfServiceHive), stampPersisted: make(map[string]time.Time)}
}

func (st *selfServiceStore) clock() time.Time {
	if st != nil && st.now != nil {
		return st.now()
	}
	return time.Now()
}

type selfServiceStoreFile struct {
	Hives     []SelfServiceHive `json:"hives"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// load reads the store from disk. A missing file is not an error: the hub
// starts with no self-service hives.
func (st *selfServiceStore) load() error {
	if st == nil {
		return errSelfServiceNilStore
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read self-service registry: %w", err)
	}
	var file selfServiceStoreFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse self-service registry: %w", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hives = make(map[string]*SelfServiceHive, len(file.Hives))
	st.stampPersisted = make(map[string]time.Time, len(file.Hives))
	for i := range file.Hives {
		h := file.Hives[i]
		if h.ID == "" {
			continue
		}
		st.hives[h.ID] = &h
		if !h.LastAcceptedHeartbeatAt.IsZero() {
			st.stampPersisted[h.ID] = h.LastAcceptedHeartbeatAt
		}
	}
	return nil
}

// saveLocked writes the store atomically. Caller holds st.mu (read or write).
func (st *selfServiceStore) saveLocked() error {
	file := selfServiceStoreFile{Hives: make([]SelfServiceHive, 0, len(st.hives)), UpdatedAt: st.clock().UTC()}
	for _, h := range st.hives {
		file.Hives = append(file.Hives, *h)
	}
	sort.Slice(file.Hives, func(i, j int) bool { return file.Hives[i].ID < file.Hives[j].ID })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal self-service registry: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(st.path), selfServiceStoreDirMode); err != nil {
		return fmt.Errorf("create self-service registry dir: %w", err)
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, selfServiceStoreFileMode); err != nil {
		return fmt.Errorf("write self-service registry: %w", err)
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return fmt.Errorf("rename self-service registry: %w", err)
	}
	return nil
}

// mintSelfServiceKey returns a fresh random heartbeat key (base64url, no
// padding) and its hash.
func mintSelfServiceKey() (raw, hash string, err error) {
	b := make([]byte, selfServiceKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("mint self-service key: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashSelfServiceKey(raw), nil
}

// hashSelfServiceKey is the only transform applied to a key before storage.
// SHA-256 is sufficient: the input is 256 bits of CSPRNG output, so there is
// nothing for a dictionary or a rainbow table to find.
func hashSelfServiceKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// register adds a new self-service hive with no per-owner cap and returns its
// raw key ONCE. See registerCapped.
func (st *selfServiceStore) register(h SelfServiceHive) (string, error) {
	return st.registerCapped(h, 0)
}

// registerCapped adds a new self-service hive and returns its raw key ONCE.
// The ID must already be in the byo- namespace; callers (the registration API)
// build it with selfServiceBuildID and run the registry-collision and
// validNamePattern checks before calling.
//
// When maxPerOwner > 0, the owner's existing hives are counted under the same
// write lock that inserts the new one, so concurrent registrations by one
// owner can never exceed the cap (#18166): the handler's earlier count is only
// a fast refusal before the GitHub ownership calls, and it races.
func (st *selfServiceStore) registerCapped(h SelfServiceHive, maxPerOwner int) (string, error) {
	if st == nil {
		return "", errSelfServiceNilStore
	}
	h.ID = strings.ToLower(strings.TrimSpace(h.ID))
	if !IsSelfServiceHiveID(h.ID) {
		return "", errSelfServiceNotByoID
	}
	if strings.TrimSpace(h.Owner) == "" {
		return "", errSelfServiceEmptyOwner
	}
	raw, hash, err := mintSelfServiceKey()
	if err != nil {
		return "", err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, exists := st.hives[h.ID]; exists {
		return "", errSelfServiceExists
	}
	if maxPerOwner > 0 {
		owned := 0
		for _, existing := range st.hives {
			if selfServiceOwns(*existing, h.Owner) {
				owned++
			}
		}
		if owned >= maxPerOwner {
			return "", errSelfServiceCapReached
		}
	}
	h.KeyHash = hash
	h.PrevKeyHash = ""
	h.PrevKeyExpiresAt = time.Time{}
	h.CreatedAt = st.clock().UTC()
	h.LastAcceptedHeartbeatAt = time.Time{}
	st.hives[h.ID] = &h
	if err := st.saveLocked(); err != nil {
		delete(st.hives, h.ID)
		return "", err
	}
	return raw, nil
}

// rotate mints a new key for id and returns it ONCE. The previous key keeps
// authenticating until selfServiceKeyRotationOverlap has passed.
func (st *selfServiceStore) rotate(id string) (string, error) {
	if st == nil {
		return "", errSelfServiceNilStore
	}
	raw, hash, err := mintSelfServiceKey()
	if err != nil {
		return "", err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.hives[id]
	if !ok {
		return "", errSelfServiceNotFound
	}
	before := *h
	h.PrevKeyHash = h.KeyHash
	h.PrevKeyExpiresAt = st.clock().UTC().Add(selfServiceKeyRotationOverlap)
	h.KeyHash = hash
	if err := st.saveLocked(); err != nil {
		*h = before
		return "", err
	}
	return raw, nil
}

// revoke removes id. After it, no key authenticates the hive.
func (st *selfServiceStore) revoke(id string) (bool, error) {
	if st == nil {
		return false, errSelfServiceNilStore
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.hives[id]
	if !ok {
		return false, nil
	}
	delete(st.hives, id)
	if err := st.saveLocked(); err != nil {
		st.hives[id] = h
		return false, err
	}
	delete(st.stampPersisted, id)
	return true, nil
}

// verify reports whether presented is the current key for id, or the previous
// key inside its overlap window. Constant-time on both comparisons. A key is
// only ever compared against the record for the ID the caller claims, so it
// authenticates that one hive and nothing else (audit F2).
func (st *selfServiceStore) verify(id, presented string) bool {
	if st == nil || id == "" || presented == "" {
		return false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	h, ok := st.hives[id]
	if !ok {
		return false
	}
	hash := hashSelfServiceKey(presented)
	if h.KeyHash != "" && secureCompareHub(hash, h.KeyHash) {
		return true
	}
	if h.PrevKeyHash != "" && st.clock().Before(h.PrevKeyExpiresAt) && secureCompareHub(hash, h.PrevKeyHash) {
		return true
	}
	return false
}

// get returns a copy of the record for id.
func (st *selfServiceStore) get(id string) (SelfServiceHive, bool) {
	if st == nil {
		return SelfServiceHive{}, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	h, ok := st.hives[id]
	if !ok {
		return SelfServiceHive{}, false
	}
	return *h, true
}

// list returns copies of every record, sorted by ID.
func (st *selfServiceStore) list() []SelfServiceHive {
	if st == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]SelfServiceHive, 0, len(st.hives))
	for _, h := range st.hives {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// noteAcceptedHeartbeat stamps LastAcceptedHeartbeatAt for id. It persists at
// most once per selfServiceHeartbeatStampInterval per hive; the in-memory
// stamp is always current. Unknown IDs are ignored: a byo- ID that is not in
// the store cannot have passed the bearer check.
func (st *selfServiceStore) noteAcceptedHeartbeat(id string, now time.Time) error {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	h, ok := st.hives[id]
	if !ok {
		return nil
	}
	h.LastAcceptedHeartbeatAt = now.UTC()
	persisted, wasPersisted := st.stampPersisted[id]
	if wasPersisted && now.Sub(persisted) < selfServiceHeartbeatStampInterval {
		return nil
	}
	if err := st.saveLocked(); err != nil {
		return err
	}
	st.stampPersisted[id] = now.UTC()
	return nil
}
