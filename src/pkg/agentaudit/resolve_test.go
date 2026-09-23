package agentaudit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/timeline"
)

type fakeAuditReader []AuditRecord

func (f fakeAuditReader) RecentAudit(int) []AuditRecord { return []AuditRecord(f) }

type captureSink struct {
	actions []string
	fields  []map[string]any
}

func (s *captureSink) Record(_ string, action, _ string, fields map[string]any) {
	s.actions = append(s.actions, action)
	s.fields = append(s.fields, fields)
}

func TestResolveCommitLinksTrailersTimelineAndAudit(t *testing.T) {
	dir := initGitRepo(t)
	sha := commitFixture(t, dir, "linked", `implement thing

Hive-Run: hivecommons/hive#8311
Hive-Plan: plan-7
Hive-Spec: spectacular.md#S-12
`)
	tl := timeline.NewStore()
	tl.Record(timeline.Event{
		IssueRef: "hivecommons/hive#8311",
		Kind:     timeline.KindStageCompleted,
		Attrs:    map[string]string{"plan": "plan-7", "plan_section": "Implementation"},
	})
	links, err := Resolver{
		GitDir:   dir,
		Timeline: tl,
		Audit: fakeAuditReader{
			{Timestamp: "2026-09-22T20:00:00Z", User: "architect", Action: "agent_rationale", Detail: "run=hivecommons/hive#8311, plan=plan-7, note=why", Agent: "planner"},
			{Timestamp: "2026-09-22T20:01:00Z", User: "owner", Action: "plan_approve", Detail: "epic=e1, run=hivecommons/hive#8311, surface=plan", Agent: "architect"},
		},
	}.ResolveCommit(context.Background(), sha)
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if links.Run != "hivecommons/hive#8311" || links.Plan != "plan-7" || links.Spec != "spectacular.md" || links.Clause != "S-12" {
		t.Fatalf("links trailers = %+v", links)
	}
	if links.PlanSection != "Implementation" {
		t.Fatalf("plan section = %q", links.PlanSection)
	}
	if links.Approval == nil || links.Approval.User != "owner" {
		t.Fatalf("approval = %+v", links.Approval)
	}
	if len(links.Rationale) != 1 || links.Rationale[0].Action != "agent_rationale" {
		t.Fatalf("rationale = %+v", links.Rationale)
	}
}

func TestResolveCommitWithoutTrailersReturnsNoLinkageAndFinding(t *testing.T) {
	dir := initGitRepo(t)
	sha := commitFixture(t, dir, "plain", "plain commit\n")
	sink := &captureSink{}
	links, err := Resolver{GitDir: dir, Sink: sink}.ResolveCommit(context.Background(), sha)
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if !links.NoLinkage || len(links.Missing) != 3 {
		t.Fatalf("links = %+v, want typed no linkage with all trailers missing", links)
	}
	if len(sink.actions) != 1 || sink.actions[0] != AuditArtifactLinkMissing {
		t.Fatalf("audit actions = %+v", sink.actions)
	}
}

func TestResolveCommitMissingRunDoesNotAttachUnrelatedAudit(t *testing.T) {
	dir := initGitRepo(t)
	sha := commitFixture(t, dir, "missing-run", `partial

Hive-Plan: shared-plan
Hive-Spec: spec.md#C-9
`)
	sink := &captureSink{}
	links, err := Resolver{
		GitDir: dir,
		Sink:   sink,
		Audit: fakeAuditReader{
			{Timestamp: "2026-09-22T20:01:00Z", User: "owner", Action: "plan_approve", Detail: "run=other/repo#1, plan=shared-plan"},
		},
	}.ResolveCommit(context.Background(), sha)
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if !links.NoLinkage || links.Approval != nil || len(links.Rationale) != 0 {
		t.Fatalf("links = %+v, want no linkage without unrelated audit", links)
	}
	if len(sink.actions) != 1 || sink.actions[0] != AuditArtifactLinkMissing {
		t.Fatalf("audit actions = %+v", sink.actions)
	}
}

func TestResolveCommitRejectsEmptySHA(t *testing.T) {
	if _, err := ResolveCommit(" "); err == nil {
		t.Fatal("ResolveCommit with empty sha must fail")
	}
}

func TestTrailerAndDetailHelpers(t *testing.T) {
	msg := "subject\n\nHive-Run: o/r#1\nHive-Plan: p1\n"
	missing := MissingTrailers(msg)
	if len(missing) != 1 || missing[0] != TrailerSpec {
		t.Fatalf("MissingTrailers = %+v", missing)
	}
	fields := ParseDetailFields("run=o/r#1, plan=p1, malformed, note=kept")
	if fields["run"] != "o/r#1" || fields["plan"] != "p1" || fields["note"] != "kept" {
		t.Fatalf("ParseDetailFields = %+v", fields)
	}
}

func TestResolveSelectsNewestApprovalAndMatchingPlanSection(t *testing.T) {
	dir := initGitRepo(t)
	sha := commitFixture(t, dir, "matching-plan", `linked

Hive-Run: o/r#2
Hive-Plan: plan-b
Hive-Spec: spec.md#C-2
`)
	tl := timeline.NewStore()
	tl.Record(timeline.Event{IssueRef: "o/r#2", Kind: timeline.KindClassified, Attrs: map[string]string{"plan": "plan-a", "section": "wrong"}})
	tl.Record(timeline.Event{IssueRef: "o/r#2", Kind: timeline.KindStageCompleted, Attrs: map[string]string{"plan": "plan-b", "plan_section_id": "right"}})
	links, err := Resolver{
		GitDir:   dir,
		Timeline: tl,
		Audit: fakeAuditReader{
			{Timestamp: "2026-09-22T20:00:00Z", User: "first", Action: "plan_approve", Detail: "run=o/r#2, plan=plan-b"},
			{Timestamp: "2026-09-22T20:02:00Z", User: "second", Action: "design_approved", Detail: "run=o/r#2"},
			{Timestamp: "2026-09-22T20:03:00Z", User: "system", Action: AuditArtifactLinkMissing, Detail: "run=o/r#2"},
		},
	}.ResolveCommit(context.Background(), sha)
	if err != nil {
		t.Fatalf("ResolveCommit: %v", err)
	}
	if links.PlanSection != "right" {
		t.Fatalf("plan section = %q, want right", links.PlanSection)
	}
	if links.Approval == nil || links.Approval.User != "second" {
		t.Fatalf("approval = %+v, want newest approval", links.Approval)
	}
	if len(links.Rationale) != 0 {
		t.Fatalf("rationale = %+v, want artifact findings excluded", links.Rationale)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Hive Test")
	runGit(t, dir, "config", "user.email", "hive@example.test")
	return dir
}

func commitFixture(t *testing.T, dir, name, message string) string {
	t.Helper()
	path := filepath.Join(dir, name+".txt")
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", message)
	out := runGit(t, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
	return string(out)
}
