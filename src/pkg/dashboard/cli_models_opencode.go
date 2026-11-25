package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	opencodeBackendID          = "opencode"
	opencodeBinaryName         = "opencode"
	opencodeModelsSubcommand   = "models"
	opencodeModelsProbeTimeout = 15 * time.Second
)

// opencodeStaticModels is the fallback served when `opencode models` cannot
// run. These are the credential-free models OpenCode ships (opencode/*-free),
// so a spoke with no provider sign-in still has something launchable.
var opencodeStaticModels = []string{
	"opencode/hy4-flash-free",
	"opencode/deepseek-v4.1-flash-free",
	"opencode/mimo-v2.6-flash-free",
	"opencode/nemotron-3.5-super-vl-free",
}

var runOpencodeModelsProbe = execOpencodeModelsProbe

// discoverOpencodeModels lists the provider/model selectors the installed
// opencode can use, via `opencode models` (one selector per line). The probe is
// best-effort: any failure serves the static credential-free list as a fallback.
func (s *Server) discoverOpencodeModels() cliModelResult {
	models, err := runOpencodeModelsProbe()
	if err != nil || len(models) == 0 {
		if err == nil {
			err = errors.New("opencode models returned no selectors")
		}
		if s.logger != nil {
			s.logger.Info("opencode model discovery unavailable, serving static fallback", "err", err.Error())
		}
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

func execOpencodeModelsProbe() ([]string, error) {
	bin, err := exec.LookPath(opencodeBinaryName)
	if err != nil {
		return nil, fmt.Errorf("opencode binary not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opencodeModelsProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, opencodeModelsSubcommand)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	return parseOpencodeModels(stdout.String()), nil
}

// parseOpencodeModels keeps only `provider/model` selector lines, dropping
// blanks and any banner or log noise the CLI prints around them.
func parseOpencodeModels(out string) []string {
	var models []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.ContainsAny(line, " \t") || strings.Count(line, "/") < 1 {
			continue
		}
		models = append(models, line)
	}
	return models
}
