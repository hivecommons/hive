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
// muted line (inside the Data collection note), and archived campaigns are
// listed read-only. The config dialog no longer carries a research question
// or "Start a new experiment": experiments are defined on the cards only.
func TestNousCampaignUIRendersPerCampaignBaseline(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const camp = s.campaign || null;",
		"<div class=\"lbl\">Baseline snapshots · experiment #${Number(camp.id) || 0}</div>",
		"<div class=\"lbl\">Hypotheses this experiment</div>",
		"html += nousDataCollectionNote(s, camp);",
		"h += nousObservationLine(obs, camp || null);",
		"if (Array.isArray(s.campaigns)) html += nousExperimentCards(s.campaigns, s);",
		"html += nousArchivedCampaigns((s.campaigns || []).filter(c => c && c.status === 'archived'));",
		".nous-observation-note {",
		".nous-archived-row {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// The settings dialog is lab-wide guardrails only: no research question
	// textarea, no "Start a new experiment", no goals PUT from the UI.
	for _, banned := range []string{
		"nc-repo-goal", "nousStartCampaign", "NOUS_NEW_EXPERIMENT_CONFIRM", "nousQuestionChanged",
		"Goal Definition", "Start a new experiment<", "/api/nous/campaign/start", "/api/nous/config/goals",
		"data-current-question", "Changing the question starts a new experiment",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html still contains %q: experiments are defined only via + New experiment", banned)
		}
	}
	// No global baseline progress bar / "Experiment Progress" box: the
	// observation history is a total, not progress; each card has its bar.
	for _, banned := range []string{
		"Complete — switch to Suggest when ready.", "<h3>Data Collection</h3>", "Experiment Progress</h3>",
		"nousProgressBar(basePct, 'Baseline snapshot collection')", "No proposals yet for your",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("index.html still renders %q", banned)
		}
	}
	if !strings.Contains(html, "<h3>Data collection</h3>") {
		t.Error("index.html lacks the compact Data collection note")
	}

	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the wiring assertions above still ran")
	}
	script := "function escapeHtml(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;');}\n" +
		"function formatDashboardDateTime(ts){return new Date(ts).toISOString().slice(0,16);}\n" +
		extractJSFunction(t, html, "nousCampaignDate") + "\n    }\n" +
		extractJSFunction(t, html, "nousObservationLine") + "\n    }\n" +
		extractJSFunction(t, html, "nousOutputLinks") + "\n    }\n" +
		extractJSFunction(t, html, "nousResearchQuestionText") + "\n    }\n" +
		extractJSFunction(t, html, "nousScopePills") + "\n    }\n" +
		extractJSFunction(t, html, "nousArchivedCampaigns") + "\n    }\n" +
		extractJSFunction(t, html, "nousDataCollectionNote") + "\n    }\n" +
		`const NOUS_BASELINE_TARGET_DEFAULT = 672;
		const out = {
			obs: nousObservationLine({ snapshots: 2688, since: '2026-09-12T10:00:00Z' }, { id: 2, snapshots: 0 }),
			obsNoCamp: nousObservationLine({ snapshots: 5 }, null),
			note: nousDataCollectionNote({ observation: { snapshots: 854, since: '2026-09-16T10:00:00Z' }, baseline: { retention: '24 h raw snapshots, then hourly buckets for 30 days' } }, { id: 1, snapshots: 854 }),
			archived: nousArchivedCampaigns([{ id: 1, question: '', startedAt: '2026-09-12T10:00:00Z', endedAt: '2026-10-16T18:00:00Z', snapshots: 2688, experiments: 1, principles: 2, results: [{ id: 'exp-old', verdict: 'improved', delta: '-20%' }] }]),
			none: nousArchivedCampaigns([]),
		};
		console.log(JSON.stringify(out));`
	raw, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got struct {
		Obs       string `json:"obs"`
		ObsNoCamp string `json:"obsNoCamp"`
		Note      string `json:"note"`
		Archived  string `json:"archived"`
		None      string `json:"none"`
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
	// The Data collection note: observation total once, what is collected,
	// cadence, retention; no progress bar and no "switch to Suggest" hint.
	for _, want := range []string{
		"Observation history: 854 snapshots since 2026-09-16T10:00.",
		"Collecting: queue issues, queue PRs, queue on hold, SLA violations, agents kicked, agent states, total tokens.",
		"One hive-state snapshot per governor evaluation cycle (4/hour).",
		"Retention: 24 h raw snapshots, then hourly buckets for 30 days.",
		"Each experiment card above carries its own baseline bar",
	} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("data collection note missing %q in %q", want, got.Note)
		}
	}
	if strings.Count(got.Note, "854") != 1 {
		t.Errorf("observation total must appear exactly once in the note: %q", got.Note)
	}
	for _, banned := range []string{"progressbar", "switch to Suggest when ready", "Experiment Progress"} {
		if strings.Contains(got.Note, banned) {
			t.Errorf("data collection note must not contain %q: %q", banned, got.Note)
		}
	}
}
