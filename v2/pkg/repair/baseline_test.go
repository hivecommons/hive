package repair

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type fakeReviewPRClient struct {
	calls int
	body  string
	state *Store
}

func (f *fakeReviewPRClient) UpsertReviewPullRequest(_ context.Context, _, branch, _, _, _ string, body, _ string) (hivegithub.RepairPullRequest, error) {
	f.calls++
	f.body = body
	head := ""
	if f.state != nil {
		for _, attempt := range f.state.Snapshot().Attempts {
			if attempt != nil && attempt.BaselineReview != nil && attempt.BaselineReview.ProposalBranch == branch {
				head = attempt.BaselineReview.ProposalCommitSHA
				break
			}
		}
	}
	return hivegithub.RepairPullRequest{Number: 29, URL: "https://example.test/pull/29", HeadSHA: head}, nil
}

func TestDetectBaselineReviewAcceptsExclusiveMissingBaselineBlock(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, ".visual-hive", "artifacts", "screenshots", "contract__mobile__mobile.png")
	writeTestPNG(t, actual)
	writeBaselineReport(t, root, map[string]any{
		"status":         "failed",
		"summary":        map[string]any{"missingBaselines": 1, "visualDiffs": 0, "consoleErrors": 0, "pageErrors": 0, "flowStepsFailed": 0},
		"verdictSummary": map[string]any{"visualHiveVerdict": "blocked", "failedBecause": []string{}},
		"verdictContributions": []map[string]any{
			{"kind": "deterministic_run", "status": "blocked", "gating": true},
			{"kind": "contract_result", "status": "blocked", "gating": true},
			{"kind": "missing_baseline", "status": "blocked", "gating": true},
		},
		"results": []map[string]any{{"screenshotAssertions": []map[string]any{{
			"contractId": "contract", "screenshotName": "mobile", "route": "/", "viewport": "mobile", "status": "missing_baseline",
			"baselinePath": filepath.Join(root, "visual-hive.baselines", "win32", "contract__mobile__mobile.png"), "actualPath": actual,
		}}}},
	})
	review, recognized, err := DetectBaselineReview(root)
	if err != nil {
		t.Fatal(err)
	}
	if !recognized || review == nil || len(review.Candidates) != 1 {
		t.Fatalf("missing baseline was not recognized: review=%+v recognized=%t", review, recognized)
	}
	candidate := review.Candidates[0]
	if candidate.Platform != "win32" || candidate.Source != "local_validation" || candidate.SHA256 == "" || candidate.BaselinePath != "visual-hive.baselines/win32/contract__mobile__mobile.png" {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
}

func TestDetectBaselineReviewRejectsMixedFailure(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, ".visual-hive", "artifacts", "screenshots", "contract__mobile__mobile.png")
	writeTestPNG(t, actual)
	writeBaselineReport(t, root, map[string]any{
		"status":         "failed",
		"summary":        map[string]any{"missingBaselines": 1, "visualDiffs": 0, "consoleErrors": 1, "pageErrors": 0, "flowStepsFailed": 0},
		"verdictSummary": map[string]any{"visualHiveVerdict": "failed", "failedBecause": []string{"playwright.console_error.contract"}},
		"verdictContributions": []map[string]any{
			{"kind": "missing_baseline", "status": "blocked", "gating": true},
			{"kind": "console_error", "status": "failed", "gating": true},
		},
		"results": []map[string]any{{"screenshotAssertions": []map[string]any{{
			"contractId": "contract", "screenshotName": "mobile", "status": "missing_baseline",
			"baselinePath": filepath.Join(root, "visual-hive.baselines", "win32", "contract__mobile__mobile.png"), "actualPath": actual,
		}}}},
	})
	if review, recognized, err := DetectBaselineReview(root); err != nil || recognized || review != nil {
		t.Fatalf("mixed failure must not bypass validation: review=%+v recognized=%t err=%v", review, recognized, err)
	}
}

func TestReadHostedBaselineReviewSupportsStrippedArtifactRoot(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "artifacts", "screenshots", "contract__mobile__mobile.png")
	writeTestPNG(t, actual)
	report := map[string]any{
		"status":               "failed",
		"summary":              map[string]any{"missingBaselines": 1, "visualDiffs": 0, "consoleErrors": 0, "pageErrors": 0, "flowStepsFailed": 0},
		"verdictSummary":       map[string]any{"visualHiveVerdict": "blocked", "failedBecause": []string{}},
		"verdictContributions": []map[string]any{{"kind": "missing_baseline", "status": "blocked", "gating": true}},
		"results": []map[string]any{{"screenshotAssertions": []map[string]any{{
			"contractId": "contract", "screenshotName": "mobile", "route": "/", "viewport": "mobile", "status": "missing_baseline",
			"baselinePath": "/home/runner/work/repo/repo/visual-hive.baselines/linux/contract__mobile__mobile.png",
			"actualPath":   "/home/runner/work/repo/repo/.visual-hive/artifacts/screenshots/contract__mobile__mobile.png",
		}}}},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	review, recognized, err := ReadHostedBaselineReview(root)
	if err != nil {
		t.Fatal(err)
	}
	if !recognized || len(review.Candidates) != 1 || review.Candidates[0].Platform != "linux" || review.Candidates[0].ActualPath != actual {
		t.Fatalf("stripped hosted artifact was not recognized: review=%+v recognized=%t", review, recognized)
	}
}

func TestDetectBaselineReviewRejectsUnsafeBaselinePath(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, ".visual-hive", "artifacts", "screenshots", "candidate.png")
	writeTestPNG(t, actual)
	writeBaselineReport(t, root, map[string]any{
		"status":               "failed",
		"summary":              map[string]any{"missingBaselines": 1},
		"verdictSummary":       map[string]any{"visualHiveVerdict": "blocked", "failedBecause": []string{}},
		"verdictContributions": []map[string]any{{"kind": "missing_baseline", "status": "blocked", "gating": true}},
		"results": []map[string]any{{"screenshotAssertions": []map[string]any{{
			"contractId": "contract", "screenshotName": "mobile", "status": "missing_baseline",
			"baselinePath": "visual-hive.baselines/linux/../../secrets.png", "actualPath": actual,
		}}}},
	})
	if _, recognized, err := DetectBaselineReview(root); err == nil || recognized {
		t.Fatalf("unsafe baseline path was accepted: recognized=%t err=%v", recognized, err)
	}
}

func TestCreateBaselineProposalUsesSeparateHeldBranchAndPersistsState(t *testing.T) {
	repository, remote := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	localRoot, hostedRoot := t.TempDir(), t.TempDir()
	local := testBaselineCandidate(t, localRoot, "win32", "local_validation")
	hosted := testBaselineCandidate(t, hostedRoot, "linux", "github_actions")
	fingerprint := "owner/repo:baseline-proposal"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, Branch: "hive/repair-one", Worktree: filepath.Join(t.TempDir(), "repair"),
		Stage: StagePROpen, Provider: "test", CommitSHA: "repair-head", PRNumber: 17, PRURL: "https://example.test/pull/17",
		BaselineReview: &BaselineReview{Status: BaselineReviewCandidateReady, Candidates: []BaselineCandidate{local}}, StartedAt: testNow,
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	client := &fakeReviewPRClient{state: state}
	finding := visualhive.FindingLifecycle{Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, IssueNumber: 48, IssueURL: "https://example.test/issues/48"}
	updated, err := CreateBaselineProposal(context.Background(), BaselineProposalConfig{
		RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "baseline-worktrees"), BaseBranch: "main", ExpectedRemoteURL: remote,
	}, finding, BaselineProposalSource{WorkflowRunID: 77, ArtifactID: 88, RunURL: "https://example.test/actions/runs/77"},
		&BaselineReview{Status: BaselineReviewCandidateReady, Candidates: []BaselineCandidate{hosted}}, state, client)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 || updated.BaselineReview.Status != BaselineReviewProposalOpen || updated.BaselineReview.ProposalPRNumber != 29 || len(updated.BaselineReview.Candidates) != 2 {
		t.Fatalf("unexpected proposal result: %+v calls=%d", updated.BaselineReview, client.calls)
	}
	for _, candidate := range []BaselineCandidate{local, hosted} {
		if output := gitOutput(t, remote, "show", updated.BaselineReview.ProposalBranch+":"+candidate.BaselinePath); len(output) == 0 {
			t.Fatalf("candidate %s was not pushed", candidate.BaselinePath)
		}
	}
	proposalMessage := gitOutput(t, remote, "show", "-s", "--format=%B", updated.BaselineReview.ProposalBranch)
	if !hasExactRepairTrailer(proposalMessage, "Hive-Repository-ID", "123") || !hasExactRepairTrailer(proposalMessage, "Hive-Operation", "baseline") {
		t.Fatalf("baseline proposal lacks repository-scoped ownership: %q", proposalMessage)
	}
	if !containsAll(client.body, "Required review", "Exact repair head", local.SHA256, hosted.SHA256) {
		t.Fatalf("review body lacks exact evidence: %s", client.body)
	}
	resumed, err := CreateBaselineProposal(context.Background(), BaselineProposalConfig{
		RepositoryDir: repository, WorktreeRoot: filepath.Join(t.TempDir(), "unused"), BaseBranch: "main", ExpectedRemoteURL: remote,
	}, finding, BaselineProposalSource{}, nil, state, client)
	if err != nil || resumed.BaselineReview.ProposalPRNumber != 29 || client.calls != 1 {
		t.Fatalf("proposal retry was not idempotent: %+v calls=%d err=%v", resumed.BaselineReview, client.calls, err)
	}
}

func writeBaselineReport(t *testing.T, root string, report map[string]any) {
	t.Helper()
	directory := filepath.Join(root, ".visual-hive")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestPNG(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, []byte("bounded-test-png")...)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

var testNow = func() (value time.Time) { return time.Unix(1, 0).UTC() }()

func testBaselineCandidate(t *testing.T, root, platform, source string) BaselineCandidate {
	t.Helper()
	actual := filepath.Join(root, ".visual-hive", "artifacts", "screenshots", "contract__mobile__mobile.png")
	writeTestPNG(t, actual)
	digest, bytes, err := inspectPNG(root, actual)
	if err != nil {
		t.Fatal(err)
	}
	return BaselineCandidate{
		ContractID: "contract", ScreenshotName: "mobile", Route: "/", Viewport: "mobile", Platform: platform,
		ActualPath: actual, BaselinePath: "visual-hive.baselines/" + platform + "/contract__mobile__mobile.png", SHA256: digest, Bytes: bytes, Source: source,
	}
}

func containsAll(value string, values ...string) bool {
	for _, candidate := range values {
		if !strings.Contains(value, candidate) {
			return false
		}
	}
	return true
}
