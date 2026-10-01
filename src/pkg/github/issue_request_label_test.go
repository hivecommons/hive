package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newLabelMockServer records the label adds and removes a label request makes.
func newLabelMockServer(t *testing.T, added *[]string, removed *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
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
		"design-approved", "needs-human", "needs-decision", "blocked",
		"hive/claimed-by-other", "hive/likely-done",
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

// A reserved label is refused in the remove direction too: taking `hold` off
// a PR is exactly the escalation the guard exists for.
func TestIssueRequestWatcher_LabelRefusesReservedRemoval(t *testing.T) {
	for _, label := range []string{"hold", VerifiedOpenLabel} {
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
