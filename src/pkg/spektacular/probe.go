package spektacular

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const versionProbeTimeout = 5 * time.Second

// Probe reports whether the configured Spektacular binary is present and which
// version it prints. It intentionally uses the cwd-independent --version flag;
// `version check` is project-scoped and requires a .spektacular directory.
type ProbeResult struct {
	Present bool
	Version string
	Binary  string
}

func Probe(ctx context.Context, binary string) (ProbeResult, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = "spektacular"
	}
	result := ProbeResult{Binary: binary}
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return result, fmt.Errorf("spektacular --version: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	result.Present = true
	result.Version = strings.TrimSpace(stdout.String())
	return result, nil
}
