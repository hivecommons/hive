package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func doShareRolePost(s *Server, path, role string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &b)
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if role == "owner" {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

// Sharing an item (route, Knowledge-only) is open to owner and read-write;
// read and anonymous callers get 403. Settings stay owner-only.
func TestInboxShareRoleMatrix(t *testing.T) {
	s, deps := apiServer(t)
	spek := &inboxTestDest{name: "spek_rfc"}
	deps.Config.Inbox = config.InboxConfig{Spek: config.InboxSpek{Repo: "myorg/rfcs"}}
	deps.Inbox = newInboxTestService(t, deps.Config.Inbox, spek)
	route := map[string]string{"source": "plaud", "item_id": "rec-1", "destination": "spek_rfc"}
	for _, role := range []string{"", "read-only", "read"} {
		if rec := doShareRolePost(s, "/api/inbox/route", role, route); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q route = %d, want 403", role, rec.Code)
		}
		if rec := doShareRolePost(s, "/api/inbox/dismiss", role, map[string]any{"items": []map[string]string{{"source": "plaud", "item_id": "rec-1"}}}); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q dismiss = %d, want 403", role, rec.Code)
		}
	}
	if rec := doShareRolePost(s, "/api/inbox/route", "read-write", route); rec.Code != http.StatusAccepted {
		t.Fatalf("read-write route = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doShareRolePost(s, "/api/inbox/dismiss", "read-write", map[string]any{"items": []map[string]string{{"source": "plaud", "item_id": "rec-1"}}}); rec.Code != http.StatusOK {
		t.Fatalf("read-write dismiss = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doShareRolePost(s, "/api/inbox/route", "owner", route); rec.Code != http.StatusAccepted {
		t.Fatalf("owner route = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doShareRolePost(s, "/api/inbox/settings", "read-write", map[string]any{"spek_repo": "o/r"}); rec.Code != http.StatusForbidden {
		t.Fatalf("read-write settings = %d, want 403 (settings stay owner-only)", rec.Code)
	}
}
