// Package hivestate keeps the per-eval-cycle hive state snapshots as a
// bounded time series and summarizes it into 24 h / 7 d trends
// (hivecommons/hive#12339).
//
// Each governor eval cycle writes one raw snapshot, <epoch-ms>.json. Raw
// snapshots older than RawRetention are rolled into hourly buckets,
// hour-<epoch-ms>.json, and everything older than Retention is deleted, so the
// directory stays bounded. A bucket keeps every queue-depth sample of its hour,
// so quantiles over compacted history are exact.
package hivestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// RawRetention is how long raw per-cycle snapshots are kept before they
	// are rolled into hourly buckets.
	RawRetention = 24 * time.Hour
	// Retention is the total history kept; older raw files and buckets are
	// deleted.
	Retention = 30 * 24 * time.Hour

	compactEvery = time.Hour
	trendsTTL    = 5 * time.Minute

	bucketPrefix = "hour-"
	fileSuffix   = ".json"

	agentStateIdle    = "idle"
	agentStateRunning = "running"
)

// Snapshot is one raw record, the schema of <epoch-ms>.json.
type Snapshot struct {
	Timestamp     string            `json:"timestamp"`
	Mode          string            `json:"mode"`
	QueueIssues   int               `json:"queue_issues"`
	QueuePRs      int               `json:"queue_prs"`
	QueueHold     int               `json:"queue_hold"`
	SLAViolations int               `json:"sla_violations"`
	AgentsKicked  []string          `json:"agents_kicked,omitempty"`
	AgentStates   map[string]string `json:"agent_states,omitempty"`
	TotalTokens   int64             `json:"total_tokens"`
}

// Bucket is an aggregate of one or more snapshots: an hourly file on disk, or
// a single raw snapshot while summarizing. Tokens is the sum of positive
// TotalTokens deltas between consecutive samples (the token collector's total
// can drop when session files are pruned; drops are ignored).
type Bucket struct {
	Start              time.Time      `json:"start"`
	Last               time.Time      `json:"last"`
	Samples            int            `json:"samples"`
	QueueIssues        []int          `json:"queue_issues"`
	QueuePRs           []int          `json:"queue_prs"`
	QueueHold          []int          `json:"queue_hold"`
	SLAViolatedSamples int            `json:"sla_violated_samples"`
	Kicks              int            `json:"kicks"`
	AgentSamples       int            `json:"agent_samples"`
	IdleAgentSamples   int            `json:"idle_agent_samples"`
	Tokens             int64          `json:"tokens"`
	FirstTokens        int64          `json:"first_tokens"`
	LastTokens         int64          `json:"last_tokens"`
	ModeSamples        map[string]int `json:"mode_samples,omitempty"`
}

// Quantiles is the nearest-rank median and p90 of a sample set.
type Quantiles struct {
	Median int `json:"median"`
	P90    int `json:"p90"`
}

// Window summarizes the snapshots inside one look-back window.
// SLAViolationRate is the fraction of samples with at least one SLA
// violation; AgentIdleRatio is idle / (idle + running) agent-samples, so
// stopped and paused agents do not count as idle. CoverageHours is how much of
// the window the samples actually span; per-day rates are scaled by it and
// stay 0 below one hour of coverage.
type Window struct {
	Samples          int       `json:"samples"`
	CoverageHours    float64   `json:"coverage_hours"`
	QueueIssues      Quantiles `json:"queue_issues"`
	QueuePRs         Quantiles `json:"queue_prs"`
	QueueHold        Quantiles `json:"queue_hold"`
	SLAViolationRate float64   `json:"sla_violation_rate"`
	// AgentSamples distinguishes a real zero idle ratio from no observed agents.
	AgentSamples   int     `json:"agent_samples,omitempty"`
	AgentIdleRatio float64 `json:"agent_idle_ratio"`
	KicksPerDay    float64 `json:"kicks_per_day"`
	TokensPerDay   float64 `json:"tokens_per_day"`
}

// Trends is the 24 h and 7 d summary of the hive state time series.
type Trends struct {
	Last24h Window `json:"24h"`
	Last7d  Window `json:"7d"`
}

// FromSnapshot converts one raw snapshot taken at at into a single-sample
// bucket.
func FromSnapshot(at time.Time, s Snapshot) Bucket {
	b := Bucket{
		Start:       at,
		Last:        at,
		Samples:     1,
		QueueIssues: []int{s.QueueIssues},
		QueuePRs:    []int{s.QueuePRs},
		QueueHold:   []int{s.QueueHold},
		Kicks:       len(s.AgentsKicked),
		FirstTokens: s.TotalTokens,
		LastTokens:  s.TotalTokens,
	}
	if s.SLAViolations > 0 {
		b.SLAViolatedSamples = 1
	}
	if s.Mode != "" {
		b.ModeSamples = map[string]int{s.Mode: 1}
	}
	for _, st := range s.AgentStates {
		switch st {
		case agentStateIdle:
			b.AgentSamples++
			b.IdleAgentSamples++
		case agentStateRunning:
			b.AgentSamples++
		}
	}
	return b
}

// merge appends next, which must not start before b, to b.
func (b *Bucket) merge(next Bucket) {
	if next.Samples == 0 {
		return
	}
	if b.Samples == 0 {
		*b = next
		b.QueueIssues = append([]int(nil), next.QueueIssues...)
		b.QueuePRs = append([]int(nil), next.QueuePRs...)
		b.QueueHold = append([]int(nil), next.QueueHold...)
		b.ModeSamples = nil
		if next.ModeSamples != nil {
			b.ModeSamples = make(map[string]int, len(next.ModeSamples))
			for m, n := range next.ModeSamples {
				b.ModeSamples[m] = n
			}
		}
		return
	}
	b.Tokens += next.Tokens
	if d := next.FirstTokens - b.LastTokens; d > 0 {
		b.Tokens += d
	}
	b.LastTokens = next.LastTokens
	b.Last = next.Last
	b.Samples += next.Samples
	b.QueueIssues = append(b.QueueIssues, next.QueueIssues...)
	b.QueuePRs = append(b.QueuePRs, next.QueuePRs...)
	b.QueueHold = append(b.QueueHold, next.QueueHold...)
	b.SLAViolatedSamples += next.SLAViolatedSamples
	b.Kicks += next.Kicks
	b.AgentSamples += next.AgentSamples
	b.IdleAgentSamples += next.IdleAgentSamples
	if len(next.ModeSamples) > 0 && b.ModeSamples == nil {
		b.ModeSamples = make(map[string]int, len(next.ModeSamples))
	}
	for m, n := range next.ModeSamples {
		b.ModeSamples[m] += n
	}
}

// Summarize computes the 24 h and 7 d trends from buckets as of now. It
// returns nil when no bucket starts in the last 7 days.
func Summarize(buckets []Bucket, now time.Time) *Trends {
	sorted := append([]Bucket(nil), buckets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	week := summarizeWindow(sorted, now, 7*24*time.Hour)
	if week.Samples == 0 {
		return nil
	}
	return &Trends{Last24h: summarizeWindow(sorted, now, 24*time.Hour), Last7d: week}
}

func summarizeWindow(sorted []Bucket, now time.Time, d time.Duration) Window {
	since := now.Add(-d)
	var agg Bucket
	for _, b := range sorted {
		if b.Start.Before(since) || b.Start.After(now) {
			continue
		}
		agg.merge(b)
	}
	if agg.Samples == 0 {
		return Window{}
	}
	covered := now.Sub(agg.Start)
	w := Window{
		Samples:          agg.Samples,
		AgentSamples:     agg.AgentSamples,
		CoverageHours:    round(covered.Hours(), 2),
		QueueIssues:      quantiles(agg.QueueIssues),
		QueuePRs:         quantiles(agg.QueuePRs),
		QueueHold:        quantiles(agg.QueueHold),
		SLAViolationRate: round(float64(agg.SLAViolatedSamples)/float64(agg.Samples), 4),
	}
	if agg.AgentSamples > 0 {
		w.AgentIdleRatio = round(float64(agg.IdleAgentSamples)/float64(agg.AgentSamples), 4)
	}
	if covered >= time.Hour {
		days := covered.Hours() / 24
		w.KicksPerDay = round(float64(agg.Kicks)/days, 1)
		w.TokensPerDay = round(float64(agg.Tokens)/days, 1)
	}
	return w
}

func quantiles(vals []int) Quantiles {
	if len(vals) == 0 {
		return Quantiles{}
	}
	s := append([]int(nil), vals...)
	sort.Ints(s)
	return Quantiles{Median: nearestRank(s, 0.5), P90: nearestRank(s, 0.9)}
}

func nearestRank(sorted []int, q float64) int {
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// fileKind classifies a directory entry name as a raw snapshot or an hourly
// bucket (ok false for anything else); at is the epoch-ms time in the name.
func fileKind(name string) (at time.Time, bucket, ok bool) {
	if !strings.HasSuffix(name, fileSuffix) {
		return time.Time{}, false, false
	}
	stem := strings.TrimSuffix(name, fileSuffix)
	if strings.HasPrefix(stem, bucketPrefix) {
		stem = strings.TrimPrefix(stem, bucketPrefix)
		bucket = true
	}
	ms, err := strconv.ParseInt(stem, 10, 64)
	if err != nil || ms < 0 {
		return time.Time{}, false, false
	}
	return time.UnixMilli(ms).UTC(), bucket, true
}

func bucketName(start time.Time) string {
	return fmt.Sprintf("%s%d%s", bucketPrefix, start.UnixMilli(), fileSuffix)
}

func readSnapshot(path string, at time.Time) (Bucket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bucket{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Bucket{}, err
	}
	return FromSnapshot(at, s), nil
}

func readBucket(path string) (Bucket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Bucket{}, err
	}
	var b Bucket
	if err := json.Unmarshal(data, &b); err != nil {
		return Bucket{}, err
	}
	return b, nil
}

// Load reads every raw snapshot and hourly bucket in dir that starts at or
// after since, sorted by start time. Unreadable files are skipped.
func Load(dir string, since time.Time) ([]Bucket, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Bucket
	for _, e := range entries {
		at, isBucket, ok := fileKind(e.Name())
		if !ok || e.IsDir() || at.Before(since) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var b Bucket
		if isBucket {
			b, err = readBucket(path)
		} else {
			b, err = readSnapshot(path, at)
		}
		if err == nil && b.Samples > 0 {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// Count reports how many snapshot files (raw or hourly bucket) in dir start
// at or after since, and the start time of the earliest file in dir
// regardless of since (zero when there are none). It is the file count the
// dashboard's baseline progress uses (hivecommons/hive#12339), scoped to a
// window; a missing directory counts as empty.
func Count(dir string, since time.Time) (n int, first time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, time.Time{}
	}
	for _, e := range entries {
		at, _, ok := fileKind(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if !at.Before(since) {
			n++
		}
	}
	return n, first
}

// Store is the hive state time series in one directory. Its methods are safe
// for concurrent use; a nil *Store is a no-op.
type Store struct {
	dir string
	now func() time.Time

	mu          sync.Mutex
	lastCompact time.Time
	trends      *Trends
	trendsAt    time.Time
}

// NewStore returns the store for dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir, now: time.Now}
}

// Record writes one raw snapshot. At most once per hour it also compacts the
// directory, so the per-cycle cost stays a single small file write.
func (s *Store) Record(snap Snapshot) error {
	if s == nil || s.dir == "" {
		return nil
	}
	now := s.now().UTC()
	if snap.Timestamp == "" {
		snap.Timestamp = now.Format(time.RFC3339)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, fmt.Sprintf("%d%s", now.UnixMilli(), fileSuffix)), data, 0o644); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}

	s.mu.Lock()
	due := now.Sub(s.lastCompact) >= compactEvery
	if due {
		s.lastCompact = now
	}
	s.mu.Unlock()
	if due {
		if err := s.Compact(now); err != nil {
			return fmt.Errorf("compacting snapshots: %w", err)
		}
	}
	return nil
}

// Compact rolls raw snapshots older than RawRetention into hourly buckets and
// deletes raw files and buckets older than Retention.
func (s *Store) Compact(now time.Time) error {
	if s == nil || s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	rawCutoff := now.Add(-RawRetention)
	dropCutoff := now.Add(-Retention)

	type rawFile struct {
		name string
		at   time.Time
	}
	byHour := map[time.Time][]rawFile{}
	var errs []error
	for _, e := range entries {
		at, isBucket, ok := fileKind(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		if at.Before(dropCutoff) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if !isBucket && at.Before(rawCutoff) {
			h := at.Truncate(time.Hour)
			byHour[h] = append(byHour[h], rawFile{name: e.Name(), at: at})
		}
	}

	for hour, files := range byHour {
		sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
		bucketPath := filepath.Join(s.dir, bucketName(hour))
		agg, err := readBucket(bucketPath)
		if err != nil {
			agg = Bucket{}
		}
		for _, f := range files {
			// A crash between writing the bucket and removing its raw files
			// leaves raws the bucket already holds; skip them.
			if agg.Samples > 0 && !f.at.After(agg.Last) {
				continue
			}
			if b, err := readSnapshot(filepath.Join(s.dir, f.name), f.at); err == nil {
				agg.merge(b)
			}
		}
		if agg.Samples > 0 {
			agg.Start = hour
			if err := writeFileAtomic(bucketPath, agg); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		for _, f := range files {
			if err := os.Remove(filepath.Join(s.dir, f.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func writeFileAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Trends returns a copy of the 24 h / 7 d summary, recomputed at most every
// five minutes. It returns nil when the store is nil, the directory is
// unreadable, or no sample falls in the last 7 days.
func (s *Store) Trends() *Trends {
	if s == nil || s.dir == "" {
		return nil
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trendsAt.IsZero() || now.Sub(s.trendsAt) >= trendsTTL {
		buckets, err := Load(s.dir, now.Add(-7*24*time.Hour))
		if err != nil {
			return nil
		}
		s.trends = Summarize(buckets, now)
		s.trendsAt = now
	}
	if s.trends == nil {
		return nil
	}
	c := *s.trends
	return &c
}
