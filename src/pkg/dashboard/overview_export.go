package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	overviewKindIssues = "issues"
	overviewKindPRs    = "prs"
	overviewCSVType    = "text/csv; charset=utf-8"
	overviewJSONType   = "application/json"
)

var plainNumberCellPattern = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

type overviewFilters struct {
	bands map[string]bool
	repos map[string]bool
	stale *bool
	held  *bool
}

type overviewJSONResponse struct {
	Kind        string     `json:"kind"`
	GeneratedAt time.Time  `json:"generated_at"`
	HiveID      string     `json:"hive_id"`
	StaleDays   int        `json:"stale_days"`
	Bands       []BandSpec `json:"bands"`
	Rows        []any      `json:"rows"`
}

func (s *Server) handleOverviewIssuesCSV(w http.ResponseWriter, r *http.Request) {
	s.handleOverviewExport(w, r, overviewKindIssues, "csv")
}

func (s *Server) handleOverviewIssuesJSON(w http.ResponseWriter, r *http.Request) {
	s.handleOverviewExport(w, r, overviewKindIssues, "json")
}

func (s *Server) handleOverviewPRsCSV(w http.ResponseWriter, r *http.Request) {
	s.handleOverviewExport(w, r, overviewKindPRs, "csv")
}

func (s *Server) handleOverviewPRsJSON(w http.ResponseWriter, r *http.Request) {
	s.handleOverviewExport(w, r, overviewKindPRs, "json")
}

func (s *Server) handleOverviewExport(w http.ResponseWriter, r *http.Request, kind, format string) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		w.Header().Set("Content-Type", overviewJSONType)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
		return
	}
	cfg := config.DashboardIssueBandsConfig{}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		cfg = s.deps.Config.Dashboard.IssueBands
	}
	validBands := issueBandOrder
	if kind == overviewKindPRs {
		validBands = prBandOrder
	}
	filters, err := parseOverviewFilters(r, validBands)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	rows, bands := s.overviewRows(status, kind, cfg, filters, now)
	if format == "json" {
		w.Header().Set("Content-Type", overviewJSONType)
		resp := overviewJSONResponse{Kind: kind, GeneratedAt: now, HiveID: status.HiveID, StaleDays: normalizeIssueBandsConfig(cfg).StaleDays, Bands: bands, Rows: rows}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slogWarnOverviewEncode(err)
		}
		return
	}
	w.Header().Set("Content-Type", overviewCSVType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+overviewFilename(status.HiveID, kind, filters, bands, now)+`"`)
	if _, err := w.Write([]byte(overviewCSV(rows, overviewCSVColumns(kind)))); err != nil {
		slogWarnOverviewEncode(err)
	}
}

func slogWarnOverviewEncode(err error) {
	if err != nil {
		slog.Warn("overview export encode failed", "error", err)
	}
}

func parseOverviewFilters(r *http.Request, validBands []string) (overviewFilters, error) {
	filters := overviewFilters{}
	valid := make(map[string]bool, len(validBands))
	for _, band := range validBands {
		valid[band] = true
	}
	bands := splitQueryValues(r.URL.Query()["band"])
	if len(bands) > 0 {
		filters.bands = map[string]bool{}
		bad := make([]string, 0)
		for _, band := range bands {
			if !valid[band] {
				bad = append(bad, band)
				continue
			}
			filters.bands[band] = true
		}
		if len(bad) > 0 {
			return filters, fmt.Errorf("unknown band %q; valid bands: %s", strings.Join(bad, ","), strings.Join(validBands, ", "))
		}
	}
	repos := splitQueryValues(r.URL.Query()["repo"])
	if len(repos) > 0 {
		filters.repos = map[string]bool{}
		for _, repo := range repos {
			filters.repos[strings.ToLower(repo)] = true
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("stale")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return filters, fmt.Errorf("invalid stale %q: expected true or false", raw)
		}
		filters.stale = &v
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("held")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return filters, fmt.Errorf("invalid held %q: expected true or false", raw)
		}
		filters.held = &v
	}
	return filters, nil
}

func splitQueryValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
	}
	return out
}

func (s *Server) overviewRows(status *StatusPayload, kind string, cfg config.DashboardIssueBandsConfig, filters overviewFilters, now time.Time) ([]any, []BandSpec) {
	if kind == overviewKindPRs {
		return s.overviewPRRows(status, cfg, filters, now)
	}
	return s.overviewIssueRows(status, cfg, filters, now)
}

type overviewIssueItem struct {
	issue github.Issue
	held  bool
	info  IssueBandInfo
}

// overviewIssueItems classifies every open and held issue across the repo
// cards that pass filters. It is the one walk the CSV/JSON export and the
// owner-advice queue breakdown both read, so they cannot disagree.
func overviewIssueItems(status *StatusPayload, cfg config.DashboardIssueBandsConfig, filters overviewFilters, now time.Time) []overviewIssueItem {
	var out []overviewIssueItem
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		if !filters.matchRepo(repoName) {
			continue
		}
		for _, issue := range frontendRepoIssues(repo, false) {
			info := IssueBand(issue, false, cfg, now)
			if !filters.matchBand(info.Band) || !filters.matchFlags(info.Stale, false) {
				continue
			}
			issue.Repo = nonEmpty(issue.Repo, repoName)
			out = append(out, overviewIssueItem{issue: issue, info: info})
		}
		for _, issue := range frontendRepoIssues(repo, true) {
			issue.Repo = nonEmpty(issue.Repo, repoName)
			info := IssueBand(issue, true, cfg, now)
			info.HoldReason = heldReason(issue.Labels, false, status.HiveID)
			if !filters.matchBand(info.Band) || !filters.matchFlags(info.Stale, true) {
				continue
			}
			out = append(out, overviewIssueItem{issue: issue, held: true, info: info})
		}
	}
	return out
}

func (s *Server) overviewIssueRows(status *StatusPayload, cfg config.DashboardIssueBandsConfig, filters overviewFilters, now time.Time) ([]any, []BandSpec) {
	byBand := make(map[string][]overviewIssueItem, len(issueBandOrder))
	counts := make(map[string]int, len(issueBandOrder))
	for _, key := range issueBandOrder {
		byBand[key] = nil
	}
	for _, item := range overviewIssueItems(status, cfg, filters, now) {
		counts[item.info.Band]++
		byBand[item.info.Band] = append(byBand[item.info.Band], item)
	}
	rows := make([]any, 0)
	for _, band := range issueBandOrder {
		items := byBand[band]
		sort.SliceStable(items, func(i, j int) bool {
			ai := issueActivity(items[i].issue)
			aj := issueActivity(items[j].issue)
			if !ai.Equal(aj) {
				return ai.Before(aj)
			}
			return items[i].issue.Number < items[j].issue.Number
		})
		for _, item := range items {
			rows = append(rows, overviewIssueCSVRow(item.issue, item.held, item.info, cfg, status.GitHubBaseURL))
		}
	}
	specs := IssueBandSpecs(cfg)
	for i := range specs {
		specs[i].Count = counts[specs[i].Key]
	}
	return rows, specs
}

type overviewPRItem struct {
	pr      github.PullRequest
	verdict *github.MergeVerdict
	held    bool
	info    PRBandInfo
}

// overviewPRItems classifies every open and held PR across the repo cards
// that pass filters; see overviewIssueItems.
func (s *Server) overviewPRItems(status *StatusPayload, cfg config.DashboardIssueBandsConfig, filters overviewFilters, now time.Time) []overviewPRItem {
	var out []overviewPRItem
	autoMergeLabel := s.autoMergeLabel()
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		if !filters.matchRepo(repoName) {
			continue
		}
		for _, entry := range frontendRepoPRs(repo, false) {
			entry.pr.Repo = nonEmpty(entry.pr.Repo, repoName)
			info := prBand(entry.pr, entry.verdict, false, cfg, now, autoMergeLabel, status.HiveID)
			if !filters.matchBand(info.Band) || !filters.matchFlags(info.Stale, false) {
				continue
			}
			out = append(out, overviewPRItem{pr: entry.pr, verdict: entry.verdict, info: info})
		}
		for _, entry := range frontendRepoPRs(repo, true) {
			entry.pr.Repo = nonEmpty(entry.pr.Repo, repoName)
			info := prBand(entry.pr, entry.verdict, true, cfg, now, autoMergeLabel, status.HiveID)
			if !filters.matchBand(info.Band) || !filters.matchFlags(info.Stale, true) {
				continue
			}
			out = append(out, overviewPRItem{pr: entry.pr, verdict: entry.verdict, held: true, info: info})
		}
	}
	return out
}

func (s *Server) overviewPRRows(status *StatusPayload, cfg config.DashboardIssueBandsConfig, filters overviewFilters, now time.Time) ([]any, []BandSpec) {
	byBand := make(map[string][]overviewPRItem, len(prBandOrder))
	counts := make(map[string]int, len(prBandOrder))
	for _, item := range s.overviewPRItems(status, cfg, filters, now) {
		counts[item.info.Band]++
		byBand[item.info.Band] = append(byBand[item.info.Band], item)
	}
	rows := make([]any, 0)
	for _, band := range prBandOrder {
		items := byBand[band]
		sort.SliceStable(items, func(i, j int) bool {
			ai := issueActivityPR(items[i].pr)
			aj := issueActivityPR(items[j].pr)
			if !ai.Equal(aj) {
				return ai.Before(aj)
			}
			ri := prReviewClassRank(items[i].pr)
			rj := prReviewClassRank(items[j].pr)
			if ri != rj {
				return ri < rj
			}
			if !items[i].pr.CreatedAt.Equal(items[j].pr.CreatedAt) {
				return items[i].pr.CreatedAt.Before(items[j].pr.CreatedAt)
			}
			return items[i].pr.Number < items[j].pr.Number
		})
		for _, item := range items {
			rows = append(rows, overviewPRCSVRow(item.pr, item.verdict, item.held, item.info, cfg, status.GitHubBaseURL))
		}
	}
	specs := PRBandSpecs(cfg)
	for i := range specs {
		specs[i].Count = counts[specs[i].Key]
	}
	return rows, specs
}

func (f overviewFilters) matchRepo(repo string) bool {
	return len(f.repos) == 0 || f.repos[strings.ToLower(repo)]
}

func (f overviewFilters) matchBand(band string) bool {
	return len(f.bands) == 0 || f.bands[band]
}

func (f overviewFilters) matchFlags(stale, held bool) bool {
	if f.stale != nil && *f.stale != stale {
		return false
	}
	if f.held != nil && *f.held != held {
		return false
	}
	return true
}

func overviewRepoName(repo FrontendRepo) string {
	if repo.Full != "" {
		return repo.Full
	}
	return repo.Name
}

func frontendRepoIssues(repo FrontendRepo, held bool) []github.Issue {
	source := repo.ActionableIssues
	if held {
		source = repo.HeldIssues
	}
	out := make([]github.Issue, 0, len(source))
	for _, raw := range source {
		if issue, ok := frontendIssue(raw); ok {
			out = append(out, issue)
		}
	}
	return out
}

func frontendIssue(raw any) (github.Issue, bool) {
	switch v := raw.(type) {
	case github.Issue:
		return v, true
	case *github.Issue:
		if v == nil {
			return github.Issue{}, false
		}
		return *v, true
	case github.HoldItem:
		return holdItemIssue(v), true
	case *github.HoldItem:
		if v == nil {
			return github.Issue{}, false
		}
		return holdItemIssue(*v), true
	default:
		return github.Issue{}, false
	}
}

func holdItemIssue(item github.HoldItem) github.Issue {
	return github.Issue{Repo: item.Repo, Number: item.Number, Title: item.Title, URL: item.URL, Labels: item.Labels, Assignees: item.Assignees, HumanAcknowledged: item.HumanAcknowledged, CreatedAt: item.CreatedAt}
}

type frontendPR struct {
	pr      github.PullRequest
	verdict *github.MergeVerdict
}

func frontendRepoPRs(repo FrontendRepo, held bool) []frontendPR {
	source := repo.OpenPrs
	if held {
		source = repo.HeldPrs
	}
	out := make([]frontendPR, 0, len(source))
	for _, raw := range source {
		if pr, ok := frontendPullRequest(raw); ok {
			out = append(out, pr)
		}
	}
	return out
}

func frontendPullRequest(raw any) (frontendPR, bool) {
	switch v := raw.(type) {
	case FrontendPR:
		return frontendPR{pr: v.PullRequest, verdict: v.MergeVerdict}, true
	case *FrontendPR:
		if v == nil {
			return frontendPR{}, false
		}
		return frontendPR{pr: v.PullRequest, verdict: v.MergeVerdict}, true
	case github.PullRequest:
		return frontendPR{pr: v}, true
	case *github.PullRequest:
		if v == nil {
			return frontendPR{}, false
		}
		return frontendPR{pr: *v}, true
	default:
		return frontendPR{}, false
	}
}

func overviewIssueCSVRow(issue github.Issue, held bool, info IssueBandInfo, cfg config.DashboardIssueBandsConfig, githubBaseURL string) map[string]any {
	return map[string]any{
		"band":          issueBandSpec(info.Band, cfg).Label,
		"band_rule":     issueBandSpec(info.Band, cfg).Rule,
		"repo":          issue.Repo,
		"number":        numberCell(issue.Number),
		"title":         issue.Title,
		"url":           overviewItemURL(issue.URL, issue.Repo, issue.Number, "issue", githubBaseURL),
		"state_signals": signalLabels(info.Signals),
		"labels":        stringsSliceAny(issue.Labels),
		"assignees":     stringsSliceAny(issue.Assignees),
		"agent_role":    info.Role,
		"acknowledged":  info.Acknowledged,
		"held":          held,
		"hold_reason":   heldString(held, info.HoldReason),
		"linked_prs":    linkedPRs(issue.LinkedPRs),
		"updated_at":    formatUpdatedAt(issue.UpdatedAt),
		"stale":         info.Stale,
	}
}

func overviewPRCSVRow(pr github.PullRequest, verdict *github.MergeVerdict, held bool, info PRBandInfo, cfg config.DashboardIssueBandsConfig, githubBaseURL string) map[string]any {
	return map[string]any{
		"band":            prBandSpec(info.Band, cfg).Label,
		"band_rule":       prBandSpec(info.Band, cfg).Rule,
		"repo":            pr.Repo,
		"number":          numberCell(pr.Number),
		"title":           pr.Title,
		"url":             overviewItemURL(pr.URL, pr.Repo, pr.Number, "pr", githubBaseURL),
		"author":          pr.Author,
		"draft":           pr.Draft,
		"merge_verdict":   mergeVerdictText(verdict),
		"mergeable":       string(pr.Mergeable),
		"failing_checks":  stringsSliceAny(pr.FailingChecks),
		"review_decision": reviewDecision(pr),
		"review_class":    string(pr.ReviewClass),
		"held":            held,
		"hold_reason":     heldString(held, info.HoldReason),
		"labels":          stringsSliceAny(pr.Labels),
		"updated_at":      formatUpdatedAt(pr.UpdatedAt),
		"stale":           info.Stale,
		"signals":         signalLabels(info.Signals),
	}
}

func overviewCSVColumns(kind string) []string {
	if kind == overviewKindPRs || kind == "pr" {
		return []string{"band", "band_rule", "repo", "number", "title", "url", "author", "draft", "merge_verdict", "mergeable", "failing_checks", "review_decision", "review_class", "held", "hold_reason", "labels", "updated_at", "stale", "signals"}
	}
	return []string{"band", "band_rule", "repo", "number", "title", "url", "state_signals", "labels", "assignees", "agent_role", "acknowledged", "held", "hold_reason", "linked_prs", "updated_at", "stale"}
}

func overviewCSV(rows []any, columns []string) string {
	var b strings.Builder
	b.WriteRune('\ufeff')
	lineBreak := "\r\n"
	b.WriteString(strings.Join(encodeCSVRecord(columns), ","))
	b.WriteString(lineBreak)
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		record := make([]string, 0, len(columns))
		for _, column := range columns {
			record = append(record, overviewCSVCell(m[column]))
		}
		b.WriteString(strings.Join(record, ","))
		b.WriteString(lineBreak)
	}
	return b.String()
}

func encodeCSVRecord(columns []string) []string {
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		out = append(out, overviewCSVCell(column))
	}
	return out
}

func overviewCSVCell(v any) string {
	var text string
	switch value := v.(type) {
	case nil:
		text = ""
	case bool:
		if value {
			text = "true"
		} else {
			text = "false"
		}
	case []string:
		text = strings.Join(value, ";")
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if item == nil {
				parts = append(parts, "")
			} else {
				parts = append(parts, fmt.Sprint(item))
			}
		}
		text = strings.Join(parts, ";")
	default:
		text = fmt.Sprint(value)
	}
	if text != "" && strings.ContainsAny(text[:1], "=+-@\t\r") && !plainNumberCellPattern.MatchString(text) {
		text = "'" + text
	}
	if strings.ContainsAny(text, "\",\r\n") {
		return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
	}
	return text
}

func overviewFilename(hiveID, kind string, filters overviewFilters, bands []BandSpec, now time.Time) string {
	parts := []string{"hive", safeOverviewFilenamePart(nonEmpty(hiveID, "hive")), kind}
	if len(filters.bands) == 1 {
		for _, spec := range bands {
			if filters.bands[spec.Key] {
				parts = append(parts, overviewBandSlug(spec.Label))
				break
			}
		}
	}
	parts = append(parts, now.Format("20060102-1504"))
	return strings.Join(parts, "-") + ".csv"
}

func overviewBandSlug(label string) string {
	replacer := strings.NewReplacer("&", " and ")
	text := strings.ToLower(replacer.Replace(label))
	var b strings.Builder
	lastDash := false
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "band"
	}
	return out
}

func safeOverviewFilenamePart(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "hive"
	}
	return out
}

func issueActivity(issue github.Issue) time.Time {
	if !issue.UpdatedAt.IsZero() {
		return issue.UpdatedAt
	}
	if !issue.CreatedAt.IsZero() {
		return issue.CreatedAt
	}
	return time.Unix(1<<62, 0)
}

func issueActivityPR(pr github.PullRequest) time.Time {
	if !pr.UpdatedAt.IsZero() {
		return pr.UpdatedAt
	}
	if !pr.CreatedAt.IsZero() {
		return pr.CreatedAt
	}
	return time.Unix(1<<62, 0)
}

func prReviewClassRank(pr github.PullRequest) int {
	switch string(pr.ReviewClass) {
	case "fix":
		return 0
	case "tests":
		return 2
	default:
		return 1
	}
}

func formatUpdatedAt(updatedAt time.Time) string {
	if updatedAt.IsZero() {
		return ""
	}
	return updatedAt.UTC().Format(time.RFC3339Nano)
}

func overviewItemURL(rawURL, repo string, number int, kind, githubBaseURL string) string {
	if rawURL != "" {
		return rawURL
	}
	if repo == "" || number == 0 {
		return ""
	}
	base := strings.TrimRight(nonEmpty(githubBaseURL, "https://github.com"), "/")
	path := "issues"
	if kind == "pr" {
		path = "pull"
	}
	return fmt.Sprintf("%s/%s/%s/%d", base, repo, path, number)
}

func signalLabels(signals []Signal) []any {
	out := make([]any, 0, len(signals))
	for _, signal := range signals {
		if signal.Label != "" {
			out = append(out, signal.Label)
		}
	}
	return out
}

func stringsSliceAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func linkedPRs(prs []github.IssueLinkedPR) []any {
	out := make([]any, 0, len(prs))
	for _, pr := range prs {
		if pr.Number == 0 {
			continue
		}
		num := fmt.Sprintf("#%d", pr.Number)
		state := pr.State
		if pr.Merged {
			state = "merged"
		}
		label := ""
		if pr.Repo != "" {
			label += pr.Repo
		}
		label += num
		if state != "" {
			label += " " + state
		}
		out = append(out, label)
	}
	return out
}

func mergeVerdictText(verdict *github.MergeVerdict) string {
	if verdict == nil {
		return ""
	}
	if verdict.State != "" && verdict.Reason != "" {
		return string(verdict.State) + ": " + verdict.Reason
	}
	if verdict.State != "" {
		return string(verdict.State)
	}
	return verdict.Reason
}

func reviewDecision(pr github.PullRequest) string {
	if pr.Protection == nil {
		return ""
	}
	return string(pr.Protection.ReviewDecision)
}

func numberCell(n int) any {
	if n == 0 {
		return ""
	}
	return n
}

func heldString(held bool, reason string) string {
	if !held {
		return ""
	}
	return reason
}

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
