package dashboard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// writeCacheTestBundle seals and writes one unsigned bundle for repo a/b#n.
func writeCacheTestBundle(t *testing.T, root string, n int) string {
	t.Helper()
	b := evSummaryBundle("a/b", n, evidence.AuthorAgent, time.Now().UTC().Add(-time.Hour), true, nil, 0, true)
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "a", "b", strconv.Itoa(n))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "abc1234.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestEvidenceBundleCacheReusesUnchangedFiles pins #19309: a second load does
// not re-read a file whose size and mtime are unchanged, re-reads one that
// changed, and forgets one that was removed.
func TestEvidenceBundleCacheReusesUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	path := writeCacheTestBundle(t, root, 1)
	writeCacheTestBundle(t, root, 2)
	c := &evidenceBundleCache{}
	v := ghpkg.ReviewEvidenceVerifier{}

	bundles, unreadable, err := c.load(root, v)
	if err != nil || len(bundles) != 2 || unreadable != 0 {
		t.Fatalf("first load: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}

	// Corrupt the file but keep its size and mtime: the cached parse is reused,
	// which proves the second walk did not decode it again.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(info.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	bundles, unreadable, err = c.load(root, v)
	if err != nil || len(bundles) != 2 || unreadable != 0 {
		t.Fatalf("unchanged size+mtime must reuse the cached bundle: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}

	// A changed mtime forces a re-read, which now fails verification.
	later := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	bundles, unreadable, err = c.load(root, v)
	if err != nil || len(bundles) != 1 || unreadable != 1 {
		t.Fatalf("changed file must be re-read: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}

	// A removed file is dropped from the result and the cache.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	bundles, unreadable, err = c.load(root, v)
	if err != nil || len(bundles) != 1 || unreadable != 0 {
		t.Fatalf("removed file must be forgotten: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}
	key := evidenceCacheKey{root: filepath.Clean(root), policy: v.PolicyKey()}
	if got := len(c.roots[key].files); got != 1 {
		t.Fatalf("cache holds %d files after removal, want 1", got)
	}
}

// TestEvidenceBundleCacheNewFileAppears pins that a bundle written after the
// first load is picked up by the next one.
func TestEvidenceBundleCacheNewFileAppears(t *testing.T) {
	root := t.TempDir()
	writeCacheTestBundle(t, root, 1)
	c := &evidenceBundleCache{}
	v := ghpkg.ReviewEvidenceVerifier{}
	if bundles, _, err := c.load(root, v); err != nil || len(bundles) != 1 {
		t.Fatalf("first load: bundles=%d err=%v", len(bundles), err)
	}
	writeCacheTestBundle(t, root, 2)
	if bundles, _, err := c.load(root, v); err != nil || len(bundles) != 2 {
		t.Fatalf("new bundle not picked up: bundles=%d err=%v", len(bundles), err)
	}
}

// TestEvidenceBundleCacheConcurrentLoads pins that concurrent loads of one
// root all see the same complete result (run under -race in CI).
func TestEvidenceBundleCacheConcurrentLoads(t *testing.T) {
	root := t.TempDir()
	const bundleCount = 5
	for i := 1; i <= bundleCount; i++ {
		writeCacheTestBundle(t, root, i)
	}
	c := &evidenceBundleCache{}
	v := ghpkg.ReviewEvidenceVerifier{}
	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bundles, unreadable, err := c.load(root, v)
			if err != nil || len(bundles) != bundleCount || unreadable != 0 {
				errs <- "concurrent load returned an incomplete result"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if len(c.inflight) != 0 {
		t.Fatalf("in-flight walks left behind: %d", len(c.inflight))
	}
}

// TestEvidenceBundleCacheMissingRoot pins that a root that does not exist yet
// is an empty result, not an error, as before the cache.
func TestEvidenceBundleCacheMissingRoot(t *testing.T) {
	c := &evidenceBundleCache{}
	bundles, unreadable, err := c.load(filepath.Join(t.TempDir(), "absent"), ghpkg.ReviewEvidenceVerifier{})
	if err != nil || len(bundles) != 0 || unreadable != 0 {
		t.Fatalf("missing root: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}
}
