package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSlackHiveOwnerRejectsUnknownHive locks the 404 for a hive id that does
// not exist: the caller must learn the target is wrong, not get a cheerful
// result for a message that can never be routed.
func TestSlackHiveOwnerRejectsUnknownHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"hello","dry_run":true}`,
		"/api/saas/hives/no-such-hive/slack", map[string]string{"id": "no-such-hive"},
		s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown hive = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestSlackHiveOwnerRejectsUnclaimedPlaceholder covers the empty-owner 400: an
// unclaimed pool slot has nobody to message, and the admin poking it must be
// told so rather than shown a zero-recipient "success".
func TestSlackHiveOwnerRejectsUnclaimedPlaceholder(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "slot1", Owner: "   ", Status: statusAvailable}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"hello","dry_run":true}`,
		"/api/saas/hives/slot1/slack", map[string]string{"id": "slot1"},
		s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unclaimed placeholder = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no owner") {
		t.Errorf("body = %q, want an explanation that the hive has no owner", rec.Body.String())
	}
}

// TestSlackHiveOwnerMissingUserRecord covers the owner-without-user-record 404:
// the hive names an owner but no SaaS user file exists for them, so there is
// no slack_id to even look up.
func TestSlackHiveOwnerMissingUserRecord(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"}); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "h-ghost", Owner: "ghost", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"hello","dry_run":true}`,
		"/api/saas/hives/h-ghost/slack", map[string]string{"id": "h-ghost"},
		s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusNotFound {
		t.Errorf("owner without user record = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "user record") {
		t.Errorf("body = %q, want an explanation naming the missing user record", rec.Body.String())
	}
}

// TestSlackHiveOwnerWithoutSlackIDIsSkippedLoudly covers the single-owner skip:
// the owner exists but has no slack_id, and the result must say "skipped" and
// NAME the unreachable owner — never read like a delivered message.
func TestSlackHiveOwnerWithoutSlackIDIsSkippedLoudly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "quietowner"}, // no slack_id
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveSaaSHive(&SaaSHive{ID: "h-quiet", Owner: "quietowner", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"node full"}`,
		"/api/saas/hives/h-quiet/slack", map[string]string{"id": "h-quiet"},
		s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	res := decodeSlackResult(t, rec)
	if res.Status != "skipped" {
		t.Errorf("status = %q, want \"skipped\"", res.Status)
	}
	if res.Recipients != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 recipients and 1 skipped", res)
	}
	if !strings.Contains(res.Note, "quietowner") || !strings.Contains(res.Note, "h-quiet") {
		t.Errorf("note = %q, want it to name the owner and the hive", res.Note)
	}
}

// TestSlackHiveOwnerLiveSendRequiresToken covers the non-dry-run 503: a hub
// with no bot token must fail loudly on a real send rather than pretend the
// owner was notified.
func TestSlackHiveOwnerLiveSendRequiresToken(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(slackTokenEnvVar, "")

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "owner", SlackID: "U01OWNER"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveSaaSHive(&SaaSHive{ID: "h-live", Owner: "owner", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"real send"}`,
		"/api/saas/hives/h-live/slack", map[string]string{"id": "h-live"},
		s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("live send without token = %d, want 503 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), slackTokenEnvVar) {
		t.Errorf("body = %q, want it to name %s so the operator can fix it", rec.Body.String(), slackTokenEnvVar)
	}
}

// TestSlackHiveOwnerPreflight covers the CORS preflight short-circuit: an
// OPTIONS request is answered 204 before any auth or hive lookup runs.
func TestSlackHiveOwnerPreflight(t *testing.T) {
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}
	req := httptest.NewRequest(http.MethodOptions, "/api/saas/hives/h/slack", nil)
	req.SetPathValue("id", "h")
	rec := httptest.NewRecorder()
	s.handleSlackMessageHiveOwner(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS = %d, want 204", rec.Code)
	}
}
