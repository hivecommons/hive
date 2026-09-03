package hub

// Tests for the owner notification on a new access request (issue #4149,
// access_notify.go): the Slack DM goes out exactly once per new pending
// request, carries the requester username and the one-click review link, and
// every configured-but-unreachable state degrades to a logged skip rather than
// an error surfaced to the requester.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slackNotifyWaitTimeout bounds how long a test waits for the background
// deliverSlackMessages goroutine to hit the fake endpoint.
const slackNotifyWaitTimeout = 2 * time.Second

func TestRequestAccessNotifiesOwnerViaSlack(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	got := make(chan map[string]string, 1)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		got <- body
		io.WriteString(w, `{"ok":true}`)
	})
	t.Setenv(slackTokenEnvVar, "xoxb-test")
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", SlackID: "U0WNER", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	mkUser(t, "requester")

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"need to debug"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request access status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case msg := <-got:
		if msg["channel"] != "U0WNER" {
			t.Errorf("DM went to channel %q, want the owner's slack_id U0WNER", msg["channel"])
		}
		text := msg["text"]
		if !strings.Contains(text, "requester") {
			t.Errorf("notification %q does not name the requester", text)
		}
		if !strings.Contains(text, "https://hub.example.com/dashboard?manage_access=h1") {
			t.Errorf("notification %q does not carry the one-click review link", text)
		}
		if !strings.Contains(text, "need to debug") {
			t.Errorf("notification %q does not carry the requester's note", text)
		}
	case <-time.After(slackNotifyWaitTimeout):
		t.Fatal("owner was never notified: no Slack DM reached the endpoint")
	}
}

// A duplicate pending request is rejected before anything is saved, so it must
// also never re-notify — that rejection IS the anti-spam deduplication.
func TestRequestAccessDoesNotRenotifyWhilePending(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	got := make(chan struct{}, 4)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		io.WriteString(w, `{"ok":true}`)
	})
	t.Setenv(slackTokenEnvVar, "xoxb-test")

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", SlackID: "U0WNER", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	mkUser(t, "requester")

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"first"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d", rec.Code)
	}
	select {
	case <-got:
	case <-time.After(slackNotifyWaitTimeout):
		t.Fatal("first request never notified")
	}

	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"again"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate request status = %d, want 400", rec.Code)
	}
	select {
	case <-got:
		t.Error("duplicate pending request re-notified the owner — notification spam")
	case <-time.After(100 * time.Millisecond):
		// Correct: no second DM.
	}
}

// Missing config never breaks the request itself: no bot token and no
// slack_id both mean the request is stored and 200-OK'd, just without a DM.
func TestRequestAccessSucceedsWhenNotificationUnconfigured(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	sent := make(chan struct{}, 1)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		sent <- struct{}{}
		io.WriteString(w, `{"ok":true}`)
	})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})
	mkUser(t, "requester")

	// No token configured.
	t.Setenv(slackTokenEnvVar, "")
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"x"}`, "requester"), "id", "h1")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request with no slack token status = %d, want 200", rec.Code)
	}

	// Token configured but owner has no slack_id (and no user record at all).
	t.Setenv(slackTokenEnvVar, "xoxb-test")
	saveSaaSHive(&SaaSHive{ID: "h2", Owner: "owner"})
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser(http.MethodPost, "/req", `{"note":"x"}`, "requester"), "id", "h2")
	s.handleRequestAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request with unreachable owner status = %d, want 200", rec.Code)
	}

	select {
	case <-sent:
		t.Error("a DM was sent despite the notification path being unconfigured")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAccessRequestReviewLinkEscapesHiveID(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com/")
	link := accessRequestReviewLink("my hive&x")
	want := "https://hub.example.com/dashboard?manage_access=my+hive%26x"
	if link != want {
		t.Errorf("link = %q, want %q", link, want)
	}
}

func TestHubDashboardBaseURLFallsBackToPublicURL(t *testing.T) {
	for _, key := range []string{"HIVE_HUB_PUBLIC_URL", "HIVE_PUBLIC_URL", "HIVE_HUB_BASE_URL", "HIVE_DASHBOARD_URL", "HIVE_HUB_URL"} {
		t.Setenv(key, "")
	}
	if got := hubDashboardBaseURL(); got != hubPublicURL() {
		t.Errorf("base URL = %q, want fallback %q", got, hubPublicURL())
	}
}
