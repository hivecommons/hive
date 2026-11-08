package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Human gates (hivecommons/crux#114 S3): a driver asks a person to decide
// something before it continues; a person answers from the CLI or the API.
//
//	POST /v1/gates                open (idempotent: the same request from the
//	                              same principal returns the same gate)
//	GET  /v1/gates[?status=]      list (a project sees only gates it opened)
//	GET  /v1/gates/{id}           poll
//	POST /v1/gates/{id}/answer    answer with one of the gate's choices
//	GET  /v1/gates/{id}/wait      long-poll (?timeout=, bounded server-side)
//
// The family admits project principals (to open, read and wait on their own
// gates); only the operator answers.
const PathGates = "/" + Version + "/gates"

// GatePath is the route of one gate.
func GatePath(id string) string { return PathGates + "/" + url.PathEscape(id) }

// GateAnswerPath is the route that answers one gate.
func GateAnswerPath(id string) string { return GatePath(id) + "/answer" }

// GateWaitPath is the long-poll route of one gate.
func GateWaitPath(id string) string { return GatePath(id) + "/wait" }

// Gate statuses, the closed set every record reports. Expired is neither
// approval nor denial.
const (
	GateWaiting  = "waiting"
	GateAnswered = "answered"
	GateExpired  = "expired"
)

// Gate error codes.
const (
	// CodeGateContextTooLarge is a context payload over 64 KiB once
	// encoded (413).
	CodeGateContextTooLarge = "gate_context_too_large"
	// CodeGateInvalidChoice is an answer that is not one of the gate's
	// choices (422); free text belongs in note.
	CodeGateInvalidChoice = "gate_invalid_choice"
	// CodeGateSelfAnswer is the opener answering its own gate without
	// as_operator (403).
	CodeGateSelfAnswer = "gate_self_answer"
	// CodeGateClosed is an answer to a gate that is already answered
	// differently or expired (409).
	CodeGateClosed = "gate_closed"
)

// Gate events on /v1/events; Data is the Gate without its context.
const (
	EventGateOpened   = "gate.opened"
	EventGateAnswered = "gate.answered"
	EventGateExpired  = "gate.expired"
)

// GateRequest is the body of POST /v1/gates.
type GateRequest struct {
	Subject string `json:"subject"`
	// Context is a driver-defined JSON payload, at most 64 KiB encoded.
	Context json.RawMessage `json:"context,omitempty"`
	// Choices defaults to ["approve","deny"].
	Choices []string `json:"choices,omitempty"`
	// Timeout is a Go duration; "" or "0" waits indefinitely.
	Timeout string `json:"timeout,omitempty"`
	// OpenedBy is the driver's own label; the authenticated principal is
	// recorded beside it as opened_by_principal.
	OpenedBy string `json:"opened_by,omitempty"`
	// IdempotencyKey, when set, identifies the gate instead of its contents.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Run, when set, is a contained run the caller owns that the gate holds
	// up: opening appends needs-input to its turn signals, answering input,
	// expiry blocked (S5).
	Run string `json:"run,omitempty"`
}

// Gate is one gate record: the response of every gate route.
type Gate struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Subject             string          `json:"subject"`
	Context             json.RawMessage `json:"context,omitempty"`
	Choices             []string        `json:"choices"`
	Timeout             string          `json:"timeout"`
	OpenedBy            string          `json:"opened_by"`
	OpenedByPrincipal   string          `json:"opened_by_principal"`
	OpenedAt            time.Time       `json:"opened_at"`
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	Run                 string          `json:"run,omitempty"`
	Answer              string          `json:"answer,omitempty"`
	AnsweredBy          string          `json:"answered_by,omitempty"`
	AnsweredByPrincipal string          `json:"answered_by_principal,omitempty"`
	AnsweredAt          *time.Time      `json:"answered_at,omitempty"`
	Note                string          `json:"note,omitempty"`
	OperatorOverride    bool            `json:"operator_override,omitempty"`
	ExpiredAt           *time.Time      `json:"expired_at,omitempty"`
}

// GateList is the response of GET /v1/gates.
type GateList struct {
	Gates []Gate `json:"gates"`
}

// GateAnswerRequest is the body of POST /v1/gates/{id}/answer.
type GateAnswerRequest struct {
	// Answer must be one of the gate's choices.
	Answer string `json:"answer"`
	// Note is free text recorded beside the answer.
	Note string `json:"note,omitempty"`
	// AsOperator is the explicit, audited override that lets the principal
	// that opened the gate answer it.
	AsOperator bool `json:"as_operator,omitempty"`
}

// OpenGate is POST /v1/gates.
func (c *Client) OpenGate(ctx context.Context, in GateRequest) (Gate, error) {
	var out Gate
	err := c.do(ctx, http.MethodPost, PathGates, in, &out)
	return out, err
}

// ListGates is GET /v1/gates[?status=].
func (c *Client) ListGates(ctx context.Context, status string) ([]Gate, error) {
	path := PathGates
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var out GateList
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Gates, err
}

// GetGate is GET /v1/gates/{id}.
func (c *Client) GetGate(ctx context.Context, id string) (Gate, error) {
	var out Gate
	err := c.do(ctx, http.MethodGet, GatePath(id), nil, &out)
	return out, err
}

// AnswerGate is POST /v1/gates/{id}/answer.
func (c *Client) AnswerGate(ctx context.Context, id string, in GateAnswerRequest) (Gate, error) {
	var out Gate
	err := c.do(ctx, http.MethodPost, GateAnswerPath(id), in, &out)
	return out, err
}

// WaitGate is one GET /v1/gates/{id}/wait: it returns when the gate is
// answered or expired, or when the server's poll window (timeout, zero for
// the server default) closes with the gate still waiting.
func (c *Client) WaitGate(ctx context.Context, id string, timeout time.Duration) (Gate, error) {
	path := GateWaitPath(id)
	if timeout > 0 {
		path += "?timeout=" + url.QueryEscape(timeout.String())
	}
	var out Gate
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}
