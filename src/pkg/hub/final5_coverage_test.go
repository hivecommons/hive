package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// saas.go — more cheap validation branches
// ============================================================

func TestHandleAssignHiveMorePaths(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, hubAdminUsername)
	s := newHandlerHub2()

	reset := func() {
		saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ClusterID: "hive-oke"})
	}

	// Invalid primary_repo.
	reset()
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/assign", `{"owner":"bob","org":"acme","repos":"r","primary_repo":"bad repo!"}`, hubAdminUsername), "id", "ph1")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid primary_repo status = %d, want 400", rec.Code)
	}

	// App creds present but the private key is not PEM.
	reset()
	body := `{"owner":"bob","org":"acme","repos":"r","app_id":"1","installation_id":"2","app_private_key":"not-a-pem"}`
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/assign", body, hubAdminUsername), "id", "ph1")
	s.handleAssignHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-PEM app key status = %d, want 400", rec.Code)
	}
}

func TestHandleCreateHiveMorePaths(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{}, SaaSQuota: 5})
	s := newHandlerHub2()

	// Invalid repo in a comma list.
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodPost, "/create", `{"org":"acme","repos":"good,bad repo!","github_token":"ghp_x"}`, "alice")
	s.handleCreateHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid repo-in-list status = %d, want 400", rec.Code)
	}

	// App auth with a non-PEM private key.
	rec = httptest.NewRecorder()
	body := `{"org":"acme","repos":"repo","auth_method":"app","app_id":"1","installation_id":"2","app_private_key":"nope"}`
	req = reqWithUser(http.MethodPost, "/create", body, "alice")
	s.handleCreateHive(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-PEM app key create status = %d, want 400", rec.Code)
	}
}

func TestIsValidRepoRefEdges(t *testing.T) {
	if isValidRepoRef("a/b/c") {
		t.Error("more than one slash should be invalid")
	}
	if !isValidRepoRef("owner/repo") {
		t.Error("owner/repo should be valid")
	}
	if isValidRepoRef("bad repo!") {
		t.Error("space/bang should be invalid")
	}
	long := ""
	for i := 0; i < 202; i++ {
		long += "a"
	}
	if isValidRepoRef(long) {
		t.Error("overlong ref should be invalid")
	}
}
