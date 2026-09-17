package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The kick-list caps bound how many issues and PRs a kick prompt may list, so
// prompt size stops tracking backlog size (hivecommons/hive#7368). They are
// operator settings edited on the Repos tab, which means they save through
// handleGovernorRepos alongside the repo list. These tests pin that a cap-only
// save works, that out-of-range values are refused, and that a save touching
// one field never silently resets the other.

func putRepos(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	return w
}

// A cap-only save carries no repos and no primaryRepo. Before the caps existed
// that shape was rejected as "at least one repo is required", so this is the
// guard that had to be relaxed — without breaking the genuinely-empty case.
func TestGovernorRepos_CapOnlySaveIsAccepted(t *testing.T) {
	srv := newFullServer(t)

	w := putRepos(t, srv, `{"maxPRsPerKick":12}`)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxPRs; got != 12 {
		t.Errorf("MaxPRs = %d, want 12", got)
	}
}

// The relaxation must not swallow the empty save: a body with no repos, no
// primary and no caps still changes nothing and must still be refused.
func TestGovernorRepos_TrulyEmptyBodyStillRejected(t *testing.T) {
	srv := newFullServer(t)

	w := putRepos(t, srv, `{}`)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for a body that changes nothing", w.Code)
	}
}

func TestGovernorRepos_SavesBothCaps(t *testing.T) {
	srv := newFullServer(t)

	w := putRepos(t, srv, `{"maxIssuesPerKick":40,"maxPRsPerKick":15}`)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxIssues; got != 40 {
		t.Errorf("MaxIssues = %d, want 40", got)
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxPRs; got != 15 {
		t.Errorf("MaxPRs = %d, want 15", got)
	}
}

// Pointer-typed fields: a save that omits a cap must leave it alone. Editing
// the repo list must not reset caps the operator set earlier.
func TestGovernorRepos_OmittedCapIsUnchanged(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.KickLimits.MaxIssues = 55
	srv.deps.Config.Governor.KickLimits.MaxPRs = 22

	w := putRepos(t, srv, `{"repos":["repo1"],"primaryRepo":"repo1"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxIssues; got != 55 {
		t.Errorf("MaxIssues = %d, want it left at 55", got)
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxPRs; got != 22 {
		t.Errorf("MaxPRs = %d, want it left at 22", got)
	}
}

func TestGovernorRepos_RejectsOutOfRangeCaps(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"issues above ceiling", fmt.Sprintf(`{"maxIssuesPerKick":%d}`, config.MaxKickListCap+1)},
		{"PRs above ceiling", fmt.Sprintf(`{"maxPRsPerKick":%d}`, config.MaxKickListCap+1)},
		{"issues negative", `{"maxIssuesPerKick":-5}`},
		{"PRs negative", `{"maxPRsPerKick":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFullServer(t)

			w := putRepos(t, srv, tc.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 for %s", w.Code, tc.body)
			}
		})
	}
}

// A rejected cap must not be half-applied: validation runs before any mutation.
func TestGovernorRepos_RejectedCapLeavesConfigUntouched(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.KickLimits.MaxPRs = 25

	body := fmt.Sprintf(`{"maxIssuesPerKick":40,"maxPRsPerKick":%d}`, config.MaxKickListCap+1)
	w := putRepos(t, srv, body)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxIssues; got != 0 {
		t.Errorf("MaxIssues = %d, want 0 — the valid field must not apply when the request is rejected", got)
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxPRs; got != 25 {
		t.Errorf("MaxPRs = %d, want it left at 25", got)
	}
}

// Zero means "unset, use the default" and must be accepted — it is how the
// operator clears a custom cap.
func TestGovernorRepos_ZeroCapIsAcceptedAsUnset(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.KickLimits.MaxPRs = 12

	w := putRepos(t, srv, `{"maxPRsPerKick":0}`)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Governor.KickLimits.MaxPRs; got != 0 {
		t.Errorf("MaxPRs = %d, want 0 (unset)", got)
	}
	if got := srv.deps.Config.Governor.KickLimits.PRsPerKick(); got != config.DefaultMaxPRsPerKick {
		t.Errorf("effective cap = %d, want the default %d", got, config.DefaultMaxPRsPerKick)
	}
}

// The Repos tab renders the number actually in force, so the read path must
// send the EFFECTIVE cap rather than a bare zero for an unset value.
func TestGovernorConfig_ExposesEffectiveCaps(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Governor.KickLimits.MaxIssues = 0 // unset
	srv.deps.Config.Governor.KickLimits.MaxPRs = 17

	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorConfigGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := resp["maxIssuesPerKick"].(float64); !ok || int(got) != config.DefaultMaxIssuesPerKick {
		t.Errorf("maxIssuesPerKick = %v, want the effective default %d", resp["maxIssuesPerKick"], config.DefaultMaxIssuesPerKick)
	}
	if got, ok := resp["maxPRsPerKick"].(float64); !ok || int(got) != 17 {
		t.Errorf("maxPRsPerKick = %v, want 17", resp["maxPRsPerKick"])
	}
}
