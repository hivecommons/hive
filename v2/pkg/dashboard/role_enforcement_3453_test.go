package dashboard

// Tests verifying that handlers hardened in #3453 fail-closed when the owner
// role is absent or unverified. Each handler under test previously defaulted
// an empty X-Hive-Role to "owner" (fail-open), which allowed any
// authenticated caller to download hive.yaml, trigger self-upgrade, or rename
// the hive — all without proving owner status.
//
// These tests cover:
// - handleConfigDownload  (GET /api/config/download)
// - handleSelfUpgrade     (POST /api/self-upgrade)
// - handleVaultsConnect   (POST /api/knowledge/vaults)
// - handleHiveIDSet       (PUT /api/hive-id)
// - handleQueuePRAutoMerge (POST /api/prs/{owner}/{repo}/{number}/queue-automerge)

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRoleEnforcement_EmptyRoleRejected verifies that handlers reject requests
// with no X-Hive-Role header (fail-closed, not fail-open to owner).
func TestRoleEnforcement_EmptyRoleRejected(t *testing.T) {
	srv := newMinimalServer(t)

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{
			name:    "handleConfigDownload empty role",
			method:  http.MethodGet,
			path:    "/api/config/download",
			handler: srv.handleConfigDownload,
		},
		{
			name:    "handleSelfUpgrade empty role",
			method:  http.MethodPost,
			path:    "/api/self-upgrade",
			handler: srv.handleSelfUpgrade,
		},
		{
			name:    "handleVaultsConnect empty role",
			method:  http.MethodPost,
			path:    "/api/knowledge/vaults",
			body:    `{"path":"/tmp/test","name":"test"}`,
			handler: srv.handleVaultsConnect,
		},
		{
			name:    "handleHiveIDSet empty role",
			method:  http.MethodPut,
			path:    "/api/hive-id",
			body:    `{"id":"evil-rename"}`,
			handler: srv.handleHiveIDSet,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Intentionally no X-Hive-Role or verified header set.
			w := httptest.NewRecorder()
			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("empty role: got status %d, want 403", w.Code)
			}
		})
	}
}

// TestRoleEnforcement_NonOwnerRoleRejected verifies contributor/read-write
// roles are rejected.
func TestRoleEnforcement_NonOwnerRoleRejected(t *testing.T) {
	srv := newMinimalServer(t)

	roles := []string{"contributor", "read-write", "read-only", "merger"}

	handlers := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"handleConfigDownload", http.MethodGet, "/api/config/download", "", srv.handleConfigDownload},
		{"handleSelfUpgrade", http.MethodPost, "/api/self-upgrade", "", srv.handleSelfUpgrade},
		{"handleVaultsConnect", http.MethodPost, "/api/knowledge/vaults", `{"path":"/tmp/x","name":"x"}`, srv.handleVaultsConnect},
		{"handleHiveIDSet", http.MethodPut, "/api/hive-id", `{"id":"test-id"}`, srv.handleHiveIDSet},
	}

	for _, h := range handlers {
		for _, role := range roles {
			t.Run(h.name+"/"+role, func(t *testing.T) {
				var req *http.Request
				if h.body != "" {
					req = httptest.NewRequest(h.method, h.path, strings.NewReader(h.body))
					req.Header.Set("Content-Type", "application/json")
				} else {
					req = httptest.NewRequest(h.method, h.path, nil)
				}
				req.Header.Set("X-Hive-Role", role)
				w := httptest.NewRecorder()
				h.handler(w, req)

				if w.Code != http.StatusForbidden {
					t.Fatalf("role=%q: got status %d, want 403", role, w.Code)
				}
			})
		}
	}
}

// TestRoleEnforcement_UnverifiedOwnerRoleRejected ensures that setting
// X-Hive-Role: owner without the verified header is rejected (prevents
// header spoofing from shared-token callers).
func TestRoleEnforcement_UnverifiedOwnerRoleRejected(t *testing.T) {
	srv := newMinimalServer(t)

	handlers := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"handleConfigDownload", http.MethodGet, "/api/config/download", "", srv.handleConfigDownload},
		{"handleSelfUpgrade", http.MethodPost, "/api/self-upgrade", "", srv.handleSelfUpgrade},
		{"handleVaultsConnect", http.MethodPost, "/api/knowledge/vaults", `{"path":"/tmp/x","name":"x"}`, srv.handleVaultsConnect},
		{"handleHiveIDSet", http.MethodPut, "/api/hive-id", `{"id":"test-id"}`, srv.handleHiveIDSet},
	}

	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			var req *http.Request
			if h.body != "" {
				req = httptest.NewRequest(h.method, h.path, strings.NewReader(h.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(h.method, h.path, nil)
			}
			// Owner role set but NOT verified — simulates spoofed header.
			req.Header.Set("X-Hive-Role", "owner")
			// ownerRoleVerifiedHeader intentionally NOT set.
			w := httptest.NewRecorder()
			h.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("unverified owner: got status %d, want 403", w.Code)
			}
		})
	}
}

// TestRoleEnforcement_VerifiedOwnerAllowed confirms that a properly verified
// owner role passes the gate (handler may still fail for other reasons like
// missing config file, but should not fail with 403).
func TestRoleEnforcement_VerifiedOwnerAllowed(t *testing.T) {
	srv := newMinimalServer(t)

	handlers := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"handleHiveIDSet", http.MethodPut, "/api/hive-id", `{"id":"valid-name"}`, srv.handleHiveIDSet},
	}

	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			var req *http.Request
			if h.body != "" {
				req = httptest.NewRequest(h.method, h.path, strings.NewReader(h.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(h.method, h.path, nil)
			}
			markOwnerRequest(req)
			w := httptest.NewRecorder()
			h.handler(w, req)

			if w.Code == http.StatusForbidden {
				t.Fatalf("verified owner got 403: body=%q", w.Body.String())
			}
		})
	}
}

// TestRoleEnforcement_QueuePRAutoMerge_EmptyRoleRejected verifies that
// handleQueuePRAutoMerge does not default empty role to owner. After #3453,
// an empty role should fail the merger-or-owner check.
func TestRoleEnforcement_QueuePRAutoMerge_EmptyRoleRejected(t *testing.T) {
	srv := newMinimalServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/prs/testorg/testrepo/42/queue-automerge", nil)
	req.SetPathValue("owner", "testorg")
	req.SetPathValue("repo", "testrepo")
	req.SetPathValue("number", "42")
	req.Header.Set("X-Hive-User", "attacker")
	// No X-Hive-Role header — should NOT default to owner.
	w := httptest.NewRecorder()
	srv.handleQueuePRAutoMerge(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("empty role on queue-automerge: got status %d, want 403; body=%q", w.Code, w.Body.String())
	}
}

func TestRoleEnforcement_QueuePRAutoMerge_UnverifiedOwnerRejected(t *testing.T) {
	srv := newMinimalServer(t)

	for _, role := range []string{"owner", " OWNER "} {
		t.Run(role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/prs/testorg/testrepo/42/queue-automerge", nil)
			req.SetPathValue("owner", "testorg")
			req.SetPathValue("repo", "testrepo")
			req.SetPathValue("number", "42")
			req.Header.Set("X-Hive-User", "attacker")
			req.Header.Set("X-Hive-Role", role)
			// ownerRoleVerifiedHeader intentionally NOT set.
			w := httptest.NewRecorder()
			srv.handleQueuePRAutoMerge(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("unverified owner role %q on queue-automerge: got status %d, want 403; body=%q", role, w.Code, w.Body.String())
			}
		})
	}
}

// TestRoleEnforcement_QueuePRAutoMerge_EmptyRoleRejectedEvenWhenOwnerVerified
// pins the #3449 fix at the point where it can actually be observed.
//
// TestRoleEnforcement_QueuePRAutoMerge_EmptyRoleRejected above sends no
// owner-verification header, so it returns 403 whether or not the empty role
// was defaulted to owner: reinstating `if role == "" { role = RoleOwner }`
// leaves that test passing, because the unverified-owner branch rejects the
// request first. It therefore cannot detect the very regression it is named
// for.
//
// Sending the verification header removes that second gate, so the ONLY thing
// left standing between an empty role and a queued merge is the fail-closed
// role check itself. If the fail-open default ever comes back, this test
// fails.
func TestRoleEnforcement_QueuePRAutoMerge_EmptyRoleRejectedEvenWhenOwnerVerified(t *testing.T) {
	srv := newMinimalServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/prs/testorg/testrepo/42/queue-automerge", nil)
	req.SetPathValue("owner", "testorg")
	req.SetPathValue("repo", "testrepo")
	req.SetPathValue("number", "42")
	req.Header.Set("X-Hive-User", "attacker")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	// X-Hive-Role deliberately absent: an empty role must never be promoted.
	w := httptest.NewRecorder()
	srv.handleQueuePRAutoMerge(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("empty role with owner-verification present must still be refused: got status %d, want 403; body=%q", w.Code, w.Body.String())
	}
}
