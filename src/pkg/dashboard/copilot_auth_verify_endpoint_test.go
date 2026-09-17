package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /api/copilot-auth/verify re-runs seat verification on demand (#7309). It is
// owner-only like the other copilot-auth mutations: a client-supplied
// X-Hive-Role alone is spoofable, so the server-only verification marker is
// required too.
func TestCopilotAuthVerifyRequiresOwnerRole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     string
		verified bool
	}{
		{name: "no role"},
		{name: "non-owner role", role: "read-write"},
		{name: "owner without server verification", role: "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := covApiServer(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/copilot-auth/verify", nil)
			if tc.role != "" {
				req.Header.Set("X-Hive-Role", tc.role)
			}
			if tc.verified {
				req.Header.Set(ownerRoleVerifiedHeader, "true")
			}

			s.handleCopilotAuthVerify(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

// The verify endpoint exists precisely because status reads the cache: verify
// must force a fresh probe every call, never serve a stale verdict.
func TestCopilotAuthVerifyForcesFreshSeatVerdict(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "tok-verify")

	// Never reach api.github.com for the login behind the token.
	prevLookup := lookupCopilotTokenLogin
	lookupCopilotTokenLogin = func(string) string { return "alice" }
	t.Cleanup(func() { lookupCopilotTokenLogin = prevLookup })

	probes := 0
	copilotSeatStub(t, func(w http.ResponseWriter, r *http.Request) {
		probes++
		w.WriteHeader(http.StatusOK)
	})

	s := covApiServer(t)
	verify := func() copilotSeatVerdict {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/copilot-auth/verify", nil)
		req.Header.Set("X-Hive-Role", "owner")
		req.Header.Set(ownerRoleVerifiedHeader, "true")
		s.handleCopilotAuthVerify(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}
		var body struct {
			Seat copilotSeatVerdict `json:"seat"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response json: %v", err)
		}
		return body.Seat
	}

	if seat := verify(); seat.State != copilotSeatActive {
		t.Fatalf("seat.state = %q, want %q", seat.State, copilotSeatActive)
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1", probes)
	}

	// A second verify must probe again (force=true), not serve the cached
	// verdict the way the polled status endpoint does.
	if seat := verify(); seat.State != copilotSeatActive {
		t.Fatalf("second seat.state = %q, want %q", seat.State, copilotSeatActive)
	}
	if probes != 2 {
		t.Fatalf("probes = %d after second verify, want 2 (verify must bypass the cache)", probes)
	}
}
