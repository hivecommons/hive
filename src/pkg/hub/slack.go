package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// SLACK MESSAGING
// ============================================================================
//
// Operator-initiated Slack messages to hub users: one user, the owner of a
// hive, or every user.
//
// The practical driver is that today these messages are hand-copied. The GitHub
// App permission notice (#2353) and the node-capacity problem (#2349) were both
// pasted one org owner at a time, out of the hub's own user list, which already
// carries the Slack IDs.
//
// REUSES THE EXISTING slack_id FIELD. SaaSUser.SlackID is already there — an
// admin-maintained CRM contact field with a length cap (maxContactSlackIDLen),
// an edit path (handleAdminUpdateUser), and a rendered dashboard input. Adding a
// second "slack_handle" field would have split the same fact across two keys on
// ~88 user records with nothing to keep them in step. Nothing about the user
// record changes in this PR.

// slackPostMessageURL is Slack's chat.postMessage endpoint. A bot token
// posting here can DM a user by member ID, which a webhook cannot do — a
// webhook is bound to one channel at creation time.
//
// TEST SEAM. This is a var rather than a const solely so tests can point it at
// an httptest server; production never assigns it. Without the seam the only
// way to exercise sendSlackDM's error handling — Slack's 200-OK-with-ok:false
// responses, the 429 path, a malformed body — would be to call the real Slack
// API from CI, which is exactly the kind of live-external-service test that
// makes a suite flaky and burns a shared quota. Override it with
// withFakeSlackEndpoint in tests, never from application code.
var slackPostMessageURL = "https://slack.com/api/chat.postMessage"

// slackSendInterval paces sending. Slack rate-limits chat.postMessage at
// roughly one message per second per workspace (its "Tier"/special limit for
// this method), and bursting past it earns HTTP 429s that drop messages.
// One second between sends is the documented sustainable rate.
//
// TEST SEAM, same rule as slackPostMessageURL: a var only so a test covering a
// multi-recipient broadcast does not have to sleep one real second per
// recipient. Production leaves it at the documented one second.
var slackSendInterval = 1 * time.Second

const (
	// slackTokenEnvVar names the environment variable holding the Slack bot
	// token (xoxb-...). It is read with NO DEFAULT: an unset variable disables
	// Slack messaging entirely rather than falling back to anything. The token
	// must never appear in this repository — a previous leak required scrubbing
	// this repo's git history, and that must not be recreated.
	//
	// Sourced from the EXISTING "hive-hub-secrets" Kubernetes Secret, which is
	// already captured by the DR backup (hubbackup.DefaultHubSecrets, #2315).
	// Putting the token there rather than in a new Secret is what keeps it in
	// the restorable set without touching the backup's collected-secret list —
	// a new Secret would be silently lost in a restore.
	slackTokenEnvVar = "HIVE_HUB_SLACK_BOT_TOKEN"

	// slackHTTPTimeout bounds a single chat.postMessage call. Slack's API is
	// normally sub-second; this is generous enough to ride out a slow response
	// without letting one recipient stall a broadcast.
	slackHTTPTimeout = 10 * time.Second

	// slackMaxMessageLen bounds an operator-supplied message body. Slack itself
	// truncates around 40000 characters; this is the last point before the text
	// leaves the hub, and it stops a pasted blob becoming 88 enormous requests.
	slackMaxMessageLen = 4000

	// slackMaxBodyBytes bounds the request body for the send endpoints. The
	// message is the only large field, so this is slackMaxMessageLen plus room
	// for the surrounding JSON and the target fields.
	slackMaxBodyBytes = 16 * 1024

	// slackPreviewLen is how much of the message a dry run echoes back. Enough
	// to confirm the right text is about to go out without reproducing a long
	// body in a JSON response and a log line.
	slackPreviewLen = 280

	// slackBroadcastConfirm is the exact value a broadcast request must send in
	// its `confirm` field. Broadcast is the one irreversible action here — it
	// reaches every user with a Slack ID and cannot be recalled — so it requires
	// a deliberate, typed acknowledgement rather than a boolean an errant client
	// could set by accident.
	slackBroadcastConfirm = "SEND TO ALL USERS"
)

// SlackRecipient is one resolved target of a send.
type SlackRecipient struct {
	// Username is the GitHub username (the hub's user key).
	Username string `json:"username"`
	// SlackID is the user's Slack member ID or handle. Empty means this user
	// CANNOT be reached — see Skipped on slackSendResult.
	SlackID string `json:"slack_id,omitempty"`
}

// Reachable reports whether this user has somewhere to send to.
func (r SlackRecipient) Reachable() bool { return strings.TrimSpace(r.SlackID) != "" }

// slackSendResult is the operator-facing outcome of a send or dry run.
//
// Skipped and SkippedUsers are NOT optional detail. A user with no slack_id is
// silently unreachable, and reporting only "sent to 61" while 27 people never
// heard anything is exactly the silent drop this must not do. The count AND the
// names are returned, and logged.
type slackSendResult struct {
	Status string `json:"status"`
	Target string `json:"target"`
	// DryRun is true when nothing was sent.
	DryRun bool `json:"dry_run"`
	// Recipients is how many users will actually be messaged.
	Recipients int `json:"recipients"`
	// Skipped is how many matched users have no slack_id and were skipped.
	Skipped int `json:"skipped"`
	// SkippedUsers names them. Capped in the response, never in the log.
	SkippedUsers []string `json:"skipped_users,omitempty"`
	// Preview is the leading slackPreviewLen characters of the message.
	Preview string `json:"preview,omitempty"`
	// EstimatedSeconds is roughly how long a paced broadcast will take, so an
	// operator is not surprised that a send to ~88 users is still going a minute
	// and a half later.
	EstimatedSeconds int `json:"estimated_seconds,omitempty"`
	// Note carries any operator-facing caveat (e.g. the confirmation
	// requirement) without needing an error status.
	Note string `json:"note,omitempty"`
}

// maxSkippedUsersReported caps how many skipped usernames the JSON response
// lists. The full set always reaches the log; this only stops one response
// growing without bound as the fleet grows.
const maxSkippedUsersReported = 50

// ============================================================================
// SENDING
// ============================================================================

// slackClient is the shared HTTP client for Slack calls. One client (and so one
// connection pool) rather than one per send, which matters for a paced
// broadcast making ~88 sequential calls.
var slackClient = &http.Client{Timeout: slackHTTPTimeout}

// sendSlackDM posts one message to one Slack user via chat.postMessage.
//
// Slack answers 200 OK with {"ok": false, "error": "..."} for application-level
// failures — an invalid user, a missing scope, a revoked token. Checking only
// the HTTP status would report every one of those as a successful send, so the
// body is decoded and ok is checked. The token is never logged; the returned
// error carries only Slack's error code.
func sendSlackDM(token, slackID, message string) error {
	payload, err := json.Marshal(map[string]string{
		"channel": slackID,
		"text":    message,
	})
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, slackPostMessageURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := slackClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		// Surfaced explicitly rather than folded into a generic status error:
		// a 429 means the pacing is wrong, which is a different fix from a bad
		// token or a bad user ID.
		return fmt.Errorf("slack rate limited (HTTP 429) — sends are being paced faster than slack allows")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode slack response: %w", err)
	}
	if !body.OK {
		return fmt.Errorf("slack rejected the message: %s", body.Error)
	}
	return nil
}

// slackBroadcastMu serializes background sends. Two concurrent broadcasts would
// each pace themselves correctly and still collectively exceed Slack's limit,
// so only one runs at a time.
var slackBroadcastMu sync.Mutex

// deliverSlackMessages sends to every recipient, paced at slackSendInterval.
//
// It runs in a GOROUTINE, never inline in the HTTP handler: at one message per
// second a broadcast to ~88 users takes about a minute and a half, which would
// hold a request open long past any sane proxy timeout and leave the operator
// staring at a hung page.
//
// Every failure is logged per-recipient with the username and Slack's own error
// string. A send that partly fails must be visible in the log, since the HTTP
// response was returned long before.
func (s *HubServer) deliverSlackMessages(token, target, message string, recipients []SlackRecipient, actor string) {
	slackBroadcastMu.Lock()
	defer slackBroadcastMu.Unlock()

	sent, failed := 0, 0
	// A nil slice ranges as empty in Go, so this is a no-op rather than a panic
	// on a background goroutine where nothing would catch it.
	for i, rcpt := range recipients {
		if i > 0 {
			// Pace BETWEEN sends, not before the first — a single-user message
			// should not sit idle for a second before going out.
			time.Sleep(slackSendInterval)
		}
		if err := sendSlackDM(token, rcpt.SlackID, message); err != nil {
			failed++
			// The Slack ID is logged (it is not a secret and it is what an
			// operator needs to fix a bad entry); the token and the message body
			// are not.
			s.logger.Warn("slack message failed",
				"target", target, "user", rcpt.Username, "slack_id", rcpt.SlackID, "error", err)
			continue
		}
		sent++
	}
	s.logger.Info("audit: slack messages delivered",
		"target", target, "actor", actor, "sent", sent, "failed", failed, "total", len(recipients))
}

// ============================================================================
// RECIPIENT RESOLUTION
// ============================================================================

// resolveSlackRecipients partitions a set of users into those that can be
// reached and those that cannot, preserving the skipped usernames.
//
// Skipping is never silent: the caller reports both the count and the names.
func resolveSlackRecipients(users []SaaSUser) (reachable []SlackRecipient, skipped []string) {
	// listAllSaaSUsers returns nil on a read error rather than an empty slice;
	// a nil slice ranges as empty, so that case resolves to zero recipients and
	// zero skipped rather than panicking.
	for _, u := range users {
		r := SlackRecipient{Username: u.GitHubUsername, SlackID: strings.TrimSpace(u.SlackID)}
		if !r.Reachable() {
			skipped = append(skipped, u.GitHubUsername)
			continue
		}
		reachable = append(reachable, r)
	}
	return reachable, skipped
}

// slackPreview returns the leading slackPreviewLen characters of a message,
// rune-safe, with an ellipsis when it was cut.
func slackPreview(message string) string {
	runes := []rune(message)
	if len(runes) <= slackPreviewLen {
		return message
	}
	return string(runes[:slackPreviewLen]) + "…"
}

// buildSlackResult assembles the operator-facing result for a resolved set.
func buildSlackResult(target, message string, recipients []SlackRecipient, skipped []string, dryRun bool) slackSendResult {
	reported := skipped
	if len(reported) > maxSkippedUsersReported {
		reported = reported[:maxSkippedUsersReported]
	}
	res := slackSendResult{
		Target:       target,
		DryRun:       dryRun,
		Recipients:   len(recipients),
		Skipped:      len(skipped),
		SkippedUsers: reported,
		Preview:      slackPreview(message),
	}
	if len(recipients) > 1 {
		// The first send is immediate, so the wait is one interval per
		// subsequent recipient.
		res.EstimatedSeconds = int((time.Duration(len(recipients)-1) * slackSendInterval).Seconds())
	}
	if dryRun {
		res.Status = "dry-run"
	} else {
		res.Status = "sending"
	}
	return res
}

// ============================================================================
// HANDLERS
// ============================================================================

// slackSendRequest is the body of the send endpoints.
type slackSendRequest struct {
	// Message is the text to send. Required.
	Message string `json:"message"`
	// DryRun reports what WOULD be sent — recipient count, skipped users and a
	// message preview — and sends nothing.
	DryRun bool `json:"dry_run,omitempty"`
	// Confirm must equal slackBroadcastConfirm for a broadcast. Ignored for the
	// single-user and hive-owner endpoints, which are individually addressed and
	// trivially recoverable.
	Confirm string `json:"confirm,omitempty"`
}

// decodeSlackRequest reads and validates the shared request body, writing the
// error response itself. Returns ok=false when the caller should stop.
func decodeSlackRequest(w http.ResponseWriter, r *http.Request) (slackSendRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, slackMaxBodyBytes)
	var body slackSendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return body, false
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return body, false
	}
	if len([]rune(body.Message)) > slackMaxMessageLen {
		http.Error(w, fmt.Sprintf(`{"error":"message is too long (max %d characters)"}`, slackMaxMessageLen), http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// slackPreflight writes the standard CORS/OPTIONS preamble, matching the other
// hub POST endpoints. Returns true when the request was an OPTIONS preflight and
// the caller should return.
func slackPreflight(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	return false
}

// requireSlackToken returns the bot token, or writes a 503 and returns "".
//
// A hub with no token must FAIL LOUDLY. Returning a cheerful "sending" for a
// send that can never happen is the worst available outcome — the operator
// would believe the fleet had been notified.
func requireSlackToken(w http.ResponseWriter) string {
	token := strings.TrimSpace(os.Getenv(slackTokenEnvVar))
	if token == "" {
		http.Error(w, fmt.Sprintf(`{"error":"slack messaging is not configured on this hub (%s is unset)"}`, slackTokenEnvVar), http.StatusServiceUnavailable)
		return ""
	}
	return token
}

// handleSlackMessageUser sends to ONE user.
//
// AUTHORIZATION: admin-or-owner. Registered behind requireAuth; an admin may
// message anyone, and a non-admin may message only themselves. Letting an
// arbitrary signed-in user DM any other user would make the hub a spam relay,
// so the self-only rule is the floor for non-admins.
func (s *HubServer) handleSlackMessageUser(w http.ResponseWriter, r *http.Request) {
	if slackPreflight(w, r) {
		return
	}
	actor := s.getAuthUser(r)
	username := r.PathValue("username")
	if !isHubAdmin(actor) && !strings.EqualFold(actor, username) {
		http.Error(w, `{"error":"only an admin can message another user"}`, http.StatusForbidden)
		return
	}
	body, ok := decodeSlackRequest(w, r)
	if !ok {
		return
	}
	u := loadSaaSUser(username)
	if u == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	recipients, skipped := resolveSlackRecipients([]SaaSUser{*u})
	res := buildSlackResult("user", body.Message, recipients, skipped, body.DryRun)

	if len(recipients) == 0 {
		// Not an error — the user exists, they just have no Slack ID on file.
		// Saying so explicitly is the whole point; a 200 "sent" here would be a
		// silent drop.
		res.Status = "skipped"
		res.Note = fmt.Sprintf("%s has no slack_id on file — set one on the admin users panel before messaging them", username)
		s.logger.Info("slack message skipped: no slack_id", "target", "user", "user", username, "actor", actor)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	if body.DryRun {
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	token := requireSlackToken(w)
	if token == "" {
		return
	}
	s.logger.Info("audit: slack message queued", "target", "user", "user", username, "actor", actor)
	go s.deliverSlackMessages(token, "user", body.Message, recipients, actor)
	_ = json.NewEncoder(w).Encode(res)
}

// handleSlackMessageHiveOwner sends to the OWNER of a given hive.
//
// AUTHORIZATION: admin-or-owner, the same shape as handleSwitchBranch — the
// owner may message themselves through this route, and an admin may reach any
// hive's owner. This is the route for "tell whoever runs this hive that their
// node is full", which is exactly what #2349 required by hand.
func (s *HubServer) handleSlackMessageHiveOwner(w http.ResponseWriter, r *http.Request) {
	if slackPreflight(w, r) {
		return
	}
	actor := s.getAuthUser(r)
	id := r.PathValue("id")
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !canonicalEqual(h.Owner, actor) && !isHubAdmin(actor) {
		http.Error(w, `{"error":"only the owner can message this hive's owner"}`, http.StatusForbidden)
		return
	}
	body, ok := decodeSlackRequest(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(h.Owner) == "" {
		// An unclaimed placeholder has no owner to message.
		http.Error(w, `{"error":"this hive has no owner"}`, http.StatusBadRequest)
		return
	}
	u := loadSaaSUser(h.Owner)
	if u == nil {
		http.Error(w, `{"error":"this hive's owner has no user record"}`, http.StatusNotFound)
		return
	}
	recipients, skipped := resolveSlackRecipients([]SaaSUser{*u})
	res := buildSlackResult("hive-owner", body.Message, recipients, skipped, body.DryRun)

	if len(recipients) == 0 {
		res.Status = "skipped"
		res.Note = fmt.Sprintf("%s (owner of %s) has no slack_id on file", h.Owner, id)
		s.logger.Info("slack message skipped: no slack_id",
			"target", "hive-owner", "hive_id", id, "owner", h.Owner, "actor", actor)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	if body.DryRun {
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	token := requireSlackToken(w)
	if token == "" {
		return
	}
	s.logger.Info("audit: slack message queued",
		"target", "hive-owner", "hive_id", id, "owner", h.Owner, "actor", actor)
	go s.deliverSlackMessages(token, "hive-owner", body.Message, recipients, actor)
	_ = json.NewEncoder(w).Encode(res)
}

// handleSlackBroadcast sends to EVERY user with a slack_id.
//
// AUTHORIZATION: admin-only (registered behind requireAdmin). Broadcast is the
// one action here that cannot be taken back.
//
// It is a footgun, and it is guarded as one:
//   - dry_run reports the recipient count, the skipped users and a message
//     preview, and sends nothing.
//   - a real send additionally requires confirm == slackBroadcastConfirm. A
//     typed acknowledgement rather than a boolean, so no client can set it by
//     accident and no dry-run request can turn into a live send by dropping one
//     field.
//   - the send is paced and runs in the background; the response returns
//     immediately with an estimate of how long it will take.
func (s *HubServer) handleSlackBroadcast(w http.ResponseWriter, r *http.Request) {
	if slackPreflight(w, r) {
		return
	}
	actor := s.getAuthUser(r)
	body, ok := decodeSlackRequest(w, r)
	if !ok {
		return
	}
	users := listAllSaaSUsers()
	recipients, skipped := resolveSlackRecipients(users)
	res := buildSlackResult("all-users", body.Message, recipients, skipped, body.DryRun)

	if body.DryRun {
		res.Note = fmt.Sprintf("dry run — nothing was sent. Re-send with %q: %q to deliver to %d users.",
			"confirm", slackBroadcastConfirm, len(recipients))
		s.logger.Info("slack broadcast dry run",
			"actor", actor, "recipients", len(recipients), "skipped", len(skipped))
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	if body.Confirm != slackBroadcastConfirm {
		// 409, not 400: the request is well-formed, it just has not been
		// confirmed. Reporting the recipient count in the refusal means the
		// operator sees the blast radius before they retype the confirmation.
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":        fmt.Sprintf("broadcast requires confirm: %q", slackBroadcastConfirm),
			"recipients":   len(recipients),
			"skipped":      len(skipped),
			"preview":      slackPreview(body.Message),
			"skipped_note": "users without a slack_id are skipped and named in skipped_users on a dry run",
		})
		return
	}
	if len(recipients) == 0 {
		res.Status = "skipped"
		res.Note = "no user on this hub has a slack_id on file — nothing to send"
		s.logger.Warn("slack broadcast had no reachable recipients",
			"actor", actor, "users", len(users), "skipped", len(skipped))
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	token := requireSlackToken(w)
	if token == "" {
		return
	}
	// The full skipped list goes to the LOG even though the response caps it:
	// "who did not get told" is the fact an operator needs after a fleet-wide
	// notice, and truncating it out of existence would be the silent drop this
	// endpoint exists to avoid.
	s.logger.Info("audit: slack broadcast queued",
		"actor", actor,
		"recipients", len(recipients),
		"skipped", len(skipped),
		"skipped_users", strings.Join(skipped, ","),
		"estimated_seconds", res.EstimatedSeconds)
	go s.deliverSlackMessages(token, "all-users", body.Message, recipients, actor)
	_ = json.NewEncoder(w).Encode(res)
}
