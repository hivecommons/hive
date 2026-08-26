package hub

import (
	"os"
	"sync"
	"time"
)

// ── meta.json read cache ─────────────────────────────────────────────────────
//
// Nearly every hub control loop and API handler walks the whole fleet through
// listSaaSHives()/loadSaaSHive(), and each walk used to open, read and parse
// every hive's meta.json from the PVC. At tens of hives that was invisible; at
// hundreds it is thousands of file reads per poller tick and per dashboard
// load, all against the same network-backed volume.
//
// This cache keeps the RAW BYTES of each meta.json in memory, keyed by file
// path and validated against the file's (mtime, size) on every access. Hits
// skip the read entirely; the caller still unmarshals a fresh SaaSHive from
// the cached bytes, so every caller keeps today's semantics exactly — an
// independent, mutation-safe copy — and no deep-copy code has to be maintained
// as fields are added to the struct.
//
// Deliberately NOT a parsed-struct cache: SaaSHive carries slices and pointers
// (Repos, IssueFilter, PendingAppConfig, PendingRequests), so handing the same
// parsed value to concurrent callers would let one caller's in-place mutation
// taint every other reader. Unmarshal-per-access costs microseconds and buys
// total isolation.
//
// Freshness:
//   - The hub's own writes always land here via storeHiveMeta (called by
//     saveSaaSHive after the tmp+rename), so hub-written state is never stale.
//   - External/direct writes (tests writing meta.json with os.WriteFile) are
//     caught by the stat check: a changed mtime or size is a miss and the file
//     is re-read. The stat is taken BEFORE the read, so if a writer replaces
//     the file between stat and read we record the OLD mtime with the NEW
//     bytes — the next access sees a newer mtime, misses, and re-reads. The
//     failure direction is an extra read, never stale data.
//   - A stat failure (file deleted) evicts the entry, so removed hives do not
//     linger in memory.
type hiveMetaEntry struct {
	mtime time.Time
	size  int64
	data  []byte
}

var (
	hiveMetaMu    sync.RWMutex
	hiveMetaCache = map[string]hiveMetaEntry{}
)

// readHiveMeta returns the current contents of the meta.json at path, serving
// from the cache when the file is unchanged since it was last read.
func readHiveMeta(path string) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		evictHiveMeta(path)
		return nil, false
	}
	hiveMetaMu.RLock()
	e, ok := hiveMetaCache[path]
	hiveMetaMu.RUnlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.data, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		evictHiveMeta(path)
		return nil, false
	}
	hiveMetaMu.Lock()
	hiveMetaCache[path] = hiveMetaEntry{mtime: fi.ModTime(), size: fi.Size(), data: data}
	hiveMetaMu.Unlock()
	return data, true
}

// storeHiveMeta records bytes just written to path so the hub's own saves are
// immediate cache hits. Called after the tmp+rename in saveSaaSHive.
func storeHiveMeta(path string, data []byte) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	hiveMetaMu.Lock()
	hiveMetaCache[path] = hiveMetaEntry{mtime: fi.ModTime(), size: fi.Size(), data: append([]byte(nil), data...)}
	hiveMetaMu.Unlock()
}

// evictHiveMeta drops the cache entry for path (deleted or unreadable hives).
func evictHiveMeta(path string) {
	hiveMetaMu.Lock()
	delete(hiveMetaCache, path)
	hiveMetaMu.Unlock()
}
