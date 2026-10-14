package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Delivery SLA metrics derived from the lifecycle journeys.
//
// Every span is read off stages the producers already record (issue
// created_at on the enumerated stage, classified, started, pr_opened, merged)
// — nothing here calls the forge. Spans are measured per ISSUE journey
// ("repo#N" keyed by the issue number); the PR-keyed journeys the audit
// bridge also records are skipped, so one merge never counts twice.
//
// Aggregates are {p50, p95, n} in hours over rolling windows, and a
// per-repo daily history (SLAHistory) is kept on disk so the dashboard's
// 30-day sparklines survive both restarts and journey eviction.

// SLASpan names one measured interval of the issue→merge journey.
type SLASpan string

const (
	// SpanOpenToTriage: issue created → classified by the hive.
	SpanOpenToTriage SLASpan = "open_to_triage"
	// SpanTriageToStart: classified → an agent signalled it started.
	SpanTriageToStart SLASpan = "triage_to_start"
	// SpanStartToPR: started → a PR naming the issue opened.
	SpanStartToPR SLASpan = "start_to_pr"
	// SpanPRToMerge: PR opened → PR merged.
	SpanPRToMerge SLASpan = "pr_to_merge"
	// SpanOpenToMerge: issue created → fixing PR merged.
	SpanOpenToMerge SLASpan = "open_to_merge"
)

// SLASpans lists every span in journey order.
var SLASpans = []SLASpan{SpanOpenToTriage, SpanTriageToStart, SpanStartToPR, SpanPRToMerge, SpanOpenToMerge}

// SLAWindows are the rolling aggregate windows /api/status reports.
var SLAWindows = map[string]time.Duration{
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// SLAHistoryDays bounds the persisted daily history.
const SLAHistoryDays = 30

// slaHistoryFormatVersion is the persisted schema version.
const slaHistoryFormatVersion = 1

// slaPersistMinInterval throttles history writes: the report is rebuilt on
// every /api/status serve, so the file is rewritten at most this often.
const slaPersistMinInterval = time.Minute

// Attribute keys the SLA derivation reads off journey stages. Producers set
// them at the moment the event is observed so no timestamp is re-fetched.
const (
	// AttrIssueCreatedAt is the issue's GitHub created_at (RFC3339) on the
	// enumerated stage.
	AttrIssueCreatedAt = "created_at"
	// AttrPRRef is the "repo#N" of the pull request on a pr_opened/merged
	// stage. On a PR-keyed journey it equals the journey ref; on an issue
	// journey it names the PR that fixes the issue.
	AttrPRRef = "pr_ref"
)

// SLASample is one closed span on one issue journey.
type SLASample struct {
	Repo  string
	Ref   string
	Span  SLASpan
	Hours float64
	// At is when the span closed (Unix ms) — the bucket day.
	At int64
}

// SLAStat is a {p50, p95, n} aggregate in hours.
type SLAStat struct {
	P50 float64 `json:"p50_h"`
	P95 float64 `json:"p95_h"`
	N   int     `json:"n"`
}

// SLAWindow holds every span's aggregate over one rolling window.
type SLAWindow struct {
	OpenToTriage  SLAStat `json:"open_to_triage"`
	TriageToStart SLAStat `json:"triage_to_start"`
	StartToPR     SLAStat `json:"start_to_pr"`
	PRToMerge     SLAStat `json:"pr_to_merge"`
	OpenToMerge   SLAStat `json:"open_to_merge"`
}

func (w *SLAWindow) set(span SLASpan, st SLAStat) {
	switch span {
	case SpanOpenToTriage:
		w.OpenToTriage = st
	case SpanTriageToStart:
		w.TriageToStart = st
	case SpanStartToPR:
		w.StartToPR = st
	case SpanPRToMerge:
		w.PRToMerge = st
	case SpanOpenToMerge:
		w.OpenToMerge = st
	}
}

// SLADay is one daily history bucket. Percentiles are in hours and are
// omitted (zero) when the day had no sample for that span; the n_* counts
// say how many issues reached each stage that day.
type SLADay struct {
	Day            string  `json:"day"`
	StartToPRP50   float64 `json:"start_to_pr_p50"`
	PRToMergeP50   float64 `json:"pr_to_merge_p50"`
	OpenToMergeP50 float64 `json:"open_to_merge_p50"`
	NStarted       int     `json:"n_started"`
	NPR            int     `json:"n_pr"`
	NMerged        int     `json:"n_merged"`
}

func (d SLADay) weight() int { return d.NStarted + d.NPR + d.NMerged }

func (d SLADay) empty() bool {
	return d.weight() == 0 && d.StartToPRP50 == 0 && d.PRToMergeP50 == 0 && d.OpenToMergeP50 == 0
}

// SLAStale is an issue an agent started without a PR appearing before the
// claim TTL lapsed.
type SLAStale struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	StartedAt string `json:"started_at"`
	Agent     string `json:"agent,omitempty"`
}

// SLARepoReport is the per-repo (or fleet-wide) /api/status payload.
type SLARepoReport struct {
	Windows map[string]SLAWindow `json:"windows"`
	History []SLADay             `json:"history"`
	// StaleInProgress lists started-without-PR issues past the claim TTL.
	StaleInProgress []SLAStale `json:"stale_in_progress"`
	// MergedLast7d is the count of issue journeys merged in the last 7 days.
	MergedLast7d int `json:"merged_7d"`
}

// SLAReport is the fleet roll-up plus per-repo breakdown.
type SLAReport struct {
	Fleet SLARepoReport            `json:"fleet"`
	Repos map[string]SLARepoReport `json:"repos"`
}

// slaIssueJourney reports whether j is keyed by an issue number: it reached
// an issue-side stage, or its pr_opened stage names a different PR ref.
func slaIssueJourney(j *Journey) bool {
	for _, k := range []Kind{KindEnumerated, KindClassified, KindKicked, KindStarted} {
		if j.Stages[k] != nil {
			return true
		}
	}
	if st := j.Stages[KindPROpened]; st != nil {
		if ref := st.Attrs[AttrPRRef]; ref != "" && ref != j.Ref {
			return true
		}
	}
	return false
}

// slaSplitRef splits "repo#N" (any repo spelling) into repo and number.
func slaSplitRef(ref string) (string, int) {
	idx := strings.LastIndex(ref, "#")
	if idx <= 0 {
		return "", 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(ref[idx+1:]))
	if err != nil || n <= 0 {
		return "", 0
	}
	return strings.TrimSpace(ref[:idx]), n
}

// slaInstants extracts the stage instants (Unix ms, 0 = unknown) the spans
// are measured between.
type slaInstants struct {
	opened, triaged, started, prOpened, merged int64
}

func slaInstantsOf(j *Journey) slaInstants {
	var in slaInstants
	if st := j.Stages[KindEnumerated]; st != nil {
		if raw := st.Attrs[AttrIssueCreatedAt]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				in.opened = t.UnixMilli()
			}
		}
	}
	if st := j.Stages[KindClassified]; st != nil {
		in.triaged = st.FirstAt
	}
	if st := j.Stages[KindStarted]; st != nil {
		in.started = st.FirstAt
	}
	if st := j.Stages[KindPROpened]; st != nil {
		in.prOpened = st.FirstAt
	}
	if st := j.Stages[KindMerged]; st != nil {
		in.merged = st.FirstAt
	}
	return in
}

func hoursBetween(from, to int64) float64 {
	return float64(to-from) / float64(time.Hour/time.Millisecond)
}

// DeriveSLASamples turns issue journeys into closed span samples. A span
// needs both ends recorded and in order; anything else is simply not a
// sample (never a zero or negative one).
func DeriveSLASamples(journeys []Journey) []SLASample {
	out := []SLASample{}
	for i := range journeys {
		j := &journeys[i]
		if !slaIssueJourney(j) {
			continue
		}
		repo, _ := slaSplitRef(j.Ref)
		if repo == "" {
			continue
		}
		in := slaInstantsOf(j)
		add := func(span SLASpan, from, to int64) {
			if from <= 0 || to <= 0 || to < from {
				return
			}
			out = append(out, SLASample{Repo: repo, Ref: j.Ref, Span: span, Hours: hoursBetween(from, to), At: to})
		}
		add(SpanOpenToTriage, in.opened, in.triaged)
		add(SpanTriageToStart, in.triaged, in.started)
		add(SpanStartToPR, in.started, in.prOpened)
		add(SpanPRToMerge, in.prOpened, in.merged)
		add(SpanOpenToMerge, in.opened, in.merged)
	}
	return out
}

// Percentile returns the nearest-rank percentile (0 < p <= 100) of values.
// An empty input yields 0.
func Percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func roundHours(h float64) float64 {
	return math.Round(h*100) / 100
}

func slaStat(values []float64) SLAStat {
	if len(values) == 0 {
		return SLAStat{}
	}
	return SLAStat{P50: roundHours(Percentile(values, 50)), P95: roundHours(Percentile(values, 95)), N: len(values)}
}

// AggregateSLAWindow rolls samples whose closing instant is within window of
// now into one SLAWindow.
func AggregateSLAWindow(samples []SLASample, window time.Duration, now time.Time) SLAWindow {
	cutoff := now.Add(-window).UnixMilli()
	bySpan := map[SLASpan][]float64{}
	for _, s := range samples {
		if s.At < cutoff || s.At > now.UnixMilli() {
			continue
		}
		bySpan[s.Span] = append(bySpan[s.Span], s.Hours)
	}
	var w SLAWindow
	for _, span := range SLASpans {
		w.set(span, slaStat(bySpan[span]))
	}
	return w
}

func dayKey(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02")
}

// DeriveSLADays buckets samples and stage counts by UTC day. The returned
// map is keyed by repo; "" is the fleet-wide roll-up.
func DeriveSLADays(journeys []Journey, samples []SLASample) map[string]map[string]*SLADay {
	out := map[string]map[string]*SLADay{}
	bucket := func(repo, day string) *SLADay {
		days := out[repo]
		if days == nil {
			days = map[string]*SLADay{}
			out[repo] = days
		}
		d := days[day]
		if d == nil {
			d = &SLADay{Day: day}
			days[day] = d
		}
		return d
	}
	type key struct{ repo, day string }
	spanValues := map[key]map[SLASpan][]float64{}
	for _, s := range samples {
		for _, repo := range []string{s.Repo, ""} {
			k := key{repo, dayKey(s.At)}
			if spanValues[k] == nil {
				spanValues[k] = map[SLASpan][]float64{}
			}
			spanValues[k][s.Span] = append(spanValues[k][s.Span], s.Hours)
		}
	}
	for k, by := range spanValues {
		d := bucket(k.repo, k.day)
		if v := by[SpanStartToPR]; len(v) > 0 {
			d.StartToPRP50 = roundHours(Percentile(v, 50))
		}
		if v := by[SpanPRToMerge]; len(v) > 0 {
			d.PRToMergeP50 = roundHours(Percentile(v, 50))
		}
		if v := by[SpanOpenToMerge]; len(v) > 0 {
			d.OpenToMergeP50 = roundHours(Percentile(v, 50))
		}
	}
	for i := range journeys {
		j := &journeys[i]
		if !slaIssueJourney(j) {
			continue
		}
		repo, _ := slaSplitRef(j.Ref)
		if repo == "" {
			continue
		}
		in := slaInstantsOf(j)
		count := func(at int64, inc func(*SLADay)) {
			if at <= 0 {
				return
			}
			inc(bucket(repo, dayKey(at)))
			inc(bucket("", dayKey(at)))
		}
		count(in.started, func(d *SLADay) { d.NStarted++ })
		count(in.prOpened, func(d *SLADay) { d.NPR++ })
		count(in.merged, func(d *SLADay) { d.NMerged++ })
	}
	return out
}

// DeriveSLAStale lists issue journeys an agent started at least ttl ago
// that have neither a PR nor a merge, bounded to starts within the history
// window. A non-positive ttl disables the check.
func DeriveSLAStale(journeys []Journey, ttl time.Duration, now time.Time) []SLAStale {
	out := []SLAStale{}
	if ttl <= 0 {
		return out
	}
	nowMs := now.UnixMilli()
	oldest := now.Add(-SLAHistoryDays * 24 * time.Hour).UnixMilli()
	for i := range journeys {
		j := &journeys[i]
		if !slaIssueJourney(j) {
			continue
		}
		st := j.Stages[KindStarted]
		if st == nil || j.Stages[KindPROpened] != nil || j.Stages[KindMerged] != nil {
			continue
		}
		if st.FirstAt < oldest || st.FirstAt+ttl.Milliseconds() > nowMs {
			continue
		}
		repo, n := slaSplitRef(j.Ref)
		if repo == "" {
			continue
		}
		out = append(out, SLAStale{Repo: repo, Number: n, StartedAt: time.UnixMilli(st.FirstAt).UTC().Format(time.RFC3339), Agent: st.Agent})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].StartedAt != out[b].StartedAt {
			return out[a].StartedAt < out[b].StartedAt
		}
		return out[a].Repo+"#"+strconv.Itoa(out[a].Number) < out[b].Repo+"#"+strconv.Itoa(out[b].Number)
	})
	return out
}

// persistedSLAHistory is the on-disk shape.
type persistedSLAHistory struct {
	Version int                 `json:"version"`
	Repos   map[string][]SLADay `json:"repos"`
}

// SLAHistory keeps the per-repo daily buckets across restarts and journey
// eviction. A freshly derived day replaces the stored one only when it is
// backed by at least as many stage events (weight), so eviction of old
// journeys can never shrink a day that was already recorded in full.
type SLAHistory struct {
	mu          sync.Mutex
	days        map[string]map[string]SLADay // repo → day → bucket ("" = fleet)
	path        string
	logger      *slog.Logger
	dirty       bool
	lastPersist time.Time
}

// NewSLAHistory returns an empty, in-memory history.
func NewSLAHistory() *SLAHistory {
	return &SLAHistory{days: map[string]map[string]SLADay{}}
}

// EnablePersistence loads path (if present) and turns on throttled atomic
// re-persistence. Like the timeline, a corrupt file is moved aside rather
// than refusing startup: this is derived telemetry.
func (h *SLAHistory) EnablePersistence(path string, logger *slog.Logger) error {
	if h == nil || path == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.path = path
	h.logger = logger
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading sla history %s: %w", path, err)
	}
	var persisted persistedSLAHistory
	if err := json.Unmarshal(data, &persisted); err != nil {
		_ = os.Rename(path, path+".corrupt")
		if logger != nil {
			logger.Warn("sla history file corrupt — moved aside, starting empty", "path", path, "error", err)
		}
		return nil
	}
	for repo, days := range persisted.Repos {
		for _, d := range days {
			if d.Day == "" {
				continue
			}
			h.setLocked(repo, d)
		}
	}
	return nil
}

func (h *SLAHistory) setLocked(repo string, d SLADay) {
	days := h.days[repo]
	if days == nil {
		days = map[string]SLADay{}
		h.days[repo] = days
	}
	days[d.Day] = d
}

// Update folds freshly derived days in (max-weight wins), prunes days older
// than SLAHistoryDays, and persists when something changed (throttled).
func (h *SLAHistory) Update(derived map[string]map[string]*SLADay, now time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for repo, days := range derived {
		for day, d := range days {
			if d == nil || d.empty() {
				continue
			}
			prev, ok := h.days[repo][day]
			if !ok || d.weight() >= prev.weight() {
				if !ok || prev != *d {
					h.dirty = true
				}
				h.setLocked(repo, *d)
			}
		}
	}
	h.pruneLocked(now)
	h.persistMaybeLocked(now)
}

func (h *SLAHistory) pruneLocked(now time.Time) {
	floor := slaHistoryFloor(now)
	for repo, days := range h.days {
		for day := range days {
			if day < floor {
				delete(days, day)
				h.dirty = true
			}
		}
		if len(days) == 0 {
			delete(h.days, repo)
		}
	}
}

func slaHistoryFloor(now time.Time) string {
	return now.UTC().AddDate(0, 0, -(SLAHistoryDays - 1)).Format("2006-01-02")
}

// Series returns a dense SLAHistoryDays-long series for repo ("" = fleet),
// oldest first, with empty buckets for days without data.
func (h *SLAHistory) Series(repo string, now time.Time) []SLADay {
	out := make([]SLADay, 0, SLAHistoryDays)
	var days map[string]SLADay
	if h != nil {
		h.mu.Lock()
		days = h.days[repo]
		h.mu.Unlock()
	}
	start := now.UTC().AddDate(0, 0, -(SLAHistoryDays - 1))
	for i := 0; i < SLAHistoryDays; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		if d, ok := days[day]; ok {
			out = append(out, d)
		} else {
			out = append(out, SLADay{Day: day})
		}
	}
	return out
}

// Repos lists the repos with any retained history (fleet "" excluded).
func (h *SLAHistory) Repos() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.days))
	for repo := range h.days {
		if repo != "" {
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

func (h *SLAHistory) persistMaybeLocked(now time.Time) {
	if h.path == "" || !h.dirty {
		return
	}
	if !h.lastPersist.IsZero() && now.Sub(h.lastPersist) < slaPersistMinInterval {
		return
	}
	if err := h.persistLocked(); err != nil {
		if h.logger != nil {
			h.logger.Warn("sla history persist failed — history will reset on restart", "path", h.path, "error", err)
		}
		return
	}
	h.dirty = false
	h.lastPersist = now
}

// Flush persists any dirty state immediately (shutdown / tests).
func (h *SLAHistory) Flush() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == "" || !h.dirty {
		return nil
	}
	if err := h.persistLocked(); err != nil {
		return err
	}
	h.dirty = false
	return nil
}

func (h *SLAHistory) persistLocked() error {
	repos := make(map[string][]SLADay, len(h.days))
	for repo, days := range h.days {
		list := make([]SLADay, 0, len(days))
		for _, d := range days {
			list = append(list, d)
		}
		sort.Slice(list, func(i, k int) bool { return list[i].Day < list[k].Day })
		repos[repo] = list
	}
	data, err := json.Marshal(persistedSLAHistory{Version: slaHistoryFormatVersion, Repos: repos})
	if err != nil {
		return fmt.Errorf("marshaling sla history: %w", err)
	}
	return writeFileAtomic(h.path, data, "sla history")
}

// SLATracker derives the SLA report from a journey snapshot and keeps the
// daily history current as a side effect.
type SLATracker struct {
	History *SLAHistory
}

// NewSLATracker returns a tracker with an in-memory history.
func NewSLATracker() *SLATracker {
	return &SLATracker{History: NewSLAHistory()}
}

// Report builds the fleet + per-repo SLA payload for journeys at now. ttl is
// the agent claim TTL that bounds "started without a PR"; repos lists the
// configured repos (org-less or org/repo) so every configured repo gets an
// entry even with no samples yet.
func (t *SLATracker) Report(journeys []Journey, repos []string, ttl time.Duration, now time.Time) SLAReport {
	if t == nil {
		t = NewSLATracker()
	}
	samples := DeriveSLASamples(journeys)
	days := DeriveSLADays(journeys, samples)
	t.History.Update(days, now)
	stale := DeriveSLAStale(journeys, ttl, now)

	names := map[string]struct{}{}
	for _, r := range repos {
		if short := ShortRepo(r); short != "" {
			names[short] = struct{}{}
		}
	}
	for _, r := range t.History.Repos() {
		names[r] = struct{}{}
	}
	for _, s := range samples {
		names[s.Repo] = struct{}{}
	}

	build := func(repo string) SLARepoReport {
		var mine []SLASample
		if repo == "" {
			mine = samples
		} else {
			for _, s := range samples {
				if s.Repo == repo {
					mine = append(mine, s)
				}
			}
		}
		rep := SLARepoReport{Windows: map[string]SLAWindow{}, History: t.History.Series(repo, now), StaleInProgress: []SLAStale{}}
		for name, window := range SLAWindows {
			rep.Windows[name] = AggregateSLAWindow(mine, window, now)
		}
		for _, s := range stale {
			if repo == "" || s.Repo == repo {
				rep.StaleInProgress = append(rep.StaleInProgress, s)
			}
		}
		rep.MergedLast7d = mergedWithin(rep.History, now, 7)
		return rep
	}
	report := SLAReport{Fleet: build(""), Repos: map[string]SLARepoReport{}}
	for name := range names {
		report.Repos[name] = build(name)
	}
	return report
}

func mergedWithin(history []SLADay, now time.Time, days int) int {
	floor := now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	n := 0
	for _, d := range history {
		if d.Day >= floor {
			n += d.NMerged
		}
	}
	return n
}

// ShortRepo strips an "org/" prefix: journey refs use the org-less repo
// name the enumeration carries.
func ShortRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repo = repo[idx+1:]
	}
	return repo
}
