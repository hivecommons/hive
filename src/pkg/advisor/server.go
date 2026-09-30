package advisor

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
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// AdvisePort is the loopback port the turn-end hook reaches the advisor
// evaluator on. Distinct from the MITM proxy (18443), the inference
// translator (18444), the local LiteLLM proxy (18445) and the Jev decision
// endpoint (18446).
const AdvisePort = 18447

// AdvisePath is the single route the endpoint serves.
const AdvisePath = "/v1/advise"

// EndpointEnvVar carries the advise endpoint base URL to advised agents, the
// way HIVE_JEV_ENDPOINT carries the Jev endpoint.
const EndpointEnvVar = "HIVE_ADVISOR_ENDPOINT"

// DefaultEndpoint is what EndpointEnvVar carries to agents.
const DefaultEndpoint = "http://127.0.0.1:18447"

// maxTranscriptBytes bounds one hook request body.
const maxTranscriptBytes = maxTranscriptChars + 8192

// Runtime is the per-agent resolved advisor shape the server needs for one
// review. The composition root maps config.AdvisorRuntime onto it so this
// package keeps the trajectory package's config-free seam.
type Runtime struct {
	Endpoint             string
	APIKey               string
	Model                string
	Instructions         string
	TimeoutS             int
	DailyBudgetTokens    int
	MaxConsecutiveBlocks int
}

// Request is what the hook posts for one finished turn.
type Request struct {
	// Turn is the backend's reference for the turn (Claude session id).
	Turn string `json:"turn,omitempty"`
	// Transcript is the turn's transcript tail.
	Transcript string `json:"transcript"`
}

// Response is the verdict the hook renders into its backend's dialect.
type Response struct {
	Severity    string `json:"severity"`
	Text        string `json:"text,omitempty"`
	Interjected bool   `json:"interjected"`
	Delivery    string `json:"delivery"`
	// Skipped names the skip reason when the review did not happen; the hook
	// treats any skipped review as "do not block" — the lane fails open.
	Skipped string `json:"skipped,omitempty"`
}

// UsageRecorder receives the tokens of every completed review so advisor
// spend lands in the governor's budget beside agent spend.
// *tokens.InferenceSink satisfies it.
type UsageRecorder interface {
	Record(agent, model string, inputTokens, outputTokens int64)
}

// Server answers turn-end hook requests on the loopback advise endpoint.
// Every dependency is injected so the composition root (cmd/hive) owns the
// wiring and tests can drive it directly. The guard invariant (#7563) holds
// by construction: the caller is identified from the socket UID the same way
// the Jev endpoint identifies it (no claimed names), inbound transcript text
// passes ioscan.EnforceInput, and outbound advisor text is canary/secret
// scrubbed before it reaches the agent or the record.
type Server struct {
	// Identify names the calling agent from the request (socket UID). Empty
	// means "unknown" and the call is refused.
	Identify func(r *http.Request) string
	// Resolve returns the live advisor runtime for the named agent; ok=false
	// means the advisor is off for it and the call is refused.
	Resolve func(agent string) (Runtime, bool)
	// Store records every review, skips included. Required.
	Store *Store
	// Usage may be nil (no budget accounting).
	Usage  UsageRecorder
	Logger *slog.Logger

	// newEvaluator is a test seam; nil uses NewEvaluator.
	newEvaluator func(Config) (*Evaluator, error)

	mu    sync.Mutex
	state map[string]*agentState
}

// agentState tracks the per-agent counters the design bounds reviews with:
// the daily token spend and the consecutive-block count.
type agentState struct {
	day               string
	spentTokens       int
	consecutiveBlocks int
}

// Handler returns the HTTP handler for the advise endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+AdvisePath, s.handleAdvise)
	return mux
}

// ListenAndServe binds the fixed loopback port and serves until it fails.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", AdvisePort))
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve answers on ln until it is closed or serving fails.
func (s *Server) Serve(ln net.Listener) error {
	if s.Logger != nil {
		s.Logger.Info("advisor advise endpoint starting", "addr", ln.Addr().String())
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

func (s *Server) handleAdvise(w http.ResponseWriter, r *http.Request) {
	agent := ""
	if s.Identify != nil {
		agent = strings.TrimSpace(s.Identify(r))
	}
	if agent == "" {
		writeJSON(w, http.StatusForbidden, errorBody{"calling agent could not be identified"})
		return
	}
	if s.Resolve == nil {
		writeJSON(w, http.StatusForbidden, errorBody{"advisor is not configured"})
		return
	}
	rt, ok := s.Resolve(agent)
	if !ok {
		writeJSON(w, http.StatusForbidden, errorBody{"advisor is not enabled for agent " + agent})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTranscriptBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"failed to read request"})
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"request must be JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Transcript) == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{"transcript is required"})
		return
	}
	resp := s.review(r.Context(), agent, rt, req)
	writeJSON(w, http.StatusOK, resp)
}

// review runs one advisor review end to end: guard the inbound text, check
// the budget, call the model, bound consecutive blockers, scrub the outbound
// text, and record exactly one Record — skips included. Every failure path
// answers with a non-blocking response: the lane FAILS OPEN.
func (s *Server) review(ctx context.Context, agent string, rt Runtime, req Request) Response {
	base := Record{
		Agent:  agent,
		Turn:   strings.TrimSpace(req.Turn),
		Model:  rt.Model,
		Heeded: HeededUndetermined,
	}

	// Guard invariant: ioscan on the inbound text, before any model sees it.
	transcript, verdict := ioscan.EnforceInput(req.Transcript)
	if verdict.Blocked {
		return s.skip(agent, base, SkipInputBlocked)
	}

	if rt.DailyBudgetTokens > 0 && s.spentToday(agent) >= rt.DailyBudgetTokens {
		return s.skip(agent, base, SkipBudgetExhausted)
	}

	newEval := s.newEvaluator
	if newEval == nil {
		newEval = NewEvaluator
	}
	eval, err := newEval(Config{
		Endpoint:     rt.Endpoint,
		APIKey:       rt.APIKey,
		Model:        rt.Model,
		Instructions: rt.Instructions,
		TimeoutS:     rt.TimeoutS,
	})
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("advisor not ready", "agent", agent, "err", err.Error())
		}
		return s.skip(agent, base, SkipUnreachable)
	}

	v, usage, err := eval.Review(ctx, agent, transcript)
	base.InputTokens = usage.InputTokens
	base.OutputTokens = usage.OutputTokens
	s.recordSpend(agent, rt.Model, usage)
	if err != nil {
		reason := SkipUnreachable
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			reason = SkipTimeout
		case strings.Contains(err.Error(), "unusable") || strings.Contains(err.Error(), "no JSON object") || strings.Contains(err.Error(), "parse verdict"):
			reason = SkipUnusableAnswer
		}
		if s.Logger != nil {
			s.Logger.Warn("advisor review failed", "agent", agent, "reason", reason, "err", err.Error())
		}
		return s.skip(agent, base, reason)
	}

	// Guard invariant: canary/secret scrubbing on the outbound text before it
	// reaches the agent's session or the record.
	v.Text = logscrub.ScrubString(v.Text, logscrub.WithMarkers())

	// Two bounds keep a blocker from holding an agent turn after turn: hive's
	// consecutive-block limit fires here, downgrading further blockers to
	// asides with a record that says so; every backend's own bound stays
	// unrelied-on above it.
	downgraded := false
	if v.Severity == SeverityBlocker && s.blockDelivered(agent, rt.MaxConsecutiveBlocks) {
		v.Severity = SeverityAside
		downgraded = true
	}
	if v.Severity != SeverityBlocker && !downgraded {
		s.resetBlocks(agent)
	}

	base.Severity = v.Severity
	base.Text = v.Text
	base.Interjected = v.Interrupting()
	base.Delivery = v.Delivery()
	base.Downgraded = downgraded
	if s.Store != nil {
		s.Store.Append(base)
	}
	if s.Logger != nil {
		s.Logger.Info("advisor review", "agent", agent, "severity", v.Severity,
			"interjected", base.Interjected, "downgraded", downgraded,
			"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	}
	return Response{
		Severity:    v.Severity,
		Text:        v.Text,
		Interjected: base.Interjected,
		Delivery:    base.Delivery,
	}
}

// skip records a skipped review and answers non-blocking, so the agent
// proceeds — the reason lands in the record for the operator.
func (s *Server) skip(agent string, base Record, reason string) Response {
	base.Skipped = reason
	base.Severity = ""
	base.Interjected = false
	base.Delivery = DeliveryNonInterrupting
	if s.Store != nil {
		s.Store.Append(base)
	}
	s.resetBlocks(agent)
	return Response{
		Severity: SeverityAside,
		Delivery: DeliveryNonInterrupting,
		Skipped:  reason,
	}
}

func (s *Server) stateFor(agent string) *agentState {
	if s.state == nil {
		s.state = make(map[string]*agentState)
	}
	st, ok := s.state[agent]
	if !ok {
		st = &agentState{}
		s.state[agent] = st
	}
	// Same day boundary hive's spend reporting uses: the UTC date.
	day := time.Now().UTC().Format("2006-01-02")
	if st.day != day {
		st.day = day
		st.spentTokens = 0
	}
	return st
}

func (s *Server) spentToday(agent string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateFor(agent).spentTokens
}

func (s *Server) recordSpend(agent, model string, usage Usage) {
	if usage.Total() == 0 {
		return
	}
	s.mu.Lock()
	s.stateFor(agent).spentTokens += usage.Total()
	s.mu.Unlock()
	if s.Usage != nil {
		s.Usage.Record(agent, model, int64(usage.InputTokens), int64(usage.OutputTokens))
	}
}

// blockDelivered counts one blocker against the agent's consecutive-block
// limit. It returns true when the limit is already reached and the blocker
// must be downgraded; a delivered blocker increments the count and every
// non-blocker resets it.
func (s *Server) blockDelivered(agent string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateFor(agent)
	if limit > 0 && st.consecutiveBlocks >= limit {
		return true
	}
	st.consecutiveBlocks++
	return false
}

func (s *Server) resetBlocks(agent string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateFor(agent).consecutiveBlocks = 0
}
