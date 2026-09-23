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
	"anthropic/claude-sonnet-5",
	"google-antigravity/gemini-3.7-flash",
}

var ompStaticReasoningEfforts = map[string][]string{
	"openai-codex/gpt-5.6-terra":          {"minimal", "low", "medium", "high", "xhigh", "max"},
	"openai-codex/gpt-5.6-sol":            {"minimal", "low", "medium", "high", "xhigh", "max"},
	"openai-codex/gpt-5.6-luna":           {"minimal", "low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-opus-5":             {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-fable-5":            {"low", "medium", "high", "xhigh", "max"},
	"anthropic/claude-sonnet-5":           {"low", "medium", "high", "xhigh", "max"},
	"google-antigravity/gemini-3.7-flash": {"low", "medium", "high"},
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

func (s *Server) discoverOmpModels() cliModelResult {
	models, efforts, err := runOmpModelsProbe()
	if err != nil || len(models) == 0 {
		if err != nil && s.logger != nil {
			s.logger.Info("omp model discovery unavailable, serving static fallback", "err", err.Error())
		}
		return cliModelResult{fallback: true, reasoningEfforts: cloneEffortMap(ompStaticReasoningEfforts)}
	}
	return cliModelResult{models: dedupeModels(models), reasoningEfforts: efforts, fallback: false}
}

func execOmpModelsProbe() ([]string, map[string][]string, error) {
	bin, err := exec.LookPath(ompBinaryName)
	if err != nil {
		return nil, nil, fmt.Errorf("omp binary not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ompModelsProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, ompModelsSubcommand, ompModelsJSONFlag)
	if home := ompProbeHome(); home != "" {
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

func ompProbeHome() string {
	for _, home := range []string{"/data/home", os.Getenv("HOME")} {
		if home == "" {
			continue
		}
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			return home
		}
	}
	return ""
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
