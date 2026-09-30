package hub

import (
	"strings"
	"testing"
	"time"
)

// #9692: the hosted spoke's startup probe allowed about 160s, and the first
// boot of the per-agent-UID build re-owns every agent home before the server
// binds. Spokes with large homes were killed mid-migration and started over.

// legacyHostedStartupBudget is the pre-#9692 budget (10s + 30 x 5s), kept only
// so the test can assert the new budget is strictly larger than the one that
// crash-looped.
const legacyHostedStartupBudget = 160 * time.Second

// probeInt reads an integer probe field from a decoded manifest map. yaml.v3
// decodes plain integers into int, so anything else (for example the
// "<no value>" a missing template key renders) is a failure.
func probeInt(t *testing.T, probe map[string]any, field string) int {
	t.Helper()
	v, ok := probe[field].(int)
	if !ok {
		t.Fatalf("startupProbe.%s = %v (%T), want an integer", field, probe[field], probe[field])
	}
	return v
}

func renderedHiveStartupProbe(t *testing.T) map[string]any {
	t.Helper()
	manifest := renderProvisionManifest(t, true, true)
	deploy := manifestObject(t, manifest, "Deployment", "hive")
	spec := manifestMap(t, deploy["spec"], "Deployment spec")
	tmpl := manifestMap(t, spec["template"], "pod template")
	podSpec := manifestMap(t, tmpl["spec"], "pod spec")
	containers, ok := podSpec["containers"].([]any)
	if !ok || len(containers) == 0 {
		t.Fatalf("pod spec containers = %T, want a non-empty list", podSpec["containers"])
	}
	for _, c := range containers {
		container := manifestMap(t, c, "container")
		if container["name"] != "hive" {
			continue
		}
		return manifestMap(t, container["startupProbe"], "hive startupProbe")
	}
	t.Fatalf("rendered Deployment has no container named hive")
	return nil
}

// TestHostedStartupProbeRendersBudgetConstants pins that the rendered manifest
// carries the named constants rather than literals that could drift from them.
func TestHostedStartupProbeRendersBudgetConstants(t *testing.T) {
	probe := renderedHiveStartupProbe(t)
	if got := probeInt(t, probe, "failureThreshold"); got != hostedStartupProbeFailureThreshold {
		t.Errorf("startupProbe.failureThreshold = %d, want %d (hostedStartupProbeFailureThreshold)",
			got, hostedStartupProbeFailureThreshold)
	}
	if got := probeInt(t, probe, "periodSeconds"); got != hostedStartupProbePeriodSeconds {
		t.Errorf("startupProbe.periodSeconds = %d, want %d", got, hostedStartupProbePeriodSeconds)
	}
	if got := probeInt(t, probe, "initialDelaySeconds"); got != hostedStartupProbeInitialDelaySeconds {
		t.Errorf("startupProbe.initialDelaySeconds = %d, want %d", got, hostedStartupProbeInitialDelaySeconds)
	}
}

// TestHostedStartupProbeBudgetCoversMigration is the invariant: the budget the
// kubelet actually enforces (initial delay + threshold x period) reaches
// hostedStartupBudget and is larger than the budget that crash-looped.
func TestHostedStartupProbeBudgetCoversMigration(t *testing.T) {
	probe := renderedHiveStartupProbe(t)
	delay := probeInt(t, probe, "initialDelaySeconds")
	period := probeInt(t, probe, "periodSeconds")
	threshold := probeInt(t, probe, "failureThreshold")
	effective := time.Duration(delay+threshold*period) * time.Second

	if effective < hostedStartupBudget {
		t.Errorf("rendered startup budget %s (%ds + %d x %ds) is below hostedStartupBudget %s",
			effective, delay, threshold, period, hostedStartupBudget)
	}
	if effective-hostedStartupBudget >= time.Duration(period)*time.Second {
		t.Errorf("rendered startup budget %s overshoots hostedStartupBudget %s by a full period or more",
			effective, hostedStartupBudget)
	}
	if effective <= legacyHostedStartupBudget {
		t.Errorf("rendered startup budget %s is not larger than the pre-#9692 %s that crash-looped first boots",
			effective, legacyHostedStartupBudget)
	}
}

// TestHostedStartupProbeTemplateHasNoLiteralThreshold guards the template text
// itself: a literal failureThreshold under startupProbe would bypass the
// constant for every newly provisioned spoke.
func TestHostedStartupProbeTemplateHasNoLiteralThreshold(t *testing.T) {
	idx := strings.Index(k8sManifestTemplate, "startupProbe:")
	if idx < 0 {
		t.Fatal("k8sManifestTemplate has no startupProbe")
	}
	block := k8sManifestTemplate[idx:]
	if end := strings.Index(block, "livenessProbe:"); end >= 0 {
		block = block[:end]
	}
	want := "failureThreshold: {{.StartupProbeFailureThreshold}}"
	if !strings.Contains(block, want) {
		t.Errorf("startupProbe block does not render failureThreshold from StartupProbeFailureThreshold:\n%s", block)
	}
}
