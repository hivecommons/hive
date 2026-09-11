package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// v1QueueServer builds a v1-authorized server with n actionable issues seeded
// in one repo, so /api/v1/queue has a real backlog to page through.
func v1QueueServer(t *testing.T, n int) *Server {
	t.Helper()
	setupContributeEnv(t)
	s := v1Server(t, "octocat")
	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", n)
	s.statusMu.Unlock()
	return s
}

type v1QueueResponse struct {
	Queue   []ReadyQueueItem `json:"queue"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	HasMore bool             `json:"has_more"`
}

func decodeV1Queue(t *testing.T, body []byte) v1QueueResponse {
	t.Helper()
	var resp v1QueueResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode /api/v1/queue response: %v (body=%s)", err, body)
	}
	return resp
}

// Auth required: no bearer token -> 401, same as every other /api/v1 path.
func TestCovV1Queue_NoToken(t *testing.T) {
	s := v1QueueServer(t, 5)
	rec := v1Get(s, "/api/v1/queue", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rec.Code)
	}
}

// Default page: no query params returns the full small backlog with correct
// total/limit/offset metadata.
func TestCovV1Queue_DefaultPage(t *testing.T) {
	s := v1QueueServer(t, 5)
	rec := v1Get(s, "/api/v1/queue", "goodtoken")
	if rec.Code != http.StatusOK {
		t.Fatalf("default page: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeV1Queue(t, rec.Body.Bytes())
	if resp.Total != 5 || len(resp.Queue) != 5 {
		t.Fatalf("default page: want total=5 len=5, got total=%d len=%d", resp.Total, len(resp.Queue))
	}
	if resp.Offset != 0 || resp.Limit != readyQueueDefaultLimit {
		t.Fatalf("default page: want offset=0 limit=%d, got offset=%d limit=%d", readyQueueDefaultLimit, resp.Offset, resp.Limit)
	}
	if resp.HasMore {
		t.Fatal("default page: has_more should be false when the whole backlog fits on one page")
	}
}

// limit+offset slicing: a backlog larger than one page slices correctly and
// reports has_more until the last page.
func TestCovV1Queue_LimitOffsetSlicing(t *testing.T) {
	s := v1QueueServer(t, 25)

	rec := v1Get(s, "/api/v1/queue?limit=10&offset=0", "goodtoken")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1: want 200, got %d", rec.Code)
	}
	page1 := decodeV1Queue(t, rec.Body.Bytes())
	if page1.Total != 25 || len(page1.Queue) != 10 || !page1.HasMore {
		t.Fatalf("page1: want total=25 len=10 has_more=true, got total=%d len=%d has_more=%v", page1.Total, len(page1.Queue), page1.HasMore)
	}

	rec = v1Get(s, "/api/v1/queue?limit=10&offset=10", "goodtoken")
	page2 := decodeV1Queue(t, rec.Body.Bytes())
	if len(page2.Queue) != 10 || !page2.HasMore {
		t.Fatalf("page2: want len=10 has_more=true, got len=%d has_more=%v", len(page2.Queue), page2.HasMore)
	}

	rec = v1Get(s, "/api/v1/queue?limit=10&offset=20", "goodtoken")
	page3 := decodeV1Queue(t, rec.Body.Bytes())
	if len(page3.Queue) != 5 || page3.HasMore {
		t.Fatalf("page3 (last, partial): want len=5 has_more=false, got len=%d has_more=%v", len(page3.Queue), page3.HasMore)
	}

	// The three pages must not overlap and must cover the full ordered set.
	seen := map[string]bool{}
	for _, page := range []v1QueueResponse{page1, page2, page3} {
		for _, item := range page.Queue {
			key := item.Repo + "#" + item.Title
			if seen[key] {
				t.Fatalf("item %q appeared on more than one page", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != 25 {
		t.Fatalf("want 25 distinct items across all pages, got %d", len(seen))
	}
}

// Offset past the end of the backlog returns an empty page with the correct
// (unchanged) total, not an error.
func TestCovV1Queue_OffsetPastEnd(t *testing.T) {
	s := v1QueueServer(t, 5)
	rec := v1Get(s, "/api/v1/queue?offset=1000", "goodtoken")
	if rec.Code != http.StatusOK {
		t.Fatalf("offset past end: want 200, got %d", rec.Code)
	}
	resp := decodeV1Queue(t, rec.Body.Bytes())
	if resp.Total != 5 || len(resp.Queue) != 0 || resp.HasMore {
		t.Fatalf("offset past end: want total=5 len=0 has_more=false, got total=%d len=%d has_more=%v", resp.Total, len(resp.Queue), resp.HasMore)
	}
}

// The queue's reported total must agree with /api/contribute/queue's
// queue_total for the SAME backlog: both are the offerable population from
// the same admission sweep, and must never drift from one another (#6477).
func TestCovV1Queue_TotalMatchesContributeQueueTotal(t *testing.T) {
	s := v1QueueServer(t, 37)

	rec := v1Get(s, "/api/v1/queue?limit=5", "goodtoken")
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 queue: want 200, got %d", rec.Code)
	}
	v1Resp := decodeV1Queue(t, rec.Body.Bytes())

	legacyRec := v1Get(s, "/api/contribute/queue", "goodtoken")
	if legacyRec.Code != http.StatusOK {
		t.Fatalf("legacy queue: want 200, got %d", legacyRec.Code)
	}
	var legacy struct {
		QueueTotal int `json:"queue_total"`
	}
	if err := json.Unmarshal(legacyRec.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("decode legacy queue response: %v", err)
	}

	if v1Resp.Total != legacy.QueueTotal {
		t.Fatalf("total mismatch: /api/v1/queue total=%d, /api/contribute/queue queue_total=%d", v1Resp.Total, legacy.QueueTotal)
	}
	if v1Resp.Total != 37 {
		t.Fatalf("expected total=37, got %d", v1Resp.Total)
	}
}

// Bad params (non-integer limit/offset) return 400, not a silently-defaulted
// 200.
func TestCovV1Queue_BadParams(t *testing.T) {
	s := v1QueueServer(t, 5)
	for _, q := range []string{
		"?limit=notanumber",
		"?limit=0",
		"?limit=-1",
		"?offset=notanumber",
		"?offset=-1",
	} {
		rec := v1Get(s, "/api/v1/queue"+q, "goodtoken")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: want 400, got %d body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

// An authenticated but unauthorized (not on the allowlist) caller gets 403,
// matching every other /api/v1 read.
func TestCovV1Queue_UnauthorizedUserForbidden(t *testing.T) {
	setupContributeEnv(t)
	s := v1Server(t, "outsider")
	s.statusMu.Lock()
	s.status = seedActionable("acme/repo", 5)
	s.statusMu.Unlock()
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"someone-else:" + config.RoleOwner}
	rec := v1Get(s, "/api/v1/queue", "forbidden-user-token")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized user: want 403, got %d", rec.Code)
	}
}

// The route is advertised in the "Unknown endpoint" list so downstream
// discovery (hivecommons/hive#6537) can find it without reading source.
func TestCovV1Queue_AdvertisedInUnknownEndpointList(t *testing.T) {
	s := v1QueueServer(t, 1)
	rec := v1Get(s, "/api/v1/bogus", "goodtoken")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	var body struct {
		Available []string `json:"available"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, ep := range body.Available {
		if ep == "/api/v1/queue" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/api/v1/queue missing from advertised endpoints: %v", body.Available)
	}
}
