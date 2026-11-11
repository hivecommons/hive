package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

// RFC #17620 phase 7: the hub-side workflow marketplace catalogue.
//
// Layout under hubMarketplaceDir:
//
//	workflows/<name>/<version>.bundle.json   immutable published bundles
//	index.json                               derived catalogue index
//	refresh/<owner>__<repo>.json             last ingest run per corpus repo
//	last-refresh.json                        summary of the last completed run
//
// Entries are keyed name@version and never overwritten: a republish with the
// same key and hash is a no-op, a different hash is a 409. Curated versions
// are never replaced by ingest; the index flags them newerIngestAvailable
// when a later ingested-unreviewed version of the same name exists.

// hubMarketplaceDir is the catalogue root; tests redirect it.
var hubMarketplaceDir = "/data/marketplace"

var (
	errMarketplaceConflict = errors.New("marketplace: name@version already published with a different hash")
	errMarketplaceNotFound = errors.New("marketplace: bundle not found")
	errMarketplaceBadKey   = errors.New("marketplace: invalid name or version")

	marketplaceNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	marketplaceVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,31}$`)
)

// marketplaceStore is the on-disk catalogue.
type marketplaceStore struct {
	root string
	mu   sync.RWMutex
	// idx is the cached index document; nil until first load.
	idx     *bundle.Index
	idxRaw  []byte
	idxETag string
}

func newMarketplaceStore(root string) *marketplaceStore {
	return &marketplaceStore{root: root}
}

func (st *marketplaceStore) workflowsDir() string { return filepath.Join(st.root, "workflows") }
func (st *marketplaceStore) refreshDir() string   { return filepath.Join(st.root, "refresh") }
func (st *marketplaceStore) indexPath() string    { return filepath.Join(st.root, "index.json") }

func (st *marketplaceStore) bundlePath(name, version string) string {
	return filepath.Join(st.workflowsDir(), name, version+".bundle.json")
}

func marketplaceValidKey(name, version string) bool {
	return marketplaceNameRe.MatchString(name) && marketplaceVersionRe.MatchString(version)
}

// Publish stores a verified bundle. Same key + same hash is idempotent;
// same key + different hash is errMarketplaceConflict.
func (st *marketplaceStore) Publish(b *bundle.Bundle, by string, now time.Time, sourceSHA string) (bundle.IndexEntry, bool, error) {
	if err := bundle.Verify(b); err != nil {
		return bundle.IndexEntry{}, false, err
	}
	name, version := b.Model.Metadata.Name, b.Model.Spec.Meta.Version
	if !marketplaceValidKey(name, version) {
		return bundle.IndexEntry{}, false, fmt.Errorf("%w: %s@%s", errMarketplaceBadKey, name, version)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if existing, err := st.readBundleLocked(name, version); err == nil {
		if existing.Hash == b.Hash {
			idx, err := st.loadIndexLocked()
			if err != nil {
				return bundle.IndexEntry{}, false, err
			}
			for _, e := range idx.Entries {
				if e.Name == name && e.Version == version {
					return e, false, nil
				}
			}
			return bundle.EntryFor(existing, existing.CreatedAt, existing.CreatedBy, sourceSHA), false, nil
		}
		return bundle.IndexEntry{}, false, fmt.Errorf("%w: %s@%s", errMarketplaceConflict, name, version)
	} else if !errors.Is(err, errMarketplaceNotFound) {
		return bundle.IndexEntry{}, false, err
	}
	data, err := bundle.Encode(b)
	if err != nil {
		return bundle.IndexEntry{}, false, err
	}
	if err := marketplaceWriteAtomic(st.bundlePath(name, version), data); err != nil {
		return bundle.IndexEntry{}, false, err
	}
	entry := bundle.EntryFor(b, now, by, sourceSHA)
	idx, err := st.loadIndexLocked()
	if err != nil {
		return bundle.IndexEntry{}, false, err
	}
	// loadIndexLocked may have rebuilt from disk and already seen the
	// bundle written above; replace rather than duplicate.
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if e.Name != name || e.Version != version {
			kept = append(kept, e)
		}
	}
	idx.Entries = append(kept, entry)
	if err := st.writeIndexLocked(idx, now); err != nil {
		return bundle.IndexEntry{}, false, err
	}
	for _, e := range st.idx.Entries {
		if e.Name == name && e.Version == version {
			entry = e
		}
	}
	return entry, true, nil
}

// Get returns the stored bundle bytes for name@version.
func (st *marketplaceStore) Get(name, version string) ([]byte, error) {
	if !marketplaceValidKey(name, version) {
		return nil, fmt.Errorf("%w: %s@%s", errMarketplaceBadKey, name, version)
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	data, err := os.ReadFile(st.bundlePath(name, version))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errMarketplaceNotFound
	}
	return data, err
}

func (st *marketplaceStore) readBundleLocked(name, version string) (*bundle.Bundle, error) {
	data, err := os.ReadFile(st.bundlePath(name, version))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errMarketplaceNotFound
	}
	if err != nil {
		return nil, err
	}
	return bundle.Decode(data)
}

// Index returns the catalogue index, its canonical bytes and ETag.
func (st *marketplaceStore) Index() (*bundle.Index, []byte, string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := st.loadIndexLocked(); err != nil {
		return nil, nil, "", err
	}
	cp := *st.idx
	cp.Entries = append([]bundle.IndexEntry(nil), st.idx.Entries...)
	return &cp, st.idxRaw, st.idxETag, nil
}

// Versions lists the published versions of name, oldest first by publish time.
func (st *marketplaceStore) Versions(name string) ([]bundle.IndexEntry, error) {
	idx, _, _, err := st.Index()
	if err != nil {
		return nil, err
	}
	var out []bundle.IndexEntry
	for _, e := range idx.Entries {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out, nil
}

// loadIndexLocked reads index.json (rebuilding from the bundle files when it
// is missing) into st.idx and returns it.
func (st *marketplaceStore) loadIndexLocked() (*bundle.Index, error) {
	if st.idx != nil {
		return st.idx, nil
	}
	data, err := os.ReadFile(st.indexPath())
	switch {
	case err == nil:
		var idx bundle.Index
		if jerr := json.Unmarshal(data, &idx); jerr == nil {
			st.setIndexLocked(&idx, data)
			return st.idx, nil
		}
		// Corrupt index: fall through and rebuild.
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	idx, err := st.rebuildLocked()
	if err != nil {
		return nil, err
	}
	if err := st.writeIndexLocked(idx, time.Now().UTC()); err != nil {
		return nil, err
	}
	return st.idx, nil
}

// rebuildLocked derives an index from the bundle files on disk.
func (st *marketplaceStore) rebuildLocked() (*bundle.Index, error) {
	idx := &bundle.Index{Entries: []bundle.IndexEntry{}}
	names, err := os.ReadDir(st.workflowsDir())
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(st.workflowsDir(), n.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".bundle.json") {
				continue
			}
			version := strings.TrimSuffix(f.Name(), ".bundle.json")
			b, err := st.readBundleLocked(n.Name(), version)
			if err != nil {
				continue
			}
			idx.Entries = append(idx.Entries, bundle.EntryFor(b, b.CreatedAt, b.CreatedBy, ""))
		}
	}
	return idx, nil
}

// writeIndexLocked normalises, flags curated entries, persists and caches.
func (st *marketplaceStore) writeIndexLocked(idx *bundle.Index, now time.Time) error {
	marketplaceNormaliseIndex(idx)
	idx.GeneratedAt = now.UTC()
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := marketplaceWriteAtomic(st.indexPath(), data); err != nil {
		return err
	}
	st.setIndexLocked(idx, data)
	return nil
}

func (st *marketplaceStore) setIndexLocked(idx *bundle.Index, raw []byte) {
	if idx.Entries == nil {
		idx.Entries = []bundle.IndexEntry{}
	}
	st.idx = idx
	st.idxRaw = raw
	sum := sha256.Sum256(raw)
	st.idxETag = `"` + hex.EncodeToString(sum[:]) + `"`
}

// marketplaceNormaliseIndex sorts entries (name, then publish time) and
// recomputes newerIngestAvailable: a curated entry is flagged when any
// ingested-unreviewed entry of the same name was published after it.
func marketplaceNormaliseIndex(idx *bundle.Index) {
	sort.SliceStable(idx.Entries, func(i, j int) bool {
		a, b := idx.Entries[i], idx.Entries[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if !a.PublishedAt.Equal(b.PublishedAt) {
			return a.PublishedAt.Before(b.PublishedAt)
		}
		return a.Version < b.Version
	})
	latestIngest := map[string]time.Time{}
	for _, e := range idx.Entries {
		if e.Tier == bundle.TierIngestedUnreviewed && e.PublishedAt.After(latestIngest[e.Name]) {
			latestIngest[e.Name] = e.PublishedAt
		}
	}
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if len(e.Adoption) == 0 {
			e.Adoption = bundle.EmptyObject
		}
		if len(e.Ratings) == 0 {
			e.Ratings = bundle.EmptyObject
		}
		if len(e.Benchmarks) == 0 {
			e.Benchmarks = bundle.EmptyObject
		}
		e.NewerIngestAvailable = e.Tier == bundle.TierCurated && latestIngest[e.Name].After(e.PublishedAt)
	}
}

// marketplaceRecordFormat versions the refresh record. Bumping it makes
// every existing record due at the next startup pass, which is how a fix to
// the ingest itself (not a change in the repository) gets re-crawled:
//
//	2 — tarball extraction recognises pax_global_header, so the pipeline
//	    files are actually read (format 1 recorded 0 files for most repos).
const marketplaceRecordFormat = 2

// marketplaceRefreshRecord is what one ingest run remembers about a repo.
type marketplaceRefreshRecord struct {
	Repo      string    `json:"repo"`
	Format    int       `json:"format,omitempty"`
	SourceSHA string    `json:"sourceSHA"`
	FilesHash string    `json:"filesHash"`
	Files     int       `json:"files"`
	CheckedAt time.Time `json:"checkedAt"`
	// Published is the name@version this run produced, or "" when the run
	// was a no-op or the repository had no pipelines.
	Published string `json:"published,omitempty"`
	Error     string `json:"error,omitempty"`
}

func marketplaceRepoSlug(repo string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(repo)), "/", "__")
}

func (st *marketplaceStore) refreshRecord(repo string) (marketplaceRefreshRecord, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	data, err := os.ReadFile(filepath.Join(st.refreshDir(), marketplaceRepoSlug(repo)+".json"))
	if err != nil {
		return marketplaceRefreshRecord{}, false
	}
	var rec marketplaceRefreshRecord
	if json.Unmarshal(data, &rec) != nil {
		return marketplaceRefreshRecord{}, false
	}
	return rec, true
}

func (st *marketplaceStore) saveRefreshRecord(rec marketplaceRefreshRecord) error {
	rec.Format = marketplaceRecordFormat
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return marketplaceWriteAtomic(filepath.Join(st.refreshDir(), marketplaceRepoSlug(rec.Repo)+".json"), data)
}

func (st *marketplaceStore) lastRefreshPath() string {
	return filepath.Join(st.root, "last-refresh.json")
}

// saveLastRefresh persists a run summary so /api/marketplace/status can
// report it across hub restarts.
func (st *marketplaceStore) saveLastRefresh(res *marketplaceRefreshResult) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return marketplaceWriteAtomic(st.lastRefreshPath(), data)
}

// loadLastRefresh returns the persisted run summary, or nil when none.
func (st *marketplaceStore) loadLastRefresh() *marketplaceRefreshResult {
	st.mu.RLock()
	defer st.mu.RUnlock()
	data, err := os.ReadFile(st.lastRefreshPath())
	if err != nil {
		return nil
	}
	var res marketplaceRefreshResult
	if json.Unmarshal(data, &res) != nil || res.StartedAt.IsZero() {
		return nil
	}
	return &res
}

// refreshRecords lists every recorded run.
func (st *marketplaceStore) refreshRecords() []marketplaceRefreshRecord {
	st.mu.RLock()
	defer st.mu.RUnlock()
	files, err := os.ReadDir(st.refreshDir())
	if err != nil {
		return nil
	}
	var out []marketplaceRefreshRecord
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(st.refreshDir(), f.Name()))
		if err != nil {
			continue
		}
		var rec marketplaceRefreshRecord
		if json.Unmarshal(data, &rec) == nil {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

// marketplaceFilesHash is a stable digest of a pipeline-file set: sorted
// path + sha256(content) pairs, hashed again.
func marketplaceFilesHash(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		fmt.Fprintf(h, "%s\x00%x\n", p, sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func marketplaceWriteAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
