package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/github"
)

// The merged ledger is the Activity Monitor's server-side record of every
// pull request the hive tracked that reached merged — whoever merged it
// (hivecommons/hive#15147, #15096). Before it, the toolbar's "Merged today" chip was
// accumulated in the tab from rows vanishing as merged and seeded from the
// in-memory audit ring on reload, so a refresh, a new tab or a roll of the
// pod reset it (the operator watched 4 become 0, and 33 become 4).
//
// Sources, all funnelled through recordMerged and deduplicated per PR:
//
//   - the audit log's pr_merged entries — the hive's own merges on every path
//     (RecordPRMergedAudit) and the PR poller's observations of human and
//     third-party-bot merges (the PRTerminalObservedHook in cmd/hive);
//   - webhook / closed-sweep snapshots of a PR GitHub reports merged
//     (applyWebhookItemSnapshots), which arrive minutes before the poller's
//     settle scan and carry the title and the merger's login.
//
// The file under /data survives a roll; entries are kept for
// mergedLedgerRetention (≥ 30 days) while the all-time total is a counter
// that only ever grows. On first boot with this feature the ledger seeds
// itself from the on-disk audit history and the durable PR throughput
// counters, so the chip does not start again from zero.

// mergedLedgerPath is where the ledger persists; tests point it elsewhere.
var mergedLedgerPath = "/data/merged-ledger.json"

const (
	// mergedLedgerRetention bounds how long an entry stays listed. The
	// 30-day window the API reports needs 30; 90 matches the audit log's
	// own MaxAge so the two never disagree about the recent past.
	mergedLedgerRetention = 90 * 24 * time.Hour
	// mergedLedgerMaxEntries caps the file on a very busy hive.
	mergedLedgerMaxEntries = 5000
	// mergedLedgerRecentMax is how many entries the API's recent list carries.
	mergedLedgerRecentMax = 20
	mergedLedgerVersion   = 1
	// MergedByHive is the merged_by value for a merge the hive performed
	// itself (sweep, queue, relay or forge path). Any other value is the
	// merger's GitHub login; "" means not known.
	MergedByHive = github.PRMergeAttributionHive
)

// MergedLedgerEntry is one merged pull request.
type MergedLedgerEntry struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Title    string    `json:"title,omitempty"`
	URL      string    `json:"url,omitempty"`
	MergedAt time.Time `json:"merged_at"`
	// MergedBy is MergedByHive or the merger's login ("" unknown).
	MergedBy string `json:"merged_by,omitempty"`
	// Lane is the tracker lane the PR was on when it merged (issue, hive,
	// contributor), Issue the issue it was nested under; both best effort.
	Lane  string `json:"lane,omitempty"`
	Issue int    `json:"issue,omitempty"`
}

func (e MergedLedgerEntry) key() string { return mergedLedgerKey(e.Repo, e.Number) }

func mergedLedgerKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// StageTrackerMergedSummary is the `merged` block of /api/stage-tracker.
type StageTrackerMergedSummary struct {
	// Today counts merges since midnight in TZ; Total7d / Total30d the
	// rolling windows; AllTime the counter that survives everything.
	Today    int `json:"today"`
	Total7d  int `json:"total_7d"`
	Total30d int `json:"total_30d"`
	AllTime  int `json:"all_time"`
	// Recent lists the newest mergedLedgerRecentMax entries, newest first.
	Recent []MergedLedgerEntry `json:"recent"`
	// TZ names the zone "today" is computed in; DayStart is that midnight.
	TZ       string    `json:"tz"`
	DayStart time.Time `json:"day_start"`
}

type mergedLedgerFile struct {
	Version int                 `json:"version"`
	AllTime int                 `json:"all_time"`
	Entries []MergedLedgerEntry `json:"entries"`
}

// MergedLedger is the persistent record; the zero value is unusable, use
// newMergedLedger.
type MergedLedger struct {
	mu      sync.Mutex
	path    string
	allTime int
	entries []MergedLedgerEntry // oldest first
	index   map[string]int
	// fromDisk is true when the constructor found a file: the ledger has
	// history and must not be seeded again.
	fromDisk bool
	// backfill is true from Seed until the first closed sweep has looked
	// back over GitHub's merge history for this hive (Backfilled clears
	// it): a first boot has only what the audit log remembered.
	backfill bool
	// onNew is called (without the lock) after a PR is recorded for the
	// first time; the server uses it to drop the tracker cache.
	onNew func(MergedLedgerEntry)
}

// newMergedLedger loads the ledger at path ("" keeps it in memory only).
func newMergedLedger(path string) *MergedLedger {
	l := &MergedLedger{path: path, index: map[string]int{}}
	if path == "" {
		return l
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	var f mergedLedgerFile
	if json.Unmarshal(data, &f) != nil {
		return l
	}
	l.fromDisk = true
	l.allTime = f.AllTime
	for _, e := range f.Entries {
		if e.Repo == "" || e.Number <= 0 {
			continue
		}
		l.upsertLocked(e)
	}
	l.sortLocked()
	return l
}

// Seeded reports whether the ledger already has history (a file was found
// or Seed ran), so a boot seeds at most once.
func (l *MergedLedger) Seeded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fromDisk
}

// Seed folds existing merge history in on first boot: the entries are
// recorded (deduplicated) and the all-time total becomes at least floor —
// the durable PR throughput counter, which may remember merges whose audit
// lines have rotated away. It persists and marks the ledger seeded.
func (l *MergedLedger) Seed(entries []MergedLedgerEntry, floor int, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range entries {
		if e.Repo == "" || e.Number <= 0 {
			continue
		}
		if _, dup := l.index[e.key()]; !dup {
			l.allTime++
		}
		l.upsertLocked(e)
	}
	if l.allTime < floor {
		l.allTime = floor
	}
	l.fromDisk = true
	l.backfill = true
	l.sortLocked()
	l.pruneLocked(now)
	l.persistLocked()
}

// NeedsBackfill reports whether this boot seeded the ledger from the
// audit log alone, so the next closed sweep should look back over
// GitHub's merge history (mergedLedgerBackfillLookback) instead of one
// day. Backfilled clears it once a sweep has read every repo.
func (l *MergedLedger) NeedsBackfill() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.backfill
}

// Backfilled records that a sweep has read GitHub's merge history.
func (l *MergedLedger) Backfilled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.backfill = false
}

// Record adds a merged PR, or fills in what an earlier record of the same
// PR did not know (title, merger, lane, issue). It reports whether the PR
// was new to the ledger; only a new PR bumps the all-time total.
func (l *MergedLedger) Record(e MergedLedgerEntry, now time.Time) bool {
	if l == nil || e.Repo == "" || e.Number <= 0 {
		return false
	}
	if e.MergedAt.IsZero() {
		e.MergedAt = now
	}
	l.mu.Lock()
	_, dup := l.index[e.key()]
	if !dup {
		l.allTime++
	}
	l.upsertLocked(e)
	l.sortLocked()
	l.pruneLocked(now)
	l.persistLocked()
	onNew := l.onNew
	l.mu.Unlock()
	if !dup && onNew != nil {
		onNew(e)
	}
	return !dup
}

// upsertLocked inserts e or merges it into the existing entry for the same
// PR: blank fields are filled, a known field is never overwritten, except
// that the earliest merge time wins (a snapshot's updated_at can trail the
// real merge).
func (l *MergedLedger) upsertLocked(e MergedLedgerEntry) {
	e.Repo = strings.TrimSpace(e.Repo)
	e.MergedAt = e.MergedAt.UTC()
	i, ok := l.index[e.key()]
	if !ok {
		l.index[e.key()] = len(l.entries)
		l.entries = append(l.entries, e)
		return
	}
	cur := &l.entries[i]
	if cur.Title == "" {
		cur.Title = e.Title
	}
	if cur.URL == "" {
		cur.URL = e.URL
	}
	if cur.MergedBy == "" {
		cur.MergedBy = e.MergedBy
	}
	if cur.Lane == "" {
		cur.Lane = e.Lane
	}
	if cur.Issue == 0 {
		cur.Issue = e.Issue
	}
	if !e.MergedAt.IsZero() && (cur.MergedAt.IsZero() || e.MergedAt.Before(cur.MergedAt)) {
		cur.MergedAt = e.MergedAt
	}
}

func (l *MergedLedger) sortLocked() {
	sort.SliceStable(l.entries, func(i, j int) bool { return l.entries[i].MergedAt.Before(l.entries[j].MergedAt) })
	for i := range l.entries {
		l.index[l.entries[i].key()] = i
	}
}

// pruneLocked drops entries past retention and the oldest beyond the cap;
// the all-time total is untouched. Entries must be sorted.
func (l *MergedLedger) pruneLocked(now time.Time) {
	cutoff := now.Add(-mergedLedgerRetention)
	drop := 0
	for drop < len(l.entries) && l.entries[drop].MergedAt.Before(cutoff) {
		drop++
	}
	if over := len(l.entries) - drop - mergedLedgerMaxEntries; over > 0 {
		drop += over
	}
	if drop == 0 {
		return
	}
	for _, e := range l.entries[:drop] {
		delete(l.index, e.key())
	}
	l.entries = append([]MergedLedgerEntry(nil), l.entries[drop:]...)
	for i := range l.entries {
		l.index[l.entries[i].key()] = i
	}
}

// persistLocked writes the file atomically (temp + rename), as the PR
// throughput counters do, so a crash cannot leave a torn file.
func (l *MergedLedger) persistLocked() {
	if l.path == "" {
		return
	}
	data, err := json.Marshal(mergedLedgerFile{Version: mergedLedgerVersion, AllTime: l.allTime, Entries: l.entries})
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		slog.Error("merged ledger write failed", "error", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		slog.Error("merged ledger rename failed", "error", err)
	}
}

// Summary computes the API block at now in loc (nil → UTC). "Today" rolls
// at midnight in loc; the 7- and 30-day totals are rolling windows.
func (l *MergedLedger) Summary(now time.Time, loc *time.Location) StageTrackerMergedSummary {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	tz := loc.String()
	if tz == "Local" {
		tz = local.Format("MST")
	}
	out := StageTrackerMergedSummary{Recent: []MergedLedgerEntry{}, TZ: tz, DayStart: dayStart}
	if l == nil {
		return out
	}
	since7 := now.Add(-7 * 24 * time.Hour)
	since30 := now.Add(-30 * 24 * time.Hour)
	l.mu.Lock()
	defer l.mu.Unlock()
	out.AllTime = l.allTime
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.MergedAt.After(now.Add(time.Minute)) {
			continue
		}
		if !e.MergedAt.Before(dayStart) {
			out.Today++
		}
		if !e.MergedAt.Before(since7) {
			out.Total7d++
		}
		if !e.MergedAt.Before(since30) {
			out.Total30d++
		}
		if len(out.Recent) < mergedLedgerRecentMax {
			out.Recent = append(out.Recent, e)
		}
	}
	return out
}

// ClosedBy returns the newest merged PR in repo recorded as nested under
// issue, other than exclude (0 when none): the PR whose merge closed the
// issue out from under an open PR that still names it (the tracker's
// "closed by #N" chip, hivecommons/hive#15232).
func (l *MergedLedger) ClosedBy(repo string, issue, exclude int) int {
	if l == nil || issue <= 0 {
		return 0
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.Issue == issue && e.Number != exclude && strings.EqualFold(e.Repo, repo) {
			return e.Number
		}
	}
	return 0
}

// Len is the number of listed (unpruned) entries.
func (l *MergedLedger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// mergedEntryFromAudit projects a pr_merged audit entry onto the ledger.
// qualify maps a short repo name onto owner/name. ok is false when the
// entry names no PR.
func mergedEntryFromAudit(e AuditEntry, qualify func(string) string) (MergedLedgerEntry, bool) {
	if e.Action != github.AuditActionPRMerged {
		return MergedLedgerEntry{}, false
	}
	repo, ok := collect.AuditEntryRepo(e)
	if !ok {
		return MergedLedgerEntry{}, false
	}
	if qualify != nil {
		repo = qualify(repo)
	}
	attrs := parseAuditDetailAttrs(e.Detail)
	number := e.Target
	if number == 0 {
		number, _ = strconv.Atoi(attrs["number"])
	}
	if number <= 0 {
		return MergedLedgerEntry{}, false
	}
	at, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		return MergedLedgerEntry{}, false
	}
	out := MergedLedgerEntry{Repo: repo, Number: number, MergedAt: at.UTC(), MergedBy: mergedByFromAudit(attrs)}
	return out, true
}

// mergedByFromAudit reads who merged from a pr_merged detail: the hive's
// own paths (sweep, queue, relay, forge, hive) are MergedByHive; an observed
// merge names its actor login; anything else is unknown.
func mergedByFromAudit(attrs map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(attrs["path"])) {
	case github.PRAuditPathSweep, github.PRAuditPathQueue, github.PRAuditPathRelay, github.PRAuditPathForge, MergedByHive:
		return MergedByHive
	}
	actor := strings.TrimSpace(attrs["actor"])
	if actor == "" || strings.EqualFold(actor, "unknown") {
		return ""
	}
	return actor
}
