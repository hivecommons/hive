package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/governor"
)

const defaultHistoryPoints = 512
const maxHistoryPoints = 10000

// History endpoints keep their existing wire shapes. since is an exclusive
// Unix-millisecond cutoff; max_points bounds the response, not stored history.
// Full-resolution consumers can request up to maxHistoryPoints explicitly.
type historyQuery struct {
	since int64
	limit int
}

func parseHistoryQuery(w http.ResponseWriter, r *http.Request) (historyQuery, bool) {
	q := historyQuery{limit: defaultHistoryPoints}
	if raw := r.URL.Query().Get("since"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "since must be a non-negative Unix timestamp in milliseconds", http.StatusBadRequest)
			return q, false
		}
		q.since = n
	}
	if raw := r.URL.Query().Get("max_points"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxHistoryPoints {
			http.Error(w, "max_points must be between 1 and 10000", http.StatusBadRequest)
			return q, false
		}
		q.limit = n
	}
	return q, true
}

func historySince[T any](entries []T, since int64, timestamp func(T) int64) []T {
	if since == 0 {
		return entries
	}
	out := make([]T, 0, len(entries))
	for _, entry := range entries {
		if timestamp(entry) > since {
			out = append(out, entry)
		}
	}
	return out
}

// Sample across the entire retained range, including both endpoints. Selecting
// original rows preserves correlated fields (agents, models, repo counts) and
// never manufactures fractional counts or rewrites the persisted history.
func sampleHistory[T any](entries []T, limit int) []T {
	if len(entries) <= limit {
		return entries
	}
	if limit == 1 {
		return entries[len(entries)-1:]
	}
	out := make([]T, limit)
	for i := range out {
		out[i] = entries[i*(len(entries)-1)/(limit-1)]
	}
	return out
}

func historyResponse[T any](w http.ResponseWriter, r *http.Request, entries []T, timestamp func(T) int64) {
	q, ok := parseHistoryQuery(w, r)
	if !ok {
		return
	}
	entries = historySince(entries, q.since, timestamp)
	w.Header().Set("X-Hive-History-Total", strconv.Itoa(len(entries)))
	entries = sampleHistory(entries, q.limit)
	w.Header().Set("X-Hive-History-Returned", strconv.Itoa(len(entries)))
	conditionalJSONResponse(w, r, entries)
}

func evalHistoryResponse(w http.ResponseWriter, r *http.Request, history any) {
	switch entries := history.(type) {
	case []governor.EvalSnapshot:
		historyResponse(w, r, entries, func(e governor.EvalSnapshot) int64 { return e.Timestamp })
	case []json.RawMessage:
		// Seeded history has extra legacy sparkline fields. Keep those bytes
		// intact and decode only the timestamp used for the cutoff.
		historyResponse(w, r, entries, func(e json.RawMessage) int64 {
			var stamp struct {
				Timestamp int64 `json:"t"`
			}
			_ = json.Unmarshal(e, &stamp)
			return stamp.Timestamp
		})
	}
}

// conditionalJSONBytes uses a weak validator because the same representation
// can be served as identity or gzip. Private revalidation prevents shared
// caches from retaining authenticated dashboard data. Hash the selected body,
// so fields/omit, since and sampling variants cannot validate each other.
func conditionalJSONBytes(w http.ResponseWriter, r *http.Request, data any, validator ...any) ([]byte, bool) {
	body, err := json.Marshal(data)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return nil, false
	}
	key := body
	if len(validator) > 0 {
		key, err = json.Marshal(validator[0])
		if err != nil {
			http.Error(w, "failed to encode validator", http.StatusInternalServerError)
			return nil, false
		}
	}
	etag := "W/" + webstatic.ETagFor(key)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	for _, line := range r.Header.Values("If-None-Match") {
		for _, candidate := range strings.Split(line, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(etag, "W/") {
				w.WriteHeader(http.StatusNotModified)
				return nil, false
			}
		}
	}
	return append(body, '\n'), true
}

func conditionalJSONResponse(w http.ResponseWriter, r *http.Request, data any, validator ...any) {
	if body, ok := conditionalJSONBytes(w, r, data, validator...); ok {
		_, _ = w.Write(body)
	}
}

// Launch rows are records, not chart samples: keep the newest launches and
// highest-spend refs in their existing order. Totals always cover the whole
// accounting window, with explicit counts disclosing omitted detail rows.
func launchCostResponse(w http.ResponseWriter, r *http.Request, resp collect.LaunchCostResponse, q historyQuery) {
	resp.ByLaunch = historySince(resp.ByLaunch, q.since, func(e collect.LaunchCostEntry) int64 {
		t, _ := time.Parse(time.RFC3339, e.LaunchedAt)
		return t.UnixMilli()
	})
	resp.ByRef = historySince(resp.ByRef, q.since, func(e collect.RefCostEntry) int64 {
		t, _ := time.Parse(time.RFC3339, e.LastLaunchedAt)
		return t.UnixMilli()
	})
	launches, refs := len(resp.ByLaunch), len(resp.ByRef)
	resp.ByLaunch = resp.ByLaunch[:min(launches, q.limit)]
	resp.ByRef = resp.ByRef[:min(refs, q.limit)]
	// Collection time alone must not invalidate otherwise identical data.
	// It remains available as response metadata, including on a 304.
	w.Header().Set("X-Hive-Collected-At", resp.CollectedAt.Format(time.RFC3339Nano))
	payload := struct {
		collect.LaunchCostResponse
		LaunchesTotal int  `json:"by_launch_total"`
		RefsTotal     int  `json:"by_ref_total"`
		Truncated     bool `json:"truncated"`
	}{resp, launches, refs, launches > len(resp.ByLaunch) || refs > len(resp.ByRef)}
	validator := payload
	validator.CollectedAt = time.Time{}
	conditionalJSONResponse(w, r, payload, validator)
}
