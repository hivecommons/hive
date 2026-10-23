package dashboard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardInternalImportCountRatchet(t *testing.T) {
	// 43 = the v5 baseline of 34 plus pkg/fleetreport and pkg/hiveadvisor,
	// which the v4 fleet self-report and hive-owner-advice features import,
	// plus pkg/scheduler for the kick_template provenance type the
	// SchedulerControl seam now carries (hivecommons/hive#7390), plus
	// pkg/review for the perspective-set config API that lets the hive
	// choose its review perspectives (hivecommons/hive#7717), plus
	// pkg/ioscan for the design-gate plan handlers (RFC #7993), plus
	// pkg/celtrigger for run-stage handoff events (hivecommons/hive#8298), plus
	// pkg/retro for autonomy signal facts and automatic ACMM decisions (#8364),
	// plus pkg/claims for the worker-claim ledger the relay honours and the
	// /api/claims routes expose (hivecommons/hive#8380), plus pkg/upstreamwatch
	// for the divergence view and /api/upstream-watch route (hivecommons/hive#10361),
	// plus pkg/releasenotes for the GET /api/version/release-notes changelog
	// gap endpoint (hivecommons/hive#11065), plus pkg/compliance for the
	// control registry and /api/compliance routes (hivecommons/hive#11078),
	// plus pkg/evidence for the schema-v1 bundle types behind the read-only
	// GET /api/evidence/summary aggregate (hivecommons/hive#12063), plus
	// pkg/hivestate for the state-snapshot time series behind the nous
	// self-report and fleet report (hivecommons/hive#12339), plus pkg/fixqueue
	// for the per-contributor red-PR fix budget the contribute hub schedules
	// stage-072 fix-pr interrupts through (hivecommons/hive#12436), plus
	// pkg/stagelabels so the Operations fleet payload can derive new-vs-rework
	// and pending (not yet dispatched) rework from the persisted stage-label
	// store without extra GitHub calls (hivecommons/hive#12502), plus
	// pkg/nous for the per-campaign experiment store that scopes the
	// Experiment Lab baseline to the active research question (hivecommons/hive#12501),
	// plus pkg/stageengine so the contribute hub submits stage facts to the
	// central engine instead of writing stage/* labels directly (ADR-0025,
	// hivecommons/hive#13009), plus pkg/decisionpolicy so the contribute hub
	// shares the one needs-decision classifier with the issue watcher when a
	// contributor parks on the target repo's own confirmation gate
	// (hivecommons/hive#13059), plus pkg/feedbackbundle, the single
	// implementation of diagnostic-bundle storage and prefilled-issue URL
	// building shared by the spoke and the hub so dashboard login can stay
	// scope-free (hivecommons/hive#1927), plus pkg/forgestate so the GitHub
	// webhook handler can stamp the receipt time and record the
	// webhook.event_to_patch_ms latency sample once the cache patch lands
	// (hivecommons/hive#13380) and so the Overview totals and the
	// merged/closed cost counters read the ForgeState cache instead of
	// issuing /search/issues on every cycle (hivecommons/hive#13376), plus
	// pkg/agentaudit so the contribute hub writes the relay_* lifecycle rows
	// into the one /data/audit.jsonl stream instead of a second audit store
	// (hivecommons/hive#13453), plus pkg/crux, the exported agent-runtime
	// surface the dashboard holds instead of *agent.Manager; pkg/agent stays
	// for the audit, trailer, provider-status and agent-mode helpers the
	// Runtime does not cover, and leaves when those move behind the boundary
	// (hivecommons/hive#13536).
	const maxDashboardInternalImports = 56

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dashboard package: %v", err)
	}
	importRe := regexp.MustCompile(`"github\.com/hivecommons/hive/pkg/([^"]+)"`)
	imports := map[string]struct{}{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, match := range importRe.FindAllStringSubmatch(string(data), -1) {
			pkg := match[1]
			if strings.HasPrefix(pkg, "dashboard/") {
				imports[pkg] = struct{}{}
				continue
			}
			if i := strings.IndexByte(pkg, '/'); i >= 0 {
				pkg = pkg[:i]
			}
			imports[pkg] = struct{}{}
		}
	}
	if len(imports) > maxDashboardInternalImports {
		t.Fatalf("pkg/dashboard imports %d internal packages, want <= %d", len(imports), maxDashboardInternalImports)
	}
}
