package dashboard

import (
	"encoding/json"
	"testing"
)

// TestAuthorizedUsersListEndpoint verifies the read-only Access endpoint returns
// the spoke allowlist with resolved roles (first entry = owner default, explicit
// roles honored, bare names default to read) and the enforced flag.
func TestAuthorizedUsersListEndpoint(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = []string{"alice:owner", "bob:read-write", "carol", ""}

	rec := doGet(s, "/api/config/authorized-users")
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"users"`
		Enforced bool `json:"enforced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enforced {
		t.Error("enforced should be true when the allowlist is non-empty")
	}
	roles := map[string]string{}
	for _, u := range resp.Users {
		roles[u.Username] = u.Role
	}
	if roles["alice"] != "owner" || roles["bob"] != "read-write" || roles["carol"] != "read" {
		t.Errorf("unexpected roles: %v", roles)
	}
	if len(resp.Users) != 3 { // the empty entry must be skipped
		t.Errorf("expected 3 users (empty entry skipped), got %d: %v", len(resp.Users), roles)
	}
}

// TestAuthorizedUsersListEndpoint_Empty: no allowlist -> enforced false, no users.
func TestAuthorizedUsersListEndpoint_Empty(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Dashboard.AuthorizedUsers = nil

	rec := doGet(s, "/api/config/authorized-users")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Users    []any `json:"users"`
		Enforced bool  `json:"enforced"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Enforced || len(resp.Users) != 0 {
		t.Errorf("empty allowlist: enforced=%v users=%d", resp.Enforced, len(resp.Users))
	}
}
