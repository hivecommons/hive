package hub

// Tests for the auth-health down-transition notification (#6558,
// auth_health_notify.go): the hive owner gets exactly one Slack DM on the
// ok/degraded -> down edge, reusing the same notification plumbing
// notifyOwnerAccessRequest (access_notify.go) already exercises, and a hive
// that stays down for many heartbeats in a row does not re-notify.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pastAuthHealthThreshold formats a BackendAuthSince comfortably older than
// AuthHealthDownThreshold, so a heartbeat reporting it lands on "down"
// immediately rather than "degraded".
func pastAuthHealthThreshold() string {
	return time.Now().Add(-AuthHealthDownThreshold() - time.Hour).UTC().Format(time.RFC3339)
}

func allAgentsUnlicensedBody(hiveID, since string) string {
	return `{"hive_id":"` + hiveID + `","agents":[
		{"name":"scanner","state":"running","enabled":true,
		 "backendAuthStatus":"unlicensed","backendAuthSince":"` + since + `"},
		{"name":"outreach","state":"running","enabled":true,
		 "backendAuthStatus":"unlicensed","backendAuthSince":"` + since + `"}
	]}`
}

func TestHandleHeartbeat_AuthHealthDownTransitionNotifiesOwnerOnce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	got := make(chan map[string]string, 4)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	t.Setenv(slackTokenEnvVar, "xoxb-test")
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.example.com")

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", SlackID: "U0WNER", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	s := newHeartbeatHub()
	since := pastAuthHealthThreshold()

	// First heartbeat: every enabled agent already unlicensed past the
	// threshold — a fresh restart inheriting an already-broken credential
	// must still notify on its FIRST beat (hadPrev is false).
	rec := postHeartbeat(t, s, allAgentsUnlicensedBody("h1", since))
	if rec.Code != http.StatusOK {
		t.Fatalf("first heartbeat status = %d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case msg := <-got:
		if msg["channel"] != "U0WNER" {
			t.Errorf("DM went to channel %q, want owner's slack_id U0WNER", msg["channel"])
		}
		text := msg["text"]
		if !containsAll(text, "h1", "failed backend auth", "https://hub.example.com/dashboard?manage_access=h1") {
			t.Errorf("notification %q missing expected content", text)
		}
	case <-time.After(slackNotifyWaitTimeout):
		t.Fatal("owner was never notified on the ok/degraded -> down transition")
	}

	// Second and third heartbeats: hive STAYS down. Must not re-notify.
	for i := 0; i < 2; i++ {
		rec = postHeartbeat(t, s, allAgentsUnlicensedBody("h1", since))
		if rec.Code != http.StatusOK {
			t.Fatalf("repeat heartbeat status = %d", rec.Code)
		}
	}
	select {
	case msg := <-got:
		t.Errorf("hive stayed down but was re-notified: %v", msg)
	case <-time.After(200 * time.Millisecond):
		// Correct: exactly one notification for the whole down episode.
	}

	entry := lookupRegistryEntry(t, s, "h1")
	if entry.AuthHealth != AuthHealthDown {
		t.Fatalf("stored AuthHealth = %q, want %q", entry.AuthHealth, AuthHealthDown)
	}
}

// TestHandleHeartbeat_AuthHealthRecoveryThenSecondOutageNotifiesAgain verifies
// the notification is keyed off a real EDGE, not a one-shot latch: a hive
// that recovers to ok and later fails again gets a second, independent
// notification.
func TestHandleHeartbeat_AuthHealthRecoveryThenSecondOutageNotifiesAgain(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	got := make(chan struct{}, 4)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		got <- struct{}{}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	t.Setenv(slackTokenEnvVar, "xoxb-test")

	saveSaaSUser(&SaaSUser{GitHubUsername: "owner", SlackID: "U0WNER", Hives: map[string]string{"h1": "owner"}})
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "owner"})

	s := newHeartbeatHub()
	since := pastAuthHealthThreshold()

	postHeartbeat(t, s, allAgentsUnlicensedBody("h1", since))
	select {
	case <-got:
	case <-time.After(slackNotifyWaitTimeout):
		t.Fatal("first outage never notified")
	}

	// Recovers to ok.
	okBody := `{"hive_id":"h1","agents":[
		{"name":"scanner","state":"running","enabled":true},
		{"name":"outreach","state":"running","enabled":true}
	]}`
	postHeartbeat(t, s, okBody)
	select {
	case <-got:
		t.Fatal("recovery beat must not notify")
	case <-time.After(200 * time.Millisecond):
	}

	// Fails again, independently.
	postHeartbeat(t, s, allAgentsUnlicensedBody("h1", since))
	select {
	case <-got:
	case <-time.After(slackNotifyWaitTimeout):
		t.Fatal("second, independent outage never notified")
	}
}

// TestNotifyOwnerAuthHealthDown_SkipsSilentlyWhenUnconfigured mirrors
// TestRequestAccessSucceedsWhenNotificationUnconfigured: a hive with no owner,
// no bot token, or an owner with no slack_id must never panic or block the
// heartbeat — it just logs and moves on.
func TestNotifyOwnerAuthHealthDown_SkipsSilentlyWhenUnconfigured(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	sent := make(chan struct{}, 1)
	withFakeSlackEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		sent <- struct{}{}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	s := newHeartbeatHub()

	// No owner on the entry at all.
	s.notifyOwnerAuthHealthDown(RegistryEntry{ID: "h-noowner", AuthHealthReason: "2 of 2 enabled agents are failing backend auth"})

	// Owner set, but no Slack token configured.
	t.Setenv(slackTokenEnvVar, "")
	s.notifyOwnerAuthHealthDown(RegistryEntry{ID: "h-notoken", Owner: "owner", AuthHealthReason: "x"})

	// Token configured, but owner has no user record / slack_id.
	t.Setenv(slackTokenEnvVar, "xoxb-test")
	s.notifyOwnerAuthHealthDown(RegistryEntry{ID: "h-nouser", Owner: "ghost-owner", AuthHealthReason: "x"})

	select {
	case <-sent:
		t.Error("a DM was sent despite every prerequisite being unconfigured")
	case <-time.After(200 * time.Millisecond):
	}
}

func lookupRegistryEntry(t *testing.T, s *HubServer, id string) RegistryEntry {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return RegistryEntry{}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
