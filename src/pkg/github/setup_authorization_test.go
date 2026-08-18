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

	gh "github.com/google/go-github/v72/github"
)

const (
	setupAuthorizationTestHead    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	setupAuthorizationTestContext = "hive/setup-authorized/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestAuthenticatedNumericUserAcceptsOnlyHumanNumericIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "human", body: `{"id":456,"login":"setup-operator","type":"User"}`},
		{name: "actions bot", body: `{"id":41898282,"login":"github-actions[bot]","type":"Bot"}`, wantErr: true},
		{name: "missing id", body: `{"login":"setup-operator","type":"User"}`, wantErr: true},
		{name: "missing type", body: `{"id":456,"login":"setup-operator"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/user" {
					http.Error(writer, "unexpected request", http.StatusNotFound)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := NewClientForTest(server.URL, "", nil, slog.Default())
			identity, err := client.AuthenticatedNumericUser(context.Background())
			if test.wantErr {
				if err == nil {
					t.Fatalf("identity %+v was accepted", identity)
				}
				return
			}
			if err != nil || identity.ID != 456 || identity.Login != "setup-operator" || identity.Type != "User" {
				t.Fatalf("identity = %+v, err %v", identity, err)
			}
		})
	}
}

func TestResolveHumanNumericUserRequiresExactNamedHuman(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "exact human", body: `{"id":789,"login":"next-operator","type":"User"}`},
		{name: "different login", body: `{"id":789,"login":"renamed-operator","type":"User"}`, wantErr: true},
		{name: "bot", body: `{"id":789,"login":"next-operator","type":"Bot"}`, wantErr: true},
		{name: "missing numeric id", body: `{"login":"next-operator","type":"User"}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/users/next-operator" {
					http.Error(writer, "unexpected request", http.StatusNotFound)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := NewClientForTest(server.URL, "", nil, slog.Default())
			identity, err := client.ResolveHumanNumericUser(context.Background(), "next-operator")
			if test.wantErr {
				if err == nil {
					t.Fatalf("identity %+v was accepted", identity)
				}
				return
			}
			if err != nil || identity.ID != 789 || identity.Login != "next-operator" || identity.Type != "User" {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
		})
	}
}

func TestVerifySetupAuthorizationStatusIsReadOnlyAndExact(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	harness.statuses = []*gh.RepoStatus{setupAuthorizationRepoStatus(77, 456, "setup-operator", "User", "success")}
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.VerifySetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusID != 77 || result.CreatorID != 456 || !result.Reused {
		t.Fatalf("verification result=%+v", result)
	}
	if harness.posts != 0 || harness.lists != 1 {
		t.Fatalf("read-only verification mutated status APIs: posts=%d lists=%d", harness.posts, harness.lists)
	}
	harness.statuses[0].Creator.ID = gh.Ptr(int64(999))
	if _, err := client.VerifySetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest()); err == nil || !strings.Contains(err.Error(), "creator") {
		t.Fatalf("foreign status was accepted: %v", err)
	}
}

func TestEnsureSetupAuthorizationStatusCreatesAndReadsBackExactLatestStatus(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusID != 1001 || result.Context != setupAuthorizationTestContext || result.CreatorID != 456 || result.CreatorLogin != "setup-operator" || result.Reused || result.Recovered {
		t.Fatalf("status result = %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.posts != 1 || harness.lists != 2 || harness.postedHead != setupAuthorizationTestHead {
		t.Fatalf("API calls posts=%d lists=%d head=%q", harness.posts, harness.lists, harness.postedHead)
	}
	if harness.posted.GetState() != "success" || harness.posted.GetContext() != setupAuthorizationTestContext || harness.posted.GetTargetURL() != "https://github.test/owner/repo/pull/7" {
		t.Fatalf("posted status = %+v", harness.posted)
	}
}

func TestEnsureSetupAuthorizationStatusReusesLatestExactHumanStatus(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	harness.statuses = []*gh.RepoStatus{setupAuthorizationRepoStatus(77, 456, "setup-operator", "User", "success")}
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Recovered || result.StatusID != 77 {
		t.Fatalf("reused result = %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.posts != 0 || harness.lists != 1 {
		t.Fatalf("reused status unexpectedly mutated API: posts=%d lists=%d", harness.posts, harness.lists)
	}
}

func TestEnsureSetupAuthorizationStatusSupersedesWrongTargetURL(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	wrong := setupAuthorizationRepoStatus(77, 456, "setup-operator", "User", "success")
	wrong.TargetURL = gh.Ptr("https://github.test/owner/repo/pull/99")
	harness.statuses = []*gh.RepoStatus{wrong}
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Reused || result.StatusID != 1001 || harness.posts != 1 {
		t.Fatalf("wrong-target status was reused instead of superseded: result=%+v posts=%d", result, harness.posts)
	}
}

func TestEnsureSetupAuthorizationStatusSupersedesForeignLatestStatus(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	// List order is newest first. An older valid status must not mask the
	// foreign latest claim; Ensure creates a new exact operator status.
	harness.statuses = []*gh.RepoStatus{
		setupAuthorizationRepoStatus(88, 999, "foreign", "User", "success"),
		setupAuthorizationRepoStatus(77, 456, "setup-operator", "User", "success"),
	}
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Reused || result.Recovered || result.StatusID != 1001 {
		t.Fatalf("superseding result = %+v", result)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.posts != 1 || len(harness.statuses) < 1 || harness.statuses[0].GetID() != 1001 {
		t.Fatalf("foreign latest status was not superseded: posts=%d statuses=%+v", harness.posts, harness.statuses)
	}
}

func TestEnsureSetupAuthorizationStatusRecoversAmbiguousCreate(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	harness.ambiguousCreate = true
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	result, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Reused || !result.Recovered || result.StatusID != 1001 {
		t.Fatalf("ambiguous create recovery = %+v", result)
	}
}

func TestEnsureSetupAuthorizationStatusRejectsForeignReadbackRace(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	harness.foreignReadback = true
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	_, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err == nil || !strings.Contains(err.Error(), "creator") {
		t.Fatalf("foreign latest readback was accepted: %v", err)
	}
}

func TestEnsureSetupAuthorizationStatusRejectsTokenIdentityDriftBeforeMutation(t *testing.T) {
	harness := newSetupAuthorizationStatusHarness(t)
	harness.userID = 457
	defer harness.server.Close()
	client := NewClientForTest(harness.server.URL, "", nil, slog.Default())

	_, err := client.EnsureSetupAuthorizationStatus(context.Background(), setupAuthorizationStatusRequest())
	if err == nil || !strings.Contains(err.Error(), "does not match workflow-bound ID") {
		t.Fatalf("identity drift was accepted: %v", err)
	}
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if harness.posts != 0 || harness.lists != 0 {
		t.Fatalf("identity drift touched status APIs: posts=%d lists=%d", harness.posts, harness.lists)
	}
}

type setupAuthorizationStatusHarness struct {
	t               *testing.T
	server          *httptest.Server
	mu              sync.Mutex
	statuses        []*gh.RepoStatus
	posted          *gh.RepoStatus
	postedHead      string
	posts           int
	lists           int
	userID          int64
	ambiguousCreate bool
	foreignReadback bool
}

func newSetupAuthorizationStatusHarness(t *testing.T) *setupAuthorizationStatusHarness {
	t.Helper()
	harness := &setupAuthorizationStatusHarness{t: t, userID: 456}
	harness.server = httptest.NewServer(http.HandlerFunc(harness.serveHTTP))
	return harness
}

func (harness *setupAuthorizationStatusHarness) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/user":
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": harness.userID, "login": "setup-operator", "type": "User"})
	case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/"+setupAuthorizationTestHead+"/statuses":
		harness.mu.Lock()
		harness.lists++
		statuses := append([]*gh.RepoStatus(nil), harness.statuses...)
		harness.mu.Unlock()
		_ = json.NewEncoder(writer).Encode(statuses)
	case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/statuses/"+setupAuthorizationTestHead:
		var input gh.RepoStatus
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		harness.mu.Lock()
		harness.posts++
		harness.postedHead = setupAuthorizationTestHead
		harness.posted = &input
		created := setupAuthorizationRepoStatus(1001, 456, "setup-operator", "User", "success")
		created.TargetURL, created.Description = input.TargetURL, input.Description
		if harness.foreignReadback {
			harness.statuses = []*gh.RepoStatus{setupAuthorizationRepoStatus(1002, 999, "foreign", "User", "success"), created}
		} else {
			harness.statuses = append([]*gh.RepoStatus{created}, harness.statuses...)
		}
		ambiguous := harness.ambiguousCreate
		harness.mu.Unlock()
		if ambiguous {
			http.Error(writer, `{"message":"transport response lost"}`, http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(created)
	default:
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}
}

func setupAuthorizationStatusRequest() SetupAuthorizationStatusRequest {
	return SetupAuthorizationStatusRequest{
		Repository: "owner/repo", HeadSHA: setupAuthorizationTestHead, Context: setupAuthorizationTestContext,
		TargetURL: "https://github.test/owner/repo/pull/7", Description: "Hive-authorized exact setup pull request #7", ExpectedCreatorID: 456,
	}
}

func setupAuthorizationRepoStatus(id, creatorID int64, login, userType, state string) *gh.RepoStatus {
	created := gh.Timestamp{Time: time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)}
	return &gh.RepoStatus{
		ID: gh.Ptr(id), Context: gh.Ptr(setupAuthorizationTestContext), State: gh.Ptr(state), CreatedAt: &created,
		TargetURL: gh.Ptr("https://github.test/owner/repo/pull/7"),
		Creator:   &gh.User{ID: gh.Ptr(creatorID), Login: gh.Ptr(login), Type: gh.Ptr(userType)},
	}
}
