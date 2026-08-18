package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// QualityRuntimeProbeResult is the audit-safe result of executing one harmless
// read-only Codex tool call through the exact ordinary Quality agent runtime.
// It contains identities and digests only; no credential or model output is
// retained.
type QualityRuntimeProbeResult struct {
	Agent          string `json:"agent"`
	UID            int    `json:"uid"`
	Home           string `json:"home"`
	CodexHome      string `json:"codex_home"`
	Backend        string `json:"backend"`
	Model          string `json:"model"`
	CommandSHA256  string `json:"command_sha256"`
	ApprovalPolicy string `json:"approval_policy"`
	ToolCall       string `json:"tool_call"`
	OutputSHA256   string `json:"output_sha256"`
}

var (
	qualityProbeResolveBackend = backendBinary
	qualityProbeRandomRead     = rand.Read
	qualityProbeHashCommand    = hashOrdinaryFile
	qualityProbeExecute        = executeQualityProbe
)

// ProbeQualityRuntime starts no tmux session and changes no repository state.
// It invokes the same Codex binary, UID, HOME, CODEX_HOME, model, sandbox, and
// unattended approval policy used by the configured ordinary Quality agent.
// Success requires Codex's JSON event stream to show the nonce path and the
// exact nonce read from that path; a prose-only answer cannot satisfy it.
func (m *Manager) ProbeQualityRuntime(ctx context.Context) (QualityRuntimeProbeResult, error) {
	if m == nil {
		return QualityRuntimeProbeResult{}, errors.New("Quality runtime manager is unavailable")
	}
	m.mu.RLock()
	configured, ok := m.agents["quality"]
	if !ok || configured == nil {
		m.mu.RUnlock()
		return QualityRuntimeProbeResult{}, errors.New("Quality agent is not configured")
	}
	agent := configured.snapshot()
	backend := effectiveBackend(&agent)
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	} else if agent.PinnedModel != "" {
		model = agent.PinnedModel
	}
	environment := append([]string(nil), m.filteredEnv(&agent)...)
	for _, pair := range m.agentEnvPairs(&agent) {
		environment = replaceEnvironmentValue(environment, pair.Key, pair.Value)
	}
	workDir := filepath.Join(m.workDir, agent.Name)
	m.mu.RUnlock()

	if backend != codexBackend || strings.TrimSpace(model) == "" || agent.UID <= 0 {
		return QualityRuntimeProbeResult{}, fmt.Errorf("Quality probe requires a non-root Codex agent with an exact model; backend=%q model=%q uid=%d", backend, model, agent.UID)
	}
	command, err := qualityProbeResolveBackend(backend)
	if err != nil {
		return QualityRuntimeProbeResult{}, err
	}
	info, err := os.Lstat(workDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return QualityRuntimeProbeResult{}, errors.New("Quality agent work directory is unavailable or linked")
	}
	nonceBytes := make([]byte, 24)
	if _, err := qualityProbeRandomRead(nonceBytes); err != nil {
		return QualityRuntimeProbeResult{}, fmt.Errorf("create Quality preflight nonce: %w", err)
	}
	nonce := "HIVE_QUALITY_PREFLIGHT_" + hex.EncodeToString(nonceBytes)
	probe, err := os.CreateTemp(workDir, ".hive-quality-preflight-*.txt")
	if err != nil {
		return QualityRuntimeProbeResult{}, fmt.Errorf("create Quality preflight file: %w", err)
	}
	probePath := probe.Name()
	defer os.Remove(probePath)
	if _, err := probe.WriteString(nonce + "\n"); err != nil {
		_ = probe.Close()
		return QualityRuntimeProbeResult{}, err
	}
	if err := probe.Chmod(0o444); err != nil {
		_ = probe.Close()
		return QualityRuntimeProbeResult{}, err
	}
	if err := probe.Close(); err != nil {
		return QualityRuntimeProbeResult{}, err
	}

	relative := "./" + filepath.Base(probePath)
	trustOverride := fmt.Sprintf(`projects.%q.trust_level="trusted"`, filepath.ToSlash(filepath.Clean(workDir)))
	args := []string{
		"--model", model,
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
		"--disable", "enable_mcp_apps",
		"-c", trustOverride,
		"exec", "--cd", workDir, "--skip-git-repo-check", "--ephemeral", "--json",
		"Use the shell tool exactly once to run `cat " + relative + "`. Then return only the file contents. Do not infer or fabricate the contents.",
	}
	launcher := command
	launcherArgs := args
	if agent.UID > 0 {
		launcher = "su-exec"
		launcherArgs = append([]string{strconv.Itoa(agent.UID), command}, args...)
	}
	for _, name := range ordinaryAgentControlPlaneCredentialNames() {
		environment = removeEnvironmentValue(environment, name)
	}
	environment = removeEnvironmentValue(environment, "HIVE_AGENT_TOKEN_CACHE")
	expectedHome := AgentHome(agent.Name, agent.UID, backend)
	expectedCodexHome := codexHomePath(agent.Name)
	if filepath.Clean(environmentValue(environment, "HOME")) != filepath.Clean(expectedHome) ||
		filepath.Clean(environmentValue(environment, "CODEX_HOME")) != filepath.Clean(expectedCodexHome) {
		return QualityRuntimeProbeResult{}, errors.New("Quality Codex runtime does not use its exact UID-bound HOME and CODEX_HOME")
	}
	stdout, stderr, err := qualityProbeExecute(ctx, launcher, launcherArgs, workDir, environment)
	if err != nil {
		return QualityRuntimeProbeResult{}, fmt.Errorf("Quality Codex unattended tool probe failed: %w: %s", err, boundedQualityProbeDiagnostic(string(stderr)))
	}
	output := string(stdout)
	if !strings.Contains(output, filepath.Base(probePath)) || !strings.Contains(output, nonce) {
		return QualityRuntimeProbeResult{}, errors.New("Quality Codex probe did not prove the exact local read-only tool call")
	}
	if strings.Contains(strings.ToLower(output+string(stderr)), "approval required") || strings.Contains(strings.ToLower(output+string(stderr)), "waiting for approval") {
		return QualityRuntimeProbeResult{}, errors.New("Quality Codex probe encountered an interactive approval")
	}
	commandHash, err := qualityProbeHashCommand(command)
	if err != nil {
		return QualityRuntimeProbeResult{}, err
	}
	outputHash := sha256.Sum256(stdout)
	return QualityRuntimeProbeResult{
		Agent: agent.Name, UID: agent.UID, Home: environmentValue(environment, "HOME"), CodexHome: environmentValue(environment, "CODEX_HOME"),
		Backend: backend, Model: model, CommandSHA256: commandHash, ApprovalPolicy: "never", ToolCall: "read-only-local-file",
		OutputSHA256: hex.EncodeToString(outputHash[:]),
	}, nil
}

func executeQualityProbe(ctx context.Context, launcher string, args []string, directory string, environment []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, launcher, args...)
	cmd.Dir = directory
	cmd.Env = environment
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	environment = removeEnvironmentValue(environment, name)
	return append(environment, name+"="+value)
}

func removeEnvironmentValue(environment []string, name string) []string {
	result := environment[:0]
	for _, value := range environment {
		key, _, found := strings.Cut(value, "=")
		if !found || strings.EqualFold(key, name) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func environmentValue(environment []string, name string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, found := strings.Cut(environment[index], "=")
		if found && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func hashOrdinaryFile(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Quality Codex executable is not an ordinary file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || int64(len(data)) != before.Size() {
		return "", errors.New("Quality Codex executable changed while hashing")
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func boundedQualityProbeDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
