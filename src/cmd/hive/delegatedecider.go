package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// delegateDeciderRunner runs the owner-delegate decider turn behind "Let hive
// decide" (decisions.delegate.llm) as one chat completion on the governor
// reviewer endpoint — the same endpoint/key resolution the intent-alignment
// and trajectory reviewers use. Config is read per call so a reload takes
// effect without a restart.
type delegateDeciderRunner struct {
	cfg  func() *config.Config
	http *http.Client
}

const (
	delegateDeciderTimeout   = 60 * time.Second
	delegateDeciderMaxTokens = 800
	delegateDeciderMaxReply  = 1 << 20
)

var errDelegateDeciderUnconfigured = errors.New("owner-delegate decider: no reviewer endpoint or model resolved")

func newDelegateDeciderRunner(cfg func() *config.Config) delegatedecision.TurnRunner {
	return &delegateDeciderRunner{cfg: cfg, http: &http.Client{Timeout: delegateDeciderTimeout}}
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
	endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
	if m := cfg.Decisions.Delegate.LLMModel(); m != "" {
		model = m
	}
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" || strings.TrimSpace(model) == "" {
		return "", errDelegateDeciderUnconfigured
	}
	body, err := json.Marshal(delegateDeciderRequest{
		Model:       model,
		Messages:    []delegateDeciderMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature: 0,
		MaxTokens:   delegateDeciderMaxTokens,
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, delegateDeciderMaxReply))
	if err != nil {
		return "", fmt.Errorf("%s: read response: %w", profile, err)
	}
	var cr delegateDeciderResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", profile, err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("%s: no choices", profile)
	}
	return cr.Choices[0].Message.Content, nil
}
