package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19553: the sandbox Job scheduling, resources, tolerations,
// pull secrets, preflight and read relay settings are editable from the
// dashboard, hive-wide and per agent, with the same validation.

const spyreResource = "ibm.com/spyre_pf"

func TestAgentSandboxJobSchedulingEditable(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxJob": map[string]any{
			"nodeSelector": map[string]any{"accelerator": "spyre", "topology.kubernetes.io/zone": "z1"},
			"resources": map[string]any{
				"limits":   map[string]any{spyreResource: "2", "memory": "32Gi"},
				"requests": map[string]any{"cpu": "500m"},
			},
			"tolerations":      []any{map[string]any{"key": "accelerator", "operator": "Exists", "effect": "NoSchedule"}},
			"imagePullSecrets": []any{"registry-pull"},
			"scheduleTimeoutS": 900,
		},
		"sandboxPreflight": map[string]any{"commands": []any{"python -c 'import torch'", " "}, "timeoutS": 60},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	sb := deps.Config.Agents["scanner"].Sandbox
	if sb == nil || sb.Job == nil {
		t.Fatalf("no job block stored: %#v", sb)
	}
	job := sb.Job
	if job.NodeSelector["accelerator"] != "spyre" || job.NodeSelector["topology.kubernetes.io/zone"] != "z1" {
		t.Errorf("node_selector = %v", job.NodeSelector)
	}
	if job.Resources.Limits[spyreResource] != "2" || job.Resources.Limits["memory"] != "32Gi" || job.Resources.Requests["cpu"] != "500m" {
		t.Errorf("resources = %#v", job.Resources)
	}
	if len(job.Tolerations) != 1 || job.Tolerations[0] != (config.SandboxJobToleration{Key: "accelerator", Operator: "Exists", Effect: "NoSchedule"}) {
		t.Errorf("tolerations = %#v", job.Tolerations)
	}
	if strings.Join(job.ImagePullSecrets, ",") != "registry-pull" || job.ScheduleTimeoutS != 900 {
		t.Errorf("pull secrets / schedule timeout = %v / %d", job.ImagePullSecrets, job.ScheduleTimeoutS)
	}
	if sb.Preflight == nil || len(sb.Preflight.Commands) != 1 || sb.Preflight.TimeoutS != 60 {
		t.Errorf("preflight = %#v", sb.Preflight)
	}

	fields := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)["sandboxFields"].(map[string]any)
	own := fields["sandboxJob"].(map[string]any)
	if own["nodeSelector"].(map[string]any)["accelerator"] != "spyre" {
		t.Errorf("GET nodeSelector = %v", own["nodeSelector"])
	}
	if own["resources"].(map[string]any)["limits"].(map[string]any)[spyreResource] != "2" {
		t.Errorf("GET resources = %v", own["resources"])
	}
	eff := fields["sandboxEffective"].(map[string]any)
	if eff["sources"].(map[string]any)["nodeSelector"] != sandboxSourceAgent || eff["preflightSource"] != sandboxSourceAgent {
		t.Errorf("effective sources = %v / %v", eff["sources"], eff["preflightSource"])
	}

	// Saving only the limits keeps the requests.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxJob": map[string]any{"resources": map[string]any{"limits": map[string]any{spyreResource: "1"}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("limits-only PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if res := deps.Config.Agents["scanner"].Sandbox.Job.Resources; res.Limits[spyreResource] != "1" || res.Limits["memory"] != "" || res.Requests["cpu"] != "500m" {
		t.Fatalf("limits-only edit = %#v, want limits replaced and requests kept", res)
	}

	// Clearing every field drops the job block, and a null preflight inherits.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxJob": map[string]any{
			"nodeSelector": map[string]any{}, "resources": nil, "tolerations": []any{},
			"imagePullSecrets": []any{}, "scheduleTimeoutS": 0,
		},
		"sandboxPreflight": nil,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if sb := deps.Config.Agents["scanner"].Sandbox; sb.Job != nil || sb.Preflight != nil {
		t.Fatalf("not cleared: job=%#v preflight=%#v", sb.Job, sb.Preflight)
	}
}

func TestAgentSandboxPreflightEmptyListTurnsItOff(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	deps.Config.AgentSandbox.Preflight = &config.SandboxPreflightConfig{Commands: []string{"true"}}
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxPreflight": map[string]any{"commands": []any{}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	a := deps.Config.Agents["scanner"]
	if a.Sandbox == nil || a.Sandbox.Preflight == nil || len(a.Sandbox.Preflight.Commands) != 0 {
		t.Fatalf("an empty override block must be kept: %#v", a.Sandbox)
	}
	if eff := a.SandboxPreflight(deps.Config.AgentSandbox); len(eff.Commands) != 0 {
		t.Fatalf("effective preflight = %v, want off", eff.Commands)
	}
}

func TestAgentSandboxJobSchedulingValidation(t *testing.T) {
	s, deps := apiServer(t)
	cases := map[string]map[string]any{
		"selector key":     {"sandboxJob": map[string]any{"nodeSelector": map[string]any{"bad key": "x"}}},
		"selector value":   {"sandboxJob": map[string]any{"nodeSelector": map[string]any{"a": "has space"}}},
		"selector prefix":  {"sandboxJob": map[string]any{"nodeSelector": map[string]any{"Bad_Prefix/a": "x"}}},
		"quantity":         {"sandboxJob": map[string]any{"resources": map[string]any{"limits": map[string]any{spyreResource: "two"}}}},
		"negative qty":     {"sandboxJob": map[string]any{"resources": map[string]any{"requests": map[string]any{"cpu": "-1"}}}},
		"empty qty":        {"sandboxJob": map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": ""}}}},
		"tol operator":     {"sandboxJob": map[string]any{"tolerations": []any{map[string]any{"key": "a", "operator": "In"}}}},
		"tol effect":       {"sandboxJob": map[string]any{"tolerations": []any{map[string]any{"key": "a", "effect": "Never"}}}},
		"tol exists value": {"sandboxJob": map[string]any{"tolerations": []any{map[string]any{"key": "a", "operator": "Exists", "value": "b"}}}},
		"tol empty key":    {"sandboxJob": map[string]any{"tolerations": []any{map[string]any{"operator": "Equal", "value": "b"}}}},
		"pull secret val":  {"sandboxJob": map[string]any{"imagePullSecrets": []any{"user:pass"}}},
		"schedule neg":     {"sandboxJob": map[string]any{"scheduleTimeoutS": -5}},
		"preflight shape":  {"sandboxPreflight": "true"},
		"preflight neg":    {"sandboxPreflight": map[string]any{"commands": []any{"true"}, "retryDelayS": -1}},
		"preflight cred":   {"sandboxPreflight": map[string]any{"commands": []any{"curl -H 'Authorization: token ghp_" + strings.Repeat("a", 36) + "' x"}}},
		"preflight long":   {"sandboxPreflight": map[string]any{"commands": []any{strings.Repeat("x", 5000)}}},
	}
	for name, body := range cases {
		rec := doPut(s, "/api/config/agent/scanner/general", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if deps.Config.Agents["scanner"].Sandbox != nil {
		t.Fatalf("a refused edit wrote a block: %#v", deps.Config.Agents["scanner"].Sandbox)
	}
}

func TestAgentSandboxEffectiveViewShowsInheritedAndHiddenDefaults(t *testing.T) {
	global := config.AgentSandboxConfig{
		Job: &config.SandboxJobConfig{
			NodeSelector:     map[string]string{"accelerator": "spyre"},
			ServiceAccount:   "hive-job",
			TTLSeconds:       120,
			ImagePullSecrets: []string{"reg"},
		},
		Preflight: &config.SandboxPreflightConfig{Commands: []string{"true"}},
	}
	inherit := agentSandboxEffectiveView(config.AgentConfig{}, global)
	src := inherit["sources"].(map[string]string)
	if src["nodeSelector"] != sandboxSourceGlobal || src["serviceAccount"] != sandboxSourceGlobal || src["resources"] != sandboxSourceUnset {
		t.Errorf("inherit sources = %v", src)
	}
	if inherit["preflightSource"] != sandboxSourceGlobal {
		t.Errorf("preflightSource = %v", inherit["preflightSource"])
	}
	if hidden := inherit["hiddenDefaults"].([]string); len(hidden) != 0 {
		t.Errorf("hiddenDefaults without an own block = %v", hidden)
	}
	// An agent with its own job block (only a scheduler name) replaces the
	// pod-template fields: the hive-wide node selector and service account
	// no longer apply, while TTL and pull secrets still fall back.
	own := config.AgentConfig{Sandbox: &config.AgentSandboxOverride{Job: &config.SandboxJobConfig{SchedulerName: "spyre-scheduler"}}}
	view := agentSandboxEffectiveView(own, global)
	job := view["job"].(map[string]interface{})
	if len(job["nodeSelector"].(map[string]string)) != 0 || job["ttlSeconds"] != 120 || strings.Join(job["imagePullSecrets"].([]string), ",") != "reg" {
		t.Errorf("effective job = %v", job)
	}
	src = view["sources"].(map[string]string)
	if src["nodeSelector"] != sandboxSourceAgent || src["ttlSeconds"] != sandboxSourceGlobal || src["imagePullSecrets"] != sandboxSourceGlobal {
		t.Errorf("own-block sources = %v", src)
	}
	if hidden := strings.Join(view["hiddenDefaults"].([]string), ","); hidden != "node_selector,service_account" {
		t.Errorf("hiddenDefaults = %q, want node_selector,service_account", hidden)
	}
	if job["scheduleTimeoutS"] != config.DefaultSandboxScheduleTimeoutS {
		t.Errorf("scheduleTimeoutS = %v, want the default", job["scheduleTimeoutS"])
	}
}

func TestSecuritySectionSetsJobSchedulingDefaults(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.AgentSandbox.Job = &config.SandboxJobConfig{ServiceAccount: "keep-me"}
	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxJobNodeSelector":     map[string]any{"accelerator": "spyre"},
		"agentSandboxJobResources":        map[string]any{"limits": map[string]any{spyreResource: "1"}},
		"agentSandboxJobTolerations":      []any{map[string]any{"key": "accelerator", "operator": "Exists"}},
		"agentSandboxJobImagePullSecrets": []any{"reg"},
		"agentSandboxJobScheduleTimeoutS": 300,
		"agentSandboxPreflight":           map[string]any{"commands": []any{"nvidia-smi || true"}, "totalTimeoutS": 200},
		"agentSandboxJobGithubReadProxy":  map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "https://hive-read-relay:8443", "noProxy": []any{"registry.internal"}},
		"agentSandboxJobSchedulerName":    "spyre-scheduler",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	g := deps.Config.AgentSandbox
	if g.Job == nil || g.Job.ServiceAccount != "keep-me" || g.Job.SchedulerName != "spyre-scheduler" {
		t.Fatalf("existing job fields lost or scheduler not set: %#v", g.Job)
	}
	if g.Job.NodeSelector["accelerator"] != "spyre" || g.Job.Resources.Limits[spyreResource] != "1" || len(g.Job.Tolerations) != 1 ||
		strings.Join(g.Job.ImagePullSecrets, ",") != "reg" || g.Job.ScheduleTimeoutS != 300 {
		t.Fatalf("job defaults not stored: %#v", g.Job)
	}
	if rp := g.Job.GitHubReadProxy; rp == nil || !rp.Enabled || rp.Listen() != ":8443" || rp.Endpoint != "https://hive-read-relay:8443" || strings.Join(rp.NoProxy, ",") != "registry.internal" {
		t.Fatalf("read relay = %#v", g.Job.GitHubReadProxy)
	}
	if g.Preflight == nil || strings.Join(g.Preflight.Commands, ",") != "nvidia-smi || true" || g.Preflight.TotalTimeoutS != 200 {
		t.Fatalf("preflight = %#v", g.Preflight)
	}
	// What the dashboard stored passes the same checks config load runs.
	if err := g.Job.GitHubReadProxy.Validate(); err != nil {
		t.Fatalf("stored read relay fails config validation: %v", err)
	}
	if err := config.ValidateSandboxPreflight(g.Preflight); err != nil {
		t.Fatalf("stored preflight fails config validation: %v", err)
	}

	sec := decodeJSON(t, doGet(s, "/api/config/governor"))
	section, ok := sec["security"].(map[string]any)
	if !ok {
		t.Fatalf("GET /api/config/governor has no security section: %v", sec)
	}
	if sel, _ := section["agentSandboxJobNodeSelector"].(map[string]any); sel["accelerator"] != "spyre" {
		t.Errorf("GET node selector = %v", section["agentSandboxJobNodeSelector"])
	}
	if rp, _ := section["agentSandboxJobGithubReadProxy"].(map[string]any); rp["enabled"] != true {
		t.Errorf("GET read relay = %v", section["agentSandboxJobGithubReadProxy"])
	}
	resp := securitySectionResponse(deps.Config)
	if resp["agentSandboxPreflight"] == nil || resp["agentSandboxJobScheduleTimeoutS"] != 300 {
		t.Errorf("response = %v / %v", resp["agentSandboxPreflight"], resp["agentSandboxJobScheduleTimeoutS"])
	}

	// An empty hive-wide preflight removes the block; a disabled, empty relay
	// block is dropped.
	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxPreflight":          map[string]any{"commands": []any{}},
		"agentSandboxJobGithubReadProxy": map[string]any{"enabled": false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.AgentSandbox.Preflight != nil || deps.Config.AgentSandbox.Job.GitHubReadProxy != nil {
		t.Fatalf("not cleared: %#v / %#v", deps.Config.AgentSandbox.Preflight, deps.Config.AgentSandbox.Job.GitHubReadProxy)
	}
}

func TestSecuritySectionJobDefaultsValidation(t *testing.T) {
	s, deps := apiServer(t)
	cases := map[string]map[string]any{
		"selector":       {"agentSandboxJobNodeSelector": map[string]any{"bad key": "x"}},
		"quantity":       {"agentSandboxJobResources": map[string]any{"limits": map[string]any{"cpu": "lots"}}},
		"toleration":     {"agentSandboxJobTolerations": []any{map[string]any{"key": "a", "effect": "Sometimes"}}},
		"pull secret":    {"agentSandboxJobImagePullSecrets": []any{"KEY=value"}},
		"schedule":       {"agentSandboxJobScheduleTimeoutS": -1},
		"preflight":      {"agentSandboxPreflight": map[string]any{"timeoutS": -1}},
		"relay no addr":  {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "endpoint": "https://relay:8443"}},
		"relay http":     {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "http://relay:8443"}},
		"relay creds":    {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "https://u:p@relay:8443"}},
		"relay bad addr": {"agentSandboxJobGithubReadProxy": map[string]any{"listenAddr": "8443"}},
		"relay off http": {"agentSandboxJobGithubReadProxy": map[string]any{"endpoint": "http://relay:8443"}},
		"no proxy":       {"agentSandboxJobGithubReadProxy": map[string]any{"noProxy": []any{"has space"}}},
		// A bad job default refuses the whole request, other fields included.
		"mixed": {"agentSandboxImage": "should-not-land", "agentSandboxJobNodeSelector": map[string]any{"bad key": "x"}},
	}
	beforeJob, beforePreflight, beforeImage := deps.Config.AgentSandbox.Job, deps.Config.AgentSandbox.Preflight, deps.Config.AgentSandbox.Image
	for name, body := range cases {
		rec := doPut(s, "/api/config/governor/security", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if g := deps.Config.AgentSandbox; g.Job != beforeJob || g.Preflight != beforePreflight || g.Image != beforeImage {
		t.Fatalf("a refused edit changed agent_sandbox: %#v", g)
	}
}
