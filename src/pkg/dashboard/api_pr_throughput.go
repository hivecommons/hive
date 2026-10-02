package dashboard

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	// prThroughputDefaultHours is the window when ?hours is absent.
	prThroughputDefaultHours = 24
	// prThroughputMaxHours caps a windowed request at the audit log's own
	// retention (auditMaxAgeDays): asking for more would report a partial
	// window as if it were complete. Longer spans use hours=0 (all time).
	prThroughputMaxHours = auditMaxAgeDays * 24

	prThroughputSourceAudit    = "audit"
	prThroughputSourceCounters = "counters"
	prThroughputMaxBuckets     = 120
)

// prThroughputAuditPath is where the windowed counts scan the on-disk audit
// log; "" means the production auditLogPath. A test seam in the shape of
// watchdogActivityAuditPath.
var prThroughputAuditPath = ""

// PRThroughput is the GET /api/pr-throughput response.
type PRThroughput struct {
	// Hours is the window actually applied (after clamping); 0 means all time.
	Hours   int  `json:"hours"`
	AllTime bool `json:"all_time"`
	// Repo is the exact repository filter applied, or empty for all repos.
	Repo string `json:"repo,omitempty"`
	// Since is the RFC3339 start of the window (for all time: the oldest event
	// the durable counters cover). Empty when there is nothing recorded yet.
	Since string `json:"since,omitempty"`
	// RecordedSince is the RFC3339 timestamp of the oldest PR event the
	// underlying data holds, so a reader can tell when the window is longer
	// than the recorded history ("over the last X of recorded history").
	RecordedSince string `json:"recorded_since,omitempty"`
	// Source names the data behind the counts: "audit" (the audit log, for a
	// window) or "counters" (the durable all-time counters).
	Source string `json:"source"`
	// Opened counts PRs the hive's agents created (agent_pr_created).
	Opened int `json:"opened"`
	// Merged counts terminal merge states the hive observed for tracked PRs
	// (pr_merged), split by attribution in MergedByPath: relay, sweep, human,
	// other_automation, or unknown for entries written before attribution
	// existed.
	Merged       int            `json:"merged"`
	MergedByPath map[string]int `json:"merged_by_path"`
	// Closed counts PRs observed closed without merging (pr_closed).
	Closed int `json:"closed"`

	Buckets       []PRThroughputBucket `json:"buckets"`
	BucketSeconds int                  `json:"bucket_seconds"`
	Metrics       PRThroughputMetrics  `json:"metrics"`
	MergedByAgent []PRThroughputTop    `json:"merged_by_agent,omitempty"`
	TopRepos      []PRThroughputTop    `json:"top_repos,omitempty"`
	ClosedReasons []PRThroughputTop    `json:"closed_reasons,omitempty"`
}

type PRThroughputBucket struct {
	T      string `json:"t"`
	Opened int    `json:"opened"`
	Merged int    `json:"merged"`
	Closed int    `json:"closed"`
}

type PRThroughputMetrics struct {
	OpenedPerDay       float64        `json:"opened_per_day"`
	MergedPerDay       float64        `json:"merged_per_day"`
	MedianTimeToMerge  *float64       `json:"median_time_to_merge_hours,omitempty"`
	P90TimeToMerge     *float64       `json:"p90_time_to_merge_hours,omitempty"`
	MergeRatio         *float64       `json:"merge_ratio,omitempty"`
	NetBacklogChange   int            `json:"net_backlog_change"`
	Trend              PRThroughTrend `json:"trend"`
	TimeToMergeSamples int            `json:"time_to_merge_samples"`
}

type PRThroughTrend struct {
	MergedPct            *float64 `json:"merged_pct,omitempty"`
	MedianTimeToMergePct *float64 `json:"median_time_to_merge_pct,omitempty"`
}

type PRThroughputTop struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// handlePRThroughput serves GET /api/pr-throughput?hours=N&repo=owner/name.
func (s *Server) handlePRThroughput(w http.ResponseWriter, r *http.Request) {
	hours := prThroughputDefaultHours
	if raw := strings.TrimSpace(r.URL.Query().Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(w, "hours must be a non-negative integer (0 = all time)", http.StatusBadRequest)
			return
		}
		hours = n
	}
	if hours > prThroughputMaxHours {
		hours = prThroughputMaxHours
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo != "" && !validPRThroughputRepo(repo) {
		jsonError(w, "repo must be owner/name", http.StatusBadRequest)
		return
	}

	audit := s.audit
	if audit == nil {
		audit = &AuditLog{}
	}
	entries := prThroughputEntries(audit)
	if hours == 0 {
		jsonResponse(w, buildPRThroughputAllTime(audit.PRThroughputCounters(), entries, time.Now().UTC(), repo))
		return
	}
	jsonResponse(w, buildPRThroughputWindow(entries, time.Now().UTC(), hours, repo))
}

func prThroughputEntries(audit *AuditLog) []AuditEntry {
	if audit.HasOnDiskLog(prThroughputAuditPath) {
		return audit.OutputActionsSince(time.Time{}, prThroughputActions, prThroughputAuditPath)
	}
	var entries []AuditEntry
	for _, e := range audit.RecentWithPrefixSince(time.Time{}, "") {
		if prThroughputActions[e.Action] {
			entries = append(entries, e)
		}
	}
	return entries
}

func validPRThroughputRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/") && !strings.ContainsAny(repo, " \t\r\n")
}

// buildPRThroughputWindow counts PR throughput entries at or after now-hours.
func buildPRThroughputWindow(entries []AuditEntry, now time.Time, hours int, repo string) PRThroughput {
	since := now.Add(-time.Duration(hours) * time.Hour)
	out := PRThroughput{
		Hours:        hours,
		Repo:         repo,
		Since:        since.Format(time.RFC3339),
		Source:       prThroughputSourceAudit,
		MergedByPath: map[string]int{},
	}
	oldest := oldestPRThroughputEvent(entries, repo)
	if !oldest.IsZero() {
		out.RecordedSince = oldest.UTC().Format(time.RFC3339)
	}
	applyPRThroughputAnalytics(&out, entries, since, now, repo)
	return out
}

// buildPRThroughputAllTime renders durable counters for hours=0 and enriches
// them with audit-derived series/velocity where retained audit history exists.
func buildPRThroughputAllTime(c PRThroughputCounters, entries []AuditEntry, now time.Time, repo string) PRThroughput {
	out := PRThroughput{
		AllTime:       true,
		Repo:          repo,
		Since:         c.Since,
		RecordedSince: c.Since,
		Source:        prThroughputSourceCounters,
		MergedByPath:  map[string]int{},
	}
	if repo != "" {
		fillPRThroughputCounts(&out, c.forRepo(repo))
	} else {
		fillPRThroughputCounts(&out, c)
	}
	oldest := oldestPRThroughputEvent(entries, repo)
	if !oldest.IsZero() {
		applyPRThroughputAnalytics(&out, entries, oldest, now, repo)
		out.MergedByPath = map[string]int{}
		if repo != "" {
			fillPRThroughputCounts(&out, c.forRepo(repo))
		} else {
			fillPRThroughputCounts(&out, c)
		}
	}
	return out
}

func fillPRThroughputCounts(out *PRThroughput, c PRThroughputCounters) {
	out.Opened = c.Opened
	out.Merged = c.Merged
	out.Closed = c.Closed
	for k, v := range c.MergedByPath {
		out.MergedByPath[k] = v
	}
}

func applyPRThroughputAnalytics(out *PRThroughput, entries []AuditEntry, since, until time.Time, repo string) {
	bucketSize := prThroughputBucketSize(out.Hours, since, until)
	out.BucketSeconds = int(bucketSize.Seconds())
	out.Buckets = prThroughputBuckets(entries, since, until, bucketSize, repo)
	counts, mergedByPath, mergedByAgent, topRepos, closedReasons, ttm := summarizePRThroughput(entries, since, until, repo)
	out.Opened, out.Merged, out.Closed = counts.Opened, counts.Merged, counts.Closed
	out.MergedByPath = mergedByPath
	out.Metrics = prThroughputMetrics(counts, ttm, since, until)

	prevStart := since.Add(-until.Sub(since))
	prevCounts, _, _, _, _, prevTTM := summarizePRThroughput(entries, prevStart, since, repo)
	out.Metrics.Trend.MergedPct = pctChange(float64(out.Merged), float64(prevCounts.Merged))
	prevMedian := percentileHours(prevTTM, 0.50)
	if out.Metrics.MedianTimeToMerge != nil && prevMedian != nil {
		out.Metrics.Trend.MedianTimeToMergePct = pctChange(*out.Metrics.MedianTimeToMerge, *prevMedian)
	}
	out.MergedByAgent = topList(mergedByAgent, 5)
	if repo == "" {
		out.TopRepos = topList(topRepos, 5)
	}
	out.ClosedReasons = topList(closedReasons, 5)
}

func prThroughputBucketSize(hours int, since, until time.Time) time.Duration {
	switch {
	case hours == 1:
		return 5 * time.Minute
	case hours == 6:
		return 15 * time.Minute
	case hours == 12 || hours == 24:
		return time.Hour
	case hours == 48:
		return 2 * time.Hour
	case hours == 168:
		return 6 * time.Hour
	case hours == 0:
		days := int(math.Ceil(until.Sub(since).Hours() / 24 / prThroughputMaxBuckets))
		if days < 1 {
			days = 1
		}
		return time.Duration(days) * 24 * time.Hour
	default:
		if hours <= 0 {
			return 24 * time.Hour
		}
		bucketHours := int(math.Ceil(float64(hours) / prThroughputMaxBuckets))
		if bucketHours < 1 {
			bucketHours = 1
		}
		return time.Duration(bucketHours) * time.Hour
	}
}

func prThroughputBuckets(entries []AuditEntry, since, until time.Time, bucketSize time.Duration, repo string) []PRThroughputBucket {
	if !until.After(since) || bucketSize <= 0 {
		return nil
	}
	start := since.Truncate(bucketSize)
	var buckets []PRThroughputBucket
	idx := map[int64]int{}
	for t := start; !t.After(until); t = t.Add(bucketSize) {
		idx[t.Unix()] = len(buckets)
		buckets = append(buckets, PRThroughputBucket{T: t.UTC().Format(time.RFC3339)})
		if len(buckets) >= prThroughputMaxBuckets {
			break
		}
	}
	if len(buckets) == 0 {
		return buckets
	}
	sorted := append([]AuditEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, _ := prThroughputTime(sorted[i])
		tj, _ := prThroughputTime(sorted[j])
		return ti.Before(tj)
	})
	seenTerminal := map[string]bool{}
	for _, e := range sorted {
		t, ok := prThroughputTime(e)
		if !ok || t.After(until) || !prThroughputEntryMatchesRepo(e, repo) {
			continue
		}
		if prThroughputTerminalAction(e.Action) {
			key := prThroughputPRKey(e)
			if key != "" {
				if seenTerminal[key] {
					continue
				}
				seenTerminal[key] = true
			}
		}
		if t.Before(since) {
			continue
		}
		bt := t.Truncate(bucketSize)
		if bt.Before(start) {
			bt = start
		}
		i, ok := idx[bt.Unix()]
		if !ok || !prThroughputActions[e.Action] {
			continue
		}
		switch e.Action {
		case ghpkg.AuditActionAgentPRCreated:
			buckets[i].Opened++
		case ghpkg.AuditActionPRMerged:
			buckets[i].Merged++
		case ghpkg.AuditActionPRClosed:
			buckets[i].Closed++
		}
	}
	return buckets
}

func summarizePRThroughput(entries []AuditEntry, since, until time.Time, repo string) (PRThroughputCounters, map[string]int, map[string]int, map[string]int, map[string]int, []float64) {
	counts := PRThroughputCounters{MergedByPath: map[string]int{}}
	mergedByPath := map[string]int{}
	mergedByAgent := map[string]int{}
	topRepos := map[string]int{}
	closedReasons := map[string]int{}
	openedAt := map[string]time.Time{}
	var ttm []float64
	sorted := append([]AuditEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, _ := prThroughputTime(sorted[i])
		tj, _ := prThroughputTime(sorted[j])
		return ti.Before(tj)
	})
	seenTerminal := map[string]bool{}
	for _, e := range sorted {
		t, ok := prThroughputTime(e)
		if !ok || !prThroughputActions[e.Action] || !prThroughputEntryMatchesRepo(e, repo) {
			continue
		}
		key := prThroughputPRKey(e)
		if prThroughputTerminalAction(e.Action) && key != "" {
			if seenTerminal[key] {
				continue
			}
			seenTerminal[key] = true
		}
		if e.Action == ghpkg.AuditActionAgentPRCreated && key != "" {
			openedAt[key] = t
		}
		inWindow := !t.Before(since) && t.Before(until)
		if !inWindow {
			continue
		}
		counts.add(e)
		if r, ok := collect.AuditEntryRepo(e); ok {
			topRepos[r]++
		}
		switch e.Action {
		case ghpkg.AuditActionPRMerged:
			path := prThroughputMergePath(e.Detail)
			mergedByPath[path]++
			if agent := prThroughputAgent(e); agent != "" {
				mergedByAgent[agent]++
			}
			if key != "" {
				if opened, ok := openedAt[key]; ok && !t.Before(opened) {
					ttm = append(ttm, t.Sub(opened).Hours())
				}
			}
		case ghpkg.AuditActionPRClosed:
			if reason := prThroughputClosedReason(e); reason != "" {
				closedReasons[reason]++
			}
		}
	}
	return counts, mergedByPath, mergedByAgent, topRepos, closedReasons, ttm
}

func prThroughputMetrics(counts PRThroughputCounters, ttm []float64, since, until time.Time) PRThroughputMetrics {
	days := until.Sub(since).Hours() / 24
	if days <= 0 {
		days = 1.0 / 24
	}
	m := PRThroughputMetrics{
		OpenedPerDay:       float64(counts.Opened) / days,
		MergedPerDay:       float64(counts.Merged) / days,
		NetBacklogChange:   counts.Opened - counts.Merged - counts.Closed,
		TimeToMergeSamples: len(ttm),
	}
	if counts.Opened > 0 {
		v := float64(counts.Merged) / float64(counts.Opened)
		m.MergeRatio = &v
	}
	m.MedianTimeToMerge = percentileHours(ttm, 0.50)
	m.P90TimeToMerge = percentileHours(ttm, 0.90)
	return m
}

func percentileHours(samples []float64, p float64) *float64 {
	if len(samples) == 0 {
		return nil
	}
	vals := append([]float64(nil), samples...)
	sort.Float64s(vals)
	if len(vals) == 1 {
		return &vals[0]
	}
	pos := p * float64(len(vals)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return &vals[lo]
	}
	v := vals[lo] + (vals[hi]-vals[lo])*(pos-float64(lo))
	return &v
}

func pctChange(current, previous float64) *float64 {
	if previous == 0 {
		return nil
	}
	v := ((current - previous) / previous) * 100
	return &v
}

func oldestPRThroughputEvent(entries []AuditEntry, repo string) time.Time {
	var oldest time.Time
	for _, e := range entries {
		t, ok := prThroughputTime(e)
		if !ok || !prThroughputActions[e.Action] || !prThroughputEntryMatchesRepo(e, repo) {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

func prThroughputTime(e AuditEntry) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, e.Timestamp)
	return t, err == nil
}

func prThroughputEntryMatchesRepo(e AuditEntry, repo string) bool {
	if repo == "" {
		return true
	}
	r, ok := collect.AuditEntryRepo(e)
	return ok && strings.EqualFold(r, repo)
}

func prThroughputPRKey(e AuditEntry) string {
	repo, ok := collect.AuditEntryRepo(e)
	if !ok {
		return ""
	}
	n := e.Target
	if n == 0 {
		if raw := parseAuditDetailAttrs(e.Detail)["number"]; raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err == nil {
				n = parsed
			}
		}
	}
	if n == 0 {
		return ""
	}
	return strings.ToLower(repo) + "#" + strconv.Itoa(n)
}

func prThroughputAgent(e AuditEntry) string {
	if a := strings.TrimSpace(e.Agent); a != "" {
		return a
	}
	attrs := parseAuditDetailAttrs(e.Detail)
	if a := strings.TrimSpace(attrs["agent"]); a != "" {
		return a
	}
	return strings.TrimSpace(e.User)
}

func prThroughputClosedReason(e AuditEntry) string {
	attrs := parseAuditDetailAttrs(e.Detail)
	for _, k := range []string{"reason", "state", "outcome"} {
		if v := strings.TrimSpace(attrs[k]); v != "" {
			return v
		}
	}
	return ""
}

func topList(counts map[string]int, limit int) []PRThroughputTop {
	if len(counts) == 0 || limit <= 0 {
		return nil
	}
	items := make([]PRThroughputTop, 0, len(counts))
	for k, v := range counts {
		if strings.TrimSpace(k) != "" && v > 0 {
			items = append(items, PRThroughputTop{Name: k, Count: v})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count == items[j].Count {
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		}
		return items[i].Count > items[j].Count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}
