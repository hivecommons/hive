package intent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// CountersPath is the durable evaluated-vs-would-block intent counter file
// the dashboard reads (#12362). It lives beside the proxy's
// write-counters.json.
const CountersPath = "/var/run/hive-metrics/intent-counters.json"

// countersFlushInterval is how often pending counts are merged into the
// file; shutdown flushes the remainder.
const countersFlushInterval = time.Minute

// TierCounts is evaluated (Attempted) / would-block (Blocked) for one bucket.
type TierCounts struct {
	Attempted int64 `json:"attempted"`
	Blocked   int64 `json:"blocked"`
}

// WeekCounts is one week of intent evaluations. Tiers is keyed by TierKey
// and BlockedByReason by ReasonCode*.
type WeekCounts struct {
	Attempted       int64                 `json:"attempted"`
	Blocked         int64                 `json:"blocked"`
	Tiers           map[string]TierCounts `json:"tiers"`
	BlockedByReason map[string]int64      `json:"blocked_by_reason"`
}

// DurableCounts is the persisted file. Totals are cumulative across
// restarts; Weeks is keyed by the UTC Monday (YYYY-MM-DD) starting the ISO
// week. Observation only: nothing here gates a merge.
type DurableCounts struct {
	UpdatedAt time.Time             `json:"updated_at"`
	Totals    map[string]EvalCounts `json:"totals"`
	Weeks     map[string]WeekCounts `json:"weeks"`
}

func newDurableCounts() DurableCounts {
	return DurableCounts{Totals: map[string]EvalCounts{}, Weeks: map[string]WeekCounts{}}
}

// CounterWeekStart returns the UTC Monday of t's ISO week as YYYY-MM-DD.
func CounterWeekStart(t time.Time) string {
	t = t.UTC()
	offset := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -offset).Format("2006-01-02")
}

// add records one evaluation into d.
func (d *DurableCounts) add(tier, blockReason string, now time.Time) {
	if d.Totals == nil {
		d.Totals = map[string]EvalCounts{}
	}
	if d.Weeks == nil {
		d.Weeks = map[string]WeekCounts{}
	}
	ec := d.Totals[tier]
	if ec.BlockedByReason == nil {
		ec.BlockedByReason = map[string]int64{}
	}
	week := CounterWeekStart(now)
	w := d.Weeks[week]
	if w.Tiers == nil {
		w.Tiers = map[string]TierCounts{}
	}
	if w.BlockedByReason == nil {
		w.BlockedByReason = map[string]int64{}
	}
	tc := w.Tiers[tier]
	ec.Attempted++
	w.Attempted++
	tc.Attempted++
	if blockReason != "" {
		ec.Blocked++
		ec.BlockedByReason[blockReason]++
		w.Blocked++
		w.BlockedByReason[blockReason]++
		tc.Blocked++
	}
	d.Totals[tier] = ec
	w.Tiers[tier] = tc
	d.Weeks[week] = w
}

// merge adds other's counts into d.
func (d *DurableCounts) merge(other DurableCounts) {
	if d.Totals == nil {
		d.Totals = map[string]EvalCounts{}
	}
	if d.Weeks == nil {
		d.Weeks = map[string]WeekCounts{}
	}
	for tier, o := range other.Totals {
		ec := d.Totals[tier]
		if ec.BlockedByReason == nil {
			ec.BlockedByReason = map[string]int64{}
		}
		ec.Attempted += o.Attempted
		ec.Blocked += o.Blocked
		for k, v := range o.BlockedByReason {
			ec.BlockedByReason[k] += v
		}
		d.Totals[tier] = ec
	}
	for week, o := range other.Weeks {
		w := d.Weeks[week]
		if w.Tiers == nil {
			w.Tiers = map[string]TierCounts{}
		}
		if w.BlockedByReason == nil {
			w.BlockedByReason = map[string]int64{}
		}
		w.Attempted += o.Attempted
		w.Blocked += o.Blocked
		for tier, tc := range o.Tiers {
			cur := w.Tiers[tier]
			cur.Attempted += tc.Attempted
			cur.Blocked += tc.Blocked
			w.Tiers[tier] = cur
		}
		for k, v := range o.BlockedByReason {
			w.BlockedByReason[k] += v
		}
		d.Weeks[week] = w
	}
}

func (d DurableCounts) empty() bool {
	return len(d.Totals) == 0 && len(d.Weeks) == 0
}

// LoadDurableCounts reads the file at path. A missing file is an empty
// result, not an error.
func LoadDurableCounts(path string) (DurableCounts, error) {
	d := newDurableCounts()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return newDurableCounts(), fmt.Errorf("parse %s: %w", path, err)
	}
	return d, nil
}

// Flush merges the counts recorded since the last flush into the file at
// path and clears them. On failure the pending counts are kept for the next
// try.
func (c *Counters) Flush(path string, now time.Time) error {
	c.mu.Lock()
	pending := c.pending
	c.pending = DurableCounts{}
	c.mu.Unlock()
	if pending.empty() {
		return nil
	}
	err := mergeIntoFile(path, pending, now)
	if err != nil {
		c.mu.Lock()
		c.pending.merge(pending)
		c.mu.Unlock()
	}
	return err
}

func mergeIntoFile(path string, pending DurableCounts, now time.Time) error {
	cur, err := LoadDurableCounts(path)
	if err != nil {
		// A corrupt file must not stall persistence forever; start over.
		cur = newDurableCounts()
	}
	cur.merge(pending)
	cur.UpdatedAt = now.UTC()
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StartPersistence flushes the counters to path every minute and returns a
// stop func that halts the loop and does a final flush. Failures are logged
// and never affect evaluation.
func (c *Counters) StartPersistence(path string, logger *slog.Logger) (stop func()) {
	if logger == nil {
		logger = slog.Default()
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(countersFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := c.Flush(path, time.Now()); err != nil {
					logger.Warn("intent counters flush failed", "error", err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		select {
		case <-done:
		default:
			close(done)
		}
		<-finished
		if err := c.Flush(path, time.Now()); err != nil {
			logger.Warn("intent counters final flush failed", "error", err)
		}
	}
}

// FlushEvaluationCounters merges the process-wide pending counts into path now.
func FlushEvaluationCounters(path string) error {
	return defaultCounters.Flush(path, time.Now())
}

// StartEvaluationCounterPersistence runs StartPersistence on the
// process-wide counters.
func StartEvaluationCounterPersistence(path string, logger *slog.Logger) (stop func()) {
	return defaultCounters.StartPersistence(path, logger)
}
