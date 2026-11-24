package dashboard

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// An issue churn row (Check if finished) names the pull requests behind it:
// inline in the card reason, capped, and as card_churn_prs on the status,
// export and MCP rows, so a maintainer does not have to hunt for them.

// cardChurnReasonCode is an issue churn row's card reason code.
const cardChurnReasonCode = "verdict:" + contributorAdmissionReasonIssueChurn

func churnItem(merged, closed int, prs ...string) AdmissionWithheldItem {
	return AdmissionWithheldItem{Reason: contributorAdmissionReasonIssueChurn, ChurnMerged: merged, ChurnClosed: closed, ChurnPRs: prs}
}

func churnRefs(from, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "#"+strconv.Itoa(from+i))
	}
	return out
}

func TestIssueChurnCardReasonListsThePRs(t *testing.T) {
	const tail = " and it is still open; close it if it is done, or add hive: churn-triaged to give it back to agents."
	for _, tc := range []struct {
		name string
		item AdmissionWithheldItem
		want string
	}{
		{"merged only", churnItem(3, 0, "#1236", "#1240", "#1251"), "3 PRs merged (#1236, #1240, #1251)" + tail},
		{"merged and closed", churnItem(2, 2, "#5", "#6", "#7", "#8"), "2 PRs merged (#5, #6), 2 closed unmerged (#7, #8)" + tail},
		{"closed only", churnItem(0, 3, "#7", "#8", "#9"), "3 PRs closed unmerged (#7, #8, #9)" + tail},
		{"one merged", churnItem(1, 2, "#5", "#7", "#8"), "1 PR merged (#5), 2 closed unmerged (#7, #8)" + tail},
		{"cross repo", churnItem(2, 0, "#5", "octo/other#9"), "2 PRs merged (#5, octo/other#9)" + tail},
		{"cap", churnItem(14, 0, churnRefs(100, 14)...), "14 PRs merged (#100, #101, #102, #103, #104 and 9 more)" + tail},
		{"cap per list", churnItem(7, 7, churnRefs(100, 14)...), "7 PRs merged (#100, #101, #102, #103, #104 and 2 more), 7 closed unmerged (#107, #108, #109, #110, #111 and 2 more)" + tail},
		{"no list (an older verdict)", churnItem(3, 0), "3 PRs merged" + tail},
	} {
		got := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(cardTestStash(1, tc.item)))
		if got.Key != cardBandCheckFinished || got.ReasonCode != cardChurnReasonCode || !cardTurnAction(got.Key) {
			t.Errorf("%s: band %q code %q", tc.name, got.Key, got.ReasonCode)
		}
		if got.Reason != tc.want {
			t.Errorf("%s: reason\n got %q\nwant %q", tc.name, got.Reason, tc.want)
		}
		if strings.ContainsAny(got.Reason, "\u2014\u2013@") {
			t.Errorf("%s: reason has a dash or an at sign: %q", tc.name, got.Reason)
		}
		if len(got.ChurnPRs) != len(tc.item.ChurnPRs) {
			t.Errorf("%s: %d churn PRs on the band, want all %d", tc.name, len(got.ChurnPRs), len(tc.item.ChurnPRs))
		}
	}
}

func TestIssueChurnCardPRsCarryStateRepoAndLedgerFacts(t *testing.T) {
	mergedAt := cardTestNow.Add(-26 * time.Hour)
	stash := cardTestStash(1, churnItem(2, 1, "#5", "octo/other#9", "#7"))
	stash.addChurnMerged(func(repo string, number int) (MergedLedgerEntry, bool) {
		if strings.EqualFold(repo, cardTestRepo) && number == 5 {
			return MergedLedgerEntry{Repo: cardTestRepo, Number: 5, Title: "Fix the thing \u2014 part 2, thanks @alice", URL: "https://github.com/octo/demo/pull/5", MergedAt: mergedAt}, true
		}
		return MergedLedgerEntry{}, false
	})
	got := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(stash))
	want := []OverviewChurnPR{
		{Repo: cardTestRepo, Number: 5, State: github.PRStateMerged, Title: "Fix the thing; part 2, thanks alice", URL: "https://github.com/octo/demo/pull/5", MergedAt: mergedAt.UTC().Format(time.RFC3339)},
		{Repo: "octo/other", Number: 9, State: github.PRStateMerged},
		{Repo: cardTestRepo, Number: 7, State: github.PRStateClosed},
	}
	if !reflect.DeepEqual(got.ChurnPRs, want) {
		t.Fatalf("churn PRs = %+v\nwant %+v", got.ChurnPRs, want)
	}
	// The status row carries repo, number and state only.
	fields := got.rowFields()
	wantRow := []OverviewChurnPR{{Repo: cardTestRepo, Number: 5, State: "merged"}, {Repo: "octo/other", Number: 9, State: "merged"}, {Repo: cardTestRepo, Number: 7, State: "closed"}}
	if !reflect.DeepEqual(fields.CardChurnPRs, wantRow) {
		t.Errorf("row churn PRs = %+v, want %+v", fields.CardChurnPRs, wantRow)
	}
	// Every other band leaves the field off the row.
	other := decideIssueCard(cardIssueRow{repoFull: cardTestRepo, issue: cardIssue(1)}, cardTestInputs(cardTestStash(1, AdmissionWithheldItem{Reason: withheldReasonCooldown, ChurnPRs: []string{"#5"}})))
	if other.ChurnPRs != nil || other.rowFields().CardChurnPRs != nil {
		t.Errorf("a cooldown row carries churn PRs: %+v", other.ChurnPRs)
	}
}

// addChurnMerged asks the ledger only for the merged PRs of issue churn
// verdicts.
func TestCardStashAddChurnMergedAsksOnlyForMergedChurnPRs(t *testing.T) {
	st := newCardStash(cardTestNow)
	st.addAdmission(queueAdmissionSnapshot{withheld: []AdmissionWithheldItem{
		{Repo: cardTestRepo, Number: 40, Key: cardTestRepo + "#40", Reason: contributorAdmissionReasonIssueChurn, ChurnMerged: 1, ChurnClosed: 1, ChurnPRs: []string{"#41", "#42"}},
		{Repo: cardTestRepo, Number: 50, Key: cardTestRepo + "#50", Reason: withheldReasonCooldown, ChurnPRs: []string{"#51"}},
	}}, 1, cardTestNow)
	var asked []string
	st.addChurnMerged(func(repo string, number int) (MergedLedgerEntry, bool) {
		asked = append(asked, repo+"#"+strconv.Itoa(number))
		return MergedLedgerEntry{Repo: repo, Number: number, Title: "t"}, true
	})
	if !reflect.DeepEqual(asked, []string{cardTestRepo + "#41"}) {
		t.Errorf("asked the ledger for %v, want only the merged churn PR", asked)
	}
	if _, ok := st.churnMerged[cardStashKey(cardTestRepo, 41)]; !ok || len(st.churnMerged) != 1 {
		t.Errorf("churnMerged = %v", st.churnMerged)
	}
}

func churnServer(t *testing.T) (*Server, *StatusPayload) {
	t.Helper()
	s := &Server{}
	s.cardDispatch.load = func() (review.DispatchState, time.Time, error) { return review.DispatchState{}, time.Time{}, nil }
	s.cardDispatch.stat = func() time.Time { return time.Time{} }
	st := newCardStash(cardTestNow)
	st.addAdmission(queueAdmissionSnapshot{withheld: []AdmissionWithheldItem{
		{Repo: cardTestRepo, Number: 40, Key: cardTestRepo + "#40", Reason: contributorAdmissionReasonIssueChurn, ChurnMerged: 2, ChurnClosed: 1, ChurnPRs: []string{"#1236", "#1240", "#1251"}},
	}}, 1, cardTestNow)
	st.addChurnMerged(func(repo string, number int) (MergedLedgerEntry, bool) {
		if number == 1236 {
			return MergedLedgerEntry{Repo: cardTestRepo, Number: 1236, Title: "First slice", MergedAt: cardTestNow.Add(-48 * time.Hour)}, true
		}
		return MergedLedgerEntry{}, false
	})
	s.storeCardStash(st)
	status := &StatusPayload{
		HiveID:    "h1",
		Timestamp: cardTestNow.Format(time.RFC3339),
		Repos: []FrontendRepo{{
			Name: "demo", Full: cardTestRepo, Issues: 2,
			ActionableIssues: []any{
				github.Issue{Repo: cardTestRepo, Number: 40, Title: "Incremental refactor", CreatedAt: cardTestNow.Add(-480 * time.Hour), UpdatedAt: cardTestNow.Add(-time.Hour)},
				github.Issue{Repo: cardTestRepo, Number: 41, Title: "Plain", CreatedAt: cardTestNow.Add(-480 * time.Hour), UpdatedAt: cardTestNow.Add(-time.Hour)},
			},
		}},
	}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	return s, status
}

// The status row carries the compact list; the export and MCP rows carry
// every PR with its link, and the title and merge time the ledger lists.
func TestIssueChurnPRsOnStatusAndExportRows(t *testing.T) {
	s, status := churnServer(t)
	served := s.statusWithOverviewBands(status, cardTestNow)
	raw, err := json.Marshal(served.Repos[0].ActionableIssues)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	wantStatus := []any{
		map[string]any{"repo": cardTestRepo, "number": float64(1236), "state": "merged"},
		map[string]any{"repo": cardTestRepo, "number": float64(1240), "state": "merged"},
		map[string]any{"repo": cardTestRepo, "number": float64(1251), "state": "closed"},
	}
	if rows[0]["card_band"] != cardBandCheckFinished || !reflect.DeepEqual(rows[0]["card_churn_prs"], wantStatus) {
		t.Errorf("status churn row: band %v, card_churn_prs %#v", rows[0]["card_band"], rows[0]["card_churn_prs"])
	}
	if _, ok := rows[1]["card_churn_prs"]; ok {
		t.Errorf("a row without churn carries card_churn_prs on the status: %v", rows[1])
	}

	var export overviewJSONResponse
	if err := json.Unmarshal(cardExport(t, s, overviewKindIssues, "json", "").Body.Bytes(), &export); err != nil {
		t.Fatal(err)
	}
	byNumber := map[float64]map[string]any{}
	for _, row := range export.Rows {
		m, _ := row.(map[string]any)
		n, _ := m["number"].(float64)
		byNumber[n] = m
	}
	churn := byNumber[40]
	if churn == nil {
		t.Fatalf("export rows = %v", export.Rows)
	}
	wantExport := []any{
		map[string]any{"repo": cardTestRepo, "number": float64(1236), "state": "merged", "title": "First slice", "url": "https://github.com/octo/demo/pull/1236", "merged_at": cardTestNow.Add(-48 * time.Hour).Format(time.RFC3339)},
		map[string]any{"repo": cardTestRepo, "number": float64(1240), "state": "merged", "url": "https://github.com/octo/demo/pull/1240"},
		map[string]any{"repo": cardTestRepo, "number": float64(1251), "state": "closed", "url": "https://github.com/octo/demo/pull/1251"},
	}
	if !reflect.DeepEqual(churn["card_churn_prs"], wantExport) {
		t.Errorf("export card_churn_prs = %#v\nwant %#v", churn["card_churn_prs"], wantExport)
	}
	if want := "2 PRs merged (#1236, #1240), 1 closed unmerged (#1251) and it is still open; close it if it is done, or add hive: churn-triaged to give it back to agents."; churn["card_reason"] != want {
		t.Errorf("export card_reason = %q, want %q", churn["card_reason"], want)
	}
	if plain := byNumber[41]; plain == nil || !reflect.DeepEqual(plain["card_churn_prs"], []any{}) {
		t.Errorf("a row without churn: card_churn_prs = %#v, want []", plain["card_churn_prs"])
	}

	csv := cardExport(t, s, overviewKindIssues, "csv", "").Body.String()
	lines := strings.Split(strings.TrimPrefix(csv, "\ufeff"), "\r\n")
	if !strings.HasSuffix(lines[0], ",card_reason,card_reason_code,card_churn_prs") {
		t.Errorf("issue CSV header = %q", lines[0])
	}
	if !strings.Contains(csv, "octo/demo#1236 merged https://github.com/octo/demo/pull/1236;octo/demo#1240 merged https://github.com/octo/demo/pull/1240;octo/demo#1251 closed https://github.com/octo/demo/pull/1251") {
		t.Errorf("issue CSV lacks the churn PR cell:\n%s", csv)
	}
}

// An issue churn row is Check if finished, not Yours to close or Triage,
// and the card_band your-turn alias still returns it: the single Your turn
// band of before the split held it, and Check if finished is a what-to-do
// band. The alias returns no Yours to close row.
func TestIssueChurnRowIsCheckIfFinishedAndInTheYourTurnAlias(t *testing.T) {
	s, status := churnServer(t)
	verified := github.Issue{Repo: cardTestRepo, Number: 42, Title: "Done", CreatedAt: cardTestNow.Add(-480 * time.Hour), UpdatedAt: cardTestNow.Add(-time.Hour)}
	status.Repos[0].ActionableIssues = append(status.Repos[0].ActionableIssues, verified)
	status.Repos[0].Issues = 3
	status.Repos[0].Ownership = map[string]Ownership{ownershipKey(ownershipKindIssue, 42): yourTurn(OwnershipReasonVerifiedDone, "reason")}
	numbers := func(query string) []int {
		t.Helper()
		w := cardExport(t, s, overviewKindIssues, "json", query)
		var body overviewJSONResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v (%s)", query, err, w.Body.String())
		}
		var out []int
		for _, row := range body.Rows {
			m, _ := row.(map[string]any)
			n, _ := m["number"].(float64)
			out = append(out, int(n))
		}
		return out
	}
	for query, want := range map[string][]int{
		"?card_band=check-finished":                     {40},
		"?card_band=to-close":                           {42},
		"?card_band=triage":                             nil,
		"?card_band=your-turn":                          {40},
		"?card_band=your-turn&card_band=to-close":       {40, 42},
		"?card_band=your-turn&card_band=check-finished": {40},
		"?card_band=your-turn&card_group=outside":       nil,
		"?card_group=your-turn":                         {40, 42},
	} {
		if got := numbers(query); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %v, want %v", query, got, want)
		}
	}
}

// The repo card folds an issue churn row's PRs under its pill as links,
// merged ones solid and closed-unmerged ones dashed, hidden with the row.
func TestRepoCardIssueChurnPRLinks(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "${issueChurnPRsHTML(i, repoFull, cardForge, repoPillFilterHiddenClass(rowKinds, pillFilter))}") {
		t.Error("renderIssuePill does not fold the churn PRs under the issue pill")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: churn PR links were not executed")
	}
	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\nglobalThis.window = { _githubBaseUrl: 'https://ghe.example/' };\n")
	for _, name := range []string{"esc", "issueChurnPRsHTML"} {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
assert.equal(issueChurnPRsHTML({}, 'octo/demo'), '');
assert.equal(issueChurnPRsHTML({ card_churn_prs: [] }, 'octo/demo'), '');
const forge = { base: 'https://github.com', changePath: '/pull' };
const html = issueChurnPRsHTML({ number: 40, card_churn_prs: [
  { repo: 'octo/demo', number: 1236, state: 'merged' },
  { repo: 'octo/other', number: 9, state: 'merged' },
  { repo: 'octo/demo', number: 1251, state: 'closed' },
] }, 'octo/demo', forge, ' repo-pill-row-filtered-hidden');
assert.match(html, /^<details class="cp-dependents repo-pill-row-filtered-hidden"><summary>PRs on this issue: 2 merged, 1 closed unmerged<\/summary>/);
assert.match(html, /<a class="cp-chip" href="https:\/\/github.com\/octo\/demo\/pull\/1236" target="_blank" rel="noopener" title="#1236 merged">#1236<\/a>/);
assert.match(html, /<a class="cp-chip" href="https:\/\/github.com\/octo\/other\/pull\/9"[^>]*>octo\/other#9<\/a>/);
assert.match(html, /<a class="cp-chip soft" href="https:\/\/github.com\/octo\/demo\/pull\/1251"[^>]*title="#1251 closed unmerged">#1251<\/a>/);
assert.equal((html.match(/<a /g) || []).length, 3);
// No forge: the GitHub base URL; an http(s) server link wins and is
// escaped; any other scheme falls back to the built link.
const bare = issueChurnPRsHTML({ card_churn_prs: [{ repo: 'octo/demo', number: 5, state: 'merged' }, { repo: 'octo/demo', number: 6, state: 'merged', url: 'https://x/"><b>' }, { repo: 'octo/demo', number: 7, state: 'merged', url: 'javascript:alert(1)' }] }, 'octo/demo');
assert.match(bare, /href="https:\/\/ghe.example\/octo\/demo\/pull\/5"/);
assert.match(bare, /href="https:\/\/x\/&quot;&gt;&lt;b&gt;"/);
assert.doesNotMatch(bare, /"><b>/);
assert.doesNotMatch(bare, /javascript:/);
assert.match(bare, /href="https:\/\/ghe.example\/octo\/demo\/pull\/7"/);
assert.match(bare, /<summary>PRs on this issue: 3 merged<\/summary>/);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("churn PR links check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
