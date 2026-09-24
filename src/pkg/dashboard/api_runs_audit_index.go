package dashboard

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	runAuditIndexDefaultLimit = 100
	runAuditIndexMaxLimit     = 500
	runAuditIndexVersion      = "existing-artifacts/v1"
)

var errRunAuditIndexPagination = errors.New("invalid pagination parameter")

type runAuditIndexResponse struct {
	Items         []runAuditIndexItem    `json:"items"`
	NextPageToken string                 `json:"next_page_token,omitempty"`
	Retention     runAuditIndexRetention `json:"retention"`
}

type runAuditIndexRetention struct {
	Contract          string `json:"contract"`
	AuditLog          string `json:"audit_log"`
	Timeline          string `json:"timeline"`
	ExpiredMarkerKind string `json:"expired_marker_kind"`
}

type runAuditIndexItem struct {
	Source     string            `json:"source"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status,omitempty"`
	Run        string            `json:"run,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	At         string            `json:"at,omitempty"`
	Actor      string            `json:"actor,omitempty"`
	ArtifactID string            `json:"artifact_id,omitempty"`
	Message    string            `json:"message,omitempty"`
	Attrs      map[string]string `json:"attrs,omitempty"`
}

type runAuditIndexQuery struct {
	repo        string
	run         string
	since       time.Time
	until       time.Time
	kind        string
	limit       int
	offset      int
	explicitRun bool
}

// handleRunAuditIndex serves GET /api/runs/audit. It is intentionally read-only:
// it joins the retained audit log, lifecycle timeline, lease receipts, and plan
// epics that Hive already writes instead of introducing another index store.
func (s *Server) handleRunAuditIndex(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	q, err := parseRunAuditIndexQuery(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	items := s.runAuditIndexItems(q)
	sortRunAuditIndexItems(items)
	items, next := paginateRunAuditIndex(items, q.offset, q.limit)
	jsonResponse(w, runAuditIndexResponse{
		Items:         items,
		NextPageToken: next,
		Retention: runAuditIndexRetention{
			Contract:          runAuditIndexVersion,
			AuditLog:          "dashboard audit log files and rotated backups: max_age=90d, max_size=5MiB, max_backups=3; in-memory fallback keeps the latest 500 entries",
			Timeline:          "lifecycle timeline journeys: capacity=500 journeys (timeline.MaxJourneys), persisted when lifecycle persistence is enabled",
			ExpiredMarkerKind: "expired",
		},
	})
}

func parseRunAuditIndexQuery(r *http.Request) (runAuditIndexQuery, error) {
	values := r.URL.Query()
	q := runAuditIndexQuery{
		repo:   strings.TrimSpace(values.Get("repo")),
		run:    strings.TrimSpace(values.Get("run")),
		kind:   strings.TrimSpace(strings.ToLower(values.Get("kind"))),
		limit:  runAuditIndexDefaultLimit,
		offset: 0,
	}
	q.explicitRun = q.run != ""
	if q.run != "" {
		if unescaped := strings.ReplaceAll(q.run, "%23", "#"); unescaped != q.run {
			q.run = unescaped
		}
		if ref, ok := worksource.ParseKey(q.run); ok {
			q.run = ref.Key()
			if q.repo == "" {
				q.repo = ref.Repo
			}
		}
	}
	var err error
	if raw := strings.TrimSpace(values.Get("since")); raw != "" {
		q.since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, err
		}
	}
	if raw := strings.TrimSpace(values.Get("until")); raw != "" {
		q.until, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, err
		}
	}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return q, errRunAuditIndexPagination
		}
		q.limit = limit
		if q.limit > runAuditIndexMaxLimit {
			q.limit = runAuditIndexMaxLimit
		}
	}
	if raw := strings.TrimSpace(values.Get("page_token")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return q, errRunAuditIndexPagination
		}
		q.offset = offset
	}
	return q, nil
}

func (s *Server) runAuditIndexItems(q runAuditIndexQuery) []runAuditIndexItem {
	items := make([]runAuditIndexItem, 0)
	items = append(items, s.runAuditIndexAuditLog(q)...)
	items = append(items, s.runAuditIndexTimeline(q)...)
	items = append(items, s.runAuditIndexPlanEpics(q)...)
	items = append(items, runAuditExpiredMarkers(q, time.Now().UTC(), s.LifecycleTimeline())...)
	return items
}

func (s *Server) runAuditIndexAuditLog(q runAuditIndexQuery) []runAuditIndexItem {
	if s == nil || s.audit == nil {
		return nil
	}
	entries := s.runAuditIndexAuditEntries(q.since)
	out := make([]runAuditIndexItem, 0, len(entries))
	for _, e := range entries {
		at, ok := parseRunAuditTime(e.Timestamp)
		if !ok || !runAuditTimeInRange(at, q) {
			continue
		}
		attrs := parseAuditDetailAttrs(e.Detail)
		run := firstRunNonEmpty(attrs["run"], attrs["run_key"], attrs["key"], attrs["issue"], attrs["issue_ref"])
		repo := firstRunNonEmpty(attrs["repo"], attrs["issue_repo"])
		if ref, ok := worksource.ParseKey(run); ok {
			run = ref.Key()
			repo = firstRunNonEmpty(repo, ref.Repo)
		}
		if run == "" {
			continue
		}
		item := runAuditIndexItem{
			Source: "audit_log", Kind: e.Action, Run: run, Repo: repo, At: at.UTC().Format(time.RFC3339),
			Actor: firstRunNonEmpty(e.User, e.UserName), ArtifactID: e.Agent, Attrs: attrs,
		}
		if runAuditItemMatches(item, q) {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) runAuditIndexAuditEntries(since time.Time) []AuditEntry {
	if s.audit.HasOnDiskLog("") {
		return s.audit.OutputActionsSince(since, nil, "")
	}
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	out := make([]AuditEntry, 0, len(s.audit.ring))
	for _, e := range s.audit.ring {
		if at, ok := parseRunAuditTime(e.Timestamp); ok && (since.IsZero() || !at.Before(since)) {
			out = append(out, e)
		}
	}
	return out
}

func (s *Server) runAuditIndexTimeline(q runAuditIndexQuery) []runAuditIndexItem {
	store := s.LifecycleTimeline()
	if store == nil {
		return nil
	}
	events := store.Recent(0)
	out := make([]runAuditIndexItem, 0, len(events))
	for _, ev := range events {
		if ev.At <= 0 {
			continue
		}
		at := time.UnixMilli(ev.At).UTC()
		if !runAuditTimeInRange(at, q) {
			continue
		}
		run, repo := canonicalRunAuditRef(ev.IssueRef)
		attrs := cloneStringMap(ev.Attrs)
		source := "timeline"
		if ev.Kind == timeline.KindStageReceipt {
			source = "lease_receipt"
		}
		item := runAuditIndexItem{
			Source: source, Kind: string(ev.Kind), Run: run, Repo: repo, At: at.Format(time.RFC3339),
			Actor: ev.Agent, ArtifactID: ev.ID, Attrs: attrs,
		}
		if item.ArtifactID == "" && ev.Kind == timeline.KindStageReceipt {
			item.ArtifactID = firstRunNonEmpty(attrs["receipt"], attrs["receipt_digest"], attrs["path"], attrs["digest"])
		}
		if runAuditItemMatches(item, q) {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) runAuditIndexPlanEpics(q runAuditIndexQuery) []runAuditIndexItem {
	if s == nil || s.deps == nil {
		return nil
	}
	out := []runAuditIndexItem{}
	for storeName, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Type != beads.TypeEpic || (b.Meta(planning.MetaPlanStatus) == "" && b.Meta(planning.MetaRunKey) == "" && b.Meta(planning.MetaIssueRepo) == "") {
				continue
			}
			at := b.UpdatedAt.Time.UTC()
			if !runAuditTimeInRange(at, q) {
				continue
			}
			run := runAuditKeyFromPlan(s, b)
			repo := firstRunNonEmpty(b.Meta(planning.MetaIssueRepo), repoFromRunAuditKey(run))
			item := runAuditIndexItem{
				Source: "plan_epic", Kind: "plan_epic", Run: run, Repo: repo, At: at.Format(time.RFC3339),
				Actor: b.Actor, ArtifactID: storeName + ":" + b.ID,
				Attrs: map[string]string{
					"title":       b.Title,
					"plan_status": b.Meta(planning.MetaPlanStatus),
					"run_key":     b.Meta(planning.MetaRunKey),
				},
			}
			if runAuditItemMatches(item, q) {
				out = append(out, item)
			}
		}
	}
	return out
}

func runAuditKeyFromPlan(s *Server, b *beads.Bead) string {
	repo := b.Meta(planning.MetaIssueRepo)
	number, _ := strconv.Atoi(strings.TrimSpace(b.Meta(planning.MetaIssueNumber)))
	runKey := b.Meta(planning.MetaRunKey)
	if s != nil {
		return s.canonicalRunKey(repo, number, runKey, b.ExternalRef)
	}
	if ref, ok := worksource.ParseKey(runKey); ok {
		return ref.Key()
	}
	if number > 0 && repo != "" {
		return worksource.Ref{Repo: repo, Number: number}.Key()
	}
	return firstRunNonEmpty(runKey, b.ExternalRef)
}

func runAuditExpiredMarkers(q runAuditIndexQuery, now time.Time, store *timeline.Store) []runAuditIndexItem {
	if q.since.IsZero() {
		return nil
	}
	out := []runAuditIndexItem{}
	auditCutoff := now.AddDate(0, 0, -auditMaxAgeDays)
	if q.since.Before(auditCutoff) {
		out = append(out, runAuditExpiredMarker(q, "audit_log", q.since, "audit log entries before retained rotation coverage may have aged out"))
	}
	oldest := oldestRunAuditTimelineAt(store)
	if !oldest.IsZero() && q.since.Before(oldest) {
		out = append(out, runAuditExpiredMarker(q, "timeline", q.since, "timeline journeys before retained capacity may have aged out"))
	}
	if len(out) == 0 && q.explicitRun {
		// If a caller asks about an old run and no source can prove coverage that far
		// back, return Unknown/expired rather than an empty response that looks final.
		if q.since.Before(auditCutoff) || oldest.IsZero() {
			out = append(out, runAuditExpiredMarker(q, "retention", q.since, "requested run may be outside retained artifacts"))
		}
	}
	filtered := out[:0]
	for _, item := range out {
		if runAuditItemMatches(item, q) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func runAuditExpiredMarker(q runAuditIndexQuery, source string, at time.Time, msg string) runAuditIndexItem {
	return runAuditIndexItem{
		Source: source, Kind: "expired", Status: "expired", Run: q.run, Repo: q.repo,
		At: at.UTC().Format(time.RFC3339), Message: msg,
		Attrs: map[string]string{"state": "Unknown"},
	}
}

func oldestRunAuditTimelineAt(store *timeline.Store) time.Time {
	if store == nil {
		return time.Time{}
	}
	oldest := int64(0)
	for _, j := range store.Journeys(0) {
		if j.FirstAt <= 0 {
			continue
		}
		if oldest == 0 || j.FirstAt < oldest {
			oldest = j.FirstAt
		}
	}
	if oldest == 0 {
		return time.Time{}
	}
	return time.UnixMilli(oldest).UTC()
}

func runAuditTimeInRange(t time.Time, q runAuditIndexQuery) bool {
	if !q.since.IsZero() && t.Before(q.since) {
		return false
	}
	if !q.until.IsZero() && t.After(q.until) {
		return false
	}
	return true
}

func runAuditItemMatches(item runAuditIndexItem, q runAuditIndexQuery) bool {
	if q.repo != "" && item.Repo != q.repo {
		return false
	}
	if q.run != "" && item.Run != q.run {
		return false
	}
	if q.kind != "" {
		kind := strings.ToLower(item.Kind)
		source := strings.ToLower(item.Source)
		if q.kind != kind && q.kind != source {
			return false
		}
	}
	return true
}

func sortRunAuditIndexItems(items []runAuditIndexItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].At != items[j].At {
			return items[i].At < items[j].At
		}
		if items[i].Run != items[j].Run {
			return items[i].Run < items[j].Run
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		return items[i].Kind < items[j].Kind
	})
}

func paginateRunAuditIndex(items []runAuditIndexItem, offset, limit int) ([]runAuditIndexItem, string) {
	if offset >= len(items) {
		return []runAuditIndexItem{}, ""
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[offset:end], next
}

func canonicalRunAuditRef(ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if repo, ext, ok := strings.Cut(ref, "!"); ok {
		runKey := ext
		if before, _, hasStage := strings.Cut(ext, ":"); hasStage {
			runKey = before
		}
		if parsed, ok := worksource.ParseKey(runKey); ok && parsed.Number > 0 {
			return parsed.Key(), parsed.Repo
		}
		return ref, repo
	}
	if parsed, ok := worksource.ParseKey(ref); ok {
		return parsed.Key(), parsed.Repo
	}
	return ref, repoFromRunAuditKey(ref)
}

func repoFromRunAuditKey(run string) string {
	if ref, ok := worksource.ParseKey(run); ok {
		return ref.Repo
	}
	if repo, _, ok := strings.Cut(run, "!"); ok {
		return repo
	}
	return ""
}

func parseRunAuditTime(raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return t, err == nil
}

func parseAuditDetailAttrs(detail string) map[string]string {
	attrs := map[string]string{}
	for _, part := range strings.Split(detail, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		attrs[key] = strings.TrimSpace(value)
	}
	return attrs
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
