package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSandboxJobMetadataAPI(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	deps.Config.AgentSandbox.Job = &config.SandboxJobConfig{ServiceAccount: "keep"}
	global := map[string]any{"agentSandboxJobPodLabels": map[string]any{"example.com/team": "global"}, "agentSandboxJobPodAnnotations": map[string]any{"example.com/note": "hello world"}}
	if rec := doPut(s, "/api/config/governor/security", global); rec.Code != http.StatusOK {
		t.Fatalf("global PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/governor/security", map[string]any{"agentSandboxJobPodLabels": map[string]any{"example.com/team": "updated"}}); rec.Code != http.StatusOK {
		t.Fatalf("partial global PUT = %d: %s", rec.Code, rec.Body.String())
	}
	section := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if section["agentSandboxJobPodLabels"].(map[string]any)["example.com/team"] != "updated" || section["agentSandboxJobPodAnnotations"].(map[string]any)["example.com/note"] != "hello world" || deps.Config.AgentSandbox.Job.ServiceAccount != "keep" {
		t.Fatalf("global metadata or existing fields lost: %#v", section)
	}
	body := map[string]any{"sandboxJob": map[string]any{"podLabels": map[string]any{"example.com/team": "agent"}, "podAnnotations": map[string]any{"example.com/agent": "note with spaces"}}}
	if rec := doPut(s, "/api/config/agent/scanner/general", body); rec.Code != http.StatusOK {
		t.Fatalf("agent PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxJob": map[string]any{"podLabels": map[string]any{"example.com/team": "changed"}}}); rec.Code != http.StatusOK {
		t.Fatalf("partial agent PUT = %d: %s", rec.Code, rec.Body.String())
	}
	fields := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)["sandboxFields"].(map[string]any)
	own := fields["sandboxJob"].(map[string]any)
	if own["podLabels"].(map[string]any)["example.com/team"] != "changed" || own["podAnnotations"].(map[string]any)["example.com/agent"] != "note with spaces" {
		t.Fatalf("agent metadata lost: %#v", own)
	}
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxJob": map[string]any{"podLabels": nil, "podAnnotations": map[string]any{}}}); rec.Code != http.StatusOK {
		t.Fatalf("clear agent PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if job := deps.Config.Agents["scanner"].Sandbox.Job; job != nil && (len(job.PodLabels) != 0 || len(job.PodAnnotations) != 0) {
		t.Fatalf("metadata not cleared: %#v", job)
	}
}

func TestSandboxJobMetadataAPIRejectsInvalidValues(t *testing.T) {
	s, deps := apiServer(t)
	cases := []struct {
		field string
		value any
	}{
		{"podLabels", map[string]any{"job-name": "bad"}},
		{"podLabels", map[string]any{"example.com/team": "bad value"}},
		{"podLabels", map[string]any{"example.com/team": 2}},
		{"podAnnotations", map[string]any{"hive.kubestellar.io/private": "bad"}},
		{"podAnnotations", map[string]any{"bad key": "value"}},
		{"podAnnotations", map[string]any{"example.com/note": strings.Repeat("x", 256*1024)}},
	}
	for _, tc := range cases {
		for _, scope := range []string{"agent", "global"} {
			var path string
			var body map[string]any
			if scope == "agent" {
				path = "/api/config/agent/scanner/general"
				body = map[string]any{"sandboxJob": map[string]any{tc.field: tc.value}}
			} else {
				path = "/api/config/governor/security"
				key := "agentSandboxJobPodLabels"
				if tc.field == "podAnnotations" {
					key = "agentSandboxJobPodAnnotations"
				}
				body = map[string]any{key: tc.value, "agentSandboxImage": "must-not-land"}
			}
			if rec := doPut(s, path, body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s %s: PUT = %d: %s", scope, tc.field, rec.Code, rec.Body.String())
			}
		}
	}
	if deps.Config.Agents["scanner"].Sandbox != nil || deps.Config.AgentSandbox.Image == "must-not-land" {
		t.Fatal("invalid metadata changed configuration")
	}
}

func TestSandboxJobMetadataRejectsInvalidMergedMaps(t *testing.T) {
	s, deps := apiServer(t)
	global := map[string]string{"global": strings.Repeat("x", 128*1024)}
	own := map[string]string{"agent": strings.Repeat("x", 128*1024)}
	deps.Config.AgentSandbox.Job = &config.SandboxJobConfig{PodAnnotations: global}
	body := map[string]any{"sandboxJob": map[string]any{"podAnnotations": own}}
	if rec := doPut(s, "/api/config/agent/scanner/general", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("merged agent metadata PUT = %d: %s", rec.Code, rec.Body.String())
	}
	agent := deps.Config.Agents["scanner"]
	agent.Sandbox = &config.AgentSandboxOverride{Job: &config.SandboxJobConfig{PodAnnotations: own}}
	deps.Config.Agents["scanner"] = agent
	deps.Config.AgentSandbox.Job.PodAnnotations = nil
	if rec := doPut(s, "/api/config/governor/security", map[string]any{"agentSandboxJobPodAnnotations": global}); rec.Code != http.StatusBadRequest {
		t.Fatalf("merged global metadata PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if len(deps.Config.AgentSandbox.Job.PodAnnotations) != 0 {
		t.Fatal("invalid global metadata was applied")
	}
}
