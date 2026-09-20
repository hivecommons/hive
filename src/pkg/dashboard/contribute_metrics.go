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
// sparklines (#persistent-history). Five series are kept, each a ring of the
// most-recent hourly buckets on the spoke PVC so the trend survives restarts and
// rolling upgrades instead of resetting to a flat line on every deploy:
//   - queue_depth   : length of the admitted ready-work queue, SAMPLED hourly
//   - tasks_done    : task completions counted in each hour (delta of cumulative)
//   - fleet_size    : number of connected clankers, SAMPLED hourly
//   - per_user_done : per-contributor (github_username) completions per hour
//   - per_user_pr   : per-contributor completions that produced a verified pull
//     request per hour (#7894) — the same delta-of-cumulative
//     rollup as per_user_done, read from TasksWithPR instead of
//     TasksCompleted
//
// Every series shares ONE timeline: index i of per_user_done[u] is the same hour
// as index i of tasks_done, because a rollup appends exactly one bucket to every
// series — including a zero to each per-user ring for an hour that contributor
// finished nothing. That alignment is what lets a client read "the last 24
// hours" as the last 24 buckets, and what makes the per-user sparklines line up
// with the shared ones instead of stretching a handful of active hours across a
// strip labelled "last 7 days" (#6543).
//
// per_user_pr is aligned at the TAIL only: its last bucket is the timeline's
// last hour and each rollup appends exactly one bucket, but a ring that started
// late is not left-padded to the timeline's length. Nothing draws it — it is
// only ever summed from the tail — so head alignment buys nothing, while its
// length stays the number of hours actually measured. That is what keeps the
// "PRs produced (24h)" coverage honest across the upgrade that introduced the
// ring: for the first day every contributor's PR ring is shorter than their
// completion ring, and the card says "Nh of history so far" instead of
// labelling zeros nobody measured as a full day of no pull requests.
//
// The store owns ONLY the counting + persistence. Sampling reads live values
// from the contribute hub; the deltas come from the cumulative per-contributor
// TasksCompleted / TasksWithPR counters that already persist per user, so a
// restart mid-hour cannot double-count (a bucket is the difference between two
// cumulative reads).

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
	// PerUserPR is persisted so the "PRs produced (24h)" figure survives a restart
	// the same way the completion figure does (#7894). It is absent from files
	// written before the ring existed, which load() treats as "no PR history
	// yet" rather than as zeros. It is NOT served by /api/contribute/metrics: the
	// sparklines do not read it, and doubling the per-user payload every poll
	// for a series nothing draws would be pure cost on a large hub. Rings here
	// may be SHORTER than tasks_done (tail-aligned, see the package comment).
	PerUserPR   map[string][]int `json:"per_user_pr,omitempty"`
	Bucket      string           `json:"bucket"`
	CollectedAt string           `json:"collected_at"`
}

// metricsStore is a concurrency-safe ring of hourly buckets per series. Every
// series is capped at metricsRetentionBuckets; per_user_done is a map of
// username -> a ring INDEX-ALIGNED with tasksDone (one bucket per rollup tick,
// zero-filled for hours the user completed nothing), and per_user_pr the same
// keyed map of TAIL-aligned rings. lastTotals and lastPRTotals hold the previous
// cumulative per-user counts so each tick can derive the hour's delta.
type metricsStore struct {
	mu sync.Mutex

	queueDepth  []int
	tasksDone   []int
	fleetSize   []int
	perUserDone map[string][]int
	perUserPR   map[string][]int

	// lastTotals is the cumulative TasksCompleted per user as of the previous
	// rollup, used to compute this hour's completion delta. Not persisted: it is
	// re-seeded from the live profiles on the first tick after a restart, so the
	// first post-restart bucket may under-count a partial hour rather than
	// mistaking the whole cumulative total for a single hour's work.
	lastTotals map[string]int

	// lastPRTotals is the same baseline for TasksWithPR, feeding perUserPR.
	lastPRTotals map[string]int

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
		perUserDone:  make(map[string][]int),
		perUserPR:    make(map[string][]int),
		lastTotals:   make(map[string]int),
		lastPRTotals: make(map[string]int),
		path:         path,
		logger:       logger,
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

// padRing left-pads a per-user ring with leading zero buckets so it is exactly
// want long, i.e. so its LAST bucket is the same hour as the last bucket of the
// shared timeline. A contributor who registered mid-window genuinely completed
// nothing in the hours before they arrived, so leading zeros are the truthful
// filler, not a guess. An over-long ring is trimmed from the front (oldest
// first), matching capRing's "keep the recent tail" rule.
func padRing(s []int, want int) []int {
	if want <= 0 {
		return nil
	}
	if len(s) == want {
		return s
	}
	if len(s) > want {
		return append([]int(nil), s[len(s)-want:]...)
	}
	return append(make([]int, want-len(s), want), s...)
}

// allZero reports whether a ring holds no work at all across the retained
// window. An empty ring counts as all-zero.
func allZero(s []int) bool {
	for _, n := range s {
		if n != 0 {
			return false
		}
	}
	return true
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
	// Per-user rings written before #6543 were RAGGED: a bucket was appended only
	// for an hour the contributor actually completed something, so a ring of 33
	// entries alongside a 168-bucket shared timeline had no recoverable mapping
	// from index to hour. Left-padding such a ring would silently CLAIM that all
	// of that work landed in the most recent 33 hours — which is exactly the wrong
	// answer for a 24-hour rollup. There are no per-bucket timestamps to do better
	// with, so a misaligned legacy ring is DROPPED (reset to zeros on the shared
	// timeline) rather than reinterpreted. Alignment then holds from the next
	// rollup on. Rings that already match the shared length are kept as-is: they
	// are aligned by construction.
	want := len(m.tasksDone)
	reset := 0
	m.perUserDone = make(map[string][]int, len(stored.PerUserDone))
	for user, ring := range stored.PerUserDone {
		ring = capRing(ring)
		if len(ring) != want {
			ring = make([]int, want)
			reset++
		}
		m.perUserDone[user] = ring
	}
	// PR rings are tail-aligned and may legitimately be SHORTER than the
	// timeline (started after it). One LONGER than the timeline cannot have been
	// written by rollup — one bucket per tick, capped alike — so it is dropped
	// rather than trusted. A file written before the PR ring existed simply has
	// no per_user_pr: the map stays empty, so every contributor reads as "no PR
	// history yet" until the first post-upgrade rollup starts their ring — the
	// same answer a contributor who registered since the last tick gets, and the
	// honest one, since nothing measured PRs per hour before this.
	m.perUserPR = make(map[string][]int, len(stored.PerUserPR))
	for user, ring := range stored.PerUserPR {
		ring = capRing(ring)
		if len(ring) == 0 || len(ring) > want {
			reset++
			continue
		}
		m.perUserPR[user] = ring
	}
	if t, err := time.Parse(time.RFC3339, stored.CollectedAt); err == nil {
		m.collectedAt = t
	}
	if m.logger != nil {
		m.logger.Info("restored contribute metrics from PVC",
			"buckets", len(m.tasksDone), "users", len(m.perUserDone),
			"realigned_user_series", reset, "path", m.path)
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
		PerUserPR:   make(map[string][]int, len(m.perUserPR)),
		Bucket:      "hour",
	}
	for user, ring := range m.perUserDone {
		out.PerUserDone[user] = append([]int(nil), ring...)
	}
	for user, ring := range m.perUserPR {
		out.PerUserPR[user] = append([]int(nil), ring...)
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
		PerUserPR:   m.perUserPR,
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
// per-user completion and with-PR totals. The store converts the cumulative
// totals into a per-hour delta internally.
type rollupSample struct {
	queueDepth int
	fleetSize  int
	// userTotals maps github_username -> cumulative TasksCompleted so far. The
	// store diffs this against the previous tick to get the hour's completions.
	userTotals map[string]int
	// userPRTotals maps github_username -> cumulative TasksWithPR so far, diffed
	// the same way into the hour's PR-producing completions (#7894). A nil map
	// (older callers, tests that only care about completions) rolls up no PR
	// buckets at all rather than booking zeros nobody measured.
	userPRTotals map[string]int
	// now is the wall clock for this tick, truncated to the hour for bucketing.
	now time.Time
}

// rollup folds one sample into a new bucket for every series and persists. It is
// the single place a bucket is appended, so the shared rings and per_user_done
// stay index-aligned — and per_user_pr tail-aligned — by construction.
// queue_depth and fleet_size are point SAMPLES; tasks_done, per_user_done and
// per_user_pr are DELTAS derived from the cumulative per-user totals.
func (m *metricsStore) rollup(s rollupSample) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Seed the baselines on the first tick after a (re)start so we book only the
	// completions that happen AFTER we start watching, never the whole historical
	// cumulative total as a single hour's work.
	if !m.seededTotals {
		m.lastTotals = make(map[string]int, len(s.userTotals))
		for user, total := range s.userTotals {
			m.lastTotals[user] = total
		}
		m.lastPRTotals = make(map[string]int, len(s.userPRTotals))
		for user, total := range s.userPRTotals {
			m.lastPRTotals[user] = total
		}
		m.seededTotals = true
	}

	deltas, hourTasks := cumulativeDeltas(s.userTotals, m.lastTotals)
	prDeltas, _ := cumulativeDeltas(s.userPRTotals, m.lastPRTotals)

	m.queueDepth = capRing(append(m.queueDepth, s.queueDepth))
	m.fleetSize = capRing(append(m.fleetSize, s.fleetSize))
	m.tasksDone = capRing(append(m.tasksDone, hourTasks))
	m.collectedAt = s.now.Truncate(time.Hour)

	// Zero-fill EVERY per-user ring onto the shared timeline, so index i means the
	// same hour in every series. Users seen this tick get their delta (often 0);
	// users known only from earlier buckets get an explicit 0 so their ring does
	// not fall behind. padRing aligns a newcomer's first bucket to the tail of the
	// timeline instead of the head.
	want := len(m.tasksDone)
	appendUserBuckets(m.perUserDone, deltas, want)
	// The PR ring gets the same bucket per tick but no left-padding: its length
	// is the number of hours it has actually measured (see the package comment).
	appendUserBuckets(m.perUserPR, prDeltas, 0)

	m.persistLocked()
}

// cumulativeDeltas diffs this tick's cumulative per-user totals against the
// previous tick's, updating last in place, and returns the per-user deltas plus
// their sum (the shared series' bucket). A drop in a cumulative count — a profile
// reset or re-registration — books as zero for the hour, never a negative bucket.
func cumulativeDeltas(totals, last map[string]int) (map[string]int, int) {
	sum := 0
	deltas := make(map[string]int, len(totals))
	for user, total := range totals {
		delta := total - last[user]
		if delta < 0 {
			delta = 0
		}
		deltas[user] = delta
		sum += delta
		last[user] = total
	}
	return deltas, sum
}

// appendUserBuckets appends this tick's bucket to every ring in one per-user
// map: the delta for users present in this tick's sample, an explicit zero for
// users known only from earlier buckets. want > 0 first left-pads each ring so
// it ends up exactly want long (the index-aligned per_user_done contract);
// want == 0 appends without padding (the tail-aligned per_user_pr contract).
//
// A contributor whose profile is gone AND who has nothing left inside the
// retained window carries no information; they are dropped rather than growing
// the map with all-zero rings forever. Anyone still registered is kept, so
// their sparkline reads as a truthful flat line rather than vanishing. Mutates
// rings in place; callers hold m.mu.
func appendUserBuckets(rings map[string][]int, deltas map[string]int, want int) {
	grow := func(ring []int, n int) []int {
		if want > 0 {
			ring = padRing(ring, want-1)
		}
		return capRing(append(ring, n))
	}
	for user, delta := range deltas {
		rings[user] = grow(rings[user], delta)
	}
	for user, ring := range rings {
		if _, fresh := deltas[user]; fresh {
			continue
		}
		if allZero(ring) {
			delete(rings, user)
			continue
		}
		rings[user] = grow(ring, 0)
	}
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

// TasksCompleted7d sums the hourly tasks_done ring into the single number the
// hub's quadrant scorer wants. The 168 buckets stay local deliberately: shipping
// them every two minutes on the heartbeat to reconstruct one integer on the hub
// would be pure waste.
//
// ok is false when this Server has no metrics store yet, which is NOT the same
// as a hive with no contributors — the latter is a real, reportable zero. It
// deliberately does NOT go through contributeMetricsStore(), because that
// accessor lazily CREATES the store and reads the PVC; the heartbeat is a
// read-only observer and must not conjure a store as a side effect of watching.
func (s *Server) TasksCompleted7d() (int, bool) {
	if s == nil {
		return 0, false
	}
	// Racing the sync.Once is fine: a nil here just means "not built yet", and
	// the next beat (two minutes later) picks it up.
	store := s.contributeMetrics
	if store == nil {
		return 0, false
	}
	return store.tasksCompleted7d()
}

// tasksCompleted7d sums the tasks_done ring. Reports ok=false only when the
// store holds no measurement at all — never rolled up AND nothing restored from
// the PVC — so that a genuine zero (contributors connected, none finished
// anything) stays distinguishable from "this spoke has not measured yet".
//
// A store that restored history from disk is reportable even before its first
// rollup: load() leaves seededTotals false, but the restored buckets are real
// past measurements. The seed guard only protects the FIRST live bucket from
// booking a whole cumulative total as one hour's work (see rollup), and it does
// that by seeding baselines before any delta is computed — so no unseeded
// partial can reach the ring in the first place.
func (m *metricsStore) tasksCompleted7d() (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.seededTotals && len(m.tasksDone) == 0 {
		return 0, false
	}
	total := 0
	for _, n := range m.tasksDone {
		total += n
	}
	return total, true
}

// recentWindowBuckets is the trailing window the "issues worked (24h)" figure
// sums — 24 hourly buckets. Named rather than inlined so the endpoint, the
// response field name and the tests cannot drift apart.
const recentWindowBuckets = 24

// userRecent sums the most recent `buckets` hourly buckets of one contributor's
// completion ring. It returns the sum and how many buckets it actually had to
// sum: a spoke that has only been up for six hours can report a truthful "6" for
// covered rather than pretending the number spans a full day.
//
// Correct ONLY because the rings are index-aligned with the shared timeline (see
// rollup): the last N buckets are the last N hours for every contributor. Before
// #6543 they were not, and this function would have been quietly wrong.
//
// known is false when this contributor has no series at all — never rolled up,
// or registered since the last tick. That is distinct from a real zero (present
// on the timeline, finished nothing), which the panel words differently.
func (m *metricsStore) userRecent(user string, buckets int) (sum int, covered int, known bool) {
	if m == nil {
		return 0, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return recentFromRings(m.perUserDone, user, buckets)
}

// userRecentPR is userRecent over the per-user PR ring: how many of this
// contributor's completions in the trailing `buckets` hours produced a verified
// pull request (#7894). Same alignment guarantee, same covered/known contract.
// known is false — not zero — for a contributor whose PR ring has not started
// yet, which is every contributor between an upgrade that introduced the ring
// and the first rollup after it.
func (m *metricsStore) userRecentPR(user string, buckets int) (sum int, covered int, known bool) {
	if m == nil {
		return 0, 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return recentFromRings(m.perUserPR, user, buckets)
}

// recentFromRings is the shared trailing-window sum behind userRecent and
// userRecentPR. Callers hold m.mu.
func recentFromRings(rings map[string][]int, user string, buckets int) (sum int, covered int, known bool) {
	if user == "" || buckets <= 0 {
		return 0, 0, false
	}
	ring, ok := rings[user]
	if !ok {
		return 0, 0, false
	}
	start := len(ring) - buckets
	if start < 0 {
		start = 0
	}
	for _, n := range ring[start:] {
		sum += n
	}
	return sum, len(ring) - start, true
}

// sampleMetricsInputs reads the live values the rollup buckets: the admitted
// ready-work queue length, the connected clanker count, and each contributor's
// cumulative completion and with-PR totals. All reads are cheap and
// side-effect-free.
func (s *Server) sampleMetricsInputs() rollupSample {
	sample := rollupSample{
		userTotals:   make(map[string]int),
		userPRTotals: make(map[string]int),
		now:          time.Now(),
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
		sample.userPRTotals[p.GitHubUsername] = p.TasksWithPR
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
