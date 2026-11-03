package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteCountersPath is the durable attempted-vs-blocked write counter file
// the dashboard reads (#12069). It lives under /data, the hive's persistent
// volume, so the totals survive a restart: the previous home under
// /var/run/hive-metrics is container-local and was wiped on every roll,
// which left the Evidence card's "all-time" direct-write denominator at a
// few hours' worth of traffic.
const WriteCountersPath = "/data/hive-metrics/write-counters.json"

// LegacyWriteCountersPath is where the counters lived before they were made
// durable. MigrateWriteCounters folds a leftover file into WriteCountersPath.
const LegacyWriteCountersPath = "/var/run/hive-metrics/write-counters.json"

// writeCountersFlushInterval is how often pending counts are merged into the
// file; shutdown flushes the remainder.
const writeCountersFlushInterval = time.Minute

// TierCounts is attempted/blocked for one bucket.
type TierCounts struct {
	Attempted int64 `json:"attempted"`
	Blocked   int64 `json:"blocked"`
}

// WeekCounts is one week of write traffic. Tiers is keyed by ACMM mode and
// BlockedByReason by DenyReason* code.
type WeekCounts struct {
	Attempted       int64                 `json:"attempted"`
	Blocked         int64                 `json:"blocked"`
	Tiers           map[string]TierCounts `json:"tiers"`
	BlockedByReason map[string]int64      `json:"blocked_by_reason"`
}

// DurableWriteCounts is the persisted file. Totals are cumulative across
// restarts; Weeks is keyed by the UTC Monday (YYYY-MM-DD) starting the ISO
// week. Observation only: nothing here gates a write or a merge.
type DurableWriteCounts struct {
	UpdatedAt time.Time `json:"updated_at"`
	// RecordedSince is when this file first started accumulating counts,
	// so a reader can say "direct writes not recorded before <date>" instead
	// of presenting a short-lived total as all-time. Zero on files written
	// before the field existed; EarliestWeek is the fallback.
	RecordedSince time.Time              `json:"recorded_since,omitzero"`
	Totals        map[string]WriteCounts `json:"totals"`
	Weeks         map[string]WeekCounts  `json:"weeks"`
}

func newDurableWriteCounts() DurableWriteCounts {
	return DurableWriteCounts{Totals: map[string]WriteCounts{}, Weeks: map[string]WeekCounts{}}
}

// WriteCounterWeekStart returns the UTC Monday of t's ISO week as YYYY-MM-DD.
func WriteCounterWeekStart(t time.Time) string {
	t = t.UTC()
	offset := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -offset).Format("2006-01-02")
}

// add records one write into d.
func (d *DurableWriteCounts) add(tier, blockReason string, now time.Time) {
	if d.Totals == nil {
		d.Totals = map[string]WriteCounts{}
	}
	if d.Weeks == nil {
		d.Weeks = map[string]WeekCounts{}
	}
	wc := d.Totals[tier]
	if wc.BlockedByReason == nil {
		wc.BlockedByReason = map[string]int64{}
	}
	week := WriteCounterWeekStart(now)
	w := d.Weeks[week]
	if w.Tiers == nil {
		w.Tiers = map[string]TierCounts{}
	}
	if w.BlockedByReason == nil {
		w.BlockedByReason = map[string]int64{}
	}
	tc := w.Tiers[tier]
	wc.Attempted++
	w.Attempted++
	tc.Attempted++
	if blockReason != "" {
		wc.Blocked++
		wc.BlockedByReason[blockReason]++
		w.Blocked++
		w.BlockedByReason[blockReason]++
		tc.Blocked++
	}
	d.Totals[tier] = wc
	w.Tiers[tier] = tc
	d.Weeks[week] = w
}

// merge adds other's counts into d.
func (d *DurableWriteCounts) merge(other DurableWriteCounts) {
	if d.Totals == nil {
		d.Totals = map[string]WriteCounts{}
	}
	if d.Weeks == nil {
		d.Weeks = map[string]WeekCounts{}
	}
	for tier, o := range other.Totals {
		wc := d.Totals[tier]
		if wc.BlockedByReason == nil {
			wc.BlockedByReason = map[string]int64{}
		}
		wc.Attempted += o.Attempted
		wc.Blocked += o.Blocked
		for k, v := range o.BlockedByReason {
			wc.BlockedByReason[k] += v
		}
		d.Totals[tier] = wc
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

// EarliestWeek returns the smallest week key (UTC Monday, YYYY-MM-DD) the
// file holds, or "" when it has none.
func (d DurableWriteCounts) EarliestWeek() string {
	wk := ""
	for k := range d.Weeks {
		if wk == "" || k < wk {
			wk = k
		}
	}
	return wk
}

// MigrateWriteCounters folds a counter file left at legacy into path and
// removes it, so counts recorded before the file became durable are not
// lost on the first restart after the upgrade. A missing legacy file is a
// no-op; a legacy file that cannot be read is left in place.
func MigrateWriteCounters(legacy, path string, now time.Time) error {
	if legacy == "" || legacy == path {
		return nil
	}
	old, err := LoadDurableWriteCounts(legacy)
	if err != nil {
		return err
	}
	if old.empty() {
		return nil
	}
	if err := mergeIntoFile(path, old, now); err != nil {
		return err
	}
	return os.Remove(legacy)
}

func (d DurableWriteCounts) empty() bool {
	return len(d.Totals) == 0 && len(d.Weeks) == 0
}

var errInvalidWriteCounts = errors.New("invalid write counters JSON")

// LoadDurableWriteCounts reads the file at path. A missing file is an empty
// result, not an error.
func LoadDurableWriteCounts(path string) (DurableWriteCounts, error) {
	d := newDurableWriteCounts()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return newDurableWriteCounts(), fmt.Errorf("parse %s: %w: %w", path, errInvalidWriteCounts, err)
	}
	return d, nil
}

// flush merges the counts recorded since the last flush into the file at path
// and clears them. On failure the pending counts are kept for the next try.
func (c *writeCounters) flush(path string, now time.Time) error {
	c.mu.Lock()
	pending := c.pending
	c.pending = DurableWriteCounts{}
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

func mergeIntoFile(path string, pending DurableWriteCounts, now time.Time) error {
	cur, err := LoadDurableWriteCounts(path)
	if err != nil {
		// Only corrupt JSON can safely be replaced; preserve unreadable files.
		if !errors.Is(err, errInvalidWriteCounts) {
			return err
		}
		cur = newDurableWriteCounts()
	}
	cur.merge(pending)
	cur.UpdatedAt = now.UTC()
	if cur.RecordedSince.IsZero() {
		cur.RecordedSince = now.UTC()
		if wk := cur.EarliestWeek(); wk != "" {
			if t, err := time.Parse("2006-01-02", wk); err == nil && t.Before(cur.RecordedSince) {
				cur.RecordedSince = t
			}
		}
	}
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

// FlushWriteCounters merges pending counts into the durable file now.
func (p *GitHubProxy) FlushWriteCounters(path string) error {
	return p.writes.flush(path, time.Now())
}

// StartWriteCounterPersistence flushes the counters to path every minute and
// returns a stop func that halts the loop and does a final flush. Failures
// are logged and never affect proxying.
func (p *GitHubProxy) StartWriteCounterPersistence(path string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(writeCountersFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := p.FlushWriteCounters(path); err != nil {
					p.logger.Warn("write counters flush failed", "error", err)
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
		if err := p.FlushWriteCounters(path); err != nil {
			p.logger.Warn("write counters final flush failed", "error", err)
		}
	}
}
