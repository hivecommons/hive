package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const knowledgePipelinePath = "/api/config/knowledge/pipeline"

// The scalar knowledge pipeline keys round-trip through
// PUT /api/config/knowledge/pipeline; invalid values are rejected without
// changing anything, a partial PUT changes only what it carries, a cleared
// field restores the default, and the display-only keys are not accepted.
func TestKnowledgePipelineParityBatch8RoundTrip(t *testing.T) {
	s := covApiServer(t)
	k := &s.deps.Config.Knowledge
	k.Engine = "llm-wiki"
	k.BeadSynthesizer = config.BeadSynthesizerConfig{
		Schedule: "hourly", MinConfidence: 0.5, TargetLayer: "project",
		MaxFactsPerCycle: 20, MaxAPICallsPerCycle: 40, VaultPath: "/data/vaults/bead-synth-wiki",
	}
	k.Curator = config.KnowledgeCurator{AutoPromoteThreshold: 0.9}
	k.Primer = config.KnowledgePrimer{MaxFacts: 25, MaxBytes: 4096, MergeStrategy: "precedence"}
	before := *k

	for _, bad := range []map[string]any{
		{"bead_synthesizer": map[string]any{"schedule": "weekly"}},
		{"bead_synthesizer": map[string]any{"min_confidence": 1.5}},
		{"bead_synthesizer": map[string]any{"min_confidence": -0.1}},
		{"bead_synthesizer": map[string]any{"target_layer": "galaxy"}},
		{"bead_synthesizer": map[string]any{"max_facts_per_cycle": -1}},
		{"bead_synthesizer": map[string]any{"max_api_calls_per_cycle": -1}},
		{"bead_synthesizer": map[string]any{"retention_policy": map[string]any{"max_beads": -1}}},
		{"bead_synthesizer": map[string]any{"retention_policy": map[string]any{"archive_after_synth_days": -1}}},
		{"bead_synthesizer": map[string]any{"retention_policy": map[string]any{"high_priority_retain_days": -1}}},
		{"curator": map[string]any{"schedule": "monthly"}},
		{"curator": map[string]any{"auto_promote_threshold": 2}},
		{"curator": map[string]any{"promote_from": "org"}},
		{"curator": map[string]any{"promote_from": "community", "promote_to": "community"}},
		{"curator": map[string]any{"promote_to": "nowhere"}},
		{"code_maps": map[string]any{"schedule": "often"}},
		{"code_maps": map[string]any{"layer": "galaxy"}},
		{"code_maps": map[string]any{"max_bytes": -1}},
		{"primer": map[string]any{"max_facts": -1}},
		{"primer": map[string]any{"max_bytes": -1}},
		{"curator": map[string]any{"enabled": true}, "primer": map[string]any{"max_facts": -3}},
	} {
		if rec := doPut(s, knowledgePipelinePath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if k.Curator.IsEnabled() || k.BeadSynthesizer.Schedule != before.BeadSynthesizer.Schedule ||
		k.BeadSynthesizer.RetentionPolicy != nil || k.Primer.MaxFacts != before.Primer.MaxFacts {
		t.Fatal("a rejected PUT changed the config")
	}

	rec := doPut(s, knowledgePipelinePath, map[string]any{
		"bead_synthesizer": map[string]any{
			"enabled":                 false,
			"schedule":                " Daily ",
			"min_confidence":          0.7,
			"target_layer":            "org",
			"max_facts_per_cycle":     5,
			"max_api_calls_per_cycle": 10,
			"retention_policy":        map[string]any{"max_beads": 1000, "archive_after_synth_days": 3},
		},
		"curator": map[string]any{
			"enabled":                true,
			"schedule":               "hourly",
			"auto_promote_threshold": 0.8,
			"promote_from":           "personal",
			"promote_to":             "project",
		},
		"code_maps": map[string]any{"enabled": true, "schedule": "daily", "layer": "org", "max_bytes": 8192},
		"primer":    map[string]any{"max_facts": 10, "max_bytes": 2048},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("pipeline PUT: %d — %s", rec.Code, rec.Body.String())
	}
	bs := k.BeadSynthesizer
	if bs.IsEnabled() || bs.Schedule != "daily" || bs.MinConfidence != 0.7 || bs.TargetLayer != "org" ||
		bs.MaxFactsPerCycle != 5 || bs.MaxAPICallsPerCycle != 10 {
		t.Fatalf("bead synthesizer settings not applied: %+v", bs)
	}
	// A retention block created here keeps the boot default of preserving
	// beads with open dependents.
	if rp := bs.RetentionPolicy; rp == nil || rp.MaxBeads != 1000 || rp.ArchiveAfterSynthDays != 3 || !rp.PreserveWithDeps {
		t.Fatalf("retention policy not applied: %+v", bs.RetentionPolicy)
	}
	if c := k.Curator; !c.IsEnabled() || c.Schedule != "hourly" || c.AutoPromoteThreshold != 0.8 ||
		c.PromoteFrom != "personal" || c.PromoteTo != "project" {
		t.Fatalf("curator settings not applied: %+v", c)
	}
	if cm := k.CodeMaps; !cm.IsEnabled() || cm.Schedule != "daily" || cm.Layer != "org" || cm.MaxBytes != 8192 {
		t.Fatalf("code map settings not applied: %+v", cm)
	}
	if k.Primer.MaxFacts != 10 || k.Primer.MaxBytes != 2048 {
		t.Fatalf("primer settings not applied: %+v", k.Primer)
	}

	var resp struct {
		Engine          string         `json:"engine"`
		BeadSynthesizer map[string]any `json:"bead_synthesizer"`
		Curator         map[string]any `json:"curator"`
		CodeMaps        map[string]any `json:"code_maps"`
		Primer          map[string]any `json:"primer"`
		Defaults        map[string]any `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.BeadSynthesizer["schedule"] != "daily" || resp.BeadSynthesizer["enabled"] != false ||
		resp.Curator["promote_to"] != "project" || resp.CodeMaps["max_bytes"] != float64(8192) ||
		resp.Primer["max_facts"] != float64(10) {
		t.Fatalf("response does not reflect the saved settings: %s", rec.Body.String())
	}
	rp, _ := resp.BeadSynthesizer["retention_policy"].(map[string]any)
	if rp["max_beads"] != float64(1000) || rp["preserve_with_deps"] != true {
		t.Fatalf("response retention_policy = %v", rp)
	}

	// A partial PUT changes only what it carries; changing one promotion
	// layer is checked against the other one already saved.
	if rec := doPut(s, knowledgePipelinePath, map[string]any{"curator": map[string]any{"promote_from": "project"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("promote_from equal to the saved promote_to: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, knowledgePipelinePath, map[string]any{
		"bead_synthesizer": map[string]any{"retention_policy": map[string]any{"preserve_with_deps": false}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if rp := k.BeadSynthesizer.RetentionPolicy; rp.PreserveWithDeps || rp.MaxBeads != 1000 ||
		k.BeadSynthesizer.Schedule != "daily" || k.Curator.PromoteFrom != "personal" {
		t.Fatalf("partial PUT changed unrelated keys: %+v / %+v", *rp, k.Curator)
	}

	// Clearing restores the defaults applyDefaults would set at Load.
	if rec := doPut(s, knowledgePipelinePath, map[string]any{
		"bead_synthesizer": map[string]any{
			"schedule": "", "min_confidence": 0, "target_layer": "",
			"max_facts_per_cycle": 0, "max_api_calls_per_cycle": 0,
		},
		"curator":   map[string]any{"auto_promote_threshold": 0, "promote_from": "", "promote_to": ""},
		"code_maps": map[string]any{"schedule": "", "layer": "", "max_bytes": 0},
		"primer":    map[string]any{"max_facts": 0, "max_bytes": 0},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clearing pipeline settings: %d — %s", rec.Code, rec.Body.String())
	}
	bs = k.BeadSynthesizer
	if bs.Schedule != config.DefaultBeadSynthSchedule || bs.MinConfidence != config.DefaultBeadSynthMinConfidence ||
		bs.TargetLayer != config.DefaultBeadSynthTargetLayer || bs.MaxFactsPerCycle != config.DefaultBeadSynthMaxFactsPerCycle ||
		bs.MaxAPICallsPerCycle != config.DefaultBeadSynthMaxAPICallsPerCycle {
		t.Fatalf("cleared bead synthesizer settings did not restore defaults: %+v", bs)
	}
	if k.Curator.AutoPromoteThreshold != config.DefaultCuratorAutoPromoteThreshold || k.Curator.PromoteFrom != "" || k.Curator.PromoteTo != "" {
		t.Fatalf("cleared curator settings did not restore defaults: %+v", k.Curator)
	}
	if k.CodeMaps.Schedule != "" || k.CodeMaps.Layer != "" || k.CodeMaps.MaxBytes != 0 {
		t.Fatalf("cleared code map settings were not reset: %+v", k.CodeMaps)
	}
	if k.Primer.MaxFacts != config.DefaultKnowledgePrimerMaxFacts || k.Primer.MaxBytes != config.DefaultKnowledgePrimerMaxBytes {
		t.Fatalf("cleared primer settings did not restore defaults: %+v", k.Primer)
	}

	// The engine, vault paths and merge strategy are display-only: the GET
	// reports them and the PUT ignores them.
	if rec := doPut(s, knowledgePipelinePath, map[string]any{
		"engine":           "file",
		"bead_synthesizer": map[string]any{"vault_path": "/etc"},
		"code_maps":        map[string]any{"vault_path": "/etc"},
		"primer":           map[string]any{"merge_strategy": "union"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("display-only PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if k.Engine != "llm-wiki" || k.BeadSynthesizer.VaultPath != "/data/vaults/bead-synth-wiki" || k.CodeMaps.VaultPath != "" ||
		k.Primer.MergeStrategy != "precedence" {
		t.Fatalf("pipeline PUT wrote a display-only key: %+v", *k)
	}
	rec = doOwnerGet(s, knowledgePipelinePath)
	if rec.Code != http.StatusOK {
		t.Fatalf("pipeline GET: %d", rec.Code)
	}
	resp.Engine, resp.Defaults = "", nil
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Engine != "llm-wiki" || resp.BeadSynthesizer["vault_path"] != "/data/vaults/bead-synth-wiki" ||
		resp.Primer["merge_strategy"] != "precedence" || resp.Defaults["primer_max_bytes"] != float64(4096) {
		t.Fatalf("GET response: %s", rec.Body.String())
	}
}

// A value already in hive.yaml that the editor would reject does not block
// saving an unrelated field.
func TestKnowledgePipelineParityBatch8ValidatesOnlyCarriedKeys(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Knowledge.Curator.Schedule = "weekly"
	if rec := doPut(s, knowledgePipelinePath, map[string]any{"primer": map[string]any{"max_facts": 12}}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Knowledge.Primer.MaxFacts != 12 || s.deps.Config.Knowledge.Curator.Schedule != "weekly" {
		t.Fatalf("knowledge = %+v", s.deps.Config.Knowledge)
	}
}

// Only a verified owner may read or change the pipeline settings.
func TestKnowledgePipelineParityBatch8OwnerOnly(t *testing.T) {
	s := covApiServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, knowledgePipelinePath, strings.NewReader(`{"primer":{"max_facts":3}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-Role", "read-write")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s as read-write: status = %d, want 403", method, rec.Code)
		}
	}
	if s.deps.Config.Knowledge.Primer.MaxFacts == 3 {
		t.Fatal("a non-owner PUT changed the config")
	}
}

// Every knowledge key this batch exposes renders with its YAML-key hint next
// to a control the Save button reads, or read-only with a restart note.
func TestParityBatch8SettingsRenderEveryKey(t *testing.T) {
	html := indexHTML(t)

	body := extractJSFunction(t, html, "renderKnowledgePipelineBody")
	for _, c := range []struct{ yaml, id string }{
		{"knowledge.bead_synthesizer.enabled", "bs-enabled"},
		{"knowledge.bead_synthesizer.schedule", "bs-schedule"},
		{"knowledge.bead_synthesizer.target_layer", "bs-target_layer"},
		{"knowledge.bead_synthesizer.min_confidence", "bs-min_confidence"},
		{"knowledge.bead_synthesizer.max_facts_per_cycle", "bs-max_facts_per_cycle"},
		{"knowledge.bead_synthesizer.max_api_calls_per_cycle", "bs-max_api_calls_per_cycle"},
		{"knowledge.bead_synthesizer.retention_policy.max_beads", "rp-max_beads"},
		{"knowledge.bead_synthesizer.retention_policy.archive_after_synth_days", "rp-archive_after_synth_days"},
		{"knowledge.bead_synthesizer.retention_policy.high_priority_retain_days", "rp-high_priority_retain_days"},
		{"knowledge.bead_synthesizer.retention_policy.preserve_with_deps", "rp-preserve_with_deps"},
		{"knowledge.curator.enabled", "cu-enabled"},
		{"knowledge.curator.schedule", "cu-schedule"},
		{"knowledge.curator.auto_promote_threshold", "cu-auto_promote_threshold"},
		{"knowledge.curator.promote_from", "cu-promote_from"},
		{"knowledge.curator.promote_to", "cu-promote_to"},
		{"knowledge.code_maps.enabled", "cm-enabled"},
		{"knowledge.code_maps.schedule", "cm-schedule"},
		{"knowledge.code_maps.layer", "cm-layer"},
		{"knowledge.code_maps.max_bytes", "cm-max_bytes"},
		{"knowledge.primer.max_facts", "pr-max_facts"},
		{"knowledge.primer.max_bytes", "pr-max_bytes"},
	} {
		if !strings.Contains(body, "('"+c.id+"', ") || !strings.Contains(body, "'"+c.yaml+"'") {
			t.Errorf("%s has no pipeline control with its YAML hint (%s)", c.yaml, c.id)
		}
	}
	for _, yaml := range []string{
		"knowledge.bead_synthesizer.vault_path",
		"knowledge.code_maps.vault_path",
		"knowledge.primer.merge_strategy",
		"knowledge.engine",
	} {
		if !strings.Contains(body, "roRow(") || !strings.Contains(body, "'"+yaml+"'") {
			t.Errorf("%s is not shown read-only with its YAML hint", yaml)
		}
	}
	if !strings.Contains(body, "set in hive.yaml; requires restart") {
		t.Error("read-only knowledge keys do not say they need a restart")
	}

	save := extractJSFunction(t, html, "saveKnowledgePipeline")
	if !strings.Contains(save, "'/api/config/knowledge/pipeline'") {
		t.Error("saveKnowledgePipeline does not save through PUT /api/config/knowledge/pipeline")
	}
	for _, readOnly := range []string{"vault_path", "merge_strategy", "engine"} {
		if strings.Contains(save, "'"+readOnly+"'") {
			t.Errorf("saveKnowledgePipeline sends display-only %s", readOnly)
		}
	}

	sharing := extractJSFunction(t, html, "renderGovKnowledgeSharing")
	if !strings.Contains(sharing, "yamlHint('knowledge.public.enabled')") {
		t.Error("knowledge.public.enabled has no YAML hint on the Knowledge tab")
	}
	if !strings.Contains(sharing, "renderKnowledgePipelineSection()") {
		t.Error("the Knowledge tab does not render the pipeline section")
	}
}
