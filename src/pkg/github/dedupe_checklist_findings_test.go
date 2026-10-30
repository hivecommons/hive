package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The real body of hivecommons/hive#15381: a dedupe pass that found 0
// near-duplicate pairs and 0 missing links, filed anyway on the strength of
// a merge-order reminder and a cross-link note, and shown as stalled work
// for hours.
func dedupeChecklist15381(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dedupe_checklist_15381.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const dedupeChecklistActionable = `<!-- hive:dedupe-checklist repo=o/r -->
Decision required: yes

## Summary

Found 1 near-duplicate pairs, 0 same-root-cause clusters, 1 items that appear already resolved, and 2 missing PR↔issue links.

## Near-duplicates (close one)

- [ ] #12 duplicates #10 — close #12

## Already resolved

- [ ] #20 — fixed by merged PR #19

## Missing links

- [ ] PR #30 → add ` + "`Fixes #29`" + `
- [ ] PR #31 → add ` + "`Fixes #28`" + `
`

func TestParseDedupeChecklistFindings(t *testing.T) {
	f, ok := ParseDedupeChecklistFindings(dedupeChecklist15381(t))
	if !ok {
		t.Fatal("#15381 body not recognised as a dedupe checklist")
	}
	want := DedupeChecklistFindings{Repo: "hivecommons/hive", Clusters: 1}
	if f != want {
		t.Errorf("#15381 findings = %+v, want %+v", f, want)
	}
	if f.Actionable() {
		t.Error("#15381 (0 pairs, 0 missing links, no task lines, decision no) must not be actionable")
	}
	if !strings.Contains(f.Summary(), "0 near-duplicate pair(s)") || !strings.Contains(f.Summary(), "decision required: no") {
		t.Errorf("summary = %q", f.Summary())
	}

	f, ok = ParseDedupeChecklistFindings(dedupeChecklistActionable)
	if !ok {
		t.Fatal("actionable body not recognised")
	}
	want = DedupeChecklistFindings{Repo: "o/r", DecisionRequired: true, NearDuplicates: 1, Resolved: 1, MissingLinks: 2, TaskItems: 4}
	if f != want {
		t.Errorf("actionable findings = %+v, want %+v", f, want)
	}
	if !f.Actionable() {
		t.Error("a checklist with task lines must be actionable")
	}

	// Each single signal is enough on its own; the template quoted in a
	// fence is not.
	for name, body := range map[string]string{
		"task line only":     "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\nFound 0 near-duplicate pairs.\n\n- [ ] #5 → add `Fixes #4`\n",
		"verdict yes only":   "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\nSee above.\n",
		"missing links only": "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\nFound 0 near-duplicate pairs and 3 missing PR↔issue links.\n",
	} {
		if f, ok := ParseDedupeChecklistFindings(body); !ok || !f.Actionable() {
			t.Errorf("%s: ok=%v actionable=%v (%+v)", name, ok, f.Actionable(), f)
		}
	}
	fenced := "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\nFound 0 near-duplicate pairs.\n\n```\n- [ ] #5 example\nFound 2 missing PR↔issue links\n```\n"
	if f, _ := ParseDedupeChecklistFindings(fenced); f.Actionable() {
		t.Errorf("fenced template counted as findings: %+v", f)
	}
	if _, ok := ParseDedupeChecklistFindings("## Summary\n\nFound 0 near-duplicate pairs."); ok {
		t.Error("a body without the marker is not a checklist")
	}
}

func TestIssueAdvisoryFromBody(t *testing.T) {
	got := IssueAdvisoryFromBody(dedupeChecklist15381(t))
	if got == nil || !got.DedupeChecklist || !got.DecisionNo || !got.Empty {
		t.Errorf("#15381 advisory = %+v, want checklist+decision-no+empty", got)
	}
	got = IssueAdvisoryFromBody(dedupeChecklistActionable)
	if got == nil || !got.DedupeChecklist || got.DecisionNo || got.Empty {
		t.Errorf("actionable advisory = %+v, want checklist, decision yes, not empty", got)
	}
	got = IssueAdvisoryFromBody("Some lane's note\nDecision required: no\n\nFYI only.")
	if got == nil || got.DedupeChecklist || !got.DecisionNo || got.Empty {
		t.Errorf("hand-written advisory = %+v, want decision-no only", got)
	}
	if IssueAdvisoryFromBody("Please fix the relay.") != nil || IssueAdvisoryFromBody("") != nil {
		t.Error("ordinary bodies are not advisories")
	}
}

// The relay refuses `issue-create` for an empty dedupe checklist, records
// the audit + a bead, and consumes the request; an actionable checklist is
// filed as before.
func TestIssueRequestWatcher_EmptyDedupeAdvisoryRefused(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantCreated bool
	}{
		{"#15381 body is refused", "", false},
		{"actionable checklist is filed", dedupeChecklistActionable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == "" {
				body = dedupeChecklist15381(t)
			}
			var bodies []string
			srv := newPriorArtIssueMockServer(t, &bodies)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)

			var audits []AuditRecord
			c.SetAttributionAuditRecord(func(rec AuditRecord) {
				if rec.Action == AuditActionAgentIssueEmptyAdvisoryRefused {
					audits = append(audits, rec)
				}
			})
			var beads []priorArtBead
			c.SetPriorArtFilingGate(nil, nil, func(title, detail, agent, ref string) {
				beads = append(beads, priorArtBead{title, detail, agent, ref})
			})

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Repo: "o/r", Title: "triage: duplicate and overlapping open issues/PRs to consolidate", Body: body,
				Labels: []string{"agent/dedupe", "triage/advisory"}, Agent: "dedupe",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if got := len(bodies) == 1; got != tc.wantCreated {
				t.Fatalf("created = %v (%d creates), want %v", got, len(bodies), tc.wantCreated)
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Errorf("request file should be consumed")
			}
			var res IssueResponse
			b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
			if err != nil {
				t.Fatalf("result file missing: %v", err)
			}
			if err := json.Unmarshal(b, &res); err != nil {
				t.Fatal(err)
			}
			if res.OK != tc.wantCreated {
				t.Errorf("result OK = %v, want %v (%+v)", res.OK, tc.wantCreated, res)
			}
			if tc.wantCreated {
				if len(audits) != 0 || len(beads) != 0 {
					t.Errorf("actionable checklist audited/beaded as empty: %d/%d", len(audits), len(beads))
				}
				return
			}
			for _, want := range []string{"not filed", "no actionable findings", "0 near-duplicate pair(s)", "0 missing PR↔issue link(s)", "kick output"} {
				if !strings.Contains(res.Error, want) {
					t.Errorf("result error %q missing %q", res.Error, want)
				}
			}
			if len(audits) != 1 || audits[0].Agent != "dedupe" || audits[0].Repo != "o/r" || !strings.Contains(audits[0].Detail, "0 near-duplicate") {
				t.Errorf("audits = %+v", audits)
			}
			if len(beads) != 1 || beads[0].agent != "dedupe" || beads[0].ref != "o/r" || !strings.Contains(beads[0].title, "found nothing to act on") {
				t.Errorf("beads = %+v", beads)
			}
		})
	}
}
