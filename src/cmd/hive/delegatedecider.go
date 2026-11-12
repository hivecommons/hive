package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// delegateDeciderRunner runs the owner-delegate decider turn behind "Let hive
// decide" (decisions.delegate.llm) as one chat completion on the governor
// reviewer endpoint — the same endpoint/key resolution the intent-alignment
// and trajectory reviewers use. On a hive with no endpoint and a Claude login,
// governor.reviewer_backend (auto by default) runs the same turn through the
// Claude CLI instead (reviewer_cli_runner.go). Config is read per call so a
// reload takes effect without a restart.
type delegateDeciderRunner struct {
	cfg  func() *config.Config
	http *http.Client
	// model returns the per-feature model override ("" keeps the reviewer
	// model); nil means the "Let hive decide" override.
	model func(*config.Config) string
	// maxTokens caps the completion; zero means delegateDeciderMaxTokens.
	maxTokens int
	// cli runs the turn through the Claude CLI when governor.reviewer_backend
	// routes it there (reviewer_cli_runner.go); nil means HTTP only.
	cli *reviewerCLIRunner
}

// workflowSpecMaxTokens fits a whole WorkflowModel JSON document.
const workflowSpecMaxTokens = 8000

// newWorkflowSpecRunner is the same reviewer-endpoint turn for the optional
// LLM refinement of written-spec ingest (POST /api/workflows/ingest/spec,
// RFC #17620 phase 6): the reviewer model, a larger completion budget. It
// stays on the HTTP endpoint (no cli), matching the ReviewerReady() check its
// endpoint reports readiness with.
func newWorkflowSpecRunner(cfg func() *config.Config) delegatedecision.TurnRunner {
	return &delegateDeciderRunner{
		cfg:       cfg,
		http:      &http.Client{Timeout: delegateDeciderTimeout},
		model:     func(*config.Config) string { return "" },
		maxTokens: workflowSpecMaxTokens,
	}
}

const (
	delegateDeciderTimeout   = 60 * time.Second
	delegateDeciderMaxTokens = 800
	delegateDeciderMaxReply  = 1 << 20
)

var errDelegateDeciderUnconfigured = errors.New("owner-delegate decider: no reviewer endpoint or model resolved")

func newDelegateDeciderRunner(cfg func() *config.Config) delegatedecision.TurnRunner {
	return &delegateDeciderRunner{cfg: cfg, http: &http.Client{Timeout: delegateDeciderTimeout}, cli: newReviewerCLIRunner()}
}

// newChatGroundingRunner is the same reviewer turn for the dashboard's
// grounded chat answers (chat.grounded_answers), with that block's model
// override instead of the decider's.
func newChatGroundingRunner(cfg func() *config.Config) delegatedecision.TurnRunner {
	return &delegateDeciderRunner{
		cfg:   cfg,
		http:  &http.Client{Timeout: delegateDeciderTimeout},
		model: func(c *config.Config) string { return c.Chat.GroundedAnswersModel() },
		cli:   newReviewerCLIRunner(),
	}
}

type delegateDeciderMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type delegateDeciderRequest struct {
	Model       string                   `json:"model"`
	Messages    []delegateDeciderMessage `json:"messages"`
	Temperature float64                  `json:"temperature"`
	MaxTokens   int                      `json:"max_tokens"`
}

type delegateDeciderResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (r *delegateDeciderRunner) RunDeciderTurn(ctx context.Context, profile, system, user string) (string, error) {
	cfg := r.cfg()
	if cfg == nil {
		return "", errDelegateDeciderUnconfigured
	}
	override := cfg.Decisions.Delegate.LLMModel()
	if r.model != nil {
		override = r.model(cfg)
	}
	route := r.route(cfg, override)
	if !route.Ready {
		return "", fmt.Errorf("%w: %s", errDelegateDeciderUnconfigured, route.Reason)
	}
	if route.Path == config.ReviewerPathClaudeCLI {
		return r.cli.run(ctx, profile, system, user, route.Model)
	}
	endpoint, apiKey, model := route.Endpoint, route.APIKey, route.Model
	body, err := json.Marshal(delegateDeciderRequest{
		Model:       model,
		Messages:    []delegateDeciderMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0,
		MaxTokens:   r.completionBudget(),
	})
	if err != nil {
		return "", fmt.Errorf("%s: marshal request: %w", profile, err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, delegateDeciderTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%s: build request: %w", profile, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: request failed: %w", profile, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: endpoint returned %d", profile, resp.StatusCode)
	}
	raw, err := readTurnReply(profile, resp.Body)
	if err != nil {
		return "", err
	}
	var cr delegateDeciderResponse
	if err := decodeTurnReply(profile, raw, &cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("%s: no choices", profile)
	}
	return cr.Choices[0].Message.Content, nil
}

// route picks the turn's path (config.ResolveReviewerTurn). A runner with
// no cli (the workflow-spec runner) is HTTP only, whatever
// governor.reviewer_backend says.
func (r *delegateDeciderRunner) route(cfg *config.Config, override string) config.ReviewerTurnRoute {
	if r.cli == nil {
		return cfg.Governor.ResolveReviewerTurnVia(config.ReviewerBackendHTTP, override, false)
	}
	return cfg.Governor.ResolveReviewerTurn(override, r.cli.signedIn())
}

func (r *delegateDeciderRunner) completionBudget() int {
	if r.maxTokens > 0 {
		return r.maxTokens
	}
	return delegateDeciderMaxTokens
}
