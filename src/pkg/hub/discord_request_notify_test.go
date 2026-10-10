package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const discordRequestNotifyWaitTimeout = 2 * time.Second

func withFastDiscordRequestRetries(t *testing.T) {
	t.Helper()
	oldBackoff := discordRequestBackoff
	discordRequestBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { discordRequestBackoff = oldBackoff })
}

func TestRequestProvisionPostsDiscordWebhookWhenConfigured(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withFastDiscordRequestRetries(t)

	gotPayload := make(chan discordWebhookPayload, 1)
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("discord method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q, want application/json", ct)
		}
		var payload discordWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode discord payload: %v", err)
		}
		gotPayload <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	t.Setenv(discordRequestsWebhookEnv, discord.URL)
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")
	s := newHandlerHub()
	mkUser(t, "github:octocat")

	rec := httptest.NewRecorder()
	s.handleRequestProvision(rec, reqWithUser(http.MethodPost, requestProvisionPath, `{
		"org":"hivecommons",
		"github_host":"github.com",
		"repos":"hive",
		"primary_repo":"hive",
		"acmm_level":3,
		"auth_method":"public",
		"full_name":"Octo Cat",
		"slack_id":"U123",
		"country":"US"
	}`, "github:octocat"))
	if rec.Code != http.StatusOK {
		t.Fatalf("request provision status = %d body=%s", rec.Code, rec.Body.String())
	}

	var payload discordWebhookPayload
	select {
	case payload = <-gotPayload:
	case <-time.After(discordRequestNotifyWaitTimeout):
		t.Fatal("discord webhook was not called")
	}
	if !strings.Contains(payload.Content, "octocat") {
		t.Fatalf("content = %q, want requester", payload.Content)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(payload.Embeds))
	}
	embedJSON, _ := json.Marshal(payload.Embeds[0])
	for _, want := range []string{"New hive request", "hivecommons/hive", "L3", "github:octocat", "Octo Cat", "U123", "https://hub.example.com/dashboard"} {
		if !strings.Contains(string(embedJSON), want) {
			t.Fatalf("embed missing %q: %s", want, string(embedJSON))
		}
	}
}

func TestRequestProvisionDiscordWebhookUnsetIsNoop(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	var calls atomic.Int32
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	t.Setenv(discordRequestsWebhookEnv, "")
	s := newHandlerHub()
	mkUser(t, "github:noop")

	rec := httptest.NewRecorder()
	s.handleRequestProvision(rec, reqWithUser(http.MethodPost, requestProvisionPath, `{
		"org":"hivecommons",
		"github_host":"github.com",
		"repos":"hive",
		"full_name":"No Op"
	}`, "github:noop"))
	if rec.Code != http.StatusOK {
		t.Fatalf("request provision status = %d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("discord webhook called %d times with webhook unset", calls.Load())
	}
}

func TestRequestProvisionDiscordFailureDoesNotBlockAndIsRedacted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	withFastDiscordRequestRetries(t)

	calls := make(chan struct{}, discordRequestWebhookMaxAttempts)
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- struct{}{}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer discord.Close()

	t.Setenv(discordRequestsWebhookEnv, discord.URL)
	s := newHandlerHub()
	s.logger = slog.Default()
	mkUser(t, "github:failcat")

	rec := httptest.NewRecorder()
	s.handleRequestProvision(rec, reqWithUser(http.MethodPost, requestProvisionPath, `{
		"org":"hivecommons",
		"github_host":"github.com",
		"repos":"hive",
		"full_name":"Fail Cat"
	}`, "github:failcat"))
	if rec.Code != http.StatusOK {
		t.Fatalf("request provision status = %d body=%s", rec.Code, rec.Body.String())
	}

	for i := 0; i < discordRequestWebhookMaxAttempts; i++ {
		select {
		case <-calls:
		case <-time.After(discordRequestNotifyWaitTimeout):
			t.Fatalf("discord retry %d did not occur", i+1)
		}
	}
	waitForDiscordRequestFailures(t, s, 1)

	versionRec := httptest.NewRecorder()
	s.handleHubVersion(versionRec, httptest.NewRequest(http.MethodGet, "/api/hub/version", nil))
	if strings.Contains(versionRec.Body.String(), discord.URL) {
		t.Fatalf("status leaked discord webhook URL: %s", versionRec.Body.String())
	}
}

func waitForDiscordRequestFailures(t *testing.T, s *HubServer, want int64) {
	t.Helper()
	deadline := time.After(discordRequestNotifyWaitTimeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.discordRequests.mu.RLock()
		failures := s.discordRequests.FailureCount
		s.discordRequests.mu.RUnlock()
		if failures == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("failure count = %d, want %d", failures, want)
		}
	}
}

func TestBuildDiscordRequestPayloadTruncatesNotes(t *testing.T) {
	pr := &ProvisionRequest{
		Username:    "github:octocat",
		UserID:      "octocat",
		GitHubHost:  "github.com",
		Org:         "hivecommons",
		Repos:       "hive",
		PrimaryRepo: "hive",
		ACMMLevel:   3,
		FullName:    strings.Repeat("a", discordEmbedFieldValueLimit+100),
		RequestedAt: "2026-10-09T22:00:00Z",
		Status:      provisionStatusPending,
	}
	payload, err := buildDiscordRequestPayload(pr)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if len(payload.Content) > discordContentLimit {
		t.Fatalf("content length = %d, want <= %d", len(payload.Content), discordContentLimit)
	}
	for _, field := range payload.Embeds[0].Fields {
		if field.Name == "Notes" && len([]rune(field.Value)) > discordEmbedFieldValueLimit {
			t.Fatalf("notes length = %d, want <= %d", len([]rune(field.Value)), discordEmbedFieldValueLimit)
		}
	}
}
