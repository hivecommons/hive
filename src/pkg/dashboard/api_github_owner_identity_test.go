package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#18197: the owner-only routes that connect the hive
// owner's GitHub account for write-tier reference repos.

const ownerIdentityPath = "/api/github-owner-identity"

// ownerIdentityFakes swaps the GitHub seams for the test's lifetime.
type ownerIdentityFakes struct {
	startScope string
	pollStatus string
	pollScope  string
	pollErr    error
	login      string
}

func installOwnerIdentityFakes(t *testing.T) *ownerIdentityFakes {
	t.Helper()
	f := &ownerIdentityFakes{pollStatus: "complete", pollScope: github.OwnerIdentityScope, login: "hive-owner"}
	start, poll, validate := ownerIdentityStartFlow, ownerIdentityPollFlow, ownerIdentityValidate
	t.Cleanup(func() { ownerIdentityStartFlow, ownerIdentityPollFlow, ownerIdentityValidate = start, poll, validate })
	ownerIdentityStartFlow = func(clientID, scope, baseURL, apiURL string) (*github.DeviceFlowState, error) {
		f.startScope = scope
		return &github.DeviceFlowState{DeviceCode: "dc", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", Interval: 5}, nil
	}
	ownerIdentityPollFlow = func(clientID, deviceCode, baseURL, apiURL string) (string, string, string, error) {
		if f.pollErr != nil {
			return "", "expired_token", "", f.pollErr
		}
		if f.pollStatus != "complete" {
			return "", f.pollStatus, "", nil
		}
		return "gho_secret", "complete", f.pollScope, nil
	}
	ownerIdentityValidate = func(_ context.Context, token, apiURL string) (*github.GitHubUser, error) {
		if token != "gho_secret" {
			return nil, errors.New("bad token")
		}
		return &github.GitHubUser{Login: f.login}, nil
	}
	return f
}

// ownerIdentityServer is a dashboard whose identity file lives in a temp dir.
func ownerIdentityServer(t *testing.T) (*Server, string, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets", "github-owner-token")
	t.Setenv(github.OwnerIdentityFileEnv, path)
	s := covApiServer(t)
	changed := 0
	s.deps.GitHubOwnerIdentityChanged = func() { changed++ }
	return s, path, &changed
}

func startOwnerIdentityFlow(t *testing.T, s *Server, user string) string {
	t.Helper()
	rec := doOwnerPostAsUser(s, ownerIdentityPath+"/start", user, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		FlowID   string `json:"flow_id"`
		UserCode string `json:"user_code"`
		Scope    string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.FlowID == "" || body.UserCode != "ABCD-1234" || body.Scope != github.OwnerIdentityScope {
		t.Fatalf("start body = %s", rec.Body.String())
	}
	return body.FlowID
}

func pollOwnerIdentity(t *testing.T, s *Server, user, flowID string) map[string]any {
	t.Helper()
	rec := doOwnerPostAsUser(s, ownerIdentityPath+"/poll", user, map[string]any{"flow_id": flowID})
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gho_secret") {
		t.Fatalf("the token must never appear in a response: %s", rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGitHubOwnerIdentity_OwnerOnly(t *testing.T) {
	s, _, _ := ownerIdentityServer(t)
	installOwnerIdentityFakes(t)
	if rec := doGetNoRole(s, ownerIdentityPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: %d", rec.Code)
	}
	for _, path := range []string{ownerIdentityPath + "/start", ownerIdentityPath + "/poll"} {
		rec := doPostNoRoleOwnerIdentity(s, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner POST %s: %d", path, rec.Code)
		}
	}
	rec := serveOwnerIdentity(s, httptest.NewRequest(http.MethodDelete, ownerIdentityPath, nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner DELETE: %d", rec.Code)
	}
}

func TestGitHubOwnerIdentity_ConnectAndDisconnect(t *testing.T) {
	s, path, changed := ownerIdentityServer(t)
	fakes := installOwnerIdentityFakes(t)

	rec := doOwnerGet(s, ownerIdentityPath)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connected":false`) || !strings.Contains(rec.Body.String(), `"scope_requested":"public_repo"`) {
		t.Fatalf("GET before connect: %d %s", rec.Code, rec.Body.String())
	}

	flowID := startOwnerIdentityFlow(t, s, "hive-owner")
	if fakes.startScope != github.OwnerIdentityScope {
		t.Fatalf("start requested scope %q, want %q", fakes.startScope, github.OwnerIdentityScope)
	}

	// A poll that cannot prove it started the flow gets nothing.
	if rec := doOwnerPostAsUser(s, ownerIdentityPath+"/poll", "hive-owner", map[string]any{"flow_id": "wrong"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong flow_id: %d %s", rec.Code, rec.Body.String())
	}
	fakes.pollStatus = "authorization_pending"
	if body := pollOwnerIdentity(t, s, "hive-owner", flowID); body["status"] != "pending" {
		t.Fatalf("pending poll = %v", body)
	}
	fakes.pollStatus = "complete"
	body := pollOwnerIdentity(t, s, "hive-owner", flowID)
	if body["status"] != "complete" {
		t.Fatalf("complete poll = %v", body)
	}
	if *changed != 1 {
		t.Fatalf("connect must trigger one re-resolve, got %d", *changed)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("identity file not written: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v, want 0600", fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	var stored github.OwnerIdentity
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Login != "hive-owner" || stored.Token != "gho_secret" || stored.Scope != github.OwnerIdentityScope || stored.ConnectedAt.IsZero() {
		t.Fatalf("stored identity = %+v", stored)
	}

	rec = doOwnerGet(s, ownerIdentityPath)
	if !strings.Contains(rec.Body.String(), `"connected":true`) || !strings.Contains(rec.Body.String(), `"login":"hive-owner"`) || strings.Contains(rec.Body.String(), "gho_secret") {
		t.Fatalf("GET after connect: %s", rec.Body.String())
	}

	// The flow is spent: polling again is refused.
	if rec := doOwnerPostAsUser(s, ownerIdentityPath+"/poll", "hive-owner", map[string]any{"flow_id": flowID}); rec.Code != http.StatusBadRequest {
		t.Fatalf("spent flow: %d", rec.Code)
	}

	rec = doDelete(s, ownerIdentityPath, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connected":false`) {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disconnect must remove the file, stat err=%v", err)
	}
	if *changed != 2 {
		t.Fatalf("disconnect must trigger a re-resolve, got %d", *changed)
	}
}

func TestGitHubOwnerIdentity_RejectsOtherAccountAndMissingScope(t *testing.T) {
	s, path, changed := ownerIdentityServer(t)
	fakes := installOwnerIdentityFakes(t)

	// The approved account is not the signed-in owner.
	fakes.login = "someone-else"
	body := pollOwnerIdentity(t, s, "hive-owner", startOwnerIdentityFlow(t, s, "hive-owner"))
	if body["status"] != "error" || !strings.Contains(body["error"].(string), "someone-else") {
		t.Fatalf("mismatched login poll = %v", body)
	}

	// GitHub did not grant the scope.
	fakes.login = "hive-owner"
	fakes.pollScope = ""
	body = pollOwnerIdentity(t, s, "hive-owner", startOwnerIdentityFlow(t, s, "hive-owner"))
	if body["status"] != "error" || !strings.Contains(body["error"].(string), github.OwnerIdentityScope) {
		t.Fatalf("missing scope poll = %v", body)
	}

	// The device flow itself failed.
	fakes.pollScope = github.OwnerIdentityScope
	fakes.pollErr = errors.New("expired_token: the device code has expired")
	body = pollOwnerIdentity(t, s, "hive-owner", startOwnerIdentityFlow(t, s, "hive-owner"))
	if body["status"] != "error" {
		t.Fatalf("failed flow poll = %v", body)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a rejected connect must store nothing, stat err=%v", err)
	}
	if *changed != 0 {
		t.Fatalf("a rejected connect must not re-resolve, got %d", *changed)
	}
}

// The login flow and its empty scope are untouched: the identity flow has
// its own state, so starting one does not disturb the other.
func TestGitHubOwnerIdentity_SeparateFromLoginFlow(t *testing.T) {
	s, _, _ := ownerIdentityServer(t)
	installOwnerIdentityFakes(t)
	startOwnerIdentityFlow(t, s, "hive-owner")
	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()
	if s.deviceFlowState != nil || s.deviceFlowID != "" {
		t.Fatal("the owner-identity flow must not touch the login device flow state")
	}
}

func doPostNoRoleOwnerIdentity(s *Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	return serveOwnerIdentity(s, req)
}

func serveOwnerIdentity(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}
