package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

// This file is the Claude Code adapter's launch-side half of the advisor lane
// (hivecommons/hive#9722): the `Stop` hook rendered into a settings JSON hive
// passes on the command line at launch. Projection at launch, never a
// config-file edit — hive does not touch the agent's own settings.json, so
// disabling the advisor needs nothing more than the next launch without the
// flag. The hook command itself is `hive advisor-hook` (pkg/advisor/cli.go),
// which talks only to the hive's loopback advise endpoint.

// SetAdvisorEnabledResolver injects the live "is the advisor on for this
// agent" predicate. Called from main.go with a closure over the live config,
// so an advisor toggled from the dashboard applies on the agent's next launch
// without a hive restart — the same discipline as the explain-mode resolver.
// A nil fn clears it, leaving the advisor off for every launch.
func (m *Manager) SetAdvisorEnabledResolver(fn func(agentName string) bool) {
	// Atomic store — no m.mu — so the launch path, which already holds m.mu,
	// can read it lock-free (see explainModeDefaultResolver for the deadlock
	// reasoning this follows).
	if fn == nil {
		m.advisorEnabledResolver.Store(nil)
		return
	}
	m.advisorEnabledResolver.Store(&fn)
}

// advisorEnabledFor reports whether the advisor lane is on for the named
// agent, or false when no resolver was injected (tests / bare setups). Safe
// to call while holding m.mu.
func (m *Manager) advisorEnabledFor(name string) bool {
	fnp := m.advisorEnabledResolver.Load()
	if fnp == nil || *fnp == nil {
		return false
	}
	return (*fnp)(name)
}

// advisorHookRunnerBinary resolves the hive executable the hook command runs
// as, same seam as agyTurnRunnerBinary: the running executable when
// resolvable, PATH lookup as the fallback. Overridable in tests.
var advisorHookRunnerBinary = func() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "hive"
}

// claudeAdvisorSettings renders the compact settings JSON that registers the
// advisor's Stop hook for one Claude Code launch. The timeout gives the hook
// room for the hub-side review timeout plus the round trip.
func claudeAdvisorSettings() string {
	command := fmt.Sprintf("%s %s", advisorHookRunnerBinary(), advisor.HookSubcommand)
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []map[string]any{
				{
					"hooks": []map[string]any{
						{"type": "command", "command": command, "timeout": 90},
					},
				},
			},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "" // cannot happen for this literal shape; fail open regardless
	}
	return string(data)
}

// advisorHooksFileName is the hive-owned Copilot hook file, dropped into the
// hooks directory under the COPILOT_HOME hive points at the agent's own
// per-agent .copilot directory. codexHooksFileName is Codex's hooks.json in
// the per-agent CODEX_HOME hive provisions. Neither is a file the operator
// maintains. Hive never writes config.json or config.toml itself; the one
// config.toml change is Codex's own hook-trust entry, which Codex writes at
// hive's request, as the agent (see trustCodexAdvisorHook).
const (
	copilotAdvisorHooksFileName = "hive-advisor.json"
	codexHooksFileName          = "hooks.json"
	advisorHookTimeoutS         = 90
)

// copilotAdvisorHooksJSON renders the Copilot CLI hook file registering the
// advisor on `agentStop`. The command asks for the Copilot dialect.
func copilotAdvisorHooksJSON() []byte {
	command := fmt.Sprintf("%s %s --format %s", advisorHookRunnerBinary(), advisor.HookSubcommand, advisor.HookFormatCopilot)
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"agentStop": []map[string]any{
				{"type": "command", "bash": command, "timeoutSec": advisorHookTimeoutS},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil // cannot happen for this literal shape; fail open regardless
	}
	return append(data, '\n')
}

// codexAdvisorHooksJSON renders the Codex CLI hooks.json registering the
// advisor's `Stop` hook. Codex speaks the same dialect as Claude Code here.
func codexAdvisorHooksJSON() []byte {
	command := fmt.Sprintf("%s %s --format %s", advisorHookRunnerBinary(), advisor.HookSubcommand, advisor.HookFormatCodex)
	doc := map[string]any{
		"hooks": map[string]any{
			"Stop": []map[string]any{
				{
					"hooks": []map[string]any{
						{"type": "command", "command": command, "timeout": advisorHookTimeoutS},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil
	}
	return append(data, '\n')
}

// advisorFileBackend reports whether the backend receives the advisor through
// hook files in the per-agent home rather than a launch argument.
func advisorFileBackend(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return b == config.AdvisorCopilotBackend || b == config.AdvisorCodexBackend
}

// advisorCopilotHome is the directory hive points COPILOT_HOME at when the
// advisor is on for a Copilot agent: the agent's own per-agent .copilot
// (interactive_home.go), which hive provisions. "" when the agent has no
// per-agent home to own.
func advisorCopilotHome(agent *AgentProcess) string {
	if agent.UID <= 0 || sharedAgentHomeForced() {
		return ""
	}
	return filepath.Join(interactiveHomePath(agent.Name), ".copilot")
}

// advisorHooksPath returns where the advisor's hook file lives for a
// file-projected backend, or "" when the agent has no hive-owned home.
func advisorHooksPath(agent *AgentProcess, backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case config.AdvisorCopilotBackend:
		home := advisorCopilotHome(agent)
		if home == "" {
			return ""
		}
		return filepath.Join(home, "hooks", copilotAdvisorHooksFileName)
	case config.AdvisorCodexBackend:
		if agent.UID <= 0 {
			return ""
		}
		return filepath.Join(codexHomePath(agent.Name), codexHooksFileName)
	}
	return ""
}

// advisorHooksActive reports whether the advisor projects hook files for this
// agent on a Copilot or Codex launch.
func (m *Manager) advisorHooksActive(agent *AgentProcess, backend string) bool {
	return m.advisorEnabledFor(agent.Name) && advisorFileBackend(backend) && advisorHooksPath(agent, backend) != ""
}

// provisionAdvisorHooks writes the advisor's hook file into the hive-owned
// home for a Copilot or Codex launch, or removes a file it wrote earlier when
// the advisor is now off, so disabling needs nothing but the next launch.
// Best-effort like the other home provisioning: a failure is logged and the
// agent launches without the advisor. Call after the homes are provisioned.
func (m *Manager) provisionAdvisorHooks(agent *AgentProcess, backend string) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if !advisorFileBackend(backend) {
		return
	}
	path := advisorHooksPath(agent, backend)
	if path == "" {
		return
	}
	user := m.agentExecUserSpec(agent)
	if !m.advisorHooksActive(agent, backend) {
		if existing, err := readFileAsUser(user, path); err == nil && strings.Contains(string(existing), advisor.HookSubcommand) {
			if err := exec.Command("su-exec", user, "rm", "-f", path).Run(); err != nil {
				m.logger.Warn("failed to remove stale advisor hook file", "agent", agent.Name, "path", path, "error", err)
			}
		}
		return
	}
	content := codexAdvisorHooksJSON()
	if backend == config.AdvisorCopilotBackend {
		content = copilotAdvisorHooksJSON()
	}
	if len(content) == 0 {
		return
	}
	if err := exec.Command("su-exec", user, "mkdir", "-p", filepath.Dir(path)).Run(); err != nil {
		m.logger.Warn("failed to create advisor hook directory; agent launches without the advisor", "agent", agent.Name, "dir", filepath.Dir(path), "error", err)
		return
	}
	if err := writeFileAsUser(user, path, content); err != nil {
		m.logger.Warn("failed to write advisor hook file; agent launches without the advisor", "agent", agent.Name, "path", path, "error", err)
		return
	}
	if backend == config.AdvisorCodexBackend {
		if err := m.trustCodexAdvisorHook(user, path); err != nil {
			m.logger.Warn("failed to trust the advisor's Codex Stop hook; agent launches but Codex may skip the advisor",
				"agent", agent.Name, "path", path, "reason", err)
		}
	}
}

// codexHookTrustTimeout bounds the whole app-server spawn and JSON-RPC
// exchange that trusts the advisor's Codex hook. A var so tests can shrink it.
var codexHookTrustTimeout = 15 * time.Second

const (
	// codexHookTrustMaxLineBytes caps one scanned app-server protocol line.
	codexHookTrustMaxLineBytes  = 1 << 20
	codexHookTrustStatusTrusted = "trusted"
	codexHookStopEvent          = "stop"
)

// codexHookTrustMessage is one newline-delimited app-server message.
type codexHookTrustMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// codexHookListEntry is one hook reported by the app-server's hooks/list.
type codexHookListEntry struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	SourcePath  string `json:"sourcePath"`
	TrustStatus string `json:"trustStatus"`
	CurrentHash string `json:"currentHash"`
}

// trustCodexAdvisorHook asks Codex to trust the advisor's Stop hook in the
// hooks.json at hooksPath (hivecommons/hive#13368). Codex skips untrusted
// hooks and nobody can approve one in hive's unattended pane, so without this
// the advisor never runs on Codex. It drives `codex app-server` the same way
// Codex's own TUI trusts a hook: hooks/list, then config/batchWrite of the
// hook's current hash. Codex computes the hash and writes config.toml itself;
// it runs as the agent so config.toml stays agent-owned
// (healForeignCodexConfig removes a foreign-owned one). Only hive's own hook
// is trusted — never a blanket bypass that would also run project hooks.
func (m *Manager) trustCodexAdvisorHook(userSpec, hooksPath string) error {
	binary, err := m.backendBinary(codexBackend)
	if err != nil {
		return err
	}
	home := filepath.Dir(hooksPath)
	ctx, cancel := context.WithTimeout(context.Background(), codexHookTrustTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "su-exec", userSpec, "env", "CODEX_HOME="+home,
		binary, "app-server", "--listen", "stdio://")
	// Never the manager's own environment: it carries hub-only secrets
	// (HIVE_GITHUB_TOKEN, HIVE_HUB_SECRET, ...) and this process runs as the
	// agent UID, readable via /proc/<pid>/environ and inherited by anything
	// Codex spawns from the agent-writable config.toml. su-exec sets HOME and
	// USER; CODEX_HOME rides the env(1) argv above.
	cmd.Env = agentSubprocessEnv()
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn codex app-server: %w", err)
	}
	// reap kills the app-server and closes hive's ends of both pipes. The
	// stdout close is what unblocks a reader still parked in Scan: cmd.Wait
	// closes it once the process is gone, or after WaitDelay when a Codex
	// descendant inherited the write end and would otherwise hold it open.
	var reapOnce sync.Once
	reap := func() {
		reapOnce.Do(func() {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = stdout.Close()
		})
	}
	defer reap()

	done := make(chan error, 1)
	go func() { done <- codexHookTrustExchange(stdin, stdout, hooksPath) }()
	select {
	case err := <-done:
		if err != nil && ctx.Err() != nil {
			return fmt.Errorf("codex app-server timed out after %s: %w", codexHookTrustTimeout, err)
		}
		return err
	case <-ctx.Done():
		// The scanner read is not context-aware; tear the process and pipes
		// down so the exchange goroutine returns within WaitDelay rather than
		// outliving the timeout behind a descendant that kept stdout.
		reap()
		err := <-done
		if err == nil {
			err = ctx.Err()
		}
		return fmt.Errorf("codex app-server timed out after %s: %w", codexHookTrustTimeout, err)
	}
}

// agentSubprocessEnv is the minimal environment for a short-lived helper hive
// runs as the agent user: PATH so the backend binary and its tools resolve,
// the egress/TLS knobs a network-using helper needs, and nothing from the
// manager's own environment that an agent-UID process must not see. extra
// entries are appended verbatim.
func agentSubprocessEnv(extra ...string) []string {
	env := make([]string, 0, len(agentSubprocessEnvPassthrough)+len(extra))
	for _, key := range agentSubprocessEnvPassthrough {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return append(env, extra...)
}

// agentSubprocessEnvPassthrough lists the manager variables agentSubprocessEnv
// forwards. Deliberately no HIVE_*, token, or secret variable.
var agentSubprocessEnvPassthrough = []string{
	"PATH", "TERM", "LANG", "LC_ALL", "TZ",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

// codexHookTrustExchange runs the JSON-RPC sequence over the app-server's
// stdio. Factored over io.Writer/io.Reader so the protocol is independent of
// the process plumbing.
func codexHookTrustExchange(in io.Writer, out io.Reader, hooksPath string) error {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), codexHookTrustMaxLineBytes)

	if err := writeCodexHookTrustRPC(in, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{"name": "hive", "version": hiveClientVersion()},
		},
	}); err != nil {
		return fmt.Errorf("send initialize: %w", err)
	}
	if _, err := awaitCodexHookTrustResponse(sc, 1, "initialize"); err != nil {
		return err
	}
	if err := writeCodexHookTrustRPC(in, map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return fmt.Errorf("send initialized: %w", err)
	}

	if err := writeCodexHookTrustRPC(in, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "hooks/list",
		"params":  map[string]any{},
	}); err != nil {
		return fmt.Errorf("send hooks/list: %w", err)
	}
	result, err := awaitCodexHookTrustResponse(sc, 2, "hooks/list")
	if err != nil {
		return err
	}
	var list struct {
		Data []struct {
			Hooks []codexHookListEntry `json:"hooks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		return fmt.Errorf("parse hooks/list result: %w", err)
	}
	var hook *codexHookListEntry
	for i := range list.Data {
		for j := range list.Data[i].Hooks {
			h := &list.Data[i].Hooks[j]
			if filepath.Clean(h.SourcePath) == filepath.Clean(hooksPath) && strings.EqualFold(h.EventName, codexHookStopEvent) {
				hook = h
				break
			}
		}
		if hook != nil {
			break
		}
	}
	if hook == nil {
		return fmt.Errorf("hooks/list reported no Stop hook from %s", hooksPath)
	}
	if hook.TrustStatus == codexHookTrustStatusTrusted {
		return nil
	}
	if hook.Key == "" || hook.CurrentHash == "" {
		return fmt.Errorf("hooks/list Stop hook from %s has no key or currentHash", hooksPath)
	}

	if err := writeCodexHookTrustRPC(in, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "config/batchWrite",
		"params": map[string]any{
			"edits": []map[string]any{{
				"keyPath":       "hooks.state",
				"value":         map[string]any{hook.Key: map[string]any{"trusted_hash": hook.CurrentHash}},
				"mergeStrategy": "upsert",
			}},
			"filePath":         nil,
			"expectedVersion":  nil,
			"reloadUserConfig": true,
		},
	}); err != nil {
		return fmt.Errorf("send config/batchWrite: %w", err)
	}
	result, err = awaitCodexHookTrustResponse(sc, 3, "config/batchWrite")
	if err != nil {
		return err
	}
	var write struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(result, &write); err != nil {
		return fmt.Errorf("parse config/batchWrite result: %w", err)
	}
	if write.Status != "ok" {
		return fmt.Errorf("config/batchWrite status %q, want \"ok\"", write.Status)
	}
	return nil
}

func writeCodexHookTrustRPC(in io.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = in.Write(append(b, '\n'))
	return err
}

// awaitCodexHookTrustResponse reads until the response to request id arrives,
// skipping server notifications and server-initiated requests, and returns
// its result.
func awaitCodexHookTrustResponse(sc *bufio.Scanner, id int64, method string) (json.RawMessage, error) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg codexHookTrustMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("%s: malformed app-server line: %w", method, err)
		}
		// A server->client request carries a method and its own id space;
		// skip it even when its id collides with ours.
		if msg.ID == nil || *msg.ID != id || msg.Method != "" {
			continue
		}
		if len(msg.Error) != 0 {
			return nil, fmt.Errorf("%s rejected: %s", method, truncateStr(string(msg.Error), 300))
		}
		return msg.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return nil, fmt.Errorf("%s: codex app-server exited before answering (codex may be too old): %w", method, io.ErrUnexpectedEOF)
}

// hiveClientVersion is the version hive reports in the app-server
// initialize clientInfo. Informational to Codex but required by the protocol.
func hiveClientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

// advisorLaunchFlag returns the launch-command suffix that projects the
// advisor's turn-end hook into the agent's backend, or "" when the advisor is
// off for the agent, the backend has no adapter yet, or the launch shape
// cannot carry it. An advisor enabled on an unsupported backend is refused,
// not silently skipped: the agent still launches normally and the refusal is
// logged and reported by the dashboard's advisor listing. Copilot carries no
// launch flag at all (its hooks directory rides COPILOT_HOME, see
// agentEnvPairs); Codex needs its experimental hooks feature switched on.
func (m *Manager) advisorLaunchFlag(agent *AgentProcess, backend string, isInference bool) string {
	if !m.advisorEnabledFor(agent.Name) {
		return ""
	}
	if !config.AdvisorSupportedBackend(backend) {
		m.logger.Warn("advisor is not active on this backend; agent launches without it",
			"agent", agent.Name, "backend", backend, "supported", config.AdvisorSupportedBackendList)
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(backend), config.AdvisorOMPBackend) {
		return m.ompAdvisorLaunchFlag(agent)
	}
	if isInference {
		// Inference-routed Claude runs bare against the translator with a fixed
		// settings FILE; there is no interactive turn boundary to hook.
		m.logger.Warn("advisor is not active for inference-routed agents", "agent", agent.Name)
		return ""
	}
	if advisorFileBackend(backend) && advisorHooksPath(agent, backend) == "" {
		m.logger.Warn("advisor needs a hive-provisioned per-agent home; agent launches without it",
			"agent", agent.Name, "backend", backend)
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case config.AdvisorCodexBackend:
		return " -c features.hooks=true"
	case config.AdvisorCopilotBackend:
		return ""
	}
	settings := claudeAdvisorSettings()
	if settings == "" {
		return ""
	}
	return " --settings " + shellQuote(settings)
}
