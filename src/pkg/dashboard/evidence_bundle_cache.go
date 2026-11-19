package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// evidenceBundleCache memoises loadEvidenceBundles (hivecommons/hive#19309).
//
// Reading every bundle under /data/evidence means opening, decoding and
// re-hashing tens of thousands of files on the PVC. Done on every
// /api/evidence/summary call it took 80-160 s on a hive with ~25k bundles,
// past the ingress's 60 s timeout, so every Evidence card showed n=0.
//
// Each file's parse and verification result is kept by (size, mtime) under
// the verifier's policy key, so a later walk only stats files and re-reads the
// ones that are new or changed. Files that disappear are dropped. Concurrent
// loads of the same root and policy share one walk.
//
// Returned bundles are shared between callers and must be treated as
// read-only. Every dashboard consumer folds them into its own structs
// (evidence.FoldPRs copies, never writes back).
type evidenceBundleCache struct {
	mu       sync.Mutex
	roots    map[evidenceCacheKey]*evidenceRootCache
	inflight map[evidenceCacheKey]*evidenceLoadCall
}

type evidenceCacheKey struct {
	root   string
	policy string
}

type evidenceRootCache struct {
	files map[string]evidenceCachedFile
}

type evidenceCachedFile struct {
	size    int64
	modTime time.Time
	bundle  *evidence.Bundle // nil when the file was unreadable or failed verification
}

type evidenceLoadCall struct {
	done       chan struct{}
	bundles    []*evidence.Bundle
	unreadable int
	err        error
}

var evidenceBundles = &evidenceBundleCache{}

// load returns every verified bundle under root, joining a walk already in
// flight for the same root and policy instead of starting another.
func (c *evidenceBundleCache) load(root string, verifier ghpkg.ReviewEvidenceVerifier) ([]*evidence.Bundle, int, error) {
	key := evidenceCacheKey{root: filepath.Clean(root), policy: verifier.PolicyKey()}
	c.mu.Lock()
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.bundles, call.unreadable, call.err
	}
	if c.inflight == nil {
		c.inflight = map[evidenceCacheKey]*evidenceLoadCall{}
	}
	call := &evidenceLoadCall{done: make(chan struct{})}
	c.inflight[key] = call
	prev := c.roots[key]
	c.mu.Unlock()

	var prevFiles map[string]evidenceCachedFile
	if prev != nil {
		prevFiles = prev.files
	}
	files, bundles, unreadable, err := walkEvidenceBundles(root, verifier, prevFiles)
	call.bundles, call.unreadable, call.err = bundles, unreadable, err

	c.mu.Lock()
	if err == nil {
		if c.roots == nil {
			c.roots = map[evidenceCacheKey]*evidenceRootCache{}
		}
		// One policy per root: a key rotation drops the old policy's entries.
		for k := range c.roots {
			if k.root == key.root && k.policy != key.policy {
				delete(c.roots, k)
			}
		}
		c.roots[key] = &evidenceRootCache{files: files}
	}
	delete(c.inflight, key)
	c.mu.Unlock()
	close(call.done)
	return bundles, unreadable, err
}

// walkEvidenceBundles walks root (<owner>/<repo>/<pr>/<sha>.json), reusing
// prev's result for any file whose size and mtime are unchanged.
func walkEvidenceBundles(root string, verifier ghpkg.ReviewEvidenceVerifier, prev map[string]evidenceCachedFile) (map[string]evidenceCachedFile, []*evidence.Bundle, int, error) {
	files := make(map[string]evidenceCachedFile, len(prev))
	var out []*evidence.Bundle
	unreadable := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			// Reporting series share the root but are not review bundles.
			if path != root && evidenceSkipDir(root, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		entry, ok := evidenceCachedFile{}, false
		info, ierr := d.Info()
		if ierr == nil {
			if have, hit := prev[path]; hit && have.size == info.Size() && have.modTime.Equal(info.ModTime()) {
				entry, ok = have, true
			}
		}
		if !ok {
			b, lerr := ghpkg.LoadVerifiedReviewEvidence(path, verifier)
			if lerr != nil || b.SchemaVersion != evidence.SchemaVersion {
				b = nil
			}
			entry = evidenceCachedFile{bundle: b}
			if ierr == nil {
				entry.size, entry.modTime = info.Size(), info.ModTime()
			}
		}
		// A file that could not be stat'ed is read every time, never cached.
		if ierr == nil {
			files[path] = entry
		}
		if entry.bundle == nil {
			unreadable++
			return nil
		}
		out = append(out, entry.bundle)
		return nil
	})
	return files, out, unreadable, err
}

// evidenceSkipDir reports whether a directory under root holds a reporting
// series (complexity snapshots, <owner>/<repo>/<issue series>) rather than
// review bundles.
func evidenceSkipDir(root, path, name string) bool {
	if name == complexity.DirName {
		return true
	}
	rel, _ := filepath.Rel(root, path)
	return name == evidence.IssueSeriesDirName && len(strings.Split(rel, string(filepath.Separator))) == 3
}
