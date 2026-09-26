package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agentaudit"
)

// DecidePort is the loopback port agents reach the decision endpoint on.
// Distinct from the MITM proxy (18443), the inference translator (18444) and
// the local LiteLLM proxy (18445).
const DecidePort = 18446

// DecidePath is the single route the endpoint serves.
const DecidePath = "/v1/decide"

// DefaultEndpoint is what HIVE_JEV_ENDPOINT carries to agents.
const DefaultEndpoint = "http://127.0.0.1:18446"

// UsageRecorder receives the input tokens of every answered decision so they
// land in the governor's budget. *tokens.InferenceSink satisfies it.
type UsageRecorder interface {
	Record(agent, model string, inputTokens, outputTokens int64)
}

// Server answers agent decision requests. Every dependency is injected so the
// composition root (cmd/hive) owns the wiring and tests can drive it directly.
type Server struct {
	// Identify names the calling agent from the request (socket UID first,
	// advisory header fallback). Empty means "unknown" and the call is refused.
	Identify func(r *http.Request) string
	// Enabled reports whether the named agent has jev_mode: assist right now.
	Enabled func(agent string) bool
	// Key resolves the hive's Jev API key; empty means not ready.
	Key func() string
	// Timeout bounds one provider round-trip.
	Timeout time.Duration
	Client  *Client
	// Usage may be nil (no budget accounting). Audit may be nil (no audit).
	Usage  UsageRecorder
	Audit  agentaudit.AuditSink
	Logger *slog.Logger
}

// Handler returns the HTTP handler for the decision endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+DecidePath, s.handleDecide)
	return mux
}

// ListenAndServe binds the fixed loopback port and serves until it fails.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", DecidePort))
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve answers on ln until it is closed or serving fails.
func (s *Server) Serve(ln net.Listener) error {
	if s.Logger != nil {
		s.Logger.Info("jev decision endpoint starting", "addr", ln.Addr().String())
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	agent := ""
	if s.Identify != nil {
		agent = strings.TrimSpace(s.Identify(r))
	}
	if agent == "" {
		writeJSON(w, http.StatusForbidden, errorBody{"calling agent could not be identified"})
		return
	}
	if s.Enabled == nil || !s.Enabled(agent) {
		writeJSON(w, http.StatusForbidden, errorBody{"jev_mode is off for agent " + agent})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxStateBytes+MaxQuestionBytes+MaxOptions*MaxOptionBytes+4096))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"failed to read request"})
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"request must be JSON: " + err.Error()})
		return
	}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return
	}
	key := ""
	if s.Key != nil {
		key = strings.TrimSpace(s.Key())
	}
	if key == "" || s.Client == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{"jev is not ready: no API key configured (set JEV_API_KEY or connect OpenRouter)"})
		return
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := s.Client.Decide(ctx, req, key)
	if err != nil {
		s.record(agent, req, Result{}, err)
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, errorBody{err.Error()})
		return
	}
	s.record(agent, req, res, nil)
	writeJSON(w, http.StatusOK, res)
}

// record feeds the budget sink and the audit log. Failures are audited too so
// an operator can see an agent hammering a broken endpoint.
func (s *Server) record(agent string, req Request, res Result, callErr error) {
	if callErr == nil && s.Usage != nil && res.InputTokens > 0 {
		s.Usage.Record(agent, res.Model, int64(res.InputTokens), 0)
	}
	if s.Logger != nil {
		if callErr != nil {
			s.Logger.Warn("jev decision failed", "agent", agent, "type", req.Type, "error", callErr)
		} else {
			s.Logger.Info("jev decision", "agent", agent, "type", req.Type, "confidence", res.Confidence, "input_tokens", res.InputTokens)
		}
	}
	if s.Audit == nil {
		return
	}
	outcome := "success"
	errText := ""
	if callErr != nil {
		outcome = "failure"
		errText = callErr.Error()
	}
	s.Audit.Record("agent", agentaudit.AuditJevDecision, agent, agentaudit.Fields(
		"outcome", outcome,
		"question_type", req.Type,
		"confidence", res.Confidence,
		"input_tokens", res.InputTokens,
		"model", res.Model,
		"error", errText,
	))
}
