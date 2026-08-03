package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const slackTestSecret = "slack-test-secret"

// postSlack drives a Slack handler as the given user.
func postSlack(t *testing.T, username, body string, path string, pathVals map[string]string, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range pathVals {
		req.SetPathValue(k, v)
	}
	if username != "" {
		req.AddCookie(&http.Cookie{
			Name:  "hive_hub_user",
			Value: mintHubUserCookieValue(slackTestSecret, username),
		})
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeSlackResult(t *testing.T, rec *httptest.ResponseRecorder) slackSendResult {
	t.Helper()
	var res slackSendResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	return res
}

// TestSlackSkipsAreVisible is the central contract: a user with no slack_id is
// unreachable, and that must be REPORTED — count and names — not silently
// dropped. "Sent to 61" while 27 people never heard anything is the failure
// this endpoint exists to avoid.
func TestSlackSkipsAreVisible(t *testing.T) {
	users := []SaaSUser{
		{GitHubUsername: "alice", SlackID: "U01ALICE"},
		{GitHubUsername: "bob"},                   // no slack_id
		{GitHubUsername: "carol", SlackID: "   "}, // whitespace only == unreachable
		{GitHubUsername: "dave", SlackID: "U04DAVE"},
	}
	reachable, skipped := resolveSlackRecipients(users)
	if len(reachable) != 2 {
		t.Errorf("reachable = %d, want 2", len(reachable))
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %d, want 2 (bob and carol)", len(skipped))
	}
	got := strings.Join(skipped, ",")
	if !strings.Contains(got, "bob") || !strings.Contains(got, "carol") {
		t.Errorf("skipped users = %q, want bob and carol NAMED — a count alone does not say who to chase", got)
	}
	// A whitespace-only Slack ID must not be treated as reachable: it would
	// produce a Slack API error per send rather than an honest skip.
	for _, r := range reachable {
		if strings.TrimSpace(r.SlackID) == "" {
			t.Errorf("recipient %q has a blank slack id but was counted reachable", r.Username)
		}
	}
	// A nil slice must resolve to nothing rather than panicking.
	if r, s := resolveSlackRecipients(nil); len(r) != 0 || len(s) != 0 {
		t.Errorf("nil users = (%d,%d), want (0,0)", len(r), len(s))
	}
}

// TestSlackBroadcastRequiresConfirmation locks the footgun guard. Broadcast is
// the one irreversible action here, so a well-formed request that has not been
// confirmed must be refused — and the refusal must show the blast radius.
func TestSlackBroadcastRequiresConfirmation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "alice", SlackID: "U01ALICE"},
		{GitHubUsername: "bob"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	// No confirmation -> refused, with the recipient count so the operator sees
	// what they are about to do before they retype it.
	rec := postSlack(t, hubAdminUsername, `{"message":"node capacity problem"}`,
		"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — an unconfirmed broadcast must not send (body=%q)", rec.Code, rec.Body.String())
	}
	var refusal map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal["recipients"] == nil {
		t.Error("the refusal must report the recipient count — the blast radius is the thing being confirmed")
	}
	if !strings.Contains(rec.Body.String(), slackBroadcastConfirm) {
		t.Errorf("the refusal must state the exact confirmation string, got %q", rec.Body.String())
	}

	// A WRONG confirmation is still a refusal. The value is typed, not a
	// boolean, precisely so a client cannot set it by accident.
	rec = postSlack(t, hubAdminUsername, `{"message":"hi","confirm":"yes"}`,
		"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast)
	if rec.Code != http.StatusConflict {
		t.Errorf("a wrong confirmation must still refuse, got %d", rec.Code)
	}
}

// TestSlackBroadcastDryRunSendsNothing locks the dry run: it must report the
// recipient count, the skipped users and a preview, and send nothing — and it
// must work with NO token configured, since it is the thing an operator runs
// before a token is even in place.
func TestSlackBroadcastDryRunSendsNothing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(slackTokenEnvVar, "") // explicitly unconfigured

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "alice", SlackID: "U01ALICE"},
		{GitHubUsername: "bob"},
		{GitHubUsername: "carol"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"please update your GitHub App permissions","dry_run":true}`,
		"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	res := decodeSlackResult(t, rec)
	if !res.DryRun || res.Status != "dry-run" {
		t.Errorf("result = %+v, want a dry run", res)
	}
	if res.Recipients != 2 {
		t.Errorf("recipients = %d, want 2 (admin + alice)", res.Recipients)
	}
	if res.Skipped != 2 {
		t.Errorf("skipped = %d, want 2 (bob + carol)", res.Skipped)
	}
	if len(res.SkippedUsers) != 2 {
		t.Errorf("skipped users = %v, want both named", res.SkippedUsers)
	}
	if !strings.Contains(res.Preview, "GitHub App permissions") {
		t.Errorf("preview = %q, want the message text so the operator can confirm it", res.Preview)
	}
}

// TestSlackBroadcastIsAdminOnlyAtTheRoute documents that broadcast carries no
// in-handler owner check because it is registered behind requireAdmin.
//
// The route registration is the authorization for this endpoint, so the test
// asserts the wiring rather than re-checking inside the handler.
func TestSlackBroadcastIsAdminOnlyAtTheRoute(t *testing.T) {
	s := &HubServer{logger: slog.Default(), mux: http.NewServeMux(), hubSecret: slackTestSecret}
	s.registerSaaSRoutes()

	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/slack/broadcast",
		strings.NewReader(`{"message":"hi","dry_run":true}`))
	// No auth cookie at all: requireAdmin must reject before the handler runs.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated broadcast = %d, want 403 from requireAdmin", rec.Code)
	}
}

// TestSlackMessageUserAuthorization locks the single-user rule: an admin may
// message anyone, a user may message only themselves. Without the self-only
// floor the hub becomes a spam relay between its own users.
func TestSlackMessageUserAuthorization(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "alice", SlackID: "U01ALICE"},
		{GitHubUsername: "mallory", SlackID: "U09MAL"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}
	const body = `{"message":"hello","dry_run":true}`

	rec := postSlack(t, "mallory", body, "/api/saas/slack/user/alice",
		map[string]string{"username": "alice"}, s.handleSlackMessageUser)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-admin messaging ANOTHER user = %d, want 403 (the hub is not a spam relay)", rec.Code)
	}
	rec = postSlack(t, "alice", body, "/api/saas/slack/user/alice",
		map[string]string{"username": "alice"}, s.handleSlackMessageUser)
	if rec.Code != http.StatusOK {
		t.Errorf("a user messaging THEMSELVES = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	rec = postSlack(t, hubAdminUsername, body, "/api/saas/slack/user/alice",
		map[string]string{"username": "alice"}, s.handleSlackMessageUser)
	if rec.Code != http.StatusOK {
		t.Errorf("an admin messaging any user = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
}

// TestSlackMessageUserWithoutHandleIsNotSilent covers the single-user skip. The
// user exists and the request is valid — they simply cannot be reached. That
// must come back as an explicit "skipped" naming the reason, never as a 200
// that reads like a delivered message.
func TestSlackMessageUserWithoutHandleIsNotSilent(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "bob"}, // no slack_id
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername, `{"message":"hello"}`,
		"/api/saas/slack/user/bob", map[string]string{"username": "bob"}, s.handleSlackMessageUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	res := decodeSlackResult(t, rec)
	if res.Status != "skipped" {
		t.Errorf("status = %q, want \"skipped\" — a user with no slack_id must never look delivered", res.Status)
	}
	if res.Recipients != 0 || res.Skipped != 1 {
		t.Errorf("result = %+v, want 0 recipients and 1 skipped", res)
	}
	if !strings.Contains(res.Note, "slack_id") {
		t.Errorf("note = %q, want an explanation naming slack_id", res.Note)
	}
}

// TestSlackHiveOwnerAuthorization locks the hive route's owner-or-admin rule,
// matching handleSwitchBranch.
func TestSlackHiveOwnerAuthorization(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "owner", SlackID: "U01OWNER"},
		{GitHubUsername: "stranger", SlackID: "U02STR"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	saveSaaSHive(&SaaSHive{ID: "h", Owner: "owner", Status: "running"})
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}
	const body = `{"message":"your node is full","dry_run":true}`

	rec := postSlack(t, "stranger", body, "/api/saas/hives/h/slack",
		map[string]string{"id": "h"}, s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a stranger = %d, want 403", rec.Code)
	}
	rec = postSlack(t, "owner", body, "/api/saas/hives/h/slack",
		map[string]string{"id": "h"}, s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusOK {
		t.Errorf("the owner = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	rec = postSlack(t, hubAdminUsername, body, "/api/saas/hives/h/slack",
		map[string]string{"id": "h"}, s.handleSlackMessageHiveOwner)
	if rec.Code != http.StatusOK {
		t.Errorf("an admin = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	// The dry run must resolve the OWNER, not the caller.
	res := decodeSlackResult(t, rec)
	if res.Recipients != 1 || res.Target != "hive-owner" {
		t.Errorf("result = %+v, want 1 recipient targeted at the hive owner", res)
	}
}

// TestSlackRequiresAMessage locks input validation: an empty or oversized
// message never reaches Slack.
func TestSlackRequiresAMessage(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"}); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	for _, body := range []string{`{}`, `{"message":""}`, `{"message":"   "}`, `not json`} {
		rec := postSlack(t, hubAdminUsername, body,
			"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
	// Over the cap.
	long := `{"message":"` + strings.Repeat("x", slackMaxMessageLen+1) + `"}`
	if rec := postSlack(t, hubAdminUsername, long,
		"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized message = %d, want 400", rec.Code)
	}
}

// TestSlackWithoutATokenFailsLoudly is the anti-silent-failure test.
//
// A confirmed broadcast on a hub with no token must return 503, NOT a cheerful
// "sending" for a send that can never happen — the operator would otherwise
// believe the fleet had been notified.
func TestSlackWithoutATokenFailsLoudly(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(slackTokenEnvVar, "")

	for _, u := range []SaaSUser{
		{GitHubUsername: hubAdminUsername, SlackID: "U00ADMIN"},
		{GitHubUsername: "alice", SlackID: "U01ALICE"},
	} {
		user := u
		if err := saveSaaSUser(&user); err != nil {
			t.Fatal(err)
		}
	}
	s := &HubServer{logger: slog.Default(), hubSecret: slackTestSecret}

	rec := postSlack(t, hubAdminUsername,
		`{"message":"hi","confirm":"`+slackBroadcastConfirm+`"}`,
		"/api/saas/admin/slack/broadcast", nil, s.handleSlackBroadcast)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — an unconfigured hub must not report a send it cannot make (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), slackTokenEnvVar) {
		t.Errorf("the error must name the env var an operator has to set, got %q", rec.Body.String())
	}
}

// TestSlackPacingIsDocumentedAndBounded guards the rate constants. Slack
// throttles chat.postMessage at roughly 1/sec; a broadcast to ~88 users must be
// paced, and the estimate shown to the operator must reflect that.
func TestSlackPacingIsDocumentedAndBounded(t *testing.T) {
	if slackSendInterval < 1_000_000_000 { // 1s in nanoseconds
		t.Errorf("slackSendInterval = %v, want at least 1s — Slack rate-limits chat.postMessage at ~1/sec", slackSendInterval)
	}
	// ~88 users is the live fleet size; the estimate must be reported so the
	// operator is not surprised the send is still running 90 seconds later.
	recipients := make([]SlackRecipient, 88)
	for i := range recipients {
		recipients[i] = SlackRecipient{Username: "u", SlackID: "U1"}
	}
	res := buildSlackResult("all-users", "hello", recipients, nil, true)
	if res.EstimatedSeconds < 80 {
		t.Errorf("estimated seconds = %d, want ~87 for 88 paced recipients", res.EstimatedSeconds)
	}
	// A single recipient is sent immediately, with no artificial delay.
	if got := buildSlackResult("user", "hello", recipients[:1], nil, true); got.EstimatedSeconds != 0 {
		t.Errorf("a single recipient must not be paced, got %d seconds", got.EstimatedSeconds)
	}
}

// TestSlackPreviewIsRuneSafe guards the preview against splitting a multi-byte
// character, which would render as a replacement glyph in the operator's
// confirmation step.
func TestSlackPreviewIsRuneSafe(t *testing.T) {
	if got := slackPreview("short"); got != "short" {
		t.Errorf("a short message must pass through unchanged, got %q", got)
	}
	long := strings.Repeat("é", slackPreviewLen+50)
	got := slackPreview(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated preview must be marked as truncated, got %q", got)
	}
	if strings.Contains(got, "�") {
		t.Error("preview split a multi-byte rune")
	}
	if n := len([]rune(strings.TrimSuffix(got, "…"))); n != slackPreviewLen {
		t.Errorf("preview kept %d runes, want %d", n, slackPreviewLen)
	}
}

// TestSlackTokenIsNeverHardcoded is a guard against the failure that once
// required scrubbing this repository's git history: the token must come from
// the environment with NO default and NO fallback value in source.
func TestSlackTokenIsNeverHardcoded(t *testing.T) {
	t.Setenv(slackTokenEnvVar, "")
	rec := httptest.NewRecorder()
	if got := requireSlackToken(rec); got != "" {
		t.Errorf("an unset env var must yield NO token, got a non-empty value of %d bytes", len(got))
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	// And a set value is used verbatim, so the Secret is the single source.
	t.Setenv(slackTokenEnvVar, "xoxb-not-a-real-token")
	rec2 := httptest.NewRecorder()
	if got := requireSlackToken(rec2); got != "xoxb-not-a-real-token" {
		t.Errorf("token = %q, want the env value", got)
	}
}
