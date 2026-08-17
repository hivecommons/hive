package repair

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Codex is a security boundary for integrated repairs. A new binary version or
// feature surface is unsupported until this inventory is explicitly reviewed
// and updated with corresponding secured-argument and real-provider tests.
var reviewedCodexVersions = map[string]map[string]string{
	"codex-cli 0.144.0-alpha.4": {
		"windows": codexReviewedFeatureInventoryV0144,
	},
	"codex-cli 0.144.1": {
		"linux": strings.NewReplacer(
			"secret_auth_storage                  stable             true", "secret_auth_storage                  stable             false",
			"unified_exec                         stable             false", "unified_exec                         stable             true",
		).Replace(codexReviewedFeatureInventoryV0144),
		"windows": codexReviewedFeatureInventoryV0144,
	},
}

func reviewedCodexFeatureInventory(version, platform string) (string, bool) {
	platforms, ok := reviewedCodexVersions[version]
	if !ok {
		return "", false
	}
	inventory, ok := platforms[platform]
	return inventory, ok
}

const codexReviewedFeatureInventoryV0144 = `apply_patch_freeform                 removed            false
apply_patch_streaming_events         under development  false
apps                                 stable             true
apps_mcp_path_override               removed            false
artifact                             under development  false
auth_elicitation                     stable             true
browser_use                          stable             true
browser_use_external                 stable             true
browser_use_full_cdp_access          stable             true
chronicle                            under development  false
code_mode                            under development  false
code_mode_host                       stable             true
code_mode_only                       under development  false
codex_git_commit                     removed            false
collaboration_modes                  removed            true
computer_use                         stable             true
concurrent_reasoning_summaries       under development  false
current_time_reminder                under development  false
default_mode_request_user_input      under development  false
deferred_executor                    under development  false
elevated_windows_sandbox             removed            false
enable_fanout                        under development  false
enable_mcp_apps                      under development  false
enable_request_compression           stable             true
exec_permission_approvals            under development  false
experimental_windows_sandbox         removed            false
external_migration                   removed            false
fast_mode                            stable             true
goals                                stable             true
guardian_approval                    stable             true
hooks                                stable             true
image_detail_original                removed            false
image_generation                     stable             true
in_app_browser                       stable             true
item_ids                             under development  false
js_repl                              removed            false
js_repl_tools_only                   removed            false
local_thread_store_compression       under development  false
memories                             experimental       false
mentions_v2                          stable             true
multi_agent                          stable             true
multi_agent_mode                     removed            false
multi_agent_v2                       under development  false
network_proxy                        experimental       false
non_prefixed_mcp_tool_names          under development  false
personality                          stable             true
plugin_hooks                         removed            false
plugin_sharing                       stable             true
plugins                              stable             true
prevent_idle_sleep                   experimental       false
realtime_conversation                under development  false
remote_compaction_v2                 stable             true
remote_control                       removed            false
remote_models                        removed            false
remote_plugin                        stable             true
request_permissions_tool             under development  false
request_rule                         removed            false
resize_all_images                    removed            true
respect_system_proxy                 under development  false
responses_websockets                 removed            false
responses_websockets_v2              removed            false
rollout_budget                       under development  false
runtime_metrics                      under development  false
search_tool                          removed            false
secret_auth_storage                  stable             true
shell_snapshot                       stable             true
shell_tool                           stable             true
shell_zsh_fork                       under development  false
skill_env_var_dependency_prompt      removed            false
skill_mcp_dependency_install         stable             true
sqlite                               removed            true
standalone_web_search                under development  false
steer                                removed            true
terminal_resize_reflow               removed            true
terminal_visualization_instructions  under development  false
token_budget                         under development  false
tool_call_mcp_elicitation            stable             true
tool_search                          removed            false
tool_search_always_defer_mcp_tools   removed            true
tool_suggest                         stable             true
tui_app_server                       removed            true
unavailable_dummy_tools              removed            false
undo                                 removed            false
unified_exec                         stable             false
unified_exec_zsh_fork                under development  false
use_agent_identity                   under development  false
use_legacy_landlock                  deprecated         false
use_linux_sandbox_bwrap              removed            false
web_search_cached                    deprecated         false
web_search_request                   deprecated         false
workspace_dependencies               stable             true
workspace_owner_usage_nudge          removed            false`

type codexFeatureCapability struct {
	stage   string
	enabled bool
}

var codexFeatureLinePattern = regexp.MustCompile(`^([a-z][a-z0-9_]*)\s+(removed|under development|stable|experimental|deprecated)\s+(true|false)$`)

func (p CodexProvider) verifyReviewedCapabilities(ctx context.Context, dir string) error {
	versionOutput, err := p.runMetadataCommand(ctx, dir, "--version")
	if err != nil {
		return fmt.Errorf("read Codex provider version: %w", err)
	}
	version := strings.TrimSpace(versionOutput)
	_, reviewed := reviewedCodexVersions[version]
	if !reviewed {
		supported := make([]string, 0, len(reviewedCodexVersions))
		for candidate := range reviewedCodexVersions {
			supported = append(supported, candidate)
		}
		sort.Strings(supported)
		return fmt.Errorf("unsupported Codex provider version %q; install a reviewed packaged version (%s)", safeExcerpt(version), strings.Join(supported, ", "))
	}
	expectedText, reviewed := reviewedCodexFeatureInventory(version, runtime.GOOS)
	if !reviewed {
		return fmt.Errorf("unsupported Codex provider platform %q for reviewed version %q", runtime.GOOS, safeExcerpt(version))
	}
	featureOutput, err := p.runMetadataCommand(ctx, dir, "features", "list")
	if err != nil {
		return fmt.Errorf("read Codex provider feature inventory: %w", err)
	}
	actual, err := parseCodexFeatureInventory(featureOutput)
	if err != nil {
		return fmt.Errorf("Codex provider returned an invalid feature inventory: %w", err)
	}
	expected, err := parseCodexFeatureInventory(expectedText)
	if err != nil {
		return fmt.Errorf("Hive's reviewed Codex feature inventory is invalid: %w", err)
	}
	for name, capability := range actual {
		reviewedCapability, ok := expected[name]
		if !ok {
			return fmt.Errorf("Codex provider exposes unreviewed feature %q; Hive refuses repair model invocation until the capability is reviewed", name)
		}
		if capability != reviewedCapability {
			return fmt.Errorf("Codex provider feature %q changed from reviewed semantics; Hive refuses repair model invocation", name)
		}
	}
	for name := range expected {
		if _, ok := actual[name]; !ok {
			return fmt.Errorf("Codex provider omitted reviewed feature %q; Hive refuses an incomplete capability inventory", name)
		}
	}
	return nil
}

func (p CodexProvider) runMetadataCommand(ctx context.Context, dir string, arguments ...string) (string, error) {
	args := append(append([]string(nil), p.Prefix...), arguments...)
	command := exec.CommandContext(ctx, p.Command, args...)
	command.Dir = dir
	command.Env = p.commandEnvironment()
	var stdout, stderr limitedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("Codex %s failed: %w: %s", strings.Join(arguments, " "), err, safeExcerpt(stderr.String()))
	}
	return stdout.String(), nil
}

func parseCodexFeatureInventory(output string) (map[string]codexFeatureCapability, error) {
	result := make(map[string]codexFeatureCapability)
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n")), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("feature list is empty")
	}
	for lineNumber, line := range lines {
		match := codexFeatureLinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 4 {
			return nil, fmt.Errorf("feature list line %d is malformed", lineNumber+1)
		}
		if _, duplicate := result[match[1]]; duplicate {
			return nil, fmt.Errorf("feature %q is duplicated", match[1])
		}
		result[match[1]] = codexFeatureCapability{stage: match[2], enabled: match[3] == "true"}
	}
	return result, nil
}
