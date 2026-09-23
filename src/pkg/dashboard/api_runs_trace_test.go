package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/timeline"
)

type traceCommitReader map[string]string

func (r traceCommitReader) CommitMessage(_ context.Context, sha string) (string, error) {
	return r[sha], nil
}

func TestRunTraceEndpointResolvesCommitLinks(t *testing.T) {
	s, _ := runsTestServer(t)
	oldGit := runTraceGit
	runTraceGit = traceCommitReader{"abc123": `ship

Hive-Run: myorg/repo1#8299
Hive-Plan: plan-main
Hive-Spec: spec.md#C-1
`}
	t.Cleanup(func() { runTraceGit = oldGit })
	s.LifecycleTimeline().Record(timeline.Event{
		IssueRef: "myorg/repo1#8299",
		Kind:     timeline.KindStageCompleted,
		Attrs:    map[string]string{"plan": "plan-main", "section": "Build"},
	})
	s.audit.Log("owner", "plan_approve", "epic=e1, run=myorg/repo1#8299, surface=plan", "architect")
	s.audit.Log("agent", "agent_rationale", "run=myorg/repo1#8299, plan=plan-main, note=selected", "coder")

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238299/trace?sha=abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET trace = %d body=%s", rec.Code, rec.Body.String())
	}
	var links agent.ArtifactLinks
	if err := json.Unmarshal(rec.Body.Bytes(), &links); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if links.Run != "myorg/repo1#8299" || links.PlanSection != "Build" || links.Spec != "spec.md" || links.Clause != "C-1" {
		t.Fatalf("links = %+v", links)
	}
	if links.Approval == nil || links.Approval.Action != "plan_approve" {
		t.Fatalf("approval = %+v", links.Approval)
	}
	if len(links.Rationale) != 1 || links.Rationale[0].Action != "agent_rationale" {
		t.Fatalf("rationale = %+v", links.Rationale)
	}
}

func TestRunTraceEndpointNoLinkageAuditsFinding(t *testing.T) {
	s, _ := runsTestServer(t)
	oldGit := runTraceGit
	runTraceGit = traceCommitReader{"def456": "plain\n"}
	t.Cleanup(func() { runTraceGit = oldGit })

	rec := doGet(s, "/api/runs/myorg%2Frepo1%238299/trace?sha=def456")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET trace = %d body=%s", rec.Code, rec.Body.String())
	}

	var links agent.ArtifactLinks
	if err := json.Unmarshal(rec.Body.Bytes(), &links); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if !links.NoLinkage || len(links.Missing) != 3 {
		t.Fatalf("links = %+v, want no linkage", links)
	}
	found := false
	for _, entry := range s.audit.Recent(10) {
		if entry.Action == agent.AuditArtifactLinkMissing && strings.Contains(entry.Detail, "sha=def456") {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifact_link_missing not recorded: %+v", s.audit.Recent(10))
	}
}

func TestRunRepoFromKeyHandlesGitHubAndExternalWork(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"myorg/repo1#8299", "myorg/repo1"},
		{"myorg/repo1!ENG-123", "myorg/repo1"},
		{"myorg/repo1", "myorg/repo1"},
	} {
		if got := runRepoFromKey(tc.key); got != tc.want {
			t.Fatalf("runRepoFromKey(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
