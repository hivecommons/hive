package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestNousCampaignUIRendersPerCampaignBaseline pins the Experiment Lab's
// per-campaign baseline (hivecommons/hive#12501): the stat tile and the Data
// Collection bar read the active campaign (0 / 672 right after a new
// research question), the whole observation history stays visible as a
// muted line, archived campaigns are listed read-only, and the config dialog
// confirms before a question change or an explicit "Start a new experiment".
func TestNousCampaignUIRendersPerCampaignBaseline(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const camp = s.campaign || null;",
		"const obs = s.observation || null;",
		"<div class=\"lbl\">Baseline snapshots · experiment #${Number(camp.id) || 0}</div>",
		"<div class=\"lbl\">Hypotheses this experiment</div>",
		"if (camp) html += nousObservationLine(obs, camp);",
		"Experiment #${Number(camp.id) || 0} started ${escapeHtml(nousCampaignDate(camp.startedAt))}. Baseline and hypotheses restart with each research question",
		"html += nousArchivedCampaigns(s.campaigns || []);",
		"const NOUS_NEW_EXPERIMENT_CONFIRM = 'Start a new experiment? Baseline and hypotheses restart; previous campaign is archived.';",
		"if (!(await hiveConfirm(NOUS_NEW_EXPERIMENT_CONFIRM))) { showToast('Research question not changed', 'info'); return; }",
		"const r = await fetch('/api/nous/campaign/start', {",
		`data-action="nousStartCampaign"`,
		"case 'nousStartCampaign': nousStartCampaign(); break;",
		`data-current-question="${escapeHtml(currentQuestion)}"`,
		"const currentQuestion = (cfg.goals && cfg.goals.repoGoal) || repoCamp.research_question || (activeCamp && activeCamp.question) || '';",
		".nous-observation-note {",
		".nous-archived-row {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// The Data Collection bar keeps reading baseline.snapshots, which the
	// server now scopes to the active campaign.
	if !strings.Contains(html, "const baseCount = Number(base.snapshots != null ? base.snapshots : (s.snapshotCount || 0));") {
		t.Error("Data Collection bar must read baseline.snapshots (campaign-scoped) first")
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertions above still ran")
	}
	script := "function escapeHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;');}\n" +
		"function formatDashboardDateTime(ts){return new Date(ts).toISOString().slice(0,16);}\n" +
		extractJSFunction(t, html, "nousCampaignDate") + "\n    }\n" +
		extractJSFunction(t, html, "nousObservationLine") + "\n    }\n" +
		extractJSFunction(t, html, "nousOutputLinks") + "\n    }\n" +
		extractJSFunction(t, html, "nousArchivedCampaigns") + "\n    }\n" +
		extractJSFunction(t, html, "nousQuestionChanged") + "\n    }\n" +
		`const out = {
			obs: nousObservationLine({ snapshots: 2688, since: '2026-09-12T10:00:00Z' }, { id: 2, snapshots: 0 }),
			obsNoCamp: nousObservationLine({ snapshots: 5 }, null),
			archived: nousArchivedCampaigns([{ id: 1, question: '', startedAt: '2026-09-12T10:00:00Z', endedAt: '2026-10-16T18:00:00Z', snapshots: 2688, experiments: 1, principles: 2, results: [{ id: 'exp-old', verdict: 'improved', delta: '-20%' }] }]),
			none: nousArchivedCampaigns([]),
			changed: [nousQuestionChanged('', 'Cut CI time'), nousQuestionChanged('Cut CI time', ' cut  ci time '), nousQuestionChanged('Cut CI time', ''), nousQuestionChanged('a', 'b')],
		};
		console.log(JSON.stringify(out));`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got struct {
		Obs       string `json:"obs"`
		ObsNoCamp string `json:"obsNoCamp"`
		Archived  string `json:"archived"`
		None      string `json:"none"`
		Changed   []bool `json:"changed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if !strings.Contains(got.Obs, "Observation history: 2688 snapshots since 2026-09-12T10:00 (2688 before this experiment).") || !strings.Contains(got.Obs, `class="nous-observation-note text-muted"`) {
		t.Errorf("observation line = %q", got.Obs)
	}
	if !strings.Contains(got.ObsNoCamp, "Observation history: 5 snapshots.") {
		t.Errorf("observation line without campaign = %q", got.ObsNoCamp)
	}
	for _, want := range []string{
		"Previous experiments", "archived · read-only",
		`data-campaign-id="1"`, "#1 · (no research question)",
		"2688 snapshots · 1 hypotheses · 2 principles",
		"<strong>exp-old</strong>", `<span class="nous-status-pill">improved</span>`, "-20%",
	} {
		if !strings.Contains(got.Archived, want) {
			t.Errorf("archived campaigns missing %q in %q", want, got.Archived)
		}
	}
	if got.None != "" {
		t.Errorf("no archived campaigns must render nothing, got %q", got.None)
	}
	if want := []bool{true, false, false, true}; len(got.Changed) != 4 || got.Changed[0] != want[0] || got.Changed[1] != want[1] || got.Changed[2] != want[2] || got.Changed[3] != want[3] {
		t.Errorf("nousQuestionChanged = %v, want %v", got.Changed, want)
	}
}
