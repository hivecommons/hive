package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests pin the #4081 fix: a stale/demoted stored role
// (SaaSUser.Hives[hiveID]) must never hide owner-gated affordances (the My
// Hives Upgrade link, the spoke's requireOwnerRole gate behind the auth
// proxy) from the hive's TRUE (canonical) owner. Owners are elevated only on
// their OWN hives; other users' roles are forwarded verbatim.

// demotedOwnerFixture stores a user whose stored role for their own hive has
// been demoted to "read" — the corruption handleApproveAccess used to write.
func demotedOwnerFixture(t *testing.T, s *HubServer, username, hiveID string) {
	t.Helper()
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{
		{ID: hiveID, Owner: username, Online: true},
	}
	s.mu.Unlock()
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: username,
		Hives:          map[string]string{hiveID: "read"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}
}

func TestMyHivesNormalizesDemotedRoleForTrueOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")

	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "spoke-owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hives := resp.Hives
	var found bool
	for _, h := range hives {
		if h.ID == "owned-hive" {
			found = true
			if h.Role != "owner" {
				t.Errorf("role = %q, want owner (demoted stored role must not mask the true owner)", h.Role)
			}
		}
	}
	if !found {
		t.Fatal("owned-hive missing from my-hives")
	}

	// The demoted stored role must be healed so the auth proxy and every
	// other stored-role reader sees owner on the next request too.
	u := loadSaaSUser("spoke-owner")
	if u == nil || u.Hives["owned-hive"] != "owner" {
		t.Errorf("stored role = %q, want owner (healed)", u.Hives["owned-hive"])
	}
}

func TestMyHivesNormalizesDemotedRoleForHostedHiveOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	// Hosted hive with NO registry entry — only the SaaS meta record — and a
	// demoted stored role for its owner.
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-norm", Owner: "hosted-owner", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "hosted-owner",
		Hives:          map[string]string{"hosted-norm": "read"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "hosted-owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hives := resp.Hives
	var found bool
	for _, h := range hives {
		if h.ID == "hosted-norm" {
			found = true
			if h.Role != "owner" {
				t.Errorf("role = %q, want owner", h.Role)
			}
		}
	}
	if !found {
		t.Fatal("hosted-norm missing from my-hives")
	}
}

func TestMyHivesDoesNotElevateNonOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{
		{ID: "someone-elses-hive", Owner: "real-owner", Online: true},
	}
	s.mu.Unlock()
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "just-a-reader",
		Hives:          map[string]string{"someone-elses-hive": "read"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleMyHives(rec, reqWithUser(http.MethodGet, "/api/saas/my-hives", "", "just-a-reader"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hives := resp.Hives
	for _, h := range hives {
		if h.ID == "someone-elses-hive" && h.Role != "read" {
			t.Errorf("role = %q, want read (non-owners must never be elevated)", h.Role)
		}
	}
}

func TestAccessStatusNormalizesDemotedRoleForTrueOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")

	rec := httptest.NewRecorder()
	s.handleAccessStatus(rec, reqWithUser(http.MethodGet, "/api/saas/access-status", "", "spoke-owner"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp struct {
		Hives map[string]struct {
			Role   string `json:"role"`
			Status string `json:"status"`
		} `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := resp.Hives["owned-hive"]
	if !ok {
		t.Fatal("owned-hive missing from access-status")
	}
	if got.Role != "owner" {
		t.Errorf("role = %q, want owner", got.Role)
	}
}

func TestSaaSAuthCheckElevatesTrueOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")

	req := reqWithUser(http.MethodGet, "/api/saas/auth-check?hive=owned-hive", "", "spoke-owner")
	req.Header.Set("X-Original-URI", "/api/self-upgrade")
	rec := httptest.NewRecorder()
	s.handleSaaSAuthCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Hive-Role"); got != "owner" {
		t.Errorf("X-Hive-Role = %q, want owner (spoke requireOwnerRole gates on this header)", got)
	}
	if got := rec.Header().Get("X-Hive-User"); got != "spoke-owner" {
		t.Errorf("X-Hive-User = %q, want spoke-owner", got)
	}
}

func TestSaaSAuthCheckElevatesSaaSHiveOwnerRole(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-auth", Owner: "hosted-owner", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "hosted-owner",
		Hives:          map[string]string{"hosted-auth": "read"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	req := reqWithUser(http.MethodGet, "/api/saas/auth-check?hive=hosted-auth", "", "hosted-owner")
	req.Header.Set("X-Original-URI", "/api/self-upgrade")
	rec := httptest.NewRecorder()
	s.handleSaaSAuthCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Hive-Role"); got != "owner" {
		t.Errorf("X-Hive-Role = %q, want owner", got)
	}
}

func TestSaaSAuthCheckForwardsNonOwnerRoleVerbatim(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{
		{ID: "owned-hive", Owner: "real-owner", Online: true},
	}
	s.mu.Unlock()
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "just-a-reader",
		Hives:          map[string]string{"owned-hive": "read"},
	}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	req := reqWithUser(http.MethodGet, "/api/saas/auth-check?hive=owned-hive", "", "just-a-reader")
	req.Header.Set("X-Original-URI", "/api/self-upgrade")
	rec := httptest.NewRecorder()
	s.handleSaaSAuthCheck(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Hive-Role"); got != "read" {
		t.Errorf("X-Hive-Role = %q, want read (non-owners must never be elevated)", got)
	}
}

func TestApproveAccessDoesNotDemoteOwner(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	hiveID := "hosted-approve"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "the-owner", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "the-owner",
		Hives:          map[string]string{hiveID: "owner"},
	}); err != nil {
		t.Fatalf("save owner: %v", err)
	}
	// The owner somehow has a pending access request against their own hive
	// (the reproduction path for the original demotion).
	saveAccessRequests(hiveID, []AccessRequest{{Username: "the-owner", Status: "pending"}})

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPut, "/approve", "", "the-owner"),
		map[string]string{"id": hiveID, "username": "the-owner"})
	s.handleApproveAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("the-owner")
	if u == nil || u.Hives[hiveID] != "owner" {
		t.Errorf("stored role = %q, want owner (approve must not demote an owner)", u.Hives[hiveID])
	}
}

func TestApproveAccessStillGrantsReadToRegularUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newHandlerHub()
	hiveID := "hosted-approve2"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "the-owner", ClusterID: "hive-oke", Org: "org", PrimaryRepo: "repo"}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "the-owner",
		Hives:          map[string]string{hiveID: "owner"},
	}); err != nil {
		t.Fatalf("save owner: %v", err)
	}
	saveAccessRequests(hiveID, []AccessRequest{{Username: "newcomer", Status: "pending"}})

	rec := httptest.NewRecorder()
	req := setPathValues(reqWithUser(http.MethodPut, "/approve", "", "the-owner"),
		map[string]string{"id": hiveID, "username": "newcomer"})
	s.handleApproveAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	u := loadSaaSUser("newcomer")
	if u == nil || u.Hives[hiveID] != "read" {
		t.Errorf("stored role = %q, want read", u.Hives[hiveID])
	}
}
