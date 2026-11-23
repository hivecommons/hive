package dashboard

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Editable sandbox fields on an agent's General tab (hivecommons/hive#18797).
// Before this the image, runtime and Job fields of an agent's sandbox block
// were config-file-only; the dashboard could only flip enabled. The keys are
// camelCase like the rest of the General section:
//
//	sandboxImage    string   the agent's own sandbox.image ("" inherits agent_sandbox.image)
//	sandboxRuntime  string   the agent's own sandbox.runtime ("" inherits agent_sandbox.runtime)
//	sandboxJob      object   workspaceClaim, workspaceClaimMount, serviceAccount,
//	                         schedulerName, ttlSeconds, envFromSecrets (secret NAMES only),
//	                         imagePullSecrets, nodeSelector, resourceLimits,
//	                         resourceRequests, tolerations, scheduleTimeoutS (#19553)
//	sandboxPreflight object  the agent's own preflight; null inherits agent_sandbox.preflight
//	sandboxCopyFrom string   copy another agent's complete block (see CopiedSandboxBlock)
//
// Affinity and volumes are carried by the copy action and preserved by an
// edit, but have no field here.

// sandboxJobBodyKey is the General-tab body key carrying the Job fields.
const sandboxJobBodyKey = "sandboxJob"

// sandboxEditKeys are the body keys that change an agent's sandbox block.
var sandboxEditKeys = []string{"sandboxImage", "sandboxRuntime", sandboxJobBodyKey, sandboxPreflightBodyKey}

// secretNameRE is a Kubernetes object name (DNS-1123 subdomain). env_from_secrets
// holds names only, so anything else, a KEY=value pair most of all, is refused.
var secretNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// maxSecretNameLen is the Kubernetes object-name length limit.
const maxSecretNameLen = 253

// sandboxEditsPresent reports whether body edits the agent's sandbox block.
func sandboxEditsPresent(body map[string]interface{}) bool {
	for _, k := range sandboxEditKeys {
		if _, ok := body[k]; ok {
			return true
		}
	}
	return false
}

// agentSandboxFieldsView is what the General tab shows for the sandbox block:
// the agent's OWN values (blank means inherited) and what each resolves to.
func agentSandboxFieldsView(a config.AgentConfig, global config.AgentSandboxConfig) map[string]interface{} {
	var own config.AgentSandboxOverride
	if a.Sandbox != nil {
		own = *a.Sandbox
	}
	job := config.SandboxJobConfig{}
	if own.Job != nil {
		job = *own.Job
	}
	jobEffective, jobSources := sandboxSchedulingEffectiveView(a, global)
	preflightSource := "agent_sandbox"
	var ownPreflight interface{}
	if own.Preflight != nil {
		preflightSource = "agent"
		ownPreflight = preflightView(own.Preflight)
	}
	effPreflight := a.SandboxPreflight(global)
	return map[string]interface{}{
		"sandboxImage":            own.Image,
		"sandboxRuntime":          own.Runtime,
		"sandboxImageEffective":   a.SandboxImage(global),
		"sandboxRuntimeEffective": a.SandboxRuntime(global),
		"sandboxJob": map[string]interface{}{
			"workspaceClaim":      job.WorkspaceClaim,
			"workspaceClaimMount": job.WorkspaceClaimMount,
			"serviceAccount":      job.ServiceAccount,
			"schedulerName":       job.SchedulerName,
			"ttlSeconds":          job.TTLSeconds,
			"envFromSecrets":      nonNilStrings(job.EnvFromSecrets),
			"imagePullSecrets":    nonNilStrings(job.ImagePullSecrets),
			"nodeSelector":        nonNilStringMap(job.NodeSelector),
			"resourceLimits":      nonNilStringMap(job.Resources.Limits),
			"resourceRequests":    nonNilStringMap(job.Resources.Requests),
			"tolerations":         nonNilTolerations(job.Tolerations),
			"scheduleTimeoutS":    job.ScheduleTimeoutS,
		},
		// What the Job runs with and where each value comes from (#19553).
		"sandboxJobEffective": jobEffective,
		"sandboxJobSources":   jobSources,
		// Hive-wide Job fields this agent's own job block hides.
		"sandboxJobHiddenGlobal":    sandboxPodTemplateFieldsHidden(a, global),
		"sandboxPreflight":          ownPreflight,
		"sandboxPreflightEffective": preflightView(&effPreflight),
		"sandboxPreflightSource":    preflightSource,
	}
}

// applySandboxEdits applies the sandboxImage, sandboxRuntime and sandboxJob
// body keys to agentCfg. Every value is validated before agentCfg changes, so
// a refusal leaves the agent untouched. An absent key is left as it is.
func applySandboxEdits(agentCfg *config.AgentConfig, body map[string]interface{}) error {
	if !sandboxEditsPresent(body) {
		return nil
	}
	next := &config.AgentSandboxOverride{}
	if agentCfg.Sandbox != nil {
		cp := *agentCfg.Sandbox
		next = &cp
	}
	if v, ok := body["sandboxImage"]; ok {
		img, _ := v.(string)
		img = sanitizeString(img)
		if strings.ContainsAny(img, " \t\r\n") {
			return fmt.Errorf("sandbox.image must not contain whitespace")
		}
		next.Image = img
	}
	if v, ok := body["sandboxRuntime"]; ok {
		rt, _ := v.(string)
		rt = strings.TrimSpace(rt)
		if rt != "" && !config.ValidSandboxRuntimes[rt] {
			return fmt.Errorf("sandbox.runtime %q is not one hive has (%s, %s, %s or %s)", rt,
				config.SandboxRuntimePodman, config.SandboxRuntimeJob, config.SandboxRuntimeOpenShell, config.SandboxRuntimeSubstrate)
		}
		next.Runtime = rt
	}
	if v, ok := body[sandboxJobBodyKey]; ok {
		m, isObj := v.(map[string]interface{})
		if !isObj && v != nil {
			return fmt.Errorf("sandboxJob must be an object")
		}
		job := config.SandboxJobConfig{}
		if next.Job != nil {
			job = *next.Job
		}
		if err := applySandboxJobFields(&job, m); err != nil {
			return err
		}
		if reflect.DeepEqual(job, config.SandboxJobConfig{}) {
			next.Job = nil
		} else {
			next.Job = &job
		}
	}
	if v, ok := body[sandboxPreflightBodyKey]; ok {
		p, err := preflightOverrideFromBody(v)
		if err != nil {
			return err
		}
		next.Preflight = p
	}
	agentCfg.Sandbox = next
	return nil
}

// applySandboxJobFields applies the editable Job keys present in m to job.
func applySandboxJobFields(job *config.SandboxJobConfig, m map[string]interface{}) error {
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"workspaceClaim", &job.WorkspaceClaim},
		{"workspaceClaimMount", &job.WorkspaceClaimMount},
		{"serviceAccount", &job.ServiceAccount},
		{"schedulerName", &job.SchedulerName},
	} {
		if v, ok := m[f.key]; ok {
			s, _ := v.(string)
			*f.dst = sanitizeString(s)
		}
	}
	if v, ok := m["ttlSeconds"]; ok {
		n, isNum := v.(float64)
		if !isNum || n < 0 || n != float64(int(n)) {
			return fmt.Errorf("sandbox.job.ttl_seconds must be a whole number >= 0")
		}
		job.TTLSeconds = int(n)
	}
	if v, ok := m["envFromSecrets"]; ok {
		names, err := secretNamesFromBody(v)
		if err != nil {
			return err
		}
		job.EnvFromSecrets = names
	}
	if v, ok := m["imagePullSecrets"]; ok {
		names, err := secretNamesFromBody(v)
		if err != nil {
			return err
		}
		if err := config.ValidateImagePullSecrets(names); err != nil {
			return err
		}
		job.ImagePullSecrets = names
	}
	return applySandboxSchedulingFields(job, m)
}

// secretNamesFromBody validates a list of Kubernetes Secret names. The block
// references secrets by name; a value pasted here would be written to the
// config file, so anything that is not a name is refused.
func secretNamesFromBody(v interface{}) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("sandbox.job.env_from_secrets must be a list of secret names")
	}
	var out []string
	for _, item := range arr {
		s, _ := item.(string)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if len(s) > maxSecretNameLen || !secretNameRE.MatchString(s) {
			return nil, fmt.Errorf("sandbox.job.env_from_secrets takes Secret names, not values: %q is not a valid name", s)
		}
		out = append(out, s)
	}
	return out, nil
}

// sandboxEnabledProblem refuses an enabled block that cannot run, naming the
// field: the same check the toggle makes, applied after field edits so
// clearing an image cannot leave an enabled agent with none.
func sandboxEnabledProblem(name string, a config.AgentConfig, global config.AgentSandboxConfig) error {
	if a.Sandbox == nil || a.Sandbox.Enabled == nil || !*a.Sandbox.Enabled {
		return nil
	}
	if problem := a.SandboxBlockProblem(global); problem != "" {
		return fmt.Errorf("cannot save %s with its sandbox enabled: %s. Set sandbox.image on the agent, or the agent_sandbox default image in Settings, Security", name, problem)
	}
	return nil
}
