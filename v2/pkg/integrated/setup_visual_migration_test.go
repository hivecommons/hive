package integrated

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeVisualHiveRecommendationPreservesLegacyPlanAtSameProfile(t *testing.T) {
	root := t.TempDir()
	original := `# Repository-owned legacy plan.
project:
  name: proof
  setupProfile: complex-app
targets:
  custom:
    kind: command
    url: http://127.0.0.1:4000
contracts:
  - id: domain-contract
    target: custom
mutation:
  enabled: true
  operators:
    - api-500
integrations:
  hive:
    enabled: false
    mode: guarded_repair
    defaultActor: repository-quality
`
	writeFixture(t, root, "visual-hive.config.yaml", original)
	if err := mergeVisualHiveRecommendation(root, "complex-app"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := yaml.Unmarshal(updated, &value); err != nil {
		t.Fatal(err)
	}
	project := anyStringMap(value["project"])
	hive := anyStringMap(anyStringMap(value["integrations"])["hive"])
	if project["name"] != "proof" || project["setupProfile"] != "complex-app" || len(anyStringMap(value["targets"])) != 1 ||
		fmt.Sprint(anyStringMap(anySlice(value["contracts"])[0])["id"]) != "domain-contract" ||
		!reflect.DeepEqual(anySlice(anyStringMap(value["mutation"])["operators"]), []any{"api-500"}) ||
		hive["enabled"] != true || hive["mode"] != "guarded_repair" || hive["defaultActor"] != "repository-quality" {
		t.Fatalf("same-profile integration enablement replaced repository-owned configuration: %+v", value)
	}
	if err := mergeVisualHiveRecommendation(root, "complex-app"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if err != nil || !reflect.DeepEqual(updated, second) {
		t.Fatalf("same-profile integration enablement is not idempotent: err=%v", err)
	}
}

func TestMarkVisualHiveConfigOriginEnablesHiveIntegrationAdditively(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "visual-hive.config.yaml", `project:
  name: generated-proof
targets:
  custom:
    kind: command
contracts:
  - id: generated-contract
    target: custom
integrations:
  hive:
    enabled: false
    mode: measured
    acmmLevel: 5
`)
	if err := markVisualHiveConfigOrigin(root, "generated", "complex-app"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := yaml.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	project := anyStringMap(value["project"])
	hive := anyStringMap(anyStringMap(value["integrations"])["hive"])
	if project["name"] != "generated-proof" || project["setupProfile"] != "complex-app" || project["hiveManagedBy"] != "hive-integrated" ||
		project["hiveConfigOrigin"] != "generated" || hive["enabled"] != true || hive["mode"] != "measured" || hive["acmmLevel"] != 5 ||
		fmt.Sprint(anyStringMap(anySlice(value["contracts"])[0])["id"]) != "generated-contract" {
		t.Fatalf("generated Visual Hive config did not preserve repository fields while enabling Hive: %+v", value)
	}
}

func TestMergeVisualHiveRecommendationReconcilesCoverageAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "visual-hive.config.yaml", `project:
  name: proof
  setupProfile: free-local
  hiveManagedBy: hive-integrated
  hiveConfigOrigin: generated
targets:
  custom:
    kind: command
    url: http://127.0.0.1:4000
contracts:
  - id: existing
    target: custom
mutation:
  enabled: false
  operators: [api-500]
`)
	report := map[string]any{"recommendedConfig": map[string]any{
		"project":    map[string]any{"setupProfile": "complex-app"},
		"targets":    map[string]any{"recommended": map[string]any{"kind": "command", "url": "http://127.0.0.1:5000"}},
		"contracts":  []any{map[string]any{"id": "recommended-contract", "target": "recommended"}},
		"selection":  map[string]any{"changedFiles": []any{map[string]any{"pattern": "src/**", "contracts": []any{"recommended-contract"}}}},
		"mutation":   map[string]any{"enabled": true, "operators": []any{"api-500", "broken-image"}},
		"costPolicy": map[string]any{"maxExternalScreenshotsPerRun": 5},
	}}
	data, _ := json.Marshal(report)
	writeFixture(t, root, ".visual-hive/recommendations.json", string(data))
	if err := mergeVisualHiveRecommendation(root, "complex-app"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := yaml.Unmarshal(first, &value); err != nil {
		t.Fatal(err)
	}
	if anyStringMap(value["project"])["setupProfile"] != "complex-app" || len(anyStringMap(value["targets"])) != 1 || len(anySlice(value["contracts"])) != 1 {
		t.Fatalf("coverage migration did not reconcile to the exact deterministic plan: %+v", value)
	}
	if anyStringMap(anyStringMap(value["integrations"])["hive"])["enabled"] != true {
		t.Fatalf("coverage migration did not enable Hive integration: %+v", value["integrations"])
	}
	operators := anySlice(anyStringMap(value["mutation"])["operators"])
	if !reflect.DeepEqual(operators, []any{"api-500", "broken-image"}) || anyStringMap(value["mutation"])["enabled"] != true {
		t.Fatalf("mutation coverage was not additively upgraded: %+v", value["mutation"])
	}
	if err := mergeVisualHiveRecommendation(root, "complex-app"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical coverage migration rewrote the Visual Hive config")
	}
	downgrade := map[string]any{"recommendedConfig": map[string]any{
		"project":   map[string]any{"setupProfile": "free-local"},
		"targets":   map[string]any{"simple": map[string]any{"kind": "command"}},
		"contracts": []any{map[string]any{"id": "simple-contract", "target": "simple"}},
		"selection": map[string]any{"changedFiles": []any{map[string]any{"pattern": "web/**", "contracts": []any{"simple-contract"}}}},
		"mutation":  map[string]any{"enabled": false, "operators": []any{}},
	}}
	downgradeData, _ := json.Marshal(downgrade)
	writeFixture(t, root, ".visual-hive/recommendations.json", string(downgradeData))
	if err := mergeVisualHiveRecommendation(root, "free-local"); err != nil {
		t.Fatal(err)
	}
	downgradedData, _ := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	var downgraded map[string]any
	if err := yaml.Unmarshal(downgradedData, &downgraded); err != nil {
		t.Fatal(err)
	}
	if len(anyStringMap(downgraded["targets"])) != 1 || fmt.Sprint(anyStringMap(anyStringMap(downgraded["targets"])["simple"])["kind"]) != "command" ||
		len(anySlice(downgraded["contracts"])) != 1 || anyStringMap(downgraded["mutation"])["enabled"] != false {
		t.Fatalf("coverage downgrade retained stale complex-app scope: %+v", downgraded)
	}
}

func TestMergeVisualHiveRecommendationPreservesPreexistingDomainPlan(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "visual-hive.config.yaml", `project:
  name: proof
  setupProfile: free-local
targets:
  custom:
    kind: command
contracts:
  - id: domain-contract
    target: custom
mutation:
  enabled: true
  operators: [api-500]
`)
	report := map[string]any{"recommendedConfig": map[string]any{
		"targets":   map[string]any{"generic": map[string]any{"kind": "command"}},
		"contracts": []any{map[string]any{"id": "generic-contract", "target": "generic"}},
		"mutation":  map[string]any{"enabled": true, "operators": []any{"broken-image"}},
	}}
	data, _ := json.Marshal(report)
	writeFixture(t, root, ".visual-hive/recommendations.json", string(data))
	if err := mergeVisualHiveRecommendation(root, "complex-app"); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	var value map[string]any
	if err := yaml.Unmarshal(updated, &value); err != nil {
		t.Fatal(err)
	}
	project := anyStringMap(value["project"])
	if project["setupProfile"] != "complex-app" || project["hiveConfigOrigin"] != "existing" || len(anyStringMap(value["targets"])) != 1 ||
		fmt.Sprint(anyStringMap(anySlice(value["contracts"])[0])["id"]) != "domain-contract" || !reflect.DeepEqual(anySlice(anyStringMap(value["mutation"])["operators"]), []any{"api-500"}) ||
		anyStringMap(anyStringMap(value["integrations"])["hive"])["enabled"] != true {
		t.Fatalf("pre-existing repository testing plan was replaced: %+v", value)
	}
}

func TestVerifyVisualHiveCoverageProfileRequiresHiveLifecycleOwnership(t *testing.T) {
	valid := []byte("project:\n  setupProfile: complex-app\nintegrations:\n  hive:\n    enabled: true\n")
	if err := verifyVisualHiveCoverageProfile(valid, "complex-app"); err != nil {
		t.Fatalf("valid integrated Visual Hive config was rejected: %v", err)
	}
	for name, config := range map[string][]byte{
		"missing":  []byte("project:\n  setupProfile: complex-app\n"),
		"disabled": []byte("project:\n  setupProfile: complex-app\nintegrations:\n  hive:\n    enabled: false\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyVisualHiveCoverageProfile(config, "complex-app"); err == nil {
				t.Fatal("non-integrated Visual Hive config was accepted as production-ready")
			}
		})
	}
}
