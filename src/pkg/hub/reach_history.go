package hub

// Fleet-level error-rate history retention (#3995, phase 2c of #3973).
//
// The registry stores only each hive's LATEST component_reach report (2a's
// deliberate design) — sufficient for reach, useless for before/after error
// deltas. This store keeps a bounded ring of HOURLY activity buckets per
// component, fed on heartbeat receive from the report's rolling window_1h
// bucket (equivalent-period windows — cumulative-since-boot counts would
// compare unequal periods), SUMMED across hives per (hour bucket, component,
// running commit). Per-hive history is deliberately NOT kept — it would
// multiply storage by fleet size for no consumer; the only per-hive state is
// one O(1) dedupe cursor so a hive re-reporting the same window can never
// double-count.
//
// All input is a sanitized-but-still-spoke-authored ReachReport, so the store
// defends itself again: window timestamps must parse and fall inside the ring
// span, counts are clamped non-negative, the component set and per-bucket
// commit fan-out are capped, and over-cap input is CLIPPED AND LOGGED, never
// silently absorbed. Honest limitation the caps cannot remove: windows are
// fleet sums, so one high-volume (or hostile-within-clamps) spoke can skew a
// window's error ratio; see ComputeErrorDelta's caution note.
//
// Persisted in its own file (reachHistoryPath), the same own-file pattern 2a
// chose for /data/reach-state.json: atomic tmp+rename writes on a modest
// cadence plus a final save at shutdown, loaded (and re-bounded — a
// hand-edited file must not bypass the caps) at hub boot.

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/reach"
	"github.com/hivecommons/hive/pkg/tracing"
)

// reachHistoryPath is the DEFAULT on-disk history file — its own file beside
// the registry, per the epic's resolved persistence pattern. A var (not a
// const) only so hub_filesystem_test.go can swap it, mirroring registryPath.
var reachHistoryPath = "/data/reach-history.json"

const (
	// reachHistoryWindows bounds the ring per component: 7 days of hourly
	// buckets (#3995). A week holds several deploy cadences of before/after
	// context on this fleet while keeping the whole store trivially small
	// (~168 buckets × components).
	reachHistoryWindows = 7 * 24

	// maxReachHistoryComponents caps DISTINCT tracked components fleet-wide.
	// Component names are code-defined, so the fleet's true set is the same
	// ~dozen every spoke reports (per-spoke cap tracing.MaxReachComponents);
	// double that cap absorbs mixed-version fleets without letting hostile
	// name churn grow hub memory. Over-cap components are clipped and logged.
	maxReachHistoryComponents = 2 * tracing.MaxReachComponents

	// maxReachHistoryCommitsPerBucket caps distinct commits recorded per
	// (component, hour bucket). Real fleets deploy a handful of commits per
	// hour at the very most; more is a hostile or broken reporter, clipped
	// and logged.
	maxReachHistoryCommitsPerBucket = 8

	// maxReachHistoryCursors caps per-component dedupe cursors (one per
	// reporting hive). The fleet is ~10² hives; 1024 leaves generous headroom
	// while bounding what a flood of fabricated hive IDs can allocate.
	// Over-cap eviction drops the stalest cursor (oldest bucket) — worst
	// case that hive's next report re-adds one window's counts.
	maxReachHistoryCursors = 1024

	// reachHistoryFutureSkew tolerates spoke clocks running slightly ahead;
	// window starts further in the future than this are hostile or broken
	// and are dropped (and counted).
	reachHistoryFutureSkew = 5 * time.Minute

	// reachHistorySaveInterval is the persistence cadence: at most one write
	// per interval, dirty-gated — the same modest, off-hot-path cadence 2a
	// uses spoke-side. Heartbeats arrive ~1/min/hive, so this bounds writes
	// to one per minute regardless of fleet size.
	reachHistorySaveInterval = time.Minute

	// maxReachHistoryOverflowLogs caps the once-per-name over-cap log set so
	// hostile name churn cannot grow it without bound (the same 256 bound
	// the spoke-side reach registry uses for its overflow log set).
	maxReachHistoryOverflowLogs = 256
)

// reachHistorySample is one persisted ring entry: fleet-summed spans for
// (hour bucket, commit) within a component.
type reachHistorySample struct {
	WindowStart time.Time `json:"window_start"`
	Commit      string    `json:"commit"`
	SpansTotal  int64     `json:"spans_total"`
	SpansError  int64     `json:"spans_error"`
}

// reachHiveCursor is the per-(component, hive) dedupe cursor: the last spoke
// window folded into the ring, identified by the spoke's own raw window
// start (its identity for one rolling window) plus the running commit. It
// lets a re-report of the same window contribute only its INCREMENT.
// Persisted with the ring: losing cursors across a hub restart would make
// the next heartbeat re-add counts a previous process already summed.
type reachHiveCursor struct {
	RawStart   string    `json:"raw_start"`
	Bucket     time.Time `json:"bucket"`
	Commit     string    `json:"commit"`
	SpansTotal int64     `json:"spans_total"`
	SpansError int64     `json:"spans_error"`
}

// reachComponentHistory is one component's ring plus its dedupe cursors.
type reachComponentHistory struct {
	Samples []reachHistorySample       `json:"samples"` // oldest bucket first
	Cursors map[string]reachHiveCursor `json:"cursors"` // keyed by hive ID
}

// reachHistoryFile is the on-disk shape.
type reachHistoryFile struct {
	Components map[string]*reachComponentHistory `json:"components"`
}

// reachHistoryStore is the hub's bounded, persisted retention. Safe for
// concurrent use; Append runs on the heartbeat path, ComponentWindows on
// /api/reach requests.
type reachHistoryStore struct {
	mu         sync.Mutex
	path       string
	logger     *slog.Logger
	components map[string]*reachComponentHistory

	// clippedComponents / droppedSamples count refused input — the visible
	// residue of the caps, mirrored in logs so a clip is never silent.
	clippedComponents int64
	droppedSamples    int64
	// loggedOverflow keeps the over-cap component log once-per-name, itself
	// capped so hostile churn cannot grow it.
	loggedOverflow map[string]bool

	dirty    bool
	lastSave time.Time
}

// newReachHistoryStore builds the store and loads any persisted history from
// path. A missing file is a normal first boot; a corrupt file starts empty
// (loudly) rather than blocking hub startup on telemetry state.
func newReachHistoryStore(path string, logger *slog.Logger) *reachHistoryStore {
	s := &reachHistoryStore{
		path:           path,
		logger:         logger,
		components:     make(map[string]*reachComponentHistory),
		loggedOverflow: make(map[string]bool),
		// Start the save clock now so a burst of boot-time heartbeats does
		// not trigger an immediate write before the interval has ever run.
		lastSave: time.Now(),
	}
	s.load()
	return s
}

// load reads and re-bounds the persisted file. Every cap is re-applied: the
// file is hub-authored, but a hand-edited or corrupt one must not bypass the
// bounds (the same discipline as tracing.LoadReachState).
func (s *reachHistoryStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("reach history load failed; starting empty (#3995)", "path", s.path, "error", err)
		}
		return
	}
	var file reachHistoryFile
	if err := json.Unmarshal(data, &file); err != nil {
		s.logger.Warn("reach history parse failed; starting empty (#3995)", "path", s.path, "error", err)
		return
	}
	trimmed := 0
	for name, hist := range file.Components {
		if name == "" || hist == nil {
			continue
		}
		if len(s.components) >= maxReachHistoryComponents {
			trimmed++
			continue
		}
		if hist.Cursors == nil {
			hist.Cursors = make(map[string]reachHiveCursor)
		}
		for len(hist.Cursors) > maxReachHistoryCursors {
			evictOldestCursor(hist)
		}
		sort.Slice(hist.Samples, func(i, j int) bool {
			return hist.Samples[i].WindowStart.Before(hist.Samples[j].WindowStart)
		})
		// Negative counts or empty commits cannot be produced by Append; a
		// file carrying them is corrupt or edited. Drop rather than trust.
		clean := hist.Samples[:0]
		for _, sm := range hist.Samples {
			if sm.Commit == "" || sm.SpansTotal < 0 || sm.SpansError < 0 {
				trimmed++
				continue
			}
			clean = append(clean, sm)
		}
		hist.Samples = clean
		trimBucketBound(hist)
		s.components[name] = hist
	}
	if trimmed > 0 {
		s.logger.Warn("reach history load re-bounded out-of-cap entries (#3995)", "trimmed", trimmed)
	}
	s.logger.Info("reach history loaded (#3995)", "path", s.path, "components", len(s.components))
}

// evictOldestCursor drops the cursor with the oldest bucket.
func evictOldestCursor(hist *reachComponentHistory) {
	oldestKey := ""
	var oldest time.Time
	for k, c := range hist.Cursors {
		if oldestKey == "" || c.Bucket.Before(oldest) {
			oldestKey, oldest = k, c.Bucket
		}
	}
	if oldestKey != "" {
		delete(hist.Cursors, oldestKey)
	}
}

// trimBucketBound enforces the ring bound: at most reachHistoryWindows
// DISTINCT hour buckets per component, evicting oldest-first. Samples are
// kept oldest-first, so eviction is a front cut at a bucket boundary.
func trimBucketBound(hist *reachComponentHistory) {
	for {
		distinct := 0
		var prev time.Time
		for i, sm := range hist.Samples {
			if i == 0 || !sm.WindowStart.Equal(prev) {
				distinct++
				prev = sm.WindowStart
			}
		}
		if distinct <= reachHistoryWindows || len(hist.Samples) == 0 {
			return
		}
		// Cut every sample in the oldest bucket.
		oldest := hist.Samples[0].WindowStart
		cut := 0
		for cut < len(hist.Samples) && hist.Samples[cut].WindowStart.Equal(oldest) {
			cut++
		}
		hist.Samples = append(hist.Samples[:0], hist.Samples[cut:]...)
	}
}

// Append folds one hive's sanitized reach report into the fleet history.
// Called on heartbeat receive with the OUTPUT of sanitizeComponentReach —
// never the raw payload — and only for beats that actually carried a fresh
// report (the carry-forward path must not re-append stale windows; its
// re-reports would be deduped by the cursor anyway, but skipping them is
// cheaper and clearer). nil-safe on both receiver and report.
func (s *reachHistoryStore) Append(hiveID string, report *tracing.ReachReport, now time.Time) {
	if s == nil || report == nil || hiveID == "" {
		return
	}
	s.mu.Lock()
	droppedThisCall := 0
	clippedThisCall := 0
	for _, entry := range report.Entries {
		if entry.Component == "" || entry.Commit == "" || entry.Window1h == nil {
			// No component/commit attribution or no rolling window — nothing
			// a delta could ever use. Not hostile, just not history input.
			continue
		}
		start, err := time.Parse(time.RFC3339, entry.Window1h.Start)
		if err != nil {
			// sanitizeComponentReach blanks unparseable times, so "" lands
			// here; anything else non-parsing means corrupted storage.
			droppedThisCall++
			continue
		}
		start = start.UTC()
		ringSpan := time.Duration(reachHistoryWindows) * time.Hour
		if start.After(now.Add(reachHistoryFutureSkew)) || start.Before(now.Add(-ringSpan)) {
			// Out-of-range window: a future timestamp is a broken or hostile
			// clock; one older than the ring would be evicted immediately (or
			// worse, poison the "before" side of a delta with ancient data).
			droppedThisCall++
			continue
		}
		winTotal := clampInt64(entry.Window1h.SpansTotal, 0, maxReachSpanCount)
		winErr := clampInt64(entry.Window1h.SpansError, 0, maxReachSpanCount)

		hist, ok := s.components[entry.Component]
		if !ok {
			if len(s.components) >= maxReachHistoryComponents {
				clippedThisCall++
				s.noteComponentOverflowLocked(entry.Component)
				continue
			}
			hist = &reachComponentHistory{Cursors: make(map[string]reachHiveCursor)}
			s.components[entry.Component] = hist
		}

		// Spoke windows have arbitrary starts; bucket onto the hourly grid so
		// cross-hive summation is meaningful (see reach.HistorySample).
		bucket := start.Truncate(time.Hour)

		cursor, hasCursor := hist.Cursors[hiveID]
		switch {
		case hasCursor && cursor.RawStart == entry.Window1h.Start && cursor.Commit == entry.Commit:
			// Same spoke window re-reported: contribute only the INCREMENT
			// since the last beat — the dedupe that keeps a window from
			// double-counting. Window counters are cumulative within their
			// window, so a decrease means the spoke restarted mid-window and
			// resumed lower; clamping the increment at zero undercounts that
			// sliver rather than ever double-counting.
			deltaTotal := winTotal - cursor.SpansTotal
			deltaErr := winErr - cursor.SpansError
			if deltaTotal < 0 {
				deltaTotal = 0
			}
			if deltaErr < 0 {
				deltaErr = 0
			}
			if deltaTotal > 0 || deltaErr > 0 {
				if !s.addSampleLocked(hist, bucket, entry.Commit, deltaTotal, deltaErr) {
					clippedThisCall++
				}
			}
			cursor.SpansTotal, cursor.SpansError = winTotal, winErr
			hist.Cursors[hiveID] = cursor
		case hasCursor && bucket.Before(cursor.Bucket):
			// A window OLDER than the hive's last folded one: out-of-order
			// replay (or a clock yanked backwards). Folding it could
			// double-count a window already summed — drop it.
			droppedThisCall++
		default:
			// New spoke window (first report from this hive, a rolled
			// window, or a new running commit): contribute its full counts.
			if winTotal > 0 || winErr > 0 {
				if !s.addSampleLocked(hist, bucket, entry.Commit, winTotal, winErr) {
					clippedThisCall++
				}
			}
			if !hasCursor && len(hist.Cursors) >= maxReachHistoryCursors {
				evictOldestCursor(hist)
			}
			hist.Cursors[hiveID] = reachHiveCursor{
				RawStart:   entry.Window1h.Start,
				Bucket:     bucket,
				Commit:     entry.Commit,
				SpansTotal: winTotal,
				SpansError: winErr,
			}
		}
		trimBucketBound(hist)
		s.dirty = true
	}
	s.droppedSamples += int64(droppedThisCall)
	s.clippedComponents += int64(clippedThisCall)
	s.mu.Unlock()

	if droppedThisCall > 0 || clippedThisCall > 0 {
		// The clip/drop is visible, never silent (#3995): one aggregate line
		// per offending beat, not one per entry, so a hostile spoke cannot
		// use the log as an amplification channel either.
		s.logger.Warn("reach history: refused out-of-bounds input (#3995)",
			"hive_id", hiveID, "dropped_windows", droppedThisCall, "clipped_over_cap", clippedThisCall)
	}
	s.maybeSave(now)
}

// noteComponentOverflowLocked logs an over-cap component once per name, with
// the once-set itself capped. Callers hold s.mu.
func (s *reachHistoryStore) noteComponentOverflowLocked(component string) {
	if s.loggedOverflow[component] || len(s.loggedOverflow) >= maxReachHistoryOverflowLogs {
		return
	}
	s.loggedOverflow[component] = true
	s.logger.Warn("reach history: component cap reached, new component refused (#3995)",
		"component", component, "cap", maxReachHistoryComponents)
}

// addSampleLocked adds counts into the (bucket, commit) sample, creating it
// if the per-bucket commit cap allows. Returns false when the cap refused a
// NEW commit for the bucket. Callers hold s.mu.
func (s *reachHistoryStore) addSampleLocked(hist *reachComponentHistory, bucket time.Time, commit string, total, errs int64) bool {
	commitsInBucket := 0
	insertAt := len(hist.Samples)
	for i := range hist.Samples {
		sm := &hist.Samples[i]
		if sm.WindowStart.Equal(bucket) {
			if sm.Commit == commit {
				sm.SpansTotal += total
				sm.SpansError += errs
				return true
			}
			commitsInBucket++
		}
		if bucket.Before(sm.WindowStart) && insertAt == len(hist.Samples) {
			insertAt = i
		}
	}
	if commitsInBucket >= maxReachHistoryCommitsPerBucket {
		return false
	}
	sample := reachHistorySample{WindowStart: bucket, Commit: commit, SpansTotal: total, SpansError: errs}
	hist.Samples = append(hist.Samples, reachHistorySample{})
	copy(hist.Samples[insertAt+1:], hist.Samples[insertAt:])
	hist.Samples[insertAt] = sample
	return true
}

// ComponentWindows implements reach.HistorySource: a snapshot of one
// component's fleet history, oldest bucket first.
func (s *reachHistoryStore) ComponentWindows(component string) []reach.HistorySample {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hist, ok := s.components[component]
	if !ok || len(hist.Samples) == 0 {
		return nil
	}
	out := make([]reach.HistorySample, 0, len(hist.Samples))
	for _, sm := range hist.Samples {
		out = append(out, reach.HistorySample{
			WindowStart: sm.WindowStart,
			Commit:      sm.Commit,
			SpansTotal:  sm.SpansTotal,
			SpansError:  sm.SpansError,
		})
	}
	return out
}

// maybeSave persists when dirty and the save interval has elapsed. Marshal
// happens under the lock (the store is small by construction); the file write
// happens outside it, mirroring saveRegistryNow's atomic tmp+rename.
func (s *reachHistoryStore) maybeSave(now time.Time) {
	s.mu.Lock()
	if !s.dirty || now.Sub(s.lastSave) < reachHistorySaveInterval {
		s.mu.Unlock()
		return
	}
	data, err := s.marshalLocked()
	s.dirty = false
	s.lastSave = now
	s.mu.Unlock()
	if err != nil {
		s.logger.Warn("reach history marshal failed (#3995)", "error", err)
		return
	}
	s.write(data)
}

// SaveNow flushes unconditionally when dirty — the shutdown hook, so the
// final partial minute of history survives a roll.
func (s *reachHistoryStore) SaveNow() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	data, err := s.marshalLocked()
	s.dirty = false
	s.lastSave = time.Now()
	s.mu.Unlock()
	if err != nil {
		s.logger.Warn("reach history marshal failed (#3995)", "error", err)
		return
	}
	s.write(data)
}

// marshalLocked renders the persisted shape. Callers hold s.mu.
func (s *reachHistoryStore) marshalLocked() ([]byte, error) {
	return json.MarshalIndent(reachHistoryFile{Components: s.components}, "", "  ")
}

// write lands data atomically (tmp + rename). Failures are logged, not
// fatal: history is telemetry, and the next dirty interval retries.
func (s *reachHistoryStore) write(data []byte) {
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		s.logger.Warn("reach history save failed (#3995)", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.logger.Warn("reach history rename failed (#3995)", "path", s.path, "error", err)
	}
}
