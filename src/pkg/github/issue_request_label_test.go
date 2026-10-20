package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

// newLabelMockServer records the label adds and removes a label request makes.
func newLabelMockServer(t *testing.T, added *[]string, removed *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			// Label definitions already exist; creation is covered by newEnsureLabelMockServer.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if added != nil {
				*added = append(*added, body...)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"name":"x"}]`)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			if removed != nil {
				idx := strings.LastIndex(r.URL.Path, "/labels/")
				*removed = append(*removed, r.URL.Path[idx+len("/labels/"):])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"body":"ok"}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/"):
			// The re-park gate reads the issue before parking it on a
			// decision; a bare issue carries no approval.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":1,"labels":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func readIssueResultFile(t *testing.T, reqPath string) IssueResponse {
	t.Helper()
	data, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	var resp IssueResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	return resp
}

// A "label" request adds and removes plain labels and audits the change with
// typed repo/target (#9587).
func TestIssueRequestWatcher_LabelAddsRemovesAndAudits(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 42, Agent: "scanner",
		Labels: []string{"bug", "bug", " area/proxy "}, RemoveLabels: []string{"stale"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := strings.Join(added, ","); got != "bug,area/proxy" {
		t.Errorf("labels added = %q, want %q (trimmed, de-duplicated, in order)", got, "bug,area/proxy")
	}
	if got := strings.Join(removed, ","); got != "stale" {
		t.Errorf("labels removed = %q, want %q", got, "stale")
	}
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.Number != 42 {
		t.Fatalf("result = %+v, want ok on #42", resp)
	}
	if strings.Join(resp.LabelsAdded, ",") != "bug,area/proxy" || strings.Join(resp.LabelsRemoved, ",") != "stale" {
		t.Errorf("result does not report what changed: %+v", resp)
	}
	rec, ok := findAudit(*recs, AuditActionAgentLabelApplied)
	if !ok {
		t.Fatalf("label write was not audited; records: %+v", *recs)
	}
	if rec.Repo != "o/r" || rec.Target != 42 || rec.Agent != "scanner" {
		t.Errorf("audit typed fields = %+v, want repo=o/r target=42 agent=scanner", rec)
	}
	if !strings.Contains(rec.Detail, "added=bug area/proxy") || !strings.Contains(rec.Detail, "removed=stale") {
		t.Errorf("audit detail = %q, want the added and removed labels", rec.Detail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("a fulfilled label request should be consumed")
	}
}

// Hive-controlled labels are refused before any GitHub call, whatever the
// lane allowlist says: they decide merges, holds and ownership.
func TestIssueRequestWatcher_LabelRefusesHiveControlledLabels(t *testing.T) {
	for _, label := range []string{
		AutoMergeQueuedLabel, "LGTM", "hold", "on-hold", HumanAckLabel,
		"design-approved", "needs-decision", "needs-direction", "blocked",
		"hive/claimed-by-other", "hive/likely-done", "stage/010-actionable", "stage/stalled",
	} {
		t.Run(label, func(t *testing.T) {
			var added, removed []string
			srv := newLabelMockServer(t, &added, &removed)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "o/r", Number: 7, Agent: "scanner",
				Labels: []string{"bug", label},
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if len(added) != 0 || len(removed) != 0 {
				t.Fatalf("reserved label reached GitHub: added=%v removed=%v", added, removed)
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("refused label request was not quarantined: %v", err)
			}
			resp := readIssueResultFile(t, reqPath)
			if resp.OK || !strings.Contains(resp.Error, label) {
				t.Errorf("result does not explain the refusal: %+v", resp)
			}
			// The refusal names the sanctioned way to park an issue (#12329).
			if !strings.Contains(resp.Error, "hive-open-issue comment --needs-decision") {
				t.Errorf("refusal does not name the park command: %q", resp.Error)
			}
		})
	}
}

// hive/verified-open is the one hive state label agents may add: the kick
// asks them to record that a merged/reference claim did not finish the issue.
func TestIssueRequestWatcher_LabelAllowsVerifiedOpenVerdict(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 9804, Agent: "scanner",
		Labels: []string{"bug", " HIVE/verified-open "},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := strings.Join(added, ","); got != "bug,HIVE/verified-open" {
		t.Fatalf("labels added = %q, want bug and hive/verified-open", got)
	}
	if len(removed) != 0 {
		t.Fatalf("unexpected removed labels: %v", removed)
	}
	resp := readIssueResultFile(t, reqPath)
	if !resp.OK || resp.Number != 9804 {
		t.Fatalf("result = %+v, want ok on #9804", resp)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Error("a fulfilled verified-open label request should be consumed")
	}
}

// The decision parks are add-only for agents (hivecommons/hive#12683):
// parking an item on a maintainer decision is the ADR-0019 escalation move
// that replaced `needs-direction` / `needs-decision`; every other stage label
// is refused and the un-park stays with a maintainer. Both the issue bay
// (stage/045) and the PR-only stage/085 pass; the request is a Parked fact
// for the stage engine, which writes the one canonical spelling for the
// item's kind (ADR-0025) — never the agent's mixed-case or PR-only label.
func TestIssueRequestWatcher_LabelAllowsNeedsDecisionPark(t *testing.T) {
	for _, park := range []string{"Stage/045-Needs-Decision", "Stage/085-Needs-Decision"} {
		t.Run(park, func(t *testing.T) {
			var added, removed []string
			srv := newLabelMockServer(t, &added, &removed)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.SetAgentIssueDecisionPolicy("escalate")
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "o/r", Number: 12683, Agent: "scanner",
				Labels: []string{park},
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if got := strings.Join(added, ","); got != NeedsDecisionStageLabel {
				t.Fatalf("labels added = %q, want the engine's %s", got, NeedsDecisionStageLabel)
			}
			if len(removed) != 0 {
				t.Fatalf("unexpected removed labels: %v", removed)
			}
			if resp := readIssueResultFile(t, reqPath); !resp.OK {
				t.Fatalf("result = %+v, want ok", resp)
			}
		})
	}
}

func TestIssueRequestWatcher_LabelDowngradesNonRiskHiveDecisionPark(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 12816, Agent: "scanner",
		Body:   "Question: which ADR gets renumbered / use a v6-only ADR block",
		Labels: []string{NeedsDecisionStageLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := strings.Join(added, ","); got != "stage/010-actionable" {
		t.Fatalf("labels added = %q, want actionable downgrade", got)
	}
	if len(removed) != 0 {
		t.Fatalf("unexpected removed labels: %v", removed)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
}

// The resolve-policy downgrade only strips decision-park spellings. `blocked`
// is an owner hold, not a decision Hive may take, so it must survive the
// downgrade and still trip the reserved-label check exactly as it does when
// no decision park is requested alongside it.
func TestIssueRequestWatcher_LabelDowngradeKeepsBlockedReserved(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 12903, Agent: "scanner",
		Body:   "Question: which ADR gets renumbered / use a v6-only ADR block",
		Labels: []string{NeedsDecisionStageLabel, issueBlockedLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("reserved label reached GitHub: added=%v removed=%v", added, removed)
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK {
		t.Fatalf("result = %+v, want refusal for reserved label %q", resp, issueBlockedLabel)
	}
}

func TestRemoveDecisionParkLabelsKeepsOwnerHolds(t *testing.T) {
	in := []string{NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel, issueNeedsHumanLabel, issueBlockedLabel, "kind/bug"}
	got := removeDecisionParkLabels(append([]string(nil), in...), NeedsDecisionStageLabel)
	if want := strings.Join([]string{issueNeedsHumanLabel, issueBlockedLabel, "kind/bug"}, ","); strings.Join(got, ",") != want {
		t.Fatalf("removeDecisionParkLabels(%v) = %v, want %q", in, got, want)
	}
}

func TestIssueRequestWatcher_LabelResolveStillParksRiskQuestion(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 12820, Agent: "scanner",
		Body:   "Risk: credentials/secrets/owner-only infrastructure\nQuestion: needs owner's GitHub secret",
		Labels: []string{NeedsDecisionStageLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := strings.Join(added, ","); got != NeedsDecisionStageLabel {
		t.Fatalf("labels added = %q, want needs-decision", got)
	}
	if len(removed) != 0 {
		t.Fatalf("unexpected removed labels: %v", removed)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
}

// A reserved label is refused in the remove direction too: taking `hold` off
// a PR is exactly the escalation the guard exists for.
func TestIssueRequestWatcher_LabelRefusesReservedRemoval(t *testing.T) {
	for _, label := range []string{"hold", VerifiedOpenLabel, NeedsDecisionStageLabel, PRNeedsDecisionStageLabel} {
		t.Run(label, func(t *testing.T) {
			var added, removed []string
			srv := newLabelMockServer(t, &added, &removed)
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "o/r", Number: 7, Agent: "scanner",
				RemoveLabels: []string{label},
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if len(removed) != 0 {
				t.Fatalf("reserved label removal reached GitHub: %v", removed)
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("refused removal was not quarantined: %v", err)
			}
		})
	}
}

// An operator-renamed merge-queue label is reserved under its configured name.
func TestIssueRequestWatcher_LabelRefusesRenamedAutoMergeLabel(t *testing.T) {
	var added []string
	srv := newLabelMockServer(t, &added, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetAutoMergeLabel("ship-it")
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 7, Agent: "scanner",
		Labels: []string{"ship-it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(added) != 0 {
		t.Fatalf("renamed merge-queue label reached GitHub: %v", added)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
}

// The lane allowlist governs the new operation like every other one.
func TestIssueRequestWatcher_LabelRefusedOutsideLaneAllowlist(t *testing.T) {
	var added []string
	srv := newLabelMockServer(t, &added, nil)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment))
	recs := captureAudit(c)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 11, Agent: "scanner", Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(added) != 0 {
		t.Fatalf("%d labels applied by a lane not allowed to label, want 0", len(added))
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, WriteOpLabel) {
		t.Errorf("result does not name the refused operation: %+v", resp)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpLabel, "o/r", 11)
	if _, ok := findAudit(*recs, AuditActionAgentLabelApplied); ok {
		t.Error("a refused request was audited as a label write")
	}
}

// A label request with nothing to do (or no target) is quarantined rather
// than retried for a day.
func TestIssueRequestWatcher_LabelMalformed(t *testing.T) {
	for name, req := range map[string]IssueRequest{
		"no labels": {Kind: "label", Repo: "o/r", Number: 3, Agent: "scanner"},
		"no number": {Kind: "label", Repo: "o/r", Agent: "scanner", Labels: []string{"bug"}},
		"no agent":  {Kind: "label", Repo: "o/r", Number: 3, Labels: []string{"bug"}},
		"empty label list": {Kind: "label", Repo: "o/r", Number: 3, Agent: "scanner",
			Labels: []string{"  ", ","}},
	} {
		t.Run(name, func(t *testing.T) {
			c := issueTestClient(t, "http://127.0.0.1:0")
			dir := withIssueDir(t)
			reqPath, err := WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if _, err := os.Stat(reqPath + ".bad"); err != nil {
				t.Errorf("malformed label request should be quarantined as .bad")
			}
		})
	}
}

func TestReservedLabelForAgents(t *testing.T) {
	for _, label := range []string{"lgtm", " LGTM ", "hold", "hive/anything", "approved-direction"} {
		if !reservedLabelForAgents(label, AutoMergeQueuedLabel) {
			t.Errorf("reservedLabelForAgents(%q) = false, want true", label)
		}
	}
	for _, label := range []string{"bug", "area/proxy", "good first issue", "", "hivelike"} {
		if reservedLabelForAgents(label, AutoMergeQueuedLabel) {
			t.Errorf("reservedLabelForAgents(%q) = true, want false", label)
		}
	}
}

func TestNormalizeLabelList(t *testing.T) {
	got := normalizeLabelList([]string{" bug , bug", "", "area/proxy", "BUG"})
	if strings.Join(got, ",") != "bug,area/proxy" {
		t.Errorf("normalizeLabelList = %v, want [bug area/proxy]", got)
	}
	if normalizeLabelList(nil) != nil || normalizeLabelList([]string{" ", ","}) != nil {
		t.Error("an empty list must normalize to nil")
	}
}

// newEnsureLabelMockServer is a label relay server that also models label
// definitions: GET /labels/{name} is 404 until created, and creation is recorded.
func newEnsureLabelMockServer(t *testing.T, existing bool, ensureCalls *int, created *[]map[string]string, added *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			*ensureCalls++
			if existing {
				_, _ = io.WriteString(w, `{"name":"hive/verified-open"}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/9/labels"):
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(*created) == 0 && !existing && len(body) > 0 && strings.EqualFold(body[0], VerifiedOpenLabel) {
				t.Errorf("label added before it was created")
			}
			*added = append(*added, body...)
			_, _ = io.WriteString(w, `[{"name":"x"}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			*created = append(*created, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"hive/verified-open"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// The label relay creates hive/verified-open with the shared definition on a
// repo that lacks it, then adds it.
func TestIssueRequestWatcher_LabelCreatesMissingVerifiedOpen(t *testing.T) {
	var ensureCalls int
	var created []map[string]string
	var added []string
	srv := newEnsureLabelMockServer(t, false, &ensureCalls, &created, &added)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 9, Agent: "scanner",
		Labels: []string{VerifiedOpenLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(created) != 1 {
		t.Fatalf("label creations = %v, want exactly one", created)
	}
	if created[0]["name"] != VerifiedOpenLabel || created[0]["color"] != VerifiedOpenLabelColor || created[0]["description"] != VerifiedOpenLabelDescription {
		t.Errorf("created label = %v, want the shared verified-open definition", created[0])
	}
	if strings.Join(added, ",") != VerifiedOpenLabel {
		t.Errorf("labels added = %v, want %s", added, VerifiedOpenLabel)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Errorf("result = %+v, want ok", resp)
	}
}

// A failed label ensure leaves the request on the retry path, with no add.
func TestIssueRequestWatcher_LabelEnsureFailureRetries(t *testing.T) {
	var added []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/9/labels") {
			var body []string
			_ = json.NewDecoder(r.Body).Decode(&body)
			added = append(added, body...)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 9, Agent: "scanner",
		Labels: []string{VerifiedOpenLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(added) != 0 {
		t.Errorf("label added despite failed ensure: %v", added)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Errorf("request should stay queued for retry: %v", err)
	}
	if _, err := os.Stat(reqPath + ".denied"); err == nil {
		t.Error("a failed ensure must not deny the request")
	}
}

// A request with only plain labels never touches label definitions.
func TestIssueRequestWatcher_LabelPlainLabelsSkipEnsure(t *testing.T) {
	var ensureCalls int
	var created []map[string]string
	var added []string
	srv := newEnsureLabelMockServer(t, false, &ensureCalls, &created, &added)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	if _, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 9, Agent: "scanner",
		Labels: []string{"bug", "area/proxy"},
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if ensureCalls != 0 || len(created) != 0 {
		t.Errorf("plain labels triggered ensure: lookups=%d created=%v", ensureCalls, created)
	}
	if strings.Join(added, ",") != "bug,area/proxy" {
		t.Errorf("labels added = %v", added)
	}
}

// Adding verified-open while removing likely-done is still refused whole, and
// the refusal names the add-only request to send instead.
func TestIssueRequestWatcher_LabelMixedVerifiedOpenRemoveLikelyDoneRefused(t *testing.T) {
	var ensureCalls int
	var created []map[string]string
	var added []string
	srv := newEnsureLabelMockServer(t, false, &ensureCalls, &created, &added)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 9, Agent: "scanner",
		Labels: []string{VerifiedOpenLabel}, RemoveLabels: []string{LikelyDoneLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if len(added) != 0 || len(created) != 0 || ensureCalls != 0 {
		t.Fatalf("refused request reached GitHub: added=%v created=%v lookups=%d", added, created, ensureCalls)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
	resp := readIssueResultFile(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, LikelyDoneLabel) ||
		!strings.Contains(resp.Error, "adds only "+VerifiedOpenLabel) ||
		!strings.Contains(resp.Error, "do not remove") {
		t.Errorf("refusal does not tell the agent what to send: %+v", resp)
	}
}

// Without an add of verified-open (or with another refused label) the generic
// refusal is kept: no resend advice.
func TestReservedLabelRefusal_ResendHintOnlyForVerifiedOpenWithHiveRemovals(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name        string
		add, remove []string
		wantHint    bool
	}{
		{"add+remove likely-done", []string{VerifiedOpenLabel}, []string{LikelyDoneLabel}, true},
		{"add+remove covered-by-pr", []string{VerifiedOpenLabel}, []string{CoveredByPRLabel}, true},
		{"remove only", nil, []string{LikelyDoneLabel}, false},
		{"also removes hold", []string{VerifiedOpenLabel}, []string{LikelyDoneLabel, "hold"}, false},
		{"also adds likely-done", []string{VerifiedOpenLabel, LikelyDoneLabel}, []string{LikelyDoneLabel}, false},
		{"removes verified-open", []string{VerifiedOpenLabel}, []string{VerifiedOpenLabel}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, refused := c.reservedLabelRefusal(tc.add, tc.remove)
			if !refused {
				t.Fatal("request must be refused")
			}
			if got := strings.Contains(reason, "Resend a label request that adds only"); got != tc.wantHint {
				t.Errorf("resend hint = %v, want %v; reason: %s", got, tc.wantHint, reason)
			}
		})
	}
}

// A contributor agent read the owner's `/hive decision … Agents stand down`
// on hivecommons/hive#12680 and re-parked the issue asking for a decision.
// With `approved-direction` applied and Hive's own un-park reply recording
// the decision, the re-park is refused and the agent is told to add
// `needs-human` and unclaim instead. A newer `/hive decision` with different
// text means direction is changing, and the park goes through.
func TestIssueRequestWatcher_LabelRefusesReparkOnRecordedDecision(t *testing.T) {
	const decision = "Agents stand down; owner/Console owners handle this"
	reply := unparkWireComment{
		ID: 902, Body: fmt.Sprintf("%s%d -->\n%s", unparkReplyMarkerPrefix, 31, renderUnparkAcceptedReplyKeeping("owner", decision, nil, true, unparkNeedsHumanOutcome{applier: stageengine.NeedsHumanApplierPerson, actor: "owner"})),
		User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkOld(), UpdatedAt: unparkOld(),
	}
	cases := []struct {
		name      string
		comments  []unparkWireComment
		wantAdded bool
	}{
		{"decision unchanged", []unparkWireComment{humanComment(31, "owner", "/hive decision "+decision), reply}, false},
		{"same decision repeated", []unparkWireComment{reply, humanComment(33, "owner", "/hive decision "+decision)}, false},
		{"new decision pending", []unparkWireComment{reply, humanComment(34, "owner", "/hive decision option B after all")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := parkedIssue(12680, tc.comments...)
			issue.Labels = []wireLabel{{Name: "needs-human"}, {Name: HumanAckLabel}}
			server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
			c := newTestClient(t, server, "org", []string{"repo"})
			c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "org/repo", Number: 12680, Agent: "karibhou",
				Labels: []string{NeedsDecisionStageLabel},
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			resp := readIssueResultFile(t, reqPath)
			if tc.wantAdded {
				if !resp.OK || len(rec.added[12680]) != 1 {
					t.Fatalf("expected the park to go through, resp=%+v added=%v", resp, rec.added[12680])
				}
				return
			}
			if resp.OK || len(rec.added[12680]) != 0 {
				t.Fatalf("expected the re-park refused, resp=%+v added=%v", resp, rec.added[12680])
			}
			for _, want := range []string{"already decided", decision, "`needs-human`", "/unclaim"} {
				if !strings.Contains(resp.Error, want) {
					t.Fatalf("refusal %q should mention %q", resp.Error, want)
				}
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Fatalf("request should be moved to .denied: %v", err)
			}
		})
	}
}

func TestReparkDecisionRefusalRecognizesRetiredDecisionLabels(t *testing.T) {
	const decision = "Agents stand down; owner/Console owners handle this"
	reply := unparkWireComment{
		ID: 902, Body: fmt.Sprintf("%s%d -->\n%s", unparkReplyMarkerPrefix, 31, renderUnparkAcceptedReplyKeeping("owner", decision, []string{issueNeedsDecisionLabel}, true, unparkNeedsHumanOutcome{applier: stageengine.NeedsHumanApplierPerson, actor: "owner"})),
		User: wireUser{Login: "hive[bot]"}, CreatedAt: unparkOld(), UpdatedAt: unparkOld(),
	}
	for _, label := range []string{NeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel} {
		t.Run(label, func(t *testing.T) {
			issue := parkedIssue(12680, humanComment(31, "owner", "/hive decision "+decision), reply)
			issue.Labels = []wireLabel{{Name: issueNeedsHumanLabel}, {Name: HumanAckLabel}}
			server, _ := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
			c := newTestClient(t, server, "org", []string{"repo"})

			reason, refused, err := c.reparkDecisionRefusal(context.Background(), "org/repo", 12680, []string{label})
			if err != nil {
				t.Fatalf("reparkDecisionRefusal: %v", err)
			}
			if !refused || !strings.Contains(reason, "already decided") || !strings.Contains(reason, issueNeedsHumanLabel) {
				t.Fatalf("label %q refused=%v reason=%q, want recorded-decision refusal", label, refused, reason)
			}
		})
	}
}

// Without `approved-direction` the same request parks as before, and an
// agent may add `needs-human` (never remove it).
func TestIssueRequestWatcher_LabelAllowsNeedsHumanAdd(t *testing.T) {
	issue := parkedIssue(12681)
	issue.Labels = []wireLabel{{Name: "kind/bug"}}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.issueAuthz = func(agent string, uid int, kind string) error { return nil }
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "org/repo", Number: 12681, Agent: "karibhou",
		Labels: []string{"needs-human", NeedsDecisionStageLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
	if got := strings.Join(rec.added[12681], ","); got != "needs-human,"+NeedsDecisionStageLabel {
		t.Fatalf("added = %q", got)
	}

	removePath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "org/repo", Number: 12681, Agent: "karibhou",
		RemoveLabels: []string{"needs-human"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, removePath); resp.OK || !strings.Contains(resp.Error, "needs-human") {
		t.Fatalf("removing needs-human must be refused, got %+v", resp)
	}
}

func TestIssueRequestWatcher_LabelResolveParksUnclassifiedQuestion(t *testing.T) {
	var added, removed []string
	srv := newLabelMockServer(t, &added, &removed)
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "label", Repo: "o/r", Number: 12969, Agent: "scanner",
		Body:   neutralDecisionText,
		Labels: []string{NeedsDecisionStageLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if got := strings.Join(added, ","); got != NeedsDecisionStageLabel {
		t.Fatalf("labels added = %q, want park %q", got, NeedsDecisionStageLabel)
	}
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
}

// A park the stage engine did not land must not be reported as done
// (hivecommons/hive#13069): when the issue still wears stage/010-actionable
// after the Parked fact, the relay answers ok:false with the reason and
// keeps the request on the retry path; once the park stage is on the
// issue the same request succeeds.
func TestIssueRequestWatcher_LabelParkReportsFailureWhenStageDidNotLand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		wantOK bool
	}{
		{"still actionable", "stage/010-actionable", false},
		{"parked", NeedsDecisionStageLabel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
					_, _ = io.WriteString(w, `[]`)
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/"):
					_, _ = fmt.Fprintf(w, `{"number":219,"state":"open","labels":[{"name":"security"},{"name":%q}]}`, tc.stage)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			c.SetAgentIssueDecisionPolicy("escalate")
			sink := &stageenginetest.Recorder{}
			c.SetStageFactSink(sink)
			dir := withIssueDir(t)

			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "label", Repo: "o/site", Number: 219, Agent: "scanner",
				Labels: []string{NeedsDecisionStageLabel},
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())

			if len(sink.Facts) != 1 || sink.Facts[0].Method != "Parked" {
				t.Fatalf("facts = %+v, want one Parked", sink.Facts)
			}
			resp := readIssueResultFile(t, reqPath)
			_, statErr := os.Stat(reqPath)
			if tc.wantOK {
				if !resp.OK || strings.Join(resp.LabelsAdded, ",") != NeedsDecisionStageLabel {
					t.Fatalf("result = %+v, want ok with the park", resp)
				}
				if !os.IsNotExist(statErr) {
					t.Fatal("a fulfilled park should be consumed")
				}
				return
			}
			if resp.OK || len(resp.LabelsAdded) != 0 || !strings.Contains(resp.Error, tc.stage) {
				t.Fatalf("result = %+v, want ok:false naming %s", resp, tc.stage)
			}
			if statErr != nil {
				t.Fatalf("an unlanded park should stay on the retry path: %v", statErr)
			}
		})
	}
}
