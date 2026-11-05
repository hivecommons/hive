package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// closeOnMergeWrongClosesFunc reads the report from GitHub; tests swap it.
var closeOnMergeWrongClosesFunc = func(ctx context.Context, c *ghpkg.Client, since time.Time) ([]ghpkg.CloseOnMergeWrongClose, error) {
	return c.CloseOnMergeWrongCloses(ctx, since)
}

// handleCloseOnMergeWrongCloses serves GET /api/close-on-merge/wrong-closes?days=N
// (default 30, at most 90): the issues the hive closed with "Fixed by #N"
// where PR #N only referenced them or said they stay open, or where the issue
// was marked hive/verified-open or hive/partially-addressed and the PR did not
// close it (projectbluefin/utah-packages#490). Read-only: maintainers reopen
// what they agree with; the hive reopens nothing.
func (s *Server) handleCloseOnMergeWrongCloses(w http.ResponseWriter, r *http.Request) {
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
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	items, err := closeOnMergeWrongClosesFunc(r.Context(), s.deps.GHClient, since)
	if items == nil {
		items = []ghpkg.CloseOnMergeWrongClose{}
	}
	resp := map[string]interface{}{"since": since.UTC().Format(time.RFC3339), "days": days, "items": items, "complete": err == nil}
	if err != nil {
		resp["error"] = err.Error()
	}
	jsonResponse(w, resp)
}
