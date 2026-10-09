package compliance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Posture run triggers.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// PostureRun is one pass over every check.
type PostureRun struct {
	At      time.Time      `json:"at"`
	Trigger string         `json:"trigger"`
	Summary PostureSummary `json:"summary"`
	Results []Result       `json:"results"`
}

// MaxPostureHistoryRuns bounds the history independently of the retention
// window: a year of hourly passes, so the default interval and retention fit
// and a short interval cannot grow the store without bound.
const MaxPostureHistoryRuns = 9000

// postureCompactSlack is how many superseded lines the JSONL file may carry
// before it is rewritten, so pruning does not rewrite the file every pass.
const postureCompactSlack = 48

// PostureHistory is the persisted posture-check history: an append-only JSONL
// file (one PostureRun per line) mirrored in memory, pruned to a retention
// window and MaxPostureHistoryRuns. An empty path keeps it in memory only.
type PostureHistory struct {
	mu        sync.Mutex
	path      string
	retention time.Duration
	maxRuns   int
	runs      []PostureRun
	fileLines int
}

// NewPostureHistory opens the history at path, loading any runs persisted by
// a previous process. Malformed lines are skipped, so a torn final write
// costs one run rather than the history. A missing file is an empty history.
func NewPostureHistory(path string, retention time.Duration, maxRuns int) (*PostureHistory, error) {
	if maxRuns <= 0 || maxRuns > MaxPostureHistoryRuns {
		maxRuns = MaxPostureHistoryRuns
	}
	h := &PostureHistory{path: path, retention: retention, maxRuns: maxRuns}
	if path == "" {
		return h, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return h, nil
		}
		return h, fmt.Errorf("compliance: reading posture history %s: %w", path, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		h.fileLines++
		var run PostureRun
		if json.Unmarshal(line, &run) != nil || run.At.IsZero() {
			continue
		}
		h.runs = append(h.runs, run)
	}
	// Lines superseded before the last compaction are dropped again here,
	// relative to the newest run so loading never depends on the wall clock.
	if n := len(h.runs); n > 0 {
		h.pruneLocked(h.runs[n-1].At)
	}
	return h, nil
}

// SetRetention changes the retention window applied on the next Append.
func (h *PostureHistory) SetRetention(d time.Duration) {
	h.mu.Lock()
	h.retention = d
	h.mu.Unlock()
}

// Append records run, prunes runs older than the retention window (relative
// to run.At) or beyond the cap, and persists. The in-memory history is
// updated even when persisting fails; the error is returned for logging.
func (h *PostureHistory) Append(run PostureRun) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, run)
	h.pruneLocked(run.At)
	if h.path == "" {
		return nil
	}
	if h.fileLines+1-len(h.runs) > postureCompactSlack {
		return h.rewriteLocked()
	}
	line, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("compliance: encoding posture run: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return fmt.Errorf("compliance: creating posture history dir: %w", err)
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("compliance: opening posture history: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("compliance: appending posture history: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("compliance: closing posture history: %w", cerr)
	}
	h.fileLines++
	return nil
}

func (h *PostureHistory) pruneLocked(now time.Time) {
	drop := 0
	if h.retention > 0 {
		cutoff := now.Add(-h.retention)
		for drop < len(h.runs) && h.runs[drop].At.Before(cutoff) {
			drop++
		}
	}
	if over := len(h.runs) - drop - h.maxRuns; over > 0 {
		drop += over
	}
	if drop > 0 {
		h.runs = append([]PostureRun(nil), h.runs[drop:]...)
	}
}

// rewriteLocked replaces the file with exactly the retained runs, atomically
// (temp file + rename in the same directory).
func (h *PostureHistory) rewriteLocked() error {
	var buf bytes.Buffer
	for _, r := range h.runs {
		line, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("compliance: encoding posture run: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("compliance: creating posture history dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(h.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("compliance: compacting posture history: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmpName, h.path)
	}
	if werr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("compliance: compacting posture history: %w", werr)
	}
	h.fileLines = len(h.runs)
	return nil
}

// Latest returns the most recent run.
func (h *PostureHistory) Latest() (PostureRun, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.runs) == 0 {
		return PostureRun{}, false
	}
	return h.runs[len(h.runs)-1], true
}

// Since returns runs at or after t, oldest first, at most limit of the most
// recent (limit <= 0 means all), and whether older matching runs were cut.
func (h *PostureHistory) Since(t time.Time, limit int) ([]PostureRun, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := len(h.runs)
	for i > 0 && !h.runs[i-1].At.Before(t) {
		i--
	}
	out := h.runs[i:]
	truncated := false
	if limit > 0 && len(out) > limit {
		out, truncated = out[len(out)-limit:], true
	}
	return append([]PostureRun(nil), out...), truncated
}

// Len reports how many runs are retained.
func (h *PostureHistory) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.runs)
}

// ErrPostureRunInProgress is returned by Run when another pass is running.
var ErrPostureRunInProgress = errors.New("compliance: a posture run is already in progress")

// PostureRunner runs the checks, records each pass in the history and reports
// pass→fail transitions.
type PostureRunner struct {
	checks  []PostureCheck
	history *PostureHistory
	deps    func() PostureDeps
	// OnTransition is called once per check whose previous result passed and
	// whose new result failed. Optional.
	OnTransition func(prev, cur Result)
	// OnPersistError reports a history write failure. Optional.
	OnPersistError func(error)
	running        sync.Mutex
}

// NewPostureRunner builds a runner over checks (nil means PostureChecks()).
// deps is called at the start of every pass so config changes apply live.
func NewPostureRunner(checks []PostureCheck, history *PostureHistory, deps func() PostureDeps) *PostureRunner {
	if checks == nil {
		checks = PostureChecks()
	}
	if history == nil {
		history, _ = NewPostureHistory("", 0, 0)
	}
	if deps == nil {
		deps = func() PostureDeps { return PostureDeps{} }
	}
	return &PostureRunner{checks: checks, history: history, deps: deps}
}

// History returns the runner's history store.
func (r *PostureRunner) History() *PostureHistory { return r.history }

// Run performs one pass now. It refuses to overlap a pass already running.
func (r *PostureRunner) Run(ctx context.Context, trigger string) (PostureRun, error) {
	if !r.running.TryLock() {
		return PostureRun{}, ErrPostureRunInProgress
	}
	defer r.running.Unlock()
	deps := r.deps().withDefaults()
	r.history.SetRetention(deps.Config.Compliance.PostureHistoryRetentionOrDefault())
	prev, hadPrev := r.history.Latest()
	results := RunPostureChecks(ctx, r.checks, deps)
	run := PostureRun{At: deps.Now().UTC(), Trigger: trigger, Summary: SummarizePosture(results), Results: results}
	if err := r.history.Append(run); err != nil && r.OnPersistError != nil {
		r.OnPersistError(err)
	}
	if hadPrev && r.OnTransition != nil {
		before := map[string]Result{}
		for _, p := range prev.Results {
			before[p.CheckID] = p
		}
		for _, cur := range results {
			if p, ok := before[cur.CheckID]; ok && p.Status == PosturePass && cur.Status == PostureFail {
				r.OnTransition(p, cur)
			}
		}
	}
	return run, nil
}

// PostureInitialDelay is how long Loop waits before its first pass, so a
// restarting hive does not spend its boot on GitHub searches.
const PostureInitialDelay = time.Minute

// Loop runs a pass every interval until ctx is done. schedule is consulted
// before every pass and returns the interval and whether the checks are
// enabled (a framework is selected); a disabled pass is skipped but the loop
// keeps polling so enabling compliance takes effect without a restart.
// after is time.After in production and a fake clock in tests.
func (r *PostureRunner) Loop(ctx context.Context, schedule func() (time.Duration, bool), after func(time.Duration) <-chan time.Time) {
	if after == nil {
		after = time.After
	}
	wait := PostureInitialDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		interval, enabled := schedule()
		if enabled {
			_, _ = r.Run(ctx, TriggerSchedule)
		}
		wait = interval
		if wait <= 0 {
			wait = PostureInitialDelay
		}
	}
}
