package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookStub is a minimal GitHub hooks API: GET/POST on /orgs/{org}/hooks and
// /repos/{owner}/{repo}/hooks, keeping created hooks per path.
type hookStub struct {
	mu      sync.Mutex
	hooks   map[string][]map[string]any
	posts   map[string]int
	nextID  int64
	lastReq map[string]any
}

func newHookStub() *hookStub {
	return &hookStub{hooks: map[string][]map[string]any{}, posts: map[string]int{}, nextID: 100}
}

func (s *hookStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimSuffix(r.URL.Path, "/")
	if !strings.HasSuffix(path, "/hooks") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		list := s.hooks[path]
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)
	case http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.lastReq = body
		s.posts[path]++
		s.nextID++
		hook := map[string]any{
			"id":     s.nextID,
			"active": body["active"],
			"events": body["events"],
			"config": body["config"],
		}
		s.hooks[path] = append(s.hooks[path], hook)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(hook)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *hookStub) postCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts[path]
}

func (s *hookStub) lastBody() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReq
}

func hookStubAuth(t *testing.T, stub http.Handler) *AppAuth {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return &AppAuth{
		cachedToken:    "ghs_hook_stub",
		tokenExpiry:    time.Now().Add(2 * time.Hour),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		installationID: 1,
		apiURL:         srv.URL + "/",
	}
}

func TestCreateWebhook_OrgCreatesThenReturnsExisting(t *testing.T) {
	stub := newHookStub()
	auth := hookStubAuth(t, stub)
	req := WebhookCreateRequest{Org: "acme", PayloadURL: "https://hive.example.org/api/webhook/github", Secret: "s3cret"}

	res, err := auth.CreateWebhook(context.Background(), req)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !res.Created || res.ID == 0 || res.ID != res.HookID || res.Scope != "organization" || res.Target != "acme" {
		t.Fatalf("first result = %+v", res)
	}
	if len(res.Events) != len(WebhookEvents) {
		t.Fatalf("events = %v, want default WebhookEvents", res.Events)
	}
	body := stub.lastBody()
	cfg, _ := body["config"].(map[string]any)
	if cfg["url"] != req.PayloadURL || cfg["content_type"] != "json" || cfg["secret"] != "s3cret" || cfg["insecure_ssl"] != "0" || body["active"] != true {
		t.Fatalf("create body = %v", body)
	}

	again, err := auth.CreateWebhook(context.Background(), req)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if again.Created || again.ID != res.ID {
		t.Fatalf("second result = %+v, want existing hook %d", again, res.ID)
	}
	if n := stub.postCount("/orgs/acme/hooks"); n != 1 {
		t.Fatalf("POST count = %d, want 1 (no duplicate)", n)
	}
}

func TestCreateWebhook_RepoScopeHitsRepoEndpoint(t *testing.T) {
	stub := newHookStub()
	auth := hookStubAuth(t, stub)
	res, err := auth.CreateWebhook(context.Background(), WebhookCreateRequest{
		Org: "acme", Repo: "widgets", PayloadURL: "https://hive.example.org/api/webhook/github", Secret: "s3cret",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stub.postCount("/repos/acme/widgets/hooks") != 1 || stub.postCount("/orgs/acme/hooks") != 0 {
		t.Fatal("want exactly one POST, to the repo endpoint")
	}
	if !res.Created || res.Scope != "repository" || res.Target != "acme/widgets" || !strings.HasSuffix(res.URL, "/acme/widgets/settings/hooks/101") {
		t.Fatalf("result = %+v", res)
	}
}

// A hook for another URL on the same target does not count as existing.
func TestCreateWebhook_OtherURLDoesNotMatch(t *testing.T) {
	stub := newHookStub()
	stub.hooks["/orgs/acme/hooks"] = []map[string]any{{"id": 7, "config": map[string]any{"url": "https://ci.example.org/hook"}}}
	auth := hookStubAuth(t, stub)
	res, err := auth.CreateWebhook(context.Background(), WebhookCreateRequest{Org: "acme", PayloadURL: "https://hive.example.org/api/webhook/github", Secret: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Created || res.ID == 7 {
		t.Fatalf("result = %+v, want a new hook", res)
	}
}
