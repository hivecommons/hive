// Package jev gives agents a typed-decision tool backed by Jev (TypeSafe AI's
// "System One" model) — hivecommons/hive#8939.
//
// Shape: an agent with jev_mode: assist runs `hive jev decide …`, which POSTs
// the question to the hive's LOCAL decision endpoint (Server, loopback only).
// The hive identifies the calling agent from the socket UID, checks that
// agent's jev_mode, attaches the hive's Jev key, forwards to the provider,
// records the input tokens against the agent in the same sink the governor
// budget reads, audits the call, and returns {answer, confidence,
// probabilities}. The agent never holds the key and never reaches the
// provider directly. With jev_mode off (the default) nothing here runs for
// that agent: no skill is installed and the endpoint refuses its calls.
//
// Wire contract (docs.typesafe.ai/primitives, docs.typesafe.ai/api): one POST
// with {state, model, questions:{id:{type, instructions, criteria}}}; answers
// come back under the same id with usage.input_tokens. The three primitives:
//
//	choice  criteria = {option: description}      → choice, probabilities, confidence
//	score   criteria = [level0, level1, …] (2–10)  → score, probabilities, legend, confidence
//	noul    criteria = {true: …, false: …} (opt.)  → noul (P(yes), 0–1); no confidence
//
// The agent-facing type names follow the issue: choice | score | probability,
// where probability is TypeSafe's Noul.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Decision types an agent may ask for.
const (
	TypeChoice      = "choice"
	TypeScore       = "score"
	TypeProbability = "probability"

	// wireNoul is the provider's name for the yes/no probability primitive.
	wireNoul = "noul"
)

// Input bounds. Jev bills input tokens only, but the agent's budget pays for
// them, and an unbounded state blob is the easy way to waste it. MaxLevels is
// the provider's own cap on Score levels.
const (
	MaxQuestionBytes = 4 << 10
	MaxStateBytes    = 64 << 10
	MaxOptions       = 32
	MaxOptionBytes   = 256
	MinLevels        = 2
	MaxLevels        = 10
	MaxLevelBytes    = 512
	maxResponseBytes = 64 << 10
	maxErrBody       = 4 << 10

	// questionKey names the single question in the systemone request; the
	// answer comes back under the same key.
	questionKey = "decision"
)

// Request is one typed decision as the agent poses it.
type Request struct {
	// Question is the instruction Jev decides on.
	Question string `json:"question"`
	// Type is choice | score | probability ("noul" is accepted as an alias).
	Type string `json:"type"`
	// Options are the candidates for a choice (2–32).
	Options []string `json:"options,omitempty"`
	// Descriptions optionally describe choice options (option → description).
	Descriptions map[string]string `json:"descriptions,omitempty"`
	// Levels are the ordered rubric levels for a score, low to high (2–10).
	Levels []string `json:"levels,omitempty"`
	// State is arbitrary JSON context (issue body, diff summary, file list…).
	State json.RawMessage `json:"state,omitempty"`
	// Criteria optionally replaces the criteria object derived from
	// Options/Levels. For probability it is the {true, false} description pair.
	Criteria json.RawMessage `json:"criteria,omitempty"`
}

// Result is what the agent gets back.
type Result struct {
	// Answer is the chosen option, the score position, or P(yes), as text.
	Answer string `json:"answer"`
	// Confidence is the provider's confidence for choice and score. Noul has
	// none by contract; the hive derives |2·P(yes) − 1| so 0.5 reads as 0 and
	// a certain yes or no reads as 1, the same "how peaked is the
	// distribution" meaning confidence carries for the other two types.
	Confidence float64 `json:"confidence"`
	// Probabilities: per option (choice), per level number (score), or
	// {"yes","no"} (probability).
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Legend maps score level numbers back to their descriptions.
	Legend      map[string]string `json:"legend,omitempty"`
	Model       string            `json:"model"`
	InputTokens int               `json:"input_tokens"`
}

// Validate rejects a request the provider could not answer or that exceeds the
// input bounds. It normalizes Type and trims whitespace in place.
func (r *Request) Validate() error {
	r.Question = strings.TrimSpace(r.Question)
	r.Type = strings.ToLower(strings.TrimSpace(r.Type))
	if r.Type == wireNoul {
		r.Type = TypeProbability
	}
	if r.Question == "" {
		return errors.New("question is required")
	}
	if len(r.Question) > MaxQuestionBytes {
		return fmt.Errorf("question exceeds %d bytes", MaxQuestionBytes)
	}
	switch r.Type {
	case TypeChoice:
		if len(r.Levels) > 0 {
			return fmt.Errorf("levels only apply to type %s", TypeScore)
		}
		if len(r.Options) < 2 {
			return errors.New("a choice needs at least two options")
		}
		if len(r.Options) > MaxOptions {
			return fmt.Errorf("at most %d options", MaxOptions)
		}
		if err := normalizeList(r.Options, "option", MaxOptionBytes, true); err != nil {
			return err
		}
		for k := range r.Descriptions {
			if !containsString(r.Options, k) {
				return fmt.Errorf("description for unknown option %q", k)
			}
		}
	case TypeScore:
		if len(r.Options) > 0 || len(r.Descriptions) > 0 {
			return fmt.Errorf("options only apply to type %s", TypeChoice)
		}
		if len(r.Levels) < MinLevels || len(r.Levels) > MaxLevels {
			return fmt.Errorf("a score needs %d to %d ordered levels", MinLevels, MaxLevels)
		}
		if err := normalizeList(r.Levels, "level", MaxLevelBytes, false); err != nil {
			return err
		}
	case TypeProbability:
		if len(r.Options) > 0 || len(r.Descriptions) > 0 {
			return fmt.Errorf("options only apply to type %s", TypeChoice)
		}
		if len(r.Levels) > 0 {
			return fmt.Errorf("levels only apply to type %s", TypeScore)
		}
	default:
		return fmt.Errorf("type must be %s, %s or %s", TypeChoice, TypeScore, TypeProbability)
	}
	if len(r.State) > MaxStateBytes {
		return fmt.Errorf("state exceeds %d bytes", MaxStateBytes)
	}
	if len(r.State) > 0 && !json.Valid(r.State) {
		return errors.New("state must be valid JSON")
	}
	if len(r.Criteria) > 0 && !json.Valid(r.Criteria) {
		return errors.New("criteria must be valid JSON")
	}
	return nil
}

func normalizeList(list []string, what string, maxBytes int, unique bool) error {
	seen := make(map[string]struct{}, len(list))
	for i, v := range list {
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("%s %d is empty", what, i)
		}
		if len(v) > maxBytes {
			return fmt.Errorf("%s %d exceeds %d bytes", what, i, maxBytes)
		}
		if unique {
			if _, dup := seen[v]; dup {
				return fmt.Errorf("duplicate %s %q", what, v)
			}
			seen[v] = struct{}{}
		}
		list[i] = v
	}
	return nil
}

// criteria builds the provider criteria for the request unless the caller
// supplied one verbatim.
func (r Request) criteria() (json.RawMessage, error) {
	if len(r.Criteria) > 0 {
		return r.Criteria, nil
	}
	switch r.Type {
	case TypeChoice:
		m := make(map[string]*string, len(r.Options))
		for _, o := range r.Options {
			if d, ok := r.Descriptions[o]; ok && strings.TrimSpace(d) != "" {
				desc := d
				m[o] = &desc
			} else {
				m[o] = nil
			}
		}
		return json.Marshal(m)
	case TypeScore:
		return json.Marshal(r.Levels)
	default:
		return nil, nil
	}
}

// Client posts decisions to the Jev provider. Safe for concurrent use. The
// config is read per call so a reloaded hive.yaml (the composition root swaps
// the live config in place) takes effect without a restart.
type Client struct {
	cfg  func() config.JevConfig
	http *http.Client
}

// NewClient returns a client reading cfg per call; a nil hc uses
// http.DefaultClient.
func NewClient(cfg func() config.JevConfig, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	if cfg == nil {
		cfg = func() config.JevConfig { return config.JevConfig{} }
	}
	return &Client{cfg: cfg, http: hc}
}

// Model returns the model id every decision is billed under.
func (c *Client) Model() string { return c.cfg().EffectiveModel() }

type wireRequest struct {
	State     json.RawMessage         `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Noul          *float64           `json:"noul"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Legend        map[string]string  `json:"legend"`
}

// Decide sends one validated request with apiKey and returns the typed answer.
// The caller bounds ctx (see config.JevConfig.EffectiveTimeout).
func (c *Client) Decide(ctx context.Context, req Request, apiKey string) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(apiKey) == "" {
		return Result{}, errors.New("no Jev API key configured")
	}
	criteria, err := req.criteria()
	if err != nil {
		return Result{}, err
	}
	wireType := req.Type
	if wireType == TypeProbability {
		wireType = wireNoul
	}
	state := req.State
	if len(state) == 0 {
		state = json.RawMessage(`{}`)
	}
	cfg := c.cfg()
	body, err := json.Marshal(wireRequest{
		State: state,
		Model: cfg.EffectiveModel(),
		Questions: map[string]wireQuestion{
			questionKey: {Type: wireType, Instructions: req.Question, Criteria: criteria},
		},
	})
	if err != nil {
		return Result{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.EffectiveEndpoint(), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := c.http.Do(hreq)
	if err != nil {
		return Result{}, fmt.Errorf("jev request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return Result{}, fmt.Errorf("jev returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var decoded wireResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return Result{}, fmt.Errorf("jev response: %w", err)
	}
	ans, ok := decoded.Answers[questionKey]
	if !ok {
		return Result{}, errors.New("jev response carried no answer")
	}
	res := Result{Model: decoded.Model, InputTokens: decoded.Usage.InputTokens}
	if res.Model == "" {
		res.Model = cfg.EffectiveModel()
	}
	switch req.Type {
	case TypeChoice:
		choice := strings.TrimSpace(ans.Choice)
		if choice == "" {
			return Result{}, errors.New("jev response carried no choice")
		}
		if !containsString(req.Options, choice) {
			return Result{}, fmt.Errorf("jev chose %q, which is not one of the offered options", choice)
		}
		res.Answer = choice
		res.Confidence = ans.Confidence
		res.Probabilities = ans.Probabilities
		if res.Confidence == 0 && ans.Probabilities != nil {
			res.Confidence = ans.Probabilities[choice]
		}
	case TypeScore:
		if ans.Score == nil {
			return Result{}, errors.New("jev response carried no score")
		}
		if *ans.Score < 0 || *ans.Score > float64(len(req.Levels)-1) {
			return Result{}, fmt.Errorf("jev scored %v, outside the offered levels 0–%d", *ans.Score, len(req.Levels)-1)
		}
		res.Answer = formatFloat(*ans.Score)
		res.Confidence = ans.Confidence
		res.Probabilities = ans.Probabilities
		res.Legend = ans.Legend
	default:
		if ans.Noul == nil {
			return Result{}, errors.New("jev response carried no noul probability")
		}
		p := *ans.Noul
		if p < 0 || p > 1 || math.IsNaN(p) {
			return Result{}, fmt.Errorf("jev returned probability %v outside 0–1", p)
		}
		res.Answer = formatFloat(p)
		res.Confidence = round6(math.Abs(2*p - 1))
		res.Probabilities = map[string]float64{"yes": p, "no": round6(1 - p)}
	}
	return res, nil
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// round6 trims binary float noise from derived values (1−0.93 prints as
// 0.06999999999999995 otherwise) without touching provider-reported numbers.
func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
