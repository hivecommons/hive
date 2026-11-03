package dashboard

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func controlTestBundle(number int, arm string) *evidence.Bundle {
	opened := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	return &evidence.Bundle{
		Repo: "o/r", Number: number, HeadSHA: "head", ControlWindow: arm,
		Author:    evidence.Author{Kind: evidence.AuthorHuman},
		CreatedAt: opened, UpdatedAt: opened,
		Merge: &evidence.MergeEvent{OpenedAt: opened, At: opened.Add(4 * time.Hour)},
		CI:    evidence.CI{Status: "success", Checks: []evidence.Check{{Name: "test", Conclusion: "success"}}},
	}
}

func TestEvidenceControlMetricGuards(t *testing.T) {
	for _, n := range []int{0, 19, 20} {
		var bundles []*evidence.Bundle
		for i := 1; i <= 20; i++ {
			b := controlTestBundle(i, evidence.ControlWindowOn)
			bundles = append(bundles, b)
		}
		for i := 1; i <= n; i++ {
			b := controlTestBundle(100+i, evidence.ControlWindowOff)
			b.Merge.At = b.Merge.OpenedAt.Add(8 * time.Hour)
			b.CI.Status = "failure"
			bundles = append(bundles, b)
		}
		c := evidenceControl(evidence.FoldPRs(bundles, "", ""))
		if c.MinN != 20 || len(c.ByRepo) != 1 {
			t.Fatalf("bad control result: %+v", c)
		}
		r := c.ByRepo[0]
		for key, delta := range r.Delta {
			if (delta != nil) != (n == 20) {
				t.Errorf("off n=%d: %s delta=%v", n, key, delta)
			}
		}
		if n == 20 {
			if d := *r.Delta["median_time_to_merge_hours"]; d != -4 {
				t.Errorf("time delta=%v, want -4 hours", d)
			}
			if d := *r.Delta["ci_pass_rate"]; d != 1 {
				t.Errorf("CI delta=%v, want 1", d)
			}
		}
	}
}

func TestEvidenceControlUsesMetricDenominators(t *testing.T) {
	var bundles []*evidence.Bundle
	for i := 1; i <= 25; i++ {
		for j, arm := range []string{evidence.ControlWindowOn, evidence.ControlWindowOff} {
			b := controlTestBundle(i+100*j, arm)
			if i > 19 {
				b.Merge = nil // unknown outcome, not a failed merge
				b.CI = evidence.CI{}
			}
			bundles = append(bundles, b)
		}
	}
	r := evidenceControl(evidence.FoldPRs(bundles, "", "")).ByRepo[0]
	if r.On.Outcomes.N != 25 || r.On.Outcomes.Unknown != 6 {
		t.Fatalf("outcomes=%+v", r.On.Outcomes)
	}
	for key, m := range r.On.Metrics {
		if m.N != 19 || r.Delta[key] != nil {
			t.Errorf("%s: metric=%+v delta=%v, want n=19 and withheld", key, m, r.Delta[key])
		}
	}
	// CI pending must remain unobserved even when it has checks.
	b := controlTestBundle(500, evidence.ControlWindowOn)
	b.CI.Status = "pending"
	a := controlArm([]*evidence.PR{{Bundle: b, Merge: b.Merge}})
	if m := a.Metrics["ci_pass_rate"]; m.N != 0 || m.Value != nil {
		t.Fatalf("pending CI counted as settled: %+v", m)
	}
}

func TestEvidenceControlNeverPoolsReposOrInfersAssignment(t *testing.T) {
	var bundles []*evidence.Bundle
	for i := 1; i <= 20; i++ {
		bundles = append(bundles, controlTestBundle(i, evidence.ControlWindowOn))
		b := controlTestBundle(100+i, evidence.ControlWindowOff)
		b.Repo = "o/other"
		bundles = append(bundles, b)
	}
	bundles = append(bundles, controlTestBundle(500, ""), controlTestBundle(501, "bad"))
	b := controlTestBundle(502, evidence.ControlWindowOn)
	otherHead := *b
	otherHead.HeadSHA = "second"
	otherHead.ControlWindow = evidence.ControlWindowOff
	bundles = append(bundles, b, &otherHead)
	c := evidenceControl(evidence.FoldPRs(bundles, "", ""))
	if c.Unassigned != 3 || len(c.ByRepo) != 2 || c.ByRepo[0].Repo != "o/other" {
		t.Fatalf("unexpected grouping: %+v", c)
	}
	for _, r := range c.ByRepo {
		for key, d := range r.Delta {
			if d != nil {
				t.Errorf("pooled unrelated repos: %s %s=%v", r.Repo, key, *d)
			}
		}
	}
}

func TestEvidenceControlSummaryFoldsFiltersAndUsesOutcomeHints(t *testing.T) {
	b := controlTestBundle(1, evidence.ControlWindowOff)
	b.Merge = nil
	duplicate := *b
	duplicate.HeadSHA = "second"
	duplicate.CreatedAt = b.CreatedAt.Add(time.Hour)
	otherRepo := controlTestBundle(2, evidence.ControlWindowOn)
	otherRepo.Repo = "o/other"
	otherKind := controlTestBundle(3, evidence.ControlWindowOn)
	otherKind.Author.Kind = evidence.AuthorAgent
	old := controlTestBundle(4, evidence.ControlWindowOff)
	old.CreatedAt = b.CreatedAt.Add(-72 * time.Hour)
	old.Merge = nil
	now := b.CreatedAt.Add(12 * time.Hour)
	hints := map[string]evidence.OutcomeHint{
		evidence.PRKey(b.Repo, b.Number): {State: evidence.BackfillStateClosed, At: now},
	}
	s := buildEvidenceSummaryWindowHints([]*evidence.Bundle{b, &duplicate, otherRepo, otherKind, old}, hints,
		evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}, "o/r", evidence.AuthorHuman)
	if s.N != 1 || len(s.ControlArm.ByRepo) != 1 {
		t.Fatalf("summary filter/fold failed: n=%d control=%+v", s.N, s.ControlArm)
	}
	a := s.ControlArm.ByRepo[0].Off
	if a.Outcomes.N != 1 || a.Outcomes.Closed != 1 || a.Outcomes.Unknown != 0 {
		t.Fatalf("outcome hints not applied: %+v", a)
	}
	raw, err := json.Marshal(s.ControlArm)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"merged_rate":null`) {
		t.Fatalf("withheld delta must be explicit null: %s", raw)
	}
}

func TestEvidenceControlMissingTimesAndChangeFailures(t *testing.T) {
	b := controlTestBundle(1, evidence.ControlWindowOff)
	b.RevertedBy = []string{"o/r#2@h"}
	b.IssueReopens = []evidence.IssueReopen{{Issue: 10, At: b.Merge.At.Add(time.Hour), Source: evidence.IssueReopenSourceClaimLedger}}
	b.Merge.OpenedAt = time.Time{}
	var p *evidence.PR
	for _, pr := range evidence.FoldPRs([]*evidence.Bundle{b}, "", "") {
		p = pr
	}
	a := controlArm([]*evidence.PR{p})
	if m := a.Metrics["median_time_to_merge_hours"]; m.N != 0 || m.Value != nil {
		t.Fatalf("missing open time treated as review time: %+v", m)
	}
	for _, key := range []string{"reverted_14d_rate", "issue_reopened_14d_rate"} {
		m := a.Metrics[key]
		if m.N != 1 || m.Value == nil || math.Abs(*m.Value-1) > 1e-9 {
			t.Errorf("%s=%+v, want 1/1", key, m)
		}
	}
}

func TestEvidenceControlUIRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: control rendering not executed")
	}
	html := indexHTML(t)
	source := jsFunc(t, html, "escapeHtml") + "\n" + jsFunc(t, html, "evControlArmHtml") + `
const assert = require('node:assert/strict');
assert.match(evControlArmHtml(), /No prospectively assigned/);
const arm = (n, hours) => ({outcomes: {n: 50}, metrics: {
  ci_pass_rate: {n, value: 0.5}, median_time_to_merge_hours: {n, value: hours}
}});
const row = {repo: 'o/<script>', on: arm(20, 4), off: arm(19, 8),
  delta: {ci_pass_rate: 0.25, median_time_to_merge_hours: -4}};
let rendered = evControlArmHtml({unassigned: 7, min_n: 20, by_repo: [row]});
assert.match(rendered, /7 PR\(s\) unassigned/);
assert.match(rendered, /o\/&lt;script&gt;/);
assert.doesNotMatch(rendered, /<script>|NaN|undefined|\+25.0 pp|-4.0 h/);
assert.match(rendered, /withheld/);
row.off = arm(20, 8);
rendered = evControlArmHtml({min_n: 20, by_repo: [row]});
assert.match(rendered, /\+25.0 pp/);
assert.match(rendered, /-4.0 h/);
row.delta.ci_pass_rate = null;
rendered = evControlArmHtml({min_n: 20, by_repo: [row]});
assert.doesNotMatch(rendered, /\+25.0 pp/);
`
	if out, err := exec.Command(node, "-e", source).CombinedOutput(); err != nil {
		t.Fatalf("control renderer failed: %v\n%s", err, out)
	}
}

func TestEvidenceControlUIContract(t *testing.T) {
	data, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"evControlArmHtml(dto.control_arm)", "Number(a.n) < minN || Number(b.n) < minN",
		"unassigned or conflicting", "Descriptive, not causal", "escapeHtml(r.repo || '')",
		"No prospectively assigned control-window observations.",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("control-arm UI missing %q", want)
		}
	}
}
