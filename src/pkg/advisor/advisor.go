// Package advisor implements phase 1 of the advisor lane
// (hivecommons/hive#9722, design record src/docs/design/advisor-lane.md): a
// second, independently chosen model that reads each turn a hub-launched
// agent finishes and can object before the agent takes its next step.
//
// Hive owns the evaluator: given a transcript reference and the agent's
// identity it resolves the advisor model, builds the review prompt from the
// standing instructions and the turn, calls the model, classifies the answer
// into a severity, records the review, and returns a verdict. What differs
// per backend is only where the turn-end hook is registered and the JSON
// dialect the hook answers in; phase 1 ships the Claude Code `Stop` hook
// adapter (see cli.go and pkg/agent's settings projection).
//
// The lane deliberately inherits the trajectory lane's posture: the same
// OpenAI-compatible one-request-per-review shape, the same reviewer endpoint
// resolution (config.ResolveAdvisorRuntime rides ResolveReviewer), and the
// same fail-open rule — an advisor outage degrades toward "catches less",
// never toward "agents stall".
package advisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/jsonextract"
)

// Severities, borrowing OMP's vocabulary so an operator moving between OMP's
// native advisor and hive's sees the same terms.
const (
	// SeverityAside is recorded, never delivered into the session.
	SeverityAside = "aside"
	// SeverityConcern is delivered so the agent's next step is taken with the
	// objection in front of it; the agent may still finish its turn.
	SeverityConcern = "concern"
	// SeverityBlocker is delivered, and the agent may not end its turn until
	// it has responded to the objection.
	SeverityBlocker = "blocker"
)

// Delivery modes recorded alongside each review, so an operator can tell an
// advisor that talks a lot from one that interrupts a lot.
const (
	DeliveryInterrupting    = "interrupting"
	DeliveryNonInterrupting = "non-interrupting"
)

const (
	// DefaultTranscriptLines bounds how many trailing transcript lines are
	// sent to the advisor — same rationale as the trajectory lane's bound.
	DefaultTranscriptLines = 200

	// maxTranscriptChars is a hard ceiling on the transcript payload
	// regardless of line count.
	maxTranscriptChars = 24000
)

// Verdict is the advisor's structured judgement for one finished turn.
type Verdict struct {
	// Severity is one of SeverityAside, SeverityConcern, SeverityBlocker.
	Severity string `json:"severity"`
	// Text is what the advisor says — delivered to the agent for concerns and
	// blockers, recorded either way.
	Text string `json:"text"`
}

// Interrupting reports whether this verdict is an interrupting delivery.
// Concerns and blockers are delivered into the session; an aside never is.
func (v Verdict) Interrupting() bool {
	return v.Severity == SeverityConcern || v.Severity == SeverityBlocker
}

// Delivery names the verdict's delivery mode for the record.
func (v Verdict) Delivery() string {
	if v.Interrupting() {
		return DeliveryInterrupting
	}
	return DeliveryNonInterrupting
}

// Usage carries the tokens one review consumed, for the record and the
// per-agent daily budget.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total is the review's token spend counted against the daily budget.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// Config is the resolved subset the Evaluator needs. The caller resolves
// endpoint/key/model via config.ResolveAdvisorRuntime so this package has no
// dependency on the config package — the same seam the trajectory package
// keeps.
type Config struct {
	Endpoint     string
	APIKey       string
	Model        string
	Instructions string
	// TimeoutS bounds one review call; when it elapses the agent proceeds and
	// the review is recorded as skipped.
	TimeoutS int
}

// Evaluator makes one stateless model call per finished turn and reads back a
// strict-JSON verdict. Safe for concurrent use.
type Evaluator struct {
	endpoint     string
	apiKey       string
	model        string
	instructions string
	timeout      time.Duration
	http         *http.Client
}

// NewEvaluator builds an Evaluator. It returns (nil, error) when no endpoint
// or model resolves, so the caller can report once instead of failing every
// turn.
func NewEvaluator(c Config) (*Evaluator, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("advisor: no reviewer endpoint resolved (set governor.trajectory.endpoint or governor.litellm.endpoint)")
	}
	if strings.TrimSpace(c.Model) == "" {
		return nil, fmt.Errorf("advisor: no advisor model resolved")
	}
	timeout := time.Duration(c.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Evaluator{
		endpoint:     endpoint,
		apiKey:       c.APIKey,
		model:        strings.TrimSpace(c.Model),
		instructions: strings.TrimSpace(c.Instructions),
		timeout:      timeout,
		http:         &http.Client{Timeout: timeout},
	}, nil
}

// Model returns the advisor model id, for the record.
func (e *Evaluator) Model() string { return e.model }

// tailLines returns the last n lines of s truncated to maxTranscriptChars,
// keeping the most recent characters — the turn's end is what the advisor is
// judging.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxTranscriptChars {
		out = out[len(out)-maxTranscriptChars:]
	}
	return out
}

// buildPrompt assembles the review messages. The system prompt fixes the task
// and the strict JSON contract; the operator's standing instructions ride
// along as guidance. The advisor speaks, it never acts — its only output is
// this text.
func buildPrompt(agentName, instructions, transcript string) []chatMessage {
	system := "You are an experienced engineer reviewing, over the shoulder, the turn an " +
		"autonomous coding agent just finished. Judge ONLY whether the agent's last step was a " +
		"mistake worth flagging before it takes its next one — a wrong assumption, a broken " +
		"edit, a misread requirement, a risky command. Choose one severity: \"aside\" for a " +
		"note not worth interrupting for (including \"nothing to flag\"), \"concern\" for an " +
		"objection the agent should see before its next step, \"blocker\" only when the agent " +
		"must respond to the objection before proceeding. You have no tools and no authority: " +
		"your only output is text. If you are unsure, answer aside with empty text. " +
		"Reply with ONLY a JSON object: {\"severity\":\"aside|concern|blocker\",\"text\":\"what you would say\"}. " +
		"No prose, no markdown."
	if instructions != "" {
		system += "\n\nSTANDING INSTRUCTIONS FROM THE OPERATOR:\n" + instructions
	}
	user := fmt.Sprintf("AGENT: %s\n\nFINISHED TURN (transcript tail):\n%s", agentName, transcript)
	return []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Review scores one finished turn. It never blocks past the configured
// timeout; every transport/parse error is returned so the caller can record
// the review as skipped and FAIL OPEN — the agent proceeds unblocked.
func (e *Evaluator) Review(ctx context.Context, agentName, transcript string) (Verdict, Usage, error) {
	tail := tailLines(transcript, DefaultTranscriptLines)
	if strings.TrimSpace(tail) == "" {
		return Verdict{Severity: SeverityAside}, Usage{}, nil // nothing to judge
	}

	body, err := json.Marshal(chatRequest{
		Model:       e.model,
		Messages:    buildPrompt(agentName, e.instructions, tail),
		Temperature: 0,
		MaxTokens:   400,
	})
	if err != nil {
		return Verdict{}, Usage{}, fmt.Errorf("marshal review request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		e.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Verdict{}, Usage{}, fmt.Errorf("build review request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return Verdict{}, Usage{}, fmt.Errorf("review request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, Usage{}, fmt.Errorf("advisor endpoint returned %d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, Usage{}, fmt.Errorf("read review response: %w", err)
	}
	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return Verdict{}, Usage{}, fmt.Errorf("decode review response: %w", err)
	}
	usage := Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens}
	if len(cr.Choices) == 0 {
		return Verdict{}, usage, fmt.Errorf("advisor returned no choices")
	}
	v, err := ParseVerdict(cr.Choices[0].Message.Content)
	return v, usage, err
}

// ParseVerdict extracts the Verdict JSON from a model reply, tolerating models
// that wrap JSON in prose or code fences. An unknown severity is an unusable
// answer — the review is recorded as skipped, never guessed at.
func ParseVerdict(content string) (Verdict, error) {
	raw := jsonextract.Object(content)
	if raw == "" {
		return Verdict{}, fmt.Errorf("no JSON object in advisor reply")
	}
	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict: %w", err)
	}
	v.Severity = strings.ToLower(strings.TrimSpace(v.Severity))
	switch v.Severity {
	case SeverityAside, SeverityConcern, SeverityBlocker:
	default:
		return Verdict{}, fmt.Errorf("unusable advisor severity %q", v.Severity)
	}
	v.Text = strings.TrimSpace(v.Text)
	return v, nil
}
