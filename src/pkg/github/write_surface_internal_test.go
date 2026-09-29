package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/review"
)

// #9587 phase 2: the hive-internal writers are audited through the same typed,
// redacted path as the relay writes, and write sites pass repo/target
// explicitly.

// assertTypedWrite fails unless recs holds exactly want entries for action and
// the first carries the typed repo/target, the legacy detail pairs that agree
// with them, the governor agent, and no credential material.
func assertTypedWrite(t *testing.T, recs []AuditRecord, action, repo string, target, want int) AuditRecord {
	t.Helper()
	var got []AuditRecord
	for _, r := range recs {
		if r.Action == action {
			got = append(got, r)
		}
	}
	if len(got) != want {
		t.Fatalf("%s: got %d audit entries, want %d (all: %+v)", action, len(got), want, recs)
	}
	if want == 0 {
		return AuditRecord{}
	}
	rec := got[0]
	if rec.Repo != repo || rec.Target != target {
		t.Errorf("%s: typed repo/target = %q/%d, want %q/%d", action, rec.Repo, rec.Target, repo, target)
	}
	if rec.Agent != AttributionAgentGovernor {
		t.Errorf("%s: agent = %q, want %q", action, rec.Agent, AttributionAgentGovernor)
	}
	wantPrefix := "repo=" + repo
	if target > 0 {
		wantPrefix += fmt.Sprintf(", number=%d", target)
	}
	if !strings.HasPrefix(rec.Detail, wantPrefix) {
		t.Errorf("%s: detail %q must start with the legacy pairs %q", action, rec.Detail, wantPrefix)
	}
	assertNoCredentialMaterial(t, rec)
	return rec
}

// assertNoCredentialMaterial fails if any credential sample's secret appears in
// the record's detail or typed repo.
func assertNoCredentialMaterial(t *testing.T, rec AuditRecord) {
	t.Helper()
	for _, s := range credentialSamples {
		if strings.Contains(rec.Detail, s.secret) || strings.Contains(rec.Repo, s.secret) {
			t.Errorf("%s leaked %s: repo=%q detail=%q", rec.Action, s.name, rec.Repo, rec.Detail)
		}
	}
}

// ---------- recordWriteAudit ----------

func TestWriteAuditRecord_TypedFieldsComeFromTarget(t *testing.T) {
	rec := writeAuditRecord(AuditActionHiveLabelApplied, InvocationMeta{Agent: "governor"},
		WriteTarget{Repo: " o/r ", Number: 12}, "label", "needs-human")
	if rec.Repo != "o/r" || rec.Target != 12 {
		t.Fatalf("typed = %q/%d, want o/r/12", rec.Repo, rec.Target)
	}
	if rec.Detail != "repo=o/r, number=12, label=needs-human, agent=governor" {
		t.Fatalf("detail = %q; legacy pairs must come first, in the historical order", rec.Detail)
	}
}

// The typed field and the detail pair must never disagree, so a stray repo= or
// number= pair in extra is dropped rather than written next to target's.
func TestWriteAuditRecord_DropsConflictingExtraPairs(t *testing.T) {
	rec := writeAuditRecord(AuditActionFleetReportPosted, InvocationMeta{Agent: "governor"},
		WriteTarget{Repo: "o/r", Number: 5}, "repo", "evil/other", "number", "999", "outcome", "created")
	if strings.Contains(rec.Detail, "evil/other") || strings.Contains(rec.Detail, "999") {
		t.Fatalf("conflicting extra pair reached the detail: %q", rec.Detail)
	}
	if strings.Count(rec.Detail, "repo=") != 1 || strings.Count(rec.Detail, "number=") != 1 {
		t.Fatalf("want exactly one repo= and one number= pair: %q", rec.Detail)
	}
	if rec.Repo != "o/r" || rec.Target != 5 {
		t.Fatalf("typed = %q/%d, want o/r/5", rec.Repo, rec.Target)
	}
}

// A write with no numbered target (Number 0 or negative) records no number
// pair and a zero typed target, the same shape a refused open_pr always had.
func TestWriteAuditRecord_NoTargetOmitsNumber(t *testing.T) {
	for _, n := range []int{0, -3} {
		rec := writeAuditRecord(AuditActionAgentWriteRefused, InvocationMeta{Agent: "scanner"},
			WriteTarget{Repo: "o/r", Number: n}, "op", WriteOpOpenPR)
		if rec.Target != 0 || strings.Contains(rec.Detail, "number=") {
			t.Fatalf("Number %d: target=%d detail=%q, want no target", n, rec.Target, rec.Detail)
		}
	}
}

func TestRecordWriteAudit_RedactsEverySlot(t *testing.T) {
	for _, s := range credentialSamples {
		t.Run(s.name, func(t *testing.T) {
			c := testClient(t, "http://127.0.0.1:1")
			recs := captureAudit(c)
			c.recordWriteAudit(AuditActionSignedCommitSkipNoted, hiveWriteMeta(),
				WriteTarget{Repo: "o/" + s.text, Number: 3}, "reason", "commit carried "+s.text)
			if len(*recs) != 1 {
				t.Fatalf("got %d records, want 1", len(*recs))
			}
			rec := (*recs)[0]
			assertNoCredentialMaterial(t, rec)
			if rec.Target != 3 {
				t.Errorf("redaction disturbed the typed target: %d", rec.Target)
			}
		})
	}
}

// A nil client must be a no-op, like every other audit entry point.
func TestRecordWriteAudit_NilClient(t *testing.T) {
	var c *Client
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(), WriteTarget{Repo: "o/r", Number: 1})
	c.deliverAuditRecord(AuditRecord{Action: AuditActionHiveLabelApplied})
}

// With no typed sink the legacy sink still receives the entry, and with no
// sink at all the entry goes to the hive log without panicking.
func TestRecordWriteAudit_LegacySinkAndLogFallback(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	var got []string
	c.SetAttributionAudit(func(action, detail, agent string) { got = append(got, action+"|"+detail+"|"+agent) })
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(), WriteTarget{Repo: "o/r", Number: 2}, "label", "l")
	if len(got) != 1 || got[0] != AuditActionHiveLabelApplied+"|repo=o/r, number=2, label=l, agent=governor|governor" {
		t.Fatalf("legacy sink got %q", got)
	}

	bare := testClient(t, "http://127.0.0.1:1")
	bare.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(), WriteTarget{Repo: "o/r", Number: 2})
}

func TestInternalWriteAuditActionsAreDistinctAndDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "github-write-surface.md"))
	if err != nil {
		t.Fatalf("reading the write-surface inventory: %v", err)
	}
	doc := string(raw)
	seen := map[string]bool{}
	for _, a := range InternalWriteAuditActions() {
		if seen[a] {
			t.Errorf("duplicate audit action %q", a)
		}
		seen[a] = true
		if !strings.Contains(doc, "`"+a+"`") {
			t.Errorf("docs/github-write-surface.md does not list audit action %q", a)
		}
	}
	if strings.Contains(doc, "| no |") || strings.Contains(doc, "| no (") {
		t.Error("the inventory still marks a write as unaudited")
	}
}

// ---------- the six writers ----------

func TestApplyHumanDecisionLabel_Audited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/labels/queue-triage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"queue-triage"}`))
	})
	mux.HandleFunc("/repos/acme/widget/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"queue-triage"}]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "acme", []string{"widget"})
	recs := captureAudit(c)

	if err := c.ApplyHumanDecisionLabel(context.Background(), "widget", 42, "queue-triage"); err != nil {
		t.Fatalf("ApplyHumanDecisionLabel: %v", err)
	}
	rec := assertTypedWrite(t, *recs, AuditActionHiveLabelApplied, "acme/widget", 42, 1)
	if !strings.Contains(rec.Detail, "label=queue-triage") {
		t.Errorf("detail must name the label: %q", rec.Detail)
	}
}

// A label the repo does not define is never applied, so nothing is audited.
func TestApplyHumanDecisionLabel_MissingLabelNotAudited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()
	c := newTestClient(t, server, "acme", []string{"widget"})
	recs := captureAudit(c)
	_ = c.ApplyHumanDecisionLabel(context.Background(), "acme/widget", 42, "typo")
	assertTypedWrite(t, *recs, AuditActionHiveLabelApplied, "", 0, 0)
}

func TestPostRecommendations_AuditedOnCreateAndUpdateOnly(t *testing.T) {
	org, repo := "testorg", "testrepo"

	srv := newRecServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})
	recs := captureAudit(c)
	if _, err := c.PostRecommendations(context.Background(), repo, recTestTitle, "**1 ready.**", nil, true); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := assertTypedWrite(t, *recs, AuditActionRecommendationsPosted, org+"/"+repo, 77, 1)
	if !strings.Contains(rec.Detail, "outcome="+auditOutcomeCreated) {
		t.Errorf("create detail = %q", rec.Detail)
	}

	upd := newRecServer(t, org, repo)
	upd.existing = []map[string]any{{"number": 42, "title": recTestTitle, "body": "old"}}
	c = newTestClient(t, upd.Server, org, []string{repo})
	recs = captureAudit(c)
	if _, err := c.PostRecommendations(context.Background(), repo, recTestTitle, "new", nil, true); err != nil {
		t.Fatalf("update: %v", err)
	}
	rec = assertTypedWrite(t, *recs, AuditActionRecommendationsPosted, org+"/"+repo, 42, 1)
	if !strings.Contains(rec.Detail, "outcome="+auditOutcomeUpdated) {
		t.Errorf("update detail = %q", rec.Detail)
	}

	// An unchanged body is not rewritten, so it is not audited either.
	same := newRecServer(t, org, repo)
	same.existing = []map[string]any{{"number": 42, "title": recTestTitle, "body": "same"}}
	c = newTestClient(t, same.Server, org, []string{repo})
	recs = captureAudit(c)
	if _, err := c.PostRecommendations(context.Background(), repo, recTestTitle, "same", nil, true); err != nil {
		t.Fatalf("unchanged: %v", err)
	}
	assertTypedWrite(t, *recs, AuditActionRecommendationsPosted, "", 0, 0)
}

func TestEnsureFleetReport_AuditedOnCommentWithRedactedFingerprint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 7}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/reactions":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recs := captureAudit(c)
	// The fingerprint is recorded; a credential inside it must not be.
	fp := "fp " + credentialSamples[0].text
	if _, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: fp, Body: "evidence"}); err != nil {
		t.Fatal(err)
	}
	rec := assertTypedWrite(t, *recs, AuditActionFleetReportPosted, fleetReportRepo, 7, 1)
	if !strings.Contains(rec.Detail, "outcome="+auditOutcomeCommented) || !strings.Contains(rec.Detail, "reaction=true") {
		t.Errorf("detail = %q", rec.Detail)
	}
}

func TestEnsureFleetReport_AuditedOnCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/labels":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "fleet-report"})
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/hivecommons/hive/issues" || r.URL.Path == "/repos/hivecommons/hive/issues/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9", "user": map[string]any{"login": "bot"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recs := captureAudit(c)
	if _, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: "fp", Title: "Fleet report", Body: "<!-- hive-fleet-fingerprint:fp -->\nbody", Labels: []string{"fleet-report"}}); err != nil {
		t.Fatal(err)
	}
	rec := assertTypedWrite(t, *recs, AuditActionFleetReportPosted, fleetReportRepo, 9, 1)
	if !strings.Contains(rec.Detail, "outcome="+auditOutcomeCreated) {
		t.Errorf("detail = %q", rec.Detail)
	}
}

func TestPostFleetReportRecovery_AuditedWithCloseOutcome(t *testing.T) {
	closeStatus := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/5/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/5":
			w.WriteHeader(closeStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, tc := range []struct {
		name       string
		owner      bool
		status     int
		wantClosed string
		wantErr    bool
	}{
		{"not owner", false, http.StatusOK, "closed=false", false},
		{"owner closes", true, http.StatusOK, "closed=true", false},
		{"close fails", true, http.StatusInternalServerError, "closed=false", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeStatus = tc.status
			recs := captureAudit(c)
			err := c.PostFleetReportRecovery(context.Background(), 5, fleetreport.Report{Fingerprint: "fp", Body: "recovered"}, tc.owner)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			rec := assertTypedWrite(t, *recs, AuditActionFleetReportRecovered, fleetReportRepo, 5, 1)
			if !strings.Contains(rec.Detail, tc.wantClosed) {
				t.Errorf("detail %q, want %s", rec.Detail, tc.wantClosed)
			}
		})
	}
}

func TestMigrateHiveHoldLabel_AuditsEachLabelAdded(t *testing.T) {
	org, repo := "testorg", "testrepo"
	oldLabel, newLabel := "hive/h1", "hive-pause/h1"
	issues := []wireIssue{
		{Number: 1, Title: "audit hold", User: wireUser{"alice"}, Labels: []wireLabel{{Name: oldLabel}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "provenance", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: oldLabel}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(2)},
	}
	added := map[int][]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/"+newLabel, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		_ = json.NewEncoder(w).Encode(label)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/1/labels", addLabelsHandler(t, added, 1))
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/2/events", func(w http.ResponseWriter, r *http.Request) {
		writeEvents(t, w, oldLabel, "agent/scanner", time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	// A typed-only hold entry (no repo=/number= in detail): the migration's
	// reader must still find it.
	line := `{"ts":"2026-09-25T10:51:15Z","action":"repo_item_hold_add","detail":"label=hive/h1","repo":"testorg/testrepo","target":1}` + "\n"
	if err := os.WriteFile(auditPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, server, org, []string{repo})
	recs := captureAudit(c)
	report, err := c.MigrateHiveHoldLabel(context.Background(), HiveHoldMigrationOptions{
		HiveID: "h1", AuditPath: auditPath, DataDir: dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"), ReportPath: filepath.Join(dir, "report.json"),
	})
	if err != nil {
		t.Fatalf("MigrateHiveHoldLabel: %v", err)
	}
	if report.Summary.AuditHolds != 1 {
		t.Fatalf("typed-only audit hold was not read: %+v", report.Summary)
	}
	rec := assertTypedWrite(t, *recs, AuditActionHoldMigrationLabelAdded, org+"/"+repo, 1, 1)
	if !strings.Contains(rec.Detail, "label="+newLabel) || !strings.Contains(rec.Detail, "migration="+HiveHoldMigrationID) {
		t.Errorf("detail = %q", rec.Detail)
	}
}

// Reader compatibility: typed fields win, legacy detail pairs still work, and
// an entry with neither is ignored.
func TestAuditItemRef_TypedFirstThenLegacy(t *testing.T) {
	cases := []struct {
		name       string
		entry      hiveHoldAuditEntry
		wantRepo   string
		wantNumber int
	}{
		{"typed only", hiveHoldAuditEntry{Detail: "label=x", Repo: "o/r", Target: 4}, "o/r", 4},
		{"legacy only", hiveHoldAuditEntry{Detail: "repo=o/r, number=5, label=x"}, "o/r", 5},
		{"typed wins", hiveHoldAuditEntry{Detail: "repo=o/old, number=9", Repo: "o/r", Target: 6}, "o/r", 6},
		{"neither", hiveHoldAuditEntry{Detail: "label=x"}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, number := auditItemRef(tc.entry, parseAuditDetail(tc.entry.Detail))
			if repo != tc.wantRepo || number != tc.wantNumber {
				t.Fatalf("got %q/%d, want %q/%d", repo, number, tc.wantRepo, tc.wantNumber)
			}
		})
	}
}

func TestSignedReconcile_ReauthorIsAudited(t *testing.T) {
	m, c := reconcileFixture(t, true)
	recs := captureAudit(c)
	m.mergeBase = verifiedSHA
	m.prCommits = []map[string]any{
		prCommitJSON(verifiedSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
		prCommitJSON(agentSHA, false, "", "quality@hive.kubestellar.io"),
		prCommitJSON(m.headSHA, false, "", "4744647+hive-app[bot]@users.noreply.github.com"),
	}

	c.reconcileSignedCommits(context.Background())

	rec := assertTypedWrite(t, *recs, AuditActionSignedCommitReauthored, "o/r", 77, 1)
	for _, want := range []string{"branch=" + reconcileBranch, "base=" + verifiedSHA, "commit=" + reconcileOID, "replaced_commits="} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("detail %q missing %q", rec.Detail, want)
		}
	}
	assertTypedWrite(t, *recs, AuditActionSignedCommitSkipNoted, "", 0, 0)
}

func TestSignedReconcile_SkipNoteIsAuditedOnce(t *testing.T) {
	m, c := reconcileFixture(t, true)
	recs := captureAudit(c)
	m.prCommits = []map[string]any{
		prCommitJSON(verifiedSHA, true, reconcileBot, "1+hive-app[bot]@users.noreply.github.com"),
		prCommitJSON(m.headSHA, false, "alice", "alice@example.com"),
	}

	c.reconcileSignedCommits(context.Background())
	// Same head again: settled, so no second comment and no second entry.
	c.reconcileSignedCommits(context.Background())

	rec := assertTypedWrite(t, *recs, AuditActionSignedCommitSkipNoted, "o/r", 77, 1)
	if !strings.Contains(rec.Detail, "reason=") {
		t.Errorf("detail must carry the reason: %q", rec.Detail)
	}
	assertTypedWrite(t, *recs, AuditActionSignedCommitReauthored, "", 0, 0)
}

func TestReviewBacklog_FiledIssuesAndSummaryAudited(t *testing.T) {
	dir := withReviewDir(t)
	stateDir := t.TempDir()
	oldBacklog, oldLinks, oldReportDir := ReviewBacklogPath, ReviewLinksPath, review.DefaultReportDir
	ReviewBacklogPath = filepath.Join(stateDir, reviewBacklogFile)
	ReviewLinksPath = filepath.Join(stateDir, ReviewLinksFile)
	review.DefaultReportDir = filepath.Join(stateDir, "reports")
	t.Cleanup(func() {
		ReviewBacklogPath, ReviewLinksPath, review.DefaultReportDir = oldBacklog, oldLinks, oldReportDir
	})
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "reviewer"},
	}})

	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/pulls/5/reviews":
			_, _ = io.WriteString(w, `{"id":1,"state":"COMMENTED","html_url":"https://github.test/o/r/pull/5#pullrequestreview-1","commit_id":"abc"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/labels":
			_, _ = io.WriteString(w, `{"name":"from-review"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues":
			created++
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.test/o/r/issues/%d"}`, 100+created, 100+created)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/5/comments":
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 2 })
	recs := captureAudit(c)

	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Body: "out-of-scope only", Agent: "reviewer", Report: reviewBacklogFixtureReport(t)}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	rec := assertTypedWrite(t, *recs, AuditActionReviewBacklogIssueFiled, "o/r", 101, 2)
	for _, want := range []string{"pr=5", "perspective=correctness", "reused=false", "review_agent=reviewer"} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("filed-issue detail %q missing %q", rec.Detail, want)
		}
	}
	rec = assertTypedWrite(t, *recs, AuditActionReviewBacklogSummaryPosted, "o/r", 5, 1)
	if !strings.Contains(rec.Detail, "issues=2") {
		t.Errorf("summary detail %q missing issues=2", rec.Detail)
	}
}
