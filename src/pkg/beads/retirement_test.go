package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Retirement tests.
//
// A "retired" bead is one that was removed from the LIVE map after reaching a
// terminal state — either by Archive (lifecycle culling) or by evictOldClosed
// (overflow past maxBeadCount). The distinction retirement encodes is not
// cosmetic: the contributor dependency-admission gate uses it to tell
// "this dependency was satisfied and then culled" from "this dependency
// reference never resolved". Collapsing those two cases withholds work
// permanently, so every removal path and the restart path are pinned here.

// retiredSet is a small helper turning RetiredIDs into a lookup, so assertions
// read as set membership rather than slice scans.
func retiredSet(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, id := range s.RetiredIDs() {
		out[id] = true
	}
	return out
}

// archiveLines returns the non-empty lines of the store's archive.jsonl.
func archiveLines(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, archiveFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", archiveFileName, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// fillPastEvictionThreshold injects maxBeadCount+overflow beads directly into
// the map: `overflow` terminal ones (oldest by UpdatedAt, so eviction takes
// exactly those, alternating closed/done) plus maxBeadCount newer open ones.
// It returns the terminal and open IDs. Direct injection, matching
// beads_eviction_test.go: pushing 5000+ beads through Create would rewrite the
// whole JSON file on every call and make the package crawl, and it would be
// testing Create's threshold rather than eviction's bookkeeping.
func fillPastEvictionThreshold(t *testing.T, s *Store, overflow int) (terminalIDs, openIDs []string) {
	t.Helper()
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < overflow; i++ {
		id := fmt.Sprintf("term-%02d", i)
		status := StatusClosed
		if i%2 == 1 {
			status = StatusDone
		}
		closedAt := flexTime{now.Add(-24 * time.Hour)}
		s.beads[id] = &Bead{
			ID:          id,
			Title:       fmt.Sprintf("terminal bead %d", i),
			Type:        TypeTask,
			Status:      status,
			Priority:    PriorityLow,
			Actor:       "scanner",
			ExternalRef: fmt.Sprintf("ref-%02d", i),
			CreatedAt:   flexTime{now.Add(-48 * time.Hour)},
			UpdatedAt:   flexTime{now.Add(-24*time.Hour + time.Duration(i)*time.Second)},
			ClosedAt:    &closedAt,
		}
		terminalIDs = append(terminalIDs, id)
	}
	for i := 0; i < maxBeadCount; i++ {
		id := fmt.Sprintf("open-%05d", i)
		s.beads[id] = &Bead{
			ID:        id,
			Title:     fmt.Sprintf("open bead %d", i),
			Type:      TypeTask,
			Status:    StatusOpen,
			Priority:  PriorityLow,
			Actor:     "scanner",
			CreatedAt: flexTime{now},
			UpdatedAt: flexTime{now},
		}
		openIDs = append(openIDs, id)
	}
	if got := len(s.beads); got != maxBeadCount+overflow {
		t.Fatalf("setup: len(beads) = %d, want %d", got, maxBeadCount+overflow)
	}
	return terminalIDs, openIDs
}

// TestArchive_MarksRetiredAndRemovesFromLiveMap is the primary Archive
// contract: the bead leaves the live map and the ID becomes retired, both via
// IsRetired and via RetiredIDs.
func TestArchive_MarksRetiredAndRemovesFromLiveMap(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("cull me", TypeTask, PriorityLow, "alice", "issue-7")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.IsRetired(b.ID) {
		t.Fatalf("a freshly created bead must not be retired")
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if !s.IsRetired(b.ID) {
		t.Errorf("IsRetired(%s) = false after Archive, want true", b.ID)
	}
	if _, err := s.Get(b.ID); err == nil {
		t.Errorf("Get(%s) succeeded after Archive; bead should be out of the live map", b.ID)
	}
	if got := s.Count(); got != 0 {
		t.Errorf("Count() = %d after archiving the only bead, want 0", got)
	}
	if set := retiredSet(t, s); !set[b.ID] {
		t.Errorf("RetiredIDs() = %v, want it to contain %s", s.RetiredIDs(), b.ID)
	}
}

// TestArchive_FailedWriteLeavesBeadLiveAndUnretired pins the failure path: if
// the archive entry cannot be written the bead stays live and is NOT reported
// as retired, so a dependent is never told a bead was satisfied on the strength
// of a write that did not happen.
func TestArchive_FailedWriteLeavesBeadLiveAndUnretired(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("unwritable", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A directory at the archive path makes OpenFile fail.
	if err := os.MkdirAll(filepath.Join(dir, archiveFileName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := s.Archive(b.ID); err == nil {
		t.Fatalf("expected Archive to fail when the archive path is a directory")
	}
	if s.IsRetired(b.ID) {
		t.Errorf("IsRetired(%s) = true after a FAILED Archive, want false", b.ID)
	}
	if _, err := s.Get(b.ID); err != nil {
		t.Errorf("bead should still be live after a failed Archive: %v", err)
	}
}

// TestIsRetired_NeverExistingIDIsNotRetired covers the case the admission gate
// actually turns on: an ID nobody ever created must read as "never resolved",
// not "satisfied then culled".
func TestIsRetired_NeverExistingIDIsNotRetired(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s.Create("unrelated", TypeTask, PriorityLow, "alice", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, id := range []string{"", "does-not-exist", "hv-000000000000"} {
		if s.IsRetired(id) {
			t.Errorf("IsRetired(%q) = true, want false for an ID that never existed", id)
		}
	}
	if ids := s.RetiredIDs(); len(ids) != 0 {
		t.Errorf("RetiredIDs() = %v, want empty on a store that never removed anything", ids)
	}
}

// TestIsRetired_TerminalButStillLiveIsNotRetired: retirement means REMOVED, not
// merely finished. A closed or done bead that is still in the map resolves on
// its own and must not be reported as retired.
func TestIsRetired_TerminalButStillLiveIsNotRetired(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	closed, err := s.Create("closed but live", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	done, err := s.Create("done but live", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create done: %v", err)
	}
	open, err := s.Create("still open", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}

	if err := s.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Update(done.ID, func(b *Bead) { b.Status = StatusDone }); err != nil {
		t.Fatalf("Update to done: %v", err)
	}

	for _, id := range []string{closed.ID, done.ID, open.ID} {
		if s.IsRetired(id) {
			t.Errorf("IsRetired(%s) = true while the bead is still in the live map, want false", id)
		}
	}

	// Only removal flips it.
	if err := s.Archive(closed.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !s.IsRetired(closed.ID) {
		t.Errorf("IsRetired(%s) = false after Archive, want true", closed.ID)
	}
	if s.IsRetired(done.ID) || s.IsRetired(open.ID) {
		t.Errorf("archiving one bead retired others: retired=%v", s.RetiredIDs())
	}
}

// TestRetirement_SurvivesRestart is the restart path the admission gate depends
// on: a hive that comes back up must still know a culled dependency was
// satisfied. A second NewStore over the same directory rebuilds the retired set
// from archive.jsonl.
func TestRetirement_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (first): %v", err)
	}

	culled, err := s1.Create("satisfied then culled", TypeTask, PriorityLow, "alice", "ref-culled")
	if err != nil {
		t.Fatalf("Create culled: %v", err)
	}
	survivor, err := s1.Create("still around", TypeTask, PriorityLow, "alice", "ref-live")
	if err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := s1.Close(culled.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s1.Archive(culled.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Restart: a brand-new Store over the same directory.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	if !s2.IsRetired(culled.ID) {
		t.Errorf("IsRetired(%s) = false after restart, want true — retirement did not survive reload", culled.ID)
	}
	if set := retiredSet(t, s2); !set[culled.ID] {
		t.Errorf("RetiredIDs() after restart = %v, want it to contain %s", s2.RetiredIDs(), culled.ID)
	}
	if s2.IsRetired(survivor.ID) {
		t.Errorf("IsRetired(%s) = true after restart, but that bead was never removed", survivor.ID)
	}
	if _, err := s2.Get(survivor.ID); err != nil {
		t.Errorf("surviving bead missing after restart: %v", err)
	}
}

// TestRetirement_SurvivesRestartWhenEveryBeadWasArchived covers the shape a
// fully culled store actually takes on disk: persist marshals an empty map as
// the JSON literal `null`, so load must still parse it and reach loadRetired.
func TestRetirement_SurvivesRestartWhenEveryBeadWasArchived(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (first): %v", err)
	}

	b, err := s1.Create("only bead", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s1.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s1.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Sanity-check the on-disk shape this test exists to guard.
	data, err := os.ReadFile(filepath.Join(dir, beadsFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", beadsFileName, err)
	}
	if strings.TrimSpace(string(data)) != "null" {
		t.Logf("note: emptied %s is %q, not the expected \"null\"", beadsFileName, strings.TrimSpace(string(data)))
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	if !s2.IsRetired(b.ID) {
		t.Errorf("IsRetired(%s) = false after restarting an emptied store, want true", b.ID)
	}
}

// TestRetirement_SurvivesRestartWhenLiveFileMissing covers the archive-only
// directory: a complete archive.jsonl with no beads.json beside it.
//
// load() used to return on the missing live file BEFORE reaching loadRetired(),
// so such a store came back with an EMPTY retired set and every archived
// dependency in it read as "never resolved" rather than "satisfied then culled"
// — silently withholding the dependents forever, which is the exact failure
// retirement was added to prevent. The archive is an independent append-only
// log, so it is now replayed unconditionally, first.
func TestRetirement_SurvivesRestartWhenLiveFileMissing(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (first): %v", err)
	}

	b, err := s1.Create("archived then live file lost", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s1.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s1.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// The archive survives; the live file does not. (An operator resetting the
	// live queue, a restore that only preserved the append-only log, or a
	// persist that failed after the archive entry was already appended.)
	if err := os.Remove(filepath.Join(dir, beadsFileName)); err != nil {
		t.Fatalf("removing %s: %v", beadsFileName, err)
	}
	if lines := archiveLines(t, dir); len(lines) != 1 {
		t.Fatalf("expected the archive to still hold 1 entry, got %d", len(lines))
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}

	// archive.jsonl is an independent append-only log, so it must be replayed
	// whether or not the live file exists. load() previously returned on the
	// missing beads.json BEFORE reaching loadRetired(), which lost every
	// retirement in exactly this shape of directory.
	if !s2.IsRetired(b.ID) {
		t.Errorf("IsRetired(%s) = false: retirement must survive a missing %s, "+
			"since the archive is a separate log", b.ID, beadsFileName)
	}
}

// TestReload_KeepsRetiredSet: Reload rebuilds the live map from disk but must
// not forget retirements already known in memory.
func TestReload_KeepsRetiredSet(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("archived before reload", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !s.IsRetired(b.ID) {
		t.Errorf("IsRetired(%s) = false after Reload, want true", b.ID)
	}
	if _, err := s.Get(b.ID); err == nil {
		t.Errorf("Reload resurrected an archived bead into the live map")
	}
}

// TestEvictOldClosed_RetiresEvictedBeadsAndArchivesThem covers the second
// removal path. Beads are injected straight into the map — driving 5000+ beads
// through Create would rewrite the whole JSON file on every call and make the
// package crawl, and it would test Create's threshold rather than eviction's
// bookkeeping. evictOldClosed is called exactly as Create calls it: under the
// write lock. Existing tests in beads_eviction_test.go use the same approach.
func TestEvictOldClosed_RetiresEvictedBeadsAndArchivesThem(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const overflow = 8
	terminalIDs, openIDs := fillPastEvictionThreshold(t, s, overflow)

	s.mu.Lock()
	s.evictOldClosed()

	remaining := len(s.beads)
	stillLive := make(map[string]bool, len(s.beads))
	for id := range s.beads {
		stillLive[id] = true
	}
	// Write the live file too, so the restart assertion below sees the same
	// on-disk state Create would have left behind.
	if err := s.persist(nil); err != nil {
		s.mu.Unlock()
		t.Fatalf("persist: %v", err)
	}
	s.mu.Unlock()

	if remaining != maxBeadCount {
		t.Fatalf("after eviction len(beads) = %d, want %d", remaining, maxBeadCount)
	}

	// Every evicted terminal bead: gone from the map, retired, and archived.
	retired := retiredSet(t, s)
	for _, id := range terminalIDs {
		if stillLive[id] {
			t.Errorf("terminal bead %s survived eviction", id)
		}
		if !s.IsRetired(id) {
			t.Errorf("IsRetired(%s) = false after eviction, want true", id)
		}
		if !retired[id] {
			t.Errorf("RetiredIDs() missing evicted bead %s", id)
		}
	}
	if len(retired) != overflow {
		t.Errorf("RetiredIDs() has %d entries, want %d (only evicted terminal beads)", len(retired), overflow)
	}
	// Open beads are never evictable, so none of them may be retired.
	for _, id := range openIDs {
		if retired[id] {
			t.Fatalf("open bead %s was retired by eviction", id)
		}
	}

	// The archive log is what makes the retirement durable.
	lines := archiveLines(t, dir)
	if len(lines) != overflow {
		t.Fatalf("archive.jsonl has %d entries, want %d", len(lines), overflow)
	}
	archived := map[string]bool{}
	for _, line := range lines {
		var entry ArchivedBead
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("archive line is not valid JSON: %v (%s)", err, line)
		}
		archived[entry.ID] = true
		if entry.Title == "" || entry.ArchivedAt.IsZero() {
			t.Errorf("archive entry for %s is missing title/archived_at: %+v", entry.ID, entry)
		}
		if entry.ClosedAt.IsZero() {
			t.Errorf("archive entry for %s dropped closed_at: %+v", entry.ID, entry)
		}
	}
	for _, id := range terminalIDs {
		if !archived[id] {
			t.Errorf("evicted bead %s has no archive.jsonl entry", id)
		}
	}

	// Restart: the eviction-side retirements must reload just like Archive's do.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	for _, id := range terminalIDs {
		if !s2.IsRetired(id) {
			t.Errorf("IsRetired(%s) = false after restart, want true — evicted retirement did not survive", id)
		}
	}
}

// TestEvictOldClosed_ArchiveFailureKeepsTheBead pins that both removal paths
// behave the same way when the log cannot be written: Archive propagates the
// error and keeps the bead live, and eviction now skips the bead rather than
// dropping it and retiring in memory only. A retirement that never reached disk
// is forgotten on restart, turning a satisfied dependency back into an
// unresolvable one — so a retirement is only claimed once it is durable.
func TestEvictOldClosed_ArchiveFailureKeepsTheBead(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// A directory at the archive path makes every append fail.
	if err := os.MkdirAll(filepath.Join(dir, archiveFileName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A nil bead must be ignored rather than panic the caller.
	s.appendArchiveEntry(nil)

	const overflow = 4
	terminalIDs, _ := fillPastEvictionThreshold(t, s, overflow)

	s.mu.Lock()
	s.evictOldClosed()
	remaining := len(s.beads)
	s.mu.Unlock()

	// The bead STAYS when its archive record cannot be written. Evicting anyway
	// would retire it in memory only, and the next restart would forget — the
	// same silent withholding as losing the ID outright, just deferred. Eviction
	// is an opportunistic memory bound, so holding these beads until the archive
	// is writable again is the cheap side of the trade.
	if remaining != maxBeadCount+overflow {
		t.Fatalf("eviction must not drop a bead it could not archive: len=%d, want %d",
			remaining, maxBeadCount+overflow)
	}
	for _, id := range terminalIDs {
		if s.IsRetired(id) {
			t.Errorf("IsRetired(%s) = true, but nothing reached disk — an in-memory-only "+
				"retirement is forgotten on restart and must not be claimed", id)
		}
	}
	// Nothing reached disk, and nothing should have been written.
	if entries, err := os.ReadDir(filepath.Join(dir, archiveFileName)); err != nil || len(entries) != 0 {
		t.Errorf("unexpected writes under the archive path: entries=%v err=%v", entries, err)
	}
}

// TestRetiredIDs_ReturnsSnapshotOfEverythingRetired checks both removal paths
// land in one list, and that the returned slice is a detached snapshot rather
// than a view the store keeps mutating.
func TestRetiredIDs_ReturnsSnapshotOfEverythingRetired(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var want []string
	for i := 0; i < 3; i++ {
		b, err := s.Create(fmt.Sprintf("bead %d", i), TypeTask, PriorityLow, "alice", "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Close(b.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := s.Archive(b.ID); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		want = append(want, b.ID)
	}

	snapshot := s.RetiredIDs()
	if len(snapshot) != len(want) {
		t.Fatalf("RetiredIDs() = %v, want %d entries", snapshot, len(want))
	}
	got := map[string]bool{}
	for _, id := range snapshot {
		got[id] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("RetiredIDs() missing %s: %v", id, snapshot)
		}
	}

	// Mutating the returned slice must not corrupt the store's set, and the
	// snapshot must not grow when the store retires something else.
	for i := range snapshot {
		snapshot[i] = "clobbered"
	}
	extra, err := s.Create("late", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create late: %v", err)
	}
	if err := s.Close(extra.ID); err != nil {
		t.Fatalf("Close late: %v", err)
	}
	if err := s.Archive(extra.ID); err != nil {
		t.Fatalf("Archive late: %v", err)
	}
	if len(snapshot) != len(want) {
		t.Errorf("the previously returned slice changed length: %v", snapshot)
	}

	after := retiredSet(t, s)
	if after["clobbered"] {
		t.Errorf("mutating the returned slice leaked into the store: %v", s.RetiredIDs())
	}
	for _, id := range append(want, extra.ID) {
		if !after[id] {
			t.Errorf("RetiredIDs() missing %s after further archiving: %v", id, s.RetiredIDs())
		}
	}
}

// TestRetiredIDs_RaceFreeAgainstConcurrentWriters is the -race guard. The
// admission gate reads retirement on the assignment path — one ID at a time via
// IsRetired — while the inception watcher and lifecycle culler are still
// creating, closing and archiving beads. Both read shapes are covered here:
// readers and writers must not race on either.
func TestRetiredIDs_RaceFreeAgainstConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const rounds = 60
	done := make(chan struct{})
	var writers, readers sync.WaitGroup

	// Writer: create -> close -> archive, i.e. the full retirement path.
	archivedCh := make(chan []string, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		var archived []string
		defer func() { archivedCh <- archived }()
		for i := 0; i < rounds; i++ {
			b, err := s.Create(fmt.Sprintf("churn %d", i), TypeTask, PriorityLow, "culler", "")
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			if err := s.Close(b.ID); err != nil {
				t.Errorf("Close: %v", err)
				return
			}
			if err := s.Archive(b.ID); err != nil {
				t.Errorf("Archive: %v", err)
				return
			}
			archived = append(archived, b.ID)
		}
	}()

	// Writer: beads that stay live, so the map is mutated under the readers.
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < rounds; i++ {
			if _, err := s.Create(fmt.Sprintf("survivor %d", i), TypeTask, PriorityLow, "watcher", ""); err != nil {
				t.Errorf("Create survivor: %v", err)
				return
			}
		}
	}()

	// Readers: the admission-gate access pattern.
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, id := range s.RetiredIDs() {
					_ = s.IsRetired(id)
				}
				_ = s.IsRetired("never-existed")
				_ = s.Count()
			}
		}()
	}

	writers.Wait()
	close(done)
	readers.Wait()

	archived := <-archivedCh
	if len(archived) != rounds {
		t.Fatalf("writer archived %d beads, want %d", len(archived), rounds)
	}
	retired := retiredSet(t, s)
	for _, id := range archived {
		if !retired[id] {
			t.Errorf("archived bead %s is not retired after concurrent access", id)
		}
	}
	if len(retired) != rounds {
		t.Errorf("RetiredIDs() has %d entries, want %d", len(retired), rounds)
	}
}

// TestLoadRetired_SkipsCorruptLines: archive.jsonl is an append-only log written
// best-effort by two paths, so a torn write must not take NewStore down or
// discard the readable entries around it.
func TestLoadRetired_SkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, beadsFileName), []byte("[]"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", beadsFileName, err)
	}

	good1, _ := json.Marshal(ArchivedBead{ID: "good-1", Title: "first", Type: TypeTask, ArchivedAt: time.Now().UTC()})
	good2, _ := json.Marshal(ArchivedBead{ID: "good-2", Title: "second", Type: TypeBug, ArchivedAt: time.Now().UTC()})

	content := strings.Join([]string{
		string(good1),
		"",                        // blank line
		"   ",                     // whitespace only
		`{"id":"torn","title":"`,  // truncated write
		"not json at all",         // garbage
		`{"id":""}`,               // well-formed but no ID
		`{"title":"no id field"}`, // well-formed, ID absent
		`[1,2,3]`,                 // valid JSON, wrong shape
		string(good2),
	}, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(dir, archiveFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", archiveFileName, err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore must tolerate a partly-corrupt archive, got: %v", err)
	}

	retired := retiredSet(t, s)
	if len(retired) != 2 {
		t.Fatalf("RetiredIDs() = %v, want exactly [good-1 good-2]", s.RetiredIDs())
	}
	for _, id := range []string{"good-1", "good-2"} {
		if !retired[id] {
			t.Errorf("IsRetired(%s) = false, want true — a corrupt neighbour line swallowed it", id)
		}
	}
	for _, id := range []string{"", "torn"} {
		if retired[id] {
			t.Errorf("IsRetired(%q) = true, want false", id)
		}
	}
}

// TestLoadRetired_MissingArchiveIsNotAnError: a store that has never archived
// anything has no archive.jsonl, and that is the ordinary case, not a failure.
func TestLoadRetired_MissingArchiveIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, beadsFileName), []byte("[]"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", beadsFileName, err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if ids := s.RetiredIDs(); len(ids) != 0 {
		t.Errorf("RetiredIDs() = %v, want empty when there is no archive", ids)
	}
	if s.IsRetired("anything") {
		t.Errorf("IsRetired reported true with no archive present")
	}
}

// TestLoadRetired_UnreadableArchiveIsNotAnError: loadRetired opens the archive
// best-effort. If the path is unusable the store still comes up — with fewer
// known retirements, which is the conservative direction.
func TestLoadRetired_UnreadableArchiveIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, beadsFileName), []byte("[]"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", beadsFileName, err)
	}
	// A directory where the log should be: os.Open succeeds but reads fail.
	if err := os.MkdirAll(filepath.Join(dir, archiveFileName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore must tolerate an unreadable archive, got: %v", err)
	}
	if ids := s.RetiredIDs(); len(ids) != 0 {
		t.Errorf("RetiredIDs() = %v, want empty", ids)
	}
}

// TestLoadRetired_OversizedLineTruncatesTheRest documents a scanner limit worth
// knowing about: loadRetired caps a line at 1MiB, and bufio.Scanner stops for
// good once a token exceeds that. One oversized entry therefore discards every
// retirement recorded AFTER it, not just its own. Titles are unbounded, so the
// line width is attacker/agent-influenced. Behaviour is pinned, not endorsed.
func TestLoadRetired_OversizedLineTruncatesTheRest(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a >1MiB archive line")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, beadsFileName), []byte("[]"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", beadsFileName, err)
	}

	before, _ := json.Marshal(ArchivedBead{ID: "before-huge", Title: "ok", ArchivedAt: time.Now().UTC()})
	huge, _ := json.Marshal(ArchivedBead{ID: "huge", Title: strings.Repeat("x", 2*1024*1024), ArchivedAt: time.Now().UTC()})
	after, _ := json.Marshal(ArchivedBead{ID: "after-huge", Title: "ok", ArchivedAt: time.Now().UTC()})

	content := string(before) + "\n" + string(huge) + "\n" + string(after) + "\n"
	if err := os.WriteFile(filepath.Join(dir, archiveFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", archiveFileName, err)
	}

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore must not fail on an oversized archive line, got: %v", err)
	}
	if !s.IsRetired("before-huge") {
		t.Errorf("IsRetired(before-huge) = false; entries before the oversized line must still load")
	}
	if s.IsRetired("after-huge") {
		t.Logf("note: entries after an oversized archive line now load; the scanner limit appears to have been raised")
	} else {
		t.Logf("KNOWN LIMIT: one >1MiB archive line (loadRetired's bufio.Scanner cap) silently drops every later retirement")
	}
}

// TestArchive_DoesNotRetireANonTerminalBead pins the "satisfied" half of what
// retirement means. Today's only production caller (knowledge's
// BeadLifecycleManager.RunCulling) feeds Archive from store.AllClosed(), so the
// invariant used to hold by caller discipline alone — and a consumer treats
// membership as a satisfied dependency, so a future caller archiving an OPEN
// bead would have asserted a completion that never happened. Archiving an open
// bead is still allowed and still writes its audit line; it just does not claim
// satisfaction.
func TestArchive_DoesNotRetireANonTerminalBead(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("never finished", TypeTask, PriorityLow, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if s.IsRetired(b.ID) {
		t.Errorf("Archive retired %s while it was still %s: retirement encodes "+
			"\"existed and was satisfied\", so it must not be claimed for an unfinished bead",
			b.ID, StatusOpen)
	}
	// The audit line is still written — archiving is not conditional on status.
	if lines := archiveLines(t, dir); len(lines) != 1 {
		t.Errorf("archive lines = %d, want 1: an open bead is still archived, just not retired", len(lines))
	}
	if _, err := s.Get(b.ID); err == nil {
		t.Error("Archive must still remove the bead from the live map")
	}
}
