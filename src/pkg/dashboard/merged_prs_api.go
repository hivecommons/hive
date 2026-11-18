package dashboard

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GET /api/merged-prs reads the merged ledger over a window, with filters:
// the Activity monitor's Merged list and the admin MCP's merged_prs_read
// both read it. Before it, the only way to the ledger was the stage
// tracker's recently_merged (30 minutes) and its recent-20 summary, so "which
// PRs did the hive merge on its own in the last day" had no answer.
//
// Query parameters, all optional:
//
//	since           RFC3339 instant or a duration back from now ("24h",
//	                "90m", "7d"); default 24h, at most the ledger's
//	                retention (90 days)
//	repo            owner/name (or the bare name)
//	merged_by       hive | human (any login other than the hive) | a login
//	lane            the tracker lane the PR merged from
//	author          only PRs by this login
//	exclude_author  comma-separated logins whose PRs are left out
//	approved        human | none — the human approvals the hive recorded
//	limit, offset   paging (default 50, at most 500)
//
// counts describe the window after the repo and lane filters, before the
// merger, author and approval filters, so by_hive and by_human stay
// comparable whatever else is asked. An entry whose author or approvals
// were not recorded (every entry from before those fields existed) is
// left out of an author or approval filter and counted in
// author_unknown / approvals_unknown instead of being guessed.

const (
	mergedPRsDefaultWindow = 24 * time.Hour
	mergedPRsDefaultLimit  = 50
	mergedPRsMaxLimit      = 500
)

// MergedPRsQuery is a parsed /api/merged-prs request.
type MergedPRsQuery struct {
	Since          time.Time
	Repo           string
	MergedBy       string
	Lane           string
	Author         string
	ExcludeAuthors []string
	Approved       string
	Limit          int
	Offset         int
}

// MergedPRsCounts summarises the window (see the endpoint comment).
type MergedPRsCounts struct {
	Total   int `json:"total"`
	ByHive  int `json:"by_hive"`
	ByHuman int `json:"by_human"`
	Unknown int `json:"unknown"`
}

// MergedPRsResponse is the GET /api/merged-prs payload.
type MergedPRsResponse struct {
	Since   time.Time           `json:"since"`
	Until   time.Time           `json:"until"`
	Counts  MergedPRsCounts     `json:"counts"`
	Matched int                 `json:"matched"`
	Entries []MergedLedgerEntry `json:"entries"`
	// NextOffset is the offset of the next page, 0 when this is the last.
	NextOffset int `json:"next_offset,omitempty"`
	// AuthorUnknown / ApprovalsUnknown count entries an author or approval
	// filter left out because the ledger never recorded that field.
	AuthorUnknown    int `json:"author_unknown,omitempty"`
	ApprovalsUnknown int `json:"approvals_unknown,omitempty"`
	// RetentionDays is how far back the ledger lists entries.
	RetentionDays int `json:"retention_days"`
}

// parseMergedPRsSince reads since: RFC3339, a Go duration, or "<n>d".
func parseMergedPRsSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now.Add(-mergedPRsDefaultWindow), nil
	}
	var since time.Time
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		since = t.UTC()
	} else {
		d, err := parseMergedPRsDuration(raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("since must be an RFC3339 time or a duration such as 24h or 7d, got %q", raw)
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("since duration must be positive, got %q", raw)
		}
		since = now.Add(-d)
	}
	if since.After(now) {
		return time.Time{}, fmt.Errorf("since must not be in the future")
	}
	if now.Sub(since) > mergedLedgerRetention {
		return time.Time{}, fmt.Errorf("since must be within the ledger's %d days of retention", int(mergedLedgerRetention/(24*time.Hour)))
	}
	return since, nil
}

func parseMergedPRsDuration(raw string) (time.Duration, error) {
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return 0, err
		}
		// Past the ledger's retention the window is refused anyway; cap n
		// before multiplying, since a large day count overflows int64 and
		// wraps to a short window that would pass the checks.
		if maxDays := int(mergedLedgerRetention / (24 * time.Hour)); n > maxDays {
			return mergedLedgerRetention + 24*time.Hour, nil
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

// parseMergedPRsQuery reads the request's parameters.
func parseMergedPRsQuery(r *http.Request, now time.Time) (MergedPRsQuery, error) {
	v := r.URL.Query()
	since, err := parseMergedPRsSince(v.Get("since"), now)
	if err != nil {
		return MergedPRsQuery{}, err
	}
	q := MergedPRsQuery{
		Since:    since,
		Repo:     strings.TrimSpace(v.Get("repo")),
		MergedBy: strings.TrimPrefix(strings.TrimSpace(v.Get("merged_by")), "@"),
		Lane:     strings.TrimSpace(v.Get("lane")),
		Author:   strings.TrimPrefix(strings.TrimSpace(v.Get("author")), "@"),
		Approved: strings.ToLower(strings.TrimSpace(v.Get("approved"))),
		Limit:    mergedPRsDefaultLimit,
	}
	for _, login := range strings.Split(v.Get("exclude_author"), ",") {
		if login = strings.TrimPrefix(strings.TrimSpace(login), "@"); login != "" {
			q.ExcludeAuthors = append(q.ExcludeAuthors, login)
		}
	}
	switch q.Approved {
	case "", "human", "none":
	default:
		return MergedPRsQuery{}, fmt.Errorf("approved must be human or none, got %q", q.Approved)
	}
	if raw := strings.TrimSpace(v.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > mergedPRsMaxLimit {
			return MergedPRsQuery{}, fmt.Errorf("limit must be 1-%d", mergedPRsMaxLimit)
		}
		q.Limit = n
	}
	if raw := strings.TrimSpace(v.Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return MergedPRsQuery{}, fmt.Errorf("offset must be 0 or more")
		}
		q.Offset = n
	}
	return q, nil
}

// mergedRepoMatches matches owner/name, or a bare name against either.
func mergedRepoMatches(entryRepo, want string) bool {
	if want == "" {
		return true
	}
	if strings.EqualFold(entryRepo, want) {
		return true
	}
	short := entryRepo[strings.LastIndex(entryRepo, "/")+1:]
	if !strings.Contains(want, "/") && strings.EqualFold(short, want) {
		return true
	}
	return false
}

// mergedByMatches applies the merged_by filter.
func mergedByMatches(by, want string) bool {
	switch strings.ToLower(want) {
	case "":
		return true
	case MergedByHive:
		return by == MergedByHive
	case "human":
		return by != "" && by != MergedByHive
	}
	return strings.EqualFold(by, want)
}

// Query answers one /api/merged-prs request at now.
func (l *MergedLedger) Query(q MergedPRsQuery, now time.Time) MergedPRsResponse {
	out := MergedPRsResponse{Since: q.Since, Until: now, Entries: []MergedLedgerEntry{}, RetentionDays: int(mergedLedgerRetention / (24 * time.Hour))}
	var matched []MergedLedgerEntry
	for _, e := range l.Since(q.Since, now) {
		if !mergedRepoMatches(e.Repo, q.Repo) || (q.Lane != "" && !strings.EqualFold(e.Lane, q.Lane)) {
			continue
		}
		out.Counts.Total++
		switch {
		case e.MergedBy == MergedByHive:
			out.Counts.ByHive++
		case e.MergedBy == "":
			out.Counts.Unknown++
		default:
			out.Counts.ByHuman++
		}
		if !mergedByMatches(e.MergedBy, q.MergedBy) {
			continue
		}
		if q.Author != "" || len(q.ExcludeAuthors) > 0 {
			if e.Author == "" {
				out.AuthorUnknown++
				continue
			}
			if q.Author != "" && !strings.EqualFold(e.Author, q.Author) {
				continue
			}
			excluded := false
			for _, login := range q.ExcludeAuthors {
				if strings.EqualFold(e.Author, login) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}
		if q.Approved != "" {
			if !e.ApprovalsKnown {
				out.ApprovalsUnknown++
				continue
			}
			if (q.Approved == "human") != (len(e.ApprovedBy) > 0) {
				continue
			}
		}
		matched = append(matched, e)
	}
	out.Matched = len(matched)
	if q.Offset < len(matched) {
		end := min(q.Offset+q.Limit, len(matched))
		out.Entries = append(out.Entries, matched[q.Offset:end]...)
		if end < len(matched) {
			out.NextOffset = end
		}
	}
	return out
}

// registerMergedPRsRoute registers GET /api/merged-prs.
func (s *Server) registerMergedPRsRoute() {
	s.mux.HandleFunc("GET /api/merged-prs", s.handleMergedPRs)
}

// handleMergedPRs serves GET /api/merged-prs.
func (s *Server) handleMergedPRs(w http.ResponseWriter, r *http.Request) {
	now := mergedLedgerNow().UTC()
	q, err := parseMergedPRsQuery(r, now)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, s.mergedLedger().Query(q, now))
}
