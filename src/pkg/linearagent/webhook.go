package linearagent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Linear webhook receiver (RFC #4492 Part 2, component B).
//
// A sibling of pkg/channels/webhook.go, which is GitHub-shaped: it hardcodes
// X-Hub-Signature-256 (a "sha256=<hex>" prefix scheme) and X-GitHub-Event.
// Linear's contract is different on every axis this handler touches:
//
//   - the signature header is `Linear-Signature`, a BARE hex HMAC-SHA256 of
//     the raw body (no "sha256=" prefix);
//   - the event type rides in `Linear-Event` and in the body's `type` field;
//   - replay protection is IN-BAND: the signed body carries a
//     `webhookTimestamp` (Unix ms) and Linear's docs say to reject anything
//     more than a minute from local time. GitHub has no equivalent, which is
//     why the channels receiver has no replay guard and this one must.
//
// Ordering is deliberate: signature FIRST (over the raw bytes, before any
// JSON parse), then the timestamp read from the now-authenticated body. A
// timestamp check before signature verification would be deciding trust from
// unauthenticated data.
//
// Linear requires a response within 5 seconds and the first activity within
// 10. This handler therefore does no synchronous downstream work at all: it
// verifies, parses, hands the event to the responder, and returns 200. The
// responder owns the 10-second ack (see responder.go).

const (
	// webhookSecretEnv holds the signing secret from the OAuth app's webhook
	// configuration. Environment-only, like the GitHub webhook secret.
	webhookSecretEnv = "LINEAR_WEBHOOK_SECRET"

	// signatureHeader is Linear's HMAC header: hex(HMAC-SHA256(raw body)).
	signatureHeader = "Linear-Signature"

	// eventHeader names the entity type ("AgentSessionEvent", "Issue", …).
	eventHeader = "Linear-Event"

	// webhookMaxBody bounds one webhook body. Agent session payloads carry an
	// issue, its comments, and promptContext — generous, but bounded.
	webhookMaxBody = 4 << 20

	// webhookMaxSkew is how far the signed webhookTimestamp may sit from
	// local time. Linear's documented recommendation is one minute.
	webhookMaxSkew = time.Minute

	// agentSessionEventType is the payload type this receiver acts on.
	agentSessionEventType = "AgentSessionEvent"

	// Session event actions.
	actionCreated  = "created"
	actionPrompted = "prompted"
)

// SessionEvent is the subset of an AgentSessionEvent webhook this integration
// consumes. Field names follow the wire payload.
type SessionEvent struct {
	Action           string       `json:"action"`
	Type             string       `json:"type"`
	OrganizationID   string       `json:"organizationId"`
	WebhookTimestamp int64        `json:"webhookTimestamp"`
	AgentSession     AgentSession `json:"agentSession"`
	// AgentActivity carries the user's message on `prompted`.
	AgentActivity struct {
		Body    string `json:"body"`
		Content struct {
			Body string `json:"body"`
		} `json:"content"`
	} `json:"agentActivity"`
	// PromptContext is Linear's pre-formatted context string. Observed both
	// at the top level and under agentSession across payload revisions, so
	// both are parsed and PromptText prefers whichever is present.
	PromptContext string `json:"promptContext"`
}

// AgentSession is the session object inside the webhook payload.
type AgentSession struct {
	ID    string `json:"id"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		URL        string `json:"url"`
	} `json:"issue"`
	Comment struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	PromptContext string `json:"promptContext"`
}

// PromptText returns the best available task text for the event: the user's
// message on `prompted`, else Linear's promptContext, else the mention
// comment, else the issue title.
func (e *SessionEvent) PromptText() string {
	if e.Action == actionPrompted {
		if b := strings.TrimSpace(e.AgentActivity.Content.Body); b != "" {
			return b
		}
		if b := strings.TrimSpace(e.AgentActivity.Body); b != "" {
			return b
		}
	}
	if pc := strings.TrimSpace(e.PromptContext); pc != "" {
		return pc
	}
	if pc := strings.TrimSpace(e.AgentSession.PromptContext); pc != "" {
		return pc
	}
	if b := strings.TrimSpace(e.AgentSession.Comment.Body); b != "" {
		return b
	}
	return strings.TrimSpace(e.AgentSession.Issue.Title)
}

// WebhookReceiver verifies and routes Linear webhooks.
type WebhookReceiver struct {
	// secret resolves the signing secret per-request, so a secret set after
	// boot is picked up without a restart (matches the channels receiver
	// reading its env on every request).
	secret func() string

	// handle receives each verified AgentSessionEvent. It is called on a new
	// goroutine — the HTTP response never waits on it.
	handle func(SessionEvent)

	logger *slog.Logger
	now    func() time.Time
}

// NewWebhookReceiver builds a receiver that reads LINEAR_WEBHOOK_SECRET and
// forwards AgentSessionEvents to handle.
func NewWebhookReceiver(handle func(SessionEvent), logger *slog.Logger) *WebhookReceiver {
	if logger == nil {
		logger = slog.Default()
	}
	return &WebhookReceiver{
		secret: func() string { return os.Getenv(webhookSecretEnv) },
		handle: handle,
		logger: logger,
		now:    time.Now,
	}
}

// SetSecretFunc overrides secret resolution. Tests only.
func (w *WebhookReceiver) SetSecretFunc(f func() string) { w.secret = f }

// SetClock overrides the receiver's clock. Tests only.
func (w *WebhookReceiver) SetClock(f func() time.Time) { w.now = f }

func (w *WebhookReceiver) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBody))
	if err != nil {
		http.Error(rw, "failed to read body", http.StatusBadRequest)
		return
	}

	secret := w.secret()
	if secret == "" {
		// Fail closed, exactly like the GitHub channels receiver: accepting
		// unsigned webhooks would let any network-reachable client mint agent
		// sessions and kick agents.
		w.logger.Warn("linearagent: webhook rejected because LINEAR_WEBHOOK_SECRET is not configured")
		http.Error(rw, "webhook secret not configured; set LINEAR_WEBHOOK_SECRET to the Linear webhook signing secret", http.StatusUnauthorized)
		return
	}

	if !verifyLinearSignature(body, r.Header.Get(signatureHeader), secret) {
		http.Error(rw, "invalid signature", http.StatusUnauthorized)
		return
	}

	var ev SessionEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(rw, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Replay guard, checked only AFTER the signature authenticated the body.
	sent := time.UnixMilli(ev.WebhookTimestamp)
	if skew := w.now().Sub(sent); ev.WebhookTimestamp == 0 || skew > webhookMaxSkew || skew < -webhookMaxSkew {
		http.Error(rw, "stale webhook timestamp", http.StatusUnauthorized)
		return
	}

	eventType := ev.Type
	if eventType == "" {
		eventType = r.Header.Get(eventHeader)
	}
	if eventType != agentSessionEventType {
		// Not ours (the app may also have data-change categories enabled).
		// 200 so Linear does not retry.
		respondOK(rw, eventType, false)
		return
	}
	if ev.Action != actionCreated && ev.Action != actionPrompted {
		respondOK(rw, eventType+"."+ev.Action, false)
		return
	}
	if ev.AgentSession.ID == "" {
		http.Error(rw, "agentSession.id missing", http.StatusBadRequest)
		return
	}

	// Hand off and answer immediately: Linear's 5-second response budget must
	// never wait on the responder (whose kick path can block on tmux).
	if w.handle != nil {
		go w.handle(ev)
	}
	respondOK(rw, eventType+"."+ev.Action, true)
}

func respondOK(rw http.ResponseWriter, event string, handled bool) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(rw, `{"ok":true,"event":%q,"handled":%t}`, event, handled) // best-effort; client may already be gone
}

// verifyLinearSignature checks a bare-hex HMAC-SHA256 signature over the raw
// body. Constant-time compare; an empty or undecodable header fails.
func verifyLinearSignature(body []byte, signature, secret string) bool {
	sig, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(sig) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}
