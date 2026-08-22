package linearagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// fakeLinearAPI is a recording fake for api.linear.app: it captures every
// GraphQL request (query, variables, auth header) and serves canned data.
type fakeLinearAPI struct {
	srv      *httptest.Server
	requests []fakeGraphQLRequest
	// respond, when set, overrides the default success response.
	respond func(w http.ResponseWriter, req fakeGraphQLRequest)
}

type fakeGraphQLRequest struct {
	Authorization string
	Query         string
	Variables     map[string]interface{}
}

func newFakeLinearAPI(t *testing.T) *fakeLinearAPI {
	t.Helper()
	f := &fakeLinearAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("fake linear: bad body: %v", err)
		}
		req := fakeGraphQLRequest{
			Authorization: r.Header.Get("Authorization"),
			Query:         body.Query,
			Variables:     body.Variables,
		}
		f.requests = append(f.requests, req)
		if f.respond != nil {
			f.respond(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "agentActivityCreate"):
			w.Write([]byte(`{"data":{"agentActivityCreate":{"success":true}}}`))
		case strings.Contains(body.Query, "viewer"):
			w.Write([]byte(`{"data":{"viewer":{"id":"viewer-9"},"organization":{"id":"org-9","name":"Acme","urlKey":"acme"}}}`))
		default:
			w.Write([]byte(`{"data":{}}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestClient(t *testing.T, api *fakeLinearAPI, tokenURL string, tok Token) (*Client, *Store) {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "l.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	inst := testInstall()
	inst.Token = tok
	if err := store.Set(inst); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c := NewClient(store, Credentials{ClientID: "cid", ClientSecret: "cs"}, api.srv.Client(), tokenURL, api.srv.URL, quietLogger())
	return c, store
}

func TestClient_CreateActivity(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})

	err := c.CreateActivity(context.Background(), "sess-1", ActivityContent{Type: ActivityThought, Body: "hello"})
	if err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	req := api.requests[0]
	if req.Authorization != "Bearer at" {
		t.Errorf("auth = %q", req.Authorization)
	}
	if !strings.Contains(req.Query, "agentActivityCreate") {
		t.Errorf("query = %q", req.Query)
	}
	input := req.Variables["input"].(map[string]interface{})
	if input["agentSessionId"] != "sess-1" {
		t.Errorf("session id = %v", input["agentSessionId"])
	}
	content := input["content"].(map[string]interface{})
	if content["type"] != "thought" || content["body"] != "hello" {
		t.Errorf("content = %v", content)
	}
	if _, present := content["action"]; present {
		t.Errorf("empty fields must be omitted: %v", content)
	}
}

func TestClient_CreateActivity_TruncatesBody(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})
	long := strings.Repeat("x", activityBodyLimit+100)
	if err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityResponse, Body: long}); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	body := api.requests[0].Variables["input"].(map[string]interface{})["content"].(map[string]interface{})["body"].(string)
	if len([]rune(body)) != activityBodyLimit+1 { // +1 for the ellipsis
		t.Errorf("body length = %d", len([]rune(body)))
	}
}

func TestClient_CreateActivity_ReportedFailure(t *testing.T) {
	api := newFakeLinearAPI(t)
	api.respond = func(w http.ResponseWriter, _ fakeGraphQLRequest) {
		w.Write([]byte(`{"data":{"agentActivityCreate":{"success":false}}}`))
	}
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})
	if err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"}); err == nil {
		t.Fatal("success=false must error")
	}
}

func TestClient_CreateActivity_GraphQLError(t *testing.T) {
	api := newFakeLinearAPI(t)
	api.respond = func(w http.ResponseWriter, _ fakeGraphQLRequest) {
		w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})
	err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_CreateActivity_HTTPError(t *testing.T) {
	api := newFakeLinearAPI(t)
	api.respond = func(w http.ResponseWriter, _ fakeGraphQLRequest) {
		w.WriteHeader(http.StatusBadGateway)
	}
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)})
	err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_RefreshesExpiredToken(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	c, store := newTestClient(t, api, tokens.srv.URL,
		Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Minute)})

	if err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"}); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	if len(tokens.forms) != 1 || tokens.forms[0].Get("grant_type") != "refresh_token" || tokens.forms[0].Get("refresh_token") != "rt-old" {
		t.Fatalf("refresh forms = %v", tokens.forms)
	}
	if api.requests[0].Authorization != "Bearer at-1" {
		t.Errorf("activity used %q, want refreshed token", api.requests[0].Authorization)
	}
	// The refreshed grant is persisted.
	inst, _ := store.Get()
	if inst.Token.AccessToken != "at-1" || inst.Token.RefreshToken != "rt-1" {
		t.Errorf("persisted token = %+v", inst.Token)
	}
}

func TestClient_RefreshKeepsOldRefreshTokenWhenAbsent(t *testing.T) {
	api := newFakeLinearAPI(t)
	tokens := newFakeTokenServer(t)
	tokens.respond = func(w http.ResponseWriter, _ url.Values) {
		json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "at-2", "expires_in": 100})
	}
	c, store := newTestClient(t, api, tokens.srv.URL,
		Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Minute)})
	if err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"}); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	inst, _ := store.Get()
	if inst.Token.RefreshToken != "rt-old" {
		t.Errorf("refresh token = %q, want the old one kept", inst.Token.RefreshToken)
	}
}

func TestClient_NotInstalled(t *testing.T) {
	api := newFakeLinearAPI(t)
	store, _ := NewStore(filepath.Join(t.TempDir(), "l.json"))
	c := NewClient(store, Credentials{}, api.srv.Client(), "", api.srv.URL, quietLogger())
	err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_ExpiredWithoutRefreshToken(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at", ExpiresAt: time.Now().Add(-time.Minute)})
	err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought})
	if err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("err = %v", err)
	}
}

func TestClient_NoExpiryMeansNoRefresh(t *testing.T) {
	api := newFakeLinearAPI(t)
	c, _ := newTestClient(t, api, "", Token{AccessToken: "at-forever"})
	if err := c.CreateActivity(context.Background(), "s", ActivityContent{Type: ActivityThought, Body: "b"}); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	if api.requests[0].Authorization != "Bearer at-forever" {
		t.Errorf("auth = %q", api.requests[0].Authorization)
	}
}

func TestFetchIdentity(t *testing.T) {
	api := newFakeLinearAPI(t)
	id, err := FetchIdentity(context.Background(), api.srv.Client(), api.srv.URL, "at-x")
	if err != nil {
		t.Fatalf("FetchIdentity: %v", err)
	}
	if id.ViewerID != "viewer-9" || id.OrganizationID != "org-9" || id.OrganizationName != "Acme" || id.OrganizationURLKey != "acme" {
		t.Errorf("identity = %+v", id)
	}
	if api.requests[0].Authorization != "Bearer at-x" {
		t.Errorf("auth = %q", api.requests[0].Authorization)
	}
}

func TestFetchIdentity_NoViewer(t *testing.T) {
	api := newFakeLinearAPI(t)
	api.respond = func(w http.ResponseWriter, _ fakeGraphQLRequest) {
		w.Write([]byte(`{"data":{"viewer":{}}}`))
	}
	if _, err := FetchIdentity(context.Background(), api.srv.Client(), api.srv.URL, "at"); err == nil {
		t.Fatal("empty viewer must error")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 10); got != "héllo" {
		t.Errorf("short = %q", got)
	}
	if got := truncate("héllo", 2); got != "hé…" {
		t.Errorf("cut = %q", got)
	}
}
