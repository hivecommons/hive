package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#18197: the owner-only consent that connects the hive
// owner's GitHub account for remote repos.

const ownerIdentityPath = "/api/github-owner-identity"

// ownerIdentityServer wires a server with a temp identity store and fakes
// for the three GitHub calls. It returns the store and a pointer to the
// granted scope / login the fakes answer with.
func ownerIdentityServer(t *testing.T) (*Server, *github.OwnerIdentityStore, *ownerIdentityFakes) {
	t.Helper()
	s := covApiServer(t)
	store := github.NewOwnerIdentityStore(filepath.Join(t.TempDir(), "github-owner-token"))
	s.deps.OwnerIdentity = store
	f := &ownerIdentityFakes{login: "nate-double-u", scope: github.OwnerIdentityScope, status: "complete"}
	s.deps.OwnerIdentityChanged = func() { f.changed++ }

	prevStart, prevPoll, prevValidate := ownerIdentityStartFlow, ownerIdentityPollFlow, ownerIdentityValidate
	t.Cleanup(func() {
		ownerIdentityStartFlow, ownerIdentityPollFlow, ownerIdentityValidate = prevStart, prevPoll, prevValidate
	})
	ownerIdentityStartFlow = func(clientID, scope, baseURL, apiURL string) (*github.DeviceFlowState, error) {
		f.startScope = scope
		if f.startErr != nil {
			return nil, f.startErr
		}
		return &github.DeviceFlowState{DeviceCode: "dev-code", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5}, nil
	}
	ownerIdentityPollFlow = func(clientID, deviceCode, baseURL, apiURL string) (string, string, string, error) {
		f.polls++
		if deviceCode != "dev-code" {
			return "", "", "", errors.New("wrong device code")
		}
		if f.pollErr != nil {
			return "", "access_denied", "", f.pollErr
		}
		if f.status != "complete" {
			return "", f.status, "", nil
		}
		return "gho_owner_token", "complete", f.scope, nil
	}
	ownerIdentityValidate = func(_ context.Context, token, apiURL string) (*github.GitHubUser, error) {
		if token != "gho_owner_token" || f.validateErr != nil {
			return nil, errors.New("token invalid: status 401")
		}
		return &github.GitHubUser{Login: f.login}, nil
	}
	return s, store, f
}

type ownerIdentityFakes struct {
	login, scope, status string
	startScope           string
	startErr, pollErr    error
	validateErr          error
	polls, changed       int
}

func doOwnerDelete(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func doOwnerPostAsBody(s *Server, path, user string, body any) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var raw strings.Builder
	_ = json.NewEncoder(&raw).Encode(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw.String()))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return m
}

func TestOwnerIdentity_OwnerOnlyAndUnavailable(t *testing.T) {
	s := covApiServer(t)
	// No store wired: 503 on every route, never a panic.
	if rec := doOwnerGet(s, ownerIdentityPath); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doOwnerPost(s, ownerIdentityPath+"/start", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store start: %d", rec.Code)
	}
	if rec := doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": "x"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store poll: %d", rec.Code)
	}
	if rec := doOwnerDelete(s, ownerIdentityPath); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store delete: %d", rec.Code)
	}

	s, _, _ = ownerIdentityServer(t)
	if rec := doGetNoRole(s, ownerIdentityPath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, ownerIdentityPath+"/start", nil)
	req.Header.Set("X-Hive-Role", "owner") // role header alone, no verified marker
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified owner start: expected 403, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, ownerIdentityPath, nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous delete: expected 403, got %d", rec.Code)
	}
}

func TestOwnerIdentity_ConnectFlow(t *testing.T) {
	s, store, f := ownerIdentityServer(t)

	rec := doOwnerGet(s, ownerIdentityPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["connected"] != false || got["scope_required"] != github.OwnerIdentityScope || got["private_repos_supported"] != false || got["reason"] != github.OwnerIdentityNotConnected {
		t.Fatalf("disconnected GET = %v", got)
	}
	if strings.Contains(rec.Body.String(), "token") && !strings.Contains(rec.Body.String(), "github-owner-token") {
		t.Fatalf("GET must never carry a token: %s", rec.Body.String())
	}

	// Poll before start: refused.
	if rec := doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("poll before start: %d", rec.Code)
	}

	rec = doOwnerPostAsBody(s, ownerIdentityPath+"/start", "nate-double-u", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if f.startScope != github.OwnerIdentityScope {
		t.Fatalf("start asked GitHub for scope %q, want %q", f.startScope, github.OwnerIdentityScope)
	}
	started := decodeMap(t, rec)
	flowID, _ := started["flow_id"].(string)
	if flowID == "" || started["user_code"] != "ABCD-1234" || started["verification_uri"] != "https://github.com/login/device" || started["scope"] != github.OwnerIdentityScope {
		t.Fatalf("start body = %v", started)
	}

	// Wrong flow_id: refused, flow intact.
	if rec := doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": "stolen"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong flow_id: %d", rec.Code)
	}
	// Pending and slow_down are not terminal.
	f.status = "authorization_pending"
	if m := decodeMap(t, doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": flowID})); m["status"] != "pending" {
		t.Fatalf("pending poll = %v", m)
	}
	f.status = "slow_down"
	if m := decodeMap(t, doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": flowID})); m["status"] != "slow_down" {
		t.Fatalf("slow_down poll = %v", m)
	}
	f.status = "complete"
	rec = doOwnerPostAsBody(s, ownerIdentityPath+"/poll", "nate-double-u", map[string]any{"flow_id": flowID})
	m := decodeMap(t, rec)
	if m["status"] != "complete" || m["login"] != "nate-double-u" {
		t.Fatalf("complete poll = %v", m)
	}
	if strings.Contains(rec.Body.String(), "gho_owner_token") {
		t.Fatalf("the token must never be echoed: %s", rec.Body.String())
	}
	if f.changed != 1 {
		t.Fatalf("OwnerIdentityChanged ran %d times, want 1", f.changed)
	}
	st := store.Status()
	if !st.Connected || !st.Usable || st.Login != "nate-double-u" || st.Scope != github.OwnerIdentityScope {
		t.Fatalf("stored status = %+v", st)
	}
	if tok, ok := store.Token(); !ok || tok != "gho_owner_token" {
		t.Fatalf("stored token = %q,%v", tok, ok)
	}
	// The flow is spent: polling again needs a new start.
	if rec := doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": flowID}); rec.Code != http.StatusBadRequest {
		t.Fatalf("spent flow: %d", rec.Code)
	}
	got = decodeMap(t, doOwnerGet(s, ownerIdentityPath))
	if got["connected"] != true || got["usable"] != true || got["login"] != "nate-double-u" {
		t.Fatalf("connected GET = %v", got)
	}

	// Disconnect removes the token and re-resolves.
	rec = doOwnerDelete(s, ownerIdentityPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if m := decodeMap(t, rec); m["status"] != "disconnected" || m["login"] != "nate-double-u" || !strings.Contains(m["note"].(string), "github.com") {
		t.Fatalf("delete body = %v", m)
	}
	if _, ok := store.Token(); ok {
		t.Fatal("token must be gone after disconnect")
	}
	if f.changed != 2 {
		t.Fatalf("OwnerIdentityChanged ran %d times, want 2", f.changed)
	}
}

func TestOwnerIdentity_Rejections(t *testing.T) {
	start := func(t *testing.T, s *Server, user string) string {
		t.Helper()
		rec := doOwnerPostAsBody(s, ownerIdentityPath+"/start", user, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
		}
		return decodeMap(t, rec)["flow_id"].(string)
	}
	assertRejected := func(t *testing.T, s *Server, store *github.OwnerIdentityStore, f *ownerIdentityFakes, flowID, user, want string) {
		t.Helper()
		m := decodeMap(t, doOwnerPostAsBody(s, ownerIdentityPath+"/poll", user, map[string]any{"flow_id": flowID}))
		if m["status"] != "error" || !strings.Contains(m["error"].(string), want) {
			t.Fatalf("poll = %v, want error naming %q", m, want)
		}
		if store.Status().Connected {
			t.Fatal("a rejected consent must store nothing")
		}
		if f.changed != 0 {
			t.Fatal("a rejected consent must not re-resolve policies")
		}
	}

	t.Run("scope too narrow", func(t *testing.T) {
		s, store, f := ownerIdentityServer(t)
		f.scope = "read:user"
		assertRejected(t, s, store, f, start(t, s, "nate-double-u"), "nate-double-u", github.OwnerIdentityScope)
	})
	t.Run("someone else's account", func(t *testing.T) {
		s, store, f := ownerIdentityServer(t)
		f.login = "intruder"
		assertRejected(t, s, store, f, start(t, s, "nate-double-u"), "nate-double-u", "not the logged-in owner")
	})
	t.Run("token does not validate", func(t *testing.T) {
		s, store, f := ownerIdentityServer(t)
		f.validateErr = errors.New("boom")
		assertRejected(t, s, store, f, start(t, s, "nate-double-u"), "nate-double-u", "could not verify")
	})
	t.Run("github denies the flow", func(t *testing.T) {
		s, store, f := ownerIdentityServer(t)
		f.pollErr = errors.New("access_denied: the user cancelled")
		assertRejected(t, s, store, f, start(t, s, "nate-double-u"), "nate-double-u", "access_denied")
	})
	t.Run("wider repo scope is accepted", func(t *testing.T) {
		s, store, f := ownerIdentityServer(t)
		f.scope = "repo"
		flowID := start(t, s, "nate-double-u")
		m := decodeMap(t, doOwnerPostAsBody(s, ownerIdentityPath+"/poll", "nate-double-u", map[string]any{"flow_id": flowID}))
		if m["status"] != "complete" || !store.Status().Connected {
			t.Fatalf("repo scope must satisfy public_repo: %v", m)
		}
	})
	t.Run("local caller has no user to match", func(t *testing.T) {
		s, store, _ := ownerIdentityServer(t)
		flowID := start(t, s, "")
		m := decodeMap(t, doOwnerPost(s, ownerIdentityPath+"/poll", map[string]any{"flow_id": flowID}))
		if m["status"] != "complete" || !store.Status().Connected {
			t.Fatalf("an unattributed local owner may connect: %v", m)
		}
	})
	t.Run("start failure is a 502", func(t *testing.T) {
		s, _, f := ownerIdentityServer(t)
		f.startErr = errors.New("device code request returned 503")
		if rec := doOwnerPost(s, ownerIdentityPath+"/start", nil); rec.Code != http.StatusBadGateway {
			t.Fatalf("start failure: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// The login device flow and its scope-free contract are untouched: the
// consent lives in its own file, which the #1927 guard does not and must not
// have to scan, and the two flows keep separate state.
func TestOwnerIdentity_SeparateFromLoginFlow(t *testing.T) {
	s, _, _ := ownerIdentityServer(t)
	rec := doOwnerPostAsBody(s, ownerIdentityPath+"/start", "nate-double-u", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d", rec.Code)
	}
	s.deviceFlowMu.Lock()
	loginFlow := s.deviceFlowState
	s.deviceFlowMu.Unlock()
	if loginFlow != nil {
		t.Fatal("the owner consent must not touch the login device-flow state")
	}
	s.ownerIdentityFlowMu.Lock()
	flow := s.ownerIdentityFlow
	s.ownerIdentityFlowMu.Unlock()
	if flow == nil || flow.startedBy != "nate-double-u" {
		t.Fatalf("consent flow = %+v", flow)
	}
}
