package review

import (
	"strings"
	"testing"
)

// hunk joins diff body lines into one unified hunk the GitHub files API
// shape: no file header, a synthetic @@ line, then the +/-/space lines.
func hunk(lines ...string) string {
	return "@@ -1,8 +1,8 @@\n" + strings.Join(lines, "\n")
}

func TestWorkflowPermissionsTighteningOnly(t *testing.T) {
	cases := []struct {
		name  string
		files []ChangedFile
		want  bool
	}{
		{
			name: "workflow-level write to read (pluk#305 shape)",
			files: []ChangedFile{{Path: ".github/workflows/publish.yml", Status: "modified", Patch: hunk(
				" permissions:",
				"-  contents: write",
				"-  id-token: write",
				"+  contents: read",
				" ",
				" jobs:",
				"   publish:",
				"     permissions:",
				"       contents: read",
				"       id-token: write",
			)}},
			want: true,
		},
		{
			name: "scope removed entirely",
			files: []ChangedFile{{Path: ".github/workflows/ci.yaml", Patch: hunk(
				" permissions:",
				"   contents: read",
				"-  pull-requests: write",
				" jobs:",
			)}},
			want: true,
		},
		{
			name: "block replaced by empty permissions",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-permissions:",
				"-  contents: write",
				"+permissions: {}",
				" jobs:",
			)}},
			want: true,
		},
		{
			name: "write-all narrowed to read-all",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-permissions: write-all",
				"+permissions: read-all",
			)}},
			want: true,
		},
		{
			name: "tightening plus an unrelated non-sensitive file",
			files: []ChangedFile{
				{Path: ".github/workflows/ci.yml", Patch: hunk("-  contents: write", "+  contents: read")},
				{Path: "docs/ci.md", Patch: hunk("-old", "+new")},
			},
			want: true,
		},
		{
			name: "widening read to write",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				" permissions:",
				"-  contents: read",
				"+  contents: write",
			)}},
			want: false,
		},
		{
			name: "new scope added to an existing block",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				" permissions:",
				"   contents: read",
				"+  pull-requests: read",
			)}},
			want: false,
		},
		{
			name: "tightening plus a new step",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-  contents: write",
				"+  contents: read",
				" jobs:",
				"   build:",
				"     steps:",
				"+      - run: curl -sSf https://example.com/install.sh | sh",
			)}},
			want: false,
		},
		{
			name: "tightening plus a new pull_request_target trigger",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				" on:",
				"   pull_request:",
				"+  pull_request_target:",
				" permissions:",
				"-  contents: write",
				"+  contents: read",
			)}},
			want: false,
		},
		{
			name: "tightening plus a new id-token write on a job",
			files: []ChangedFile{{Path: ".github/workflows/publish.yml", Patch: hunk(
				" permissions:",
				"-  contents: write",
				"+  contents: read",
				" jobs:",
				"   publish:",
				"     permissions:",
				"       contents: read",
				"+      id-token: write",
			)}},
			want: false,
		},
		{
			name: "tightening plus a new secrets reference",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-  contents: write",
				"+  contents: read",
				"+        token: ${{ secrets.DEPLOY_TOKEN }}",
			)}},
			want: false,
		},
		{
			name: "tightening plus a changed uses ref",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-  contents: write",
				"+  contents: read",
				"-      - uses: actions/checkout@v4",
				"+      - uses: actions/checkout@main",
			)}},
			want: false,
		},
		{
			name: "job permissions block removed (inherits wider workflow grant)",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"   build:",
				"-    permissions:",
				"-      contents: read",
				"     steps:",
			)}},
			want: false,
		},
		{
			name: "lowered in one job paid for by a raise in another",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"   a:",
				"     permissions:",
				"-      contents: write",
				"   b:",
				"     permissions:",
				"+      contents: read",
			)}},
			want: false,
		},
		{
			name: "new permissions block with scopes",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"+permissions:",
				"+  contents: read",
			)}},
			want: false,
		},
		{
			name: "read-all is not provably narrower than a bare block",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-permissions:",
				"-  contents: read",
				"+permissions: read-all",
			)}},
			want: false,
		},
		{
			name: "non-permission key with a permission-looking value",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: hunk(
				"-        mode: write",
				"+        mode: read",
			)}},
			want: false,
		},
		{
			name: "tightening plus a sensitive non-workflow path",
			files: []ChangedFile{
				{Path: ".github/workflows/ci.yml", Patch: hunk("-  contents: write", "+  contents: read")},
				{Path: "OWNERS", Patch: hunk("+- mallory")},
			},
			want: false,
		},
		{
			name:  "deleted workflow",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Status: "removed", Patch: hunk("-permissions:", "-  contents: write")}},
			want:  false,
		},
		{
			name:  "renamed workflow",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Status: "renamed", Patch: hunk("-  contents: write", "+  contents: read")}},
			want:  false,
		},
		{
			name:  "missing patch fails closed",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml"}},
			want:  false,
		},
		{
			name:  "patch without hunks fails closed",
			files: []ChangedFile{{Path: ".github/workflows/ci.yml", Patch: "-  contents: write\n+  contents: read"}},
			want:  false,
		},
		{
			name:  "no workflow touched",
			files: []ChangedFile{{Path: "pkg/x/x.go", Patch: hunk("-a", "+b")}},
			want:  false,
		},
		{
			name:  "no files",
			files: nil,
			want:  false,
		},
		{
			name:  "empty path fails closed",
			files: []ChangedFile{{Path: " ", Patch: hunk("-  contents: write", "+  contents: read")}},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorkflowPermissionsTighteningOnly(tc.files); got != tc.want {
				t.Fatalf("WorkflowPermissionsTighteningOnly = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecisionQuestionRiskClassForPermissionsTightening(t *testing.T) {
	cases := []struct {
		question   string
		tightening bool
		class      string
	}{
		{"please confirm the permissions narrowing is acceptable to merge", false, "permissions-credentials"},
		{"please confirm the permissions narrowing is acceptable to merge", true, ""},
		{"edit .github/workflows/ci.yml to remove a scope?", true, ""},
		// Other classes are untouched by the carve-out.
		{"publish the release to ghcr now?", true, "external-side-effects"},
		{"is this change to OWNERS acceptable?", false, "sensitive-paths"},
		{"should the deploy be irreversible?", true, "data-loss-irreversible"},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			got, risk := DecisionQuestionRiskClassFor(tc.question, tc.tightening)
			if got != tc.class || risk != (tc.class != "") {
				t.Fatalf("DecisionQuestionRiskClassFor(%q, %v) = (%q, %v), want %q", tc.question, tc.tightening, got, risk, tc.class)
			}
		})
	}
}

// pluk#305: a Hive-authored tightening-only PR whose reviewer parked it with
// "confirm the permissions narrowing" must resolve by policy, not park at 085.
func TestAggregateReportsResolvesPermissionsTighteningDecision(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.PermissionsTighteningOnly = true
	r.DecisionRequested = true
	r.DecisionQuestion = "please confirm the workflow permissions narrowing (id-token: write moved to the job) is acceptable to merge"
	r.Summary = "Permissions narrowed at workflow level; looks correct."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictApprove || got.RequiresHuman || got.DecisionRequested || !got.MergeEligible {
		t.Fatalf("aggregate = %+v, want approve resolved by policy", got)
	}
	if !got.PermissionsTighteningOnly {
		t.Fatalf("aggregate did not carry PermissionsTighteningOnly: %+v", got)
	}
	found := false
	for _, reason := range got.Reasons {
		if reason == PermissionsTighteningReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("Reasons = %v, want %q recorded for audit", got.Reasons, PermissionsTighteningReason)
	}
	if norm := got.Normalize(); norm.Verdict != VerdictApprove {
		t.Fatalf("Normalize() = %+v, want approve preserved", norm)
	}
}

// The same question without the structural proof still parks: the flag is
// only ever set by the relay after WorkflowPermissionsTighteningOnly.
func TestAggregateReportsPermissionsQuestionStillParksWithoutProof(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.DecisionRequested = true
	r.DecisionQuestion = "please confirm the workflow permissions narrowing is acceptable to merge"

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want requires_human", got)
	}
	for _, reason := range got.Reasons {
		if reason == PermissionsTighteningReason {
			t.Fatalf("Reasons = %v, must not claim a tightening suppression", got.Reasons)
		}
	}
}

// A requires_human whose only risk signal is the word "permission" in the
// summary resolves to an advisory approve on a proved tightening.
func TestAggregateReportsPermissionsTighteningSummaryIsAdvisory(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.PermissionsTighteningOnly = true
	r.Summary = "Looks right; workflow permissions narrowed, nothing widened."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictApprove || got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want advisory approve", got)
	}
}

// Other risk classes survive the carve-out: a tightening PR whose reviewer
// raises a genuine security question still reaches a person.
func TestAggregateReportsPermissionsTighteningKeepsOtherRiskClasses(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.PermissionsTighteningOnly = true
	r.DecisionRequested = true
	r.DecisionQuestion = "the narrowed workflow also publishes to the npm registry; should it?"

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want requires_human for external-side-effects", got)
	}
}

// The carve-out is for Hive-authored PRs only; a contributor's tightening
// keeps the human hold the lane always had.
func TestAggregateReportsPermissionsTighteningNonHiveStillParks(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.PermissionsTighteningOnly = true
	r.DecisionRequested = true
	r.DecisionQuestion = "please confirm the permissions narrowing is acceptable"

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want requires_human for a non-hive author", got)
	}
}

func TestPromptsCarryPermissionsTighteningGuidance(t *testing.T) {
	pr := PullRequest{Repo: "o/r", Number: 1, Title: "ci: narrow permissions", HeadSHA: "abc"}
	for name, prompt := range map[string]string{
		"single":   BuildPerspectivePromptWith(PerspectiveSecurity, pr, PromptOptions{}),
		"combined": BuildCombinedPrompt(pr, []Perspective{PerspectiveSecurity}, PromptOptions{}),
	} {
		if !strings.Contains(prompt, "only removes `permissions:` scopes or lowers them") {
			t.Fatalf("%s prompt lacks the permissions-tightening carve-out", name)
		}
	}
	publish := BuildPerspectivePromptWith(PerspectiveSecurity, pr, PromptOptions{PostComments: true})
	if !strings.Contains(publish, "is NOT a permissions/sensitive-path risk class") {
		t.Fatalf("publish instruction lacks the decision-question carve-out")
	}
}
