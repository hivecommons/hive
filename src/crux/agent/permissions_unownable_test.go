package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// These tests pin the #18120 fix: an entry owned by a uid the unprivileged
// watcher can never chown (a sandbox kick workspace written by the Job pod's
// uid, a root seed) is skipped BEFORE the syscall and reported as one summary
// line per tick, never as one WARN per path. On hive-tall-frog a surge of
// kicks minted ~4096 such paths and the per-path deduper warned once for each
// of them before saturating.

const unownableSummaryMsg = "permissions watcher: entries owned by another uid need a chown this process cannot perform; skipped without retrying"

// foreignOwnedEntry creates a regular file and returns a FileInfo that reports
// it as owned by a uid this process is not, with a gid that is not NodeGID, so
// fixEntry wants a chown it cannot perform. Skips as root, where every chown
// is possible and the skip arm is unreachable.
func foreignOwnedEntry(t *testing.T, dir, name string) (string, os.FileInfo) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: every chown is permitted, skip arm unreachable")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	foreign := uint32(os.Geteuid() + 1)
	return path, fakeOwnerInfo{FileInfo: fi, sys: syntheticStat(t, path, foreign, uint32(NodeGID)+1)}
}

func TestCannotChownUnprivilegedForeignOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chown is always permitted")
	}
	me := uint32(os.Geteuid())
	if cannotChown(me) {
		t.Error("an entry this process owns must be attempted (group change may succeed)")
	}
	if !cannotChown(me + 1) {
		t.Error("an entry another uid owns can never be chowned without CAP_CHOWN")
	}
	if !cannotChown(0) {
		t.Error("a root-owned entry can never be chowned without CAP_CHOWN")
	}
}

// TestFixEntryForeignOwnerSkippedWithoutPerPathWarn is the core regression
// test: many foreign-owned entries across several ticks produce ZERO per-path
// chown WARN lines and leave the mode untouched (0600 stays 0600: the entry is
// not ours to widen either).
func TestFixEntryForeignOwnerSkippedWithoutPerPathWarn(t *testing.T) {
	resetPermWarnDedupe()
	resetPermUnownable()
	t.Cleanup(resetPermWarnDedupe)
	t.Cleanup(resetPermUnownable)

	dir := t.TempDir()
	const paths = 50
	h := &recordingHandler{}
	logger := slog.New(h)
	var first string
	for i := range paths {
		path, fi := foreignOwnedEntry(t, dir, "skill-"+strconv.Itoa(i))
		if i == 0 {
			first = path
		}
		fixEntry(path, fi, logger)
	}

	if got := h.count(slog.LevelWarn, "permissions watcher: chown failed", ""); got != 0 {
		t.Errorf("per-path chown WARN logged %d times for unownable entries, want 0", got)
	}
	if got := h.count(slog.LevelDebug, "permissions watcher: chown failed", ""); got != 0 {
		t.Errorf("per-path chown DEBUG logged %d times: the chown must not be attempted at all", got)
	}
	if got := h.count(slog.LevelWarn, "permissions watcher: chmod file failed", ""); got != 0 {
		t.Errorf("chmod attempted on %d entries we do not own, want 0", got)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode of foreign-owned entry changed to %v, want 0600 untouched", info.Mode().Perm())
	}

	permUnownable.mu.Lock()
	count, sample := permUnownable.count, permUnownable.sample
	permUnownable.mu.Unlock()
	if count != paths {
		t.Errorf("tally counted %d skipped entries, want %d", count, paths)
	}
	if sample != first {
		t.Errorf("tally sample = %q, want the first skipped path %q", sample, first)
	}
}

// TestUnownableTallyOneSummaryPerTickAtMost pins the summary cadence: WARN
// when the condition appears, DEBUG while it holds unchanged, WARN again only
// when it grows, nothing when the tick saw none, and WARN once more when the
// condition clears and then reappears.
func TestUnownableTallyOneSummaryPerTickAtMost(t *testing.T) {
	u := &unownableTally{}
	h := &recordingHandler{}
	logger := slog.New(h)

	tick := func(n int) bool {
		for i := range n {
			u.skipped("/data/agents/sandbox/quality-1/.claude/skills/" + strconv.Itoa(i))
		}
		return u.flush(logger)
	}

	if tick(0) {
		t.Error("an empty tick must not warn")
	}
	if !tick(4087) {
		t.Error("first tick with unownable entries must warn")
	}
	if tick(4087) {
		t.Error("unchanged count on the next tick must be demoted to debug")
	}
	if tick(10) {
		t.Error("a smaller count must not warn")
	}
	if !tick(5000) {
		t.Error("a count above the high-water mark is new information and must warn")
	}
	if tick(0) {
		t.Error("a clear tick must not warn")
	}
	if !tick(1) {
		t.Error("reappearance after the condition cleared must warn again")
	}

	if got := h.count(slog.LevelWarn, unownableSummaryMsg, ""); got != 3 {
		t.Errorf("summary WARN logged %d times across 7 ticks, want 3", got)
	}
	if got := h.count(slog.LevelDebug, "permissions watcher: entries owned by another uid skipped (unchanged)", ""); got != 2 {
		t.Errorf("summary DEBUG logged %d times, want 2", got)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.count != 0 || u.sample != "" {
		t.Errorf("flush left count=%d sample=%q, want reset", u.count, u.sample)
	}
	if u.highWater != 1 {
		t.Errorf("highWater = %d after the final tick of 1, want 1", u.highWater)
	}
}

// TestUnownableTallySummaryCarriesCountAndSample pins the attrs an operator
// needs to act on the one line they get.
func TestUnownableTallySummaryCarriesCountAndSample(t *testing.T) {
	u := &unownableTally{}
	h := &recordingHandler{}
	u.skipped("/data/agents/sandbox/quality-1/.claude/skills/upgrade-vllm")
	u.skipped("/data/agents/sandbox/quality-1/.claude/skills/other")
	u.flush(slog.New(h))

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		t.Fatalf("got %d records, want 1", len(h.records))
	}
	attrs := map[string]string{}
	h.records[0].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	if attrs["count"] != "2" {
		t.Errorf("count attr = %q, want 2", attrs["count"])
	}
	if attrs["sample_path"] != "/data/agents/sandbox/quality-1/.claude/skills/upgrade-vllm" {
		t.Errorf("sample_path attr = %q, want the first skipped path", attrs["sample_path"])
	}
	if attrs["hint"] != unownableHint {
		t.Errorf("hint attr = %q, want unownableHint", attrs["hint"])
	}
}

// TestFixPermissionsFlushesUnownableSummary pins that a watcher tick ends by
// emitting the summary, so the tally does not accumulate across ticks.
func TestFixPermissionsFlushesUnownableSummary(t *testing.T) {
	resetPermWarnDedupe()
	resetPermUnownable()
	t.Cleanup(resetPermWarnDedupe)
	t.Cleanup(resetPermUnownable)

	h := &recordingHandler{}
	logger := slog.New(h)
	// Seed the tally as fixEntry would during the walk; the walk roots are
	// left at their production values, which do not exist here, so the tick
	// itself touches nothing and only the flush is observable.
	path, fi := foreignOwnedEntry(t, t.TempDir(), "upgrade-vllm")
	fixEntry(path, fi, logger)

	oldDirs, oldGlob, oldCaps, oldRepo := WatchedHomeDirs, ModeFileGlob, CapsFileGlob, SharedRepoParent
	WatchedHomeDirs = nil
	ModeFileGlob = filepath.Join(t.TempDir(), "none-*")
	CapsFileGlob = filepath.Join(t.TempDir(), "none-*")
	SharedRepoParent = t.TempDir()
	t.Cleanup(func() {
		WatchedHomeDirs, ModeFileGlob, CapsFileGlob, SharedRepoParent = oldDirs, oldGlob, oldCaps, oldRepo
	})

	fixPermissions(logger)
	if got := h.count(slog.LevelWarn, unownableSummaryMsg, ""); got != 1 {
		t.Errorf("summary WARN logged %d times after one tick, want 1", got)
	}
	permUnownable.mu.Lock()
	count := permUnownable.count
	permUnownable.mu.Unlock()
	if count != 0 {
		t.Errorf("tally count = %d after the tick flushed, want 0", count)
	}
}
