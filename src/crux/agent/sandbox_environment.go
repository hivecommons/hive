package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

// A Job-runtime agent is told which hardware hive requested for its run
// (hivecommons/hive#19549).
//
// Without it, an agent on an accelerator node learns what it has only by
// probing, and a failed probe reads as "this host has no device": the agent
// then skips the device tests and says so in its review. Hive knows the answer
// at kick time — it built the Job from sandbox.job — so a kick whose Job
// requests extended resources gets a short hive-written section naming them,
// whether sandbox.preflight passed on the node, and that a missing device is an
// environment failure. The text comes from configuration only, never from the
// kick message, and carries resource names and counts only: no Secret, service
// account or other credential-bearing name.

const (
	// sandboxEnvironmentPromptMarker opens the hive-written section.
	sandboxEnvironmentPromptMarker = "[hive:sandbox-environment]"
	// sandboxEnvironmentMaxResources bounds how many resources are listed.
	sandboxEnvironmentMaxResources = 16
	// sandboxEnvironmentMaxField bounds one resource name or quantity.
	sandboxEnvironmentMaxField = 128
	// sandboxEnvironmentInstruction is the one instruction the section gives.
	sandboxEnvironmentInstruction = "Hardware was requested for this run. If a device test cannot see it, report that as an environment failure in your review. Do not describe this host as having no device, and do not skip device tests silently."
)

// sandboxResource is one extended resource the Job requests.
type sandboxResource struct {
	Name     string
	Quantity string
}

// sandboxEnvironment is what the section states. A nil environment adds no
// section.
type sandboxEnvironment struct {
	Runtime   string
	Resources []sandboxResource
	// PreflightCommands is how many sandbox.preflight commands run in the
	// Job before the agent. Zero means no preflight checked the node.
	PreflightCommands int
	// SetupCommands and SetupHealthCommands count the sandbox.setup
	// commands that run in the Job before the agent (#19574).
	SetupCommands       int
	SetupHealthCommands int
}

// isExtendedResource reports whether a Kubernetes resource name is an
// extended resource (a device such as vendor.com/gpu), not one of the
// built-in compute resources: cpu, memory, ephemeral-storage, storage,
// hugepages-* and anything in the kubernetes.io domain.
func isExtendedResource(name string) bool {
	name = strings.TrimSpace(name)
	slash := strings.IndexByte(name, '/')
	if slash <= 0 || slash == len(name)-1 {
		return false
	}
	domain := name[:slash]
	return domain != "kubernetes.io" && !strings.HasSuffix(domain, ".kubernetes.io")
}

// sandboxEnvironmentField keeps a config value to one short printable line.
func sandboxEnvironmentField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > sandboxEnvironmentMaxField {
		s, _ = truncateUTF8(s, sandboxEnvironmentMaxField)
	}
	return s
}

// sandboxJobExtendedResources returns the extended resources a Job config
// requests, sorted by name. A limit wins over a request for the same name
// (Kubernetes requires them equal for extended resources anyway); a quantity
// of zero requests nothing and is left out.
func sandboxJobExtendedResources(job config.SandboxJobConfig) []sandboxResource {
	byName := map[string]string{}
	for _, m := range []map[string]string{job.Resources.Requests, job.Resources.Limits} {
		for name, qty := range m {
			name, qty = sandboxEnvironmentField(name), sandboxEnvironmentField(qty)
			if !isExtendedResource(name) || qty == "" || strings.Trim(qty, "0.") == "" {
				continue
			}
			byName[name] = qty
		}
	}
	out := make([]sandboxResource, 0, len(byName))
	for name, qty := range byName {
		out = append(out, sandboxResource{Name: name, Quantity: qty})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > sandboxEnvironmentMaxResources {
		out = out[:sandboxEnvironmentMaxResources]
	}
	return out
}

// sandboxEnvironmentFor returns the environment a kick's section states, or
// nil when there is nothing to say: a runtime other than job, or a Job that
// requests no extended resources. preflightWired is whether the launcher runs
// sandbox.preflight for this kick (a production Job launcher does; a test
// factory's launcher does not).
func sandboxEnvironmentFor(runtime string, job config.SandboxJobConfig, preflight config.SandboxPreflightConfig, preflightWired bool) *sandboxEnvironment {
	if runtime != config.SandboxRuntimeJob {
		return nil
	}
	resources := sandboxJobExtendedResources(job)
	if len(resources) == 0 {
		return nil
	}
	env := &sandboxEnvironment{Runtime: runtime, Resources: resources}
	if preflightWired {
		for _, c := range preflight.Commands {
			if strings.TrimSpace(c) != "" {
				env.PreflightCommands++
			}
		}
	}
	return env
}

// sandboxEnvironmentForLauncher is sandboxEnvironmentFor with preflightWired
// read from the launcher prepareJobKickLocked configured.
func sandboxEnvironmentForLauncher(launcher sandbox.Launcher, runtime string, job config.SandboxJobConfig, preflight config.SandboxPreflightConfig) *sandboxEnvironment {
	_, wired := launcher.(*kubejob.Launcher)
	return sandboxEnvironmentFor(runtime, job, preflight, wired)
}

// withSetup records the sandbox.setup step the launcher runs for this kick,
// when it is the production Job launcher. A nil environment stays nil.
func (env *sandboxEnvironment) withSetup(launcher sandbox.Launcher, setup config.SandboxSetupConfig) *sandboxEnvironment {
	if env == nil {
		return nil
	}
	if _, wired := launcher.(*kubejob.Launcher); !wired {
		return env
	}
	for _, list := range []struct {
		cmds []string
		n    *int
	}{{setup.Commands, &env.SetupCommands}, {setup.HealthCommands, &env.SetupHealthCommands}} {
		for _, c := range list.cmds {
			if strings.TrimSpace(c) != "" {
				*list.n++
			}
		}
	}
	return env
}

// sandboxEnvironmentPromptSection is the hive-written block prepended to the
// prompt of a kick that has an environment. Nil returns "".
func sandboxEnvironmentPromptSection(env *sandboxEnvironment) string {
	if env == nil || len(env.Resources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(sandboxEnvironmentPromptMarker + "\n")
	fmt.Fprintf(&b, "SANDBOX ENVIRONMENT. Runtime: %s. Hive scheduled this run as a Kubernetes Job that requests these extended resources:\n", env.Runtime)
	for _, r := range env.Resources {
		fmt.Fprintf(&b, "- %s: %s\n", r.Name, r.Quantity)
	}
	if env.PreflightCommands > 0 {
		fmt.Fprintf(&b, "Node preflight: the %d configured sandbox.preflight command(s) passed on this node before you started; hive does not start the agent on a node where one fails.\n", env.PreflightCommands)
	} else {
		b.WriteString("Node preflight: none is configured, so hive has not checked the device on this node.\n")
	}
	if env.SetupCommands+env.SetupHealthCommands > 0 {
		fmt.Fprintf(&b, "Workspace setup: the %d configured sandbox.setup command(s) and %d health check(s) already ran in this workspace before you started; do not repeat them.\n", env.SetupCommands, env.SetupHealthCommands)
	}
	b.WriteString(sandboxEnvironmentInstruction + "\n")
	b.WriteString("---\n\n")
	return b.String()
}
