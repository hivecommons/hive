package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// The runs.* keys round-trip through the Features PUT/GET; invalid values are
// rejected without changing anything, an absent key leaves its value
// untouched, and the vibe-kanban mcp_command is never written.
func TestGovernorFeaturesRunsSettingsRoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Runs.External.VibeKanban.MCPCommand = "npx vibe-kanban --mcp"
	before := s.deps.Config.Runs

	for _, bad := range []map[string]any{
		{"runsWaitTimeoutSeconds": -1},
		{"runsWaitSeverity": "urgent"},
		{"runsTriageMinBodyChars": -1},
		{"spektacularRecheckDefaultInterval": "weekly"},
		{"spektacularRecheckMaxDeltaTasks": -1},
		{"spektacularRecheckDiscoveryProxy": "http://user:pw@relay:18443"},
		{"spektacularRecheckDiscoveryProxy": "socks5://relay:1080"},
		{"spektacularDiscoveryTimeout": "-1s"},
		{"spektacularDiscoveryMaxTotalItems": -1},
		{"extFlueEndpoint": "ftp://flue.example"},
		{"extFlueEndpoint": "http://user:secret@127.0.0.1:8585"},
		{"extFlueWorkflowVersion": "has space"},
		{"extOmpWorkflowVersion": strings.Repeat("v", maxRunsSettingLen+1)},
		{"extVibeKanbanMode": "publish"},
		{"extVibeKanbanStateDir": "relative/state"},
		{"runsTriageEnabled": true, "runsWaitSeverity": "nope"},
	} {
		if rec := doPut(s, opsFeaturesPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.Runs.Triage.Enabled != before.Triage.Enabled || s.deps.Config.Runs.WaitSeverity != before.WaitSeverity {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"runsCheckpointSpec":      false,
		"runsCheckpointPlan":      true,
		"runsCheckpointImplement": false,
		"runsWaitTimeoutSeconds":  7200,
		"runsWaitSeverity":        " Info ",

		"runsTriageEnabled":        true,
		"runsTriageMinBodyChars":   120,
		"runsTriageClarifyComment": false,

		"spektacularRecheckEnabled":         true,
		"spektacularRecheckDefaultInterval": "72h",
		"spektacularRecheckMaxDeltaTasks":   25,
		"spektacularRecheckDiscoveryProxy":  "http://relay:18443",
		"spektacularDiscoveryEnabled":       true,
		"spektacularDiscoveryTimeout":       "45s",
		"spektacularDiscoveryMaxTotalItems": 40,

		"extFlueEndpoint":        "http://127.0.0.1:8585/",
		"extFlueWorkflowVersion": "flue-fixture/1.0.0",
		"extOmpWorkflowVersion":  "omp/2",

		"extVibeKanbanEnabled":         true,
		"extVibeKanbanMode":            config.FlueBindingModeReportOnly,
		"extVibeKanbanProjectId":       "proj-123",
		"extVibeKanbanStateDir":        "/data/vk",
		"extVibeKanbanWorkflowVersion": "vk/1",
		"extVibeKanbanMcpCommand":      "rm -rf /",
	}); rec.Code != http.StatusOK {
		t.Fatalf("runs PUT: %d — %s", rec.Code, rec.Body.String())
	}
	r := s.deps.Config.Runs
	if r.CheckpointBlocks("spec") || !r.CheckpointBlocks("plan") || r.Checkpoints.Implement == nil || *r.Checkpoints.Implement {
		t.Fatalf("checkpoints not applied: %+v", r.Checkpoints)
	}
	if r.WaitTimeoutSeconds != 7200 || r.WaitSeverity != "info" {
		t.Fatalf("wait settings not applied: %d %q", r.WaitTimeoutSeconds, r.WaitSeverity)
	}
	if !r.Triage.Enabled || r.Triage.MinBodyChars != 120 || r.Triage.ShouldClarifyComment() {
		t.Fatalf("triage not applied: %+v", r.Triage)
	}
	rc := r.Spektacular.Recheck
	if !rc.Enabled || rc.DefaultInterval != 72*time.Hour || rc.MaxDeltaTasks != 25 || rc.DiscoveryProxy != "http://relay:18443" ||
		!rc.Discovery.Enabled || rc.Discovery.Timeout != 45*time.Second || rc.Discovery.MaxTotalItems != 40 {
		t.Fatalf("recheck not applied: %+v", rc)
	}
	if r.External.Flue.Endpoint != "http://127.0.0.1:8585" || r.External.Flue.WorkflowVersion != "flue-fixture/1.0.0" || r.External.OMP.WorkflowVersion != "omp/2" {
		t.Fatalf("flue/omp not applied: %+v", r.External)
	}
	vk := r.External.VibeKanban
	if vk != (config.VibeKanbanBindingConfig{Enabled: true, Mode: config.FlueBindingModeReportOnly, MCPCommand: "npx vibe-kanban --mcp", ProjectID: "proj-123", StateDir: "/data/vk", WorkflowVersion: "vk/1"}) {
		t.Fatalf("vibe-kanban not applied (or mcp_command written): %+v", vk)
	}

	// A partial PUT changes only what it carries; empty values reset to the
	// default.
	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"runsWaitSeverity":                  "",
		"spektacularRecheckDefaultInterval": "",
		"spektacularRecheckMaxDeltaTasks":   0,
		"extFlueEndpoint":                   "",
		"extVibeKanbanStateDir":             "",
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	r = s.deps.Config.Runs
	if r.WaitSeverity != "" || r.WaitTimeoutSeconds != 7200 || r.Spektacular.Recheck.DefaultInterval != 0 ||
		r.Spektacular.Recheck.MaxDeltaTasks != 0 || r.Spektacular.Recheck.Discovery.MaxTotalItems != 40 ||
		r.External.Flue.Endpoint != "" || r.External.Flue.WorkflowVersion != "flue-fixture/1.0.0" ||
		r.External.VibeKanban.StateDir != "" || r.External.VibeKanban.ProjectID != "proj-123" {
		t.Fatalf("partial PUT changed unrelated keys or did not reset: %+v", r)
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var payload struct {
		Features struct {
			CheckpointSpec      bool                   `json:"runsCheckpointSpec"`
			CheckpointImplement bool                   `json:"runsCheckpointImplement"`
			WaitTimeoutSeconds  int                    `json:"runsWaitTimeoutSeconds"`
			TriageMinBodyChars  int                    `json:"runsTriageMinBodyChars"`
			TriageClarify       bool                   `json:"runsTriageClarifyComment"`
			RecheckInterval     string                 `json:"spektacularRecheckDefaultInterval"`
			DiscoveryTimeout    string                 `json:"spektacularDiscoveryTimeout"`
			DiscoveryProxy      string                 `json:"spektacularRecheckDiscoveryProxy"`
			FlueWorkflowVersion string                 `json:"extFlueWorkflowVersion"`
			VKEnabled           bool                   `json:"extVibeKanbanEnabled"`
			VKMode              string                 `json:"extVibeKanbanMode"`
			VKModeSetting       string                 `json:"extVibeKanbanModeSetting"`
			VKMcpCommand        string                 `json:"extVibeKanbanMcpCommand"`
			VKLinked            *bool                  `json:"extVibeKanbanLinked"`
			Defaults            map[string]interface{} `json:"runsDefaults"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	f := payload.Features
	if f.CheckpointSpec || f.CheckpointImplement || f.WaitTimeoutSeconds != 7200 || f.TriageMinBodyChars != 120 || f.TriageClarify {
		t.Fatalf("runs payload = %+v", f)
	}
	if f.RecheckInterval != "" || f.DiscoveryTimeout != "45s" || f.DiscoveryProxy != "http://relay:18443" || f.FlueWorkflowVersion != "flue-fixture/1.0.0" {
		t.Fatalf("recheck/flue payload = %+v", f)
	}
	if !f.VKEnabled || f.VKMode != config.FlueBindingModeReportOnly || f.VKModeSetting != config.FlueBindingModeReportOnly ||
		f.VKMcpCommand != "npx vibe-kanban --mcp" || f.VKLinked == nil {
		t.Fatalf("vibe-kanban payload = %+v", f)
	}
	if f.Defaults["runsWaitSeverity"] != config.DefaultRunsWaitSeverity ||
		f.Defaults["spektacularRecheckDefaultInterval"] != config.DefaultSpektacularRecheckInterval.String() {
		t.Fatalf("runsDefaults = %v", f.Defaults)
	}
}

// With exact recheck sources configured the discovery proxy is mandatory, so
// the dialog cannot clear it.
func TestGovernorFeaturesRunsDiscoveryProxyRequiredWithSources(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Runs.Spektacular.Recheck.DiscoveryProxy = "http://relay:18443"
	s.deps.Config.Runs.Spektacular.Recheck.EgressAllowlist = []string{"api.github.com"}
	s.deps.Config.Runs.Spektacular.Recheck.Sources = []config.SpektacularDiscoverySource{{Name: "release", Kind: "release", URL: "https://api.github.com/repos/example/x/releases/latest"}}
	if rec := doPut(s, opsFeaturesPath, map[string]any{"spektacularRecheckDiscoveryProxy": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("clearing the proxy with sources configured: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Runs.Spektacular.Recheck.DiscoveryProxy != "http://relay:18443" {
		t.Fatal("rejected PUT cleared the proxy")
	}
}

// Every newly exposed runs.* key is rendered on the Features tab with its
// YAML-key hint and a control bound to the Features dirty map.
func TestRunsSettingsRenderEveryKey(t *testing.T) {
	body := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	for _, c := range []struct{ yaml, control string }{
		{"runs.checkpoints.spec", `data-section="features" data-key="runsCheckpointSpec"`},
		{"runs.checkpoints.plan", `data-section="features" data-key="runsCheckpointPlan"`},
		{"runs.checkpoints.implement", `data-section="features" data-key="runsCheckpointImplement"`},
		{"runs.wait_timeout_seconds", `data-arg0="features" data-arg1="runsWaitTimeoutSeconds" data-arg-types="s,s,N"`},
		{"runs.wait_severity", `data-arg0="features" data-arg1="runsWaitSeverity" data-arg-types="s,s,v"`},
		{"runs.triage.enabled", `data-section="features" data-key="runsTriageEnabled"`},
		{"runs.triage.clarify_comment", `data-section="features" data-key="runsTriageClarifyComment"`},
		{"runs.triage.min_body_chars", `data-arg0="features" data-arg1="runsTriageMinBodyChars" data-arg-types="s,s,N"`},
		{"runs.spektacular.binary", `data-arg0="features" data-arg1="spektacularBinary" data-arg-types="s,s,w"`},
		{"runs.spektacular.recheck.enabled", `data-section="features" data-key="spektacularRecheckEnabled"`},
		{"runs.spektacular.recheck.default_interval", `data-arg0="features" data-arg1="spektacularRecheckDefaultInterval" data-arg-types="s,s,w"`},
		{"runs.spektacular.recheck.max_delta_tasks", `data-arg0="features" data-arg1="spektacularRecheckMaxDeltaTasks" data-arg-types="s,s,N"`},
		{"runs.spektacular.recheck.discovery_proxy", `data-arg0="features" data-arg1="spektacularRecheckDiscoveryProxy" data-arg-types="s,s,w"`},
		{"runs.spektacular.recheck.discovery.enabled", `data-section="features" data-key="spektacularDiscoveryEnabled"`},
		{"runs.spektacular.recheck.discovery.timeout", `data-arg0="features" data-arg1="spektacularDiscoveryTimeout" data-arg-types="s,s,w"`},
		{"runs.spektacular.recheck.discovery.max_total_items", `data-arg0="features" data-arg1="spektacularDiscoveryMaxTotalItems" data-arg-types="s,s,N"`},
		{"runs.external.flue.mode", `data-arg0="features" data-arg1="extFlueMode" data-arg-types="s,s,v"`},
		{"runs.external.flue.endpoint", `data-arg0="features" data-arg1="extFlueEndpoint" data-arg-types="s,s,w"`},
		{"runs.external.flue.workflow_version", `data-arg0="features" data-arg1="extFlueWorkflowVersion" data-arg-types="s,s,w"`},
		{"runs.external.omp.mode", `data-arg0="features" data-arg1="extOmpMode" data-arg-types="s,s,v"`},
		{"runs.external.omp.workflow_version", `data-arg0="features" data-arg1="extOmpWorkflowVersion" data-arg-types="s,s,w"`},
		{"runs.external.vibe_kanban.enabled", `data-section="features" data-key="extVibeKanbanEnabled"`},
		{"runs.external.vibe_kanban.mode", `data-arg0="features" data-arg1="extVibeKanbanMode" data-arg-types="s,s,v"`},
		{"runs.external.vibe_kanban.project_id", `data-arg0="features" data-arg1="extVibeKanbanProjectId" data-arg-types="s,s,w"`},
		{"runs.external.vibe_kanban.state_dir", `data-arg0="features" data-arg1="extVibeKanbanStateDir" data-arg-types="s,s,w"`},
		{"runs.external.vibe_kanban.workflow_version", `data-arg0="features" data-arg1="extVibeKanbanWorkflowVersion" data-arg-types="s,s,w"`},
		{"runs.external.vibe_kanban.mcp_command", `value="${esc(f.extVibeKanbanMcpCommand || '')}" placeholder="not set" readonly`},
	} {
		if !strings.Contains(body, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if !strings.Contains(body, c.control) {
			t.Errorf("%s has no Features control (%s)", c.yaml, c.control)
		}
	}
	if strings.Contains(body, "Endpoint and workflow version are yaml-only") {
		t.Error("runs help text still claims exposed keys are yaml-only")
	}
}
