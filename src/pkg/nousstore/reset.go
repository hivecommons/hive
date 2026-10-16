package nousstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Reset (RFC #11585).
//
// An owner can throw away what the Experiment Lab has collected without
// touching what they configured. Three scopes, each a directory under the
// root, so a reset is a directory move rather than a file-by-file delete:
//
//	baseline   <root>/snapshots/   every baseline sample
//	knowledge  <root>/governor/    the ledger and the learned principles
//	gates      <root>/gates/       the pending gate, decision and history
//
// panel-config.json (mode, scope, goals, …) is never reset here: it is
// configuration the owner typed, not data the lab collected.
//
// Each reset renames the directory into <root>/.trash/<stamp>/<name> and
// recreates it empty, so a crash half-way can never leave a partially
// deleted tree: either the rename happened (the live dir is gone and the
// old content is intact in the trash) or it did not. The trash keeps only
// the newest entry; everything older is removed after each reset.
const (
	// TrashSubdir holds the previous content of reset directories.
	TrashSubdir = ".trash"

	// trashStampLayout orders trash entries lexically by time.
	trashStampLayout = "20060102T150405.000000000Z"

	// trashKeep is how many trash entries survive a prune: the newest one.
	trashKeep = 1
)

// rename is os.Rename, replaceable by tests to inject a mid-reset failure.
var rename = os.Rename

// emptyLedger and emptyPrinciples are what the knowledge files hold after a
// reset: valid, empty Nous documents, so the durable governor dir still
// counts as populated (ResolveGovernorDir never falls back to the legacy
// tmpfs copy) and every reader parses them without a special case.
var (
	emptyLedger     = []byte("{\n  \"iterations\": []\n}\n")
	emptyPrinciples = []byte("{\n  \"principles\": []\n}\n")
)

// TrashDir is <root>/.trash.
func (l Layout) TrashDir() string { return filepath.Join(l.Root, TrashSubdir) }

// ResetScope selects which directories Reset clears.
type ResetScope struct {
	Baseline  bool
	Knowledge bool
	Gates     bool
}

// ResetResult counts what Reset removed.
type ResetResult struct {
	Snapshots     int
	LedgerEntries int
	Principles    int
}

// Reset clears every directory selected by scope, all under one trash
// stamp from now so a multi-scope reset lands in a single trash entry and
// the prune after each step never removes the step before. On error the
// counts so far are returned alongside.
func (l Layout) Reset(scope ResetScope, now time.Time) (ResetResult, error) {
	var res ResetResult
	if scope.Baseline {
		n, err := l.ResetBaseline(now)
		if err != nil {
			return res, err
		}
		res.Snapshots = n
	}
	if scope.Knowledge {
		ledger, principles, err := l.ResetKnowledge(now)
		if err != nil {
			return res, err
		}
		res.LedgerEntries, res.Principles = ledger, principles
	}
	if scope.Gates {
		if err := l.ResetGates(now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ResetBaseline moves every baseline snapshot aside and recreates an empty
// snapshot dir. It returns the number of snapshot files removed; a missing
// dir is zero, not an error.
func (l Layout) ResetBaseline(now time.Time) (int, error) {
	count, err := countFiles(l.SnapshotDir())
	if err != nil {
		return 0, err
	}
	if err := l.resetDir(l.SnapshotDir(), SnapshotsSubdir, now); err != nil {
		return 0, err
	}
	return count, nil
}

// ResetKnowledge moves the governor dir aside and recreates it holding an
// empty ledger and an empty principles file. It returns how many ledger
// entries and principles were removed; unparseable or missing files count
// as zero (the content is still moved to the trash, never lost).
func (l Layout) ResetKnowledge(now time.Time) (ledgerEntries, principles int, err error) {
	if entries, _, perr := readLedger(l.LedgerPath()); perr == nil {
		ledgerEntries = len(entries)
	}
	if ps, _, perr := readPrinciples(l.PrinciplesPath()); perr == nil {
		principles = len(ps)
	}
	if err := l.resetDir(l.GovernorDir(), GovernorSubdir, now); err != nil {
		return 0, 0, err
	}
	if err := writeAtomic(l.LedgerPath(), emptyLedger); err != nil {
		return 0, 0, err
	}
	if err := writeAtomic(l.PrinciplesPath(), emptyPrinciples); err != nil {
		return 0, 0, err
	}
	return ledgerEntries, principles, nil
}

// ResetGates moves the gate protocol files aside and recreates an empty
// gates dir.
func (l Layout) ResetGates(now time.Time) error {
	return l.resetDir(l.GatesDir(), GatesSubdir, now)
}

// resetDir renames dir into the trash entry for now, recreates dir empty
// and prunes older trash entries. A missing dir is simply recreated.
func (l Layout) resetDir(dir, name string, now time.Time) error {
	stamp := now.UTC().Format(trashStampLayout)
	entry := filepath.Join(l.TrashDir(), stamp)
	if err := os.MkdirAll(entry, dirMode); err != nil {
		return fmt.Errorf("create nous trash %s: %w", entry, err)
	}
	if _, err := os.Stat(dir); err == nil {
		if err := rename(dir, freeTrashName(entry, name)); err != nil {
			return fmt.Errorf("move %s to trash: %w", dir, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("recreate %s: %w", dir, err)
	}
	return l.pruneTrash(stamp)
}

// freeTrashName returns <entry>/<name>, or <entry>/<name>.N when a reset at
// the same stamp already moved a directory of that name there, so nothing
// in the trash is ever overwritten.
func freeTrashName(entry, name string) string {
	dst := filepath.Join(entry, name)
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			return dst
		}
		dst = filepath.Join(entry, fmt.Sprintf("%s.%d", name, i))
	}
}

// pruneTrash removes every trash entry except the trashKeep newest, never
// removing keep (the entry the current reset is writing into).
func (l Layout) pruneTrash(keep string) error {
	entries, err := os.ReadDir(l.TrashDir())
	if err != nil {
		return fmt.Errorf("list nous trash: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var errs []error
	for i, n := range names {
		if i < trashKeep || n == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.TrashDir(), n)); err != nil {
			errs = append(errs, fmt.Errorf("prune nous trash %s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

// countFiles counts the regular files directly inside dir; a missing dir
// is zero.
func countFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() {
			n++
		}
	}
	return n, nil
}

// Invalidate drops the reader's cache so the next Load re-reads both files
// even when their stat signature did not change — a reset that replaces a
// file with one of identical size within the same mtime tick must not be
// mistaken for "unchanged".
func (r *KnowledgeReader) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded = false
}
