package dashboard

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
)

// Sandbox Job scheduling, resources, preflight and read relay settings in the
// dashboard (hivecommons/hive#19553). These were hive.yaml-only: an owner
// running an agent on accelerator nodes had to hand-edit node_selector,
// resources and tolerations. The same parsers serve the hive-wide block
// (PUT /api/config/governor/security, agentSandbox* keys) and one agent's
// block (PUT /api/config/agent/<name>/general, sandboxJob and
// sandboxPreflight), so both refuse exactly the same values.
//
// Wire shapes (camelCase like the rest of the dashboard API):
//
//	nodeSelector     {"<label key>": "<label value>"}
//	resources        {"limits": {"<resource>": "<quantity>"}, "requests": {...}}
//	tolerations      [{"key", "operator", "value", "effect"}]
//	scheduleTimeoutS whole seconds, 0 uses config.DefaultSandboxScheduleTimeoutS
//	preflight        {"commands": [...], "timeoutS", "totalTimeoutS", "retryDelayS"}
//	setup            {"commands": [...], "healthCommands": [...], "timeoutS", "totalTimeoutS"}
//	                 (sandbox.setup, #19574; body keys sandboxSetup and agentSandboxSetup)
//	githubReadProxy  {"enabled", "listenAddr", "endpoint", "noProxy": [...]}

// Bounds on the operator-supplied lists and maps. Kubernetes has no hard cap
// on selector or resource entries; these keep a pasted blob out of the config.
const (
	maxSandboxJobMapEntries     = 64
	maxSandboxJobTolerations    = 32
	maxSandboxNoProxyHosts      = 64
	maxSandboxNoProxyHostLen    = 253
	maxK8sLabelNameLen          = 63
	maxK8sLabelPrefixLen        = 253
	maxSandboxPreflightTimeoutS = 24 * 60 * 60
	// maxSandboxSetupTimeoutS bounds the setup step's timeouts the same way.
	maxSandboxSetupTimeoutS = maxSandboxPreflightTimeoutS
)

// Kubernetes toleration operators and taint effects. An empty operator means
// Equal and an empty effect matches every effect, as in Kubernetes.
const (
	tolerationOpEqual           = "Equal"
	tolerationOpExists          = "Exists"
	taintEffectNoSchedule       = "NoSchedule"
	taintEffectPreferNoSchedule = "PreferNoSchedule"
	taintEffectNoExecute        = "NoExecute"
)

var validTolerationOps = map[string]bool{"": true, tolerationOpEqual: true, tolerationOpExists: true}

var validTaintEffects = map[string]bool{"": true, taintEffectNoSchedule: true, taintEffectPreferNoSchedule: true, taintEffectNoExecute: true}

// k8sNameRE is the name part of a Kubernetes qualified name (label key,
// resource name) and the shape of a non-empty label value.
var k8sNameRE = regexp.MustCompile(`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`)

// k8sQuantityRE is a Kubernetes resource quantity without a sign: "2",
// "500m", "1.5", "16Gi", "1e3". A negative quantity is never a valid request.
var k8sQuantityRE = regexp.MustCompile(`^([0-9]+(\.[0-9]*)?|\.[0-9]+)([KMGTPE]i|[numkMGTPE]|[eE][+-]?[0-9]+)?$`)

// noProxyHostRE is a host or domain (optionally with a leading dot or a
// port) for github_read_proxy.no_proxy.
var noProxyHostRE = regexp.MustCompile(`^\.?[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?(:[0-9]+)?$`)

// Sandbox job sources reported by the effective view.
const (
	sandboxSourceAgent  = "agent"
	sandboxSourceGlobal = "agent_sandbox"
	sandboxSourceUnset  = ""
)

// validateK8sQualifiedName checks a label key or resource name:
// [prefix/]name, prefix a DNS subdomain, name at most 63 characters.
func validateK8sQualifiedName(field, s string) error {
	name := s
	if i := strings.LastIndex(s, "/"); i >= 0 {
		prefix := s[:i]
		name = s[i+1:]
		if prefix == "" || len(prefix) > maxK8sLabelPrefixLen || !secretNameRE.MatchString(prefix) {
			return fmt.Errorf("%s: %q has an invalid prefix (a lowercase DNS name such as example.com)", field, s)
		}
	}
	if name == "" || len(name) > maxK8sLabelNameLen || !k8sNameRE.MatchString(name) {
		return fmt.Errorf("%s: %q is not a valid Kubernetes name (letters, digits, '-', '_' or '.', at most %d characters, optionally prefixed by a DNS name and '/')", field, s, maxK8sLabelNameLen)
	}
	return nil
}

// stringMapFromBody reads a JSON object of strings. Numbers are accepted and
// formatted, so a quantity typed as 2 is "2". nil or {} is nil.
func stringMapFromBody(field string, v interface{}) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object of key: value pairs", field)
	}
	if len(obj) > maxSandboxJobMapEntries {
		return nil, fmt.Errorf("%s takes at most %d entries, got %d", field, maxSandboxJobMapEntries, len(obj))
	}
	out := make(map[string]string, len(obj))
	for k, raw := range obj {
		key := strings.TrimSpace(k)
		var val string
		switch x := raw.(type) {
		case string:
			val = strings.TrimSpace(x)
		case float64:
			val = strconv.FormatFloat(x, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("%s: the value of %q must be a string", field, key)
		}
		if _, found := logscrub.FindSecret(key + " " + val); found {
			return nil, fmt.Errorf("%s: %q looks like it carries a credential; this field is written to the Job manifest", field, key)
		}
		out[key] = val
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// nodeSelectorFromBody reads and validates job.node_selector.
func nodeSelectorFromBody(field string, v interface{}) (map[string]string, error) {
	m, err := stringMapFromBody(field, v)
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(m) {
		if err := validateK8sQualifiedName(field, k); err != nil {
			return nil, err
		}
		if val := m[k]; val != "" && (len(val) > maxK8sLabelNameLen || !k8sNameRE.MatchString(val)) {
			return nil, fmt.Errorf("%s: %q=%q is not a valid label value (letters, digits, '-', '_' or '.', at most %d characters)", field, k, val, maxK8sLabelNameLen)
		}
	}
	return m, nil
}

// resourceMapFromBody reads and validates job.resources.limits or .requests.
func resourceMapFromBody(field string, v interface{}) (map[string]string, error) {
	m, err := stringMapFromBody(field, v)
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(m) {
		if err := validateK8sQualifiedName(field, k); err != nil {
			return nil, err
		}
		if !k8sQuantityRE.MatchString(m[k]) {
			return nil, fmt.Errorf("%s: %q=%q is not a Kubernetes quantity (for example 2, 500m, 16Gi)", field, k, m[k])
		}
	}
	return m, nil
}

// resourcesFromBody reads job.resources: {"limits": {...}, "requests": {...}}.
// A key left out keeps current's map, so the limits editor can save without
// the requests one; null clears both.
func resourcesFromBody(field string, v interface{}, current config.SandboxJobResources) (config.SandboxJobResources, error) {
	if v == nil {
		return config.SandboxJobResources{}, nil
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return current, fmt.Errorf("%s must be an object with limits and requests", field)
	}
	out := current
	for _, f := range []struct {
		key string
		dst *map[string]string
	}{{"limits", &out.Limits}, {"requests", &out.Requests}} {
		raw, present := obj[f.key]
		if !present {
			continue
		}
		m, err := resourceMapFromBody(field+"."+f.key, raw)
		if err != nil {
			return current, err
		}
		*f.dst = m
	}
	return out, nil
}

// tolerationsFromBody reads and validates job.tolerations.
func tolerationsFromBody(field string, v interface{}) ([]config.SandboxJobToleration, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a list", field)
	}
	if len(arr) > maxSandboxJobTolerations {
		return nil, fmt.Errorf("%s takes at most %d entries, got %d", field, maxSandboxJobTolerations, len(arr))
	}
	out := make([]config.SandboxJobToleration, 0, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be an object with key, operator, value and effect", field, i)
		}
		t := config.SandboxJobToleration{
			Key:      strings.TrimSpace(stringField(obj, "key")),
			Operator: strings.TrimSpace(stringField(obj, "operator")),
			Value:    strings.TrimSpace(stringField(obj, "value")),
			Effect:   strings.TrimSpace(stringField(obj, "effect")),
		}
		if !validTolerationOps[t.Operator] {
			return nil, fmt.Errorf("%s[%d].operator must be %s or %s, got %q", field, i, tolerationOpEqual, tolerationOpExists, t.Operator)
		}
		if !validTaintEffects[t.Effect] {
			return nil, fmt.Errorf("%s[%d].effect must be empty (every effect), %s, %s or %s, got %q", field, i, taintEffectNoSchedule, taintEffectPreferNoSchedule, taintEffectNoExecute, t.Effect)
		}
		if t.Operator == tolerationOpExists && t.Value != "" {
			return nil, fmt.Errorf("%s[%d]: operator %s takes no value", field, i, tolerationOpExists)
		}
		if t.Key == "" && t.Operator != tolerationOpExists {
			return nil, fmt.Errorf("%s[%d]: an empty key needs operator %s (it tolerates every taint)", field, i, tolerationOpExists)
		}
		if t.Key != "" {
			if err := validateK8sQualifiedName(fmt.Sprintf("%s[%d].key", field, i), t.Key); err != nil {
				return nil, err
			}
		}
		if t.Value != "" && (len(t.Value) > maxK8sLabelNameLen || !k8sNameRE.MatchString(t.Value)) {
			return nil, fmt.Errorf("%s[%d].value %q is not a valid taint value", field, i, t.Value)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// wholeSecondsFromBody reads a non-negative whole number of seconds.
func wholeSecondsFromBody(field string, v interface{}, limit int) (int, error) {
	if v == nil {
		return 0, nil
	}
	n, ok := v.(float64)
	if !ok || n < 0 || n != float64(int(n)) || int(n) > limit {
		return 0, fmt.Errorf("%s must be a whole number of seconds from 0 to %d", field, limit)
	}
	return int(n), nil
}

// commandListFromBody reads a list of shell commands, blanks dropped.
func commandListFromBody(field string, raw interface{}) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	arr, isArr := raw.([]interface{})
	if !isArr {
		return nil, fmt.Errorf("%s must be a list of shell commands", field)
	}
	var out []string
	for _, item := range arr {
		s, _ := item.(string)
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// refuseCredentialCommands refuses a command that carries a credential shape:
// every command here is written to the Job manifest and the pod log.
func refuseCredentialCommands(field string, cmds []string) error {
	for i, c := range cmds {
		if category, found := logscrub.FindSecret(c); found {
			return fmt.Errorf("%s[%d] looks like it carries a credential (%s). Commands are written to the Job manifest and the pod log; pass secrets with env_from_secrets instead", field, i, category)
		}
	}
	return nil
}

// preflightFromBody reads a sandbox.preflight block. nil means "no block"
// (inherit, or off hive-wide); an object with no commands is a block that
// turns preflight off, which only means something per agent.
func preflightFromBody(field string, v interface{}) (*config.SandboxPreflightConfig, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object with commands", field)
	}
	p := &config.SandboxPreflightConfig{}
	var err error
	if p.Commands, err = commandListFromBody(field+".commands", obj["commands"]); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		key, yaml string
		dst       *int
	}{
		{"timeoutS", "timeout_s", &p.TimeoutS},
		{"totalTimeoutS", "total_timeout_s", &p.TotalTimeoutS},
		{"retryDelayS", "retry_delay_s", &p.RetryDelayS},
	} {
		if *f.dst, err = wholeSecondsFromBody(field+"."+f.yaml, obj[f.key], maxSandboxPreflightTimeoutS); err != nil {
			return nil, err
		}
	}
	if err := config.ValidateSandboxPreflight(p); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	if err := refuseCredentialCommands(field+".commands", p.Commands); err != nil {
		return nil, err
	}
	return p, nil
}

// setupFromBody reads a sandbox.setup block (#19574), the same way: nil is no
// block, an object with neither list is a block that turns setup off.
func setupFromBody(field string, v interface{}) (*config.SandboxSetupConfig, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object with commands and healthCommands", field)
	}
	p := &config.SandboxSetupConfig{}
	var err error
	if p.Commands, err = commandListFromBody(field+".commands", obj["commands"]); err != nil {
		return nil, err
	}
	if p.HealthCommands, err = commandListFromBody(field+".health_commands", obj["healthCommands"]); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		key, yaml string
		dst       *int
	}{
		{"timeoutS", "timeout_s", &p.TimeoutS},
		{"totalTimeoutS", "total_timeout_s", &p.TotalTimeoutS},
	} {
		if *f.dst, err = wholeSecondsFromBody(field+"."+f.yaml, obj[f.key], maxSandboxSetupTimeoutS); err != nil {
			return nil, err
		}
	}
	if err := config.ValidateSandboxSetup(p); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	if err := refuseCredentialCommands(field+".commands", p.Commands); err != nil {
		return nil, err
	}
	if err := refuseCredentialCommands(field+".health_commands", p.HealthCommands); err != nil {
		return nil, err
	}
	return p, nil
}

// readProxyFromBody reads agent_sandbox.job.github_read_proxy. An all-empty
// block is nil, so clearing the fields removes it from the config.
func readProxyFromBody(field string, v interface{}) (*config.SandboxGitHubReadProxy, error) {
	if v == nil {
		return nil, nil
	}
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	rp := &config.SandboxGitHubReadProxy{
		ListenAddr: strings.TrimSpace(stringField(obj, "listenAddr")),
		Endpoint:   strings.TrimSpace(stringField(obj, "endpoint")),
	}
	if b, ok := obj["enabled"].(bool); ok {
		rp.Enabled = b
	}
	if rp.ListenAddr != "" {
		if _, port, err := net.SplitHostPort(rp.ListenAddr); err != nil || port == "" {
			return nil, fmt.Errorf("%s.listen_addr must be host:port, for example :8443", field)
		}
	}
	if raw, ok := obj["noProxy"]; ok && raw != nil {
		arr, isArr := raw.([]interface{})
		if !isArr {
			return nil, fmt.Errorf("%s.no_proxy must be a list of hosts", field)
		}
		for _, item := range arr {
			s, _ := item.(string)
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if len(s) > maxSandboxNoProxyHostLen || !noProxyHostRE.MatchString(s) {
				return nil, fmt.Errorf("%s.no_proxy: %q is not a host or domain", field, s)
			}
			rp.NoProxy = append(rp.NoProxy, s)
		}
		if len(rp.NoProxy) > maxSandboxNoProxyHosts {
			return nil, fmt.Errorf("%s.no_proxy takes at most %d hosts", field, maxSandboxNoProxyHosts)
		}
	}
	if !rp.Enabled && rp.ListenAddr == "" && rp.Endpoint == "" && len(rp.NoProxy) == 0 {
		return nil, nil
	}
	if err := rp.Validate(); err != nil {
		return nil, err
	}
	if rp.Endpoint != "" && !rp.Enabled {
		// Validate skips a disabled block; still refuse an endpoint that
		// would fail the moment the owner turns it on.
		probe := *rp
		probe.Enabled = true
		if probe.ListenAddr == "" {
			probe.ListenAddr = ":0"
		}
		if err := probe.Validate(); err != nil {
			return nil, err
		}
	}
	return rp, nil
}

// rawJSONValue decodes one json.RawMessage body field into the generic value
// the parsers above take. present is false for an absent key.
func rawJSONValue(raw json.RawMessage) (v interface{}, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, err
	}
	return v, true, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nonNilStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilTolerations(in []config.SandboxJobToleration) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, t := range in {
		out = append(out, map[string]string{"key": t.Key, "operator": t.Operator, "value": t.Value, "effect": t.Effect})
	}
	return out
}

func resourcesView(r config.SandboxJobResources) map[string]interface{} {
	return map[string]interface{}{"limits": nonNilStringMap(r.Limits), "requests": nonNilStringMap(r.Requests)}
}

func preflightView(p *config.SandboxPreflightConfig) interface{} {
	if p == nil {
		return nil
	}
	return map[string]interface{}{
		"commands":      nonNilStrings(p.Commands),
		"timeoutS":      p.TimeoutS,
		"totalTimeoutS": p.TotalTimeoutS,
		"retryDelayS":   p.RetryDelayS,
	}
}

func setupView(p *config.SandboxSetupConfig) interface{} {
	if p == nil {
		return nil
	}
	return map[string]interface{}{
		"commands":       nonNilStrings(p.Commands),
		"healthCommands": nonNilStrings(p.HealthCommands),
		"timeoutS":       p.TimeoutS,
		"totalTimeoutS":  p.TotalTimeoutS,
	}
}

func readProxyView(rp *config.SandboxGitHubReadProxy) map[string]interface{} {
	if rp == nil {
		rp = &config.SandboxGitHubReadProxy{}
	}
	return map[string]interface{}{
		"enabled":    rp.Enabled,
		"listenAddr": rp.ListenAddr,
		"endpoint":   rp.Endpoint,
		"noProxy":    nonNilStrings(rp.NoProxy),
	}
}

// sandboxPreflightDefaults are the timeouts a zero preflight field resolves
// to, sent so the UI can show them as placeholders.
func sandboxPreflightDefaults() map[string]int {
	return map[string]int{
		"timeoutS":         config.DefaultSandboxPreflightTimeoutS,
		"totalTimeoutS":    config.DefaultSandboxPreflightTotalTimeoutS,
		"scheduleTimeoutS": config.DefaultSandboxScheduleTimeoutS,
		"maxCommands":      config.MaxSandboxPreflightCommands,
	}
}

// sandboxSetupDefaults are the timeouts a zero setup field resolves to.
func sandboxSetupDefaults() map[string]int {
	return map[string]int{
		"timeoutS":      config.DefaultSandboxSetupTimeoutS,
		"totalTimeoutS": config.DefaultSandboxSetupTotalTimeoutS,
		"maxCommands":   config.MaxSandboxSetupCommands,
	}
}

// agentSandboxEffectiveView is what an agent's Job and preflight settings
// resolve to and where each value comes from. A per-agent job block replaces
// the hive-wide pod-template fields WHOLESALE (config.AgentConfig.SandboxJob),
// so those fields come from the agent as soon as it has any job block; the
// claim, TTL, schedule timeout and pull secrets fall back field by field.
// hiddenDefaults names the hive-wide pod-template fields that are set but not
// applied to this agent because its own job block replaces them.
func agentSandboxEffectiveView(a config.AgentConfig, global config.AgentSandboxConfig) map[string]interface{} {
	eff := a.SandboxJob(global)
	var own config.SandboxJobConfig
	ownJob := a.Sandbox != nil && a.Sandbox.Job != nil
	if ownJob {
		own = *a.Sandbox.Job
	}
	var g config.SandboxJobConfig
	if global.Job != nil {
		g = *global.Job
	}
	wholesale := func(globalSet bool) string {
		switch {
		case ownJob:
			return sandboxSourceAgent
		case globalSet:
			return sandboxSourceGlobal
		default:
			return sandboxSourceUnset
		}
	}
	fallback := func(ownSet, globalSet bool) string {
		switch {
		case ownSet:
			return sandboxSourceAgent
		case globalSet:
			return sandboxSourceGlobal
		default:
			return sandboxSourceUnset
		}
	}
	globalResources := len(g.Resources.Limits) > 0 || len(g.Resources.Requests) > 0
	sources := map[string]string{
		"nodeSelector":     wholesale(len(g.NodeSelector) > 0),
		"resources":        wholesale(globalResources),
		"tolerations":      wholesale(len(g.Tolerations) > 0),
		"serviceAccount":   wholesale(g.ServiceAccount != ""),
		"schedulerName":    wholesale(g.SchedulerName != ""),
		"ttlSeconds":       fallback(own.TTLSeconds != 0, g.TTLSeconds != 0),
		"scheduleTimeoutS": fallback(own.ScheduleTimeoutS != 0, g.ScheduleTimeoutS != 0),
		"imagePullSecrets": fallback(len(own.ImagePullSecrets) > 0, len(g.ImagePullSecrets) > 0),
	}
	var hidden []string
	if ownJob {
		for _, f := range []struct {
			name      string
			globalSet bool
			ownSet    bool
		}{
			{"node_selector", len(g.NodeSelector) > 0, len(own.NodeSelector) > 0},
			{"resources", globalResources, len(own.Resources.Limits) > 0 || len(own.Resources.Requests) > 0},
			{"tolerations", len(g.Tolerations) > 0, len(own.Tolerations) > 0},
			{"service_account", g.ServiceAccount != "", own.ServiceAccount != ""},
			{"scheduler_name", g.SchedulerName != "", own.SchedulerName != ""},
		} {
			if f.globalSet && !f.ownSet {
				hidden = append(hidden, f.name)
			}
		}
	}
	pre := a.SandboxPreflight(global)
	preSource := sandboxSourceUnset
	switch {
	case a.Sandbox != nil && a.Sandbox.Preflight != nil:
		preSource = sandboxSourceAgent
	case global.Preflight != nil:
		preSource = sandboxSourceGlobal
	}
	setup := a.SandboxSetup(global)
	setupSource := sandboxSourceUnset
	switch {
	case a.Sandbox != nil && a.Sandbox.Setup != nil:
		setupSource = sandboxSourceAgent
	case global.Setup != nil:
		setupSource = sandboxSourceGlobal
	}
	scheduleTimeout := eff.ScheduleTimeoutS
	if scheduleTimeout == 0 {
		scheduleTimeout = config.DefaultSandboxScheduleTimeoutS
	}
	return map[string]interface{}{
		"job": map[string]interface{}{
			"nodeSelector":     nonNilStringMap(eff.NodeSelector),
			"resources":        resourcesView(eff.Resources),
			"tolerations":      nonNilTolerations(eff.Tolerations),
			"serviceAccount":   eff.ServiceAccount,
			"schedulerName":    eff.SchedulerName,
			"ttlSeconds":       eff.TTLSeconds,
			"scheduleTimeoutS": scheduleTimeout,
			"imagePullSecrets": nonNilStrings(eff.ImagePullSecrets),
		},
		"sources":         sources,
		"hiddenDefaults":  nonNilStrings(hidden),
		"preflight":       preflightView(&pre),
		"preflightSource": preSource,
		"setup":           setupView(&setup),
		"setupSource":     setupSource,
	}
}

// sandboxJobDefaultsBody is the part of PUT /api/config/governor/security
// that edits the hive-wide Job scheduling, preflight, setup and read relay
// settings.
// Each key is raw so an absent key is "unchanged" and null clears the field.
type sandboxJobDefaultsBody struct {
	NodeSelector     json.RawMessage `json:"agentSandboxJobNodeSelector"`
	Resources        json.RawMessage `json:"agentSandboxJobResources"`
	Tolerations      json.RawMessage `json:"agentSandboxJobTolerations"`
	ImagePullSecrets json.RawMessage `json:"agentSandboxJobImagePullSecrets"`
	ScheduleTimeoutS json.RawMessage `json:"agentSandboxJobScheduleTimeoutS"`
	Preflight        json.RawMessage `json:"agentSandboxPreflight"`
	Setup            json.RawMessage `json:"agentSandboxSetup"`
	GitHubReadProxy  json.RawMessage `json:"agentSandboxJobGithubReadProxy"`
}

// sandboxJobDefaultsEdit is a validated hive-wide edit, applied only after
// every other field in the request has validated.
type sandboxJobDefaultsEdit struct {
	job          *config.SandboxJobConfig
	jobChanged   bool
	preflight    *config.SandboxPreflightConfig
	preflightSet bool
	setup        *config.SandboxSetupConfig
	setupSet     bool
}

// parse validates the body against the current block without changing it.
func (b sandboxJobDefaultsBody) parse(current config.AgentSandboxConfig) (sandboxJobDefaultsEdit, error) {
	var edit sandboxJobDefaultsEdit
	job := config.SandboxJobConfig{}
	if current.Job != nil {
		job = *current.Job
	}
	scheduling := map[string]interface{}{}
	for _, f := range []struct {
		key string
		raw json.RawMessage
	}{
		{"nodeSelector", b.NodeSelector},
		{"resources", b.Resources},
		{"tolerations", b.Tolerations},
		{"scheduleTimeoutS", b.ScheduleTimeoutS},
	} {
		v, present, err := rawJSONValue(f.raw)
		if err != nil {
			return edit, fmt.Errorf("invalid %s", f.key)
		}
		if present {
			scheduling[f.key] = v
		}
	}
	if err := applySandboxJobScheduling(&job, scheduling, "agent_sandbox.job"); err != nil {
		return edit, err
	}
	edit.jobChanged = len(scheduling) > 0
	if v, present, err := rawJSONValue(b.ImagePullSecrets); err != nil {
		return edit, fmt.Errorf("invalid agentSandboxJobImagePullSecrets")
	} else if present {
		names, err := secretNamesFromBody(v)
		if err != nil {
			return edit, err
		}
		if err := config.ValidateImagePullSecrets(names); err != nil {
			return edit, err
		}
		job.ImagePullSecrets = names
		edit.jobChanged = true
	}
	if v, present, err := rawJSONValue(b.GitHubReadProxy); err != nil {
		return edit, fmt.Errorf("invalid agentSandboxJobGithubReadProxy")
	} else if present {
		rp, err := readProxyFromBody("agent_sandbox.job.github_read_proxy", v)
		if err != nil {
			return edit, err
		}
		job.GitHubReadProxy = rp
		edit.jobChanged = true
	}
	if edit.jobChanged {
		edit.job = &job
	}
	if v, present, err := rawJSONValue(b.Preflight); err != nil {
		return edit, fmt.Errorf("invalid agentSandboxPreflight")
	} else if present {
		pre, err := preflightFromBody("agent_sandbox.preflight", v)
		if err != nil {
			return edit, err
		}
		// Hive-wide, an empty block and no block both mean "off"; store none.
		if pre != nil && len(pre.Commands) == 0 && pre.TimeoutS == 0 && pre.TotalTimeoutS == 0 && pre.RetryDelayS == 0 {
			pre = nil
		}
		edit.preflight = pre
		edit.preflightSet = true
	}
	if v, present, err := rawJSONValue(b.Setup); err != nil {
		return edit, fmt.Errorf("invalid agentSandboxSetup")
	} else if present {
		setup, err := setupFromBody("agent_sandbox.setup", v)
		if err != nil {
			return edit, err
		}
		// As for preflight: hive-wide, an empty block means off; store none.
		if setup != nil && !setup.Enabled() && setup.TimeoutS == 0 && setup.TotalTimeoutS == 0 {
			setup = nil
		}
		edit.setup = setup
		edit.setupSet = true
	}
	return edit, nil
}

// apply writes a parsed edit into the live block.
func (e sandboxJobDefaultsEdit) apply(sb *config.AgentSandboxConfig) {
	if e.jobChanged {
		sb.Job = e.job
	}
	if e.preflightSet {
		sb.Preflight = e.preflight
	}
	if e.setupSet {
		sb.Setup = e.setup
	}
}

// sandboxJobDefaultsResponse is the hive-wide scheduling, preflight and read
// relay part of the Security section response.
func sandboxJobDefaultsResponse(cfg *config.Config) map[string]interface{} {
	job := sandboxJobOrEmpty(cfg)
	return map[string]interface{}{
		"agentSandboxJobNodeSelector":     nonNilStringMap(job.NodeSelector),
		"agentSandboxJobResources":        resourcesView(job.Resources),
		"agentSandboxJobTolerations":      nonNilTolerations(job.Tolerations),
		"agentSandboxJobImagePullSecrets": nonNilStrings(job.ImagePullSecrets),
		"agentSandboxJobScheduleTimeoutS": job.ScheduleTimeoutS,
		"agentSandboxPreflight":           preflightView(cfg.AgentSandbox.Preflight),
		"agentSandboxPreflightDefaults":   sandboxPreflightDefaults(),
		"agentSandboxSetup":               setupView(cfg.AgentSandbox.Setup),
		"agentSandboxSetupDefaults":       sandboxSetupDefaults(),
		"agentSandboxJobGithubReadProxy":  readProxyView(job.GitHubReadProxy),
	}
}
