package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19553: sandbox Job scheduling, resources, preflight and the
// read relay are editable from the dashboard, and validated by the API.

func TestSecuritySandboxSchedulingFieldsStored(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxJobNodeSelector":     map[string]string{"accelerator": "spyre", " kubernetes.io/arch ": "amd64"},
		"agentSandboxJobResourceLimits":   map[string]string{"ibm.com/spyre_pf": "2", "memory": "16Gi"},
		"agentSandboxJobResourceRequests": map[string]string{"cpu": "500m"},
		"agentSandboxJobTolerations": []map[string]string{
			{"key": "ibm.com/spyre", "operator": "Exists", "effect": "NoSchedule"},
			{"key": "dedicated", "operator": "Equal", "value": "ci"},
			{},
		},
		"agentSandboxJobImagePullSecrets": []string{"quay-pull", " "},
		"agentSandboxJobScheduleTimeoutS": 900,
		"agentSandboxPreflight":           map[string]any{"commands": []string{"spyre-smi", " "}, "timeoutS": 60, "totalTimeoutS": 120, "retryDelayS": 5},
		"agentSandboxJobGithubReadProxy":  map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "https://hive-relay.hive.svc:8443", "noProxy": []string{"internal.example.com"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d: %s", rec.Code, rec.Body.String())
	}
	sb := deps.Config.AgentSandbox
	job := sb.Job
	if job == nil || job.NodeSelector["accelerator"] != "spyre" || job.NodeSelector["kubernetes.io/arch"] != "amd64" ||
		job.Resources.Limits["ibm.com/spyre_pf"] != "2" || job.Resources.Requests["cpu"] != "500m" ||
		len(job.Tolerations) != 2 || job.Tolerations[0].Operator != "Exists" || job.Tolerations[1].Value != "ci" ||
		strings.Join(job.ImagePullSecrets, ",") != "quay-pull" || job.ScheduleTimeoutS != 900 {
		t.Fatalf("job fields not stored: %#v", job)
	}
	if sb.Preflight == nil || strings.Join(sb.Preflight.Commands, ",") != "spyre-smi" || sb.Preflight.TimeoutS != 60 ||
		sb.Preflight.TotalTimeoutS != 120 || sb.Preflight.RetryDelayS != 5 {
		t.Fatalf("preflight not stored: %#v", sb.Preflight)
	}
	if rp := job.GitHubReadProxy; rp == nil || !rp.Enabled || rp.ListenAddr != ":8443" || rp.Endpoint != "https://hive-relay.hive.svc:8443" {
		t.Fatalf("read relay not stored: %#v", job.GitHubReadProxy)
	}

	sec, _ := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if ns, _ := sec["agentSandboxJobNodeSelector"].(map[string]any); ns["accelerator"] != "spyre" {
		t.Fatalf("GET node selector = %v", sec["agentSandboxJobNodeSelector"])
	}
	if rp, _ := sec["agentSandboxJobGithubReadProxy"].(map[string]any); rp["listenAddr"] != ":8443" {
		t.Fatalf("GET read relay = %v", sec["agentSandboxJobGithubReadProxy"])
	}

	// Empty values clear; the preflight block is dropped when empty.
	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxJobNodeSelector": map[string]string{},
		"agentSandboxJobTolerations":  []any{},
		"agentSandboxPreflight":       map[string]any{"commands": []string{}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.AgentSandbox.Job.NodeSelector != nil || deps.Config.AgentSandbox.Job.Tolerations != nil || deps.Config.AgentSandbox.Preflight != nil {
		t.Fatalf("not cleared: job=%#v preflight=%#v", deps.Config.AgentSandbox.Job, deps.Config.AgentSandbox.Preflight)
	}
	// Absent keys are left alone.
	if deps.Config.AgentSandbox.Job.Resources.Limits["memory"] != "16Gi" {
		t.Fatalf("an absent key was changed: %#v", deps.Config.AgentSandbox.Job.Resources)
	}
}

func TestSecuritySandboxSchedulingValidation(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"label key":          {"agentSandboxJobNodeSelector": map[string]string{"bad key!": "x"}},
		"label value":        {"agentSandboxJobNodeSelector": map[string]string{"accelerator": "has space"}},
		"label prefix":       {"agentSandboxJobNodeSelector": map[string]string{"UPPER.example/x": "y"}},
		"resource name":      {"agentSandboxJobResourceLimits": map[string]string{"a/b/c": "1"}},
		"quantity":           {"agentSandboxJobResourceLimits": map[string]string{"memory": "lots"}},
		"negative quantity":  {"agentSandboxJobResourceRequests": map[string]string{"cpu": "-1"}},
		"empty quantity":     {"agentSandboxJobResourceRequests": map[string]string{"cpu": ""}},
		"toleration op":      {"agentSandboxJobTolerations": []map[string]string{{"key": "a", "operator": "In"}}},
		"toleration effect":  {"agentSandboxJobTolerations": []map[string]string{{"key": "a", "effect": "NoRun"}}},
		"exists with value":  {"agentSandboxJobTolerations": []map[string]string{{"key": "a", "operator": "Exists", "value": "b"}}},
		"empty key equal":    {"agentSandboxJobTolerations": []map[string]string{{"operator": "Equal", "value": "b"}}},
		"pull secret value":  {"agentSandboxJobImagePullSecrets": []string{"TOKEN=abc"}},
		"schedule timeout":   {"agentSandboxJobScheduleTimeoutS": -1},
		"preflight negative": {"agentSandboxPreflight": map[string]any{"commands": []string{"true"}, "timeoutS": -5}},
		"preflight too many": {"agentSandboxPreflight": map[string]any{"commands": strings.Split(strings.Repeat("x,", 40), ",")}},
		"relay listen":       {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "listenAddr": "8443", "endpoint": "https://relay:8443"}},
		"relay no listen":    {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "endpoint": "https://relay:8443"}},
		"relay http":         {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "http://relay:8443"}},
		"relay creds":        {"agentSandboxJobGithubReadProxy": map[string]any{"enabled": true, "listenAddr": ":8443", "endpoint": "https://u:p@relay:8443"}},
		"relay off, http":    {"agentSandboxJobGithubReadProxy": map[string]any{"endpoint": "http://relay:8443"}},
		"relay no_proxy":     {"agentSandboxJobGithubReadProxy": map[string]any{"noProxy": []string{"a b"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s, deps := apiServer(t)
			before := deps.Config.AgentSandbox
			rec := doPut(s, "/api/config/governor/security", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if deps.Config.AgentSandbox.Job != before.Job || deps.Config.AgentSandbox.Preflight != before.Preflight {
				t.Fatalf("a refused save changed agent_sandbox: %#v", deps.Config.AgentSandbox)
			}
		})
	}
}

func TestAgentSandboxSchedulingFieldsEditable(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.AgentSandbox.Job = &config.SandboxJobConfig{
		NodeSelector:     map[string]string{"pool": "shared"},
		ScheduleTimeoutS: 300,
		ImagePullSecrets: []string{"hive-pull"},
	}
	deps.Config.AgentSandbox.Preflight = &config.SandboxPreflightConfig{Commands: []string{"nvidia-smi"}}

	// No own job block: everything comes from agent_sandbox.
	fields, _ := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)["sandboxFields"].(map[string]any)
	srcs, _ := fields["sandboxJobSources"].(map[string]any)
	if srcs["nodeSelector"] != "agent_sandbox" || fields["sandboxPreflightSource"] != "agent_sandbox" {
		t.Fatalf("sources before edit = %v, preflight %v", srcs, fields["sandboxPreflightSource"])
	}
	if hidden, _ := fields["sandboxJobHiddenGlobal"].([]any); len(hidden) != 0 {
		t.Fatalf("hidden before edit = %v", hidden)
	}

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxJob": map[string]any{
			"resourceLimits":   map[string]any{"ibm.com/spyre_pf": "1"},
			"tolerations":      []any{map[string]any{"key": "ibm.com/spyre", "operator": "Exists"}},
			"scheduleTimeoutS": 1200,
		},
		"sandboxPreflight": map[string]any{"commands": []any{}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	sb := deps.Config.Agents["scanner"].Sandbox
	if sb == nil || sb.Job == nil || sb.Job.Resources.Limits["ibm.com/spyre_pf"] != "1" || len(sb.Job.Tolerations) != 1 || sb.Job.ScheduleTimeoutS != 1200 {
		t.Fatalf("job not stored: %#v", sb)
	}
	if sb.Preflight == nil || len(sb.Preflight.Commands) != 0 {
		t.Fatalf("empty preflight override must be kept (turns preflight off): %#v", sb.Preflight)
	}

	fields, _ = decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)["sandboxFields"].(map[string]any)
	srcs, _ = fields["sandboxJobSources"].(map[string]any)
	if srcs["nodeSelector"] != "agent" || srcs["scheduleTimeoutS"] != "agent" || srcs["imagePullSecrets"] != "agent_sandbox" {
		t.Fatalf("sources after edit = %v", srcs)
	}
	hidden, _ := fields["sandboxJobHiddenGlobal"].([]any)
	if len(hidden) != 1 || hidden[0] != "node_selector" {
		t.Fatalf("own job block must report the hidden hive-wide node selector, got %v", hidden)
	}
	if fields["sandboxPreflightSource"] != "agent" {
		t.Fatalf("preflight source = %v", fields["sandboxPreflightSource"])
	}

	// null clears the preflight override back to inherit.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxPreflight": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.Agents["scanner"].Sandbox.Preflight != nil {
		t.Fatal("null did not clear the preflight override")
	}
}

func TestAgentSandboxSchedulingValidation(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"node selector key": {"sandboxJob": map[string]any{"nodeSelector": map[string]any{"-bad": "x"}}},
		"node selector val": {"sandboxJob": map[string]any{"nodeSelector": map[string]any{"a": 3}}},
		"quantity":          {"sandboxJob": map[string]any{"resourceRequests": map[string]any{"cpu": "1 core"}}},
		"toleration shape":  {"sandboxJob": map[string]any{"tolerations": "x"}},
		"toleration effect": {"sandboxJob": map[string]any{"tolerations": []any{map[string]any{"key": "a", "effect": "Never"}}}},
		"schedule timeout":  {"sandboxJob": map[string]any{"scheduleTimeoutS": -1}},
		"pull secret value": {"sandboxJob": map[string]any{"imagePullSecrets": []any{"user:pass"}}},
		"preflight shape":   {"sandboxPreflight": "true"},
		"preflight bound":   {"sandboxPreflight": map[string]any{"commands": []any{"true"}, "retryDelayS": -1}},
	} {
		t.Run(name, func(t *testing.T) {
			s, deps := apiServer(t)
			rec := doPut(s, "/api/config/agent/scanner/general", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if deps.Config.Agents["scanner"].Sandbox != nil {
				t.Fatalf("a refused edit wrote a block: %#v", deps.Config.Agents["scanner"].Sandbox)
			}
		})
	}
}

func TestSandboxQualifiedNamesAndQuantities(t *testing.T) {
	for _, ok := range []string{"cpu", "memory", "ibm.com/spyre_pf", "nvidia.com/gpu", "hugepages-2Mi", "kubernetes.io/hostname"} {
		if !qualifiedNameOK(ok) {
			t.Errorf("qualifiedNameOK(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "/x", "a/", "a/b/c", "-a", "a b", strings.Repeat("a", 64)} {
		if qualifiedNameOK(bad) {
			t.Errorf("qualifiedNameOK(%q) = true", bad)
		}
	}
	for _, ok := range []string{"1", "500m", "4Gi", "0.5", "1e3", "2k", "+1"} {
		if !resourceQuantityRE.MatchString(ok) {
			t.Errorf("quantity %q refused", ok)
		}
	}
	for _, bad := range []string{"-1", "1GB", "Gi", "1.2.3", "1 Gi"} {
		if resourceQuantityRE.MatchString(bad) {
			t.Errorf("quantity %q accepted", bad)
		}
	}
}
