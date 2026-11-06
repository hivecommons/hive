package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The report reads GitHub on the installation token: one issue listing per
// repo, then a comments read (and a PR read) per closed issue. It is owner
// only, bounded in time and size, and cached per window so repeated GETs do
// not re-scan.
const (
	closeOnMergeAuditDeadline  = 60 * time.Second
	closeOnMergeAuditMaxIssues = 500
	closeOnMergeAuditCacheTTL  = 10 * time.Minute
)

// closeOnMergeWrongClosesFunc reads the report from GitHub; tests swap it.
var closeOnMergeWrongClosesFunc = func(ctx context.Context, c *ghpkg.Client, opts ghpkg.CloseOnMergeAuditOptions) (ghpkg.CloseOnMergeAudit, error) {
	return c.CloseOnMergeWrongCloses(ctx, opts)
}

type closeOnMergeAuditEntry struct {
	at   time.Time
	resp map[string]interface{}
}

// closeOnMergeAuditCache holds one answer per `days` value. Its lock is held
// for the whole scan, so concurrent requests wait for the first and are then
// served from the cache instead of scanning again.
var closeOnMergeAuditCache = struct {
	sync.Mutex
	entries map[int]closeOnMergeAuditEntry
}{entries: map[int]closeOnMergeAuditEntry{}}

// handleCloseOnMergeWrongCloses serves GET /api/close-on-merge/wrong-closes?days=N
// (default 30, at most 90): the issues the hive closed with "Fixed by #N"
// where PR #N only referenced them or said they stay open, or where the issue
// was marked hive/verified-open or hive/partially-addressed and the PR did not
// close it (projectbluefin/utah-packages#490). Owner only and read-only:
// maintainers reopen what they agree with; the hive reopens nothing.
func (s *Server) handleCloseOnMergeWrongCloses(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 90 {
			jsonError(w, "days must be 1 to 90", http.StatusBadRequest)
			return
		}
		days = n
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	cache := &closeOnMergeAuditCache
	cache.Lock()
	defer cache.Unlock()
	if e, ok := cache.entries[days]; ok && time.Since(e.at) < closeOnMergeAuditCacheTTL {
		resp := map[string]interface{}{}
		for k, v := range e.resp {
			resp[k] = v
		}
		resp["cached_at"] = e.at.UTC().Format(time.RFC3339)
		jsonResponse(w, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), closeOnMergeAuditDeadline)
	defer cancel()
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	audit, err := closeOnMergeWrongClosesFunc(ctx, s.deps.GHClient, ghpkg.CloseOnMergeAuditOptions{Since: since, MaxIssues: closeOnMergeAuditMaxIssues})
	items := audit.Items
	if items == nil {
		items = []ghpkg.CloseOnMergeWrongClose{}
	}
	resp := map[string]interface{}{
		"since": since.UTC().Format(time.RFC3339), "days": days, "items": items,
		"inspected": audit.Inspected, "truncated": audit.Truncated, "complete": err == nil && !audit.Truncated,
	}
	if audit.Truncated {
		resp["note"] = "stopped after " + strconv.Itoa(closeOnMergeAuditMaxIssues) + " closed issues; ask for fewer days to cover the rest"
	}
	if err != nil {
		resp["error"] = err.Error()
	} else {
		// Only a scan that finished is reused; a failed one is retried.
		cache.entries[days] = closeOnMergeAuditEntry{at: time.Now(), resp: resp}
	}
	jsonResponse(w, resp)
}
