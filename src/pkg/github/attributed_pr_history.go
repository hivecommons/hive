package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Attributed PR history: the durable record behind the PRs-by-model and
// Most-effective-models cards' 30d and All windows.
//
// The closed attributed-PR scan (fetchAttributedClosedPRs) is bounded to the
// last attributedClosedPRDefaultLookback of updates and five pages per pass
// (#9253/#9255), and its result lived only in memory. A PR closed and left
// alone for longer than the lookback dropped out of every window and a restart
// lost them all, so models lost different shares of their PRs depending on
// when they were used. This store keeps every attributed PR the enumeration
// sees, open or closed, and a one-time backfill walks the closed history back
// to the backfill horizon.
//
// Format: one JSON file (AttributedPRHistoryPath) written atomically
// (tmp + rename), like the PR rework cache beside it. It holds parsed
// attribution and outcome fields only, never a PR body. The live scan stays
// exactly as bounded as before; only the backfill reaches further, and only
// within its own per-pass budget.
const (
	// AttributedPRHistoryFile is the store's file name under ReviewLinksDir.
	AttributedPRHistoryFile          = "attributed-pr-history.json"
	attributedPRHistorySchemaVersion = 1

	// Closed records whose PR was created longer ago than the retention are
	// dropped when the store is written, and at most
	// attributedPRHistoryMaxRecords closed records are kept (oldest created
	// go first). Either prune moves the coverage start forward, so a window
	// reaching past it reads as partial.
	attributedPRHistoryRetention  = 400 * 24 * time.Hour
	attributedPRHistoryMaxRecords = 20000

	// attributedPRHistoryScanStamp is how stale the persisted last-scan time
	// may get before a scan alone rewrites the file. Continuity is judged
	// against it, so a stale stamp can only make a gap look earlier, never
	// hide one.
	attributedPRHistoryScanStamp = time.Hour

	attributedPRBackfillLookbackEnv = "HIVE_ATTRIBUTED_PR_BACKFILL_LOOKBACK"
	// attributedPRBackfillDefaultLookback is how far back (by PR creation)
	// the one-time backfill walks: the longest bounded window the cards offer.
	attributedPRBackfillDefaultLookback = 30 * 24 * time.Hour
	// Per enumeration pass, across all repos: closed-PR list pages, and PRs
	// whose rework is measured (about three calls each). Only spent while
	// the API budget is normal.
	attributedPRBackfillPagesPerPass  = 2
	attributedPRBackfillReworkPerPass = 5
	// attributedPRBackfillReworkTries bounds rework retries for one PR, so a
	// PR whose reviews cannot be read does not hold the backfill open.
	attributedPRBackfillReworkTries = 3
)

// AttributedPRHistoryPath is where the hive keeps its attributed PR history.
var AttributedPRHistoryPath = filepath.Join(ReviewLinksDir, AttributedPRHistoryFile)

// AttributedPRCoverage says how far back the attributed PR history is
// complete across the client's active repos, in PR creation time: every
// attributed PR created at or after Since is held.
type AttributedPRCoverage struct {
	// Available is false when no history store is configured; callers then
	// fall back to the live scan's lookback.
	Available bool
	// Since is the coverage start; zero when Full.
	Since time.Time
	// Full means the history holds every attributed PR the repos ever had.
	Full bool
}

// attributedPRRecord is one stored PR: the attribution trailer's parsed
// values and the outcome and rework fields the PRs-by-model cards read.
type attributedPRRecord struct {
	Repo       string        `json:"repo"`
	Number     int           `json:"number"`
	Title      string        `json:"title,omitempty"`
	URL        string        `json:"url,omitempty"`
	Labels     []string      `json:"labels,omitempty"`
	Agent      string        `json:"agent,omitempty"`
	Backend    string        `json:"backend,omitempty"`
	Model      string        `json:"model,omitempty"`
	Effort     string        `json:"effort,omitempty"`
	State      string        `json:"state,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitzero"`
	ClosedAt   time.Time     `json:"closed_at,omitzero"`
	MergedAt   time.Time     `json:"merged_at,omitzero"`
	Supersedes int           `json:"supersedes,omitempty"`
	Rework     PRReworkStats `json:"rework,omitzero"`
	// ReworkKnown marks Rework as the closed PR's measurement (from the
	// rework cache or a backfill fetch), not a zero left by a failed fetch.
	ReworkKnown bool      `json:"rework_known,omitempty"`
	ReworkTries int       `json:"rework_tries,omitempty"`
	FirstSeenAt time.Time `json:"first_seen_at,omitzero"`
}

func (r *attributedPRRecord) closed() bool {
	return !r.MergedAt.IsZero() || !r.ClosedAt.IsZero() || strings.EqualFold(r.State, "closed")
}

func (r *attributedPRRecord) reworkPending() bool {
	return r.closed() && !r.ReworkKnown && r.ReworkTries < attributedPRBackfillReworkTries
}

func (r *attributedPRRecord) pullRequest() PullRequest {
	return PullRequest{
		Repo:           r.Repo,
		Number:         r.Number,
		Title:          r.Title,
		URL:            r.URL,
		Labels:         append([]string(nil), r.Labels...),
		CreatedAt:      r.CreatedAt,
		ClosedAt:       r.ClosedAt,
		MergedAt:       r.MergedAt,
		State:          r.State,
		HiveAttributed: true,
		HiveAgent:      r.Agent,
		HiveBackend:    r.Backend,
		HiveModel:      r.Model,
		HiveEffort:     r.Effort,
		Supersedes:     r.Supersedes,
		Rework:         r.Rework,
	}
}

func attributedPRRecordFrom(pr PullRequest) *attributedPRRecord {
	return &attributedPRRecord{
		Repo:       pr.Repo,
		Number:     pr.Number,
		Title:      pr.Title,
		URL:        pr.URL,
		Labels:     append([]string(nil), pr.Labels...),
		Agent:      pr.HiveAgent,
		Backend:    pr.HiveBackend,
		Model:      pr.HiveModel,
		Effort:     pr.HiveEffort,
		State:      pr.State,
		CreatedAt:  utcOrZero(pr.CreatedAt),
		ClosedAt:   utcOrZero(pr.ClosedAt),
		MergedAt:   utcOrZero(pr.MergedAt),
		Supersedes: pr.Supersedes,
		Rework:     pr.Rework,
	}
}

func utcOrZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}

// attributedPRRepoState is one repo's scan continuity and backfill progress.
type attributedPRRepoState struct {
	// LastScanAt is a live closed-PR scan's start time, at most
	// attributedPRHistoryScanStamp older than the latest scan.
	LastScanAt time.Time `json:"last_scan_at,omitzero"`
	// RunStart is the first scan of the current unbroken run of scans (each
	// reached back, by updated time, to the one before it). RunFloor is the
	// updated-time floor that first scan reached; every closed PR created at
	// or after it has been seen since. RunFull is set once a scan in the run
	// listed the repo's whole closed-PR history.
	RunStart time.Time                 `json:"run_start,omitzero"`
	RunFloor time.Time                 `json:"run_floor,omitzero"`
	RunFull  bool                      `json:"run_full,omitempty"`
	Backfill attributedPRBackfillState `json:"backfill"`
}

// attributedPRBackfillState is the resumable cursor of the one-time backfill:
// closed PRs sorted by creation, newest first, down to Horizon.
type attributedPRBackfillState struct {
	StartedAt   time.Time `json:"started_at,omitzero"`
	Horizon     time.Time `json:"horizon,omitzero"`
	NextPage    int       `json:"next_page,omitempty"`
	Listed      bool      `json:"listed,omitempty"`
	ReachedEnd  bool      `json:"reached_end,omitempty"`
	Done        bool      `json:"done,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

type attributedPRHistoryFile struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	// PrunedThrough: closed records created before it may have been
	// dropped by retention, so coverage never starts earlier.
	PrunedThrough time.Time                         `json:"pruned_through,omitzero"`
	Repos         map[string]*attributedPRRepoState `json:"repos"`
	Records       map[string]*attributedPRRecord    `json:"records"`
}

// attributedPRHistory is the in-memory store over one file. Every method is
// nil-safe and best-effort: a store problem is logged, never returned to
// enumeration.
type attributedPRHistory struct {
	mu       sync.Mutex
	path     string
	logger   *slog.Logger
	file     attributedPRHistoryFile
	dirty    bool
	lastWarn string
}

// attributedPRHistories shares one store per path across clients, so a
// rebuilt App client and the one it replaces never write diverging copies.
var attributedPRHistories sync.Map

func attributedPRHistoryAt(path string, logger *slog.Logger) *attributedPRHistory {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if v, ok := attributedPRHistories.Load(path); ok {
		return v.(*attributedPRHistory)
	}
	v, _ := attributedPRHistories.LoadOrStore(path, loadAttributedPRHistory(path, logger))
	return v.(*attributedPRHistory)
}

// loadAttributedPRHistory reads path. A missing file is an empty store; an
// unreadable, malformed or other-schema file is logged and treated as empty
// (the backfill then runs again).
func loadAttributedPRHistory(path string, logger *slog.Logger) *attributedPRHistory {
	h := &attributedPRHistory{path: path, logger: logger}
	h.file = attributedPRHistoryFile{SchemaVersion: attributedPRHistorySchemaVersion}
	defer h.ensureMapsLocked()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Warn("attributed PR history: file unreadable; starting empty", "path", path, "error", err)
		}
		return h
	}
	var loaded attributedPRHistoryFile
	if err := json.Unmarshal(raw, &loaded); err != nil || loaded.SchemaVersion != attributedPRHistorySchemaVersion {
		if logger != nil {
			logger.Warn("attributed PR history: file malformed or from another schema; starting empty",
				"path", path, "schema_version", loaded.SchemaVersion, "error", err)
		}
		return h
	}
	h.file = loaded
	return h
}

func (h *attributedPRHistory) ensureMapsLocked() {
	if h.file.Repos == nil {
		h.file.Repos = map[string]*attributedPRRepoState{}
	}
	if h.file.Records == nil {
		h.file.Records = map[string]*attributedPRRecord{}
	}
}

func (h *attributedPRHistory) repoLocked(repo string) *attributedPRRepoState {
	h.ensureMapsLocked()
	st := h.file.Repos[repo]
	if st == nil {
		st = &attributedPRRepoState{}
		h.file.Repos[repo] = st
	}
	return st
}

// noteScan records a finished live closed-PR scan of repo that started at
// scanAt. floor is the oldest updated time the scan is complete back to;
// complete means it listed the repo's whole closed history. A scan whose floor
// is newer than the previous scan leaves a stretch nobody listed, so the run
// (and the coverage it carries) starts over.
func (h *attributedPRHistory) noteScan(repo string, scanAt, floor time.Time, complete bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repoLocked(repo)
	runChanged := false
	switch {
	case complete:
		if st.RunStart.IsZero() {
			st.RunStart = scanAt
		}
		runChanged = !st.RunFull
		st.RunFull = true
	case st.LastScanAt.IsZero() || floor.After(st.LastScanAt):
		st.RunStart, st.RunFloor, st.RunFull = scanAt, floor, false
		runChanged = true
	}
	if runChanged || st.LastScanAt.IsZero() || scanAt.Sub(st.LastScanAt) >= attributedPRHistoryScanStamp {
		st.LastScanAt = scanAt
		h.dirty = true
	}
}

// upsert stores the attributed PRs of one pass. A record that stays open is
// not rewritten (open PRs are served from the live listing); a closed one
// takes its rework from the rework cache, keeping a measured rework over a
// failed fetch's zero.
func (h *attributedPRHistory) upsert(prs []PullRequest, reworkCache map[string]PRReworkStats, now time.Time) {
	if h == nil || len(prs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureMapsLocked()
	for _, pr := range prs {
		if !pr.HiveAttributed || strings.TrimSpace(pr.Repo) == "" || pr.Number <= 0 {
			continue
		}
		key := ReviewLinkKey(pr.Repo, pr.Number)
		rec := attributedPRRecordFrom(pr)
		if rec.closed() {
			if cached, ok := reworkCache[key]; ok {
				rec.Rework, rec.ReworkKnown = cached, true
			}
		}
		old := h.file.Records[key]
		if old == nil {
			rec.FirstSeenAt = now.UTC()
			h.file.Records[key] = rec
			h.dirty = true
			continue
		}
		if !old.closed() && !rec.closed() {
			continue
		}
		rec.FirstSeenAt = old.FirstSeenAt
		if !rec.ReworkKnown {
			if old.ReworkKnown && rec.closed() {
				rec.Rework, rec.ReworkKnown = old.Rework, true
			} else {
				rec.ReworkTries = old.ReworkTries
			}
		}
		if !sameAttributedPRRecord(old, rec) {
			h.file.Records[key] = rec
			h.dirty = true
		}
	}
}

func sameAttributedPRRecord(a, b *attributedPRRecord) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// flush prunes and writes the store when anything changed.
func (h *attributedPRHistory) flush(now time.Time) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.dirty {
		return nil
	}
	h.pruneLocked(now)
	h.file.SchemaVersion = attributedPRHistorySchemaVersion
	h.file.GeneratedAt = now.UTC()
	raw, err := json.Marshal(h.file)
	if err != nil {
		return fmt.Errorf("encoding attributed PR history: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return fmt.Errorf("creating attributed PR history directory: %w", err)
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("writing attributed PR history: %w", err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing attributed PR history: %w", err)
	}
	h.dirty = false
	return nil
}

// pruneLocked drops closed records past the retention, then the oldest closed
// records beyond the cap, raising PrunedThrough to cover what went.
func (h *attributedPRHistory) pruneLocked(now time.Time) {
	cutoff := now.Add(-attributedPRHistoryRetention).UTC()
	var closed []string
	for key, rec := range h.file.Records {
		if !rec.closed() {
			continue
		}
		if !rec.CreatedAt.IsZero() && rec.CreatedAt.Before(cutoff) {
			delete(h.file.Records, key)
			if cutoff.After(h.file.PrunedThrough) {
				h.file.PrunedThrough = cutoff
			}
			continue
		}
		closed = append(closed, key)
	}
	if len(closed) <= attributedPRHistoryMaxRecords {
		return
	}
	sort.Slice(closed, func(i, j int) bool {
		a, b := h.file.Records[closed[i]], h.file.Records[closed[j]]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return closed[i] < closed[j]
	})
	for _, key := range closed[:len(closed)-attributedPRHistoryMaxRecords] {
		if through := h.file.Records[key].CreatedAt.Add(time.Nanosecond); through.After(h.file.PrunedThrough) {
			h.file.PrunedThrough = through
		}
		delete(h.file.Records, key)
	}
}

// coveredSinceLocked is one repo's coverage start. ok is false before the
// repo's first live scan.
func (h *attributedPRHistory) coveredSinceLocked(repo string) (since time.Time, full, ok bool) {
	st := h.file.Repos[repo]
	if st == nil || st.LastScanAt.IsZero() {
		return time.Time{}, false, false
	}
	if st.RunFull {
		return time.Time{}, true, true
	}
	since = st.RunFloor
	// The backfill's horizon counts only while the run of live scans that
	// was under way when it started is unbroken: those scans saw every PR
	// that closed after the walk passed its place in the list.
	if bf := st.Backfill; bf.Done && !bf.StartedAt.IsZero() && !st.RunStart.After(bf.StartedAt) {
		if bf.ReachedEnd {
			return time.Time{}, true, true
		}
		if bf.Horizon.Before(since) {
			since = bf.Horizon
		}
	}
	return since, false, true
}

// snapshot returns the closed records of repos as PullRequests and the
// coverage across repos.
func (h *attributedPRHistory) snapshot(repos []string, now time.Time) ([]PullRequest, AttributedPRCoverage) {
	if h == nil || len(repos) == 0 {
		return nil, AttributedPRCoverage{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ensureMapsLocked()
	cov := AttributedPRCoverage{Available: true, Full: true}
	want := make(map[string]bool, len(repos))
	for _, repo := range repos {
		want[repo] = true
		since, full, ok := h.coveredSinceLocked(repo)
		switch {
		case !ok:
			cov.Full, cov.Since = false, now
		case !full:
			if cov.Full || since.After(cov.Since) {
				cov.Since = since
			}
			cov.Full = false
		}
	}
	if !h.file.PrunedThrough.IsZero() {
		if cov.Full || h.file.PrunedThrough.After(cov.Since) {
			cov.Since = h.file.PrunedThrough
		}
		cov.Full = false
	}
	if cov.Full {
		cov.Since = time.Time{}
	}
	var out []PullRequest
	for _, rec := range h.file.Records {
		if want[rec.Repo] && rec.closed() {
			out = append(out, rec.pullRequest())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out, cov
}

// backfillStart returns repo's backfill cursor, starting (or restarting) it
// when needed. ok is false when the repo needs no backfill: done, or no live
// scan yet to anchor it.
func (h *attributedPRHistory) backfillStart(repo string, now time.Time, lookback time.Duration) (attributedPRBackfillState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.repoLocked(repo)
	bf := &st.Backfill
	if bf.Done || st.LastScanAt.IsZero() {
		return *bf, false
	}
	if st.RunFull {
		// The live scan already listed the whole closed history; only the
		// rework measurements remain.
		if !bf.Listed {
			*bf = attributedPRBackfillState{StartedAt: now.UTC(), Listed: true, ReachedEnd: true}
			h.dirty = true
		}
		return *bf, true
	}
	// A gap in the live scans after the walk began means PRs that closed in
	// the gap may sit behind the cursor; walk again from the top.
	if bf.StartedAt.IsZero() || st.RunStart.After(bf.StartedAt) {
		*bf = attributedPRBackfillState{StartedAt: now.UTC(), Horizon: now.Add(-lookback).UTC(), NextPage: 1}
		h.dirty = true
	}
	return *bf, true
}

// backfillPage stores one listed page and advances the cursor.
func (h *attributedPRHistory) backfillPage(repo string, prs []PullRequest, reworkCache map[string]PRReworkStats, nextPage int, reachedHorizon bool, now time.Time) {
	h.upsert(prs, reworkCache, now)
	h.mu.Lock()
	defer h.mu.Unlock()
	bf := &h.repoLocked(repo).Backfill
	bf.NextPage = nextPage
	if reachedHorizon || nextPage == 0 {
		bf.Listed = true
		bf.ReachedEnd = !reachedHorizon
	}
	h.dirty = true
}

// reworkPending lists repo's closed records still waiting for a rework
// measurement, merged first, newest first.
func (h *attributedPRHistory) reworkPending(repo string) []PullRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []PullRequest
	for _, rec := range h.file.Records {
		if rec.Repo == repo && rec.reworkPending() {
			out = append(out, rec.pullRequest())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		mi, mj := !out[i].MergedAt.IsZero(), !out[j].MergedAt.IsZero()
		if mi != mj {
			return mi
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Number > out[j].Number
	})
	return out
}

// setRework records a rework measurement (ok) or one more failed try.
func (h *attributedPRHistory) setRework(key string, rework PRReworkStats, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.file.Records[key]
	if rec == nil {
		return
	}
	if ok {
		rec.Rework, rec.ReworkKnown = rework, true
	} else {
		rec.ReworkTries++
	}
	h.dirty = true
}

// backfillFinish marks repo's backfill complete once the walk reached its
// horizon and no closed record still waits for rework.
func (h *attributedPRHistory) backfillFinish(repo string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bf := &h.repoLocked(repo).Backfill
	if bf.Done || !bf.Listed {
		return
	}
	for _, rec := range h.file.Records {
		if rec.Repo == repo && rec.reworkPending() {
			return
		}
	}
	bf.Done, bf.CompletedAt = true, now.UTC()
	h.dirty = true
}

// warn logs a store problem once per distinct message.
func (h *attributedPRHistory) warn(msg string, err error, args ...any) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	repeat := h.lastWarn == err.Error()
	h.lastWarn = err.Error()
	h.mu.Unlock()
	if repeat || h.logger == nil {
		return
	}
	h.logger.Warn(msg, append(args, "path", h.path, "error", err)...)
}

// SetAttributedPRHistoryPath persists the attributed PR history at path
// (loaded now): the closed PRs behind the PRs-by-model cards' 30d and All
// windows survive the bounded live scan and a restart, and the one-time
// backfill runs from it. Empty disables the store; the cards then rest on the
// live scan alone, as before.
func (c *Client) SetAttributedPRHistoryPath(path string) {
	if c == nil {
		return
	}
	c.attributedHistory.Store(attributedPRHistoryAt(path, c.logger))
}

func (c *Client) attributedPRHistory() *attributedPRHistory {
	if c == nil {
		return nil
	}
	return c.attributedHistory.Load()
}

// AttributedPRHistory returns the stored closed attributed PRs of the active
// repos and how far back they are complete. Coverage.Available is false when
// no store is configured. Callers merge these under the live enumeration's
// records, which carry the freshest state.
func (c *Client) AttributedPRHistory() ([]PullRequest, AttributedPRCoverage) {
	return c.attributedPRHistory().snapshot(c.activeRepos(), time.Now())
}

// recordAttributedPRHistory stores one repo's attributed PRs from this pass
// (after rework enrichment, so closed records carry their measurement). The
// write happens once per pass in syncAttributedPRHistory.
func (c *Client) recordAttributedPRHistory(prs []PullRequest) {
	h := c.attributedPRHistory()
	if h == nil || len(prs) == 0 {
		return
	}
	h.upsert(prs, loadPRReworkCache(), time.Now())
}

// syncAttributedPRHistory runs the enumeration pass's backfill step and
// writes the store. Errors are logged, never returned: the history is a
// dashboard aid and must not fail enumeration.
func (c *Client) syncAttributedPRHistory(ctx context.Context, repos []string) {
	h := c.attributedPRHistory()
	if h == nil {
		return
	}
	now := time.Now()
	c.backfillAttributedPRHistory(ctx, h, repos, now)
	if err := h.flush(now); err != nil {
		h.warn("attributed PR history: write failed; keeping it in memory and retrying next pass", err)
	}
}

func attributedPRBackfillLookback() time.Duration {
	return lookbackFromEnv(attributedPRBackfillLookbackEnv, attributedPRBackfillDefaultLookback)
}

// attributedPRBackfillAllowed is the backfill's rate-limit gate: it spends
// calls only while the API budget is normal, leaving conserve and critical
// headroom to the work that unsticks items.
func (c *Client) attributedPRBackfillAllowed(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	mode, _ := c.APIBudgetMode()
	return mode == APIBudgetNormal
}

// backfillAttributedPRHistory is one pass of the one-time backfill: at most
// attributedPRBackfillPagesPerPass closed-PR pages and
// attributedPRBackfillReworkPerPass rework measurements across all repos,
// resuming from each repo's saved cursor. A repo whose backfill is done costs
// nothing, so once every repo is done a pass makes no GitHub call.
func (c *Client) backfillAttributedPRHistory(ctx context.Context, h *attributedPRHistory, repos []string, now time.Time) {
	if h == nil || len(repos) == 0 {
		return
	}
	ctx = WithPacingClass(ctx, PacingLow)
	lookback := attributedPRBackfillLookback()
	pages, reworks := attributedPRBackfillPagesPerPass, attributedPRBackfillReworkPerPass
	var reworkCache map[string]PRReworkStats
	reworkCacheChanged := false
	defer func() {
		if reworkCacheChanged {
			storePRReworkCache(reworkCache)
		}
	}()
	for _, repo := range repos {
		if pages <= 0 && reworks <= 0 {
			return
		}
		bf, ok := h.backfillStart(repo, now, lookback)
		if !ok {
			continue
		}
		if reworkCache == nil {
			reworkCache = loadPRReworkCache()
		}
		stopped := false
		if !bf.Listed && pages > 0 {
			pages, stopped = c.backfillAttributedPRPages(ctx, h, repo, bf, pages, reworkCache, now)
		}
		if !stopped && reworks > 0 {
			var changed bool
			reworks, stopped, changed = c.backfillAttributedPRRework(ctx, h, repo, reworks, reworkCache)
			reworkCacheChanged = reworkCacheChanged || changed
		}
		h.backfillFinish(repo, now)
		if stopped {
			return
		}
	}
}

// backfillAttributedPRPages lists closed PRs by creation, newest first, from
// the cursor until the page budget runs out or the walk passes the horizon.
// stopped reports that the pass should end (budget gate or a listing error,
// retried next pass from the same cursor).
func (c *Client) backfillAttributedPRPages(ctx context.Context, h *attributedPRHistory, repo string, bf attributedPRBackfillState, budget int, reworkCache map[string]PRReworkStats, now time.Time) (int, bool) {
	owner, repoName := c.splitRepo(repo)
	page := bf.NextPage
	if page <= 0 {
		page = 1
	}
	for budget > 0 {
		if !c.attributedPRBackfillAllowed(ctx) {
			return budget, true
		}
		opts := &gh.PullRequestListOptions{
			State:       "closed",
			Sort:        "created",
			Direction:   "desc",
			ListOptions: gh.ListOptions{PerPage: 100, Page: page},
		}
		budget--
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			h.warn("attributed PR history: backfill listing failed; retrying next pass", fmt.Errorf("listing closed PRs for %s/%s: %w", owner, repoName, err), "repo", repo)
			return budget, true
		}
		var recs []PullRequest
		reached := false
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			if pr.GetCreatedAt().Time.Before(bf.Horizon) {
				reached = true
				break
			}
			if meta, ok := ParseAttributionTrailer(pr.GetBody()); ok {
				recs = append(recs, pullRequestAttributionRecord(repo, pr, meta))
			}
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		h.backfillPage(repo, recs, reworkCache, next, reached, now)
		if reached || next == 0 {
			return budget, false
		}
		page = next
	}
	return budget, false
}

// backfillAttributedPRRework measures rework for repo's closed records that
// lack it, through the same fetch and cache the live enrichment uses.
func (c *Client) backfillAttributedPRRework(ctx context.Context, h *attributedPRHistory, repo string, budget int, reworkCache map[string]PRReworkStats) (left int, stopped, cacheChanged bool) {
	for _, pr := range h.reworkPending(repo) {
		key := ReviewLinkKey(pr.Repo, pr.Number)
		if cached, ok := reworkCache[key]; ok {
			h.setRework(key, cached, true)
			continue
		}
		if budget <= 0 {
			return 0, false, cacheChanged
		}
		if !c.attributedPRBackfillAllowed(ctx) {
			return budget, true, cacheChanged
		}
		budget--
		rework, err := c.fetchPRRework(ctx, pr)
		if err != nil {
			h.setRework(key, PRReworkStats{}, false)
			continue
		}
		reworkCache[key] = rework
		cacheChanged = true
		h.setRework(key, rework, true)
	}
	return budget, false, cacheChanged
}
