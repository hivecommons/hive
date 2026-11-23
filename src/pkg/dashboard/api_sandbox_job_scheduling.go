package dashboard

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/config"
)

// Sandbox Job scheduling, resources, preflight and read relay from the
// dashboard (hivecommons/hive#19553). Before this these were YAML-only; an
// owner running a sandboxed agent on accelerator nodes had to hand-edit
// hive.yaml. The body keys and the hive.yaml leaves they write:
//
// Hive-wide, PUT /api/config/governor/security (Settings, Security,
// "Advanced sandbox"):
//
//	agentSandboxJobNodeSelector       agent_sandbox.job.node_selector.<key>
//	agentSandboxJobResourceLimits     agent_sandbox.job.resources.limits.<key>
//	agentSandboxJobResourceRequests   agent_sandbox.job.resources.requests.<key>
//	agentSandboxJobTolerations        agent_sandbox.job.tolerations[].key,
//	                                  agent_sandbox.job.tolerations[].operator,
//	                                  agent_sandbox.job.tolerations[].value,
//	                                  agent_sandbox.job.tolerations[].effect
//	agentSandboxJobImagePullSecrets   agent_sandbox.job.image_pull_secrets[]
//	agentSandboxJobScheduleTimeoutS   agent_sandbox.job.schedule_timeout_s
//	agentSandboxPreflight             agent_sandbox.preflight.commands[],
//	                                  agent_sandbox.preflight.timeout_s,
//	                                  agent_sandbox.preflight.total_timeout_s,
//	                                  agent_sandbox.preflight.retry_delay_s
//	agentSandboxJobGithubReadProxy    agent_sandbox.job.github_read_proxy.enabled,
//	                                  agent_sandbox.job.github_read_proxy.listen_addr,
//	                                  agent_sandbox.job.github_read_proxy.endpoint,
//	                                  agent_sandbox.job.github_read_proxy.no_proxy[]
//
// One agent, PUT /api/config/agent/<name>/general (agent General tab,
// "Sandbox settings"):
//
//	sandboxJob.nodeSelector       agents.<key>.sandbox.job.node_selector.<key>
//	sandboxJob.resourceLimits     agents.<key>.sandbox.job.resources.limits.<key>
//	sandboxJob.resourceRequests   agents.<key>.sandbox.job.resources.requests.<key>
//	sandboxJob.tolerations        agents.<key>.sandbox.job.tolerations[].key,
//	                              agents.<key>.sandbox.job.tolerations[].operator,
//	                              agents.<key>.sandbox.job.tolerations[].value,
//	                              agents.<key>.sandbox.job.tolerations[].effect
//	sandboxJob.imagePullSecrets   agents.<key>.sandbox.job.image_pull_secrets[]
//	sandboxJob.scheduleTimeoutS   agents.<key>.sandbox.job.schedule_timeout_s
//	sandboxPreflight              agents.<key>.sandbox.preflight.commands[],
//	                              agents.<key>.sandbox.preflight.timeout_s,
//	                              agents.<key>.sandbox.preflight.total_timeout_s,
//	                              agents.<key>.sandbox.preflight.retry_delay_s
//
// The read relay is hive-wide only: SandboxJob ignores a per-agent block's
// relay, so there is no per-agent control for it.

// sandboxPreflightBodyKey is the General-tab body key for the per-agent
// preflight override: null inherits agent_sandbox.preflight, an object
// replaces it (an empty command list turns preflight off for the agent).
const sandboxPreflightBodyKey = "sandboxPreflight"

// Bounds on the dashboard-edited maps and lists.
const (
	maxSandboxJobMapEntries    = 64
	maxSandboxJobTolerations   = 32
	maxSandboxReadRelayNoProxy = 64
	maxResourceQuantityLen     = 64
)

var (
	// labelNameRE is a Kubernetes qualified-name name part (and a non-empty
	// label value): alphanumeric at both ends, '-', '_' and '.' between.
	labelNameRE = regexp.MustCompile(`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`)
	// dns1123SubdomainRE is a qualified-name prefix (example.com).
	dns1123SubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	// resourceQuantityRE is a non-negative Kubernetes resource quantity:
	// a decimal number with an optional binary, decimal or exponent suffix.
	resourceQuantityRE = regexp.MustCompile(`^\+?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+|[KMGTPE]i|[numkMGTPE])?$`)
)

// qualifiedNameOK reports whether s is a Kubernetes qualified name, the form
// of a label key and of a resource name: [prefix/]name, the prefix a DNS-1123
// subdomain of at most 253 bytes and the name at most 63.
func qualifiedNameOK(s string) bool {
	parts := strings.Split(s, "/")
	name := parts[0]
	switch len(parts) {
	case 1:
	case 2:
		prefix := parts[0]
		name = parts[1]
		if prefix == "" || len(prefix) > 253 || !dns1123SubdomainRE.MatchString(prefix) {
			return false
		}
	default:
		return false
	}
	return len(name) <= 63 && labelNameRE.MatchString(name)
}

// labelValueOK reports whether s is a Kubernetes label value (may be empty).
func labelValueOK(s string) bool {
	return s == "" || (len(s) <= 63 && labelNameRE.MatchString(s))
}

// cleanNodeSelector trims and validates a node selector. An empty map is nil
// so the YAML key is dropped.
func cleanNodeSelector(field string, in map[string]string) (map[string]string, error) {
	if len(in) > maxSandboxJobMapEntries {
		return nil, fmt.Errorf("%s takes at most %d labels, got %d", field, maxSandboxJobMapEntries, len(in))
	}
	out := map[string]string{}
	for k, v := range in {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if !qualifiedNameOK(k) {
			return nil, fmt.Errorf("%s: %q is not a valid Kubernetes label key", field, k)
		}
		if !labelValueOK(v) {
			return nil, fmt.Errorf("%s: %q is not a valid Kubernetes label value for %s", field, v, k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// cleanResourceList trims and validates resource limits or requests: names
// are qualified names (cpu, memory, ibm.com/spyre_pf), values non-negative
// quantities (500m, 4Gi, 2).
func cleanResourceList(field string, in map[string]string) (map[string]string, error) {
	if len(in) > maxSandboxJobMapEntries {
		return nil, fmt.Errorf("%s takes at most %d resources, got %d", field, maxSandboxJobMapEntries, len(in))
	}
	out := map[string]string{}
	for k, v := range in {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if !qualifiedNameOK(k) {
			return nil, fmt.Errorf("%s: %q is not a valid Kubernetes resource name", field, k)
		}
		if v == "" || len(v) > maxResourceQuantityLen || !resourceQuantityRE.MatchString(v) {
			return nil, fmt.Errorf("%s: %q is not a valid quantity for %s (for example 2, 500m or 4Gi)", field, v, k)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// cleanTolerations trims and validates tolerations: operator Equal or Exists,
// effect NoSchedule, PreferNoSchedule or NoExecute (or empty for all), a
// label key and value, no value with Exists, and an empty key only with
// Exists. Entries with every field empty are dropped.
func cleanTolerations(field string, in []config.SandboxJobToleration) ([]config.SandboxJobToleration, error) {
	if len(in) > maxSandboxJobTolerations {
		return nil, fmt.Errorf("%s takes at most %d tolerations, got %d", field, maxSandboxJobTolerations, len(in))
	}
	var out []config.SandboxJobToleration
	for i, t := range in {
		t = config.SandboxJobToleration{
			Key:      strings.TrimSpace(t.Key),
			Operator: strings.TrimSpace(t.Operator),
			Value:    strings.TrimSpace(t.Value),
			Effect:   strings.TrimSpace(t.Effect),
		}
		if t == (config.SandboxJobToleration{}) {
			continue
		}
		switch t.Operator {
		case "", "Equal", "Exists":
		default:
			return nil, fmt.Errorf("%s[%d].operator must be Equal or Exists, got %q", field, i, t.Operator)
		}
		switch t.Effect {
		case "", "NoSchedule", "PreferNoSchedule", "NoExecute":
		default:
			return nil, fmt.Errorf("%s[%d].effect must be NoSchedule, PreferNoSchedule or NoExecute, got %q", field, i, t.Effect)
		}
		if t.Key != "" && !qualifiedNameOK(t.Key) {
			return nil, fmt.Errorf("%s[%d].key %q is not a valid Kubernetes label key", field, i, t.Key)
		}
		if t.Key == "" && t.Operator != "Exists" {
			return nil, fmt.Errorf("%s[%d]: an empty key needs operator Exists", field, i)
		}
		if t.Operator == "Exists" && t.Value != "" {
			return nil, fmt.Errorf("%s[%d]: operator Exists takes no value", field, i)
		}
		if !labelValueOK(t.Value) {
			return nil, fmt.Errorf("%s[%d].value %q is not a valid Kubernetes label value", field, i, t.Value)
		}
		out = append(out, t)
	}
	return out, nil
}

// cleanPullSecretNames trims a list of image pull Secret names and refuses
// anything that is not a name.
func cleanPullSecretNames(in []string) ([]string, error) {
	names := trimNonEmpty(in)
	if len(names) == 0 {
		return nil, nil
	}
	if err := config.ValidateImagePullSecrets(names); err != nil {
		return nil, err
	}
	return names, nil
}

// sandboxPreflightBody is the dashboard shape of a preflight block.
type sandboxPreflightBody struct {
	Commands      []string `json:"commands"`
	TimeoutS      int      `json:"timeoutS"`
	TotalTimeoutS int      `json:"totalTimeoutS"`
	RetryDelayS   int      `json:"retryDelayS"`
}

// preflightFromBody validates a preflight body with the same bounds config
// load applies. Blank command lines are dropped.
func preflightFromBody(prefix string, in sandboxPreflightBody) (*config.SandboxPreflightConfig, error) {
	var cmds []string
	for _, c := range in.Commands {
		if strings.TrimSpace(c) != "" {
			cmds = append(cmds, strings.TrimSpace(c))
		}
	}
	p := &config.SandboxPreflightConfig{Commands: cmds, TimeoutS: in.TimeoutS, TotalTimeoutS: in.TotalTimeoutS, RetryDelayS: in.RetryDelayS}
	if err := cruxconfig.ValidateSandboxPreflight(p); err != nil {
		return nil, fmt.Errorf("%s.%w", prefix, err)
	}
	return p, nil
}

// preflightView is the dashboard shape of a preflight block (nil-safe).
func preflightView(p *config.SandboxPreflightConfig) map[string]interface{} {
	if p == nil {
		p = &config.SandboxPreflightConfig{}
	}
	return map[string]interface{}{
		"commands":      nonNilStrings(p.Commands),
		"timeoutS":      p.TimeoutS,
		"totalTimeoutS": p.TotalTimeoutS,
		"retryDelayS":   p.RetryDelayS,
	}
}

// sandboxReadRelayBody is the dashboard shape of github_read_proxy.
type sandboxReadRelayBody struct {
	Enabled    bool     `json:"enabled"`
	ListenAddr string   `json:"listenAddr"`
	Endpoint   string   `json:"endpoint"`
	NoProxy    []string `json:"noProxy"`
}

// readRelayFromBody validates the read relay block. An enabled block must
// pass the config-load check; a disabled one may leave fields blank, but any
// field it does set must still be well formed, so turning it on later cannot
// surface an error the save did not. An all-empty block is nil.
func readRelayFromBody(in sandboxReadRelayBody) (*config.SandboxGitHubReadProxy, error) {
	out := &config.SandboxGitHubReadProxy{
		Enabled:    in.Enabled,
		ListenAddr: strings.TrimSpace(in.ListenAddr),
		Endpoint:   strings.TrimSpace(in.Endpoint),
		NoProxy:    trimNonEmpty(in.NoProxy),
	}
	if len(out.NoProxy) == 0 {
		out.NoProxy = nil
	}
	if len(out.NoProxy) > maxSandboxReadRelayNoProxy {
		return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.no_proxy takes at most %d hosts, got %d", maxSandboxReadRelayNoProxy, len(out.NoProxy))
	}
	for _, h := range out.NoProxy {
		if len(h) > 253 || strings.ContainsAny(h, " \t\r\n,/@") {
			return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.no_proxy: %q is not a host or domain", h)
		}
	}
	if out.Enabled {
		if err := out.Validate(); err != nil {
			return nil, err
		}
		return out, nil
	}
	if out.ListenAddr != "" {
		if _, port, err := net.SplitHostPort(out.ListenAddr); err != nil || port == "" {
			return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.listen_addr must be host:port (the host may be empty to bind every interface)")
		}
	}
	if out.Endpoint != "" {
		u, err := url.Parse(out.Endpoint)
		switch {
		case err != nil || u.Hostname() == "":
			return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.endpoint must be an https://host:port URL")
		case u.Scheme != "https":
			return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.endpoint must use https://")
		case u.User != nil:
			return nil, fmt.Errorf("agent_sandbox.job.github_read_proxy.endpoint must not carry credentials; hive adds a per-kick key itself")
		}
	}
	if out.ListenAddr == "" && out.Endpoint == "" && out.NoProxy == nil {
		return nil, nil
	}
	return out, nil
}

// readRelayView is the dashboard shape of the hive-wide read relay.
func readRelayView(p *config.SandboxGitHubReadProxy) map[string]interface{} {
	if p == nil {
		p = &config.SandboxGitHubReadProxy{}
	}
	return map[string]interface{}{
		"enabled":    p.Enabled,
		"listenAddr": p.ListenAddr,
		"endpoint":   p.Endpoint,
		"noProxy":    nonNilStrings(p.NoProxy),
	}
}

// stringMapFromBody reads a JSON object of string values from a decoded body.
func stringMapFromBody(field string, v interface{}) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object of key: value strings", field)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s must be a string", field, k)
		}
		out[k] = s
	}
	return out, nil
}

// remarshal decodes a generic body value into dst (a typed struct or slice).
func remarshal(field string, v interface{}, dst interface{}) error {
	raw, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(raw, dst)
	}
	if err != nil {
		return fmt.Errorf("%s has the wrong shape: %v", field, err)
	}
	return nil
}

// applySandboxSchedulingFields applies the per-agent scheduling keys of a
// sandboxJob body object (nodeSelector, resourceLimits, resourceRequests,
// tolerations, scheduleTimeoutS) to job.
func applySandboxSchedulingFields(job *config.SandboxJobConfig, m map[string]interface{}) error {
	if v, ok := m["nodeSelector"]; ok {
		in, err := stringMapFromBody("sandbox.job.node_selector", v)
		if err != nil {
			return err
		}
		if job.NodeSelector, err = cleanNodeSelector("sandbox.job.node_selector", in); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		key, yaml string
		dst       *map[string]string
	}{
		{"resourceLimits", "sandbox.job.resources.limits", &job.Resources.Limits},
		{"resourceRequests", "sandbox.job.resources.requests", &job.Resources.Requests},
	} {
		v, ok := m[f.key]
		if !ok {
			continue
		}
		in, err := stringMapFromBody(f.yaml, v)
		if err != nil {
			return err
		}
		if *f.dst, err = cleanResourceList(f.yaml, in); err != nil {
			return err
		}
	}
	if v, ok := m["tolerations"]; ok {
		var in []config.SandboxJobToleration
		if v != nil {
			if err := remarshal("sandbox.job.tolerations", v, &in); err != nil {
				return err
			}
		}
		out, err := cleanTolerations("sandbox.job.tolerations", in)
		if err != nil {
			return err
		}
		job.Tolerations = out
	}
	if v, ok := m["scheduleTimeoutS"]; ok {
		n, isNum := v.(float64)
		if !isNum || n < 0 || n != float64(int(n)) {
			return fmt.Errorf("sandbox.job.schedule_timeout_s must be a whole number >= 0")
		}
		job.ScheduleTimeoutS = int(n)
	}
	return nil
}

// preflightOverrideFromBody reads the per-agent sandboxPreflight key: null
// clears the override, an object replaces it.
func preflightOverrideFromBody(v interface{}) (*config.SandboxPreflightConfig, error) {
	if v == nil {
		return nil, nil
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return nil, fmt.Errorf("sandboxPreflight must be an object or null")
	}
	var in sandboxPreflightBody
	if err := remarshal("sandboxPreflight", v, &in); err != nil {
		return nil, err
	}
	return preflightFromBody("sandbox", in)
}

// sandboxPodTemplateFieldsHidden lists the hive-wide Job fields the agent's
// own job block hides: SandboxJob takes the pod-template fields from the
// agent block wholesale, so a hive-wide value the agent block leaves empty
// does not apply to this agent. Empty when the agent has no job block.
func sandboxPodTemplateFieldsHidden(a config.AgentConfig, global config.AgentSandboxConfig) []string {
	if a.Sandbox == nil || a.Sandbox.Job == nil || global.Job == nil {
		return []string{}
	}
	own, g := a.Sandbox.Job, global.Job
	hidden := []string{}
	for _, f := range []struct {
		name          string
		global, local bool
	}{
		{"node_selector", len(g.NodeSelector) > 0, len(own.NodeSelector) > 0},
		{"affinity", g.Affinity != nil, own.Affinity != nil},
		{"tolerations", len(g.Tolerations) > 0, len(own.Tolerations) > 0},
		{"resources", len(g.Resources.Limits)+len(g.Resources.Requests) > 0, len(own.Resources.Limits)+len(own.Resources.Requests) > 0},
		{"service_account", g.ServiceAccount != "", own.ServiceAccount != ""},
		{"scheduler_name", g.SchedulerName != "", own.SchedulerName != ""},
		{"env_from_secrets", len(g.EnvFromSecrets) > 0, len(own.EnvFromSecrets) > 0},
		{"volumes", len(g.Volumes) > 0, len(own.Volumes) > 0},
	} {
		if f.global && !f.local {
			hidden = append(hidden, f.name)
		}
	}
	return hidden
}

// sandboxSchedulingEffectiveView is what the agent's Job runs with for the
// scheduling fields and where each value comes from: "agent" (this agent's
// own sandbox.job), "agent_sandbox" (the hive-wide defaults) or "default".
func sandboxSchedulingEffectiveView(a config.AgentConfig, global config.AgentSandboxConfig) (map[string]interface{}, map[string]string) {
	eff := a.SandboxJob(global)
	hasOwn := a.Sandbox != nil && a.Sandbox.Job != nil
	var own config.SandboxJobConfig
	if hasOwn {
		own = *a.Sandbox.Job
	}
	podTemplate := "agent_sandbox"
	if hasOwn {
		podTemplate = "agent"
	}
	pullSrc := "agent_sandbox"
	if len(own.ImagePullSecrets) > 0 {
		pullSrc = "agent"
	}
	timeout := eff.ScheduleTimeoutS
	timeoutSrc := "agent_sandbox"
	if own.ScheduleTimeoutS > 0 {
		timeoutSrc = "agent"
	}
	if timeout == 0 {
		timeout = cruxconfig.DefaultSandboxScheduleTimeoutS
		timeoutSrc = "default"
	}
	view := map[string]interface{}{
		"nodeSelector":     nonNilStringMap(eff.NodeSelector),
		"resourceLimits":   nonNilStringMap(eff.Resources.Limits),
		"resourceRequests": nonNilStringMap(eff.Resources.Requests),
		"tolerations":      nonNilTolerations(eff.Tolerations),
		"imagePullSecrets": nonNilStrings(eff.ImagePullSecrets),
		"scheduleTimeoutS": timeout,
	}
	sources := map[string]string{
		"nodeSelector":     podTemplate,
		"resourceLimits":   podTemplate,
		"resourceRequests": podTemplate,
		"tolerations":      podTemplate,
		"imagePullSecrets": pullSrc,
		"scheduleTimeoutS": timeoutSrc,
	}
	return view, sources
}

func nonNilStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func nonNilTolerations(in []config.SandboxJobToleration) []config.SandboxJobToleration {
	if in == nil {
		return []config.SandboxJobToleration{}
	}
	return in
}

// sandboxSchedulingBody is the hive-wide scheduling part of a Security PUT;
// a nil field is absent from the body and left unchanged.
type sandboxSchedulingBody struct {
	nodeSelector     *map[string]string
	limits           *map[string]string
	requests         *map[string]string
	tolerations      *[]config.SandboxJobToleration
	pullSecrets      *[]string
	scheduleTimeoutS *int
	preflight        *sandboxPreflightBody
	readRelay        *sandboxReadRelayBody
}

// sandboxSchedulingEdit is a validated sandboxSchedulingBody, ready to apply.
type sandboxSchedulingEdit struct {
	in               sandboxSchedulingBody
	nodeSelector     map[string]string
	limits           map[string]string
	requests         map[string]string
	tolerations      []config.SandboxJobToleration
	pullSecrets      []string
	scheduleTimeoutS int
	preflight        *config.SandboxPreflightConfig
	readRelay        *config.SandboxGitHubReadProxy
}

// parseSandboxSchedulingBody validates every present field before anything
// is written, so a refused save leaves agent_sandbox untouched.
func parseSandboxSchedulingBody(in sandboxSchedulingBody) (sandboxSchedulingEdit, error) {
	out := sandboxSchedulingEdit{in: in}
	var err error
	if in.nodeSelector != nil {
		if out.nodeSelector, err = cleanNodeSelector("agent_sandbox.job.node_selector", *in.nodeSelector); err != nil {
			return out, err
		}
	}
	if in.limits != nil {
		if out.limits, err = cleanResourceList("agent_sandbox.job.resources.limits", *in.limits); err != nil {
			return out, err
		}
	}
	if in.requests != nil {
		if out.requests, err = cleanResourceList("agent_sandbox.job.resources.requests", *in.requests); err != nil {
			return out, err
		}
	}
	if in.tolerations != nil {
		if out.tolerations, err = cleanTolerations("agent_sandbox.job.tolerations", *in.tolerations); err != nil {
			return out, err
		}
	}
	if in.pullSecrets != nil {
		if out.pullSecrets, err = cleanPullSecretNames(*in.pullSecrets); err != nil {
			return out, err
		}
	}
	if in.scheduleTimeoutS != nil {
		if *in.scheduleTimeoutS < 0 {
			return out, fmt.Errorf("agent_sandbox.job.schedule_timeout_s must not be negative, got %d", *in.scheduleTimeoutS)
		}
		out.scheduleTimeoutS = *in.scheduleTimeoutS
	}
	if in.preflight != nil {
		var p *config.SandboxPreflightConfig
		if p, err = preflightFromBody("agent_sandbox", *in.preflight); err != nil {
			return out, err
		}
		// An all-empty hive-wide block is no block: preflight is off and the
		// YAML key is dropped.
		if len(p.Commands) == 0 && p.TimeoutS == 0 && p.TotalTimeoutS == 0 && p.RetryDelayS == 0 {
			p = nil
		}
		out.preflight = p
	}
	if in.readRelay != nil {
		if out.readRelay, err = readRelayFromBody(*in.readRelay); err != nil {
			return out, err
		}
	}
	return out, nil
}

// apply writes the validated fields to the hive-wide sandbox block.
func (e sandboxSchedulingEdit) apply(sb *config.AgentSandboxConfig) {
	in := e.in
	if in.preflight != nil {
		sb.Preflight = e.preflight
	}
	if in.nodeSelector == nil && in.limits == nil && in.requests == nil && in.tolerations == nil &&
		in.pullSecrets == nil && in.scheduleTimeoutS == nil && in.readRelay == nil {
		return
	}
	if sb.Job == nil {
		sb.Job = &config.SandboxJobConfig{}
	}
	job := sb.Job
	if in.nodeSelector != nil {
		job.NodeSelector = e.nodeSelector
	}
	if in.limits != nil {
		job.Resources.Limits = e.limits
	}
	if in.requests != nil {
		job.Resources.Requests = e.requests
	}
	if in.tolerations != nil {
		job.Tolerations = e.tolerations
	}
	if in.pullSecrets != nil {
		job.ImagePullSecrets = e.pullSecrets
	}
	if in.scheduleTimeoutS != nil {
		job.ScheduleTimeoutS = e.scheduleTimeoutS
	}
	if in.readRelay != nil {
		job.GitHubReadProxy = e.readRelay
	}
}
