package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	ompBackendID          = "omp"
	ompBinaryName         = "omp"
	ompModelsSubcommand   = "models"
	ompModelsJSONFlag     = "--json"
	ompModelsProbeTimeout = 5 * time.Second
)

var ompStaticModels = []string{
	"openai-codex/gpt-5.6-terra",
	"openai-codex/gpt-5.6-sol",
	"openai-codex/gpt-5.6-luna",
	"anthropic/claude-opus-5",
	"anthropic/claude-fable-5",
	"anthropic/claude-sonnet-5-5",
	"anthropic/claude-sonnet-5",
	"google-antigravity/gemini-3.7-flash",
}

var ompStaticReasoningEfforts = map[string][]string{
	"openai-codex/gpt-5.6-terra":          {"low", "medium", "high", "xhigh", "max"},
	"openai-codex/gpt-5.6-sol":            {"low", "medium", "high", "xhigh", "max"},
	"openai-codex/gpt-5.6-luna":           {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-opus-5":             {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-fable-5":            {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-sonnet-5-5":         {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-sonnet-5":           {"low", "medium", "high", "xhigh", "max"},
	"google-antigravity/gemini-3.7-flash": {"minimal", "low", "medium", "high"},
}

type ompModelEntry struct {
	Selector  string   `json:"selector"`
	Reasoning bool     `json:"reasoning"`
	Thinking  []string `json:"thinking"`
}

type ompModelsPayload struct {
	Models []ompModelEntry `json:"models"`
}

var runOmpModelsProbe = execOmpModelsProbe

// ompAgentHomes lists the omp agents' HOMEs, where their sign-ins live.
func (s *Server) ompAgentHomes() []string {
	if s == nil || s.deps == nil || s.deps.AgentMgr == nil {
		return nil
	}
	return s.deps.AgentMgr.OmpAgentHomes()
}

// discoverOmpModels probes each omp agent's own HOME and unions the results,
// since the model cache is keyed by backend, not by agent. With no omp agent
// it probes the process HOME. Any agent without a sign-in contributes nothing.
func (s *Server) discoverOmpModels() cliModelResult {
	homes := s.ompAgentHomes()
	if len(homes) == 0 {
		homes = []string{ompFallbackProbeHome()}
	}
	var models []string
	efforts := map[string][]string{}
	var lastErr error
	for _, home := range homes {
		m, e, err := runOmpModelsProbe(home)
		if err != nil || len(m) == 0 {
			if err == nil {
				err = errors.New("omp models returned no selectors")
			}
			lastErr = err
			if s.logger != nil {
				s.logger.Warn("omp model discovery found no models for this HOME", "home", home, "err", err.Error())
			}
			continue
		}
		models = append(models, m...)
		for id, levels := range e {
			if _, ok := efforts[id]; !ok {
				efforts[id] = levels
			}
		}
	}
	if len(models) == 0 {
		if s.logger != nil {
			s.logger.Warn("omp model discovery unavailable, serving static fallback", "homes", homes, "err", fmt.Sprint(lastErr))
		}
		return cliModelResult{fallback: true, reasoningEfforts: cloneEffortMap(ompStaticReasoningEfforts)}
	}
	return cliModelResult{models: dedupeModels(models), reasoningEfforts: efforts, fallback: false}
}

func execOmpModelsProbe(home string) ([]string, map[string][]string, error) {
	bin, err := exec.LookPath(ompBinaryName)
	if err != nil {
		return nil, nil, fmt.Errorf("omp binary not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ompModelsProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, ompModelsSubcommand, ompModelsJSONFlag)
	if st, err := os.Stat(home); err == nil && st.IsDir() {
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Dir = home
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("omp models timed out: %w", ctx.Err())
		}
		return nil, nil, fmt.Errorf("omp models failed: %w", err)
	}
	return parseOmpModelsJSON(stdout.Bytes())
}

func ompFallbackProbeHome() string {
	return os.Getenv("HOME")
}

func parseOmpModelsJSON(raw []byte) ([]string, map[string][]string, error) {
	var payload ompModelsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		var arr []ompModelEntry
		if arrErr := json.Unmarshal(raw, &arr); arrErr != nil {
			return nil, nil, fmt.Errorf("parse omp models json: %w", err)
		}
		payload.Models = arr
	}
	var models []string
	efforts := map[string][]string{}
	for _, m := range payload.Models {
		id := strings.TrimSpace(m.Selector)
		if id == "" {
			continue
		}
		models = append(models, id)
		if m.Reasoning && len(m.Thinking) > 0 {
			efforts[id] = dedupeModels(m.Thinking)
		}
	}
	models = dedupeModels(models)
	if len(models) == 0 {
		return nil, nil, errors.New("omp models returned no selectors")
	}
	return models, efforts, nil
}

func cloneEffortMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (s *Server) validateAgentReasoningEffort(backend, model, effort string) error {
	if effort == "" {
		return nil
	}
	if backend != ompBackendID {
		return config.ValidateReasoningEffort(backend, effort)
	}
	levels := s.ompReasoningEffortsForModel(model)
	if len(levels) == 0 {
		return fmt.Errorf("model %s does not expose omp thinking levels", model)
	}
	for _, level := range levels {
		if level == effort {
			return nil
		}
	}
	return fmt.Errorf("invalid reasoning effort %q for omp model %s (accepted: %s)", effort, model, strings.Join(levels, ", "))
}

func (s *Server) ompReasoningEffortsForModel(model string) []string {
	if s != nil && s.cliModels != nil {
		if r, ok := s.cliModels.get(ompBackendID); ok && len(r.reasoningEfforts) > 0 {
			if levels := matchOmpEffortLevels(r.reasoningEfforts, model); len(levels) > 0 {
				return levels
			}
		}
	}
	return matchOmpEffortLevels(ompStaticReasoningEfforts, model)
}

func matchOmpEffortLevels(efforts map[string][]string, model string) []string {
	if model == "" {
		return nil
	}
	if levels := efforts[model]; len(levels) > 0 {
		return append([]string(nil), levels...)
	}
	for id, levels := range efforts {
		if strings.EqualFold(id, model) || strings.EqualFold(strings.TrimPrefix(id, providerPrefix(id)), strings.TrimPrefix(model, providerPrefix(model))) {
			return append([]string(nil), levels...)
		}
	}
	return nil
}

func providerPrefix(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[:i+1]
	}
	return ""
}
