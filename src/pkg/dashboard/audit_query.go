package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/github"
)

// Audit log filters (GET /api/audit) and item history (GET /api/audit/item).
//
// The audit page used to answer only its newest auditMaxEntries rows of the
// in-memory ring (auditRingCap, emptied by a restart), so an operator could
// not ask what the hive did in one repository or on one PR once it scrolled
// out. A request that names repo, number, action or since now scans every
// retained audit file (the current log and its rotated, compressed backups:
// the same sources the activity collector reads) and filters BEFORE the
// auditMaxEntries cap, so the cap never hides a match that a narrower query
// would have shown. A hive with no audit files (a local run, tests) falls
// back to the ring. A request without those filters is unchanged.

const (
	// auditQueryMaxWindow bounds since: the audit files keep about this much
	// (auditMaxBackups rotations), and the merged ledger keeps 90 days.
	auditQueryMaxWindow = 90 * 24 * time.Hour
	// auditItemHistoryMaxEntries caps one item's history; an item with more
	// than this many audit rows is rare and reports truncated.
	auditItemHistoryMaxEntries = 500
)

// auditQuery is a parsed /api/audit filter. The zero value matches
// everything.
type auditQuery struct {
	Repo    string
	Number  int
	Actions []string // exact names; a trailing * matches a prefix
	Since   time.Time
}

// active reports whether the query asks for the full-log scan.
func (q auditQuery) active() bool {
	return q.Repo != "" || q.Number > 0 || len(q.Actions) > 0 || !q.Since.IsZero()
}

// parseAuditQuery reads repo, number, action and since from the request.
// since is RFC3339 or a duration back from now ("24h", "7d").
func parseAuditQuery(r *http.Request, now time.Time) (auditQuery, error) {
	v := r.URL.Query()
	q := auditQuery{Repo: strings.TrimSpace(v.Get("repo"))}
	if raw := strings.TrimSpace(v.Get("number")); raw != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(raw, "#"))
		if err != nil || n <= 0 {
			return auditQuery{}, fmt.Errorf("number must be a positive integer, got %q", raw)
		}
		q.Number = n
	}
	for _, a := range strings.Split(v.Get("action"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			q.Actions = append(q.Actions, a)
		}
	}
	if raw := strings.TrimSpace(v.Get("since")); raw != "" {
		since, err := parseAuditSince(raw, now)
		if err != nil {
			return auditQuery{}, err
		}
		q.Since = since
	}
	return q, nil
}

// parseAuditSince accepts RFC3339 or a positive duration back from now; a
// "d" suffix means days. It refuses a window wider than auditQueryMaxWindow.
func parseAuditSince(raw string, now time.Time) (time.Time, error) {
	var since time.Time
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		since = t.UTC()
	} else {
		d, derr := parseAuditDuration(raw)
		if derr != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("since must be RFC3339 or a duration like 24h or 7d, got %q", raw)
		}
		since = now.Add(-d).UTC()
	}
	if now.Sub(since) > auditQueryMaxWindow {
		return time.Time{}, fmt.Errorf("since must be within %d days", int(auditQueryMaxWindow/(24*time.Hour)))
	}
	return since, nil
}

func parseAuditDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

// auditNumberRe reads the number= pair entries written before the typed
// Target field existed carry in Detail (hivecommons/hive#9587).
var auditNumberRe = regexp.MustCompile(`(?:^|[,\s])number=#?(\d+)`)

// auditEntryNumber is the issue or PR number an entry names: the typed
// Target, else a number= pair in Detail.
func auditEntryNumber(e AuditEntry) int {
	if e.Target > 0 {
		return e.Target
	}
	if m := auditNumberRe.FindStringSubmatch(e.Detail); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// auditRepoMatches compares an entry's repository with the filter. Entries
// carry either owner/name or the bare name, so a side without an owner
// matches on the name alone.
func auditRepoMatches(entryRepo, want string) bool {
	entryRepo, want = strings.ToLower(strings.TrimSpace(entryRepo)), strings.ToLower(strings.TrimSpace(want))
	if entryRepo == "" || want == "" {
		return entryRepo == want
	}
	if entryRepo == want {
		return true
	}
	if !strings.Contains(entryRepo, "/") || !strings.Contains(want, "/") {
		return auditRepoName(entryRepo) == auditRepoName(want)
	}
	return false
}

func auditRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func auditActionMatches(action string, actions []string) bool {
	if len(actions) == 0 {
		return true
	}
	for _, a := range actions {
		if prefix, ok := strings.CutSuffix(a, "*"); ok {
			if strings.HasPrefix(action, prefix) {
				return true
			}
		} else if action == a {
			return true
		}
	}
	return false
}

// matches reports whether e passes every filter in q.
func (q auditQuery) matches(e AuditEntry) bool {
	if !q.Since.IsZero() {
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || ts.Before(q.Since) {
			return false
		}
	}
	if q.Repo != "" {
		repo, ok := collect.AuditEntryRepo(e)
		if !ok || !auditRepoMatches(repo, q.Repo) {
			return false
		}
	}
	if q.Number > 0 && auditEntryNumber(e) != q.Number {
		return false
	}
	return auditActionMatches(e.Action, q.Actions)
}

// auditScanFiltered returns every retained entry passing keep, newest first:
// the on-disk audit files when the hive has them, else the ring. scanned
// names the source ("files" or "ring").
func (s *Server) auditScanFiltered(since time.Time, keep func(AuditEntry) bool) (entries []AuditEntry, scanned string) {
	if s == nil || s.audit == nil {
		return nil, "none"
	}
	if s.audit.HasOnDiskLog("") {
		all := s.audit.OutputActionsSince(since, nil, "")
		for i := len(all) - 1; i >= 0; i-- {
			if keep(all[i]) {
				entries = append(entries, all[i])
			}
		}
		return entries, "files"
	}
	for _, e := range s.audit.Recent(0) {
		if keep(e) {
			entries = append(entries, e)
		}
	}
	return entries, "ring"
}

// stampAuditDisplayNames attaches display names for opaque OIDC actor keys,
// as handleAuditLog does for the default page.
func (s *Server) stampAuditDisplayNames(entries []AuditEntry) {
	for i := range entries {
		if dn := s.authorizedDisplayName(entries[i].User); dn != "" && dn != entries[i].User {
			entries[i].UserName = dn
		}
	}
}

// filteredAuditEntries answers a filtered /api/audit request: every
// retained entry passing q, contributor and sensitive, newest first, capped
// at auditMaxEntries AFTER filtering. matched is the count before the cap.
func (s *Server) filteredAuditEntries(q auditQuery, contributor string, sensitive bool, now time.Time) (entries []AuditEntry, matched int, scanned string) {
	since := q.Since
	if sensitive && since.IsZero() {
		// ?sensitive=1 alone means the 24h sensitive window; keep that
		// window when it is combined with the other filters.
		since = now.Add(-auditSummaryHours * time.Hour)
	}
	keep := func(e AuditEntry) bool {
		if !q.matches(e) {
			return false
		}
		if contributor != "" && !strings.EqualFold(e.User, contributor) {
			return false
		}
		return !sensitive || auditEntryIsSensitive(e)
	}
	entries, scanned = s.auditScanFiltered(since, keep)
	matched = len(entries)
	if len(entries) > auditMaxEntries {
		entries = entries[:auditMaxEntries]
	}
	auditMarkSensitiveEntries(entries)
	s.stampAuditDisplayNames(entries)
	return entries, matched, scanned
}

// AuditItemHistory is GET /api/audit/item: everything the hive knows about
// one issue or PR, open or closed.
type AuditItemHistory struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// State is "open" (on the Activity monitor), "merged" (in the merged
	// ledger), "closed" (left the monitor closed within the last 30
	// minutes) or "unknown" (none of those: closed earlier, never tracked,
	// or the monitor has not built yet).
	State string `json:"state"`
	// Entries are the item's audit rows, newest first: every retained row
	// naming this repo and number, plus the rows of LinkedIssue when the PR
	// was nested under an issue (an agent's claim is recorded on the issue).
	// Each row's target says which item it is about.
	Entries   []AuditEntry `json:"entries"`
	Truncated bool         `json:"truncated,omitempty"`
	// Scanned is where the rows came from: "files" (every retained audit
	// file) or "ring" (the in-memory ring, on a hive without audit files).
	Scanned string `json:"scanned"`
	// LinkedIssue is the issue the PR was nested under, from the ledger or
	// the monitor row; 0 when none is known.
	LinkedIssue int `json:"linked_issue,omitempty"`
	// Merged is the merged-ledger entry when the PR merged in the last 90
	// days: merged_at, merged_by (the hive or a login), lane, issue.
	Merged *MergedLedgerEntry `json:"merged,omitempty"`
	// Tracker is the item's Activity monitor row while it is still open.
	Tracker *StageTrackerItem `json:"tracker,omitempty"`
	// Closed is the monitor's recently-closed record (last 30 minutes).
	Closed *StageTrackerClosed `json:"closed,omitempty"`
}

// handleAuditItemHistory serves GET /api/audit/item?repo=owner/name&number=N.
func (s *Server) handleAuditItemHistory(w http.ResponseWriter, r *http.Request) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		http.Error(w, "insufficient access", http.StatusForbidden)
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	rawNumber := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("number")), "#")
	number, err := strconv.Atoi(rawNumber)
	if repo == "" || err != nil || number <= 0 {
		jsonError(w, "repo (owner/name) and a positive number are required", http.StatusBadRequest)
		return
	}
	jsonResponse(w, s.auditItemHistory(repo, number))
}

// auditItemHistory assembles one item's history from the audit files, the
// merged ledger and the cached Activity monitor body.
func (s *Server) auditItemHistory(repo string, number int) AuditItemHistory {
	if !strings.Contains(repo, "/") {
		repo = s.mergedRepoQualifier()(repo)
	}
	h := AuditItemHistory{Repo: repo, Number: number, State: "unknown"}
	tracker, closed := s.stageTrackerCachedItem(repo, number)
	if tracker != nil {
		h.Tracker = tracker
		h.State = "open"
		if tracker.Kind == "pr" && tracker.NestedUnder > 0 {
			h.LinkedIssue = tracker.NestedUnder
		}
	}
	if e, ok := s.mergedLedger().Entry(repo, number); ok {
		h.Merged = &e
		h.State = "merged"
		if e.Issue > 0 {
			h.LinkedIssue = e.Issue
		}
	}
	if closed != nil && h.Merged == nil {
		h.Closed = closed
		if h.Tracker == nil {
			h.State = "closed"
		}
	}
	keep := func(e AuditEntry) bool {
		repoOf, ok := collect.AuditEntryRepo(e)
		if !ok || !auditRepoMatches(repoOf, repo) {
			return false
		}
		n := auditEntryNumber(e)
		return n == number || (h.LinkedIssue > 0 && n == h.LinkedIssue)
	}
	entries, scanned := s.auditScanFiltered(time.Time{}, keep)
	if len(entries) > auditItemHistoryMaxEntries {
		entries = entries[:auditItemHistoryMaxEntries]
		h.Truncated = true
	}
	auditMarkSensitiveEntries(entries)
	s.stampAuditDisplayNames(entries)
	if entries == nil {
		entries = []AuditEntry{}
	}
	if h.State == "unknown" && auditItemClosedByHive(entries, number) {
		// Closed earlier than the monitor's 30-minute record, by the hive.
		h.State = "closed"
	}
	h.Entries, h.Scanned = entries, scanned
	return h
}

// auditItemClosedByHive reports whether entries hold the hive's own close of
// number (pr_closed or agent_issue_closed).
func auditItemClosedByHive(entries []AuditEntry, number int) bool {
	for _, e := range entries {
		if auditEntryNumber(e) == number && (e.Action == github.AuditActionPRClosed || e.Action == github.AuditActionIssueClosed) {
			return true
		}
	}
	return false
}

// stageTrackerCachedItem finds repo#number (issue or PR) in the cached
// Activity monitor body: its open row, or its recently-closed record.
func (s *Server) stageTrackerCachedItem(repo string, number int) (*StageTrackerItem, *StageTrackerClosed) {
	s.stageTrackerCacheMu.Lock()
	body := s.stageTrackerCache.body
	s.stageTrackerCacheMu.Unlock()
	if len(body) == 0 {
		return nil, nil
	}
	var resp struct {
		Items          []StageTrackerItem   `json:"items"`
		RecentlyClosed []StageTrackerClosed `json:"recently_closed"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return nil, nil
	}
	for i := range resp.Items {
		if resp.Items[i].Number == number && auditRepoMatches(resp.Items[i].Repo, repo) {
			return &resp.Items[i], nil
		}
	}
	for i := range resp.RecentlyClosed {
		if resp.RecentlyClosed[i].Number == number && auditRepoMatches(resp.RecentlyClosed[i].Repo, repo) {
			return nil, &resp.RecentlyClosed[i]
		}
	}
	return nil, nil
}
