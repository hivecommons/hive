package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// lockFileName is the advisory cross-process lock guarding the
// load-modify-persist cycle on beads.json (#4742). It is a separate file, not
// beads.json itself, because persist replaces beads.json by rename — locking
// the data file would leave the lock attached to the orphaned inode.
const lockFileName = "beads.lock"

// lockAndRefresh serializes a mutation against every other process writing the
// same store (#4742). The store's RWMutex only covers goroutines inside ONE
// process, but each `bd` CLI invocation is its own process: two parallel
// `bd create` calls both snapshot beads.json, both rewrite it whole, and the
// loser's snapshot silently drops the winner's bead (last-writer-wins data
// loss in the findings ledger).
//
// It takes a blocking exclusive flock on beads.lock, then re-reads beads.json
// so the in-memory map includes everything concurrent processes persisted
// since our load. The caller applies its mutation and persists while still
// holding the lock, then calls the returned unlock. Must be called with s.mu
// held.
//
// Best-effort by design: if the lock file cannot be created or the filesystem
// does not support flock, the store degrades to the old unserialized behavior
// rather than refusing to record a finding.
func (s *Store) lockAndRefresh() func() {
	f, err := os.OpenFile(filepath.Join(s.dir, lockFileName), os.O_CREATE|os.O_RDWR, 0660)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}
	s.refreshFromDisk()
	return func() {
		// The flock is released implicitly by Close, and unconditionally by
		// process exit, so a crashed holder can never wedge the store.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// refreshFromDisk merges beads persisted by other processes into the in-memory
// map. Called under s.mu with the cross-process flock held, so the on-disk
// file is quiescent.
//
// Merge rules:
//   - on-disk bead we do not have: adopt it (another process created it),
//     unless we retired it ourselves.
//   - both sides have it and the on-disk copy is newer: take the on-disk copy
//     (another process updated it after our last persist).
//   - in-memory bead absent from disk that the (re-read) archive marks
//     retired: drop it — another process archived or evicted it, and
//     persisting our stale copy would resurrect it.
func (s *Store) refreshFromDisk() {
	data, err := os.ReadFile(filepath.Join(s.dir, beadsFileName))
	if err != nil {
		return
	}
	var onDisk []*Bead
	if err := json.Unmarshal(data, &onDisk); err != nil {
		return
	}

	// Pick up retirements recorded by other processes since our load: the
	// archive is append-only, so re-reading it only ever adds IDs.
	s.loadRetired()

	seen := make(map[string]bool, len(onDisk))
	for _, b := range onDisk {
		if b == nil || b.ID == "" {
			continue
		}
		seen[b.ID] = true
		cur, ok := s.beads[b.ID]
		if !ok {
			if s.retired[b.ID] {
				continue
			}
			s.beads[b.ID] = b
			continue
		}
		if b.UpdatedAt.After(cur.UpdatedAt.Time) {
			s.beads[b.ID] = b
		}
	}
	for id := range s.beads {
		if !seen[id] && s.retired[id] {
			delete(s.beads, id)
		}
	}
}
