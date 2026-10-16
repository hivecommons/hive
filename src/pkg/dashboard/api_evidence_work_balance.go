package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Panel 6 (#12366, RFC #12062): work balance. Weekly issues opened by actor
// kind against issues closed, over the Throughput window and repo, built from
// the same audit entries as /api/pr-throughput. Read-only reporting; nothing
// here is consulted by merge eligibility or auto-merge.

// evidenceWorkBalanceActions are the audit actions the work-balance rows are
// built from. The hive audits only the issue writes it mediates, so an issue
// a human or a third-party bot opens directly on GitHub is not observed; that
// gap is disclosed through EvidenceMissing.IssuesHumanOpensUnobserved rather
// than reported as zero human activity.
var evidenceWorkBalanceActions = map[string]bool{
	ghpkg.AuditActionAgentIssueCreated: true,
	ghpkg.AuditActionHiveIssueCreated:  true,
	ghpkg.AuditActionIssueClosed:       true,
}

// EvidenceWorkBalanceWeek is one ISO week of issue activity. Opened is split
// by actor kind (human / agent / bot); Closed is every audited issue close in
// the week regardless of who closed it. N is Opened plus Closed. Throttled
// is reserved for governor throttle shading and is always false until a hive
// reports issue-creation throttle windows (EvidenceMissing.ThrottleWindows).
type EvidenceWorkBalanceWeek struct {
	Week      string         `json:"week"`
	N         int            `json:"n"`
	Opened    map[string]int `json:"opened"`
	Closed    int            `json:"closed"`
	Throttled bool           `json:"throttled"`
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

// buildEvidenceWorkBalance aggregates audited issue opens and closes per ISO
// week for the window: a trailing hours window, a custom from/to range, or
// (Hours 0, not custom) every retained audit entry; the audit log's own
// retention is the real horizon.
func buildEvidenceWorkBalance(entries []AuditEntry, win evidenceWindow, repo string) []EvidenceWorkBalanceWeek {
	bounded := win.Custom || win.Hours > 0
	weeks := map[string]*EvidenceWorkBalanceWeek{}
	for _, e := range entries {
		if !evidenceWorkBalanceActions[e.Action] {
			continue
		}
		t, ok := prThroughputTime(e)
		if !ok || !prThroughputEntryMatchesRepo(e, repo) {
			continue
		}
		if bounded && t.Before(win.From) {
			continue
		}
		if !win.To.IsZero() && (t.After(win.To) || (win.Custom && t.Equal(win.To))) {
			continue
		}
		y, w := t.UTC().ISOWeek()
		key := fmt.Sprintf("%04d-W%02d", y, w)
		row := weeks[key]
		if row == nil {
			row = &EvidenceWorkBalanceWeek{Week: key, Opened: map[string]int{}}
			for _, k := range []string{evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot} {
				row.Opened[k] = 0
			}
			weeks[key] = row
		}
		row.N++
		if kind, ok := evidenceWorkBalanceOpenerKind(e); ok {
			row.Opened[kind]++
			continue
		}
		row.Closed++
	}
	keys := make([]string, 0, len(weeks))
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]EvidenceWorkBalanceWeek, 0, len(keys))
	for _, k := range keys {
		out = append(out, *weeks[k])
	}
	return out
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
