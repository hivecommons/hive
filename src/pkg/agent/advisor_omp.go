package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

// This file is the OMP adapter's launch-side half of the advisor lane
// (hivecommons/hive#9723, phase 2 of #9638): a `session_stop` extension passed
// with `--extension <file>` and a roles overlay passed with `--config <file>`.
// Projection at launch, never a config-file edit — both files live in a
// directory hive owns under the agent's home and are rewritten on every
// launch; OMP rewrites its own config.yml and strips comments, so the
// operator's file is never touched. The extension shells out to the same
// `hive advisor-hook` command the Claude adapter uses and relays its answer
// in OMP's dialect (`decision: "block"` with a reason).

const (
	ompAdvisorDirName       = ".hive-advisor"
	ompAdvisorExtensionFile = "session-stop.ts"
	ompAdvisorRolesFile     = "roles.yml"
	ompAdvisorDirMode       = 0o755
	ompAdvisorFileMode      = 0o644
)

// ompAdvisorDir resolves the hive-owned directory holding one agent's OMP
// advisor files. Overridable in tests.
var ompAdvisorDir = func(agentName string) string {
	return filepath.Join(interactiveHomePath(agentName), ompAdvisorDirName)
}

// SetAdvisorRolesResolver injects the live model_roles map. Called from
// main.go with a closure over the live config, so a role edited from
// hive.yaml applies on the agent's next launch. A nil fn clears it.
func (m *Manager) SetAdvisorRolesResolver(fn func() map[string]config.ModelRole) {
	if fn == nil {
		m.advisorRolesResolver.Store(nil)
		return
	}
	m.advisorRolesResolver.Store(&fn)
}

func (m *Manager) advisorRoles() map[string]config.ModelRole {
	fnp := m.advisorRolesResolver.Load()
	if fnp == nil || *fnp == nil {
		return nil
	}
	return (*fnp)()
}

// ompAdvisorExtension renders the extension source hive owns. The hook gets
// Claude's Stop-hook payload shape (session_id, transcript_path,
// stop_hook_active) so one hook command serves both backends. Every failure
// fails open: an advisor outage must never stall the agent.
func ompAdvisorExtension() string {
	binary, _ := json.Marshal(advisorHookRunnerBinary())
	subcommand, _ := json.Marshal(advisor.HookSubcommand)
	return fmt.Sprintf(`import { spawn } from "node:child_process";

const HOOK_BINARY = %s;
const HOOK_ARGS = [%s];
const HOOK_TIMEOUT_MS = 90000;

function runHook(payload, signal) {
  return new Promise(resolve => {
    let out = "";
    let child;
    try {
      child = spawn(HOOK_BINARY, HOOK_ARGS, { stdio: ["pipe", "pipe", "ignore"], signal, timeout: HOOK_TIMEOUT_MS });
    } catch {
      resolve("");
      return;
    }
    child.on("error", () => resolve(""));
    child.stdout.on("data", chunk => {
      out += chunk;
    });
    child.on("close", () => resolve(out));
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload));
  });
}

export default function (pi) {
  pi.on("session_stop", async event => {
    if (!event.session_file) return undefined;
    const out = await runHook(
      {
        session_id: event.session_id,
        transcript_path: event.session_file,
        stop_hook_active: event.stop_hook_active,
      },
      event.signal,
    );
    try {
      const answer = JSON.parse(out.trim());
      if (answer && answer.decision === "block" && answer.reason) {
        return { decision: "block", reason: answer.reason };
      }
    } catch {}
    return undefined;
  });
}
`, binary, subcommand)
}

// ompAdvisorRolesOverlay renders the roles overlay: the operator's
// model_roles projected as OMP `modelRoles` selectors
// (`<model>[:<effort>]`). Roles pinned to another backend are skipped. Returns
// "" when no role applies. Strings are JSON-quoted, which is valid YAML.
func ompAdvisorRolesOverlay(roles map[string]config.ModelRole) string {
	names := make([]string, 0, len(roles))
	for name, role := range roles {
		backend := strings.TrimSpace(role.Backend)
		if backend != "" && !strings.EqualFold(backend, config.AdvisorOMPBackend) {
			continue
		}
		if strings.TrimSpace(name) == "" || strings.TrimSpace(role.Model) == "" {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("modelRoles:\n")
	for _, name := range names {
		role := roles[name]
		selector := normalizeModelNameForBackend(strings.TrimSpace(role.Model), config.AdvisorOMPBackend, false)
		if effort := strings.TrimSpace(role.ReasoningEffort); effort != "" {
			selector += ":" + effort
		}
		key, _ := json.Marshal(name)
		val, _ := json.Marshal(selector)
		fmt.Fprintf(&b, "  %s: %s\n", key, val)
	}
	return b.String()
}

// ompAdvisorLaunchFlag renders the hive-owned extension (and roles overlay,
// when roles are defined) for one OMP launch and returns the launch-command
// suffix that passes them. Any failure to render returns "": the agent
// launches normally without the advisor.
func (m *Manager) ompAdvisorLaunchFlag(agent *AgentProcess) string {
	dir := ompAdvisorDir(agent.Name)
	if err := os.MkdirAll(dir, ompAdvisorDirMode); err != nil {
		m.logger.Warn("advisor: cannot create OMP advisor dir; agent launches without it",
			"agent", agent.Name, "dir", dir, "error", err)
		return ""
	}
	m.ownAgentDir(agent.Name, dir, agent.UID)
	extPath := filepath.Join(dir, ompAdvisorExtensionFile)
	if err := os.WriteFile(extPath, []byte(ompAdvisorExtension()), ompAdvisorFileMode); err != nil {
		m.logger.Warn("advisor: cannot write OMP extension; agent launches without it",
			"agent", agent.Name, "path", extPath, "error", err)
		return ""
	}
	flag := " --extension " + shellQuote(extPath)
	if overlay := ompAdvisorRolesOverlay(m.advisorRoles()); overlay != "" {
		rolesPath := filepath.Join(dir, ompAdvisorRolesFile)
		if err := os.WriteFile(rolesPath, []byte(overlay), ompAdvisorFileMode); err != nil {
			m.logger.Warn("advisor: cannot write OMP roles overlay; launching without it",
				"agent", agent.Name, "path", rolesPath, "error", err)
		} else {
			flag += " --config " + shellQuote(rolesPath)
		}
	}
	return flag
}
