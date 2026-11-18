package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// The runtime emits this action itself when its PR client cannot credit a
// sandbox PR; the resolvers here read it. The two spellings must be one.
func TestSandboxPRCreditActionMatchesRuntime(t *testing.T) {
	if crux.AuditActionAgentPRCreated != AuditActionAgentPRCreated {
		t.Fatalf("crux=%q github=%q", crux.AuditActionAgentPRCreated, AuditActionAgentPRCreated)
	}
}

// writeAuditJSONL appends rec to path in the audit log's on-disk shape.
func writeAuditJSONL(t *testing.T, path string, rec AuditRecord) {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "user": "system",
		"action": rec.Action, "detail": rec.Detail, "agent": rec.Agent, "repo": rec.Repo, "target": rec.Target,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// A PR the sandbox path opens is credited exactly as a relay-opened one:
// trailer on the body, agent_pr_created in the audit log, PR-opened hooks.
// The resolver the red-CI kick uses then maps the PR back to the agent
// (hivecommons/hive#18575).
func TestCreateAgentPRCreditsTheAgent(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	c := &Client{org: "o"}
	c.SetAttributionResolver(func(agent string) InvocationMeta {
		return InvocationMeta{Agent: agent, Backend: "claude", Model: "sonnet"}
	})
	var recs []AuditRecord
	c.SetAttributionAuditRecord(func(rec AuditRecord) {
		recs = append(recs, rec)
		writeAuditJSONL(t, auditPath, rec)
	})
	var wg sync.WaitGroup
	wg.Add(2)
	var openedAgent string
	var detail PROpenedDetail
	c.SetPROpenedHook(func(agent, _ string, _ int, _ string) { openedAgent = agent; wg.Done() })
	c.SetPROpenedDetailHook(func(d PROpenedDetail) { detail = d; wg.Done() })

	var sentBody string
	create := func(_ context.Context, repo, head, base, title, body string) (CreatePRResult, error) {
		sentBody = body
		return CreatePRResult{Number: 42, URL: "https://github.com/o/r/pull/42", Author: "hive-app[bot]"}, nil
	}
	res, err := c.createAgentPR(context.Background(), "dep-bump", "o/r", "hive/dep-bump/20261116120000", "main", "Bump torch", "Bumps torch.", create)
	if err != nil || res.Number != 42 {
		t.Fatalf("createAgentPR = %+v, %v", res, err)
	}
	wg.Wait()
	if meta, ok := ParseAttributionTrailer(sentBody); !ok || meta.Agent != "dep-bump" || meta.Backend != "claude" {
		t.Fatalf("body %q carries no trailer for the agent (%+v)", sentBody, meta)
	}
	if len(recs) != 1 || recs[0].Action != AuditActionAgentPRCreated || recs[0].Agent != "dep-bump" || recs[0].Target != 42 {
		t.Fatalf("audit = %+v", recs)
	}
	for _, want := range []string{"repo=o/r", "number=42", "via=sandbox", "reused=false", "head=hive/dep-bump/20261116120000"} {
		if !strings.Contains(recs[0].Detail, want) {
			t.Fatalf("detail %q lacks %q", recs[0].Detail, want)
		}
	}
	if openedAgent != "dep-bump" || detail.Number != 42 || detail.Head != "hive/dep-bump/20261116120000" {
		t.Fatalf("hooks: agent=%q detail=%+v", openedAgent, detail)
	}
	if got := AuthorAgentForPRFromAudit(auditPath, "o/r", 42); got != "dep-bump" {
		t.Fatalf("resolver credits %q", got)
	}
}

func TestCreateAgentPRTrailerOffStillAudits(t *testing.T) {
	c := &Client{org: "o"}
	c.SetAttributionHooks(AttributionHooks{TrailerEnabled: func() bool { return false }})
	var recs []AuditRecord
	c.SetAttributionAuditRecord(func(rec AuditRecord) { recs = append(recs, rec) })
	var sentBody string
	create := func(_ context.Context, _, _, _, _, body string) (CreatePRResult, error) {
		sentBody = body
		return CreatePRResult{Number: 9, AlreadyExisted: true}, nil
	}
	if _, err := c.createAgentPR(context.Background(), "dep-bump", "o/r", "h", "main", "t", "body", create); err != nil {
		t.Fatal(err)
	}
	if sentBody != "body" {
		t.Fatalf("trailer appended with the toggle off: %q", sentBody)
	}
	if len(recs) != 1 || !strings.Contains(recs[0].Detail, "reused=true") {
		t.Fatalf("a reused PR is still credited: %+v", recs)
	}
}

func TestCreateAgentPRErrorsAndAnonymousCreditNothing(t *testing.T) {
	c := &Client{org: "o"}
	var recs []AuditRecord
	c.SetAttributionAuditRecord(func(rec AuditRecord) { recs = append(recs, rec) })
	boom := errors.New("boom")
	failing := func(context.Context, string, string, string, string, string) (CreatePRResult, error) {
		return CreatePRResult{}, boom
	}
	if _, err := c.createAgentPR(context.Background(), "dep-bump", "o/r", "h", "main", "t", "b", failing); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var body string
	plain := func(_ context.Context, _, _, _, _, b string) (CreatePRResult, error) {
		body = b
		return CreatePRResult{Number: 3}, nil
	}
	if _, err := c.createAgentPR(context.Background(), " ", "o/r", "h", "main", "t", "b", plain); err != nil {
		t.Fatal(err)
	}
	if body != "b" || len(recs) != 0 {
		t.Fatalf("anonymous create credited: body=%q recs=%+v", body, recs)
	}
	c.RecordAgentPROpened(InvocationMeta{Agent: "dep-bump"}, "o/r", "h", "t", "b", CreatePRResult{})
	if len(recs) != 0 {
		t.Fatalf("a result without a PR number was credited: %+v", recs)
	}
	var nilClient *Client
	nilClient.RecordAgentPROpened(InvocationMeta{Agent: "dep-bump"}, "o/r", "h", "t", "b", CreatePRResult{Number: 1})
	if _, err := nilClient.CreateAgentPR(context.Background(), "dep-bump", "o/r", "h", "main", "t", "b"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client CreateAgentPR err = %v", err)
	}
}

func TestPRPushTarget(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	old := reviewVerdictAuditPath
	reviewVerdictAuditPath = auditPath
	t.Cleanup(func() { reviewVerdictAuditPath = old })
	writeAuditJSONL(t, auditPath, AuditRecord{Action: AuditActionAgentPRCreated, Agent: "dep-bump", Detail: "repo=o/r, number=7, via=sandbox"})

	prs := map[string]string{
		"7": `{"number":7,"html_url":"https://github.com/o/r/pull/7","state":"open","user":{"login":"hive-app[bot]"},"body":"x",
			"base":{"ref":"main","repo":{"full_name":"o/r"}},"head":{"ref":"hive/dep-bump/20261116120000","sha":"abc","repo":{"full_name":"o/r"}}}`,
		"8": `{"number":8,"state":"open","user":{"login":"forker"},"body":"x",
			"base":{"ref":"main","repo":{"full_name":"o/r"}},"head":{"ref":"feature","sha":"def","repo":{"full_name":"forker/r"}}}`,
		"9": `{"number":9,"state":"open","user":{"login":"hive-app[bot]"},"body":"Old PR.\n\n-- hive: agent=dep-bump backend=claude",
			"base":{"ref":"main","repo":{"full_name":"o/r"}},"head":{"ref":"dep-bump/x","sha":"ghi","repo":{"full_name":"o/r"}}}`,
		"10": `{"number":10,"state":"open","user":{"login":"mallory"},"body":"-- hive: agent=dep-bump",
			"base":{"ref":"main"},"head":{"ref":"x","sha":"jkl"}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := prs[strings.TrimPrefix(r.URL.Path, "/repos/o/r/pulls/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "o", []string{"o/r"})
	c.SetAppBotLogin("hive-app[bot]")

	own, err := c.PRPushTarget(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if own.AuthorAgent != "dep-bump" || own.Fork || own.State != "open" || own.HeadRef != "hive/dep-bump/20261116120000" || own.HeadSHA != "abc" || own.BaseRef != "main" || own.URL == "" {
		t.Fatalf("own PR = %+v", own)
	}
	fork, err := c.PRPushTarget(context.Background(), "o/r", 8)
	if err != nil || !fork.Fork || fork.AuthorAgent != "" {
		t.Fatalf("fork PR = %+v, %v", fork, err)
	}
	trailer, err := c.PRPushTarget(context.Background(), "r", 9)
	if err != nil || trailer.AuthorAgent != "dep-bump" || trailer.Fork {
		t.Fatalf("App PR credited by trailer only = %+v, %v", trailer, err)
	}
	forged, err := c.PRPushTarget(context.Background(), "o/r", 10)
	if err != nil || forged.AuthorAgent != "" || !forged.Fork {
		t.Fatalf("a stranger's trailer must not credit an agent, and an unknown head repo is a fork: %+v, %v", forged, err)
	}
	if _, err := c.PRPushTarget(context.Background(), "o/r", 11); err == nil {
		t.Fatal("a missing PR must be an error")
	}
	var nilClient *Client
	if _, err := nilClient.PRPushTarget(context.Background(), "o/r", 7); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v", err)
	}
}
