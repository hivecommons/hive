// Package retro implements a deterministic post-completion retro lane.
package retro

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/timeline"
)

const (
	Actor = "retro"

	DefaultScanIntervalS        = 60 * 60
	DefaultMaxFixAttempts       = 3
	DefaultMaxKicks             = 5
	DefaultLongStallDays        = 7
	defaultLongStallHours       = DefaultLongStallDays * 24
	metadataAnalyzedAt          = "retro_analyzed_at"
	metadataFindingType         = "finding_type"
	metadataAdvisoryAgent       = "advisory_agent"
	metadataSeverity            = "severity"
	metadataDetail              = "detail"
	metadataSourceBead          = "retro_source_bead"
	metadataSourceActor         = "retro_source_actor"
	metadataSourcePR            = "retro_source_pr"
	metadataSourceIssue         = "retro_source_issue"
	metadataPattern             = "retro_pattern"
	metadataRecordWallClock     = "retro_wall_clock"
	metadataAnalysisRootCause   = "retro_analysis_root_cause"
	metadataAnalysisImprovement = "retro_analysis_improvement"
	metadataLessonSlug          = "retro_lesson_slug"
	PatternExcessiveFixAttempts = "excessive_fix_attempts"
	PatternExcessiveKicks       = "excessive_kicks"
	PatternLongStall            = "long_stall"
	PatternDriftPause           = "drift_pause"
)

type Config struct {
	Enabled             bool
	ScanIntervalS       int
	MaxFixAttempts      int
	MaxKicks            int
	LongStallDays       int
	RecentClosedWindowS int
	AnalysisModel       string
	AnalysisEndpoint    string
	AnalysisAPIKey      string
}

type Thresholds struct {
	MaxFixAttempts int
	MaxKicks       int
	LongStall      time.Duration
}

type RetroRecord struct {
	BeadID         string
	Title          string
	Type           beads.BeadType
	Status         beads.Status
	Actor          string
	ExternalRef    string
	IssueRef       string
	PRRef          string
	PRState        string
	KicksReceived  int
	CIFailureCount int
	FixAttempts    int
	DriftPauses    int
	ClaimedAt      time.Time
	ClosedAt       time.Time
	ClaimToClose   time.Duration
}

type Finding struct {
	Pattern  string
	Title    string
	Detail   string
	Severity string
}

type AttemptReader interface {
	Attempts(repo string, number int) int
}

type KnowledgeSink interface {
	IngestRetroLesson(ctx context.Context, lesson knowledge.RetroLesson) (slug string, created bool, err error)
}

type Lane struct {
	stores        map[string]*beads.Store
	advisoryStore *beads.Store
	timeline      *timeline.Store
	attempts      AttemptReader
	analyzer      *Analyzer
	knowledge     KnowledgeSink
	logger        *slog.Logger
	interval      time.Duration
	thresholds    Thresholds
	lastRun       time.Time
	recentWindow  time.Duration
}

func NewLane(stores map[string]*beads.Store, advisoryStore *beads.Store, tl *timeline.Store, attempts AttemptReader, cfg Config, logger *slog.Logger) *Lane {
	intervalS := cfg.ScanIntervalS
	if intervalS <= 0 {
		intervalS = DefaultScanIntervalS
	}
	if logger == nil {
		logger = slog.Default()
	}
	analyzer, err := NewAnalyzer(cfg.AnalysisEndpoint, cfg.AnalysisAPIKey, cfg.AnalysisModel)
	if err != nil {
		logger.Warn("retro: LLM analysis disabled", "error", err)
	}
	return &Lane{
		stores:        stores,
		advisoryStore: advisoryStore,
		timeline:      tl,
		attempts:      attempts,
		analyzer:      analyzer,
		logger:        logger,
		interval:      time.Duration(intervalS) * time.Second,
		thresholds:    thresholdsFromConfig(cfg),
		recentWindow:  time.Duration(cfg.RecentClosedWindowS) * time.Second,
	}
}

func (l *Lane) SetAnalyzer(analyzer *Analyzer) {
	l.analyzer = analyzer
}

func (l *Lane) SetKnowledgeSink(sink KnowledgeSink) {
	l.knowledge = sink
}

func (l *Lane) Due(now time.Time) bool {
	return l.lastRun.IsZero() || now.Sub(l.lastRun) >= l.interval
}

func (l *Lane) Run(ctx context.Context) int {
	l.lastRun = time.Now()
	if l.advisoryStore == nil {
		return 0
	}
	created := 0
	for storeName, store := range l.stores {
		if ctx.Err() != nil {
			return created
		}
		if store == nil || store == l.advisoryStore {
			continue
		}
		for _, b := range store.AllClosed() {
			if ctx.Err() != nil {
				return created
			}
			if !l.eligible(b, l.lastRun) {
				continue
			}
			record := Reconstruct(b, l.timeline, l.attempts)
			if !record.HasAssociatedClosedPR() {
				continue
			}
			findings := Detect(record, l.thresholds)
			analysis := l.analyze(ctx, record, findings)
			l.ingestLesson(ctx, record, analysis)
			for _, f := range findings {
				if l.createAdvisory(record, f, analysis) {
					created++
				}
			}
			if err := store.SetMetadata(b.ID, metadataAnalyzedAt, l.lastRun.UTC().Format(time.RFC3339)); err != nil {
				l.logger.Warn("retro: failed to mark bead analyzed", "store", storeName, "bead", b.ID, "error", err)
			}
		}
	}
	return created
}

func (l *Lane) eligible(b *beads.Bead, now time.Time) bool {
	if b == nil || b.Type == beads.TypeAdvisory {
		return false
	}
	if b.Status != beads.StatusClosed && b.Status != beads.StatusDone {
		return false
	}
	if b.Metadata != nil {
		if _, ok := b.Metadata[metadataAnalyzedAt]; ok {
			return false
		}
	}
	if l.recentWindow > 0 {
		completed := completedAt(b)
		if !completed.IsZero() && now.Sub(completed) > l.recentWindow {
			return false
		}
	}
	return true
}

func completedAt(b *beads.Bead) time.Time {
	if b == nil {
		return time.Time{}
	}
	if b.ClosedAt != nil {
		return b.ClosedAt.Time
	}
	if t := parseTime(metaString(b, "closed_at")); !t.IsZero() {
		return t
	}
	if b.Status == beads.StatusDone || b.Status == beads.StatusClosed {
		return b.UpdatedAt.Time
	}
	return time.Time{}
}

func (l *Lane) analyze(ctx context.Context, r RetroRecord, findings []Finding) *Analysis {
	if len(findings) == 0 || l.analyzer == nil {
		return nil
	}
	analysis, err := l.analyzer.Analyze(ctx, r, findings)
	if err != nil {
		l.logger.Warn("retro: LLM analysis skipped", "source_bead", r.BeadID, "error", err)
		return nil
	}
	return &analysis
}

func (l *Lane) ingestLesson(ctx context.Context, r RetroRecord, analysis *Analysis) {
	if analysis == nil || !analysis.Generalizable || l.knowledge == nil {
		return
	}
	slug, created, err := l.knowledge.IngestRetroLesson(ctx, knowledge.RetroLesson{
		Lesson:     analysis.GeneralizableLesson,
		SourceBead: r.BeadID,
		SourcePR:   r.PRRef,
		SourceDate: r.ClosedAt,
	})
	if err != nil {
		l.logger.Warn("retro: lesson ingestion skipped", "source_bead", r.BeadID, "error", err)
		return
	}
	if created {
		l.logger.Info("retro: lesson ingested", "source_bead", r.BeadID, "slug", slug)
	}
}

func (l *Lane) createAdvisory(r RetroRecord, f Finding, analysis *Analysis) bool {
	if l.openDuplicate(f.Title) {
		return false
	}
	b, err := l.advisoryStore.Create(f.Title, beads.TypeAdvisory, severityToPriority(f.Severity), Actor, r.PRRef)
	if err != nil {
		l.logger.Warn("retro: failed to create advisory bead", "pattern", f.Pattern, "source_bead", r.BeadID, "error", err)
		return false
	}
	meta := map[string]string{
		metadataSeverity:        f.Severity,
		metadataFindingType:     "retro",
		metadataAdvisoryAgent:   Actor,
		metadataDetail:          f.Detail,
		metadataSourceBead:      r.BeadID,
		metadataSourceActor:     r.Actor,
		metadataSourcePR:        r.PRRef,
		metadataSourceIssue:     r.IssueRef,
		metadataPattern:         f.Pattern,
		metadataRecordWallClock: r.ClaimToClose.String(),
	}
	if analysis != nil {
		meta[metadataAnalysisRootCause] = analysis.RootCauseHypothesis
		meta[metadataAnalysisImprovement] = analysis.ProcessImprovement
	}
	for k, v := range meta {
		if v != "" {
			_ = l.advisoryStore.SetMetadata(b.ID, k, v)
		}
	}
	if analysis != nil {
		_ = l.advisoryStore.Update(b.ID, func(bd *beads.Bead) {
			bd.Notes = advisoryNotes(f, analysis)
		})
	}
	return true
}

func advisoryNotes(f Finding, analysis *Analysis) string {
	var b strings.Builder
	b.WriteString(f.Detail)
	if analysis == nil {
		return b.String()
	}
	b.WriteString("\n\nModel-generated retro analysis:\n")
	b.WriteString("- Root-cause hypothesis: ")
	b.WriteString(analysis.RootCauseHypothesis)
	b.WriteString("\n- Actionable process improvement: ")
	b.WriteString(analysis.ProcessImprovement)
	return b.String()
}

func (l *Lane) openDuplicate(title string) bool {
	for _, b := range l.advisoryStore.List(beads.ListFilter{}) {
		if b.Type == beads.TypeAdvisory && b.Title == title && b.Status != beads.StatusClosed && b.Status != beads.StatusDone {
			return true
		}
	}
	return false
}

func Reconstruct(b *beads.Bead, tl *timeline.Store, attempts AttemptReader) RetroRecord {
	if b == nil {
		return RetroRecord{}
	}
	r := RetroRecord{
		BeadID:      b.ID,
		Title:       b.Title,
		Type:        b.Type,
		Status:      b.Status,
		Actor:       b.Actor,
		ExternalRef: b.ExternalRef,
		IssueRef:    firstNonEmpty(metaString(b, "issue_ref"), canonicalIssueRef(b.ExternalRef)),
		PRRef:       firstNonEmpty(metaString(b, "pr_ref"), metaString(b, "pull_request"), metaString(b, "github_pr")),
		PRState:     strings.ToLower(firstNonEmpty(metaString(b, "pr_state"), metaString(b, "pull_request_state"))),
		ClaimedAt:   parseTime(firstNonEmpty(metaString(b, "claimed_at"), metaString(b, "claim_at"), metaString(b, "started_at"))),
		ClosedAt:    closedAt(b),
	}
	if r.IssueRef == "" {
		r.IssueRef = canonicalIssueRef(r.PRRef)
	}
	applyNumericMetadata(b, &r)
	applyTimeline(tl, &r)
	applyPRMetadata(b, &r)
	if attempts != nil {
		if repo, number, ok := splitPRRef(r.PRRef); ok {
			if n := attempts.Attempts(repo, number); n > r.FixAttempts {
				r.FixAttempts = n
			}
		}
	}
	if r.CIFailureCount < r.FixAttempts {
		r.CIFailureCount = r.FixAttempts
	}
	if r.ClaimedAt.IsZero() {
		r.ClaimedAt = b.CreatedAt.Time
	}
	if !r.ClaimedAt.IsZero() && !r.ClosedAt.IsZero() && r.ClosedAt.After(r.ClaimedAt) {
		r.ClaimToClose = r.ClosedAt.Sub(r.ClaimedAt)
	}
	return r
}

func (r RetroRecord) HasAssociatedClosedPR() bool {
	if r.PRRef == "" {
		return false
	}
	state := strings.ToLower(r.PRState)
	return state == "merged" || state == "closed" || state == "close" || state == "done"
}

func Detect(r RetroRecord, t Thresholds) []Finding {
	t = thresholdsWithDefaults(t)
	var findings []Finding
	if r.FixAttempts >= t.MaxFixAttempts {
		findings = append(findings, Finding{Pattern: PatternExcessiveFixAttempts, Severity: "medium", Title: fmt.Sprintf("Retro: %s needed %d fix attempts", r.BeadID, r.FixAttempts), Detail: fmt.Sprintf("PR %s required %d failed CI/fix attempt(s), meeting the threshold of %d.", r.PRRef, r.FixAttempts, t.MaxFixAttempts)})
	}
	if r.KicksReceived >= t.MaxKicks {
		findings = append(findings, Finding{Pattern: PatternExcessiveKicks, Severity: "low", Title: fmt.Sprintf("Retro: %s needed %d kicks before completion", r.BeadID, r.KicksReceived), Detail: fmt.Sprintf("Bead %s received %d kicks before completion, meeting the threshold of %d.", r.BeadID, r.KicksReceived, t.MaxKicks)})
	}
	if r.ClaimToClose > t.LongStall {
		findings = append(findings, Finding{Pattern: PatternLongStall, Severity: "medium", Title: fmt.Sprintf("Retro: %s stalled for %s", r.BeadID, roundDuration(r.ClaimToClose)), Detail: fmt.Sprintf("Bead %s took %s from claim to close, exceeding %s.", r.BeadID, roundDuration(r.ClaimToClose), roundDuration(t.LongStall))})
	}
	if r.DriftPauses > 0 {
		findings = append(findings, Finding{Pattern: PatternDriftPause, Severity: "medium", Title: fmt.Sprintf("Retro: %s had trajectory drift pause", r.BeadID), Detail: fmt.Sprintf("Trajectory review paused or flagged drift %d time(s) during PR %s.", r.DriftPauses, r.PRRef)})
	}
	return findings
}

func thresholdsFromConfig(c Config) Thresholds {
	longStallDays := c.LongStallDays
	if longStallDays <= 0 {
		longStallDays = DefaultLongStallDays
	}
	return thresholdsWithDefaults(Thresholds{MaxFixAttempts: c.MaxFixAttempts, MaxKicks: c.MaxKicks, LongStall: time.Duration(longStallDays) * 24 * time.Hour})
}

func thresholdsWithDefaults(t Thresholds) Thresholds {
	if t.MaxFixAttempts <= 0 {
		t.MaxFixAttempts = DefaultMaxFixAttempts
	}
	if t.MaxKicks <= 0 {
		t.MaxKicks = DefaultMaxKicks
	}
	if t.LongStall <= 0 {
		t.LongStall = defaultLongStallHours * time.Hour
	}
	return t
}

func applyNumericMetadata(b *beads.Bead, r *RetroRecord) {
	r.KicksReceived = maxInt(r.KicksReceived, metaInt(b, "kicks_received"), metaInt(b, "kick_count"), metaInt(b, "kicks"))
	r.CIFailureCount = maxInt(r.CIFailureCount, metaInt(b, "ci_failure_count"), metaInt(b, "ci_failures"), metaInt(b, "failing_checks"))
	r.FixAttempts = maxInt(r.FixAttempts, metaInt(b, "fix_attempts"), metaInt(b, "failed_fix_attempts"))
	r.DriftPauses = maxInt(r.DriftPauses, metaInt(b, "drift_pauses"), metaInt(b, "trajectory_pauses"), metaInt(b, "trajectory_drift_pauses"))
}

func applyPRMetadata(b *beads.Bead, r *RetroRecord) {
	if r.PRRef == "" {
		repo := firstNonEmpty(metaString(b, "pr_repo"), repoPart(r.IssueRef))
		if n := metaInt(b, "pr_number"); repo != "" && n > 0 {
			r.PRRef = fmt.Sprintf("%s#%d", repo, n)
		}
	}
	if r.PRState == "" {
		if metaBool(b, "pr_merged") || metaString(b, "merged_at") != "" {
			r.PRState = "merged"
		} else if metaString(b, "pr_closed_at") != "" || metaString(b, "closed_pr_at") != "" {
			r.PRState = "closed"
		}
	}
}

func applyTimeline(tl *timeline.Store, r *RetroRecord) {
	if tl == nil || r.IssueRef == "" {
		return
	}
	events := tl.ByIssue(r.IssueRef)
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Kind {
		case timeline.KindKicked:
			r.KicksReceived++
			if r.ClaimedAt.IsZero() {
				r.ClaimedAt = time.UnixMilli(e.At)
			}
		case timeline.KindPROpened:
			if r.PRRef == "" {
				r.PRRef = firstNonEmpty(attr(e, "pr_ref"), attr(e, "pr"), prRefFromAttrs(e.Attrs, r.IssueRef))
			}
		case timeline.KindMerged:
			if r.PRRef == "" {
				r.PRRef = firstNonEmpty(attr(e, "pr_ref"), attr(e, "pr"), prRefFromAttrs(e.Attrs, r.IssueRef))
			}
			r.PRState = "merged"
		}
		if isDriftPause(e) {
			r.DriftPauses++
		}
		if state := strings.ToLower(firstNonEmpty(attr(e, "pr_state"), attr(e, "state"))); state == "closed" || state == "merged" {
			r.PRState = state
		}
	}
}

func isDriftPause(e timeline.Event) bool {
	text := strings.ToLower(strings.Join([]string{string(e.Kind), e.Agent, attr(e, "action"), attr(e, "trigger"), attr(e, "reason")}, " "))
	return strings.Contains(text, "trajectory") && (strings.Contains(text, "drift") || strings.Contains(text, "pause"))
}

func attr(e timeline.Event, key string) string {
	if e.Attrs == nil {
		return ""
	}
	return e.Attrs[key]
}

func prRefFromAttrs(attrs map[string]string, issueRef string) string {
	if len(attrs) == 0 {
		return ""
	}
	repo := firstNonEmpty(attrs["pr_repo"], repoPart(issueRef))
	if repo == "" {
		return ""
	}
	for _, key := range []string{"pr_number", "number", "pr"} {
		if n, err := strconv.Atoi(strings.TrimPrefix(attrs[key], "#")); err == nil && n > 0 {
			return fmt.Sprintf("%s#%d", repo, n)
		}
	}
	return ""
}

func closedAt(b *beads.Bead) time.Time {
	return completedAt(b)
}

func canonicalIssueRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if strings.Count(ref, "#") == 1 && !strings.HasPrefix(ref, "#") {
		return ref
	}
	return ""
}

func splitPRRef(ref string) (string, int, bool) {
	parts := strings.Split(strings.TrimSpace(ref), "#")
	if len(parts) != 2 || parts[0] == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[1])
	return parts[0], n, err == nil && n > 0
}

func repoPart(ref string) string {
	if repo, _, ok := splitPRRef(ref); ok {
		return repo
	}
	return ""
}

func metaString(b *beads.Bead, key string) string {
	if b == nil || b.Metadata == nil {
		return ""
	}
	v, ok := b.Metadata[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return strings.TrimSpace(x.String())
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", x))
	}
}

func metaInt(b *beads.Bead, key string) int {
	s := metaString(b, key)
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func metaBool(b *beads.Bead, key string) bool {
	s := strings.ToLower(metaString(b, key))
	return s == "true" || s == "yes" || s == "1"
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func severityToPriority(sev string) beads.Priority {
	switch strings.ToLower(sev) {
	case "critical":
		return beads.PriorityCritical
	case "high":
		return beads.PriorityHigh
	case "medium":
		return beads.PriorityMedium
	case "low":
		return beads.PriorityLow
	default:
		return beads.PriorityMinor
	}
}

func roundDuration(d time.Duration) time.Duration {
	if d < time.Hour {
		return d.Round(time.Minute)
	}
	return d.Round(time.Hour)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func maxInt(vals ...int) int {
	m := 0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

var _ AttemptReader = (*escalation.Store)(nil)
