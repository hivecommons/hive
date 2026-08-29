package retro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/outputschema"
)

const (
	DefaultAnalysisMaxTokens = 350

	analysisTimeout       = 30 * time.Second
	maxRecordTitleChars   = 180
	maxFindingDetailChars = 240
	maxPromptChars        = 6000

	MaxAnalysisFieldChars = 500
	MinLessonChars        = 25
	MaxLessonChars        = 500
)

// Analysis is the structured model-generated retrospective for a deterministic
// retro finding set.
type Analysis struct {
	RootCauseHypothesis string `json:"root_cause_hypothesis"`
	ProcessImprovement  string `json:"process_improvement"`
	Generalizable       bool   `json:"generalizable"`
	GeneralizableLesson string `json:"generalizable_lesson"`
}

type analysisClient interface {
	Complete(ctx context.Context, messages []chatMessage, maxTokens int) (string, error)
}

type Analyzer struct {
	client analysisClient
	model  string
}

func NewAnalyzer(endpoint, apiKey, model string) (*Analyzer, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, nil
	}
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("retro analysis: no model endpoint resolved")
	}
	return &Analyzer{client: &openAIChatClient{endpoint: endpoint, apiKey: apiKey, model: model, http: &http.Client{Timeout: analysisTimeout}}, model: model}, nil
}

func newAnalyzerWithClient(model string, client analysisClient) *Analyzer {
	return &Analyzer{model: strings.TrimSpace(model), client: client}
}

func (a *Analyzer) Analyze(ctx context.Context, record RetroRecord, findings []Finding) (Analysis, error) {
	if a == nil || a.client == nil || strings.TrimSpace(a.model) == "" {
		return Analysis{}, nil
	}
	messages := buildAnalysisPrompt(record, findings)
	var lastErr error
	for attempt := 0; attempt <= outputschema.MaxValidationRetries; attempt++ {
		reply, err := a.client.Complete(ctx, messages, DefaultAnalysisMaxTokens)
		if err != nil {
			return Analysis{}, err
		}
		analysis, err := ParseAnalysis(reply)
		if err == nil {
			return analysis, nil
		}
		lastErr = err
		messages = append(messages, chatMessage{Role: "user", Content: correctiveAnalysisPrompt(err)})
	}
	return Analysis{}, lastErr
}

func buildAnalysisPrompt(record RetroRecord, findings []Finding) []chatMessage {
	system := "You are Hive's retro analyst. Analyze only the supplied compact post-completion record and deterministic finding types. " +
		"Return ONLY one JSON object with keys root_cause_hypothesis, process_improvement, generalizable, generalizable_lesson. " +
		"Use concise, non-secret, process-level language. Do not invent facts. If the lesson is too specific to this bead, set generalizable=false and generalizable_lesson=\"\"."
	var b strings.Builder
	fmt.Fprintf(&b, "RETRO_RECORD\nbead_id: %s\ntitle: %s\ntype: %s\nactor: %s\nissue: %s\npr: %s\npr_state: %s\nkicks_received: %d\nci_failures: %d\nfix_attempts: %d\ndrift_pauses: %d\nclaim_to_close: %s\n\n",
		record.BeadID, truncate(record.Title, maxRecordTitleChars), record.Type, record.Actor, record.IssueRef, record.PRRef, record.PRState,
		record.KicksReceived, record.CIFailureCount, record.FixAttempts, record.DriftPauses, record.ClaimToClose.String())
	b.WriteString("DETERMINISTIC_FINDINGS\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- pattern: %s\n  severity: %s\n  detail: %s\n", f.Pattern, f.Severity, truncate(f.Detail, maxFindingDetailChars))
	}
	user := truncate(b.String(), maxPromptChars)
	return []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}
}

func ParseAnalysis(content string) (Analysis, error) {
	raw := extractJSONObject(content)
	if raw == "" {
		return Analysis{}, fmt.Errorf("no JSON object in retro analysis reply")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var a Analysis
	if err := dec.Decode(&a); err != nil {
		return Analysis{}, fmt.Errorf("parse retro analysis: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Analysis{}, fmt.Errorf("retro analysis must contain exactly one JSON object")
	}
	a.RootCauseHypothesis = strings.TrimSpace(a.RootCauseHypothesis)
	a.ProcessImprovement = strings.TrimSpace(a.ProcessImprovement)
	a.GeneralizableLesson = strings.TrimSpace(a.GeneralizableLesson)
	if a.RootCauseHypothesis == "" {
		return Analysis{}, fmt.Errorf("root_cause_hypothesis is required")
	}
	if a.ProcessImprovement == "" {
		return Analysis{}, fmt.Errorf("process_improvement is required")
	}
	if runeLen(a.RootCauseHypothesis) > MaxAnalysisFieldChars {
		return Analysis{}, fmt.Errorf("root_cause_hypothesis must be at most %d characters", MaxAnalysisFieldChars)
	}
	if runeLen(a.ProcessImprovement) > MaxAnalysisFieldChars {
		return Analysis{}, fmt.Errorf("process_improvement must be at most %d characters", MaxAnalysisFieldChars)
	}
	if a.Generalizable {
		if runeLen(a.GeneralizableLesson) < MinLessonChars {
			return Analysis{}, fmt.Errorf("generalizable_lesson must be at least %d characters when generalizable", MinLessonChars)
		}
		if runeLen(a.GeneralizableLesson) > MaxLessonChars {
			return Analysis{}, fmt.Errorf("generalizable_lesson must be at most %d characters", MaxLessonChars)
		}
	} else {
		a.GeneralizableLesson = ""
	}
	return a, nil
}

func correctiveAnalysisPrompt(err error) string {
	return fmt.Sprintf("Your previous retro analysis was invalid: %s. Return exactly one JSON object with required string fields root_cause_hypothesis and process_improvement, boolean generalizable, and string generalizable_lesson. Keep each string within documented bounds. Hive will retry validation at most %d times.", err, outputschema.MaxValidationRetries)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatClient struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
}

func (c *openAIChatClient) Complete(ctx context.Context, messages []chatMessage, maxTokens int) (string, error) {
	body, err := json.Marshal(chatRequest{Model: c.model, Messages: messages, Temperature: 0, MaxTokens: maxTokens})
	if err != nil {
		return "", fmt.Errorf("marshal retro analysis request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, analysisTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build retro analysis request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("retro analysis request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("retro analyzer returned %d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read retro analysis response: %w", err)
	}
	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", fmt.Errorf("decode retro analysis response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("retro analyzer returned no choices")
	}
	return cr.Choices[0].Message.Content, nil
}

type chatRequest struct {
	Model       string        `json:"model,omitempty"`
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
}

func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func runeLen(s string) int {
	return len([]rune(strings.TrimSpace(s)))
}
