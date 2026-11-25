package github

import (
	"context"
	"os"
	"strings"
	"testing"
)

// fakeOpenModeGate refuses every PR on repo "o/r" unless agent is "drafter"
// (standing in for an agent the hive's hand-off record proves is doing
// workflow work), and records what it was asked.
type fakeOpenModeGate struct {
	calls  int
	issues []int
	via    string
}

func (g *fakeOpenModeGate) gate(agent, repo string, issues []int, via string) (string, string) {
	g.via = via
	g.calls++
	g.issues = issues
	if !strings.EqualFold(repo, "o/r") {
		return "on", ""
	}
	if agent == "drafter" {
		return "workflow_only", ""
	}
	return "workflow_only", "o/r accepts only workflow hand-off pull requests"
}

func TestPRRequestWatcher_OpenPRsGateRefusesAndAudits(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	g := &fakeOpenModeGate{}
	c.SetPROpenModeGate(g.gate)
	recs := captureAudit(c)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/cov-1", Title: "[scanner] test: harden", Body: "Fixes #1", Agent: "scanner", IssueN: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d PRs opened past the open-PRs gate, want 0", created)
	}
	if g.calls != 1 || len(g.issues) != 1 || g.issues[0] != 1 {
		t.Errorf("gate must see the request's issues: calls=%d issues=%v", g.calls, g.issues)
	}
	if g.via != "relay" {
		t.Errorf("gate via = %q, want relay", g.via)
	}
	if _, err := os.Stat(reqPath + ".rejected"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
	if res := readPRResult(t, reqPath); res.OK || !strings.Contains(res.Error, GateOpenPRs) || !strings.Contains(res.Error, "workflow hand-off") {
		t.Errorf("result does not explain the refusal: %+v", res)
	}
	rec, ok := findAudit(*recs, AuditActionAgentWriteRefused)
	if !ok {
		t.Fatalf("refusal not audited: %+v", *recs)
	}
	if rec.Agent != "scanner" || rec.Repo != "o/r" || !strings.Contains(rec.Detail, "gate="+GateOpenPRs) || !strings.Contains(rec.Detail, "mode=workflow_only") {
		t.Errorf("refusal audit = %+v", rec)
	}
}

func TestPRRequestWatcher_OpenPRsGateAllows(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	g := &fakeOpenModeGate{}
	c.SetPROpenModeGate(g.gate)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "drafter/doc-1", Title: "[drafter] docs: analysis", Body: "Fixes #1", Agent: "drafter"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	if created != 1 {
		t.Fatalf("an allowed PR did not open: %d created, want 1", created)
	}
}

func TestPRRequestWatcher_NoOpenPRsGateIsOn(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	if _, refused := c.refuseOpenPRs("scanner", "o/r", nil, "relay"); refused {
		t.Error("a client with no gate must not refuse")
	}
	var nilClient *Client
	if _, refused := nilClient.refuseOpenPRs("scanner", "o/r", nil, "relay"); refused {
		t.Error("nil client must not refuse")
	}
}

// The sandbox path asks the same gate before CreatePR runs.
func TestCreateAgentPR_OpenPRsGate(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	g := &fakeOpenModeGate{}
	c.SetPROpenModeGate(g.gate)
	recs := captureAudit(c)
	calls := 0
	create := func(ctx context.Context, repo, head, base, title, body string) (CreatePRResult, error) {
		calls++
		return CreatePRResult{Number: 7, URL: "https://github.com/o/r/pull/7"}, nil
	}
	if _, err := c.createAgentPR(context.Background(), "scanner", "o/r", "scanner/x", "main", "t", "b", create); err == nil || !strings.Contains(err.Error(), "workflow hand-off") {
		t.Fatalf("sandbox PR must be refused with the reason, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("CreatePR ran %d times past a refusal", calls)
	}
	if rec, ok := findAudit(*recs, AuditActionAgentWriteRefused); !ok || !strings.Contains(rec.Detail, "via="+SandboxPRVia) {
		t.Errorf("sandbox refusal not audited: %+v", *recs)
	}
	if _, err := c.createAgentPR(context.Background(), "drafter", "o/r", "drafter/x", "main", "t", "b", create); err != nil || calls != 1 {
		t.Fatalf("allowed sandbox PR: err=%v calls=%d", err, calls)
	}
	if _, err := c.createAgentPR(context.Background(), "scanner", "o/other", "scanner/y", "main", "t", "b", create); err != nil || calls != 2 {
		t.Fatalf("another repo must be unaffected: err=%v calls=%d", err, calls)
	}
}
