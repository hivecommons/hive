package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// sandboxCopyFromKeys are the General-tab body keys naming the agent whose
// sandbox block a toggle-on copies: the dashboard's camelCase spelling and
// the documented copy_from.
var sandboxCopyFromKeys = []string{"sandboxCopyFrom", "copy_from"}

// sandboxCopyFromBody returns the copy-from agent a General-tab body names,
// or "" for none.
func sandboxCopyFromBody(body map[string]interface{}) string {
	for _, k := range sandboxCopyFromKeys {
		if v, ok := body[k].(string); ok {
			if name := strings.TrimSpace(sanitizeString(v)); name != "" {
				return name
			}
		}
	}
	return ""
}

// sandboxCopySources lists the other agents whose sandbox block is complete
// (their own fields plus agent_sandbox defaults resolve an image and a
// usable runtime), sorted, for the General tab's copy-from picker.
func (s *Server) sandboxCopySources(exclude string) []string {
	out := []string{}
	for name, a := range s.deps.Config.Agents {
		if name == exclude || a.Sandbox == nil {
			continue
		}
		if a.SandboxBlockProblem(s.deps.Config.AgentSandbox) == "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// sandboxBlockForToggle returns the sandbox block the General tab's "Run in
// sandbox" toggle writes (hivecommons/hive#18525). Writing enabled: true
// alone produced an agent whose every kick failed with "sandbox image is not
// configured" when agent_sandbox had no image, so turning the toggle on now
// writes a complete block or refuses:
//
//   - with copyFrom, the named agent's complete block is copied, minus its
//     publish preflight (verify commands are per-repository work); the
//     receiving agent keeps its own publish block;
//   - otherwise the agent's own fields, then agent_sandbox defaults, must
//     resolve a complete block, and the image and runtime are pinned into it.
//
// Turning it off keeps the block and only sets enabled: false.
func (s *Server) sandboxBlockForToggle(name string, agentCfg config.AgentConfig, enable bool, copyFrom string) (*config.AgentSandboxOverride, error) {
	global := s.deps.Config.AgentSandbox
	if !enable {
		block := &config.AgentSandboxOverride{}
		if agentCfg.Sandbox != nil {
			cp := *agentCfg.Sandbox
			block = &cp
		}
		off := false
		block.Enabled = &off
		return block, nil
	}
	if copyFrom != "" {
		if copyFrom == name {
			return nil, fmt.Errorf("cannot copy a sandbox block from %q into itself", name)
		}
		src, ok := s.deps.Config.Agents[copyFrom]
		if !ok {
			return nil, fmt.Errorf("copy_from: agent %q not found", copyFrom)
		}
		if src.Sandbox == nil {
			return nil, fmt.Errorf("copy_from: agent %q has no sandbox block to copy", copyFrom)
		}
		if problem := src.SandboxBlockProblem(global); problem != "" {
			return nil, fmt.Errorf("copy_from: agent %q has no complete sandbox block: %s", copyFrom, problem)
		}
		var keep *config.SandboxPublishConfig
		if agentCfg.Sandbox != nil {
			keep = agentCfg.Sandbox.Publish
		}
		return config.CopiedSandboxBlock(&src, global, keep), nil
	}
	if problem := agentCfg.SandboxBlockProblem(global); problem != "" {
		msg := fmt.Sprintf("cannot run %s in a sandbox: %s. Set the default sandbox image in Settings, Security, or set sandbox.image on this agent, or pass copy_from naming an agent with a complete sandbox block", name, problem)
		if sources := s.sandboxCopySources(name); len(sources) > 0 {
			msg += " (candidates: " + strings.Join(sources, ", ") + ")"
		} else {
			msg += " (no agent has one yet)"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return agentCfg.PrefilledSandboxBlock(global), nil
}
