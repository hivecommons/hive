package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
)

// Panel 6 (#12366, #12517, RFC #12062): work balance answers "is the queue
// keeping up?". Per ISO week: issues and PRs opened (by author kind) against
// issues closed and PRs merged or closed, plus the queue depth at week end.
// Three sources, each named in the row and in the explainer:
//
//   - issues: the GitHub-truth rollup the metrics collector keeps under
//     /data (every author, every issue updated in the lookback); weeks that
//     start before the rollup's coverage use the backfilled issue series,
//     with an audit fallback (issue_source "audit") where it lacks coverage.
//   - prs: the folded evidence bundles (live + backfilled), by PR open time,
//     merge time and close time.
//   - queue: the governor's hive-state snapshots (actionable open issues and
//     PRs, the same numbers as the dashboard queue), last sample in the week.
//
// Read-only reporting; nothing here is consulted by merge eligibility.

// evidenceWorkBalanceActions are the audit actions the audit-only fallback
// rows are built from.
var evidenceWorkBalanceActions = map[string]bool{
	ghpkg.AuditActionAgentIssueCreated: true,
	ghpkg.AuditActionHiveIssueCreated:  true,
	ghpkg.AuditActionIssueClosed:       true,
}

const (
	evidenceWorkBalanceSourceGitHub = "github"
	evidenceWorkBalanceSourceAudit  = "audit"
)

// EvidenceWorkBalanceWeek is one ISO week. Opened is issues opened by author
// kind; Closed is issues closed by anyone. PRsOpened is PRs opened by author
// kind; PRsMerged and PRsClosed are PR merges and closes-without-merge in the
// week. N is every one of those events. QueueIssues/QueuePRs are the
// actionable queue depth at the last governor sample in the week (nil when
// no sample). IssueSource says where the issue columns came from.
type EvidenceWorkBalanceWeek struct {
	Week        string         `json:"week"`
	N           int            `json:"n"`
	Opened      map[string]int `json:"opened"`
	Closed      int            `json:"closed"`
	PRsOpened   map[string]int `json:"prs_opened"`
	PRsMerged   int            `json:"prs_merged"`
	PRsClosed   int            `json:"prs_closed"`
	QueueIssues *int           `json:"queue_issues,omitempty"`
	QueuePRs    *int           `json:"queue_prs,omitempty"`
	IssueSource string         `json:"issue_source"`
	Throttled   bool           `json:"throttled"`
}

// EvidenceWorkBalanceSources documents the provenance of each series so the
// card can state it, and the coverage boundary of the GitHub rollup.
type EvidenceWorkBalanceSources struct {
	// IssuesGitHubSince is the start of complete GitHub-truth issue coverage
	// (RFC3339); empty when the rollup has never been written. Weeks before
	// it may use the historical issue series or the audit fallback.
	IssuesGitHubSince string `json:"issues_github_since,omitempty"`
	// IssuesFetchedAt is the rollup's last successful refresh.
	IssuesFetchedAt string `json:"issues_fetched_at,omitempty"`
	// IssuesKnown is how many issues the rollup holds.
	IssuesKnown int `json:"issues_known"`
	// AuditOnlyWeeks is how many rows in the response fell back to the audit
	// log for issues.
	AuditOnlyWeeks int `json:"audit_only_weeks"`
	// IssueSeriesWeeks counts repo-weeks supplied by the historical issue series.
	IssueSeriesWeeks int `json:"issue_series_weeks"`
	// QueueSamples is how many governor samples supplied week-end depths;
	// QueueRetentionDays is how far back the snapshots are kept.
	QueueSamples       int `json:"queue_samples"`
	QueueRetentionDays int `json:"queue_retention_days"`
}

// evidenceWorkBalanceOpenerKind maps an audited issue-create to an actor kind.
// Issue-request-watcher creates are the agent's own work; creates by the hive
// itself (advisory and ACMM-gap issues) are automation, so bot.
func evidenceWorkBalanceOpenerKind(e AuditEntry) (string, bool) {
	switch e.Action {
	case ghpkg.AuditActionAgentIssueCreated:
		return evidence.AuthorAgent, true
	case ghpkg.AuditActionHiveIssueCreated:
		return evidence.AuthorBot, true
	}
	return "", false
}

// evidenceWorkBalanceInputs is everything buildEvidenceWorkBalance reads.
type evidenceWorkBalanceInputs struct {
	Entries []AuditEntry
	Rollup  IssueActivityRollup
	PRs     map[string]*evidence.PR
	Buckets []hivestate.Bucket
	Series  []evidence.IssueSeriesWeek
}

func evidenceWeekKey(t time.Time) string {
	return evidence.IssueWeekKey(t)
}

// evidenceWeekStart is the UTC Monday 00:00 of the ISO week containing t.
func evidenceWeekStart(t time.Time) time.Time {
	t = t.UTC().Truncate(24 * time.Hour)
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

func evidenceWindowHasTime(win evidenceWindow, t time.Time) bool {
	bounded := win.Custom || win.Hours > 0
	if bounded && t.Before(win.From) {
		return false
	}
	if !win.To.IsZero() && (t.After(win.To) || (win.Custom && t.Equal(win.To))) {
		return false
	}
	return true
}

// buildEvidenceWorkBalance aggregates opens, closes and week-end queue
// depth per ISO week for the window and repo.
func buildEvidenceWorkBalance(in evidenceWorkBalanceInputs, win evidenceWindow, repo string) ([]EvidenceWorkBalanceWeek, EvidenceWorkBalanceSources) {
	weeks := map[string]*EvidenceWorkBalanceWeek{}
	row := func(t time.Time) *EvidenceWorkBalanceWeek {
		key := evidenceWeekKey(t)
		r := weeks[key]
		if r == nil {
			r = &EvidenceWorkBalanceWeek{Week: key, Opened: map[string]int{}, PRsOpened: map[string]int{}, IssueSource: evidenceWorkBalanceSourceAudit}
			for _, k := range []string{evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot} {
				r.Opened[k] = 0
				r.PRsOpened[k] = 0
			}
			weeks[key] = r
		}
		return r
	}
	var src EvidenceWorkBalanceSources
	githubSince := in.Rollup.Since
	if !githubSince.IsZero() {
		src.IssuesGitHubSince = githubSince.UTC().Format(time.RFC3339)
	}
	if !in.Rollup.FetchedAt.IsZero() {
		src.IssuesFetchedAt = in.Rollup.FetchedAt.UTC().Format(time.RFC3339)
	}
	src.IssuesKnown = len(in.Rollup.Issues)
	src.QueueRetentionDays = int(hivestate.Retention.Hours() / 24)
	githubWeek := func(t time.Time) bool {
		return !githubSince.IsZero() && !evidenceWeekStart(t).Before(githubSince)
	}
	// The collector remains authoritative for every week it fully covers.
	// Backfills replace audit data per repository, never an entire mixed row.
	seriesWeeks := map[string]bool{}
	for _, series := range in.Series {
		w := series.IssueWeek
		if repo != "" && !strings.EqualFold(series.Repo, repo) {
			continue
		}
		start := evidenceWeekStart(w.CoveredFrom)
		end := start.AddDate(0, 0, 7)
		if githubWeek(start) || evidenceWeekKey(start) != w.Week {
			continue
		}
		if (win.Custom || win.Hours > 0) && win.From.After(start) {
			start = win.From
		}
		if !win.To.IsZero() && win.To.Before(end) {
			end = win.To
		}
		if !end.After(start) || w.CoveredFrom.After(start) || w.CoveredTo.Before(end) {
			continue
		}
		key := strings.ToLower(series.Repo) + "/" + w.Week
		if seriesWeeks[key] {
			continue
		}
		seriesWeeks[key] = true
		src.IssueSeriesWeeks++
		r := row(start) // Zero-event files still establish observed coverage.
		r.IssueSource = evidenceWorkBalanceSourceGitHub
		for _, event := range w.Events {
			if !w.Covers(event.At) || !evidenceWindowHasTime(win, event.At) {
				continue
			}
			switch event.Action {
			case evidence.IssueActionOpened:
				r.Opened[event.Kind]++
				r.N++
			case evidence.IssueActionClosed:
				r.Closed++
				r.N++
			}
		}
	}
	// Audit-created issue numbers are the agent's own work even when the
	// hive's App account is the GitHub author.
	agentIssues := map[string]bool{}
	for _, e := range in.Entries {
		if e.Action != ghpkg.AuditActionAgentIssueCreated || e.Target == 0 {
			continue
		}
		if r, ok := auditEntryRepo(e); ok {
			agentIssues[ghpkg.IssueActivityKey(r, e.Target)] = true
		}
	}

	// Issues, GitHub truth: weeks fully inside the rollup's coverage.
	for key, a := range in.Rollup.Issues {
		if repo != "" && !strings.EqualFold(a.Repo, repo) {
			continue
		}
		kind := a.Kind
		if agentIssues[key] {
			kind = evidence.AuthorAgent
		}
		if kind == "" {
			kind = evidence.AuthorHuman
		}
		if evidenceWindowHasTime(win, a.CreatedAt) && githubWeek(a.CreatedAt) {
			r := row(a.CreatedAt)
			r.IssueSource = evidenceWorkBalanceSourceGitHub
			r.Opened[kind]++
			r.N++
		}
		if !a.ClosedAt.IsZero() && evidenceWindowHasTime(win, a.ClosedAt) && githubWeek(a.ClosedAt) {
			r := row(a.ClosedAt)
			r.IssueSource = evidenceWorkBalanceSourceGitHub
			r.Closed++
			r.N++
		}
	}
	// Issues, audit fallback: weeks the rollup does not cover.
	for _, e := range in.Entries {
		if !evidenceWorkBalanceActions[e.Action] {
			continue
		}
		t, ok := prThroughputTime(e)
		if !ok || !prThroughputEntryMatchesRepo(e, repo) || !evidenceWindowHasTime(win, t) || githubWeek(t) {
			continue
		}
		if r, ok := auditEntryRepo(e); ok && seriesWeeks[strings.ToLower(r)+"/"+evidenceWeekKey(t)] {
			continue
		}
		r := row(t)
		// A row may mix backfilled repositories with audit-only ones.
		r.IssueSource = evidenceWorkBalanceSourceAudit
		r.N++
		if kind, ok := evidenceWorkBalanceOpenerKind(e); ok {
			r.Opened[kind]++
			continue
		}
		r.Closed++
	}
	// PRs from folded evidence bundles.
	for _, p := range in.PRs {
		if p == nil || p.Bundle == nil {
			continue
		}
		if evidenceWindowHasTime(win, p.OpenedAt) {
			r := row(p.OpenedAt)
			kind := p.Author.Kind
			if kind == "" {
				kind = evidence.AuthorHuman
			}
			r.PRsOpened[kind]++
			r.N++
		}
		if p.Merge != nil && !p.Merge.At.IsZero() {
			if evidenceWindowHasTime(win, p.Merge.At) {
				r := row(p.Merge.At)
				r.PRsMerged++
				r.N++
			}
		} else if p.Closed {
			if at := evidencePRClosedAt(p); !at.IsZero() && evidenceWindowHasTime(win, at) {
				r := row(at)
				r.PRsClosed++
				r.N++
			}
		}
	}
	// Queue depth at week end: the latest governor sample in each week.
	type sample struct {
		at          time.Time
		issues, prs int
	}
	latest := map[string]sample{}
	for _, b := range in.Buckets {
		if b.Samples == 0 || len(b.QueueIssues) == 0 || len(b.QueuePRs) == 0 {
			continue
		}
		at := b.Last
		if at.IsZero() {
			at = b.Start
		}
		if !evidenceWindowHasTime(win, at) {
			continue
		}
		src.QueueSamples += b.Samples
		key := evidenceWeekKey(at)
		if cur, ok := latest[key]; ok && !at.After(cur.at) {
			continue
		}
		latest[key] = sample{at: at, issues: b.QueueIssues[len(b.QueueIssues)-1], prs: b.QueuePRs[len(b.QueuePRs)-1]}
	}
	for key, smp := range latest {
		if r := weeks[key]; r != nil {
			i, p := smp.issues, smp.prs
			r.QueueIssues, r.QueuePRs = &i, &p
		}
	}
	keys := make([]string, 0, len(weeks))
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]EvidenceWorkBalanceWeek, 0, len(keys))
	for _, k := range keys {
		r := weeks[k]
		if r.IssueSource == evidenceWorkBalanceSourceAudit {
			src.AuditOnlyWeeks++
		}
		out = append(out, *r)
	}
	return out, src
}

// evidencePRClosedAt is when a closed-without-merge PR was closed: the
// backfill's closed_at when recorded, else the record's updated_at.
func evidencePRClosedAt(p *evidence.PR) time.Time {
	if p.Backfill != nil && !p.Backfill.ClosedAt.IsZero() {
		return p.Backfill.ClosedAt
	}
	return p.UpdatedAt
}

func auditEntryRepo(e AuditEntry) (string, bool) {
	return collect.AuditEntryRepo(e)
}

// evidenceWorkBalanceEntries returns the audit entries the work-balance rows
// are built from, reading the on-disk throughput audit log when present and
// the in-memory ring otherwise, exactly like /api/pr-throughput.
func evidenceWorkBalanceEntries(audit *AuditLog) []AuditEntry {
	if audit == nil {
		audit = &AuditLog{}
	}
	var out []AuditEntry
	for _, e := range prThroughputEntries(audit) {
		if evidenceWorkBalanceActions[strings.TrimSpace(e.Action)] {
			out = append(out, e)
		}
	}
	return out
}

// evidenceWorkBalanceBuckets loads the governor's hive-state snapshots for
// the window; nil when Nous is not configured.
func (s *Server) evidenceWorkBalanceBuckets(win evidenceWindow) []hivestate.Bucket {
	if s == nil || s.deps == nil || s.deps.Nous == nil || s.deps.Nous.SnapshotDir == "" {
		return nil
	}
	since := win.From
	if !(win.Custom || win.Hours > 0) {
		since = time.Time{}
	}
	buckets, err := hivestate.Load(s.deps.Nous.SnapshotDir, since)
	if err != nil {
		return nil
	}
	return buckets
}
