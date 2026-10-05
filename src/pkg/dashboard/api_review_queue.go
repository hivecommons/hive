package dashboard

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const (
	// reviewQueueDefaultLimit is the page size when ?limit is absent.
	reviewQueueDefaultLimit = 50
	// reviewQueueMaxLimit caps one page so a client cannot ask for an
	// unbounded response; larger queues are walked with ?offset.
	reviewQueueMaxLimit = 200
)

type reviewQueueHistoryStore struct {
	mu      sync.Mutex
	entries []ReviewQueueHistoryEntry
}

var reviewQueueHistoryByServer sync.Map

// ReviewQueueHistoryEntry is one priority-split review queue depth sample.
type ReviewQueueHistoryEntry struct {
	Timestamp int64 `json:"t"`
	Total     int   `json:"total"`
	High      int   `json:"high"`
	Normal    int   `json:"normal"`
	Low       int   `json:"low"`
}

// reviewQueueResponse is one page of the PR review queue.
type reviewQueueResponse struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	SnapshotAt  time.Time                 `json:"snapshot_at,omitzero"`
	Total       int                       `json:"total"`
	Limit       int                       `json:"limit"`
	Offset      int                       `json:"offset"`
	HasMore     bool                      `json:"has_more"`
	History     []ReviewQueueHistoryEntry `json:"history"`
	Items       []ghpkg.ReviewQueueEntry  `json:"items"`
}

// handleReviewQueue serves GET /api/review/queue?limit=N&offset=M: every open
// PR in the governed repos, agent- or contributor-authored, in one ranked
// order (class, then confidence band, then CI, then age) with the reasons for
// each position (hivecommons/hive#9590). Paged like /api/v1/queue (#6537).
//
// It reads only the last enumeration snapshot and the review verdict
// artifact, so a request never calls GitHub; the rank is recomputed from both
// on each request so a verdict recorded since the last eval cycle is
// reflected at once.
func (s *Server) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := reviewQueuePaging(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	artifact, err := review.LoadArtifact("")
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			jsonError(w, "review verdicts unreadable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		artifact = review.Artifact{}
	}
	opts := ghpkg.ReviewQueueOptions{
		Verdicts:     artifact,
		ChangedPaths: ghpkg.CachedPRChangedPaths,
		Now:          time.Now(),
	}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		opts.Org = s.deps.Config.Project.Org
		opts.AIAuthor = s.deps.Config.EffectiveAIAuthor()
	}
	actionable := s.lastActionableForPRModels()
	queue := ghpkg.ReviewQueueFromActionable(actionable, opts)
	history := s.recordReviewQueueHistory(queue, opts.Now)

	resp := reviewQueueResponse{
		GeneratedAt: opts.Now.UTC(),
		SnapshotAt:  actionable.GeneratedAt,
		Total:       len(queue),
		Limit:       limit,
		Offset:      offset,
		History:     history,
		Items:       []ghpkg.ReviewQueueEntry{},
	}
	if offset < len(queue) {
		end := offset + limit
		if end > len(queue) {
			end = len(queue)
		}
		resp.Items = queue[offset:end]
	}
	resp.HasMore = offset+len(resp.Items) < len(queue)
	jsonResponse(w, resp)
}

type errReviewQueuePaging string

func (e errReviewQueuePaging) Error() string { return string(e) }

// reviewQueuePaging reads ?limit and ?offset with the same rules as
// handleAPIv1Queue, plus an upper bound on limit.
func reviewQueuePaging(r *http.Request) (limit, offset int, err error) {
	limit = reviewQueueDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n <= 0 || n > reviewQueueMaxLimit {
			return 0, 0, errReviewQueuePaging("invalid limit: must be an integer between 1 and " + strconv.Itoa(reviewQueueMaxLimit))
		}
		limit = n
	}
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			return 0, 0, errReviewQueuePaging("invalid offset: must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, nil
}

func (s *Server) recordReviewQueueHistory(queue []ghpkg.ReviewQueueEntry, now time.Time) []ReviewQueueHistoryEntry {
	if s == nil {
		return nil
	}
	entry := ReviewQueueHistoryEntry{Timestamp: now.UTC().UnixMilli(), Total: len(queue)}
	for _, item := range queue {
		switch item.Priority {
		case ghpkg.ReviewPriorityHigh:
			entry.High++
		case ghpkg.ReviewPriorityLow:
			entry.Low++
		default:
			entry.Normal++
		}
	}

	storeAny, _ := reviewQueueHistoryByServer.LoadOrStore(s, &reviewQueueHistoryStore{})
	store := storeAny.(*reviewQueueHistoryStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	if n := len(store.entries); n > 0 && entry.Timestamp-store.entries[n-1].Timestamp < trendHistoryMinIntervalMs {
		store.entries[n-1] = entry
	} else {
		store.entries = append(store.entries, entry)
	}
	if len(store.entries) > trendHistoryMaxEntries {
		store.entries = store.entries[len(store.entries)-trendHistoryMaxEntries:]
	}
	out := make([]ReviewQueueHistoryEntry, len(store.entries))
	copy(out, store.entries)
	return out
}
