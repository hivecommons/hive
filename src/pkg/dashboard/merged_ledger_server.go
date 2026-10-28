package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// mergedLedger returns the server's merged ledger, building it on first
// use: the file under /data when that volume exists (memory only
// otherwise), seeded from the audit history on a first boot, and wired to
// the audit log so every accepted pr_merged entry lands in it.
func (s *Server) mergedLedger() *MergedLedger {
	if s == nil {
		return nil
	}
	s.mergedOnce.Do(func() {
		path := mergedLedgerPath
		if _, err := os.Stat(filepath.Dir(path)); err != nil {
			path = ""
		}
		l := newMergedLedger(path)
		l.onNew = func(MergedLedgerEntry) { s.invalidateStageTrackerCache() }
		s.merged = l
		if s.audit != nil {
			if !l.Seeded() {
				s.seedMergedLedger(l, time.Now())
			}
			s.audit.SetPRMergedHook(s.recordMergedAudit)
		}
	})
	return s.merged
}

// seedMergedLedger folds the existing merge history in once: every
// pr_merged line in the audit files (plus the in-memory ring, which is all
// a hive without /data has), with the durable PR throughput counter's
// merged total as the all-time floor.
func (s *Server) seedMergedLedger(l *MergedLedger, now time.Time) {
	qualify := s.mergedRepoQualifier()
	seen := map[string]bool{}
	var entries []MergedLedgerEntry
	add := func(es []AuditEntry) {
		for _, e := range es {
			entry, ok := mergedEntryFromAudit(e, qualify)
			if !ok || seen[entry.key()] {
				continue
			}
			seen[entry.key()] = true
			entries = append(entries, s.enrichMergedEntry(entry))
		}
	}
	add(s.audit.ActionsWithPrefixSince(time.Time{}, github.AuditActionPRMerged, ""))
	add(s.audit.RecentWithPrefixSince(time.Time{}, github.AuditActionPRMerged))
	l.Seed(entries, s.audit.PRThroughputCounters().Merged, now)
	if s.logger != nil {
		s.logger.Info("merged ledger seeded from audit history", "entries", len(entries), "all_time", l.Summary(now, nil).AllTime)
	}
}

// mergedRepoQualifier maps the short repo names audit entries carry onto
// owner/name, from the status snapshot's repos and the configured org.
func (s *Server) mergedRepoQualifier() func(string) string {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	byStatus := stageTrackerRepoQualifier(status)
	org := ""
	if s.deps != nil && s.deps.Config != nil {
		org = s.deps.Config.Project.Org
	}
	return func(repo string) string {
		q := byStatus(repo)
		if !strings.Contains(q, "/") && org != "" {
			return org + "/" + q
		}
		return q
	}
}

// recordMergedAudit is the audit log's pr_merged observer.
func (s *Server) recordMergedAudit(e AuditEntry) {
	entry, ok := mergedEntryFromAudit(e, s.mergedRepoQualifier())
	if !ok {
		return
	}
	s.mergedLedger().Record(s.enrichMergedEntry(entry), time.Now())
}

// recordMergedSnapshots records every merged PR among webhook / sweep
// snapshots; it is what makes a human's merge show up within seconds
// rather than at the poller's next settle scan.
func (s *Server) recordMergedSnapshots(snaps []github.WebhookItemSnapshot) {
	if s == nil || len(snaps) == 0 {
		return
	}
	now := time.Now()
	qualify := s.mergedRepoQualifier()
	for _, snap := range snaps {
		if !snap.IsPR || !snap.Merged || snap.Number <= 0 || snap.Repo == "" {
			continue
		}
		entry := MergedLedgerEntry{Repo: qualify(snap.Repo), Number: snap.Number, Title: snap.Title, URL: snap.URL, MergedAt: snap.MergedAt}
		if entry.MergedAt.IsZero() {
			entry.MergedAt = snap.UpdatedAt
		}
		switch {
		case snap.MergedByHive:
			entry.MergedBy = MergedByHive
		default:
			entry.MergedBy = strings.TrimSpace(snap.MergedBy)
		}
		s.mergedLedger().Record(s.enrichMergedEntry(entry), now)
	}
}

// enrichMergedEntry fills the title, URL, lane and issue from what the
// tracker last knew about the PR (the cached /api/stage-tracker body, then
// the status snapshot). Best effort: a PR the tracker never drew keeps
// whatever the source knew.
func (s *Server) enrichMergedEntry(e MergedLedgerEntry) MergedLedgerEntry {
	if s == nil || (e.Title != "" && e.Lane != "" && e.Issue != 0 && e.URL != "") {
		return e
	}
	if it, ok := s.stageTrackerCachedPR(e.Repo, e.Number); ok {
		if e.Title == "" {
			e.Title = it.Title
		}
		if e.URL == "" {
			e.URL = it.URL
		}
		if e.Lane == "" {
			e.Lane = it.Lane
		}
		if e.Issue == 0 {
			e.Issue = it.NestedUnder
		}
	}
	if e.Title == "" || e.URL == "" {
		s.statusMu.RLock()
		status := s.status
		s.statusMu.RUnlock()
		if status != nil {
			for _, repo := range status.Repos {
				if !strings.EqualFold(overviewRepoName(repo), e.Repo) && !strings.EqualFold(repo.Name, e.Repo) {
					continue
				}
				for _, it := range repoOpenItems(repo) {
					if it.Kind != "pr" || it.Number != e.Number {
						continue
					}
					title, url := it.PR.pr.Title, it.PR.pr.URL
					if it.Sidelined {
						title, url = it.WorkPR.Title, it.WorkPR.URL
					}
					if e.Title == "" {
						e.Title = title
					}
					if e.URL == "" {
						e.URL = url
					}
				}
			}
		}
	}
	return e
}

// stageTrackerCachedPR finds a PR row in the last tracker body served.
// The body is decoded on demand: merges are rare next to tracker polls,
// and the response struct is not retained.
func (s *Server) stageTrackerCachedPR(repo string, number int) (StageTrackerItem, bool) {
	s.stageTrackerCacheMu.Lock()
	body := s.stageTrackerCache.body
	s.stageTrackerCacheMu.Unlock()
	if len(body) == 0 {
		return StageTrackerItem{}, false
	}
	var resp struct {
		Items []StageTrackerItem `json:"items"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return StageTrackerItem{}, false
	}
	for _, it := range resp.Items {
		if it.Kind == "pr" && it.Number == number && strings.EqualFold(it.Repo, repo) {
			return it, true
		}
	}
	return StageTrackerItem{}, false
}

// mergedSummaryNow is the API's merged block: "today" in the spoke's
// configured zone (the process TZ; UTC when unset).
func (s *Server) mergedSummaryNow(now time.Time) StageTrackerMergedSummary {
	return s.mergedLedger().Summary(now, time.Local)
}
