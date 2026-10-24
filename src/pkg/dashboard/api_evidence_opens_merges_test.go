package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestEvidenceHiveMergedPR pins the "hive-merged PR" definition: a merge this
// hive performed counts for any author; a merge learned from the forge
// counts only when the author is an agent (the Outcome card's agent-authored
// set); open or closed-unmerged PRs never count.
func TestEvidenceHiveMergedPR(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	pr := func(kind string, merged, elsewhere bool) *evidence.PR {
		b := &evidence.Bundle{Repo: "o/r", Number: 1, Author: evidence.Author{Login: "a", Kind: kind}, CreatedAt: now, UpdatedAt: now}
		p := &evidence.PR{Bundle: b, OpenedAt: now, MergedElsewhere: elsewhere}
		if merged {
			p.Merge = &evidence.MergeEvent{At: now, OpenedAt: now}
		}
		return p
	}
	cases := []struct {
		name string
		p    *evidence.PR
		want bool
	}{
		{"nil", nil, false},
		{"no bundle", &evidence.PR{Merge: &evidence.MergeEvent{At: now}}, false},
		{"open human", pr(evidence.AuthorHuman, false, false), false},
		{"hive merged human", pr(evidence.AuthorHuman, true, false), true},
		{"hive merged bot", pr(evidence.AuthorBot, true, false), true},
		{"hive merged agent", pr(evidence.AuthorAgent, true, false), true},
		{"human merged elsewhere", pr(evidence.AuthorHuman, true, true), false},
		{"bot merged elsewhere", pr(evidence.AuthorBot, true, true), false},
		{"agent merged elsewhere", pr(evidence.AuthorAgent, true, true), true},
		{"merge without time", &evidence.PR{Bundle: &evidence.Bundle{Repo: "o/r"}, Merge: &evidence.MergeEvent{}}, false},
	}
	for _, c := range cases {
		if got := evidenceHiveMergedPR(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEvidenceOpensMergesEvents pins the event collection: only the two hive
// issue-create audit actions count as opens (closes, PR creates and
// unparseable entries do not), merges come from hive-merged folded PRs, and
// the repo filter applies to both sides case-insensitively.
func TestEvidenceOpensMergesEvents(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	entries := []AuditEntry{
		{Timestamp: ts(now.Add(-time.Hour)), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: ts(now.Add(-2 * time.Hour)), Action: ghpkg.AuditActionHiveIssueCreated, Repo: "O/R", Target: 2},
		{Timestamp: ts(now), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=x/y, number=3"},
		{Timestamp: ts(now), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=1"},
		{Timestamp: ts(now), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=4"},
		{Timestamp: "nope", Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=o/r, number=5"},
		{Timestamp: ts(now), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "no repo here"},
	}
	bundle := func(repo string, n int, kind string) *evidence.Bundle {
		return &evidence.Bundle{Repo: repo, Number: n, Author: evidence.Author{Login: "a", Kind: kind}, CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now}
	}
	hiveMerged := bundle("o/r", 10, evidence.AuthorHuman)
	hiveMerged.Merge = &evidence.MergeEvent{At: now.Add(-3 * time.Hour), OpenedAt: now.Add(-24 * time.Hour)}
	otherRepo := bundle("x/y", 11, evidence.AuthorAgent)
	otherRepo.Merge = &evidence.MergeEvent{At: now.Add(-4 * time.Hour), OpenedAt: now.Add(-24 * time.Hour)}
	open := bundle("o/r", 12, evidence.AuthorAgent)
	// Merges learned from the forge (an Outcome with state merged) are
	// MergedElsewhere after folding: excluded for a human, kept for an agent.
	elsewhereHuman := bundle("o/r", 13, evidence.AuthorHuman)
	elsewhereHuman.Merge = &evidence.MergeEvent{At: now.Add(-5 * time.Hour), OpenedAt: now.Add(-24 * time.Hour)}
	elsewhereHuman.Outcome = &evidence.Outcome{State: evidence.BackfillStateMerged, At: now.Add(-5 * time.Hour), MergedAt: now.Add(-5 * time.Hour)}
	elsewhereAgent := bundle("o/r", 14, evidence.AuthorAgent)
	elsewhereAgent.Merge = &evidence.MergeEvent{At: now.Add(-6 * time.Hour), OpenedAt: now.Add(-24 * time.Hour)}
	elsewhereAgent.Outcome = &evidence.Outcome{State: evidence.BackfillStateMerged, At: now.Add(-6 * time.Hour), MergedAt: now.Add(-6 * time.Hour)}
	prs := evidence.FoldPRs([]*evidence.Bundle{hiveMerged, otherRepo, open, elsewhereHuman, elsewhereAgent}, "", "")
	if !prs[evidence.PRKey("o/r", 13)].MergedElsewhere || !prs[evidence.PRKey("o/r", 14)].MergedElsewhere {
		t.Fatal("fixture: forge-observed merges must fold as MergedElsewhere")
	}

	count := func(events []evidence.OpensMergesEvent) (opens, merges int) {
		for _, ev := range events {
			if ev.Merge {
				merges++
			} else {
				opens++
			}
		}
		return opens, merges
	}
	all := evidenceOpensMergesEvents(entries, prs, "")
	if opens, merges := count(all); opens != 3 || merges != 3 {
		t.Errorf("all repos: opens %d merges %d (%+v)", opens, merges, all)
	}
	or := evidenceOpensMergesEvents(entries, prs, "o/r")
	if opens, merges := count(or); opens != 2 || merges != 2 {
		t.Errorf("o/r: opens %d merges %d (%+v)", opens, merges, or)
	}
	for _, ev := range or {
		if ev.Repo != "o/r" && ev.Repo != "O/R" {
			t.Errorf("repo filter leaked %+v", ev)
		}
		if ev.At.IsZero() {
			t.Errorf("event without time %+v", ev)
		}
	}
	if none := evidenceOpensMergesEvents(nil, nil, ""); len(none) != 0 {
		t.Errorf("no inputs = %+v", none)
	}
}

// TestBuildEvidenceOpensMerges checks the window predicate follows the
// Throughput contract (bounded presets, exclusive custom end) and the mode
// reaches the band.
func TestBuildEvidenceOpensMerges(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	events := []evidence.OpensMergesEvent{
		{Repo: "o/r", At: now.Add(-time.Hour)},
		{Repo: "o/r", At: now.Add(-2 * time.Hour), Merge: true},
		{Repo: "o/r", At: now.Add(-48 * time.Hour), Merge: true},
	}
	day := buildEvidenceOpensMerges(events, "SURGE", evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}, now)
	if day.Mode != evidence.CadenceSurge || day.Total.Opens != 1 || day.Total.Merges != 1 || day.Status != evidence.OpensMergesInBand {
		t.Errorf("24h = %+v", day)
	}
	if day.Rolling30d.Merges != 2 || day.Rolling30d.Opens != 1 {
		t.Errorf("rolling ignores the window: %+v", day.Rolling30d)
	}
	all := buildEvidenceOpensMerges(events, "", evidenceWindow{To: now}, now)
	if all.Mode != evidence.CadenceUnknown || all.Total.Merges != 2 || all.Status != evidence.OpensMergesBelow {
		t.Errorf("all time = %+v", all)
	}
	custom := buildEvidenceOpensMerges(events, "idle", evidenceWindow{Hours: 24, From: now.Add(-49 * time.Hour), To: now.Add(-48 * time.Hour), Custom: true}, now)
	if custom.Total.Opens != 0 || custom.Total.Merges != 0 || custom.Status != evidence.OpensMergesNoData {
		t.Errorf("custom end must be exclusive: %+v", custom.Total)
	}
	if s := (*Server)(nil); s.evidenceGovernorMode() != "" {
		t.Error("nil server must report no mode")
	}
	if s := (&Server{}); s.evidenceGovernorMode() != "" {
		t.Error("server without a governor must report no mode")
	}
}

// TestHandleEvidenceSummaryOpensMerges checks the summary handler ships
// issue_opens_pr_merges from the audit ring with the card graded alongside
// its siblings.
func TestHandleEvidenceSummaryOpensMerges(t *testing.T) {
	reviewEvidenceTestEnv(t)
	audit := &AuditLog{}
	audit.Log("system", ghpkg.AuditActionAgentIssueCreated, "repo=o/r, number=1", "")
	audit.Log("system", ghpkg.AuditActionHiveIssueCreated, "repo=o/r, number=2", "")
	audit.Log("system", ghpkg.AuditActionIssueClosed, "repo=o/r, number=1", "")
	s := &Server{audit: audit}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?hours=24&repo=o/r", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp struct {
		OpensMerges evidence.OpensMerges              `json:"issue_opens_pr_merges"`
		Cards       map[string]map[string]interface{} `json:"cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	om := resp.OpensMerges
	if om.Mode != evidence.CadenceUnknown || om.Total.Opens != 2 || om.Total.Merges != 0 || om.Total.Ratio != nil || om.Status != evidence.OpensMergesAbove {
		t.Errorf("issue_opens_pr_merges = %+v", om)
	}
	if len(om.Repos) != 1 || om.Repos[0].Repo != "o/r" || om.Repos[0].Opens != 2 {
		t.Errorf("repos = %+v", om.Repos)
	}
	if len(om.Rolling30dDays) != evidence.OpensMergesRollingDays || om.Rolling30d.Opens != 2 {
		t.Errorf("rolling = %+v (%d days)", om.Rolling30d, len(om.Rolling30dDays))
	}
	card, ok := resp.Cards[evidenceCardOpensMerges]
	if !ok || card["plain_summary"] == "" {
		t.Fatalf("cards.%s = %+v", evidenceCardOpensMerges, card)
	}
}
