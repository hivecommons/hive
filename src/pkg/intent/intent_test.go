package intent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pr   PR
		want Tier
	}{
		{"docs only", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "docs/readme.md", Additions: 3}}}, Tier0},
		{"test additive", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo_test.go", Additions: 8}}}, Tier0},
		{"test deletion escalates", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo_test.go", Additions: 1, Deletions: 1}}}, Tier1},
		{"removed test escalates", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "tests/e2e_test.go", Status: "removed"}}}, Tier1},
		{"guardrail workflow", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: ".github/workflows/ci.yml"}}}, Tier3},
		{"guardrail owners", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "OWNERS"}}}, Tier3},
		{"guardrail dashboard overlay", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "hive.yaml.dashboard"}}}, Tier3},
		{"feature label", PR{AgentAuthor: true, Title: "add widget", Labels: []string{"enhancement"}, Files: []ChangedFile{{Filename: "pkg/widget/widget.go"}}}, Tier2},
		{"default bugfix chore", PR{AgentAuthor: true, Files: []ChangedFile{{Filename: "pkg/foo/foo.go"}}}, Tier1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.pr, Config{}); got.Tier != tt.want {
				t.Fatalf("Tier = %d (%s), want %d", got.Tier, got.Reason, tt.want)
			}
		})
	}
}

func TestAlignmentContextBoundsAndEvidence(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, err := store.Create("Implement proxy retry plan", beads.TypeEpic, beads.PriorityHigh, "architect", "gh-hivecommons/hive#2803")
	if err != nil {
		t.Fatal(err)
	}
	epic.Notes = "Authorized plan keeps changes under pkg/proxy."
	if err := store.Update(epic.ID, func(b *beads.Bead) { b.Notes = epic.Notes }); err != nil {
		t.Fatal(err)
	}
	child, err := store.Create("Add proxy retry tests", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child.ID, BeadParentEpicKey, epic.ID); err != nil {
		t.Fatal(err)
	}
	files := make([]ChangedFile, MaxAlignmentFiles+5)
	for i := range files {
		files[i] = ChangedFile{Filename: strings.Repeat("deep/", 20) + "pkg/proxy/file.go", Additions: 1}
	}
	ctx := BuildAlignmentContext(PR{Title: "proxy retry", Body: "Fixes #2803", Files: files}, []TextEvidence{{
		Source: "issue hivecommons/hive#2803",
		Title:  "Proxy retry bug",
		Body:   "Fix proxy retries.",
	}}, map[string]*beads.Store{"architect": store}, []IssueRef{{Repo: "hivecommons/hive", Number: 2803}})
	if ctx.TruncatedFiles != 5 {
		t.Fatalf("TruncatedFiles = %d, want 5", ctx.TruncatedFiles)
	}
	if len(ctx.Files) != MaxAlignmentFiles {
		t.Fatalf("Files = %d, want %d", len(ctx.Files), MaxAlignmentFiles)
	}
	if len(ctx.CheckFiles) != MaxAlignmentFiles+5 {
		t.Fatalf("CheckFiles = %d, want %d", len(ctx.CheckFiles), MaxAlignmentFiles+5)
	}
	if len(ctx.DiffSummary) > MaxAlignmentBytes {
		t.Fatalf("DiffSummary length = %d, max %d", len(ctx.DiffSummary), MaxAlignmentBytes)
	}
	for _, want := range []string{"Proxy retry bug", "Authorized plan keeps changes", "Add proxy retry tests"} {
		if !strings.Contains(ctx.AuthorizedIntent, want) {
			t.Fatalf("AuthorizedIntent missing %q: %s", want, ctx.AuthorizedIntent)
		}
	}
}

func alignmentContextWithOutOfScopeFileBeyondCap() AlignmentContext {
	files := make([]ChangedFile, MaxAlignmentFiles+1)
	for i := 0; i < MaxAlignmentFiles; i++ {
		files[i] = ChangedFile{Filename: "docs/guide.md", Additions: 1}
	}
	files[MaxAlignmentFiles] = ChangedFile{Filename: "pkg/proxy/rules.go", Additions: 1}
	return BuildAlignmentContext(PR{Title: "docs", Body: "Fixes #1", Files: files}, []TextEvidence{{
		Source: "issue hivecommons/hive#1",
		Title:  "Documentation update",
		Body:   "Update docs only.",
	}}, nil, nil)
}

func TestDeterministicAlignment(t *testing.T) {
	tests := []struct {
		name          string
		ctx           AlignmentContext
		tier          Tier
		want          string
		code          string
		wantTruncated int
	}{
		{
			name: "aligned referenced package",
			ctx: AlignmentContext{
				AuthorizedIntent: "Fix proxy retries in pkg/proxy.",
				PRBody:           "Fixes #1; updates proxy retry handling.",
				Files:            []DiffFileSummary{{Filename: "pkg/proxy/rules.go"}},
			},
			tier: Tier1,
			want: AlignmentStatusAligned,
		},
		{
			name: "misaligned docs intent code diff",
			ctx: AlignmentContext{
				AuthorizedIntent: "Update docs for the install guide.",
				PRBody:           "Fixes #1; documentation update.",
				Files:            []DiffFileSummary{{Filename: "pkg/proxy/rules.go"}},
			},
			tier: Tier1,
			want: AlignmentStatusMisaligned,
			code: "diff-outside-referenced-scope",
		},
		{
			name:          "out-of-scope file beyond prompt cap is still checked",
			ctx:           alignmentContextWithOutOfScopeFileBeyondCap(),
			tier:          Tier1,
			want:          AlignmentStatusMisaligned,
			code:          "diff-outside-referenced-scope",
			wantTruncated: 1,
		},
		{
			name: "generic path segment does not authorize sibling package",
			ctx: AlignmentContext{
				AuthorizedIntent: "Fix pkg/intent authorization behavior.",
				PRBody:           "Fixes #1; updates intent verification.",
				Files:            []DiffFileSummary{{Filename: "pkg/config/config.go"}},
			},
			tier: Tier1,
			want: AlignmentStatusMisaligned,
			code: "diff-outside-referenced-scope",
		},
		{
			name: "explicit two segment path authorizes matching subtree",
			ctx: AlignmentContext{
				AuthorizedIntent: "Fix cmd/hive merge-gate wiring.",
				PRBody:           "Fixes #1; updates cmd/hive.",
				Files:            []DiffFileSummary{{Filename: "cmd/hive/main.go"}},
			},
			tier: Tier1,
			want: AlignmentStatusAligned,
		},
		{
			name: "tier0 escape",
			ctx: AlignmentContext{
				AuthorizedIntent: "Add tests.",
				PRBody:           "Test-only change.",
				Files:            []DiffFileSummary{{Filename: "pkg/proxy/rules.go"}},
			},
			tier: Tier0,
			want: AlignmentStatusMisaligned,
			code: "tier0-diff-escape",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateAlignment(tt.ctx, tt.tier, Config{})
			if got.Status != tt.want {
				t.Fatalf("Status = %q, want %q: %+v", got.Status, tt.want, got)
			}
			if tt.wantTruncated != 0 && tt.ctx.TruncatedFiles != tt.wantTruncated {
				t.Fatalf("TruncatedFiles = %d, want %d", tt.ctx.TruncatedFiles, tt.wantTruncated)
			}
			if tt.code != "" {
				found := false
				for _, f := range got.DeterministicFindings {
					found = found || f.Code == tt.code
				}
				if !found {
					t.Fatalf("missing finding code %q: %+v", tt.code, got.DeterministicFindings)
				}
			}
		})
	}
}

func TestVerdictSerializationBackwardCompatible(t *testing.T) {
	oldJSON := []byte(`{"tier":1,"authorized":true,"reason":"linked issue evidence present","evidence":{"LinkedIssue":true},"agent_pr":true}`)
	var old Verdict
	if err := json.Unmarshal(oldJSON, &old); err != nil {
		t.Fatal(err)
	}
	if old.Alignment != nil {
		t.Fatalf("old verdict alignment = %+v, want nil", old.Alignment)
	}
	next := Verdict{Tier: Tier1, Authorized: true, Reason: ReasonLinkedIssue, AgentPR: true, Alignment: &AlignmentVerdict{Status: AlignmentStatusAligned}}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"alignment"`) {
		t.Fatalf("new verdict did not include alignment: %s", data)
	}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		tier Tier
		ev   Evidence
		want bool
	}{
		{"tier0 allowed", Tier0, Evidence{}, true},
		{"tier1 needs issue", Tier1, Evidence{}, false},
		{"tier1 linked issue", Tier1, Evidence{LinkedIssue: true}, true},
		{"tier2 needs plan", Tier2, Evidence{LinkedIssue: true}, false},
		{"tier2 approved plan", Tier2, Evidence{ApprovedPlan: true}, true},
		{"tier2 human approval", Tier2, Evidence{HumanApproval: true}, true},
		{"tier3 ignores plan", Tier3, Evidence{ApprovedPlan: true}, false},
		{"tier3 human approval", Tier3, Evidence{HumanApproval: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(Classification{Tier: tt.tier, AgentPR: true}, tt.ev)
			if got.Authorized != tt.want {
				t.Fatalf("Authorized = %v (%s), want %v", got.Authorized, got.Reason, tt.want)
			}
		})
	}
}

func TestEvidence(t *testing.T) {
	if !LinkedIssueInBody("Fixes #2803") || !LinkedIssueInBody("Closes hivecommons/hive#2803") {
		t.Fatal("expected closing issue refs to be detected")
	}
	for _, body := range []string{"Refs #2803", "Ref #2803", "Part of #2803", "Related to hivecommons/hive#2803"} {
		if !LinkedIssueInBody(body) {
			t.Fatalf("expected non-closing issue ref %q to be detected", body)
		}
		ev := Evidence{LinkedIssue: LinkedIssueInBody(body)}
		if !ev.LinkedIssue {
			t.Fatalf("expected LinkedIssue true for %q", body)
		}
	}
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, _ := store.Create("epic", beads.TypeEpic, beads.PriorityHigh, "architect", "gh-hivecommons/hive#2803")
	if err := store.SetMetadata(epic.ID, BeadPlanStatusKey, BeadPlanApprovedStatus); err != nil {
		t.Fatal(err)
	}
	ev := BuildEvidenceForRepo("Fixes #2803", "hivecommons/hive", map[string]*beads.Store{"architect": store}, false)
	if !ev.LinkedIssue || !ev.ApprovedPlan {
		t.Fatalf("evidence = %+v, want linked issue and approved plan", ev)
	}
	unrelated := BuildEvidenceForRepo("Fixes #9999", "hivecommons/hive", map[string]*beads.Store{"architect": store}, false)
	if unrelated.ApprovedPlan {
		t.Fatalf("unrelated approved plan should not authorize PR evidence: %+v", unrelated)
	}
	otherRepoSameNumber := BuildEvidenceForRepo("Fixes #2803", "other/repo", map[string]*beads.Store{"architect": store}, false)
	if otherRepoSameNumber.ApprovedPlan {
		t.Fatalf("same issue number in another repo should not match approved plan: %+v", otherRepoSameNumber)
	}
}
