package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func postSpokeBreaker(s *HubServer, hiveID, user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/"+hiveID+"/breaker", strings.NewReader(body))
	req.SetPathValue("id", hiveID)
	req.AddCookie(testAuthCookie(user))
	rec := httptest.NewRecorder()
	s.handleSpokeBreaker(rec, req)
	return rec
}

// #15092: only the hive's owner (or a hub admin) may pause a spoke from the
// hub, the body must say which way, and the request is queued durably on the
// hive record for the next heartbeat.
func TestHandleSpokeBreakerAuthzAndQueue(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHubServerForTest(t)
	owner, other := "owner", "other"
	mkUser(t, owner)
	mkUser(t, other)
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: owner, Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}

	if rec := postSpokeBreaker(s, "missing", owner, `{"engaged":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown hive status = %d, want 404", rec.Code)
	}
	if rec := postSpokeBreaker(s, "h1", other, `{"engaged":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner status = %d, want 403", rec.Code)
	}
	if h := loadSaaSHive("h1"); h.PendingBreaker != "" {
		t.Fatalf("forbidden request still queued %q", h.PendingBreaker)
	}
	for _, bad := range []string{``, `{}`, `{"engaged":"yes"}`} {
		if rec := postSpokeBreaker(s, "h1", owner, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400", bad, rec.Code)
		}
	}

	rec := postSpokeBreaker(s, "h1", owner, `{"engaged":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner engage status = %d body %s", rec.Code, rec.Body.String())
	}
	if h := loadSaaSHive("h1"); h.PendingBreaker != spoke.BreakerEngage || h.PendingBreakerBy != owner {
		t.Fatalf("queued = %q by %q, want engage by owner", h.PendingBreaker, h.PendingBreakerBy)
	}

	// The last click wins over an undelivered earlier one.
	if rec := postSpokeBreaker(s, "h1", owner, `{"engaged":false}`); rec.Code != http.StatusOK {
		t.Fatalf("owner release status = %d", rec.Code)
	}
	if h := loadSaaSHive("h1"); h.PendingBreaker != spoke.BreakerRelease {
		t.Fatalf("queued = %q, want release", h.PendingBreaker)
	}
}

// A queued instruction rides exactly one heartbeat: delivering it on every
// beat would re-engage a breaker the owner has since released on the spoke.
func TestPendingBreakerForHeartbeatDeliversOnce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHubServerForTest(t)
	if err := saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner", PendingBreaker: spoke.BreakerEngage, PendingBreakerBy: "owner"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}

	if got := s.pendingBreakerForHeartbeat("h1"); got != spoke.BreakerEngage {
		t.Fatalf("first beat = %q, want engage", got)
	}
	if got := s.pendingBreakerForHeartbeat("h1"); got != "" {
		t.Fatalf("second beat = %q, want nothing (one-shot)", got)
	}
	if h := loadSaaSHive("h1"); h.PendingBreaker != "" || h.PendingBreakerBy != "" {
		t.Fatalf("record not cleared: %+v", h)
	}
	if got := s.pendingBreakerForHeartbeat("no-such-hive"); got != "" {
		t.Fatalf("unknown hive = %q, want nothing", got)
	}

	// A corrupted record is cleared, never delivered.
	if err := saveSaaSHive(&SaaSHive{ID: "h2", Owner: "owner", PendingBreaker: "explode"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if got := s.pendingBreakerForHeartbeat("h2"); got != "" {
		t.Fatalf("invalid action delivered as %q", got)
	}
}
