package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/internal/visualhivepr"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	testPRBaseSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testPRHeadSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

type pullRequestV3Fixture struct {
	BundleZip              []byte
	SourceZip              []byte
	Workflow               []byte
	Config                 []byte
	PlanSHA256             string
	ReportSHA256           string
	ConfigSHA256           string
	ChangedFilesSHA256     string
	PlanContractsSHA256    string
	EvaluationScopesSHA256 string
	WorkflowSHA256         string
	RuntimeStableSHA256    string
	ExecutionBindingSHA256 string
	BaselineSHA256         string
	RuntimeReceiptSHA256   string
	Baseline               []byte
}

type pullRequestServerMutation struct {
	PRState                        string
	Merged                         bool
	BaseRepository                 string
	BaseRepositoryID               int64
	DefaultBranch                  string
	DefaultBranchSHA               string
	BaseBranch                     string
	BaseSHA                        string
	HeadRepository                 string
	HeadRepositoryID               int64
	HeadBranch                     string
	HeadSHA                        string
	RunAttempt                     int
	RunWorkflowID                  int64
	RunEvent                       string
	RunStatus                      string
	RunConclusion                  string
	RunName                        string
	RunPath                        string
	RunBaseRepository              string
	RunBaseRepositoryID            int64
	RunHeadRepository              string
	RunHeadRepositoryID            int64
	ExtraMatchingRun               bool
	WrongRunAssociation            bool
	WrongAssociationRepositories   bool
	ExtraRunAssociation            bool
	OmitRunAssociations            bool
	MinimalAssociationRepositories bool
	WorkflowName                   string
	WorkflowPath                   string
	WorkflowState                  string
	WorkflowContent                []byte
	HeadWorkflowContent            []byte
	ConfigContent                  []byte
	BaselineContent                []byte
	BundleArtifactID               int64
	BundleArtifactName             string
	SourceArtifactID               int64
	SourceArtifactName             string
	ArtifactRunID                  int64
	ArtifactRepositoryID           int64
	ArtifactHeadRepositoryID       int64
	ArtifactHeadBranch             string
	ArtifactHeadSHA                string
	ArtifactsExpired               bool
	ExpireArtifactsOnSecondList    bool
	BundleZip                      []byte
	SourceZip                      []byte
	CloseOnSecondPRGet             bool
	DriftOnSecondRunGet            bool
	DriftDefaultBranchOnSecondGet  bool
	DriftBaseBranchOnSecondGet     bool
	CheckAppID                     int64
}

func TestFetchAndVerifyVisualHivePullRequestBundleBindsExactPRProvenance(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{})
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	request := pullRequestBundleRequest(t, fixture)
	bundle, verified, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := verified.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	identity := receipt.Identity
	if bundle.Validation.Trusted || bundle.Validation.Authoritative || identity.Authority != (visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}) || identity.Source.PullRequest != 7 ||
		identity.Source.Head.SHA != testPRHeadSHA || identity.Source.Base.SHA != testPRBaseSHA || identity.Workflow.RunID != "77" || identity.Workflow.RunAttempt != "3" ||
		identity.Workflow.CheckSuiteID != "501" || identity.Workflow.JobID != "700" || identity.Workflow.CheckRunID != "900" || identity.Workflow.AppID != "15368" ||
		identity.BundleArtifact.ID != "99" || identity.SourceArtifact.ID != "98" || identity.BundleArtifact.Name == identity.SourceArtifact.Name ||
		identity.Source.Plan.SHA256 != fixture.PlanSHA256 || identity.Source.Report.SHA256 != fixture.ReportSHA256 || identity.Source.Config.SHA256 != fixture.ConfigSHA256 || identity.Source.ChangedFiles.SHA256 != fixture.ChangedFilesSHA256 || identity.Source.Runtime.StableSHA256 != fixture.RuntimeStableSHA256 ||
		identity.Source.PlanContracts.SHA256 != fixture.PlanContractsSHA256 || identity.Source.EvaluationScopes.SHA256 != fixture.EvaluationScopesSHA256 ||
		identity.Source.Runtime.ExecutionBindingSHA256 != fixture.ExecutionBindingSHA256 || identity.Source.Baselines.SHA256 != fixture.BaselineSHA256 {
		t.Fatalf("unexpected verified PR bundle: validation=%+v receipt=%+v", bundle.Validation, receipt)
	}
}

func TestFetchAndVerifyVisualHivePullRequestBundleAcceptsGitHubForkAssociationShapes(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	for _, test := range []struct {
		name     string
		mutation pullRequestServerMutation
	}{
		{name: "omitted fork association", mutation: pullRequestServerMutation{OmitRunAssociations: true}},
		{name: "minimal association repositories", mutation: pullRequestServerMutation{MinimalAssociationRepositories: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newPullRequestVerifierServer(t, fixture, test.mutation)
			defer server.Close()
			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			if _, _, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), pullRequestBundleRequest(t, fixture)); err != nil {
				t.Fatalf("real GitHub fork association shape was rejected: %v", err)
			}
		})
	}
}

func TestFetchAndVerifyVisualHivePullRequestBundleClassifiesMissingSuccessfulRun(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{RunConclusion: "failure"})
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, _, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), pullRequestBundleRequest(t, fixture))
	if !errors.Is(err, ErrNoSuccessfulVisualHivePullRequestRun) {
		t.Fatalf("failed exact-head workflow error = %v, want unavailable-success sentinel", err)
	}
}

func TestFetchAndVerifyVisualHivePullRequestBundleRejectsAdversarialBindings(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	mixed := buildPullRequestV3Fixture(t, strings.Repeat("d", 40))
	forged := buildPullRequestV3FixtureWithBindingMutation(t, testPRHeadSHA, func(binding *visualhive.PullRequestSourceBinding) {
		binding.Plan.SHA256 = strings.Repeat("f", 64)
	})
	tests := []struct {
		name          string
		mutateServer  func(*pullRequestServerMutation)
		mutateRequest func(*VisualHivePullRequestBundleRequest)
	}{
		{name: "wrong PR", mutateRequest: func(r *VisualHivePullRequestBundleRequest) { r.PullRequestNumber = 8 }},
		{name: "closed PR", mutateServer: func(m *pullRequestServerMutation) { m.PRState = "closed" }},
		{name: "merged PR", mutateServer: func(m *pullRequestServerMutation) { m.Merged = true }},
		{name: "wrong base repo", mutateServer: func(m *pullRequestServerMutation) { m.BaseRepository = "other/repo" }},
		{name: "wrong base repo id", mutateServer: func(m *pullRequestServerMutation) { m.BaseRepositoryID = 124 }},
		{name: "non-default base definition", mutateServer: func(m *pullRequestServerMutation) { m.DefaultBranch = "release" }},
		{name: "default branch revision drift", mutateServer: func(m *pullRequestServerMutation) { m.DefaultBranchSHA = strings.Repeat("e", 40) }},
		{name: "wrong base branch", mutateServer: func(m *pullRequestServerMutation) { m.BaseBranch = "release" }},
		{name: "wrong base sha", mutateServer: func(m *pullRequestServerMutation) { m.BaseSHA = strings.Repeat("e", 40) }},
		{name: "fork repo drift", mutateServer: func(m *pullRequestServerMutation) { m.HeadRepository = "attacker/repo" }},
		{name: "wrong head repo id", mutateServer: func(m *pullRequestServerMutation) { m.HeadRepositoryID = 457 }},
		{name: "wrong head branch", mutateServer: func(m *pullRequestServerMutation) { m.HeadBranch = "other" }},
		{name: "wrong head sha", mutateServer: func(m *pullRequestServerMutation) { m.HeadSHA = strings.Repeat("e", 40) }},
		{name: "ambiguous exact run selection", mutateServer: func(m *pullRequestServerMutation) { m.ExtraMatchingRun = true }},
		{name: "wrong attempt", mutateServer: func(m *pullRequestServerMutation) { m.RunAttempt = 4 }},
		{name: "wrong workflow id", mutateServer: func(m *pullRequestServerMutation) { m.RunWorkflowID = 13 }},
		{name: "wrong workflow name", mutateServer: func(m *pullRequestServerMutation) { m.RunName = "Attacker Workflow" }},
		{name: "wrong workflow path", mutateServer: func(m *pullRequestServerMutation) { m.RunPath = ".github/workflows/other.yml" }},
		{name: "pull request target", mutateServer: func(m *pullRequestServerMutation) { m.RunEvent = "pull_request_target" }},
		{name: "incomplete run", mutateServer: func(m *pullRequestServerMutation) { m.RunStatus = "in_progress" }},
		{name: "wrong run conclusion", mutateServer: func(m *pullRequestServerMutation) { m.RunConclusion = "failure" }},
		{name: "wrong run base repo", mutateServer: func(m *pullRequestServerMutation) { m.RunBaseRepository = "other/repo" }},
		{name: "wrong run head repo", mutateServer: func(m *pullRequestServerMutation) { m.RunHeadRepository = "other/repo" }},
		{name: "missing exact PR association", mutateServer: func(m *pullRequestServerMutation) { m.WrongRunAssociation = true }},
		{name: "PR association repository drift", mutateServer: func(m *pullRequestServerMutation) { m.WrongAssociationRepositories = true }},
		{name: "ambiguous PR associations", mutateServer: func(m *pullRequestServerMutation) { m.ExtraRunAssociation = true }},
		{name: "merge workflow ref", mutateServer: func(m *pullRequestServerMutation) {
			m.RunPath = ".github/workflows/visual-hive-pr.yml@refs/pull/7/merge"
		}},
		{name: "merge head ref", mutateRequest: func(r *VisualHivePullRequestBundleRequest) { r.ExpectedHeadBranch = "refs/pull/7/merge" }},
		{name: "workflow definition drift", mutateServer: func(m *pullRequestServerMutation) { m.WorkflowContent = []byte("name: attacker\n") }},
		{name: "inactive workflow definition", mutateServer: func(m *pullRequestServerMutation) { m.WorkflowState = "disabled_manually" }},
		{name: "workflow API name drift", mutateServer: func(m *pullRequestServerMutation) { m.WorkflowName = "Other Workflow" }},
		{name: "workflow API path drift", mutateServer: func(m *pullRequestServerMutation) { m.WorkflowPath = ".github/workflows/other.yml" }},
		{name: "PR head workflow drift", mutateServer: func(m *pullRequestServerMutation) {
			m.HeadWorkflowContent = []byte("name: attacker-controlled-head-workflow\n")
		}},
		{name: "config drift", mutateServer: func(m *pullRequestServerMutation) { m.ConfigContent = []byte("project: attacker\n") }},
		{name: "wrong source artifact name", mutateServer: func(m *pullRequestServerMutation) { m.SourceArtifactName = "visual-hive-pr-source-wrong" }},
		{name: "duplicate artifact name", mutateServer: func(m *pullRequestServerMutation) { m.SourceArtifactName = "visual-hive-pr-bundle-77-3" }},
		{name: "artifact run drift", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactRunID = 78 }},
		{name: "artifact repository drift", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactRepositoryID = 124 }},
		{name: "artifact head repository drift", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactHeadRepositoryID = 457 }},
		{name: "artifact head branch drift", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactHeadBranch = "other" }},
		{name: "artifact head drift", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactHeadSHA = strings.Repeat("e", 40) }},
		{name: "expired artifacts", mutateServer: func(m *pullRequestServerMutation) { m.ArtifactsExpired = true }},
		{name: "artifacts expire during verification", mutateServer: func(m *pullRequestServerMutation) { m.ExpireArtifactsOnSecondList = true }},
		{name: "same artifacts", mutateServer: func(m *pullRequestServerMutation) { m.SourceArtifactID = 99 }},
		{name: "wrong GitHub Actions App", mutateServer: func(m *pullRequestServerMutation) { m.CheckAppID = 999 }},
		{name: "producer drift", mutateRequest: func(r *VisualHivePullRequestBundleRequest) { r.ExpectedProducerGitCommit = strings.Repeat("f", 40) }},
		{name: "tracked baseline drift", mutateServer: func(m *pullRequestServerMutation) { m.BaselineContent = []byte("\x89PNG\r\n\x1a\nattacker-baseline") }},
		{name: "forged internal plan digest claim", mutateServer: func(m *pullRequestServerMutation) { m.SourceZip, m.BundleZip = forged.SourceZip, forged.BundleZip }},
		{name: "mixed source and bundle", mutateServer: func(m *pullRequestServerMutation) { m.SourceZip = mixed.SourceZip }},
		{name: "mixed bundle and source", mutateServer: func(m *pullRequestServerMutation) { m.BundleZip = mixed.BundleZip }},
		{name: "PR closes during download", mutateServer: func(m *pullRequestServerMutation) { m.CloseOnSecondPRGet = true }},
		{name: "run drifts during download", mutateServer: func(m *pullRequestServerMutation) { m.DriftOnSecondRunGet = true }},
		{name: "default branch changes during download", mutateServer: func(m *pullRequestServerMutation) { m.DriftDefaultBranchOnSecondGet = true }},
		{name: "base revision changes during download", mutateServer: func(m *pullRequestServerMutation) { m.DriftBaseBranchOnSecondGet = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutation := pullRequestServerMutation{}
			if test.mutateServer != nil {
				test.mutateServer(&mutation)
			}
			server := newPullRequestVerifierServer(t, fixture, mutation)
			defer server.Close()
			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			request := pullRequestBundleRequest(t, fixture)
			if test.mutateRequest != nil {
				test.mutateRequest(&request)
			}
			if _, _, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), request); err == nil {
				t.Fatal("adversarial PR bundle binding was accepted")
			}
		})
	}
}

func TestFetchAndVerifyVisualHivePullRequestBundleNeverTrustsPRHeadWorkflowBytes(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{HeadWorkflowContent: []byte("name: attacker-controlled-head-workflow\n")})
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, _, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), pullRequestBundleRequest(t, fixture)); err == nil || !strings.Contains(err.Error(), "PR head modified") {
		t.Fatalf("PR-modified workflow was allowed to authorize its own artifacts: %v", err)
	}
}

func TestFetchAndVerifyVisualHivePullRequestBundleIgnoresCallerPreseededExtractionCache(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{})
	defer server.Close()
	destination := t.TempDir()
	for _, relative := range []string{"artifact-99", "source-artifact-98"} {
		dir := filepath.Join(destination, relative)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".hive-extraction-complete"), []byte("attacker preseed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	request := pullRequestBundleRequest(t, fixture)
	request.DestinationDir = destination
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, verified, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), request)
	if err != nil {
		t.Fatalf("fresh verifier-owned extraction was not used: %v", err)
	}
	root, err := verified.EvidenceRoot()
	if err != nil || !strings.Contains(filepath.ToSlash(root), "/.visual-hive-pr-verification-") {
		t.Fatalf("verified evidence did not come from a fresh contained extraction root: root=%q err=%v", root, err)
	}
}

func pullRequestBundleRequest(t *testing.T, fixture pullRequestV3Fixture) VisualHivePullRequestBundleRequest {
	t.Helper()
	return VisualHivePullRequestBundleRequest{
		Repository: "owner/repo", PullRequestNumber: 7,
		ExpectedBaseRepository: "owner/repo", ExpectedBaseRepositoryID: 123, ExpectedBaseBranch: "main", ExpectedBaseSHA: testPRBaseSHA,
		ExpectedHeadRepository: "fork/repo", ExpectedHeadRepositoryID: 456, ExpectedHeadBranch: "repair", ExpectedHeadSHA: testPRHeadSHA,
		ExpectedWorkflowName: "Visual Hive PR", ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml",
		ExpectedGitHubActionsAppID: 15368, ExpectedProducerGitCommit: VisualHivePullRequestProducerCommit,
		DestinationDir: t.TempDir(), MaxACMM: 6,
	}
}

func newPullRequestVerifierServer(t *testing.T, fixture pullRequestV3Fixture, mutation pullRequestServerMutation) *httptest.Server {
	t.Helper()
	defaults := func(value, fallback string) string {
		if value == "" {
			return fallback
		}
		return value
	}
	defaultInt := func(value, fallback int64) int64 {
		if value == 0 {
			return fallback
		}
		return value
	}
	mutation.PRState = defaults(mutation.PRState, "open")
	mutation.BaseRepository = defaults(mutation.BaseRepository, "owner/repo")
	mutation.BaseRepositoryID = defaultInt(mutation.BaseRepositoryID, 123)
	mutation.DefaultBranch = defaults(mutation.DefaultBranch, "main")
	mutation.DefaultBranchSHA = defaults(mutation.DefaultBranchSHA, testPRBaseSHA)
	mutation.BaseBranch = defaults(mutation.BaseBranch, "main")
	mutation.BaseSHA = defaults(mutation.BaseSHA, testPRBaseSHA)
	mutation.HeadRepository = defaults(mutation.HeadRepository, "fork/repo")
	mutation.HeadRepositoryID = defaultInt(mutation.HeadRepositoryID, 456)
	mutation.HeadBranch = defaults(mutation.HeadBranch, "repair")
	mutation.HeadSHA = defaults(mutation.HeadSHA, testPRHeadSHA)
	if mutation.RunAttempt == 0 {
		mutation.RunAttempt = 3
	}
	mutation.RunWorkflowID = defaultInt(mutation.RunWorkflowID, 12)
	mutation.RunEvent = defaults(mutation.RunEvent, "pull_request")
	mutation.RunStatus = defaults(mutation.RunStatus, "completed")
	mutation.RunConclusion = defaults(mutation.RunConclusion, "success")
	mutation.RunName = defaults(mutation.RunName, "Visual Hive PR")
	mutation.RunPath = defaults(mutation.RunPath, ".github/workflows/visual-hive-pr.yml")
	mutation.RunBaseRepository = defaults(mutation.RunBaseRepository, "owner/repo")
	mutation.RunBaseRepositoryID = defaultInt(mutation.RunBaseRepositoryID, 123)
	mutation.RunHeadRepository = defaults(mutation.RunHeadRepository, "fork/repo")
	mutation.RunHeadRepositoryID = defaultInt(mutation.RunHeadRepositoryID, 456)
	mutation.WorkflowName = defaults(mutation.WorkflowName, "Visual Hive PR")
	mutation.WorkflowPath = defaults(mutation.WorkflowPath, ".github/workflows/visual-hive-pr.yml")
	mutation.WorkflowState = defaults(mutation.WorkflowState, "active")
	if mutation.WorkflowContent == nil {
		mutation.WorkflowContent = fixture.Workflow
	}
	if mutation.HeadWorkflowContent == nil {
		mutation.HeadWorkflowContent = mutation.WorkflowContent
	}
	if mutation.ConfigContent == nil {
		mutation.ConfigContent = fixture.Config
	}
	if mutation.BaselineContent == nil {
		mutation.BaselineContent = fixture.Baseline
	}
	mutation.BundleArtifactID = defaultInt(mutation.BundleArtifactID, 99)
	mutation.BundleArtifactName = defaults(mutation.BundleArtifactName, "visual-hive-pr-bundle-77-3")
	mutation.SourceArtifactID = defaultInt(mutation.SourceArtifactID, 98)
	mutation.SourceArtifactName = defaults(mutation.SourceArtifactName, "visual-hive-pr-source-77-3")
	mutation.ArtifactRunID = defaultInt(mutation.ArtifactRunID, 77)
	mutation.ArtifactRepositoryID = defaultInt(mutation.ArtifactRepositoryID, 123)
	mutation.ArtifactHeadRepositoryID = defaultInt(mutation.ArtifactHeadRepositoryID, 456)
	mutation.ArtifactHeadBranch = defaults(mutation.ArtifactHeadBranch, "repair")
	mutation.ArtifactHeadSHA = defaults(mutation.ArtifactHeadSHA, testPRHeadSHA)
	mutation.CheckAppID = defaultInt(mutation.CheckAppID, 15368)
	if mutation.BundleZip == nil {
		mutation.BundleZip = fixture.BundleZip
	}
	if mutation.SourceZip == nil {
		mutation.SourceZip = fixture.SourceZip
	}
	prGets := 0
	runGets := 0
	artifactGets := 0
	repositoryGets := 0
	baseBranchGets := 0
	workflowRunPayload := func(id int64, headSHA string) map[string]interface{} {
		associationBaseRepository := map[string]interface{}{"id": int64(123), "full_name": "owner/repo"}
		associationHeadRepository := map[string]interface{}{"id": int64(456), "full_name": "fork/repo"}
		if mutation.MinimalAssociationRepositories {
			associationBaseRepository = map[string]interface{}{"id": int64(123), "name": "repo", "url": "https://api.github.test/repos/owner/repo"}
			associationHeadRepository = map[string]interface{}{"id": int64(456), "name": "repo", "url": "https://api.github.test/repos/fork/repo"}
		}
		if mutation.WrongAssociationRepositories {
			associationHeadRepository = map[string]interface{}{"id": int64(999), "full_name": "attacker/repo"}
		}
		association := []interface{}{map[string]interface{}{"number": 7, "base": map[string]interface{}{"ref": mutation.BaseBranch, "sha": mutation.BaseSHA, "repo": associationBaseRepository}, "head": map[string]interface{}{"ref": mutation.HeadBranch, "sha": mutation.HeadSHA, "repo": associationHeadRepository}}}
		if mutation.WrongRunAssociation {
			association = []interface{}{map[string]interface{}{"number": 8, "base": map[string]interface{}{"ref": mutation.BaseBranch, "sha": mutation.BaseSHA, "repo": associationBaseRepository}, "head": map[string]interface{}{"ref": mutation.HeadBranch, "sha": mutation.HeadSHA, "repo": associationHeadRepository}}}
		}
		if mutation.ExtraRunAssociation {
			association = append(association, map[string]interface{}{"number": 8, "base": map[string]interface{}{"ref": mutation.BaseBranch, "sha": mutation.BaseSHA, "repo": associationBaseRepository}, "head": map[string]interface{}{"ref": mutation.HeadBranch, "sha": mutation.HeadSHA, "repo": associationHeadRepository}})
		}
		if mutation.OmitRunAssociations {
			association = nil
		}
		return map[string]interface{}{
			"id": id, "run_attempt": mutation.RunAttempt, "workflow_id": mutation.RunWorkflowID, "check_suite_id": 501, "name": mutation.RunName, "path": mutation.RunPath,
			"head_branch": mutation.HeadBranch, "head_sha": headSHA, "event": mutation.RunEvent, "status": mutation.RunStatus, "conclusion": mutation.RunConclusion,
			"html_url": fmt.Sprintf("https://github.test/owner/repo/actions/runs/%d", id), "repository": map[string]interface{}{"id": mutation.RunBaseRepositoryID, "full_name": mutation.RunBaseRepository},
			"head_repository": map[string]interface{}{"id": mutation.RunHeadRepositoryID, "full_name": mutation.RunHeadRepository}, "pull_requests": association,
		}
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			repositoryGets++
			defaultBranch := mutation.DefaultBranch
			if mutation.DriftDefaultBranchOnSecondGet && repositoryGets > 1 {
				defaultBranch = "release"
			}
			writeJSON(t, writer, map[string]interface{}{"id": 123, "full_name": "owner/repo", "default_branch": defaultBranch})
		case "/repos/fork/repo":
			_, _ = io.WriteString(writer, `{"id":456,"full_name":"fork/repo"}`)
		case "/repos/owner/repo/branches/main":
			baseBranchGets++
			branchSHA := mutation.DefaultBranchSHA
			if mutation.DriftBaseBranchOnSecondGet && baseBranchGets > 1 {
				branchSHA = strings.Repeat("e", 40)
			}
			writeJSON(t, writer, map[string]interface{}{"name": "main", "commit": map[string]interface{}{"sha": branchSHA}})
		case "/repos/owner/repo/pulls/7":
			prGets++
			state := mutation.PRState
			if mutation.CloseOnSecondPRGet && prGets > 1 {
				state = "closed"
			}
			writeJSON(t, writer, map[string]interface{}{
				"number": 7, "state": state, "merged": mutation.Merged,
				"base": map[string]interface{}{"ref": mutation.BaseBranch, "sha": mutation.BaseSHA, "repo": map[string]interface{}{"id": mutation.BaseRepositoryID, "full_name": mutation.BaseRepository}},
				"head": map[string]interface{}{"ref": mutation.HeadBranch, "sha": mutation.HeadSHA, "repo": map[string]interface{}{"id": mutation.HeadRepositoryID, "full_name": mutation.HeadRepository}},
			})
		case "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs":
			runs := []interface{}{workflowRunPayload(77, mutation.HeadSHA)}
			if mutation.ExtraMatchingRun {
				runs = append(runs, workflowRunPayload(78, mutation.HeadSHA))
			}
			writeJSON(t, writer, map[string]interface{}{"total_count": len(runs), "workflow_runs": runs})
		case "/repos/owner/repo/actions/runs/77":
			runGets++
			headSHA := mutation.HeadSHA
			if mutation.DriftOnSecondRunGet && runGets > 1 {
				headSHA = strings.Repeat("e", 40)
			}
			writeJSON(t, writer, workflowRunPayload(77, headSHA))
		case "/repos/owner/repo/actions/runs/77/attempts/3/jobs":
			writeJSON(t, writer, map[string]interface{}{"total_count": 1, "jobs": []interface{}{map[string]interface{}{
				"id": 700, "run_id": 77, "run_attempt": 3, "name": "visual-hive", "workflow_name": "Visual Hive PR",
				"head_branch": mutation.HeadBranch, "head_sha": mutation.HeadSHA, "status": "completed", "conclusion": "success",
				"html_url": "https://github.test/owner/repo/actions/runs/77/job/700", "check_run_url": "https://api.github.test/repos/owner/repo/check-runs/900",
			}}})
		case "/repos/owner/repo/check-runs/900":
			writeJSON(t, writer, map[string]interface{}{
				"id": 900, "name": "visual-hive", "head_sha": mutation.HeadSHA, "status": "completed", "conclusion": "success",
				"app": map[string]interface{}{"id": mutation.CheckAppID}, "check_suite": map[string]interface{}{"id": 501},
			})
		case "/repos/owner/repo/actions/workflows/12":
			writeJSON(t, writer, map[string]interface{}{"id": 12, "name": mutation.WorkflowName, "path": mutation.WorkflowPath, "state": mutation.WorkflowState})
		case "/repos/owner/repo/contents/.github/workflows/visual-hive-pr.yml":
			if request.URL.Query().Get("ref") != testPRBaseSHA {
				http.Error(writer, "wrong base workflow ref", http.StatusBadRequest)
				return
			}
			writeContentResponse(t, writer, ".github/workflows/visual-hive-pr.yml", mutation.WorkflowContent)
		case "/repos/fork/repo/contents/.github/workflows/visual-hive-pr.yml":
			if request.URL.Query().Get("ref") != testPRHeadSHA {
				http.Error(writer, "wrong head workflow ref", http.StatusBadRequest)
				return
			}
			writeContentResponse(t, writer, ".github/workflows/visual-hive-pr.yml", mutation.HeadWorkflowContent)
		case "/repos/fork/repo/contents/visual-hive.config.yaml":
			if request.URL.Query().Get("ref") != testPRHeadSHA {
				http.Error(writer, "wrong head config ref", http.StatusBadRequest)
				return
			}
			writeContentResponse(t, writer, "visual-hive.config.yaml", mutation.ConfigContent)
		case "/repos/fork/repo/contents/button.png":
			if request.URL.Query().Get("ref") != testPRHeadSHA {
				http.Error(writer, "wrong head baseline ref", http.StatusBadRequest)
				return
			}
			writeContentResponse(t, writer, "button.png", mutation.BaselineContent)
		case "/repos/owner/repo/actions/runs/77/artifacts":
			artifactGets++
			expired := mutation.ArtifactsExpired || (mutation.ExpireArtifactsOnSecondList && artifactGets > 1)
			writeJSON(t, writer, map[string]interface{}{"total_count": 2, "artifacts": []interface{}{
				map[string]interface{}{"id": mutation.SourceArtifactID, "name": mutation.SourceArtifactName, "size_in_bytes": len(mutation.SourceZip), "expired": expired, "workflow_run": map[string]interface{}{"id": mutation.ArtifactRunID, "repository_id": mutation.ArtifactRepositoryID, "head_repository_id": mutation.ArtifactHeadRepositoryID, "head_branch": mutation.ArtifactHeadBranch, "head_sha": mutation.ArtifactHeadSHA}},
				map[string]interface{}{"id": mutation.BundleArtifactID, "name": mutation.BundleArtifactName, "size_in_bytes": len(mutation.BundleZip), "expired": expired, "workflow_run": map[string]interface{}{"id": mutation.ArtifactRunID, "repository_id": mutation.ArtifactRepositoryID, "head_repository_id": mutation.ArtifactHeadRepositoryID, "head_branch": mutation.ArtifactHeadBranch, "head_sha": mutation.ArtifactHeadSHA}},
			}})
		case "/repos/owner/repo/actions/artifacts/99/zip":
			writer.Header().Set("Location", server.URL+"/signed-pr-bundle")
			writer.WriteHeader(http.StatusFound)
		case "/repos/owner/repo/actions/artifacts/98/zip":
			writer.Header().Set("Location", server.URL+"/signed-pr-source")
			writer.WriteHeader(http.StatusFound)
		case "/signed-pr-bundle":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed PR bundle request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(mutation.BundleZip)
		case "/signed-pr-source":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed PR source request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(mutation.SourceZip)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	return server
}

func buildPullRequestV3Fixture(t *testing.T, headSHA string) pullRequestV3Fixture {
	return buildPullRequestV3FixtureWithBindingMutation(t, headSHA, nil)
}

func buildPullRequestV3FixtureWithBindingMutation(t *testing.T, headSHA string, mutate func(*visualhive.PullRequestSourceBinding)) pullRequestV3Fixture {
	t.Helper()
	workflow := []byte("name: Visual Hive PR\non:\n  pull_request:\n")
	config := []byte("project: demo\ncontracts:\n  - contracts/card.yaml\n")
	executionBinding := map[string]string{
		"nonceSha256": strings.Repeat("1", 64), "generatedSpecSha256": "", "generatedConfigSha256": "",
		"payloadSha256": strings.Repeat("4", 64), "bindingMacSha256": strings.Repeat("5", 64),
	}
	generatedSpec := []byte("export const visualHive = true;\n")
	generatedConfig := []byte("module.exports = { workers: 1 };\n")
	executionBinding["generatedSpecSha256"] = testDigest(generatedSpec)
	executionBinding["generatedConfigSha256"] = testDigest(generatedConfig)
	type runtimeBrowser struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type runtimeFont struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}
	type runtimeEnvironment struct {
		OS                string        `json:"os"`
		Architecture      string        `json:"architecture"`
		NodeVersion       string        `json:"nodeVersion"`
		PlaywrightVersion string        `json:"playwrightVersion"`
		Locale            string        `json:"locale"`
		Timezone          string        `json:"timezone"`
		UserAgent         string        `json:"userAgent"`
		DeviceScaleFactor int           `json:"deviceScaleFactor"`
		Fonts             []runtimeFont `json:"fonts"`
	}
	browser := runtimeBrowser{Name: "chromium", Version: "130.0.1"}
	environment := runtimeEnvironment{OS: "linux", Architecture: "x64", NodeVersion: "22.14.0", PlaywrightVersion: "1.51.0", Locale: "en-US", Timezone: "UTC", UserAgent: "VisualHive/1", DeviceScaleFactor: 1, Fonts: []runtimeFont{{Name: "Arial", Available: true}}}
	stableData, _ := json.Marshal(struct {
		Browser     runtimeBrowser     `json:"browser"`
		Environment runtimeEnvironment `json:"environment"`
	}{browser, environment})
	runtimeData, err := json.Marshal(struct {
		SchemaVersion    string             `json:"schemaVersion"`
		ExecutionBinding map[string]string  `json:"executionBinding"`
		CapturedAt       string             `json:"capturedAt"`
		Browser          runtimeBrowser     `json:"browser"`
		Environment      runtimeEnvironment `json:"environment"`
	}{visualhive.PullRequestRuntimeSchema, executionBinding, "2026-07-16T12:00:00.000Z", browser, environment})
	if err != nil {
		t.Fatal(err)
	}
	executionData, _ := json.Marshal(executionBinding)
	planData := []byte(`{"schemaVersion":1,"project":"demo","mode":"pr","changedFiles":["web/button.tsx"],"effectiveChangedFiles":["web/button.tsx"],"ignoredChangedFiles":[],"targets":[{"id":"app","kind":"web","url":"http://127.0.0.1:3000","prSafe":true,"cost":1}],"items":[{"contractId":"contract.card","targetId":"app","targetUrl":"http://127.0.0.1:3000","severity":"high","cost":1,"reasons":[],"screenshots":["button"]}],"excluded":[],"mutation":{"allowed":false},"providerPolicy":{}}`)
	reportData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": 2, "repository": map[string]interface{}{"provider": "github-actions", "repository": "owner/repo", "branch": "repair", "baseBranch": "main", "commitSha": headSHA, "pullRequestNumber": 7, "runId": "77", "runAttempt": "3", "workflow": "Visual Hive PR"}, "mode": "pr", "status": "passed", "changedFiles": []string{"web/button.tsx"},
		"selectedTargets": []interface{}{map[string]interface{}{"id": "app", "kind": "web", "url": "http://127.0.0.1:3000", "prSafe": true, "cost": "low"}}, "selectedContracts": []string{"contract.card"}, "excludedContracts": []interface{}{},
		"generatedSpecPath": ".visual-hive/generated/visual-hive.generated.spec.cjs", "executionBinding": executionBinding,
		"results": []interface{}{map[string]interface{}{"contractId": "contract.card", "targetId": "app", "status": "passed", "durationMs": 10, "errors": []string{}, "artifacts": []string{}, "screenshotAssertions": []interface{}{map[string]interface{}{"baselinePath": "button.png", "status": "passed"}}}},
		"summary": map[string]int{"passed": 1, "failed": 0}, "artifacts": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := ".visual-hive/proof/pr/baselines/button.png"
	baselineData := []byte("\x89PNG\r\n\x1a\nvisual-hive-baseline")
	baselineIdentity := visualhive.PullRequestFileIdentity{Path: baselinePath, SHA256: testDigest(baselineData), Bytes: int64(len(baselineData))}
	baselineDigest := testDigest([]byte(baselineIdentity.Path + "\x00" + baselineIdentity.SHA256 + "\x00" + strconv.FormatInt(baselineIdentity.Bytes, 10)))
	changedData := []byte("web/button.tsx\n")
	planContractsData := []byte("contract.card\n")
	evaluationScopesData := []byte("contract.card\nprovider-governance\nworkflow-safety\n")
	pipelineExitData := []byte("0\n")
	runnerOutcomeData := []byte("{\"schemaVersion\":\"hive.visual-runner-outcome.v1\",\"outcome\":\"success\",\"conclusion\":\"success\"}\n")
	beadsData := []byte("[]")
	verdictData := []byte(`{"schemaVersion":"visual-hive.verdict.v1","summary":{"status":"passed"}}`)
	paritySummary := map[string]int{"expected": 1, "actual": 1, "present": 1, "blocked": 0, "missing": 0, "unexpected": 0, "mismatched": 0}
	cliIdentity := map[string]interface{}{"path": "doctor", "aliases": []string{}, "contractSha256": strings.Repeat("a", 64)}
	parityData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": "visual-hive.capability-parity.v1", "baselineVersion": "visual-hive.capability-baseline.v1", "generatedAt": "2026-07-16T12:00:00.000Z", "status": "passed", "runtimeStatus": "ready",
		"summary": paritySummary, "domains": testV3CapabilityDomains("cli", paritySummary),
		"checks": []interface{}{map[string]interface{}{"domain": "cli", "key": "doctor", "status": "present", "parity": true, "message": "present", "expected": cliIdentity, "actual": cliIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		".visual-hive/capability-parity.json":                     parityData,
		".visual-hive/hive/beads.json":                            beadsData,
		".visual-hive/verdict.json":                               verdictData,
		visualhive.PullRequestPlanPath:                            planData,
		visualhive.PullRequestReportPath:                          reportData,
		visualhive.PullRequestRuntimePath:                         runtimeData,
		visualhive.PullRequestConfigPath:                          config,
		visualhive.PullRequestWorkflowPath:                        workflow,
		visualhive.PullRequestChangedFilesPath:                    changedData,
		visualhive.PullRequestPlanContractsPath:                   planContractsData,
		visualhive.PullRequestEvaluationScopesPath:                evaluationScopesData,
		visualhive.PullRequestPipelineExitPath:                    pipelineExitData,
		visualhive.PullRequestRunnerOutcomePath:                   runnerOutcomeData,
		".visual-hive/generated/visual-hive.generated.spec.cjs":   generatedSpec,
		".visual-hive/generated/visual-hive.generated.config.cjs": generatedConfig,
		baselinePath: baselineData,
	}
	identity := func(path string) visualhive.PullRequestFileIdentity {
		data := files[path]
		return visualhive.PullRequestFileIdentity{Path: path, SHA256: testDigest(data), Bytes: int64(len(data))}
	}
	binding := visualhive.PullRequestSourceBinding{
		SchemaVersion: visualhive.PullRequestSourceBindingSchema, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		Base:     visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "main", SHA: testPRBaseSHA},
		Head:     visualhive.PullRequestRevisionIdentity{Repository: "fork/repo", RepositoryID: "456", Ref: "repair", SHA: headSHA},
		Workflow: visualhive.PullRequestWorkflowIdentity{Name: "Visual Hive PR", Path: ".github/workflows/visual-hive-pr.yml", Event: "pull_request", RunID: "77", RunAttempt: "3", Definition: identity(visualhive.PullRequestWorkflowPath), ProducerCommit: VisualHivePullRequestProducerCommit, SourceName: "visual-hive-pr-source-77-3", BundleName: "visual-hive-pr-bundle-77-3"},
		Plan:     identity(visualhive.PullRequestPlanPath), Report: identity(visualhive.PullRequestReportPath), Config: identity(visualhive.PullRequestConfigPath), ChangedFiles: identity(visualhive.PullRequestChangedFilesPath),
		PlanContracts: identity(visualhive.PullRequestPlanContractsPath), EvaluationScopes: identity(visualhive.PullRequestEvaluationScopesPath),
		Runtime:   visualhive.PullRequestRuntimeIdentity{Receipt: identity(visualhive.PullRequestRuntimePath), GeneratedSpec: identity(".visual-hive/generated/visual-hive.generated.spec.cjs"), GeneratedConfig: identity(".visual-hive/generated/visual-hive.generated.config.cjs"), StableSHA256: testDigest(stableData), ExecutionBindingSHA256: testDigest(executionData)},
		Baselines: visualhive.PullRequestBaselineIdentity{SHA256: baselineDigest, Files: []visualhive.PullRequestFileIdentity{baselineIdentity}},
	}
	if mutate != nil {
		mutate(&binding)
	}
	runnerPayloadData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": visualhive.PullRequestRunnerExecutionSchema, "repository": binding.Repository, "pullRequest": binding.PullRequest,
		"baseRef": binding.Base.Ref, "headRef": binding.Head.Ref, "headSha": binding.Head.SHA, "runId": binding.Workflow.RunID,
		"runAttempt": binding.Workflow.RunAttempt, "workflow": binding.Workflow.Name, "producerCommit": binding.Workflow.ProducerCommit,
		"executionBinding": executionBinding, "report": binding.Report, "runtime": binding.Runtime.Receipt,
		"generatedSpec": binding.Runtime.GeneratedSpec, "generatedConfig": binding.Runtime.GeneratedConfig,
		"pipelineExit": identity(visualhive.PullRequestPipelineExitPath), "runnerOutcome": identity(visualhive.PullRequestRunnerOutcomePath),
	})
	if err != nil {
		t.Fatal(err)
	}
	runnerKey := make([]byte, 32)
	for index := range runnerKey {
		runnerKey[index] = byte(index + 1)
	}
	runnerMAC := hmac.New(sha256.New, runnerKey)
	_, _ = runnerMAC.Write(runnerPayloadData)
	runnerAttestationData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": visualhive.PullRequestRunnerAttestationSchema, "algorithm": "HMAC-SHA256",
		"keyCommitmentSha256": testDigest(runnerKey), "keyBase64": base64.StdEncoding.EncodeToString(runnerKey),
		"payloadSha256": testDigest(runnerPayloadData), "payloadBase64": base64.StdEncoding.EncodeToString(runnerPayloadData),
		"macSha256": hex.EncodeToString(runnerMAC.Sum(nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	files[visualhive.PullRequestRunnerAttestationPath] = runnerAttestationData
	binding.RunnerAttestation = identity(visualhive.PullRequestRunnerAttestationPath)
	bindingData, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	files[visualhive.PullRequestSourceBindingPath] = bindingData
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]visualhive.ArtifactIndexEntry, 0, len(paths))
	summary := visualhive.ArtifactIndexSummary{}
	for _, path := range paths {
		kind, contentType := "text", "text/plain"
		switch {
		case strings.HasSuffix(path, ".json"):
			kind, contentType, summary.JSON = "json", "application/json", summary.JSON+1
		case strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml"):
			kind, contentType, summary.YAML = "yaml", "application/yaml", summary.YAML+1
		case strings.HasSuffix(path, ".png"):
			kind, contentType, summary.Image = "image", "image/png", summary.Image+1
		default:
			summary.Text++
		}
		data := files[path]
		entries = append(entries, visualhive.ArtifactIndexEntry{Path: path, Kind: kind, ContentType: contentType, Bytes: int64(len(data)), SHA256: testDigest(data), SafeToRender: kind != "image", Labels: []string{}})
		summary.TotalBytes += int64(len(data))
	}
	summary.ArtifactCount = int64(len(entries))
	summary.DiscoveredArtifactCount = summary.ArtifactCount
	index := visualhive.ArtifactIndexReport{SchemaVersion: 1, Project: "demo", GeneratedAt: "2026-07-16T12:00:00.000Z", Root: ".visual-hive", ContentAddressed: true, Complete: true, Summary: summary, Artifacts: entries, Warnings: []string{}}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(time.Millisecond)
	manifest := visualhive.Manifest{
		SchemaVersion: visualhive.ManifestSchemaV3, DigestAlgorithm: visualhive.ContentAddressedDigestAlgorithm, BundleID: "pr-verified-bundle", GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Producer: visualhive.Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: VisualHivePullRequestProducerCommit},
		Source:   visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/repair", CommitSHA: headSHA, Event: "pull_request", WorkflowName: "Visual Hive PR", WorkflowRunID: "77", WorkflowRunAttempt: "3", WorkflowArtifactID: "98", Conclusion: "success", Trusted: false},
		Project:  "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 4,
		Scan:         visualhive.Scan{Scope: "changed-files", AuthoritativeForResolution: false, EvaluatedContracts: []string{"contract.card", "provider-governance", "workflow-safety"}, EvaluatedFiles: []string{"web/button.tsx"}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
		Observations: []visualhive.Observation{},
		Files: []visualhive.File{
			{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: testDigest(indexData), Size: int64(len(indexData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: testDigest(parityData), Size: int64(len(parityData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/hive/beads.json", SourcePath: ".visual-hive/hive/beads.json", SHA256: testDigest(beadsData), Size: int64(len(beadsData)), MediaType: "application/json"},
		},
		ArtifactIndex:    &visualhive.ArtifactIndexBinding{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: testDigest(indexData), SchemaVersion: 1, ContentAddressed: true, Complete: true, ArtifactCount: summary.ArtifactCount, TotalBytes: summary.TotalBytes},
		CapabilityParity: &visualhive.CapabilityParityBinding{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: testDigest(parityData), SchemaVersion: "visual-hive.capability-parity.v1", BaselineVersion: "visual-hive.capability-baseline.v1", Status: "passed", RuntimeStatus: "ready", Summary: visualhive.CapabilityParitySummary{Expected: 1, Actual: 1, Present: 1}},
		ReplayProtection: visualhive.ReplayProtection{Nonce: "pr-verified-bundle"}, Provenance: visualhive.Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety: visualhive.Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = testDigest([]byte("visual-hive.bundle.replay.v3\x00owner/repo\x00123\x00" + headSHA + "\x0077\x003\x0098\x00pr-verified-bundle"))
	manifest.OverallDigest = testV3BundleDigest(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundleBuffer := new(bytes.Buffer)
	bundleArchive := zip.NewWriter(bundleBuffer)
	writeZipEntry(t, bundleArchive, ".visual-hive/bundles/pr-verified-bundle/manifest.json", manifestData)
	writeZipEntry(t, bundleArchive, ".visual-hive/bundles/pr-verified-bundle/files/.visual-hive/artifacts-index.json", indexData)
	writeZipEntry(t, bundleArchive, ".visual-hive/bundles/pr-verified-bundle/files/.visual-hive/capability-parity.json", parityData)
	writeZipEntry(t, bundleArchive, ".visual-hive/bundles/pr-verified-bundle/files/.visual-hive/hive/beads.json", beadsData)
	if err := bundleArchive.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBuffer := new(bytes.Buffer)
	sourceArchive := zip.NewWriter(sourceBuffer)
	writeZipEntry(t, sourceArchive, ".visual-hive/artifacts-index.json", indexData)
	for _, path := range paths {
		writeZipEntry(t, sourceArchive, path, files[path])
	}
	if err := sourceArchive.Close(); err != nil {
		t.Fatal(err)
	}
	return pullRequestV3Fixture{
		BundleZip: bundleBuffer.Bytes(), SourceZip: sourceBuffer.Bytes(), Workflow: workflow, Config: config,
		PlanSHA256: testDigest(planData), ReportSHA256: testDigest(reportData), ConfigSHA256: testDigest(config), ChangedFilesSHA256: testDigest(changedData), WorkflowSHA256: testDigest(workflow), RuntimeStableSHA256: testDigest(stableData),
		PlanContractsSHA256: testDigest(planContractsData), EvaluationScopesSHA256: testDigest(evaluationScopesData),
		ExecutionBindingSHA256: testDigest(executionData), BaselineSHA256: baselineDigest, RuntimeReceiptSHA256: testDigest(runtimeData), Baseline: baselineData,
	}
}

func writeContentResponse(t *testing.T, writer http.ResponseWriter, path string, content []byte) {
	t.Helper()
	writeJSON(t, writer, map[string]interface{}{"type": "file", "path": path, "sha": testGitBlobSHA(content), "size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
}

func testGitBlobSHA(content []byte) string {
	hash := sha1.New()
	_, _ = hash.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	_, _ = hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value interface{}) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("write test JSON: %v", err)
	}
}

func TestVisualHivePullRequestBundleRequestRequiresOnlyIndependentImmutablePins(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	request := pullRequestBundleRequest(t, fixture)
	request.ExpectedGitHubActionsAppID = 0
	if err := validateVisualHivePullRequestBundleRequest(request); err == nil {
		t.Fatal("request without an exact GitHub Actions App identity was accepted")
	}
	request = pullRequestBundleRequest(t, fixture)
	request.DestinationDir = ""
	if err := validateVisualHivePullRequestBundleRequest(request); err == nil {
		t.Fatal("request without a verifier-owned destination was accepted")
	}
	request = pullRequestBundleRequest(t, fixture)
	request.ExpectedHeadBranch = "refs/pull/7/merge"
	if err := validateVisualHivePullRequestBundleRequest(request); err == nil {
		t.Fatal("merge ref was accepted")
	}
	request = pullRequestBundleRequest(t, fixture)
	request.ExpectedProducerGitCommit = strings.Repeat("f", 40)
	if err := validateVisualHivePullRequestBundleRequest(request); err == nil {
		t.Fatal("unaudited but well-formed producer commit was accepted")
	}
}

func TestVisualHivePullRequestProducerCommitMatchesAuditedRuntime(t *testing.T) {
	const auditedProducerCommit = "1de3765924043ba87d46f200e8883b9233d94a18"
	if VisualHivePullRequestProducerCommit != auditedProducerCommit {
		t.Fatalf("Visual Hive PR producer pin %q does not match audited runtime %q", VisualHivePullRequestProducerCommit, auditedProducerCommit)
	}
}

func TestVerifiedVisualHivePullRequestBundleRejectsLocalForgeryAndTamper(t *testing.T) {
	if _, err := (VerifiedVisualHivePullRequestBundle{}).Receipt(); err == nil {
		t.Fatal("zero/local verified PR capability was accepted")
	}
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{})
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, verified, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), pullRequestBundleRequest(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	display, err := verified.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	display.Identity.Check.Summary = "changed after live verification"
	unchanged, err := verified.Receipt()
	if err != nil || unchanged.Identity.Check.Summary == display.Identity.Check.Summary {
		t.Fatalf("display snapshot mutated the internal sealed receipt: err=%v receipt=%+v", err, unchanged)
	}
	unsealed := verified
	unsealed.sealedReceipt = visualhivepr.SealedReceipt{}
	if _, err := unsealed.Receipt(); err == nil {
		t.Fatal("unsealed verified PR capability retained authority")
	}
	if _, err := (VerifiedVisualHivePullRequestBundle{evidenceRootPath: "caller-controlled"}).Receipt(); err == nil {
		t.Fatal("caller-built plain PR receipt acquired a verification capability")
	}
}

func TestVerifiedVisualHivePullRequestBundleAppliesReplaySafeCheckEvidenceOnly(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	server := newPullRequestVerifierServer(t, fixture, pullRequestServerMutation{})
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, verified, err := client.FetchAndVerifyVisualHivePullRequestBundle(context.Background(), pullRequestBundleRequest(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fingerprint := strings.Repeat("9", 64)
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, Findings: map[string]*visualhive.FindingLifecycle{
			fingerprint: {Repository: "owner/repo", RepositoryID: "123", RepositoryFingerprint: fingerprint, Fingerprint: "finding.card", Status: visualhive.StatusPROpen, Branch: "repair", RepairCommitSHA: testPRHeadSHA, PRNumber: 7, IssueNumber: 44, IssueURL: "https://github.test/owner/repo/issues/44"},
		},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := verified.ApplyCheckEvidence(store, fingerprint)
	if err != nil || first.Idempotent {
		t.Fatalf("apply verified PR receipt: result=%+v err=%v", first, err)
	}
	finding, ok := store.Finding(fingerprint)
	if !ok || finding.Status != visualhive.StatusReady || finding.IssueNumber != 44 || finding.LastPullRequestCheckReceipt == nil || len(finding.LastCheckRuns) != 1 || !finding.LastCheckRuns[0].ProvenanceVerified || len(store.Snapshot().Outbox) != 0 {
		t.Fatalf("verified PR receipt changed more than check evidence: finding=%+v state=%+v", finding, store.Snapshot())
	}
	second, err := verified.ApplyCheckEvidence(store, fingerprint)
	if err != nil || !second.Idempotent {
		t.Fatalf("exact PR receipt replay was not a no-op: result=%+v err=%v", second, err)
	}
	receipt, err := verified.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	receipt.Identity.Check.Summary = "caller-mutated display snapshot"
	receipt.Identity.Authority.WriteBaselines = true
	third, err := verified.ApplyCheckEvidence(store, fingerprint)
	if err != nil || !third.Idempotent {
		t.Fatalf("mutating a display snapshot changed the opaque receipt capability: result=%+v err=%v", third, err)
	}
	after, _ := store.Finding(fingerprint)
	if after.LastPullRequestCheckReceipt.Identity.Authority.WriteBaselines || after.LastCheckSummary == receipt.Identity.Check.Summary {
		t.Fatalf("display snapshot mutation acquired lifecycle authority: %+v", after)
	}
}

func TestPullRequestV3FixtureCarriesStrictNormativeBinding(t *testing.T) {
	fixture := buildPullRequestV3Fixture(t, testPRHeadSHA)
	reader, err := zip.NewReader(bytes.NewReader(fixture.SourceZip), int64(len(fixture.SourceZip)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if file.Name != visualhive.PullRequestSourceBindingPath {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(opened)
		_ = opened.Close()
		if err != nil {
			t.Fatal(err)
		}
		binding, err := visualhive.ParsePullRequestSourceBinding(data)
		if err != nil {
			t.Fatal(err)
		}
		if binding.SchemaVersion != visualhive.PullRequestSourceBindingSchema || binding.Runtime.Receipt.Path != visualhive.PullRequestRuntimePath || binding.Config.Path != visualhive.PullRequestConfigPath {
			t.Fatalf("unexpected normative PR source binding: %+v", binding)
		}
		return
	}
	t.Fatal("source fixture omitted the normative PR source binding")
}
