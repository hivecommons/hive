package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Persistent hourly time-series feeding the Operations-tab and Leaderboard
// sparklines (#persistent-history). Four series are kept, each a ring of the
// most-recent hourly buckets on the spoke PVC so the trend survives restarts and
// rolling upgrades instead of resetting to a flat line on every deploy:
//   - queue_depth   : length of the admitted ready-work queue, SAMPLED hourly
//   - tasks_done    : task completions counted in each hour (delta of cumulative)
//   - fleet_size    : number of connected clankers, SAMPLED hourly
//   - per_user_done : per-contributor (github_username) completions per hour
//
// The store owns ONLY the counting + persistence. Sampling reads live values
// from the contribute hub; the deltas come from the cumulative per-contributor
// TasksCompleted counters that already persist per user, so a restart mid-hour
// cannot double-count (a bucket is the difference between two cumulative reads).

const (
	// metricsRetentionBuckets is how many hourly buckets each series keeps: 168
	// hours == 7 days. Old buckets fall off the front of the ring. Seven days is
	// enough to show a week-over-week trend in a tiny sparkline while keeping the
	// on-disk file and the JSON payload trivially small.
	metricsRetentionBuckets = 168

	// defaultMetricsFile is where the hourly series are mirrored on the spoke PVC
	// (same /data volume as the contributor store and fleet-stats). Overridable
	// via HIVE_METRICS_FILE so tests can point it at a temp dir, matching how the
	// contributor dir / federation registry are made overridable.
	defaultMetricsFile = "/data/metrics.json"
)

// metricsRollupInterval is how often the rollup goroutine samples + rolls a
// bucket. A var (not const) so tests can inject a short interval to exercise the
// bucketing without waiting an hour. Production keeps a 1-hour cadence, matching
// the "bucket":"hour" contract of the endpoint.
var metricsRollupInterval = time.Hour

// getMetricsFile resolves the PVC path for the persisted series, overridable for
// tests via HIVE_METRICS_FILE (mirrors getContributorsDir / federation registry).
func getMetricsFile() string {
	if v := os.Getenv("HIVE_METRICS_FILE"); v != "" {
		return v
	}
	return defaultMetricsFile
}

// metricsPersistShape is the on-disk / wire JSON shape. Field names are the
// stable contract the client sparklines read; keep them in sync with the
// endpoint response and the client fetch.
type metricsPersistShape struct {
	QueueDepth  []int            `json:"queue_depth"`
	TasksDone   []int            `json:"tasks_done"`
	FleetSize   []int            `json:"fleet_size"`
	PerUserDone map[string][]int `json:"per_user_done"`
	Bucket      string           `json:"bucket"`
	CollectedAt string           `json:"collected_at"`
}

// metricsStore is a concurrency-safe ring of hourly buckets per series. Every
// series is capped at metricsRetentionBuckets; per_user_done is a map of
// username -> its own capped ring. lastTotals holds the previous cumulative
// per-user completion counts so each tick can derive the hour's delta.
type metricsStore struct {
	mu sync.Mutex

	queueDepth  []int
	tasksDone   []int
	fleetSize   []int
	perUserDone map[string][]int

	// lastTotals is the cumulative TasksCompleted per user as of the previous
	// rollup, used to compute this hour's completion delta. Not persisted: it is
	// re-seeded from the live profiles on the first tick after a restart, so the
	// first post-restart bucket may under-count a partial hour rather than
	// mistaking the whole cumulative total for a single hour's work.
	lastTotals map[string]int

	// seededTotals guards the first-tick seed so a restart does not book the
	// entire historical cumulative count as one hour of completions.
	seededTotals bool

	collectedAt time.Time
	path        string
	logger      *slog.Logger
}

// newMetricsStore builds an empty store. Call load() to restore prior history
// from the PVC and Start() to run the hourly rollup.
func newMetricsStore(path string, logger *slog.Logger) *metricsStore {
	return &metricsStore{
		perUserDone: make(map[string][]int),
		lastTotals:  make(map[string]int),
		path:        path,
		logger:      logger,
	}
}

// capRing trims a series to the most-recent metricsRetentionBuckets tail so the
// ring never grows without bound. Returns the (possibly reused) slice.
func capRing(s []int) []int {
	if len(s) > metricsRetentionBuckets {
		return s[len(s)-metricsRetentionBuckets:]
	}
	return s
}

// load restores the series from the PVC file. A missing file is a clean no-op
// (first boot). A corrupt/unreadable file is logged and ignored — the store
// starts empty rather than crashing, matching the tombstone/fleet-stats loaders'
// resilience contract (never panic on a bad /data file).
func (m *metricsStore) load() {
	if m == nil || m.path == "" {
		return
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if !os.IsNotExist(err) && m.logger != nil {
			m.logger.Warn("metrics store unreadable — starting with empty history",
				"path", m.path, "error", err)
		}
		return
	}
	var stored metricsPersistShape
	if err := json.Unmarshal(data, &stored); err != nil {
		if m.logger != nil {
			m.logger.Warn("metrics store corrupt — starting with empty history",
				"path", m.path, "error", err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueDepth = capRing(stored.QueueDepth)
	m.tasksDone = capRing(stored.TasksDone)
	m.fleetSize = capRing(stored.FleetSize)
	m.perUserDone = make(map[string][]int, len(stored.PerUserDone))
	for user, ring := range stored.PerUserDone {
		m.perUserDone[user] = capRing(ring)
	}
	if t, err := time.Parse(time.RFC3339, stored.CollectedAt); err == nil {
		m.collectedAt = t
	}
	if m.logger != nil {
		m.logger.Info("restored contribute metrics from PVC",
			"buckets", len(m.tasksDone), "users", len(m.perUserDone), "path", m.path)
	}
}

// snapshot returns a copy-on-read wire shape so callers (the endpoint) cannot
// mutate the rings. Safe for concurrent use.
func (m *metricsStore) snapshot() metricsPersistShape {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := metricsPersistShape{
		QueueDepth:  append([]int(nil), m.queueDepth...),
		TasksDone:   append([]int(nil), m.tasksDone...),
		FleetSize:   append([]int(nil), m.fleetSize...),
		PerUserDone: make(map[string][]int, len(m.perUserDone)),
		Bucket:      "hour",
	}
	for user, ring := range m.perUserDone {
		out.PerUserDone[user] = append([]int(nil), ring...)
	}
	if !m.collectedAt.IsZero() {
		out.CollectedAt = m.collectedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// persistLocked mirrors the current series to the PVC atomically (temp file +
// rename, matching the fleet-stats / session store writers). Callers must hold
// m.mu. 0600 keeps every PVC-persisted dashboard file in one trust domain; the
// contents are only counts + already-public usernames, never secrets.
func (m *metricsStore) persistLocked() {
	if m.path == "" {
		return
	}
	shape := metricsPersistShape{
		QueueDepth:  m.queueDepth,
		TasksDone:   m.tasksDone,
		FleetSize:   m.fleetSize,
		PerUserDone: m.perUserDone,
		Bucket:      "hour",
	}
	if !m.collectedAt.IsZero() {
		shape.CollectedAt = m.collectedAt.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(shape)
	if err != nil {
		return
	}
	ensureDir(filepath.Dir(m.path))
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if m.logger != nil {
			m.logger.Warn("failed to write metrics store", "path", m.path, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, m.path); err != nil && m.logger != nil {
		m.logger.Warn("failed to replace metrics store", "path", m.path, "error", err)
	}
}

// rollupSample is the live data one rollup tick observes: the current admitted
// queue depth, the current connected fleet size, and the current CUMULATIVE
// per-user completion totals. The store converts the cumulative totals into a
// per-hour delta internally.
type rollupSample struct {
	queueDepth int
	fleetSize  int
	// userTotals maps github_username -> cumulative TasksCompleted so far. The
	// store diffs this against the previous tick to get the hour's completions.
	userTotals map[string]int
	// now is the wall clock for this tick, truncated to the hour for bucketing.
	now time.Time
}

// rollup folds one sample into a new bucket for every series and persists. It is
// the single place a bucket is appended, so the four rings stay index-aligned by
// construction. queue_depth and fleet_size are point SAMPLES; tasks_done and
// per_user_done are DELTAS derived from the cumulative per-user totals.
func (m *metricsStore) rollup(s rollupSample) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Seed the baseline on the first tick after a (re)start so we book only the
	// completions that happen AFTER we start watching, never the whole historical
	// cumulative total as a single hour's work.
	if !m.seededTotals {
		m.lastTotals = make(map[string]int, len(s.userTotals))
		for user, total := range s.userTotals {
			m.lastTotals[user] = total
		}
		m.seededTotals = true
	}

	hourTasks := 0
	for user, total := range s.userTotals {
		delta := total - m.lastTotals[user]
		if delta < 0 {
			// A profile reset / re-registration lowered the cumulative count.
			// Treat as zero for this hour rather than a negative bucket.
			delta = 0
		}
		if delta > 0 {
			m.perUserDone[user] = capRing(append(m.perUserDone[user], delta))
			hourTasks += delta
		}
		m.lastTotals[user] = total
	}

	m.queueDepth = capRing(append(m.queueDepth, s.queueDepth))
	m.fleetSize = capRing(append(m.fleetSize, s.fleetSize))
	m.tasksDone = capRing(append(m.tasksDone, hourTasks))
	m.collectedAt = s.now.Truncate(time.Hour)

	m.persistLocked()
}

// metricsSampler is the minimal live-data surface the rollup needs, so the store
// stays testable without a full Server. The Server satisfies it via
// sampleMetricsInputs.
type metricsSampler interface {
	sampleMetricsInputs() rollupSample
}

// Start runs the hourly rollup loop until ctx is cancelled. It ticks on
// metricsRollupInterval; the ticker takes ctx and is stopped on return so there
// is no uncancellable timer leak (matching the fleet-stats collector contract).
func (m *metricsStore) Start(ctx context.Context, sampler metricsSampler) {
	if m == nil || sampler == nil {
		return
	}
	ticker := time.NewTicker(metricsRollupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.rollup(sampler.sampleMetricsInputs())
		}
	}
}

// contributeMetricsStore lazily builds the per-Server metrics store, loading any
// prior history from the PVC on first use. Guarded by contributeMetricsOnce so
// the zero-value Server needs no constructor change (matching the token/fact/cost
// history accessors' lazy pattern).
func (s *Server) contributeMetricsStore() *metricsStore {
	s.contributeMetricsOnce.Do(func() {
		s.contributeMetrics = newMetricsStore(getMetricsFile(), s.logger)
		s.contributeMetrics.load()
	})
	return s.contributeMetrics
}

// sampleMetricsInputs reads the live values the rollup buckets: the admitted
// ready-work queue length, the connected clanker count, and each contributor's
// cumulative completion total. All reads are cheap and side-effect-free.
func (s *Server) sampleMetricsInputs() rollupSample {
	sample := rollupSample{
		userTotals: make(map[string]int),
		now:        time.Now(),
	}
	if s.contributeHub != nil {
		// Admitted ready-work queue length — the SAME admissible set the
		// /api/contribute/queue endpoint serves (ReadyQueue), so the sparkline
		// tracks exactly what the operator sees in the queue panel.
		sample.queueDepth = len(s.contributeHub.ReadyQueue(readyQueueDefaultLimit))
		// Connected clankers — distinct connected contributors, the same count the
		// Operations "Connected clankers" panel shows.
		sample.fleetSize = s.contributeHub.ActiveCount()
	}
	for _, p := range listContributorProfiles() {
		if p.GitHubUsername == "" {
			continue
		}
		sample.userTotals[p.GitHubUsername] = p.TasksCompleted
	}
	return sample
}

// StartContributeMetrics wires the hourly rollup goroutine to ctx, so it shuts
// down cleanly when the process context is cancelled (no goroutine leak). Call
// once at startup, like the other background collectors (fleet-stats, metrics).
func (s *Server) StartContributeMetrics(ctx context.Context) {
	store := s.contributeMetricsStore()
	go store.Start(ctx, s)
}

// handleContributeMetrics serves the persisted hourly series for the sparklines.
// GET only, read-only, no side effects. Public within the contribute surface
// (the /api/contribute* prefix is treated as public by isPublicPath, exactly
// like /api/contribute/fleet and /queue), because it exposes only counts and the
// github_usernames already shown on the public leaderboard — no tokens, no PII.
func (s *Server) handleContributeMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.contributeMetricsStore().snapshot()
	// Sort per-user keys for a stable payload (aids caching/diffing and tests).
	users := make([]string, 0, len(snap.PerUserDone))
	for u := range snap.PerUserDone {
		users = append(users, u)
	}
	sort.Strings(users)
	ordered := make(map[string][]int, len(snap.PerUserDone))
	for _, u := range users {
		ordered[u] = snap.PerUserDone[u]
	}
	jsonResponse(w, map[string]any{
		"queue_depth":   snap.QueueDepth,
		"tasks_done":    snap.TasksDone,
		"fleet_size":    snap.FleetSize,
		"per_user_done": ordered,
		"bucket":        "hour",
		"collected_at":  snap.CollectedAt,
	})
}
