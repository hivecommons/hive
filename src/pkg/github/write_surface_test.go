package github

import (
	"context"
	"os"
	"strings"
	"testing"
)

// allowOnly returns an allowlist predicate that restricts one agent to the
// listed operations and leaves every other agent unrestricted, the shape
// config.AgentMayWrite has for a hive that lists a single lane.
func allowOnly(agentName string, ops ...string) func(string, string) bool {
	return func(agent, op string) bool {
		if agent != agentName {
			return true
		}
		for _, o := range ops {
			if o == op {
				return true
			}
		}
		return false
	}
}

// captureAudit wires a typed audit sink and returns the slice it appends to.
func captureAudit(c *Client) *[]AuditRecord {
	var recs []AuditRecord
	c.SetAttributionAuditRecord(func(r AuditRecord) { recs = append(recs, r) })
	return &recs
}

func findAudit(recs []AuditRecord, action string) (AuditRecord, bool) {
	for _, r := range recs {
		if r.Action == action {
			return r, true
		}
	}
	return AuditRecord{}, false
}

func TestAgentMayWrite_FailsOpenWithoutPredicate(t *testing.T) {
	var nilClient *Client
	if !nilClient.AgentMayWrite("scanner", WriteOpOpenPR) {
		t.Error("nil client must fail open")
	}
	c := testClient(t, "http://127.0.0.1:1")
	for _, op := range WriteOps() {
		if !c.AgentMayWrite("scanner", op) {
			t.Errorf("no allowlist configured, but %s was refused", op)
		}
	}
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment))
	if c.AgentMayWrite("scanner", WriteOpOpenPR) {
		t.Error("allowlisted lane was allowed an operation outside its list")
	}
	if !c.AgentMayWrite("scanner", WriteOpComment) {
		t.Error("allowlisted lane was refused an operation on its list")
	}
	if !c.AgentMayWrite("reviewer", WriteOpOpenPR) {
		t.Error("a lane with no allowlist entry was restricted")
	}
	if !c.AgentMayWrite("", WriteOpOpenPR) {
		t.Error("an unnamed agent must fail open (the authorizer refuses it first)")
	}
	c.SetWriteAllowlistFunc(nil)
	if !c.AgentMayWrite("scanner", WriteOpOpenPR) {
		t.Error("clearing the predicate must restore unrestricted behaviour")
	}
}

func TestWriteOpsAreDistinctAndRecognized(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range WriteOps() {
		if seen[op] {
			t.Errorf("duplicate op %q", op)
		}
		seen[op] = true
		if !IsWriteOp(op) || !IsWriteOp(" "+strings.ToUpper(op)+" ") {
			t.Errorf("IsWriteOp(%q) = false", op)
		}
	}
	if IsWriteOp("push_branch") || IsWriteOp("") {
		t.Error("IsWriteOp accepted an operation the relays do not perform")
	}
}

func TestIssueRequestWriteOpMapping(t *testing.T) {
	for kind, want := range map[string]string{
		"issue":   WriteOpCreateIssue,
		"comment": WriteOpComment,
		"claim":   WriteOpClaim,
		"close":   WriteOpCloseIssue,
	} {
		if got := issueRequestWriteOp(kind); got != want {
			t.Errorf("issueRequestWriteOp(%q) = %q, want %q", kind, got, want)
		}
	}
}

// The typed fields come from the same repo/number pairs that the legacy detail
// carries, so the two can never disagree.
func TestAuditRecordFor_TypedRepoAndTarget(t *testing.T) {
	rec := auditRecordFor(AuditActionAgentPRCreated, InvocationMeta{Agent: "scanner", Backend: "claude"},
		"repo", "o/r", "number", "42", "url", "https://github.com/o/r/pull/42")
	if rec.Repo != "o/r" || rec.Target != 42 || rec.Agent != "scanner" || rec.Action != AuditActionAgentPRCreated {
		t.Fatalf("typed fields wrong: %+v", rec)
	}
	for _, want := range []string{"repo=o/r", "number=42", "agent=scanner", "backend=claude"} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("legacy detail lost %q: %q", want, rec.Detail)
		}
	}

	// A zero or unparseable number is "no target", never a bogus target.
	for _, n := range []string{"0", "", "abc", "-3"} {
		if got := auditRecordFor("x", InvocationMeta{}, "repo", "o/r", "number", n); got.Target != 0 {
			t.Errorf("number=%q produced Target=%d, want 0", n, got.Target)
		}
	}
	if got := auditRecordFor("x", InvocationMeta{}, "url", "u"); got.Repo != "" || got.Target != 0 {
		t.Errorf("a write with no repo/number must have empty typed fields: %+v", got)
	}
}

func TestRecordCreationAudit_TypedSinkWinsOverLegacy(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	legacy := 0
	c.SetAttributionAudit(func(action, detail, agent string) { legacy++ })
	recs := captureAudit(c)

	c.recordCreationAudit(AuditActionAgentCommentCreated, InvocationMeta{Agent: "quality"}, "repo", "o/r", "number", "7")

	if legacy != 0 {
		t.Errorf("legacy sink called %d times alongside the typed sink; one entry must be recorded once", legacy)
	}
	if len(*recs) != 1 || (*recs)[0].Repo != "o/r" || (*recs)[0].Target != 7 {
		t.Fatalf("typed sink got %+v", *recs)
	}
}

func TestRecordCreationAudit_LegacySinkStillServed(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	var gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) { gotDetail = detail })
	c.recordCreationAudit(AuditActionAgentCommentCreated, InvocationMeta{Agent: "quality"}, "repo", "o/r", "number", "7")
	if !strings.Contains(gotDetail, "repo=o/r") || !strings.Contains(gotDetail, "number=7") {
		t.Errorf("legacy sink detail = %q", gotDetail)
	}
}

// credentialSamples are the credential shapes that must never reach the audit
// log, each paired with the secret substring whose presence would be a leak.
var credentialSamples = []struct {
	name, text, secret string
}{
	{"installation token", "ghs_abcdefghijklmnopqrstuvwxyz0123456789", "abcdefghijklmnopqrstuvwxyz0123456789"},
	{"personal token", "ghp_ZYXWVUTSRQPONMLKJIHGFEDCBA98765", "ZYXWVUTSRQPONMLKJIHGFEDCBA98765"},
	{"fine-grained token", "github_pat_11AAAAAAA0abcdefghijklmnop", "11AAAAAAA0abcdefghijklmnop"},
	{"bearer header", "Authorization: Bearer s3cr3tBearerValue0123456789", "s3cr3tBearerValue0123456789"},
	{"token header", "Authorization: token plainSecretNoPrefix42", "plainSecretNoPrefix42"},
	{"basic header", "authorization: Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA=="},
	{"json header", `{"Authorization":"opaqueValue99"}`, "opaqueValue99"},
	{"jwt", "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiIxMjM0NTYiLCJpYXQiOjE3MDAwMDB9.c2lnbmF0dXJlc2lnbmF0dXJlc2ln", "c2lnbmF0dXJlc2lnbmF0dXJlc2ln"},
}

// No credential material may appear in any audited argument or result
// (#9587 acceptance). Every value is fed through every slot a write site can
// fill: the repo (typed and legacy), an argument pair, and a result pair.
func TestRecordCreationAudit_RedactsCredentialMaterial(t *testing.T) {
	for _, s := range credentialSamples {
		t.Run(s.name, func(t *testing.T) {
			c := testClient(t, "http://127.0.0.1:1")
			recs := captureAudit(c)
			c.recordCreationAudit(AuditActionIssueClosed, InvocationMeta{Agent: "quality"},
				"repo", "o/"+s.text,
				"number", "3",
				"override_reason", "reporter pasted "+s.text,
				"url", "https://x.example/?q="+s.text)
			if len(*recs) != 1 {
				t.Fatalf("got %d records, want 1", len(*recs))
			}
			rec := (*recs)[0]
			for field, v := range map[string]string{"detail": rec.Detail, "repo": rec.Repo} {
				if strings.Contains(v, s.secret) {
					t.Errorf("%s leaked credential material: %q", field, v)
				}
			}
			if !strings.Contains(rec.Detail, "[REDACTED]") {
				t.Errorf("detail shows no redaction marker: %q", rec.Detail)
			}
			if rec.Target != 3 {
				t.Errorf("redaction disturbed the typed target: %d", rec.Target)
			}
		})
	}
}

func TestRedactAuditText_LeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{"", "repo=o/r, number=12, state=approved", "reporter asked to close as duplicate of #4"} {
		if got := redactAuditText(s); got != s {
			t.Errorf("redactAuditText(%q) = %q, want unchanged", s, got)
		}
	}
}

// ---------- relay enforcement ----------

func assertRefusalAudited(t *testing.T, recs []AuditRecord, agent, op, repo string, target int) {
	t.Helper()
	rec, ok := findAudit(recs, AuditActionAgentWriteRefused)
	if !ok {
		t.Fatalf("refusal was not audited; records: %+v", recs)
	}
	if rec.Agent != agent || rec.Repo != repo || rec.Target != target {
		t.Errorf("refusal audit typed fields = %+v, want agent=%s repo=%s target=%d", rec, agent, repo, target)
	}
	if !strings.Contains(rec.Detail, "op="+op) || !strings.Contains(rec.Detail, "outcome=refused") {
		t.Errorf("refusal audit detail = %q, want op=%s and outcome=refused", rec.Detail, op)
	}
}

func TestPRRequestWatcher_RefusesOpOutsideLaneAllowlist(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment, WriteOpCreateIssue))
	recs := captureAudit(c)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	reqPath, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("%d PRs opened by a lane not allowed to open PRs, want 0", created)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused request was not quarantined: %v", err)
	}
	if res := readPRResult(t, reqPath); res.OK || !strings.Contains(res.Error, "write allowlist") || !strings.Contains(res.Error, WriteOpOpenPR) {
		t.Errorf("result does not explain the refusal: %+v", res)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpOpenPR, "o/r", 0)
	if _, ok := findAudit(*recs, AuditActionAgentPRCreated); ok {
		t.Error("a refused request was audited as a creation")
	}
}

// Allowed operations are unaffected, and the creation audit carries typed
// repo/target so #4836 needs no reconstruction.
func TestPRRequestWatcher_AllowedOpOpensWithTypedAudit(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpOpenPR))
	recs := captureAudit(c)

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-2", Title: "[scanner] fix: thing", Body: "Fixes #1", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("an allowlisted open_pr was blocked: %d created, want 1", created)
	}
	rec, ok := findAudit(*recs, AuditActionAgentPRCreated)
	if !ok {
		t.Fatalf("creation not audited: %+v", *recs)
	}
	if rec.Repo != "o/r" || rec.Target != 42 || rec.Agent != "scanner" {
		t.Errorf("creation audit typed fields = %+v, want repo=o/r target=42 agent=scanner", rec)
	}
	if _, refused := findAudit(*recs, AuditActionAgentWriteRefused); refused {
		t.Error("an allowed request was audited as refused")
	}
}

// Kinds on the one issue relay are separate operations: a lane may comment
// without being allowed to file.
func TestIssueRequestWatcher_AllowlistIsPerKind(t *testing.T) {
	created, commented := 0, 0
	srv := newIssueMockServer(t, "", &created, nil, &commented)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("quality", WriteOpComment))
	recs := captureAudit(c)
	dir := withIssueDir(t)

	issuePath, err := WriteIssueRequest(dir, IssueRequest{Repo: "o/r", Title: "found a thing", Body: "detail", Agent: "quality"})
	if err != nil {
		t.Fatal(err)
	}
	commentPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "triage note", Agent: "quality"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if created != 0 {
		t.Errorf("%d issues filed by a comment-only lane, want 0", created)
	}
	if _, err := os.Stat(issuePath + ".denied"); err != nil {
		t.Errorf("refused create was not quarantined: %v", err)
	}
	assertRefusalAudited(t, *recs, "quality", WriteOpCreateIssue, "o/r", 0)

	if commented != 1 {
		t.Errorf("allowlisted comment was blocked: %d posted, want 1", commented)
	}
	if _, err := os.Stat(commentPath); !os.IsNotExist(err) {
		t.Errorf("allowed comment request was not consumed")
	}
	rec, ok := findAudit(*recs, AuditActionAgentCommentCreated)
	if !ok || rec.Repo != "o/r" || rec.Target != 41 {
		t.Errorf("comment audit typed fields = %+v (found=%v), want repo=o/r target=41", rec, ok)
	}
}

// End to end through a relay: an agent-supplied field carrying a credential is
// masked before it reaches the audit log.
func TestIssueRequestWatcher_AuditRedactsAgentSuppliedCredential(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	recs := captureAudit(c)
	dir := withIssueDir(t)

	const secret = "plainSecretNoPrefix42"
	if _, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 5, Body: "note", Agent: "quality",
		OverrideReason: "Authorization: token " + secret,
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())

	if commented != 1 {
		t.Fatalf("comment not posted: %d", commented)
	}
	rec, ok := findAudit(*recs, AuditActionAgentCommentCreated)
	if !ok {
		t.Fatalf("comment not audited: %+v", *recs)
	}
	if strings.Contains(rec.Detail, secret) || strings.Contains(rec.Repo, secret) {
		t.Errorf("audit leaked the credential: %+v", rec)
	}
}

func TestReviewRequestWatcher_RefusesOpOutsideLaneAllowlist(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpComment))
	recs := captureAudit(c)
	dir := withReviewDir(t)

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "approve", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 0 {
		t.Fatalf("%d reviews submitted by a lane not allowed to review, want 0", reviewed)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused review was not quarantined: %v", err)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpReview, "o/r", 5)
}

func TestReviewRequestWatcher_ResolveThreadIsItsOwnOp(t *testing.T) {
	c := reviewTestClient(t, "http://127.0.0.1:1")
	c.SetWriteAllowlistFunc(allowOnly("reviewer", WriteOpReview))
	recs := captureAudit(c)
	dir := withReviewDir(t)

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: "resolve_thread", ThreadID: "PRRT_x", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused resolve_thread was not quarantined: %v", err)
	}
	assertRefusalAudited(t, *recs, "reviewer", WriteOpResolveThread, "o/r", 9)
}

func TestMergeRequestWatcher_RefusesOpOutsideLaneAllowlist(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := scopeTestClient(t, srv.URL)
	c.SetWriteAllowlistFunc(allowOnly("scanner", WriteOpOpenPR))
	recs := captureAudit(c)

	dir := t.TempDir()
	old := mergeRequestDirForTest
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = old }()

	// UpdateBranch true: a refused merge must receive no write at all, not
	// even the head-branch update.
	reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 7, Agent: "scanner", ExpectSHA: "deadbeef", UpdateBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 0 {
		t.Fatalf("%d merges by a lane not allowed to merge, want 0", merges)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("refused merge was not quarantined: %v", err)
	}
	if resp := readMergeResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, "write allowlist") {
		t.Errorf("result does not explain the refusal: %+v", resp)
	}
	assertRefusalAudited(t, *recs, "scanner", WriteOpMergePR, "o/r", 7)
}
